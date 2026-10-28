package agentflow_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"aos/internal/agentflow"
	"aos/internal/artifact"
	"aos/internal/capability"
	"aos/internal/kernel"
	"aos/internal/runtime"
	"aos/internal/tools"
)

func exitWith(code int, reason string, retryable bool) map[string]any {
	return map[string]any{"type": "exit", "exit_status": map[string]any{"code": code, "reason": reason, "retryable": retryable}}
}

func messageAction(text string) map[string]any {
	return map[string]any{"type": "message", "message": text}
}

type exitFixture struct {
	kernel *kernel.Kernel
	store  *artifact.Store
	engine *agentflow.Engine
}

func newExitFixture(t *testing.T, completer agentflow.JSONCompleter) exitFixture {
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
	return exitFixture{kernel: k, store: store, engine: &agentflow.Engine{
		Kernel:    k,
		Artifacts: store,
		Tools:     tools.NewRuntime(),
		Factory:   agentflow.StaticBackendFactory{Completer: completer},
	}}
}

func soloSpec(maxSteps int) agentflow.WorkflowSpec {
	return agentflow.WorkflowSpec{
		Name:       "solo",
		EntryAgent: "solver",
		Agents:     []agentflow.AgentSpec{{ID: "solver", SystemPrompt: "Solve.", MaxSteps: maxSteps}},
	}
}

func (f exitFixture) rootStatus(t *testing.T, summary agentflow.RunSummary) kernel.ExitStatus {
	t.Helper()
	status, err := f.kernel.Await(summary.RootPID)
	if err != nil {
		t.Fatalf("await root: %v", err)
	}
	if status != summary.ExitStatus {
		t.Fatalf("RunSummary.ExitStatus %+v differs from the recorded root exit %+v", summary.ExitStatus, status)
	}
	return status
}

func TestWorkflowRootSucceedsWhenEntrySucceeds(t *testing.T) {
	f := newExitFixture(t, &sequenceCompleter{actions: []map[string]any{exitWith(0, "done", false)}})
	summary, err := f.engine.Run(context.Background(), soloSpec(4), agentflow.RunRequest{Goal: "go"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if status := f.rootStatus(t, summary); status != (kernel.ExitStatus{Code: 0, Reason: "workflow completed"}) {
		t.Fatalf("unexpected root exit %+v", status)
	}
}

func TestWorkflowRootFailsWhenEntryExitsNonZero(t *testing.T) {
	f := newExitFixture(t, &sequenceCompleter{actions: []map[string]any{exitWith(3, "tests failed", true)}})
	summary, err := f.engine.Run(context.Background(), soloSpec(4), agentflow.RunRequest{Goal: "go"})
	if err != nil {
		t.Fatalf("a failed entry agent is an outcome, not a Go error: %v", err)
	}
	want := kernel.ExitStatus{Code: 3, Reason: "entry agent solver exited with code 3: tests failed", Retryable: true}
	if status := f.rootStatus(t, summary); status != want {
		t.Fatalf("root exit = %+v, want %+v", status, want)
	}
}

func TestWorkflowRootFailsWhenEntryHitsStepLimit(t *testing.T) {
	f := newExitFixture(t, &sequenceCompleter{actions: []map[string]any{messageAction("thinking"), messageAction("still thinking")}})
	summary, err := f.engine.Run(context.Background(), soloSpec(2), agentflow.RunRequest{Goal: "go"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	status := f.rootStatus(t, summary)
	if status.Code != 1 || !status.Retryable || !strings.Contains(status.Reason, "step limit exceeded") {
		t.Fatalf("step-limited entry should fail the workflow, got %+v", status)
	}
}

// A child's exit reaches its parent as structured data: in DelegateResult,
// in the parent's history, and in the delegate_result artifact.
func TestDelegatedChildExitReachesParent(t *testing.T) {
	completer := &promptCapture{actions: []map[string]any{
		delegateAction("verifier"),
		exitWith(1, "FAIL: 2 of 5 checks failed", false),
		exitWith(0, "reported", false),
	}}
	f := newExitFixture(t, completer)
	spec := agentflow.WorkflowSpec{
		Name:       "verified",
		EntryAgent: "coordinator",
		Agents: []agentflow.AgentSpec{
			{ID: "coordinator", SystemPrompt: "Delegate.", AllowedAgents: []string{"verifier"}, Capabilities: capability.Set{Tools: []string{"spawn_agent"}}},
			{ID: "verifier", SystemPrompt: "Verify."},
		},
	}
	summary, err := f.engine.Run(context.Background(), spec, agentflow.RunRequest{Goal: "go"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(completer.prompts) != 3 {
		t.Fatalf("expected 3 prompts, got %d", len(completer.prompts))
	}
	// The prompt carries history as JSON, so the reason's quotes are escaped.
	if after := completer.prompts[2]; !strings.Contains(after, `delegate_result agent=verifier`) ||
		!strings.Contains(after, `exit_code=1 exit_reason=\"FAIL: 2 of 5 checks failed\" exit_retryable=false`) {
		t.Fatalf("parent history after delegation lacks the child's exit fields:\n%s", after)
	}

	artifacts, err := f.store.List()
	if err != nil {
		t.Fatalf("list artifacts: %v", err)
	}
	found := false
	for _, record := range artifacts {
		if record.Type != "delegate_result" {
			continue
		}
		var result runtime.DelegateResult
		if err := f.store.ReadJSON(record.ID, &result); err != nil {
			t.Fatalf("read delegate_result: %v", err)
		}
		if result.ExitStatus != (kernel.ExitStatus{Code: 1, Reason: "FAIL: 2 of 5 checks failed"}) {
			t.Fatalf("delegate_result artifact exit = %+v", result.ExitStatus)
		}
		found = true
	}
	if !found {
		t.Fatal("no delegate_result artifact was recorded")
	}

	// The coordinator chose to exit 0 after a failed verifier; gating that is
	// a separate change, so the root follows the entry agent.
	if summary.ExitStatus.Code != 0 {
		t.Fatalf("root should follow the entry agent's exit, got %+v", summary.ExitStatus)
	}

	direct := f.engine.DelegateHandler(spec, agentflow.RunRequest{Goal: "go"}, spec.Agents[0], nil)
	parent, err := f.kernel.Spawn(kernel.ProcessSpec{Role: "agent:coordinator", Metadata: map[string]string{"agent_id": "coordinator"}})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	completer.actions = append(completer.actions, exitWith(2, "flaky check", true))
	result, err := direct(context.Background(), parent, runtime.DelegateCall{AgentID: "verifier", Goal: "again"}, runtime.Session{})
	if err != nil {
		t.Fatalf("delegate: %v", err)
	}
	if result.ExitStatus != (kernel.ExitStatus{Code: 2, Reason: "flaky check", Retryable: true}) {
		t.Fatalf("DelegateResult.ExitStatus = %+v", result.ExitStatus)
	}
}
