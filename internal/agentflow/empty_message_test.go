package agentflow

import (
	"context"
	"testing"

	"aos/internal/runtime"
)

// A message action whose reply sits under another key is read from it; one
// with no reply text at all is not a usable action.
func TestMessageActionNeedsText(t *testing.T) {
	for raw, want := range map[string]string{
		`{"action":{"type":"message","message":"hi"}}`:  "hi",
		`{"action":{"type":"message","content":"hi"}}`:  "hi",
		`{"action":{"type":"message","text":"hi"}}`:     "hi",
		`{"action":{"type":"message","response":"hi"}}`: "hi",
		`{"type":"message","answer":"hi"}`:              "hi",
	} {
		if result, ok := parseGenerateResult(raw); !ok || result.Action.Message != want {
			t.Fatalf("%s parsed to %+v, %v", raw, result.Action, ok)
		}
	}
	for _, raw := range []string{`{"action":{"type":"message"}}`, `{"action":{"type":"message","message":"  ","notes":"done"}}`, `{"type":"message"}`} {
		if result, ok := parseGenerateResult(raw); ok {
			t.Fatalf("%s should not be usable, got %+v", raw, result.Action)
		}
	}
}

type scriptedMessages struct {
	replies []string
	calls   int
}

func (s *scriptedMessages) CompleteMessages(context.Context, []runtime.Message) (string, error) {
	reply := s.replies[min(s.calls, len(s.replies)-1)]
	s.calls++
	return reply, nil
}

func (s *scriptedMessages) CompleteJSON(context.Context, string, string, any) error { return nil }

// An empty reply is repaired; if the repair is empty too, the notes are
// shown rather than nothing.
func TestEmptyMessageIsRepairedThenFallsBackToNotes(t *testing.T) {
	request := runtime.GenerateRequest{History: []runtime.Message{{Role: "user", Content: "all done?"}}}
	repaired := &scriptedMessages{replies: []string{`{"action":{"type":"message","notes":"report updated"}}`, `{"action":{"type":"message","message":"Yes, all done."}}`}}
	result, err := PromptBackend{Completer: repaired}.Generate(context.Background(), request)
	if err != nil || result.Action.Message != "Yes, all done." || repaired.calls != 2 {
		t.Fatalf("repair: %+v %v after %d calls", result.Action, err, repaired.calls)
	}
	stubborn := &scriptedMessages{replies: []string{`{"action":{"type":"message","notes":"Updated report.html: 13 edits."}}`}}
	result, err = PromptBackend{Completer: stubborn}.Generate(context.Background(), request)
	if err != nil || result.Action.Type != "message" || result.Action.Message != "Updated report.html: 13 edits." {
		t.Fatalf("notes fallback: %+v %v", result.Action, err)
	}
}
