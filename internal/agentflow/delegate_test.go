package agentflow_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"aos/internal/agentflow"
	"aos/internal/artifact"
	"aos/internal/kernel"
	"aos/internal/runtime"
	"aos/internal/tools"
)

// sequenceCompleter returns one scripted action per completion, in order.
type sequenceCompleter struct {
	actions []map[string]any
	calls   int
}

func (s *sequenceCompleter) CompleteJSON(_ context.Context, _ string, _ string, target any) error {
	if s.calls >= len(s.actions) {
		return errors.New("script exhausted")
	}
	payload := map[string]any{"action": s.actions[s.calls]}
	s.calls++
	bytes, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return json.Unmarshal(bytes, target)
}

func delegateAction(agentID string) map[string]any {
	return map[string]any{"type": "delegate", "delegate": map[string]any{"agent_id": agentID, "goal": "do the " + agentID + " part"}}
}

func exitAction(reason string) map[string]any {
	return map[string]any{"type": "exit", "exit_status": map[string]any{"code": 0, "reason": reason}}
}

// An agent run outside Engine.Run (as the interactive TUI does) delegates
// through Engine.DelegateHandler and sees the same trace events.
func TestEngineDelegateHandlerServesExternalRuntime(t *testing.T) {
	root := t.TempDir()
	k, err := kernel.New(filepath.Join(root, "kernel"))
	if err != nil {
		t.Fatalf("new kernel: %v", err)
	}
	store, err := artifact.New(filepath.Join(root, "artifacts"))
	if err != nil {
		t.Fatalf("new artifact store: %v", err)
	}
	spec := agentflow.WorkflowSpec{
		Name:       "interactive-delegation",
		EntryAgent: "coordinator",
		Agents: []agentflow.AgentSpec{
			{ID: "coordinator", SystemPrompt: "Delegate.", AllowedAgents: []string{"writer"}},
			{ID: "writer", SystemPrompt: "Write."},
		},
	}
	coordinator, _ := spec.Agent("coordinator")
	completer := &sequenceCompleter{actions: []map[string]any{
		delegateAction("writer"),
		exitAction("writer-done"),
		exitAction("coordinator-done"),
	}}
	var kinds []string
	engine := &agentflow.Engine{
		Kernel:    k,
		Artifacts: store,
		Tools:     tools.NewRuntime(),
		Factory:   agentflow.StaticBackendFactory{Completer: completer},
		Trace: func(event agentflow.TraceEvent) {
			kinds = append(kinds, event.AgentID+":"+event.Kind)
		},
	}

	process, err := k.Spawn(kernel.ProcessSpec{Role: "agent:coordinator", Metadata: map[string]string{"agent_id": "coordinator"}})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	var children []agentflow.AgentRun
	agentRuntime := &runtime.AgentRuntime{
		Kernel:    k,
		Artifacts: store,
		Tools:     engine.Tools,
		MaxSteps:  6,
		Backend:   agentflow.PromptBackend{Completer: completer, Workflow: spec, Agent: coordinator},
		DelegateHandler: engine.DelegateHandler(spec, agentflow.RunRequest{Goal: "interactive"}, coordinator, func(run agentflow.AgentRun) {
			children = append(children, run)
		}),
	}
	result, err := agentRuntime.RunWithOptions(context.Background(), process.PID, []runtime.Message{{Role: "user", Content: "go"}}, runtime.RunOptions{ContinueOnToolError: true})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.ExitStatus.Reason != "coordinator-done" {
		t.Fatalf("unexpected exit %+v", result.ExitStatus)
	}
	if len(children) != 1 || children[0].AgentID != "writer" || children[0].ExitStatus.Reason != "writer-done" {
		t.Fatalf("expected one finished writer child, got %+v", children)
	}
	if children[0].ParentPID != process.PID {
		t.Fatalf("child parent pid = %s, want %s", children[0].ParentPID, process.PID)
	}
	want := []string{"coordinator:delegate", "writer:start", "writer:exit", "coordinator:delegate_result"}
	if len(kinds) != len(want) {
		t.Fatalf("trace kinds = %v, want %v", kinds, want)
	}
	for idx := range want {
		if kinds[idx] != want[idx] {
			t.Fatalf("trace kinds = %v, want %v", kinds, want)
		}
	}

	denied := engine.DelegateHandler(spec, agentflow.RunRequest{}, coordinator, nil)
	if _, err := denied(context.Background(), process, runtime.DelegateCall{AgentID: "benchmarker"}, runtime.Session{}); !errors.Is(err, agentflow.ErrDelegationDenied) {
		t.Fatalf("expected ErrDelegationDenied for an agent outside allowed_agents, got %v", err)
	}
}
