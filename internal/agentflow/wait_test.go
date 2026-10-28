package agentflow

import (
	"context"
	"path/filepath"
	"testing"

	"aos/internal/artifact"
	"aos/internal/kernel"
	"aos/internal/runtime"
)

// A wait keeps what the agent asked and its notes, in the strict schema and
// in loose shapes the generic parser accepts.
func TestParseKeepsWaitMessageAndNotes(t *testing.T) {
	for raw, want := range map[string]runtime.GenerateAction{
		`{"action":{"type":"wait","message":"Which file?","notes":"plan: ask"}}`: {Type: "wait", Message: "Which file?", Notes: "plan: ask"},
		`{"type":"wait","question":"Which file?","notes":"plan: ask"}`:           {Type: "wait", Message: "Which file?", Notes: "plan: ask"},
		`{"action":{"type":"wait"}}`:                                             {Type: "wait"},
	} {
		result, ok := parseGenerateResult(raw)
		if !ok || result.Action.Type != want.Type || result.Action.Message != want.Message || result.Action.Notes != want.Notes {
			t.Fatalf("%s parsed to %+v, %v", raw, result.Action, ok)
		}
	}
}

// A run that ends in a wait carries the question (or the notes) for the
// terminal to show, and records the question as the agent's reply.
func TestWaitTextShowsWhatTheAgentAsked(t *testing.T) {
	cases := []struct {
		action runtime.GenerateAction
		want   string
	}{
		{runtime.GenerateAction{Type: "wait", Message: "Which file should I edit?", Notes: "need a path"}, "Which file should I edit?"},
		{runtime.GenerateAction{Type: "wait", Notes: "need the target path from the user"}, "need the target path from the user"},
		{runtime.GenerateAction{Type: "wait"}, ""},
	}
	for _, c := range cases {
		root := t.TempDir()
		k, _ := kernel.New(filepath.Join(root, "kernel"))
		store, _ := artifact.New(filepath.Join(root, "artifacts"))
		process, _ := k.Spawn(kernel.ProcessSpec{Role: "interactive"})
		var shown []string
		agent := &runtime.AgentRuntime{Kernel: k, Artifacts: store, Backend: &runtime.FakeBackend{Steps: []runtime.GenerateResult{{Action: c.action}}}}
		result, err := agent.RunWithOptions(context.Background(), process.PID, nil, runtime.RunOptions{
			OnAssistantMessage: func(message string) { shown = append(shown, message) },
		})
		if err != runtime.ErrAgentBlocked || !result.Blocked {
			t.Fatalf("%+v: expected a blocked run, got %+v %v", c.action, result, err)
		}
		if got := runtime.WaitText(result.Session.Messages); got != c.want {
			t.Fatalf("%+v: WaitText = %q, want %q", c.action, got, c.want)
		}
		if (c.action.Message != "") != (len(shown) == 1) {
			t.Fatalf("%+v: assistant messages %v", c.action, shown)
		}
	}
}
