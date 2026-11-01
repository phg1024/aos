package inference_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aos/internal/inference"
	"aos/internal/runlog"
	"aos/internal/runtime"
)

// Every call can be rebuilt from the ledger: follow Base back to a full
// message list, append each call's own messages, and the rebuilt body hashes
// to what was actually sent.
func TestCallLedgerRebuildsEachRequest(t *testing.T) {
	var sent []string
	backend := inference.NewVLLMBackend(inference.VLLMConfig{
		BaseURL: "http://vllm.local",
		Model:   "local-model",
		HTTPClient: &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			body, _ := io.ReadAll(r.Body)
			digest := sha256.Sum256(body)
			sent = append(sent, hex.EncodeToString(digest[:]))
			return jsonResponse(http.StatusOK, map[string]any{
				"choices": []map[string]any{{"message": map[string]any{"content": `{"action":{"type":"exit","exit_status":{"code":0,"reason":"ok"}}}`}}},
				"usage":   map[string]any{"prompt_tokens": 100, "completion_tokens": 7, "prompt_tokens_details": map[string]any{"cached_tokens": 60}},
			}), nil
		})},
	})
	path := filepath.Join(t.TempDir(), runlog.Dir, runlog.CallsFile)
	log, err := runlog.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	backend.SetCallLog(log)

	call := func(ctx context.Context, contents ...string) {
		messages := make([]runtime.Message, 0, len(contents))
		for i, content := range contents {
			role := "user"
			if i == 0 {
				role = "system"
			} else if i%2 == 0 {
				role = "assistant"
			}
			messages = append(messages, runtime.Message{Role: role, Content: content})
		}
		if _, err := backend.CompleteMessages(ctx, messages); err != nil {
			t.Fatalf("call: %v", err)
		}
	}
	ctx := runtime.WithCallInfo(context.Background(), runtime.CallInfo{PID: "pid-000002", AgentID: "writer", Step: 3, Elapsed: 12.5, Run: "evt-000002"})
	call(ctx, "system prompt", "goal")
	call(ctx, "system prompt", "goal", "step 1", "result 1")
	call(runtime.AsRepair(ctx), "system prompt", "goal", "step 1", "result 1", "bad reply", "repair please")
	call(ctx, "system prompt", "goal", "step 1", "result 1", "step 2", "result 2")
	call(ctx, "compacted summary", "goal") // history rewritten: not an extension

	records, err := runlog.Read[inference.CallRecord](path)
	if err != nil || len(records) != 10 {
		t.Fatalf("expected a start and an outcome for each of 5 calls, got %d (%v)", len(records), err)
	}
	if open := inference.InFlight(records); len(open) != 0 {
		t.Fatalf("no call is in flight: %+v", open)
	}
	// Starts hold the requests; a start's base is the process's previous
	// non-repair start.
	wantBase := []int{0, 1, 3, 3, 0}
	for call := 0; call < 5; call++ {
		start, outcome := records[2*call], records[2*call+1]
		if start.Phase != inference.PhaseStart || outcome.Phase != inference.PhaseOutcome || outcome.Start != start.Seq {
			t.Fatalf("call %d: lines %+v / %+v", call+1, start, outcome)
		}
		if start.Base != wantBase[call] || len(start.Messages) != 2 || len(outcome.Messages) != 0 {
			t.Fatalf("call %d: base %d with %d messages, want base %d with 2", call+1, start.Base, len(start.Messages), wantBase[call])
		}
		body, err := inference.RebuildRequestBody(start.Params, inference.RequestMessages(records, 2*call))
		if err != nil {
			t.Fatalf("rebuild: %v", err)
		}
		digest := sha256.Sum256(body)
		if got := hex.EncodeToString(digest[:]); got != start.RequestSHA256 || got != sent[call] || outcome.RequestSHA256 != got {
			t.Fatalf("call %d: rebuilt hash %s, recorded %s, sent %s", call+1, got, start.RequestSHA256, sent[call])
		}
		if start.Step != 3 || start.ElapsedSeconds != 12.5 || start.Run != "evt-000002" || outcome.Step != 3 {
			t.Fatalf("call %d: step, elapsed and run should be recorded: %+v", call+1, start)
		}
		if outcome.Response != `{"action":{"type":"exit","exit_status":{"code":0,"reason":"ok"}}}` || outcome.PromptTokens != 100 || outcome.CachedTokens != 60 || outcome.CompletionTokens != 7 {
			t.Fatalf("call %d: the outcome should hold the raw reply and usage: %+v", call+1, outcome)
		}
	}
	if !records[4].Repair || !records[5].Repair || records[2].Repair || records[0].AgentID != "writer" {
		t.Fatalf("unexpected attribution: %+v", records[:6])
	}
}

// A start is on disk before the request goes out, so a call cut off in
// flight leaves a start with no outcome; a failed call's outcome holds its
// error.
func TestCallLedgerShowsCallsInFlight(t *testing.T) {
	path := filepath.Join(t.TempDir(), runlog.Dir, runlog.CallsFile)
	log, _ := runlog.Open(path)
	var startsSeen int
	backend := inference.NewVLLMBackend(inference.VLLMConfig{
		BaseURL: "http://vllm.local",
		Model:   "local-model",
		HTTPClient: &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			records, _ := runlog.Read[inference.CallRecord](path)
			startsSeen = len(records)
			return jsonResponse(http.StatusBadRequest, map[string]any{"error": "bad"}), nil
		})},
	})
	backend.SetCallLog(log)
	ctx := runtime.WithCallInfo(context.Background(), runtime.CallInfo{PID: "pid-000002"})
	if _, err := backend.CompleteMessages(ctx, []runtime.Message{{Role: "user", Content: "goal"}}); err == nil {
		t.Fatal("the call should fail")
	}
	records, _ := runlog.Read[inference.CallRecord](path)
	if startsSeen != 1 || len(records) != 2 || records[1].Error == "" || records[1].Start != records[0].Seq {
		t.Fatalf("start before sending, outcome with the error after: saw %d before sending, %+v", startsSeen, records)
	}
	// Cut the ledger after the start, as a kill in flight would, with a
	// torn half line behind it.
	raw, _ := os.ReadFile(path)
	firstLine := strings.SplitAfter(string(raw), "\n")[0]
	os.WriteFile(path, []byte(firstLine+`{"seq":2,"phase":"outc`), 0o644)
	records, err := runlog.Read[inference.CallRecord](path)
	if err != nil || len(records) != 1 {
		t.Fatalf("the torn line should be skipped: %+v %v", records, err)
	}
	if open := inference.InFlight(records); len(open) != 1 || open[0].Seq != 1 || open[0].Finished() {
		t.Fatalf("the cut call should be in flight: %+v", open)
	}
}
