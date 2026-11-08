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

// A ReplyReader reads a recorded reply and repair to the action a step
// took from them, naming the rung, and Read names how the first parse found
// and read the object, with the reply checks and without them (records
// from before reply_checks). The checked columns are where they differ.
func TestResolveRecordedReplyMatchesAStep(t *testing.T) {
	type reading struct{ extraction, shape, source string }
	cases := []struct {
		name, first, repair string
		before, checked     reading // checked zero: as before
	}{
		{"clean", `{"action":{"type":"message","message":"hi"}}`, "", reading{agentflow.ExtractedAsSent, agentflow.ShapeNested, agentflow.SourceReply}, reading{}},
		{"after prose", `Sure: {"action":{"type":"exit","exit_status":{"code":0,"reason":"done"}}}`, "", reading{agentflow.ExtractedTrailing, agentflow.ShapeNested, agentflow.SourceReply}, reading{}},
		{"bare keys", `{action:{type:"message",message:"hi"}}`, "", reading{agentflow.ExtractedLoose, agentflow.ShapeNested, agentflow.SourceReply}, reading{}},
		{"generic", `{"action":{"type":"tool","tool":"read_file","path":"a.txt"}}`, "", reading{agentflow.ExtractedAsSent, agentflow.ShapeGeneric, agentflow.SourceReply}, reading{}},
		{"flat refused by generic", `{"type":"delegate","delegate":{"goal":"g"}}`, "",
			reading{agentflow.ExtractedAsSent, agentflow.ShapeFlat, agentflow.SourceReply}, reading{agentflow.ExtractedAsSent, "", agentflow.SourceUnrepairable}},
		{"flat refused, then repaired", `{"type":"delegate","delegate":{"goal":"g"}}`, `{"action":{"type":"delegate","delegate":{"agent_id":"w","goal":"g"}}}`,
			reading{agentflow.ExtractedAsSent, agentflow.ShapeFlat, agentflow.SourceReply}, reading{agentflow.ExtractedAsSent, "", agentflow.SourceRepair}},
		{"one brace short", `{"action":{"type":"delegate","notes":"n","delegate":{"agent_id":"w","goal":"g"}}`, "", reading{agentflow.ExtractedTrailing, agentflow.ShapeGeneric, agentflow.SourceReply}, reading{}},
		{"example after the action", `{"action":{"type":"exit","exit_status":{"code":0}}} or {"action":{"type":"message","message":"example"}}`, `{"action":{"type":"exit","exit_status":{"code":0}}}`,
			reading{agentflow.ExtractedTrailing, agentflow.ShapeNested, agentflow.SourceReply}, reading{agentflow.ExtractedAmbiguous, "", agentflow.SourceRepair}},
		{"<think> action, then prose", `<think>{"action":{"type":"exit","exit_status":{"code":0}}}</think>Here you go.`, "Here you go.",
			reading{agentflow.ExtractedLoose, agentflow.ShapeNested, agentflow.SourceReply}, reading{agentflow.ExtractedAsSent, "", agentflow.SourceFallbackMessage}},
		{"repair", `I will read {read_file a.txt}`, `{"action":{"type":"message","message":"fixed"}}`, reading{agentflow.ExtractedAsSent, "", agentflow.SourceRepair}, reading{}},
		{"fallbackMessage", "All done.", "Done.", reading{agentflow.ExtractedAsSent, "", agentflow.SourceFallbackMessage}, reading{}},
		{"notesAsMessage", `{"action":{"type":"message","notes":"n","message":""}}`, `{"action":{"type":"message","notes":"n","message":""}}`, reading{agentflow.ExtractedAsSent, agentflow.ShapeGeneric, agentflow.SourceNotesAsMessage}, reading{}},
		{"unrepairable", `{"action": {"type": "tool"`, `{"action": {"type": "tool"`, reading{agentflow.ExtractedAsSent, "", agentflow.SourceUnrepairable}, reading{}},
	}
	for _, noChecks := range []bool{true, false} {
		reader := agentflow.ReplyReader{NoReplyChecks: noChecks}
		for _, c := range cases {
			want := c.before
			if !noChecks && c.checked != (reading{}) {
				want = c.checked
			}
			replies := &transcriptReplies{first: c.first, repair: c.repair}
			backend := agentflow.PromptBackend{Completer: replies, Workflow: agentflow.WorkflowSpec{Name: "w", EntryAgent: "a"}, Agent: agentflow.AgentSpec{ID: "a", SystemPrompt: "Work."}, NoReplyChecks: noChecks}
			stepResult, stepErr := backend.Generate(context.Background(), runtime.GenerateRequest{Process: kernel.Process{PID: "pid-1", Role: "a"}})
			result, source, err := reader.Resolve(c.first, c.repair, replies.calls > 1)
			if source != want.source || !reflect.DeepEqual(result, stepResult) || (err == nil) != (stepErr == nil) {
				t.Fatalf("noChecks %t, %s: resolved %q %+v (%v), the step took %+v (%v)", noChecks, c.name, source, result, err, stepResult, stepErr)
			}
			got := reader.Read(c.first)
			if got.Extraction != want.extraction || got.Shape != want.shape {
				t.Fatalf("noChecks %t, %s: read %q/%q, want %q/%q", noChecks, c.name, got.Extraction, got.Shape, want.extraction, want.shape)
			}
		}
	}
	if _, _, err := agentflow.ResolveRecordedReply("prose", "", false); err == nil {
		t.Fatal("a reply that needed a repair, with none recorded, should not resolve")
	}
	if agentflow.ReaderFor(map[string]string{}).NoReplyChecks != true || agentflow.ReaderFor(map[string]string{agentflow.ReplyChecksField: agentflow.ReplyChecksV1}).NoReplyChecks {
		t.Fatal("ReaderFor should follow reply_checks")
	}
}
