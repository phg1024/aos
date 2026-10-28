package inference

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"sync"
	"time"

	"aos/internal/runlog"
	"aos/internal/runtime"
)

// ChatMessage is one message of a chat request as sent and as recorded.
type ChatMessage = chatMessage

// RequestParams are a request's fields other than its messages.
type RequestParams struct {
	Model                 string         `json:"model"`
	Temperature           float64        `json:"temperature"`
	TopP                  float64        `json:"top_p,omitempty"`
	MaxTokens             int            `json:"max_tokens,omitempty"`
	ReasoningBudgetTokens int            `json:"reasoning_budget_tokens,omitempty"`
	Stream                bool           `json:"stream,omitempty"`
	StreamOptions         *streamOptions `json:"stream_options,omitempty"`
}

// CallRecord is one model call in the calls ledger (runlog.CallsFile). Its
// usage makes running totals derivable after a kill. Its messages are only
// those added since the process's previous call (Base); a call whose
// messages do not extend that one, such as after compaction, records them
// all with Base 0. RequestSHA256 is the hash of the exact request body,
// which RebuildRequestBody reproduces.
type CallRecord struct {
	Seq              int           `json:"seq"`
	Time             time.Time     `json:"time"`
	PID              string        `json:"pid,omitempty"`
	AgentID          string        `json:"agent_id,omitempty"`
	Repair           bool          `json:"repair,omitempty"`
	Backend          string        `json:"backend"`
	Model            string        `json:"model"`
	PromptTokens     int           `json:"prompt_tokens"`
	CachedTokens     int           `json:"cached_tokens"`
	CompletionTokens int           `json:"completion_tokens"`
	LatencyMS        int64         `json:"latency_ms"`
	Error            string        `json:"error,omitempty"`
	Params           RequestParams `json:"params"`
	Base             int           `json:"base,omitempty"`
	Messages         []ChatMessage `json:"messages"`
	RequestSHA256    string        `json:"request_sha256"`
}

// RebuildRequestBody reproduces the exact body of a call from its params and
// its full message list (its base chain's messages plus its own).
func RebuildRequestBody(params RequestParams, messages []ChatMessage) ([]byte, error) {
	return json.Marshal(chatCompletionRequest{
		Model:                 params.Model,
		Messages:              messages,
		Temperature:           params.Temperature,
		TopP:                  params.TopP,
		MaxTokens:             params.MaxTokens,
		ReasoningBudgetTokens: params.ReasoningBudgetTokens,
		Stream:                params.Stream,
		StreamOptions:         params.StreamOptions,
	})
}

// SetCallLog makes the backend append a CallRecord to log for every call.
func (b *VLLMBackend) SetCallLog(log *runlog.Log) {
	b.calls = &callRecorder{log: log, last: map[string]previousCall{}}
}

type callRecorder struct {
	log  *runlog.Log
	mu   sync.Mutex
	last map[string]previousCall
}

// previousCall is a process's last non-repair call, which the next one
// usually extends.
type previousCall struct {
	seq      int
	messages []chatMessage
}

func (b *VLLMBackend) recordCall(ctx context.Context, payload chatCompletionRequest, usage *responseUsage, latency time.Duration, callErr error) {
	if b.calls == nil {
		return
	}
	info, _ := runtime.CallInfoFrom(ctx)
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	digest := sha256.Sum256(body)
	record := CallRecord{
		Time:          time.Now().UTC(),
		PID:           info.PID,
		AgentID:       info.AgentID,
		Repair:        info.Repair,
		Backend:       b.baseURL,
		Model:         payload.Model,
		LatencyMS:     latency.Milliseconds(),
		Params:        RequestParams{Model: payload.Model, Temperature: payload.Temperature, TopP: payload.TopP, MaxTokens: payload.MaxTokens, ReasoningBudgetTokens: payload.ReasoningBudgetTokens, Stream: payload.Stream, StreamOptions: payload.StreamOptions},
		Messages:      payload.Messages,
		RequestSHA256: hex.EncodeToString(digest[:]),
	}
	if usage != nil {
		record.PromptTokens, record.CompletionTokens = usage.PromptTokens, usage.CompletionTokens
		if usage.PromptTokensDetails != nil {
			record.CachedTokens = usage.PromptTokensDetails.CachedTokens
		}
	}
	if callErr != nil {
		record.Error = callErr.Error()
	}

	recorder := b.calls
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if previous, ok := recorder.last[info.PID]; ok && info.PID != "" && extends(payload.Messages, previous.messages) {
		record.Base = previous.seq
		record.Messages = payload.Messages[len(previous.messages):]
	}
	seq, err := recorder.log.Append(func(seq int) any {
		record.Seq = seq
		return record
	})
	if err == nil && info.PID != "" && !info.Repair {
		recorder.last[info.PID] = previousCall{seq: seq, messages: slices.Clone(payload.Messages)}
	}
}

func extends(messages, prefix []chatMessage) bool {
	return len(messages) >= len(prefix) && slices.Equal(messages[:len(prefix)], prefix)
}
