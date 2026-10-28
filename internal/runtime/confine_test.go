package runtime_test

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"

	"aos/internal/artifact"
	"aos/internal/capability"
	"aos/internal/kernel"
	"aos/internal/runtime"
	"aos/internal/sandbox"
	"aos/internal/tools"
)

// A web tool without network and a run that cannot be confined are
// refused, and each refusal is a capability.denied kernel event.
func TestNetworkAndUnconfinedRunDenialsAreLogged(t *testing.T) {
	root := t.TempDir()
	k, _ := kernel.New(filepath.Join(root, "kernel"))
	store, _ := artifact.New(filepath.Join(root, "artifacts"))
	process, _ := k.Spawn(kernel.ProcessSpec{
		Role:          "worker",
		Mounts:        []capability.Mount{{Name: "ws", HostPath: root}},
		CapabilitySet: capability.Set{Tools: []string{"web_fetch", "run"}, ReadPaths: []string{root}, WritePaths: []string{root}},
	})
	toolRuntime := tools.NewRuntime()
	toolRuntime.Confine = func(context.Context, sandbox.Policy, string, string) (*exec.Cmd, error) {
		return nil, sandbox.ErrUnavailable
	}
	backend := &funcBackend{fn: func(call int, _ context.Context, _ runtime.GenerateRequest) (runtime.GenerateResult, error) {
		switch call {
		case 1:
			return toolAction("web_fetch", map[string]string{"url": "https://example.com/"}), nil
		case 2:
			return toolAction("run", map[string]string{"command": "echo hi"}), nil
		}
		return exitResult("done"), nil
	}}
	agent := &runtime.AgentRuntime{Kernel: k, Artifacts: store, Tools: toolRuntime, Backend: backend}
	if _, err := agent.RunWithOptions(context.Background(), process.PID, nil, runtime.RunOptions{ContinueOnToolError: true}); err != nil {
		t.Fatalf("run: %v", err)
	}
	events, _ := k.Events()
	denied := map[string]string{}
	for _, event := range events {
		if event.Type == "capability.denied" {
			denied[event.Data["capability"]] = event.Data["action"]
		}
	}
	if denied["network"] != "web_fetch" || denied["unconfined_run"] != "run" {
		t.Fatalf("denials = %v", denied)
	}
}
