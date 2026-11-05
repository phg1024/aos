//go:build linux

package sandbox_test

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"aos/internal/sandbox"
)

func TestLandlockConfinesRun(t *testing.T) {
	if err := sandbox.Available(); err != nil {
		t.Skip(err)
	}
	root := t.TempDir()
	ws, ref, scratch := filepath.Join(root, "ws"), filepath.Join(root, "ref"), filepath.Join(root, "scratch")
	for _, dir := range []string{ws, ref, scratch} {
		os.MkdirAll(dir, 0o755)
	}
	os.WriteFile(filepath.Join(root, "secret.txt"), []byte("hunter2"), 0o600)
	policy := sandbox.Policy{Read: []string{ref}, Write: []string{ws}, Temp: scratch}
	run := func(p sandbox.Policy, command string) (string, error) {
		cmd, err := sandbox.Command(context.Background(), p, ws, command)
		if err != nil {
			return "", err
		}
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	if out, err := run(policy, "echo ok > inside.txt && cat inside.txt"); err != nil || strings.TrimSpace(out) != "ok" {
		t.Fatalf("in scope: %q %v", out, err)
	}
	for _, command := range []string{"cat " + filepath.Join(root, "secret.txt"), "echo x > " + filepath.Join(root, "outside.txt"), "echo x > " + filepath.Join(ref, "verdict.txt")} {
		if out, err := run(policy, command); err == nil || strings.Contains(out, "hunter2") {
			t.Fatalf("%q was allowed: %q", command, out)
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	// bash's /dev/tcp where there is bash, else busybox nc.
	probe := "if command -v bash >/dev/null; then bash -c 'exec 3<>/dev/tcp/127.0.0.1/" + itoa(port) + "'; else nc -z -w 2 127.0.0.1 " + itoa(port) + "; fi"
	if _, err := run(policy, probe); err == nil {
		t.Fatal("a TCP connection was allowed without network")
	}
	networked := policy
	networked.Network = true
	if out, err := run(networked, probe); err != nil {
		t.Fatalf("with network: %q %v", out, err)
	}
	nested := policy
	nested.Hidden = []string{filepath.Join(ws, ".aos-state")}
	if _, err := sandbox.Command(context.Background(), nested, ws, "true"); !errors.Is(err, sandbox.ErrUnavailable) {
		t.Fatalf("a hidden root inside a write root cannot be expressed and must be refused: %v", err)
	}
}

func itoa(n int) string {
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return digits
}

// A GPU grant adds its devices and nothing else: another device, the state directory, a
// path outside the scopes and the network all stay refused. This needs no GPU: with no card
// here the grant names an ordinary device in place of one.
func TestAGPUGrantGrantsNothingElse(t *testing.T) {
	if err := sandbox.Available(); err != nil {
		t.Skip(err)
	}
	root := t.TempDir()
	ws, state, outside, scratch := filepath.Join(root, "ws"), filepath.Join(root, "state"), filepath.Join(root, "outside"), filepath.Join(root, "scratch")
	for _, dir := range []string{ws, state, outside, scratch} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(state, "secret"), []byte("hunter2"), 0o644); err != nil {
		t.Fatal(err)
	}
	devices, err := sandbox.ResolveGPUs(strings.Fields(os.Getenv("AOS_TEST_GPU_UUID")))
	if err != nil {
		t.Fatal(err)
	}
	policy := sandbox.Policy{Read: []string{ws}, Write: []string{ws}, Hidden: []string{state}, Temp: scratch, Devices: devices}
	run := func(command string) (string, error) {
		cmd, err := sandbox.Command(context.Background(), policy, ws, command)
		if err != nil {
			t.Fatal(err)
		}
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	if out, err := run("echo ok > " + filepath.Join(ws, "written")); err != nil {
		t.Fatalf("its own workspace: %q %v", out, err)
	}
	// The grant's one wider edge, which CUDA cannot start without: its own thread's name.
	if out, err := run("echo x > /proc/self/comm"); err != nil {
		t.Errorf("/proc with a grant: %q %v", out, err)
	}
	ungranted := policy
	ungranted.Devices = nil
	cmd, err := sandbox.Command(context.Background(), ungranted, ws, "echo x > /proc/self/comm")
	if err != nil {
		t.Fatal(err)
	}
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Errorf("/proc was writable with no grant: %q", out)
	}
	refused := []string{
		"cat " + filepath.Join(state, "secret"),
		"echo x > " + filepath.Join(state, "new"),
		"cat /proc/1/environ",
		"cat /proc/$PPID/environ",
		"cat " + filepath.Join(outside, "nothing") + " ; echo x > " + filepath.Join(outside, "new"),
	}
	found, _ := filepath.Glob("/dev/dri/*")
	found = append(found, "/dev/nvidia-uvm-tools", "/dev/nvidia-modeset")
	for _, device := range found {
		if info, err := os.Stat(device); err == nil && !info.IsDir() {
			refused = append(refused, "exec 3<>"+device)
		}
	}
	for _, command := range refused {
		if out, err := run(command); err == nil {
			t.Errorf("%q was allowed: %q", command, out)
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	probe := "if command -v bash >/dev/null; then bash -c 'exec 3<>/dev/tcp/127.0.0.1/" + itoa(listener.Addr().(*net.TCPAddr).Port) + "'; else exit 0; fi"
	if out, err := run(probe); err == nil && strings.Contains(probe, "bash -c") && commandExists("bash") {
		t.Errorf("the network was allowed: %q", out)
	}
}

func commandExists(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// The measurement: through this helper, a card's UUID in AOS_TEST_GPU_UUID, and nothing that
// allocates GPU memory. With its devices cuInit returns 0 and nvidia-smi lists the card; without
// them it does not. It skips where no UUID is given.
func TestGPUNeedsItsGrant(t *testing.T) {
	uuid := os.Getenv("AOS_TEST_GPU_UUID")
	if uuid == "" {
		t.Skip("set AOS_TEST_GPU_UUID on a machine with the card")
	}
	devices, err := sandbox.ResolveGPUs([]string{uuid})
	if err != nil {
		t.Fatal(err)
	}
	const check = `python3 -c "import ctypes; print('cuInit', ctypes.CDLL('libcuda.so.1').cuInit(0))"; nvidia-smi -L; echo "nvidia-smi exit $?"`
	measure := func(devices []string) string {
		policy := sandbox.Policy{Temp: t.TempDir(), Devices: devices}
		cmd, err := sandbox.Command(context.Background(), policy, t.TempDir(), check)
		if err != nil {
			t.Fatal(err)
		}
		out, _ := cmd.CombinedOutput()
		return string(out)
	}
	with, without := measure(devices), measure(nil)
	t.Logf("devices %v\nwith the grant:\n%s\nwithout:\n%s", devices, with, without)
	if !strings.Contains(with, "cuInit 0") || !strings.Contains(with, "nvidia-smi exit 0") {
		t.Errorf("with the grant:\n%s", with)
	}
	if strings.Contains(without, "cuInit 0") || strings.Contains(without, "nvidia-smi exit 0") {
		t.Errorf("without the grant:\n%s", without)
	}
}
