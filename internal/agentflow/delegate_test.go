package agentflow_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aos/internal/agentflow"
	"aos/internal/artifact"
	"aos/internal/capability"
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
			{ID: "coordinator", SystemPrompt: "Delegate.", AllowedAgents: []string{"writer"}, Capabilities: capability.Set{Tools: []string{"spawn_agent"}}},
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

	process, err := k.Spawn(kernel.ProcessSpec{Role: "agent:coordinator", Metadata: map[string]string{"agent_id": "coordinator"}, CapabilitySet: capability.Set{Tools: []string{"spawn_agent"}}})
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

// funcCompleter answers each completion with fn(call number, ctx).
type funcCompleter struct {
	calls int
	fn    func(call int, ctx context.Context) (map[string]any, error)
}

func (f *funcCompleter) CompleteJSON(ctx context.Context, _ string, _ string, target any) error {
	f.calls++
	action, err := f.fn(f.calls, ctx)
	if err != nil {
		return err
	}
	bytes, err := json.Marshal(map[string]any{"action": action})
	if err != nil {
		return err
	}
	return json.Unmarshal(bytes, target)
}

// Cancelling while a delegated agent works stops both levels, leaves the
// parent resumable, and closes the child so the agent can be delegated again.
func TestEngineDelegateHandlerCancelLeavesParentResumable(t *testing.T) {
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
		Name:       "interactive-cancel",
		EntryAgent: "coordinator",
		Agents: []agentflow.AgentSpec{
			{ID: "coordinator", SystemPrompt: "Delegate.", AllowedAgents: []string{"writer"}, Capabilities: capability.Set{Tools: []string{"spawn_agent"}}},
			{ID: "writer", SystemPrompt: "Write."},
		},
	}
	coordinator, _ := spec.Agent("coordinator")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	completer := &funcCompleter{fn: func(call int, ctx context.Context) (map[string]any, error) {
		switch call {
		case 1, 3: // coordinator
			return delegateAction("writer"), nil
		case 2: // writer, interrupted mid-generation
			cancel()
			<-ctx.Done()
			return nil, ctx.Err()
		case 4: // writer after resume
			return exitAction("writer-done"), nil
		default: // coordinator
			return exitAction("coordinator-done"), nil
		}
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
	process, err := k.Spawn(kernel.ProcessSpec{Role: "agent:coordinator", Metadata: map[string]string{"agent_id": "coordinator"}, CapabilitySet: capability.Set{Tools: []string{"spawn_agent"}}})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	agentRuntime := &runtime.AgentRuntime{
		Kernel:          k,
		Artifacts:       store,
		Tools:           engine.Tools,
		MaxSteps:        6,
		Backend:         agentflow.PromptBackend{Completer: completer, Workflow: spec, Agent: coordinator},
		DelegateHandler: engine.DelegateHandler(spec, agentflow.RunRequest{Goal: "interactive"}, coordinator, nil),
	}
	options := runtime.RunOptions{ContinueOnToolError: true}

	result, err := agentRuntime.RunWithOptions(ctx, process.PID, []runtime.Message{{Role: "user", Content: "go"}}, options)
	if !errors.Is(err, runtime.ErrRunCancelled) || !result.Cancelled {
		t.Fatalf("expected a cancelled parent run, got result=%+v err=%v", result, err)
	}
	children, err := k.Children(process.PID)
	if err != nil || len(children) != 1 {
		t.Fatalf("expected one child, got %v (err %v)", children, err)
	}
	if children[0].State != kernel.StateExited || children[0].ExitStatus == nil || children[0].ExitStatus.Code != 130 {
		t.Fatalf("cancelled child should be exited with code 130, got %+v", children[0])
	}
	if strings.Join(kinds, ",") != "coordinator:delegate,writer:start,writer:cancelled" {
		t.Fatalf("unexpected trace %v", kinds)
	}
	last := result.Session.Messages[len(result.Session.Messages)-1]
	if last.Content != "delegate_cancelled agent=writer reason=interrupted_by_user" {
		t.Fatalf("expected delegate_cancelled note, got %q", last.Content)
	}

	resumed, err := agentRuntime.Resume(context.Background(), process.PID, result.CheckpointID, nil, options)
	if err != nil {
		t.Fatalf("resume after cancel: %v", err)
	}
	if resumed.ExitStatus.Reason != "coordinator-done" {
		t.Fatalf("unexpected resumed exit %+v", resumed.ExitStatus)
	}
}

// promptCapture answers from a script and records each prompt it was sent.
type promptCapture struct {
	actions []map[string]any
	prompts []string
}

func (p *promptCapture) CompleteJSON(_ context.Context, system string, user string, target any) error {
	p.prompts = append(p.prompts, system+"\n"+user)
	if len(p.prompts) > len(p.actions) {
		return errors.New("script exhausted")
	}
	bytes, err := json.Marshal(map[string]any{"action": p.actions[len(p.prompts)-1]})
	if err != nil {
		return err
	}
	return json.Unmarshal(bytes, target)
}

func TestEngineSharesClockWithDelegatedAgents(t *testing.T) {
	root := t.TempDir()
	k, err := kernel.New(filepath.Join(root, "kernel"))
	if err != nil {
		t.Fatalf("new kernel: %v", err)
	}
	store, err := artifact.New(filepath.Join(root, "artifacts"))
	if err != nil {
		t.Fatalf("new artifact store: %v", err)
	}
	completer := &promptCapture{actions: []map[string]any{delegateAction("writer"), exitAction("writer-done"), exitAction("coordinator-done")}}
	engine := &agentflow.Engine{
		Kernel:    k,
		Artifacts: store,
		Tools:     tools.NewRuntime(),
		Factory:   agentflow.StaticBackendFactory{Completer: completer},
		Clock:     runtime.NewClock(15 * time.Minute),
	}
	spec := agentflow.WorkflowSpec{
		Name:       "clocked",
		EntryAgent: "coordinator",
		Agents: []agentflow.AgentSpec{
			{ID: "coordinator", SystemPrompt: "Delegate.", AllowedAgents: []string{"writer"}, Capabilities: capability.Set{Tools: []string{"spawn_agent"}}},
			{ID: "writer", SystemPrompt: "Write."},
		},
	}
	if _, err := engine.Run(context.Background(), spec, agentflow.RunRequest{Goal: "go"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(completer.prompts) != 3 {
		t.Fatalf("expected 3 prompts, got %d", len(completer.prompts))
	}
	for idx, prompt := range completer.prompts {
		if !strings.Contains(prompt, "clock time_budget=15m0s") {
			t.Fatalf("prompt %d (writer is #1) has no clock note", idx)
		}
	}
}
