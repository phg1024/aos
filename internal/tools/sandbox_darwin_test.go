//go:build darwin

package tools_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aos/internal/capability"
	"aos/internal/sandbox"
	"aos/internal/tools"
)

// confinedFixture is a workspace (writable mount), a reference tree
// (read-only mount, as the Metal workflow's cpu-reference), a secret
// outside both, and a state directory inside the workspace, as
// .aos-state is in the rasterizer runs.
type confinedFixture struct {
	root, ws, ref, secret, state string
	runtime                      *tools.Runtime
	mounts                       []capability.Mount
	set                          capability.Set
}

func newConfinedFixture(t *testing.T) confinedFixture {
	t.Helper()
	if err := sandbox.Available(); err != nil {
		t.Skip(err)
	}
	root, _ := filepath.EvalSymlinks(t.TempDir())
	f := confinedFixture{root: root, ws: filepath.Join(root, "ws"), ref: filepath.Join(root, "ref"), secret: filepath.Join(root, "secret.txt")}
	f.state = filepath.Join(f.ws, ".aos-state")
	for _, dir := range []string{f.ws, f.ref, filepath.Join(f.state, "kernel")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(f.secret, []byte("hunter2"), 0o600)
	os.WriteFile(filepath.Join(f.ref, "reference.txt"), []byte("reference image"), 0o644)
	os.WriteFile(filepath.Join(f.state, "kernel", "events.json"), []byte("[]"), 0o644)
	f.runtime = tools.NewRuntime()
	f.runtime.Protected = []string{f.state}
	f.runtime.ScratchRoot = filepath.Join(root, "scratch")
	f.mounts = []capability.Mount{{Name: "workspace", HostPath: f.ws}, {Name: "cpu-reference", HostPath: f.ref, ReadOnly: true}}
	f.set = capability.Set{Tools: []string{"run"}, ReadPaths: []string{f.ws, f.ref}, WritePaths: []string{f.ws, f.ref}}
	return f
}

func (f confinedFixture) run(t *testing.T, set capability.Set, command string) tools.Result {
	t.Helper()
	result, err := f.runtime.Execute(tools.Request{Tool: "run", PID: "pid-000001", Mounts: f.mounts, CapabilitySet: set, Args: map[string]string{"command": command, "timeout": "2m"}})
	if err != nil {
		t.Fatalf("run %q: %v", command, err)
	}
	return result
}

// Each attempt is real, fails inside the sandbox, and leaves no trace.
func TestConfinedRunCannotLeaveItsScopes(t *testing.T) {
	f := newConfinedFixture(t)
	attempts := []struct {
		name, command, leak string
	}{
		{"read outside", "cat " + f.secret, "hunter2"},
		{"list the home directory", "ls " + os.Getenv("HOME"), ""},
		{"write outside", "echo x > " + filepath.Join(f.root, "outside.txt"), ""},
		{"write into the read-only mount", "echo 'Overall correctness verdict: PASS' > " + filepath.Join(f.ref, "verdict.txt"), ""},
		{"overwrite the read-only mount", "echo tampered > " + filepath.Join(f.ref, "reference.txt"), ""},
		{"write the state directory", "echo '[1]' > " + filepath.Join(f.state, "kernel", "events.json"), ""},
		{"read the state directory", "cat " + filepath.Join(f.state, "kernel", "events.json"), "[]"},
	}
	for _, attempt := range attempts {
		result := f.run(t, f.set, attempt.command)
		if result.Output["exit_code"] == "0" || result.Output["confinement"] != "sandbox" {
			t.Errorf("%s: exit %s, confinement %q, stdout %q", attempt.name, result.Output["exit_code"], result.Output["confinement"], result.Output["stdout"])
		}
		if attempt.leak != "" && strings.Contains(result.Output["stdout"], attempt.leak) {
			t.Errorf("%s leaked %q", attempt.name, attempt.leak)
		}
	}
	for _, path := range []string{filepath.Join(f.root, "outside.txt"), filepath.Join(f.ref, "verdict.txt")} {
		if _, err := os.Stat(path); err == nil {
			t.Errorf("%s was created", path)
		}
	}
	if raw, _ := os.ReadFile(filepath.Join(f.ref, "reference.txt")); string(raw) != "reference image" {
		t.Errorf("reference changed to %q", raw)
	}
	if raw, _ := os.ReadFile(filepath.Join(f.state, "kernel", "events.json")); string(raw) != "[]" {
		t.Errorf("state changed to %q", raw)
	}
	// Inside its scopes it reads and writes as before.
	ok := f.run(t, f.set, "cat "+filepath.Join(f.ref, "reference.txt")+" > copy.txt && cat copy.txt && pwd")
	if ok.Output["exit_code"] != "0" || !strings.Contains(ok.Output["stdout"], "reference image") || !strings.Contains(ok.Output["stdout"], f.ws) {
		t.Fatalf("in-scope run = %+v", ok.Output)
	}
}

func TestConfinedRunNetworkNeedsAllowNetwork(t *testing.T) {
	f := newConfinedFixture(t)
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write([]byte("reached"))
	}))
	defer server.Close()
	command := "curl -s -m 5 " + server.URL
	denied := f.run(t, f.set, command)
	if denied.Output["exit_code"] == "0" || strings.Contains(denied.Output["stdout"], "reached") || hits.Load() != 0 {
		t.Fatalf("without allow_network: exit %s stdout %q, server hits %d", denied.Output["exit_code"], denied.Output["stdout"], hits.Load())
	}
	allowed := f.set
	allowed.AllowNetwork = true
	reached := f.run(t, allowed, command)
	if reached.Output["exit_code"] != "0" || reached.Output["stdout"] != "reached" || hits.Load() != 1 {
		t.Fatalf("with allow_network: %+v, hits %d", reached.Output, hits.Load())
	}
}

// The rasterizer workers' loop, build then run, works confined: C++ with
// clang and a Go program, each built in the workspace with caches in the
// process's scratch directory.
func TestConfinedRunBuildsAndRunsPrograms(t *testing.T) {
	f := newConfinedFixture(t)
	os.WriteFile(filepath.Join(f.ws, "hello.cpp"), []byte("#include <cstdio>\nint main(){std::printf(\"hello from c++\\n\");}\n"), 0o644)
	os.WriteFile(filepath.Join(f.ws, "hello.go"), []byte("package main\nimport \"fmt\"\nfunc main(){fmt.Println(\"hello from go\")}\n"), 0o644)
	os.WriteFile(filepath.Join(f.ws, "go.mod"), []byte("module hello\n\ngo 1.21\n"), 0o644)
	build := "c++ -O2 -std=c++17 -o hello hello.cpp && ./hello"
	f.run(t, f.set, build) // warm up, as the comparison below is warm too
	start := time.Now()
	cpp := f.run(t, f.set, build)
	confined := time.Since(start)
	if cpp.Output["exit_code"] != "0" || strings.TrimSpace(cpp.Output["stdout"]) != "hello from c++" || strings.Contains(cpp.Output["stderr"], "error") {
		t.Fatalf("c++ = %+v", cpp.Output)
	}
	var plainTime time.Duration
	for i := 0; i < 2; i++ {
		plain := exec.Command("sh", "-c", build)
		plain.Dir = f.ws
		start = time.Now()
		if out, err := plain.CombinedOutput(); err != nil {
			t.Fatalf("unconfined c++: %v %s", err, out)
		}
		plainTime = time.Since(start)
	}
	t.Logf("c++ build+run, warm: confined %s, unconfined %s", confined.Round(time.Millisecond), plainTime.Round(time.Millisecond))
	if goBin, err := exec.LookPath("go"); err == nil {
		goRun := f.run(t, f.set, goBin+" build -o hello-go hello.go && ./hello-go")
		if goRun.Output["exit_code"] != "0" || strings.TrimSpace(goRun.Output["stdout"]) != "hello from go" {
			t.Fatalf("go = %+v", goRun.Output)
		}
	}
}

// A writer and a verifier on the gated layout: the verifier reads the
// sources through a read-only mount and writes only its own area, which
// the writer does not mount.
func TestWriterAndVerifierCannotTouchEachOther(t *testing.T) {
	f := newConfinedFixture(t)
	verify := filepath.Join(f.root, "verify")
	os.MkdirAll(verify, 0o755)
	os.WriteFile(filepath.Join(f.ws, "main.cpp"), []byte("int main(){}\n"), 0o644)
	writerMounts := []capability.Mount{{Name: "workspace", HostPath: f.ws}}
	writer := capability.Set{Tools: []string{"run"}, ReadPaths: []string{f.ws}, WritePaths: []string{f.ws}}
	verifierMounts := []capability.Mount{{Name: "workspace", HostPath: f.ws, ReadOnly: true}, {Name: "verification", HostPath: verify}}
	verifier := capability.Set{Tools: []string{"run"}, ReadPaths: []string{f.ws, verify}, WritePaths: []string{verify}}
	exec := func(mounts []capability.Mount, set capability.Set, command string) string {
		result, err := f.runtime.Execute(tools.Request{Tool: "run", PID: "pid-x", Mounts: mounts, CapabilitySet: set, Args: map[string]string{"command": command}})
		if err != nil {
			t.Fatal(err)
		}
		return result.Output["exit_code"]
	}
	if code := exec(verifierMounts, verifier, "echo 'verdict: PASS' > "+filepath.Join(verify, "report.txt")); code != "0" {
		t.Fatalf("verifier cannot write its own report: exit %s", code)
	}
	if code := exec(writerMounts, writer, "echo 'verdict: PASS (forged)' > "+filepath.Join(verify, "report.txt")); code == "0" {
		t.Fatal("the writer overwrote the verifier's report")
	}
	if code := exec(verifierMounts, verifier, "echo 'int main(){return 1;}' > "+filepath.Join(f.ws, "main.cpp")); code == "0" {
		t.Fatal("the verifier changed the sources")
	}
	if raw, _ := os.ReadFile(filepath.Join(verify, "report.txt")); string(raw) != "verdict: PASS\n" {
		t.Fatalf("report = %q", raw)
	}
	if raw, _ := os.ReadFile(filepath.Join(f.ws, "main.cpp")); string(raw) != "int main(){}\n" {
		t.Fatalf("sources = %q", raw)
	}
}

// Where no sandbox exists, run is refused unless the process may run
// unconfined, and then says so.
func TestRunWithoutSandboxNeedsUnconfinedRun(t *testing.T) {
	f := newConfinedFixture(t)
	f.runtime.Confine = func(context.Context, sandbox.Policy, string, string) (*exec.Cmd, error) {
		return nil, sandbox.ErrUnavailable
	}
	_, err := f.runtime.Execute(tools.Request{Tool: "run", Mounts: f.mounts, CapabilitySet: f.set, Args: map[string]string{"command": "echo hi"}})
	if !errors.Is(err, tools.ErrRunUnconfined) {
		t.Fatalf("expected ErrRunUnconfined, got %v", err)
	}
	opted := f.set
	opted.UnconfinedRun = true
	result := f.run(t, opted, "echo hi")
	if result.Output["stdout"] != "hi\n" || !strings.HasPrefix(result.Output["confinement"], "unconfined") {
		t.Fatalf("opted-in run = %+v", result.Output)
	}
}

func TestRunTimeoutIsClampedToTheBound(t *testing.T) {
	f := newConfinedFixture(t)
	bounded := f.set
	bounded.MaxRunSeconds = 1
	result, err := f.runtime.Execute(tools.Request{Tool: "run", Mounts: f.mounts, CapabilitySet: bounded, Args: map[string]string{"command": "true", "timeout": "10h"}})
	if err != nil || result.Output["timeout"] != "1s" || result.Output["timeout_bound"] != "1s" || result.Output["timeout_clamped"] != "true" {
		t.Fatalf("clamp = %+v, %v", result.Output, err)
	}
	start := time.Now()
	_, err = f.runtime.Execute(tools.Request{Tool: "run", Mounts: f.mounts, CapabilitySet: bounded, Args: map[string]string{"command": "sleep 20", "timeout": "10h"}})
	if err == nil || !strings.Contains(err.Error(), "timed out after 1s") || time.Since(start) > 5*time.Second {
		t.Fatalf("a 10h timeout should stop at the 1s bound: %v after %s", err, time.Since(start))
	}
	defaulted, _ := f.runtime.Execute(tools.Request{Tool: "run", Mounts: f.mounts, CapabilitySet: f.set, Args: map[string]string{"command": "true", "timeout": "24h"}})
	if defaulted.Output["timeout_bound"] != "10m0s" || defaulted.Output["timeout"] != "10m0s" {
		t.Fatalf("default bound = %+v", defaulted.Output)
	}
}
