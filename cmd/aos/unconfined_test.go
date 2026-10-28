package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aos/internal/agentflow"
	"aos/internal/capability"
	"aos/internal/inspect"
	"aos/internal/runtime"
)

// sequenceCompleter answers with actions in order, then exits.
type sequenceCompleter struct {
	actions []map[string]any
	next    *int
}

func (c sequenceCompleter) CompleteJSON(_ context.Context, _ string, _ string, target any) error {
	action := map[string]any{"type": "exit", "exit_status": map[string]any{"code": 0, "reason": "done"}}
	if *c.next < len(c.actions) {
		action = c.actions[*c.next]
	}
	*c.next++
	bytes, _ := json.Marshal(map[string]any{"action": action})
	return json.Unmarshal(bytes, target)
}

func writeSpec(t *testing.T, dir, spec string) string {
	t.Helper()
	path := filepath.Join(dir, "workflow.json")
	if err := os.WriteFile(path, []byte(spec), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// unconfined_run is granted like any capability: a child cannot hold it
// unless its parent may grant it, and when it does, inspect and the HTML
// report say so.
func TestUnconfinedRunIsAttenuatedAndVisible(t *testing.T) {
	dir := t.TempDir()
	refused := `{"name":"w","entry_agent":"lead","agents":[
		{"id":"lead","system_prompt":"Lead.","allowed_agents":["worker"],"capabilities":{"tools":["spawn_agent"]},"delegable":{"tools":["run"]}},
		{"id":"worker","system_prompt":"Work.","capabilities":{"tools":["run"],"unconfined_run":true}}]}`
	next := 0
	err := runWorkflow(context.Background(), sequenceCompleter{next: &next}, runtime.CompactionPolicy{}, capability.Budget{}, writeSpec(t, dir, refused), nil, "", "", filepath.Join(dir, "refused"), "go", false)
	if !errors.Is(err, agentflow.ErrWorkflowSpecInvalid) || !strings.Contains(err.Error(), "unconfined_run") {
		t.Fatalf("a worker asking for unconfined_run its lead cannot grant: %v", err)
	}

	granted := strings.Replace(refused, `"delegable":{"tools":["run"]}`, `"delegable":{"tools":["run"],"unconfined_run":true}`, 1)
	stateDir, report := filepath.Join(dir, "state"), filepath.Join(dir, "report.html")
	next = 0
	completer := sequenceCompleter{next: &next, actions: []map[string]any{{"type": "delegate", "delegate": map[string]any{"agent_id": "worker", "goal": "build"}}}}
	captureStdout(t, func() {
		if err := runWorkflow(context.Background(), completer, runtime.CompactionPolicy{}, capability.Budget{}, writeSpec(t, dir, granted), nil, "", report, stateDir, "go", false); err != nil {
			t.Errorf("run: %v", err)
		}
	})
	loaded, err := inspect.Load(stateDir, 0)
	if err != nil {
		t.Fatal(err)
	}
	var marked []string
	for _, process := range loaded.Processes {
		if process.UnconfinedRun {
			marked = append(marked, process.AgentID)
		}
	}
	if strings.Join(marked, ",") != "worker" {
		t.Fatalf("unconfined processes = %v", marked)
	}
	html, _ := os.ReadFile(report)
	if !strings.Contains(string(html), "agent:worker (UNCONFINED run)") {
		t.Fatal("the HTML report should mark the unconfined worker")
	}
}
