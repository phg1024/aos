package inference_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"path/filepath"
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
	ctx := runtime.WithCallInfo(context.Background(), runtime.CallInfo{PID: "pid-000002", AgentID: "writer"})
	call(ctx, "system prompt", "goal")
	call(ctx, "system prompt", "goal", "step 1", "result 1")
	call(runtime.AsRepair(ctx), "system prompt", "goal", "step 1", "result 1", "bad reply", "repair please")
	call(ctx, "system prompt", "goal", "step 1", "result 1", "step 2", "result 2")
	call(ctx, "compacted summary", "goal") // history rewritten: not an extension

	records, err := runlog.Read[inference.CallRecord](path)
	if err != nil || len(records) != 5 {
		t.Fatalf("expected 5 records, got %d (%v)", len(records), err)
	}
	wantBase := []int{0, 1, 2, 2, 0}
	wantOwn := []int{2, 2, 2, 2, 2}
	bySeq := map[int]inference.CallRecord{}
	for idx, record := range records {
		bySeq[record.Seq] = record
		if record.Base != wantBase[idx] || len(record.Messages) != wantOwn[idx] {
			t.Fatalf("call %d: base %d with %d messages, want base %d with %d", idx+1, record.Base, len(record.Messages), wantBase[idx], wantOwn[idx])
		}
		var full []inference.ChatMessage
		for chain := record; ; chain = bySeq[chain.Base] {
			full = append(append([]inference.ChatMessage(nil), chain.Messages...), full...)
			if chain.Base == 0 {
				break
			}
		}
		body, err := inference.RebuildRequestBody(record.Params, full)
		if err != nil {
			t.Fatalf("rebuild: %v", err)
		}
		digest := sha256.Sum256(body)
		if got := hex.EncodeToString(digest[:]); got != record.RequestSHA256 || got != sent[idx] {
			t.Fatalf("call %d: rebuilt hash %s, recorded %s, sent %s", idx+1, got, record.RequestSHA256, sent[idx])
		}
	}
	if !records[2].Repair || records[1].Repair || records[0].PromptTokens != 100 || records[0].CachedTokens != 60 || records[0].CompletionTokens != 7 || records[0].AgentID != "writer" {
		t.Fatalf("unexpected attribution or usage: %+v", records[:3])
	}
}
