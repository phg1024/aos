package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"aos/internal/agentflow"
	"aos/internal/ipc"
	"aos/internal/kernel"
	"aos/internal/runtime"
)

// recordingCompleter answers with actions in order (then exits) and keeps
// every prompt it was given.
type recordingCompleter struct {
	actions []map[string]any
	prompts *[]string
}

func (c recordingCompleter) CompleteJSON(_ context.Context, _ string, user string, target any) error {
	*c.prompts = append(*c.prompts, user)
	action := map[string]any{"type": "exit", "exit_status": map[string]any{"code": 0, "reason": "done"}}
	if n := len(*c.prompts) - 1; n < len(c.actions) {
		action = c.actions[n]
	}
	bytes, _ := json.Marshal(map[string]any{"action": action})
	return json.Unmarshal(bytes, target)
}

const teamSpec = `{"name":"team","entry_agent":"lead","agents":[
	{"id":"lead","system_prompt":"Lead.","allowed_agents":["worker","asker"],"capabilities":{"tools":["spawn_agent","message_agent"]},"delegable":{"tools":["message_agent"]}},
	{"id":"worker","system_prompt":"Work.","capabilities":{"tools":["message_agent"]}},
	{"id":"asker","system_prompt":"Ask."}]}`

func runTeam(t *testing.T, actions []map[string]any) (string, []string, error) {
	t.Helper()
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	var prompts []string
	var err error
	captureStdout(t, func() {
		err = runWorkflow(context.Background(), recordingCompleter{actions: actions, prompts: &prompts}, runtime.CompactionPolicy{}, nil,
			writeSpec(t, dir, teamSpec), nil, "", "", stateDir, "go", false)
	})
	return stateDir, prompts, err
}

func exitAction(code int, reason string) map[string]any {
	return map[string]any{"type": "exit", "exit_status": map[string]any{"code": code, "reason": reason}}
}

func delegateTo(agent, goal string) map[string]any {
	return map[string]any{"type": "delegate", "delegate": map[string]any{"agent_id": agent, "goal": goal}}
}

// aos run carries mail: a worker subscribes and messages its lead, the
// lead reads it as a message from the worker, not from the user, and every
// message is acknowledged once checkpointed.
func TestWorkflowAgentsMessageEachOther(t *testing.T) {
	stateDir, prompts, err := runTeam(t, []map[string]any{
		delegateTo("worker", "report back"),
		{"type": "subscribe", "subscribe": map[string]any{"topic": "news"}},
		{"type": "send", "send": map[string]any{"to": "pid-000002", "body": "hello lead"}},
		exitAction(0, "reported"),
		{"type": "publish", "publish": map[string]any{"topic": "news", "body": "update"}},
		exitAction(0, "done"),
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	leadAfter := prompts[4]
	if !strings.Contains(leadAfter, runtime.PeerMessagePrefix+"worker (pid-000003) [msg-000001]: hello lead") {
		t.Fatalf("the lead should read the worker's message with its sender:\n%s", leadAfter)
	}
	bus, _ := ipc.New(filepath.Join(stateDir, "bus"), nil)
	if pending, _ := bus.Pending(); len(pending) != 0 {
		t.Fatalf("unacknowledged mail left: %v", pending)
	}
	k, _ := kernel.New(filepath.Join(stateDir, "kernel"))
	events, _ := k.Events()
	seen := map[string]int{}
	for _, event := range events {
		if strings.HasPrefix(event.Type, "ipc.") {
			seen[event.Type]++
		}
	}
	if seen["ipc.sent"] != 1 || seen["ipc.delivered"] != 1 || seen["ipc.acked"] != 1 {
		t.Fatalf("ipc events = %v", seen)
	}
}

// A delegated child that waits is parked, its lead sees the question,
// answers by delegating again, and the same process resumes and exits.
func TestWaitingChildIsAnsweredAndResumed(t *testing.T) {
	stateDir, prompts, err := runTeam(t, []map[string]any{
		delegateTo("asker", "pick a colour"),
		{"type": "wait", "message": "Which colour?"},
		delegateTo("asker", "blue"),
		exitAction(0, "chose blue"),
		exitAction(0, "done"),
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(prompts[2], "status=waiting") || !strings.Contains(prompts[2], "Which colour?") {
		t.Fatalf("the lead should see the child waiting and its question:\n%s", prompts[2])
	}
	if !strings.Contains(prompts[3], "Answer from lead (pid-000002), who delegated your task: blue") {
		t.Fatalf("the child should resume with the answer:\n%s", prompts[3])
	}
	k, _ := kernel.New(filepath.Join(stateDir, "kernel"))
	processes, _ := k.Processes()
	askers := 0
	for _, process := range processes {
		if process.Metadata["agent_id"] == "asker" {
			askers++
			if process.PID != "pid-000003" || process.ExitStatus == nil || process.ExitStatus.Code != 0 || process.ExitStatus.Reason != "chose blue" {
				t.Fatalf("asker = %+v %+v", process, process.ExitStatus)
			}
		}
	}
	if askers != 1 {
		t.Fatalf("the answer should resume the same process, found %d askers", askers)
	}
}

// A lead that exits without answering leaves no stray: the parked child is
// closed as abandoned, retryably, with its checkpoint.
func TestParkedChildIsAbandonedWhenItsLeadExits(t *testing.T) {
	stateDir, _, err := runTeam(t, []map[string]any{
		delegateTo("asker", "pick a colour"),
		{"type": "wait", "message": "Which colour?"},
		exitAction(0, "done without asking"),
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	k, _ := kernel.New(filepath.Join(stateDir, "kernel"))
	status, err := k.Await("pid-000003")
	if err != nil || status.Code != 1 || status.Reason != agentflow.AbandonedReason || !status.Retryable || status.Origin != "" || status.Checkpoint == "" {
		t.Fatalf("abandoned child = %+v, %v", status, err)
	}
	processes, _ := k.Processes()
	for _, process := range processes {
		if process.State != kernel.StateExited {
			t.Fatalf("%s left %s", process.PID, process.State)
		}
	}
}

// In a workflow nobody answers the entry agent: its wait fails the run and
// it is closed as abandoned rather than left blocked.
func TestWaitingEntryAgentFailsTheWorkflow(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	var prompts []string
	var err error
	captureStdout(t, func() {
		err = runWorkflow(context.Background(), recordingCompleter{prompts: &prompts, actions: []map[string]any{{"type": "wait", "message": "Which file?"}}},
			runtime.CompactionPolicy{}, nil, writeSoloWorkflow(t, dir), nil, "", "", stateDir, "go", false)
	})
	if err == nil || !strings.Contains(err.Error(), "abandoned while waiting") || !strings.Contains(err.Error(), "Which file?") {
		t.Fatalf("expected the abandoned entry to fail the workflow, got %v", err)
	}
	k, _ := kernel.New(filepath.Join(stateDir, "kernel"))
	if status, err := k.Await("pid-000002"); err != nil || !status.Retryable {
		t.Fatalf("entry = %+v, %v", status, err)
	}
}
