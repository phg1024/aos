package runtime_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aos/internal/artifact"
	"aos/internal/capability"
	"aos/internal/kernel"
	"aos/internal/lease"
	"aos/internal/runtime"
	"aos/internal/tools"
)

type fenceFixture struct {
	kernel    *kernel.Kernel
	store     *artifact.Store
	leases    *lease.Manager
	workspace string
}

func newFenceFixture(t *testing.T) fenceFixture {
	t.Helper()
	root := t.TempDir()
	k, err := kernel.New(filepath.Join(root, "kernel"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := artifact.New(filepath.Join(root, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	leases, err := lease.New(filepath.Join(root, "leases"), k)
	if err != nil {
		t.Fatal(err)
	}
	workspace, _ := filepath.EvalSymlinks(t.TempDir())
	return fenceFixture{kernel: k, store: store, leases: leases, workspace: workspace}
}

func (f fenceFixture) spawn(t *testing.T) kernel.Process {
	t.Helper()
	process, err := f.kernel.Spawn(kernel.ProcessSpec{
		Role:          "worker",
		Mounts:        []capability.Mount{{Name: tools.WorkspaceMount, HostPath: f.workspace}},
		CapabilitySet: capability.Set{Tools: []string{"acquire_lease", "release_lease", "write_file", "edit_file", "read_file"}, ReadPaths: []string{f.workspace}, WritePaths: []string{f.workspace}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return process
}

func (f fenceFixture) runtime(backend runtime.Backend) *runtime.AgentRuntime {
	return &runtime.AgentRuntime{Kernel: f.kernel, Artifacts: f.store, Leases: f.leases, Tools: tools.NewRuntime(), Backend: backend, MaxSteps: 8}
}

func (f fenceFixture) events(t *testing.T, eventType string) []kernel.Event {
	t.Helper()
	events, err := f.kernel.Events()
	if err != nil {
		t.Fatal(err)
	}
	var out []kernel.Event
	for _, event := range events {
		if event.Type == eventType {
			out = append(out, event)
		}
	}
	return out
}

// toolErrors are the tool_error messages the agent saw, in order.
func toolErrors(result runtime.RunResult) []string {
	var out []string
	for _, message := range result.Session.Messages {
		if strings.HasPrefix(message.Content, "tool_error ") {
			out = append(out, message.Content)
		}
	}
	return out
}

func (f fenceFixture) run(t *testing.T, process kernel.Process, actions ...runtime.GenerateResult) runtime.RunResult {
	t.Helper()
	result, err := f.runtime(steps(actions...)).RunWithOptions(context.Background(), process.PID, nil, runtime.RunOptions{ContinueOnToolError: true})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return result
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, _ := os.ReadFile(path)
	return string(data)
}

// A4 inverted: a writer whose lease lapsed and was taken over by another
// process cannot write the file, even after the new holder releases it.
// The refusal is a tool error, a lease.fenced event, and no intent.
func TestStaleHolderIsFenced(t *testing.T) {
	f := newFenceFixture(t)
	target := filepath.Join(f.workspace, "c.go")
	writer, thief := f.spawn(t), f.spawn(t)
	if _, err := f.leases.Acquire("c.go", writer.PID, 30*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if _, err := f.leases.Acquire("c.go", thief.PID, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("thief's version"), 0o644); err != nil {
		t.Fatal(err)
	}
	// One run: a write under the thief's lease; the thief releases; a
	// write with the stale token; acquire_lease; a write that goes through.
	actions := []runtime.GenerateResult{
		toolAction("write_file", map[string]string{"path": target, "content": "stale holder"}),
		toolAction("write_file", map[string]string{"path": target, "content": "stale holder, again"}),
		toolAction("acquire_lease", map[string]string{"resource": "c.go"}),
		toolAction("write_file", map[string]string{"path": target, "content": "re-acquired"}),
	}
	var afterFirst string
	backend := &funcBackend{fn: func(call int, _ context.Context, _ runtime.GenerateRequest) (runtime.GenerateResult, error) {
		if call == 2 {
			afterFirst = read(t, target)
			if err := f.leases.Release("c.go", thief.PID); err != nil {
				t.Error(err)
			}
		}
		if call <= len(actions) {
			return actions[call-1], nil
		}
		return exitResult("done"), nil
	}}
	result, err := f.runtime(backend).RunWithOptions(context.Background(), writer.PID, nil, runtime.RunOptions{ContinueOnToolError: true})
	if err != nil {
		t.Fatal(err)
	}
	errs := toolErrors(result)
	if len(errs) != 2 || !strings.Contains(errs[0], "write fenced by a lease") || !strings.Contains(errs[0], thief.PID) {
		t.Fatalf("tool errors: %v", errs)
	}
	if !strings.Contains(errs[1], "your token 1") || !strings.Contains(errs[1], "acquire_lease again") {
		t.Fatalf("stale after release: %v", errs[1])
	}
	if afterFirst != "thief's version" || read(t, target) != "re-acquired" {
		t.Fatalf("file after the fenced write %q, at the end %q", afterFirst, read(t, target))
	}
	fenced := f.events(t, runtime.LeaseFencedEvent)
	if len(fenced) != 2 || fenced[0].ProcessID != writer.PID || fenced[0].Data["reason"] != lease.FenceHeldByOther || fenced[0].Data["holder"] != thief.PID || fenced[0].Data["token"] != "2" || fenced[0].Data["resource"] != "c.go" {
		t.Fatalf("lease.fenced = %+v", fenced)
	}
	if fenced[1].Data["reason"] != lease.FenceStaleToken || fenced[1].Data["held_token"] != "1" || fenced[1].Data["holder"] != "" {
		t.Fatalf("lease.fenced = %+v", fenced[1])
	}
	// Only the write that went through has an intent.
	if intents := f.events(t, runtime.ToolStartedEvent); len(intents) != 1 {
		t.Fatalf("fenced writes record no intent: %+v", intents)
	}
}

// acquire_lease shows the token, and a renewal keeps it.
func TestLeaseTokenIsShown(t *testing.T) {
	f := newFenceFixture(t)
	process := f.spawn(t)
	var outputs []map[string]string
	agent := f.runtime(steps(
		toolAction("acquire_lease", map[string]string{"resource": "c.go"}),
		toolAction("acquire_lease", map[string]string{"resource": "c.go"}),
		toolAction("acquire_lease", map[string]string{"resource": "d.go"}),
	))
	if _, err := agent.RunWithOptions(context.Background(), process.PID, nil, runtime.RunOptions{ContinueOnToolError: true, OnToolResult: func(result tools.Result) {
		outputs = append(outputs, result.Output)
	}}); err != nil {
		t.Fatal(err)
	}
	if len(outputs) != 3 || outputs[0]["token"] != "1" || outputs[1]["token"] != "1" || outputs[2]["token"] != "1" {
		t.Fatalf("outputs = %v", outputs)
	}
}

// A path another live process leases, directly or through a directory
// above it, refuses write_file and edit_file; paths outside it and
// non-path resources are not affected; the holder writes freely.
func TestForeignLeaseBlocksWrite(t *testing.T) {
	f := newFenceFixture(t)
	holder, other := f.spawn(t), f.spawn(t)
	os.MkdirAll(filepath.Join(f.workspace, "src", "pkg"), 0o755)
	inside := filepath.Join(f.workspace, "src", "pkg", "a.go")
	os.WriteFile(inside, []byte("package a"), 0o644)
	outside := filepath.Join(f.workspace, "docs.md")
	for _, resource := range []string{"src", filepath.Join(f.workspace, "notes"), "build-lock"} {
		if _, err := f.leases.Acquire(resource, holder.PID, time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	result := f.run(t, other,
		toolAction("write_file", map[string]string{"path": inside, "content": "overwritten"}),
		toolAction("edit_file", map[string]string{"path": inside, "old_text": "package a", "new_text": "package b"}),
		toolAction("write_file", map[string]string{"path": filepath.Join(f.workspace, "notes", "n.txt"), "content": "x"}),
		toolAction("write_file", map[string]string{"path": outside, "content": "free"}),
		toolAction("write_file", map[string]string{"path": filepath.Join(f.workspace, "srcfile"), "content": "free"}),
	)
	errs := toolErrors(result)
	if len(errs) != 3 {
		t.Fatalf("tool errors: %v", errs)
	}
	for i, resource := range []string{`"src"`, `"src"`, `notes"`} {
		if !strings.Contains(errs[i], "write fenced by a lease") || !strings.Contains(errs[i], resource) {
			t.Fatalf("error %d: %s", i, errs[i])
		}
	}
	if read(t, inside) != "package a" || read(t, outside) != "free" || read(t, filepath.Join(f.workspace, "srcfile")) != "free" {
		t.Fatal("files")
	}
	if fenced := f.events(t, runtime.LeaseFencedEvent); len(fenced) != 3 || fenced[1].Data["tool"] != "edit_file" {
		t.Fatalf("lease.fenced = %+v", fenced)
	}
	f.run(t, holder, toolAction("edit_file", map[string]string{"path": inside, "old_text": "package a", "new_text": "package held"}))
	if got := read(t, inside); got != "package held" {
		t.Fatalf("the holder's edit: %q", got)
	}
}

// A recovered process writes under the lease it took over from its
// predecessor.
func TestRecoveredProcessWritesUnderItsLease(t *testing.T) {
	f := newFenceFixture(t)
	process := f.spawn(t)
	target := filepath.Join(f.workspace, "c.go")
	failing := &funcBackend{fn: func(call int, _ context.Context, _ runtime.GenerateRequest) (runtime.GenerateResult, error) {
		if call == 1 {
			return toolAction("acquire_lease", map[string]string{"resource": "c.go", "ttl": "1h"}), nil
		}
		return runtime.GenerateResult{}, errors.New("connection reset")
	}}
	agent := f.runtime(failing)
	agent.BackendRetry = runtime.BackendRetryPolicy{Attempts: -1}
	failed, err := agent.Run(context.Background(), process.PID, nil)
	if err == nil || !failed.ExitStatus.Retryable {
		t.Fatalf("first run = %+v %v", failed, err)
	}
	recovered, err := f.runtime(steps(toolAction("write_file", map[string]string{"path": target, "content": "recovered"}))).Recover(context.Background(), process.PID, "", nil, runtime.RunOptions{ContinueOnToolError: true})
	if err != nil || len(toolErrors(recovered)) != 0 || read(t, target) != "recovered" {
		t.Fatalf("recovered = %v %v, file %q", toolErrors(recovered), err, read(t, target))
	}
}

// NoLeaseFencing writes as AOS did before fencing, and hides the token.
func TestNoLeaseFencing(t *testing.T) {
	f := newFenceFixture(t)
	holder, other := f.spawn(t), f.spawn(t)
	target := filepath.Join(f.workspace, "c.go")
	if _, err := f.leases.Acquire("c.go", holder.PID, time.Hour); err != nil {
		t.Fatal(err)
	}
	var token string
	agent := f.runtime(steps(
		toolAction("write_file", map[string]string{"path": target, "content": "unfenced"}),
		toolAction("acquire_lease", map[string]string{"resource": "d.go"}),
	))
	agent.NoLeaseFencing = true
	result, err := agent.RunWithOptions(context.Background(), other.PID, nil, runtime.RunOptions{ContinueOnToolError: true, OnToolResult: func(r tools.Result) {
		if r.Tool == "acquire_lease" {
			token = r.Output["token"]
		}
	}})
	if err != nil || len(toolErrors(result)) != 0 || read(t, target) != "unfenced" || token != "" || len(f.events(t, runtime.LeaseFencedEvent)) != 0 {
		t.Fatalf("errors %v, file %q, token %q, err %v", toolErrors(result), read(t, target), token, err)
	}
}
