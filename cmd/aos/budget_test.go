package main

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"aos/internal/agentflow"
	"aos/internal/capability"
	"aos/internal/inspect"
	"aos/internal/kernel"
	"aos/internal/runtime"
)

// meteredCompleter answers like recordingCompleter and reports 300 tokens
// per call, as a server would.
type meteredCompleter struct{ recordingCompleter }

func (c meteredCompleter) CompleteJSON(ctx context.Context, system, user string, target any) error {
	if info, ok := runtime.CallInfoFrom(ctx); ok && info.Meter != nil {
		info.Meter.Add(300)
	}
	return c.recordingCompleter.CompleteJSON(ctx, system, user, target)
}

const budgetSpec = `{"name":"budgeted","entry_agent":"lead","agents":[
	{"id":"lead","system_prompt":"Lead.","allowed_agents":["worker"],"capabilities":{"tools":["spawn_agent"],"budget":{"tokens":5000}}},
	{"id":"worker","system_prompt":"Work.","max_steps":20}]}`

// A delegate's tokens count against its lead and the run, and whichever
// budget runs out first stops it; inspect shows the budgets and which one
// ended each process.
func TestDelegateTokensCountAgainstTheRunBudget(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	var prompts []string
	keepWorking := map[string]any{"type": "message", "message": "working"}
	completer := meteredCompleter{recordingCompleter{prompts: &prompts, actions: []map[string]any{
		delegateTo("worker", "work"), keepWorking, keepWorking, keepWorking, keepWorking, keepWorking,
	}}}
	var err error
	captureStdout(t, func() {
		err = runWorkflow(context.Background(), completer, runtime.CompactionPolicy{}, capability.Budget{Tokens: 1000}, writeSpec(t, dir, budgetSpec), nil, "", "", stateDir, "go", false)
	})
	var failed workflowFailed
	if !errors.As(err, &failed) || !strings.Contains(err.Error(), "token budget exceeded") {
		t.Fatalf("expected the run budget to end the workflow, got %v", err)
	}
	// lead 300, worker 300+300+300 (1200 > 1000), then both are stopped.
	if len(prompts) != 4 {
		t.Fatalf("model calls = %d, want 4", len(prompts))
	}
	k, _ := kernel.New(filepath.Join(stateDir, "kernel"))
	worker, _ := k.Await("pid-000003")
	lead, _ := k.Await("pid-000002")
	for name, status := range map[string]kernel.ExitStatus{"worker": worker, "lead": lead} {
		if status.Reason != "token budget exceeded: 1200 of 1000 tokens used (budget of run)" || !status.Retryable {
			t.Fatalf("%s exit = %+v", name, status)
		}
	}
	report, _ := inspect.Load(stateDir, 0)
	usage := map[string]runtime.Usage{}
	for _, process := range report.Processes {
		usage[process.AgentID] = process.Usage
	}
	if usage["lead"].Tokens != 1200 || usage["worker"].Tokens != 900 || report.Processes[1].Budget.Tokens != 5000 {
		t.Fatalf("usage = %+v, lead budget %+v", usage, report.Processes[1].Budget)
	}
	var text strings.Builder
	report.WriteText(&text)
	if !strings.Contains(text.String(), "tokens 1200/5000") || !strings.Contains(text.String(), `"token budget exceeded: 1200 of 1000 tokens used (budget of run)"`) {
		t.Fatalf("inspect text:\n%s", text.String())
	}
	encoded, _ := json.Marshal(report.Processes[2])
	if !strings.Contains(string(encoded), `"budget":{"tokens":5000},"usage":{"steps":4,"tokens":900`) {
		t.Fatalf("inspect json: %s", encoded)
	}
}

// A spec whose child asks for more budget than its parent may grant is
// rejected before anything runs.
func TestOverBudgetChildIsRejectedAtValidation(t *testing.T) {
	dir := t.TempDir()
	spec := strings.Replace(budgetSpec, `"max_steps":20}`, `"max_steps":20,"capabilities":{"budget":{"tokens":9000}}}`, 1)
	var prompts []string
	err := runWorkflow(context.Background(), recordingCompleter{prompts: &prompts}, runtime.CompactionPolicy{}, capability.Budget{}, writeSpec(t, dir, spec), nil, "", "", filepath.Join(dir, "state"), "go", false)
	if !errors.Is(err, agentflow.ErrWorkflowSpecInvalid) || !strings.Contains(err.Error(), "budget tokens 9000 (at most 5000)") || len(prompts) != 0 {
		t.Fatalf("expected a validation error, got %v after %d calls", err, len(prompts))
	}
}
