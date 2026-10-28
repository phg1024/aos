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
	"aos/internal/runtime"
	"aos/internal/tools"
)

// funcBackend answers each Generate call with fn(call number, ctx, request).
type funcBackend struct {
	calls int
	fn    func(call int, ctx context.Context, request runtime.GenerateRequest) (runtime.GenerateResult, error)
}

func (b *funcBackend) Generate(ctx context.Context, request runtime.GenerateRequest) (runtime.GenerateResult, error) {
	b.calls++
	return b.fn(b.calls, ctx, request)
}

func toolAction(name string, args map[string]string) runtime.GenerateResult {
	return runtime.GenerateResult{Action: runtime.GenerateAction{Type: "tool", ToolCall: &runtime.ToolCall{Name: name, Args: args}}}
}

func exitResult(reason string) runtime.GenerateResult {
	return runtime.GenerateResult{Action: runtime.GenerateAction{Type: "exit", ExitStatus: &kernel.ExitStatus{Code: 0, Reason: reason}}}
}

func newCancelFixture(t *testing.T) (*kernel.Kernel, *artifact.Store, kernel.Process) {
	t.Helper()
	root := t.TempDir()
	k, err := kernel.New(filepath.Join(root, "kernel"))
	if err != nil {
		t.Fatalf("new kernel: %v", err)
	}
	store, err := artifact.New(filepath.Join(root, "artifacts"))
	if err != nil {
		t.Fatalf("new artifact store: %v", err)
	}
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	process, err := k.Spawn(kernel.ProcessSpec{
		Role:          "interactive",
		Mounts:        []capability.Mount{{Name: "workspace", HostPath: workspace}},
		CapabilitySet: capability.Set{Tools: []string{"run"}, ReadPaths: []string{workspace}, WritePaths: []string{workspace}},
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	return k, store, process
}

// Cancelling while the model is generating must stop the run at once, even
// with ContinueOnToolError (which otherwise turns backend errors into history
// and keeps looping), and leave a checkpoint that Resume continues.
func TestAgentRuntimeCancelDuringGenerateCheckpointsAndResumes(t *testing.T) {
	k, store, process := newCancelFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var resumedHistory []runtime.Message
	backend := &funcBackend{fn: func(call int, ctx context.Context, request runtime.GenerateRequest) (runtime.GenerateResult, error) {
		switch call {
		case 1:
			return toolAction("run", map[string]string{"command": "echo before-cancel"}), nil
		case 2:
			cancel()
			<-ctx.Done()
			return runtime.GenerateResult{}, ctx.Err()
		default:
			resumedHistory = request.History
			return exitResult("done"), nil
		}
	}}
	agentRuntime := &runtime.AgentRuntime{Kernel: k, Artifacts: store, Tools: tools.NewRuntime(), Backend: backend, MaxSteps: 10}

	result, err := agentRuntime.RunWithOptions(ctx, process.PID, nil, runtime.RunOptions{ContinueOnToolError: true})
	if !errors.Is(err, runtime.ErrRunCancelled) || !errors.Is(err, context.Canceled) {
		t.Fatalf("expected ErrRunCancelled wrapping context.Canceled, got %v", err)
	}
	if !result.Cancelled || result.CheckpointID == "" {
		t.Fatalf("expected a cancelled result with a checkpoint, got %+v", result)
	}
	if backend.calls != 2 {
		t.Fatalf("run kept generating after cancel: %d calls", backend.calls)
	}
	for _, message := range result.Session.Messages {
		if strings.Contains(message.Content, "tool_error name=backend") {
			t.Fatalf("cancellation was recorded as a backend error: %q", message.Content)
		}
	}
	got, err := k.Get(process.PID)
	if err != nil {
		t.Fatalf("get process: %v", err)
	}
	if got.State != kernel.StateReady {
		t.Fatalf("cancelled process state = %s, want ready", got.State)
	}

	resumed, err := agentRuntime.Resume(context.Background(), process.PID, result.CheckpointID, nil, runtime.RunOptions{ContinueOnToolError: true})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if resumed.ExitStatus.Reason != "done" {
		t.Fatalf("unexpected resumed exit %+v", resumed.ExitStatus)
	}
	var sawEcho bool
	for _, message := range resumedHistory {
		sawEcho = sawEcho || strings.Contains(message.Content, "before-cancel")
	}
	if !sawEcho {
		t.Fatalf("resumed run lost the work done before cancel: %+v", resumedHistory)
	}
}

func TestAgentRuntimeCancelStopsRunningTool(t *testing.T) {
	k, store, process := newCancelFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	backend := &funcBackend{fn: func(call int, ctx context.Context, request runtime.GenerateRequest) (runtime.GenerateResult, error) {
		if call == 1 {
			return toolAction("run", map[string]string{"command": "sleep 30", "timeout": "60s"}), nil
		}
		t.Errorf("unexpected Generate call %d after cancel", call)
		return exitResult("unexpected"), nil
	}}
	agentRuntime := &runtime.AgentRuntime{Kernel: k, Artifacts: store, Tools: tools.NewRuntime(), Backend: backend, MaxSteps: 10}

	time.AfterFunc(200*time.Millisecond, cancel)
	start := time.Now()
	result, err := agentRuntime.RunWithOptions(ctx, process.PID, nil, runtime.RunOptions{ContinueOnToolError: true})
	if !errors.Is(err, runtime.ErrRunCancelled) {
		t.Fatalf("expected ErrRunCancelled, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("cancel took %s; the running command was not stopped", elapsed)
	}
	last := result.Session.Messages[len(result.Session.Messages)-1]
	if last.Content != "tool_cancelled name=run reason=interrupted_by_user" {
		t.Fatalf("expected a tool_cancelled note last, got %q", last.Content)
	}
}
