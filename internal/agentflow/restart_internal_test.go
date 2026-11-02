package agentflow

import (
	"context"
	"path/filepath"
	"testing"

	"aos/internal/artifact"
	"aos/internal/capability"
	"aos/internal/kernel"
	"aos/internal/runtime"
	"aos/internal/supervisor"
	"aos/internal/tools"
)

type fakeFactory map[string]runtime.Backend

func (f fakeFactory) BackendFor(agent AgentSpec, _ WorkflowSpec) (runtime.Backend, error) {
	return f[agent.ID], nil
}

// A fresh engine on the same state directory continues the count: the
// window is read from the recorded restarts, not from the engine that
// made them.
func TestRestartCountSurvivesANewEngine(t *testing.T) {
	root := t.TempDir()
	open := func() (*kernel.Kernel, *artifact.Store) {
		k, err := kernel.New(filepath.Join(root, "kernel"))
		if err != nil {
			t.Fatal(err)
		}
		store, err := artifact.New(filepath.Join(root, "artifacts"))
		if err != nil {
			t.Fatal(err)
		}
		return k, store
	}
	policy := &supervisor.RestartPolicy{Max: 1, Within: "1h", Backoff: "0s"}
	spec := WorkflowSpec{Name: "restarts", EntryAgent: "coordinator", Agents: []AgentSpec{
		{ID: "coordinator", SystemPrompt: "Delegate.", AllowedAgents: []string{"worker"}, Capabilities: capability.Set{Tools: []string{"spawn_agent"}}},
		{ID: "worker", SystemPrompt: "Work.", Restart: policy},
	}}
	exit := func(code int, retryable bool, reason string) runtime.GenerateResult {
		return runtime.GenerateResult{Action: runtime.GenerateAction{Type: "exit", ExitStatus: &kernel.ExitStatus{Code: code, Reason: reason, Retryable: retryable}}}
	}
	// First engine: the worker fails, is restarted once, and the restarted
	// worker waits; the coordinator ends, so the worker is left abandoned
	// (a retryable exit with a checkpoint).
	k, store := open()
	first := Engine{Kernel: k, Artifacts: store, Tools: tools.NewRuntime(), Factory: fakeFactory{
		"coordinator": &runtime.FakeBackend{Steps: []runtime.GenerateResult{
			{Action: runtime.GenerateAction{Type: "delegate", Delegate: &runtime.DelegateCall{AgentID: "worker", Goal: "work"}}},
			exit(0, false, "coordinated"),
		}},
		"worker": &runtime.FakeBackend{Steps: []runtime.GenerateResult{exit(1, true, "flaky"), {Action: runtime.GenerateAction{Type: "wait", Message: "which way?"}}}},
	}}
	if _, err := first.Run(context.Background(), spec, RunRequest{Goal: "go"}); err != nil {
		t.Fatal(err)
	}
	var abandoned kernel.Process
	processes, _ := k.Processes()
	for _, process := range processes {
		if process.Role == "agent:worker" && process.SupersededBy == "" {
			abandoned = process
		}
	}
	if abandoned.ExitStatus == nil || !abandoned.ExitStatus.Retryable || abandoned.ExitStatus.Checkpoint == "" || abandoned.Metadata["resumed_from"] == "" {
		t.Fatalf("the restarted worker = %+v", abandoned)
	}

	// Second engine, freshly opened: its restart of the same chain would
	// be the second within the hour, past max 1.
	k, store = open()
	worker := &runtime.FakeBackend{Steps: []runtime.GenerateResult{exit(0, false, "would succeed")}}
	second := Engine{Kernel: k, Artifacts: store, Tools: tools.NewRuntime(), Factory: fakeFactory{"worker": worker}}
	agent, _ := spec.Agent("worker")
	run, err := second.supervise(context.Background(), spec, RunRequest{Goal: "go"}, agent, abandoned.ParentPID,
		AgentRun{AgentID: "worker", PID: abandoned.PID, ExitStatus: *abandoned.ExitStatus}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if run.ExitStatus.Retryable || len(worker.Requests) != 0 {
		t.Fatalf("the second engine restarted past the intensity: %+v (%d calls)", run.ExitStatus, len(worker.Requests))
	}
}
