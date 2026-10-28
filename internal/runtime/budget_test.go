package runtime_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"aos/internal/artifact"
	"aos/internal/capability"
	"aos/internal/kernel"
	"aos/internal/runtime"
	"aos/internal/tools"
)

func budgetFixture(t *testing.T, set capability.Set) (*kernel.Kernel, *artifact.Store, kernel.Process, string) {
	t.Helper()
	root, _ := filepath.EvalSymlinks(t.TempDir())
	k, _ := kernel.New(filepath.Join(root, "kernel"))
	store, _ := artifact.New(filepath.Join(root, "artifacts"))
	workspace := filepath.Join(root, "ws")
	os.MkdirAll(workspace, 0o755)
	set.ReadPaths, set.WritePaths = []string{workspace}, []string{workspace}
	process, err := k.Spawn(kernel.ProcessSpec{Role: "agent:worker", Metadata: map[string]string{"agent_id": "worker"},
		Mounts: []capability.Mount{{Name: "ws", HostPath: workspace}}, CapabilitySet: set})
	if err != nil {
		t.Fatal(err)
	}
	return k, store, process, workspace
}

func messageAction(text string) runtime.GenerateResult {
	return runtime.GenerateResult{Action: runtime.GenerateAction{Type: "message", Message: text}}
}

// A strict time budget that runs out during a model call cancels the call
// and ends the process promptly: retryable, naming the budget, with a
// checkpoint.
func TestStrictTimeBudgetCancelsTheModelCall(t *testing.T) {
	k, store, process, _ := budgetFixture(t, capability.Set{Budget: capability.Budget{Seconds: 1, StrictTime: true}})
	var cause error
	backend := &funcBackend{fn: func(_ int, ctx context.Context, _ runtime.GenerateRequest) (runtime.GenerateResult, error) {
		<-ctx.Done()
		cause = context.Cause(ctx)
		return runtime.GenerateResult{}, ctx.Err()
	}}
	start := time.Now()
	result, err := (&runtime.AgentRuntime{Kernel: k, Artifacts: store, Backend: backend}).Run(context.Background(), process.PID, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	status := result.ExitStatus
	if status.Code != 1 || !status.Retryable || status.Origin != "" || status.Checkpoint == "" || !strings.HasPrefix(status.Reason, "time budget exceeded: 1s strict budget of worker (pid-000001)") {
		t.Fatalf("exit = %+v", status)
	}
	if !errors.Is(cause, runtime.ErrTimeBudgetExceeded) || time.Since(start) > 3*time.Second {
		t.Fatalf("the call should be cancelled by the budget promptly: cause %v after %s", cause, time.Since(start))
	}
	recorded, _ := k.Await(process.PID)
	if recorded != status {
		t.Fatalf("kernel record %+v", recorded)
	}
}

// The same during a run command: its whole process group is killed.
func TestStrictTimeBudgetKillsARunningCommand(t *testing.T) {
	k, store, process, workspace := budgetFixture(t, capability.Set{Tools: []string{"run"}, Budget: capability.Budget{Seconds: 1, StrictTime: true}})
	pidFile := filepath.Join(workspace, "sleep.pid")
	backend := &funcBackend{fn: func(call int, _ context.Context, _ runtime.GenerateRequest) (runtime.GenerateResult, error) {
		if call == 1 {
			return toolAction("run", map[string]string{"command": "sleep 30 & echo $! > " + pidFile + "; wait", "timeout": "5m"}), nil
		}
		return exitResult("should not get here"), nil
	}}
	toolRuntime := tools.NewRuntime()
	toolRuntime.ScratchRoot = filepath.Join(filepath.Dir(workspace), "scratch")
	start := time.Now()
	result, err := (&runtime.AgentRuntime{Kernel: k, Artifacts: store, Tools: toolRuntime, Backend: backend}).RunWithOptions(context.Background(), process.PID, nil, runtime.RunOptions{ContinueOnToolError: true})
	if err != nil || !strings.HasPrefix(result.ExitStatus.Reason, "time budget exceeded") || time.Since(start) > 5*time.Second {
		t.Fatalf("run = %+v, %v after %s", result.ExitStatus, err, time.Since(start))
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("the command never started: %v", err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	deadline := time.Now().Add(2 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("the command's child %d survived the budget", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Without strict_time the time budget only advises: the agent is told,
// once, to commit its answer, and is not stopped past the budget.
func TestAdvisoryTimeBudgetWarnsOnceAndNeverStops(t *testing.T) {
	k, store, process, _ := budgetFixture(t, capability.Set{Budget: capability.Budget{Seconds: 1}})
	var last []runtime.Message
	backend := &funcBackend{fn: func(call int, _ context.Context, request runtime.GenerateRequest) (runtime.GenerateResult, error) {
		last = request.History
		switch call {
		case 1:
			time.Sleep(1100 * time.Millisecond)
			return messageAction("working"), nil
		case 2, 3:
			return messageAction("still working"), nil
		}
		return exitResult("done late"), nil
	}}
	result, err := (&runtime.AgentRuntime{Kernel: k, Artifacts: store, Backend: backend, MaxSteps: 10}).Run(context.Background(), process.PID, nil)
	if err != nil || result.ExitStatus.Reason != "done late" {
		t.Fatalf("an advisory budget must not stop the agent: %+v %v", result.ExitStatus, err)
	}
	warnings, notes := 0, 0
	for _, message := range last {
		if strings.HasPrefix(message.Content, "budget warning: less than 10% of the time") {
			warnings++
		}
		if strings.HasPrefix(message.Content, "budget time=1s") {
			notes++
		}
	}
	if warnings != 1 || notes != 4 {
		t.Fatalf("warnings %d (want 1), time notes %d (want one per step)", warnings, notes)
	}
}

// The token budget is checked before each call and caps its reply; the
// agent is warned once near the end and stopped before a call that would
// start over budget.
func TestTokenBudgetCapsAndStops(t *testing.T) {
	k, store, process, _ := budgetFixture(t, capability.Set{Budget: capability.Budget{Tokens: 1000}})
	var caps []int
	var last []runtime.Message
	backend := &funcBackend{fn: func(_ int, ctx context.Context, request runtime.GenerateRequest) (runtime.GenerateResult, error) {
		info, _ := runtime.CallInfoFrom(ctx)
		caps = append(caps, info.MaxTokens)
		info.Meter.Add(300)
		last = request.History
		return messageAction("thinking"), nil
	}}
	result, err := (&runtime.AgentRuntime{Kernel: k, Artifacts: store, Backend: backend, MaxSteps: 20}).Run(context.Background(), process.PID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := result.ExitStatus.Reason; got != "token budget exceeded: 1200 of 1000 tokens used (budget of worker (pid-000001))" || !result.ExitStatus.Retryable || result.ExitStatus.Checkpoint == "" {
		t.Fatalf("exit = %+v", result.ExitStatus)
	}
	if stringsOf(caps) != "1000 700 400 100" {
		t.Fatalf("max_tokens caps = %v, want 1000 700 400 100", caps)
	}
	warnings := 0
	for _, message := range last {
		if strings.HasPrefix(message.Content, "budget warning: less than 10% of the tokens") {
			warnings++
		}
	}
	if warnings != 1 {
		t.Fatalf("token warnings = %d", warnings)
	}
	session, _ := (&runtime.AgentRuntime{Artifacts: store}).RestoreCheckpoint(result.ExitStatus.Checkpoint)
	if session.Usage.Tokens != 1200 || session.Usage.Steps != 5 {
		t.Fatalf("checkpointed usage = %+v", session.Usage)
	}
}

func stringsOf(values []int) string {
	parts := make([]string, len(values))
	for i, value := range values {
		parts[i] = strconv.Itoa(value)
	}
	return strings.Join(parts, " ")
}

// Steps are saved: a process that hit its step limit and is recovered gets
// no fresh steps unless the operator extends them.
func TestRecoverDoesNotResetTheStepLimit(t *testing.T) {
	k, store, process, _ := budgetFixture(t, capability.Set{})
	calls := 0
	backend := &funcBackend{fn: func(int, context.Context, runtime.GenerateRequest) (runtime.GenerateResult, error) {
		calls++
		return messageAction("again"), nil
	}}
	agent := &runtime.AgentRuntime{Kernel: k, Artifacts: store, Backend: backend, MaxSteps: 2}
	first, _ := agent.Run(context.Background(), process.PID, nil)
	if first.ExitStatus.Reason != "step limit exceeded" || calls != 2 {
		t.Fatalf("first = %+v after %d calls", first.ExitStatus, calls)
	}
	again, err := agent.Recover(context.Background(), process.PID, first.ExitStatus.Checkpoint, nil, runtime.RunOptions{})
	if err != nil || again.ExitStatus.Reason != "step limit exceeded" || calls != 2 {
		t.Fatalf("recovering without an extension made %d more calls: %+v %v", calls-2, again.ExitStatus, err)
	}
	extended, err := agent.Recover(context.Background(), again.PID, again.ExitStatus.Checkpoint, nil, runtime.RunOptions{Extend: runtime.Extension{Steps: 3}})
	if err != nil || calls != 5 {
		t.Fatalf("an extension of 3 steps made %d calls: %+v %v", calls-2, extended.ExitStatus, err)
	}
	session, _ := agent.RestoreCheckpoint(extended.ExitStatus.Checkpoint)
	if session.Usage.Steps != 5 || session.Extended.Steps != 3 {
		t.Fatalf("usage %+v extended %+v", session.Usage, session.Extended)
	}
}
