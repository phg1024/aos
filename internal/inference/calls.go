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

// RequestParams are a request's fields other than its messages, as sent:
// a field the request did not carry is not recorded.
type RequestParams struct {
	// API is the wire format: empty for chat completions, APIAnthropicMessages
	// for the Anthropic Messages API. A record's body is rebuilt in it.
	API   string `json:"api,omitempty"`
	Model string `json:"model"`
	// Temperature is set on every chat-completions call, which always
	// sends it, and nil on an Anthropic call, which never does. Records
	// from before it was a pointer hold it either way.
	Temperature           *float64       `json:"temperature,omitempty"`
	TopP                  float64        `json:"top_p,omitempty"`
	MaxTokens             int            `json:"max_tokens,omitempty"`
	ReasoningBudgetTokens int            `json:"reasoning_budget_tokens,omitempty"`
	Stream                bool           `json:"stream,omitempty"`
	StreamOptions         *streamOptions `json:"stream_options,omitempty"`
	// Thinking, CacheControl and Fallbacks are the Anthropic request's own
	// fields, recorded as sent.
	Thinking     *ThinkingParam     `json:"thinking,omitempty"`
	CacheControl *CacheControlParam `json:"cache_control,omitempty"`
	Fallbacks    string             `json:"fallbacks,omitempty"`
}

// The two lines of a call in the ledger.
const (
	// PhaseStart is written, and synced, before the request is sent: it
	// holds the request.
	PhaseStart = "start"
	// PhaseOutcome is written when the call returns: it holds the reply,
	// or the error, and the usage.
	PhaseOutcome = "outcome"
)

// CallRecord is one line of the calls ledger (runlog.CallsFile). A call
// has a start line and, once it returns, an outcome line naming the start;
// a start with no outcome was in flight when the process died. Ledgers
// from before the split have one line per finished call, with no phase,
// holding both halves.
//
// A start's messages are only those added since the same process's
// previous start (Base); one whose messages do not extend that one, such
// as after compaction, records them all with Base 0. RequestSHA256 is the
// hash of the exact request body, which RebuildRequestBody reproduces.
// Usage on outcomes makes running totals derivable after a kill.
type CallRecord struct {
	Seq   int    `json:"seq"`
	Phase string `json:"phase,omitempty"`
	// Start is the seq of an outcome's start line.
	Start   int       `json:"start,omitempty"`
	Time    time.Time `json:"time"`
	PID     string    `json:"pid,omitempty"`
	AgentID string    `json:"agent_id,omitempty"`
	Repair  bool      `json:"repair,omitempty"`
	// Run is the kernel event that started the run (run.started).
	Run string `json:"run,omitempty"`
	// Step and ElapsedSeconds are the process's step and the seconds its
	// budget chain had used when the step began (runtime.CallInfo).
	Step             int     `json:"step,omitempty"`
	ElapsedSeconds   float64 `json:"elapsed_s,omitempty"`
	Backend          string  `json:"backend"`
	Model            string  `json:"model"`
	PromptTokens     int     `json:"prompt_tokens"`
	CachedTokens     int     `json:"cached_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	// CacheCreationTokens are prompt tokens written to the cache, where the
	// server reports them (Anthropic); they are part of PromptTokens.
	CacheCreationTokens int    `json:"cache_creation_tokens,omitempty"`
	LatencyMS           int64  `json:"latency_ms"`
	Error               string `json:"error,omitempty"`
	Response            string `json:"response,omitempty"`
	// Reasoning is the model's thinking where the server returns it apart
	// from the reply (Anthropic thinking blocks). It is never parsed.
	Reasoning string `json:"reasoning,omitempty"`
	// Params are on a start, or a one-line record from before the split; an
	// outcome carries none.
	Params        RequestParams `json:"params,omitzero"`
	Base          int           `json:"base,omitempty"`
	Messages      []ChatMessage `json:"messages,omitempty"`
	RequestSHA256 string        `json:"request_sha256"`
}

// Finished reports whether the record closes a call: an outcome, or a
// record from before calls had two lines.
func (c CallRecord) Finished() bool { return c.Phase != PhaseStart }

// HasRequest reports whether the record holds a request: a start, or a
// record from before calls had two lines.
func (c CallRecord) HasRequest() bool { return c.Phase != PhaseOutcome }

// InFlight returns the starts in records that no outcome names: calls in
// flight when the process died.
func InFlight(records []CallRecord) []CallRecord {
	closed := map[int]bool{}
	for _, record := range records {
		if record.Phase == PhaseOutcome {
			closed[record.Start] = true
		}
	}
	var open []CallRecord
	for _, record := range records {
		if record.Phase == PhaseStart && !closed[record.Seq] {
			open = append(open, record)
		}
	}
	return open
}

// RequestMessages rebuilds the full message list of the request that
// records[i] holds, following its base chain.
func RequestMessages(records []CallRecord, i int) []ChatMessage {
	bySeq := map[int]int{}
	for j, record := range records {
		bySeq[record.Seq] = j
	}
	var chain [][]ChatMessage
	for j, ok := i, true; ok; {
		chain = append(chain, records[j].Messages)
		if records[j].Base == 0 {
			break
		}
		j, ok = bySeq[records[j].Base]
	}
	var messages []ChatMessage
	for k := len(chain) - 1; k >= 0; k-- {
		messages = append(messages, chain[k]...)
	}
	return messages
}

// TemperatureSent is the temperature a chat-completions body carries: the
// recorded one, or 0, which that body sends when none is set.
func (p RequestParams) TemperatureSent() float64 {
	if p.Temperature == nil {
		return 0
	}
	return *p.Temperature
}

// RebuildRequestBody reproduces the exact body of a call from its params and
// its full message list (its base chain's messages plus its own), in the
// wire format params.API names.
func RebuildRequestBody(params RequestParams, messages []ChatMessage) ([]byte, error) {
	if params.API == APIAnthropicMessages {
		return json.Marshal(anthropicRequestOf(params, messages))
	}
	return json.Marshal(chatCompletionRequest{
		Model:                 params.Model,
		Messages:              messages,
		Temperature:           params.TemperatureSent(),
		TopP:                  params.TopP,
		MaxTokens:             params.MaxTokens,
		ReasoningBudgetTokens: params.ReasoningBudgetTokens,
		Stream:                params.Stream,
		StreamOptions:         params.StreamOptions,
	})
}

// SetCallLog makes the backend append a start and an outcome line to log
// for every call.
func (b *VLLMBackend) SetCallLog(log *runlog.Log) {
	b.calls = &callRecorder{log: log, last: map[string]previousCall{}}
}

type callRecorder struct {
	log  *runlog.Log
	mu   sync.Mutex
	last map[string]previousCall
}

// previousCall is a process's last non-repair start, which the next one
// usually extends.
type previousCall struct {
	seq      int
	messages []chatMessage
}

// pendingCall is what recordStart wrote, for recordOutcome to close.
type pendingCall struct {
	seq  int
	sha  string
	info runtime.CallInfo
}

// requestHash is the hash of payload's exact body.
func requestHash(payload any) (string, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:]), nil
}

func paramsOf(payload chatCompletionRequest) RequestParams {
	return RequestParams{Model: payload.Model, Temperature: &payload.Temperature, TopP: payload.TopP, MaxTokens: payload.MaxTokens, ReasoningBudgetTokens: payload.ReasoningBudgetTokens, Stream: payload.Stream, StreamOptions: payload.StreamOptions}
}

// recordStart writes the call's start line before the request is sent.
func (b *VLLMBackend) recordStart(ctx context.Context, payload chatCompletionRequest) pendingCall {
	return b.calls.start(ctx, b.baseURL, payload, paramsOf(payload), payload.Messages)
}

// recordOutcome writes the outcome line of the call pending started.
func (b *VLLMBackend) recordOutcome(pending pendingCall, model string, usage *responseUsage, latency time.Duration, content string, callErr error) {
	var counts callUsage
	if usage != nil {
		counts = callUsage{prompt: usage.PromptTokens, completion: usage.CompletionTokens}
		if usage.PromptTokensDetails != nil {
			counts.cached = usage.PromptTokensDetails.CachedTokens
		}
	}
	b.calls.outcome(pending, b.baseURL, model, counts, latency, content, "", callErr)
}

// callUsage is what the server reported for one call, in the ledger's
// terms: prompt includes cached and cacheWrite.
type callUsage struct {
	prompt, completion, cached, cacheWrite int
}

// start writes a call's start line: payload is the exact body, messages
// its message list as recorded (system first). A nil recorder writes
// nothing.
func (recorder *callRecorder) start(ctx context.Context, backend string, payload any, params RequestParams, messages []chatMessage) pendingCall {
	info, _ := runtime.CallInfoFrom(ctx)
	pending := pendingCall{info: info}
	if recorder == nil {
		return pending
	}
	sha, err := requestHash(payload)
	if err != nil {
		return pending
	}
	pending.sha = sha
	record := CallRecord{
		Phase:          PhaseStart,
		Time:           time.Now().UTC(),
		PID:            info.PID,
		AgentID:        info.AgentID,
		Repair:         info.Repair,
		Run:            info.Run,
		Step:           info.Step,
		ElapsedSeconds: info.Elapsed,
		Backend:        backend,
		Model:          params.Model,
		Params:         params,
		Messages:       messages,
		RequestSHA256:  sha,
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if previous, ok := recorder.last[info.PID]; ok && info.PID != "" && extends(messages, previous.messages) {
		record.Base = previous.seq
		record.Messages = messages[len(previous.messages):]
	}
	seq, err := recorder.log.Append(func(seq int) any {
		record.Seq = seq
		return record
	})
	if err != nil {
		return pending
	}
	pending.seq = seq
	if info.PID != "" && !info.Repair {
		recorder.last[info.PID] = previousCall{seq: seq, messages: slices.Clone(messages)}
	}
	return pending
}

// outcome writes the outcome line of the call pending started.
func (recorder *callRecorder) outcome(pending pendingCall, backend, model string, usage callUsage, latency time.Duration, content, reasoning string, callErr error) {
	if recorder == nil || pending.seq == 0 {
		return
	}
	info := pending.info
	record := CallRecord{
		Phase:               PhaseOutcome,
		Start:               pending.seq,
		Time:                time.Now().UTC(),
		PID:                 info.PID,
		AgentID:             info.AgentID,
		Repair:              info.Repair,
		Run:                 info.Run,
		Step:                info.Step,
		Backend:             backend,
		Model:               model,
		PromptTokens:        usage.prompt,
		CompletionTokens:    usage.completion,
		CachedTokens:        usage.cached,
		CacheCreationTokens: usage.cacheWrite,
		LatencyMS:           latency.Milliseconds(),
		Response:            content,
		Reasoning:           reasoning,
		RequestSHA256:       pending.sha,
	}
	if callErr != nil {
		record.Error = callErr.Error()
	}
	_, _ = recorder.log.Append(func(seq int) any {
		record.Seq = seq
		return record
	})
}

func extends(messages, prefix []chatMessage) bool {
	return len(messages) >= len(prefix) && slices.Equal(messages[:len(prefix)], prefix)
}
