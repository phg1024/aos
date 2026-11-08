package agentflow_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"aos/internal/agentflow"
	"aos/internal/kernel"
	"aos/internal/runtime"
)

// transcriptReplies answers a step's call with first and its repair with
// repair.
type transcriptReplies struct {
	first, repair string
	calls         int
}

func (r *transcriptReplies) CompleteJSON(context.Context, string, string, any) error {
	return errors.New("not used")
}

func (r *transcriptReplies) CompleteMessages(context.Context, []runtime.Message) (string, error) {
	r.calls++
	if r.calls == 1 {
		return r.first, nil
	}
	return r.repair, nil
}

// ResolveRecordedReply reads a recorded reply and repair to the action a
// step took from them, naming the rung, and ReadReply names how the first
// parse found and read the object.
func TestResolveRecordedReplyMatchesAStep(t *testing.T) {
	cases := []struct {
		name, first, repair string
		extraction, shape   string
		source              string
	}{
		{"clean", `{"action":{"type":"message","message":"hi"}}`, "", agentflow.ExtractedAsSent, agentflow.ShapeNested, agentflow.SourceReply},
		{"after prose", `Sure: {"action":{"type":"exit","exit_status":{"code":0,"reason":"done"}}}`, "", agentflow.ExtractedTrailing, agentflow.ShapeNested, agentflow.SourceReply},
		{"bare keys", `{action:{type:"message",message:"hi"}}`, "", agentflow.ExtractedLoose, agentflow.ShapeNested, agentflow.SourceReply},
		{"generic", `{"action":{"type":"tool","tool":"read_file","path":"a.txt"}}`, "", agentflow.ExtractedAsSent, agentflow.ShapeGeneric, agentflow.SourceReply},
		{"flat refused by generic", `{"type":"delegate","delegate":{"goal":"g"}}`, "", agentflow.ExtractedAsSent, agentflow.ShapeFlat, agentflow.SourceReply},
		{"repair", `I will read {read_file a.txt}`, `{"action":{"type":"message","message":"fixed"}}`, agentflow.ExtractedAsSent, "", agentflow.SourceRepair},
		{"fallbackMessage", "All done.", "Done.", agentflow.ExtractedAsSent, "", agentflow.SourceFallbackMessage},
		{"notesAsMessage", `{"action":{"type":"message","notes":"n","message":""}}`, `{"action":{"type":"message","notes":"n","message":""}}`, agentflow.ExtractedAsSent, agentflow.ShapeGeneric, agentflow.SourceNotesAsMessage},
		{"unrepairable", `{"action": {"type": "tool"`, `{"action": {"type": "tool"`, agentflow.ExtractedAsSent, "", agentflow.SourceUnrepairable},
	}
	for _, c := range cases {
		replies := &transcriptReplies{first: c.first, repair: c.repair}
		backend := agentflow.PromptBackend{Completer: replies, Workflow: agentflow.WorkflowSpec{Name: "w", EntryAgent: "a"}, Agent: agentflow.AgentSpec{ID: "a", SystemPrompt: "Work."}}
		stepResult, stepErr := backend.Generate(context.Background(), runtime.GenerateRequest{Process: kernel.Process{PID: "pid-1", Role: "a"}})
		result, source, err := agentflow.ResolveRecordedReply(c.first, c.repair, replies.calls > 1)
		if source != c.source || !reflect.DeepEqual(result, stepResult) || (err == nil) != (stepErr == nil) {
			t.Fatalf("%s: resolved %q %+v (%v), the step took %+v (%v)", c.name, source, result, err, stepResult, stepErr)
		}
		reading := agentflow.ReadReply(c.first)
		if reading.Extraction != c.extraction || reading.Shape != c.shape {
			t.Fatalf("%s: read %q/%q, want %q/%q", c.name, reading.Extraction, reading.Shape, c.extraction, c.shape)
		}
	}
	if _, _, err := agentflow.ResolveRecordedReply("prose", "", false); err == nil {
		t.Fatal("a reply that needed a repair, with none recorded, should not resolve")
	}
}
