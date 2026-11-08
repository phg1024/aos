package inference

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"aos/internal/runlog"
	"aos/internal/runtime"
)

// APIAnthropicMessages marks a call recorded in the Anthropic Messages
// format (RequestParams.API).
const APIAnthropicMessages = "anthropic-messages"

const (
	// anthropicVersion is the API version every request sends. On it a
	// request with temperature, top_p or top_k is a 400, so none is sent.
	anthropicVersion        = "2026-10-21"
	defaultAnthropicBaseURL = "https://api.anthropic.com"
	defaultAnthropicModel   = "claude-sonnet-5-5"
	// DefaultAnthropicMaxTokens is max_tokens where the profile sets none.
	// The API requires the field, and thinking counts toward it. 16,000
	// leaves room for adaptive thinking and an AOS action while keeping a
	// non-streamed reply well inside the HTTP timeout; a token budget
	// lowers it to what is left.
	DefaultAnthropicMaxTokens = 16000
	// fallbackBeta enables fallbacks: "default", which re-serves a declined
	// request on another model inside the same call.
	fallbackBeta = "server-side-fallback-2026-07-01"
	// errorBodyLimit is how much of an error reply is read.
	errorBodyLimit = 64 << 10
)

// fallbackModels are the models that take fallbacks: "default".
var fallbackModels = map[string]bool{
	"claude-fable-5-1":  true,
	"claude-opus-5-5":   true,
	"claude-opus-5":     true,
	"claude-sonnet-5-5": true,
}

// ErrModelRefused is a reply that stopped with stop_reason "refusal".
var ErrModelRefused = errors.New("model declined the request")

// ThinkingParam is the Anthropic request's thinking field.
type ThinkingParam struct {
	Type    string `json:"type"`
	Display string `json:"display,omitempty"`
}

// CacheControlParam is the Anthropic request's top-level cache_control,
// which places the cache breakpoint on the last cacheable block.
type CacheControlParam struct {
	Type string `json:"type"`
}

// AnthropicConfig configures AnthropicBackend. APIKey is the key itself,
// read by the caller from the variable a profile names; it is sent only
// as the x-api-key header and never recorded.
type AnthropicConfig struct {
	BaseURL      string
	Model        string
	APIKey       string
	SystemPrompt string
	Timeout      time.Duration
	// MaxTokens is max_tokens; 0 means DefaultAnthropicMaxTokens.
	MaxTokens     int
	HTTPClient    *http.Client
	OnStreamEvent func(StreamEvent)
	// Stream asks for a streamed reply even without OnStreamEvent, as a
	// replay of a streamed run must.
	Stream bool
	// Replayer, when set, answers every call instead of the server.
	Replayer Replayer
}

// AnthropicBackend is the driver for the Anthropic Messages API
// (POST /v1/messages), the counterpart of VLLMBackend for the runtime and
// agentflow.
//
// A transcript maps onto the API so: leading system messages become the
// top-level system field, every other role is user unless it is
// assistant, and consecutive turns of one role are joined into one. The
// reply is the text blocks; thinking blocks are kept apart, sent to
// OnStreamEvent as thinking and recorded as the call's reasoning, and never
// parsed as the action or sent back. Since no thinking block is replayed,
// AOS's compaction cannot trip the API's history-editing check.
//
// Every request asks for adaptive thinking with summarized display, puts a
// top-level cache_control breakpoint on the prompt, and, for the models
// that take it, fallbacks: "default"; the ledger records the model that
// answered. temperature and top_p are never sent, so a profile's are
// ignored, as is its thinking_budget.
//
// Retries: none here. Each call is one request, and its error, a 429 or
// 529 included, goes to the runtime's BackendRetryPolicy, the only retry
// layer, so every attempt is a backend.retry event and a ledger line. The
// error carries retry-after when the server sends it, but the runtime's
// backoff does not read it.
type AnthropicBackend struct {
	baseURL       string
	model         string
	apiKey        string
	client        *http.Client
	system        string
	maxTokens     int
	onStreamEvent func(StreamEvent)
	stream        bool
	replayer      Replayer
	calls         *callRecorder

	usageMu sync.Mutex
	usage   TokenUsage
}

// NewAnthropicBackend builds the driver, with the Anthropic API's base URL
// and claude-sonnet-5-5 where config names none.
func NewAnthropicBackend(config AnthropicConfig) *AnthropicBackend {
	timeout := config.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	system := config.SystemPrompt
	if system == "" {
		system = "You are an AOS backend. Return only JSON matching the GenerateResult schema."
	}
	client := config.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	maxTokens := config.MaxTokens
	if maxTokens <= 0 {
		maxTokens = DefaultAnthropicMaxTokens
	}
	return &AnthropicBackend{
		baseURL:       normalizeBaseURL(firstNonEmpty(config.BaseURL, defaultAnthropicBaseURL)),
		model:         firstNonEmpty(config.Model, defaultAnthropicModel),
		apiKey:        config.APIKey,
		system:        system,
		client:        client,
		maxTokens:     maxTokens,
		onStreamEvent: config.OnStreamEvent,
		stream:        config.Stream || config.OnStreamEvent != nil,
		replayer:      config.Replayer,
	}
}

// SetCallLog makes the backend append a start and an outcome line to log
// for every call.
func (b *AnthropicBackend) SetCallLog(log *runlog.Log) {
	b.calls = &callRecorder{log: log, last: map[string]previousCall{}}
}

// Usage returns the token totals so far.
func (b *AnthropicBackend) Usage() TokenUsage {
	b.usageMu.Lock()
	defer b.usageMu.Unlock()
	return b.usage
}

func (b *AnthropicBackend) recordUsage(usage callUsage) {
	b.usageMu.Lock()
	defer b.usageMu.Unlock()
	b.usage.Requests++
	b.usage.PromptTokens += usage.prompt
	b.usage.CompletionTokens += usage.completion
	b.usage.CachedTokens += usage.cached
	b.usage.CacheCreationTokens += usage.cacheWrite
}

func (b *AnthropicBackend) Generate(ctx context.Context, request runtime.GenerateRequest) (runtime.GenerateResult, error) {
	content, err := b.complete(ctx, processMessages(b.system, request))
	if err != nil {
		return runtime.GenerateResult{}, err
	}
	result, err := normalizeGenerateResult(extractTrailingJSONObject(content))
	if err != nil {
		return runtime.GenerateResult{}, ErrMalformedModelResponse
	}
	return result, nil
}

func (b *AnthropicBackend) CompleteJSON(ctx context.Context, systemPrompt, userPrompt string, target any) error {
	content, err := b.CompleteText(ctx, systemPrompt, userPrompt)
	if err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(extractTrailingJSONObject(content)), target); err != nil {
		return ErrMalformedModelResponse
	}
	return nil
}

func (b *AnthropicBackend) CompleteText(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	return b.complete(ctx, []chatMessage{{Role: "system", Content: systemPrompt}, {Role: "user", Content: userPrompt}})
}

// CompleteMessages sends a whole chat transcript, mapped as the type's
// comment says.
func (b *AnthropicBackend) CompleteMessages(ctx context.Context, messages []runtime.Message) (string, error) {
	chat := make([]chatMessage, 0, len(messages))
	for _, message := range messages {
		chat = append(chat, chatMessage{Role: message.Role, Content: message.Content})
	}
	return b.complete(ctx, chat)
}

// anthropicRequest is the Messages API request body.
type anthropicRequest struct {
	Model        string             `json:"model"`
	MaxTokens    int                `json:"max_tokens"`
	System       string             `json:"system,omitempty"`
	Messages     []chatMessage      `json:"messages"`
	Thinking     *ThinkingParam     `json:"thinking,omitempty"`
	CacheControl *CacheControlParam `json:"cache_control,omitempty"`
	Fallbacks    string             `json:"fallbacks,omitempty"`
	Stream       bool               `json:"stream,omitempty"`
}

// anthropicRequestOf is the body for params and messages as the ledger
// holds them: a leading system message, then the turns.
func anthropicRequestOf(params RequestParams, messages []chatMessage) anthropicRequest {
	request := anthropicRequest{
		Model:        params.Model,
		MaxTokens:    params.MaxTokens,
		Thinking:     params.Thinking,
		CacheControl: params.CacheControl,
		Fallbacks:    params.Fallbacks,
		Stream:       params.Stream,
		Messages:     messages,
	}
	if len(messages) > 0 && messages[0].Role == "system" {
		request.System, request.Messages = messages[0].Content, messages[1:]
	}
	if request.Messages == nil {
		request.Messages = []chatMessage{}
	}
	return request
}

// wireMessages maps a transcript onto the Messages API's turns, keeping
// the system prompt first: leading system messages joined into one, any
// later system message sent as user text, every other role but assistant
// as user, empty or blank messages dropped, and consecutive turns of one role
// joined with a blank line. It refuses a transcript that does not start
// with a user turn or that ends with an assistant one, which the API would
// reject.
func wireMessages(messages []chatMessage) ([]chatMessage, error) {
	var system []string
	i := 0
	for ; i < len(messages) && messages[i].Role == "system"; i++ {
		if strings.TrimSpace(messages[i].Content) != "" {
			system = append(system, messages[i].Content)
		}
	}
	var wire []chatMessage
	if len(system) > 0 {
		wire = append(wire, chatMessage{Role: "system", Content: strings.Join(system, "\n\n")})
	}
	turns := 0
	for _, message := range messages[i:] {
		if strings.TrimSpace(message.Content) == "" {
			// The API refuses a text block of only whitespace.
			continue
		}
		role := "user"
		if message.Role == "assistant" {
			role = "assistant"
		}
		if turns > 0 && wire[len(wire)-1].Role == role {
			wire[len(wire)-1].Content += "\n\n" + message.Content
			continue
		}
		if turns == 0 && role != "user" {
			return nil, errors.New("anthropic: a transcript must start with a user turn")
		}
		wire = append(wire, chatMessage{Role: role, Content: message.Content})
		turns++
	}
	switch {
	case turns == 0:
		return nil, errors.New("anthropic: a transcript needs a user turn")
	case wire[len(wire)-1].Role == "assistant":
		return nil, errors.New("anthropic: a transcript may not end with an assistant turn (a prefill, which the API refuses)")
	}
	return wire, nil
}

func (b *AnthropicBackend) params(ctx context.Context) RequestParams {
	params := RequestParams{
		API:          APIAnthropicMessages,
		Model:        b.model,
		MaxTokens:    b.maxTokens,
		Stream:       b.stream,
		Thinking:     &ThinkingParam{Type: "adaptive", Display: "summarized"},
		CacheControl: &CacheControlParam{Type: "ephemeral"},
	}
	if fallbackModels[b.model] {
		params.Fallbacks = "default"
	}
	if info, _ := runtime.CallInfoFrom(ctx); info.MaxTokens > 0 && info.MaxTokens < params.MaxTokens {
		// What is left of the caller's token budget.
		params.MaxTokens = info.MaxTokens
	}
	return params
}

func (b *AnthropicBackend) complete(ctx context.Context, messages []chatMessage) (string, error) {
	wire, err := wireMessages(messages)
	if err != nil {
		return "", err
	}
	params := b.params(ctx)
	payload := anthropicRequestOf(params, wire)
	pending := b.calls.start(ctx, b.baseURL, payload, params, wire)
	started := time.Now()
	reply, err := b.send(ctx, payload)
	model := firstNonEmpty(reply.model, payload.Model)
	b.calls.outcome(pending, b.baseURL, model, reply.usage, time.Since(started), reply.text, reply.reasoning, err)
	if info, _ := runtime.CallInfoFrom(ctx); info.Meter != nil && reply.counted {
		info.Meter.Add(reply.usage.prompt + reply.usage.completion)
	}
	return reply.text, err
}

// anthropicReply is one call's result as the ledger records it.
type anthropicReply struct {
	text, reasoning string
	model           string
	usage           callUsage
	// counted is whether the server reported usage.
	counted bool
}

type anthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

// counts is the usage in the ledger's terms: input_tokens excludes the
// cached and cache-written tokens, which the ledger's prompt includes.
func (u anthropicUsage) counts() callUsage {
	return callUsage{
		prompt:     u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens,
		completion: u.OutputTokens,
		cached:     u.CacheReadInputTokens,
		cacheWrite: u.CacheCreationInputTokens,
	}
}

type anthropicBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Thinking string `json:"thinking"`
}

type stopDetails struct {
	Category    string `json:"category"`
	Explanation string `json:"explanation"`
}

type anthropicResponse struct {
	Model       string           `json:"model"`
	Content     []anthropicBlock `json:"content"`
	StopReason  string           `json:"stop_reason"`
	StopDetails *stopDetails     `json:"stop_details"`
	Usage       anthropicUsage   `json:"usage"`
}

// AnthropicError is an error reply from the Messages API: its status, the
// error type and message it gave, and the request id and retry-after
// headers when sent. It holds nothing from the request.
type AnthropicError struct {
	StatusCode int
	Status     string
	Type       string
	Message    string
	RequestID  string
	RetryAfter string
}

func (e *AnthropicError) Error() string {
	text := "anthropic request failed: " + e.Status
	if e.Type != "" {
		text += ": " + e.Type
	}
	if e.Message != "" {
		text += ": " + e.Message
	}
	var notes []string
	if e.RetryAfter != "" {
		notes = append(notes, "retry-after "+e.RetryAfter)
	}
	if e.RequestID != "" {
		notes = append(notes, "request-id "+e.RequestID)
	}
	if len(notes) > 0 {
		text += " (" + strings.Join(notes, ", ") + ")"
	}
	return text
}

// Temporary reports whether the same request may succeed later: a timeout,
// a conflict, a rate limit (429), a server error or an overload (529).
// The runtime retries every error alike; this is for callers that tell
// them apart.
func (e *AnthropicError) Temporary() bool {
	switch e.StatusCode {
	case http.StatusRequestTimeout, http.StatusConflict, http.StatusTooManyRequests:
		return true
	}
	return e.StatusCode >= 500
}

// send makes the call's one request.
func (b *AnthropicBackend) send(ctx context.Context, payload anthropicRequest) (anthropicReply, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return anthropicReply{}, err
	}
	if b.replayer != nil {
		recorded, err := b.replayer.Reply(ctx, body)
		if err != nil {
			return anthropicReply{text: recorded.Content}, err
		}
		reply := anthropicReply{text: recorded.Content, reasoning: recorded.Reasoning, counted: true,
			usage: callUsage{prompt: recorded.PromptTokens, completion: recorded.CompletionTokens, cached: recorded.CachedTokens, cacheWrite: recorded.CacheCreationTokens}}
		b.recordUsage(reply.usage)
		return reply, nil
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, b.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return anthropicReply{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("anthropic-version", anthropicVersion)
	if b.apiKey != "" {
		request.Header.Set("x-api-key", b.apiKey)
	}
	if payload.Fallbacks != "" {
		request.Header.Set("anthropic-beta", fallbackBeta)
	}
	if payload.Stream {
		request.Header.Set("Accept", "text/event-stream")
	}
	response, err := b.client.Do(request)
	if err != nil {
		return anthropicReply{}, err
	}
	defer response.Body.Close()
	if response.StatusCode >= 400 {
		return anthropicReply{}, readAnthropicError(response)
	}
	var reply anthropicReply
	var stop string
	var details *stopDetails
	if payload.Stream {
		reply, stop, details, err = b.readStream(response.Body)
	} else {
		var parsed anthropicResponse
		if err := json.NewDecoder(response.Body).Decode(&parsed); err != nil {
			return anthropicReply{}, fmt.Errorf("%w: %v", ErrMalformedModelResponse, err)
		}
		reply = anthropicReply{model: parsed.Model, usage: parsed.Usage.counts(), counted: true}
		var reasoning []string
		for _, block := range parsed.Content {
			switch block.Type {
			case "text":
				reply.text += block.Text
			case "thinking":
				if block.Thinking != "" {
					reasoning = append(reasoning, block.Thinking)
				}
			}
		}
		reply.reasoning = strings.Join(reasoning, "\n")
		stop, details = parsed.StopReason, parsed.StopDetails
	}
	if err != nil {
		// A stream cut short is not a completed request: as with the
		// chat-completions driver, its usage is not counted.
		reply.usage, reply.counted = callUsage{}, false
		return reply, err
	}
	if reply.counted {
		b.recordUsage(reply.usage)
	}
	if stop == "refusal" {
		return reply, refusal(details)
	}
	return reply, nil
}

func refusal(details *stopDetails) error {
	if details == nil {
		return ErrModelRefused
	}
	return fmt.Errorf("%w: category %q: %s", ErrModelRefused, details.Category, details.Explanation)
}

func readAnthropicError(response *http.Response) error {
	failure := &AnthropicError{
		StatusCode: response.StatusCode,
		Status:     response.Status,
		RequestID:  response.Header.Get("request-id"),
		RetryAfter: response.Header.Get("retry-after"),
	}
	raw, _ := io.ReadAll(io.LimitReader(response.Body, errorBodyLimit))
	var parsed struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &parsed) == nil {
		failure.Type, failure.Message = parsed.Error.Type, parsed.Error.Message
	}
	return failure
}

// streamEvent is the union of the Messages API's server-sent events, as
// far as this driver reads them.
type streamEvent struct {
	Type    string `json:"type"`
	Message *struct {
		Model string         `json:"model"`
		Usage anthropicUsage `json:"usage"`
	} `json:"message"`
	ContentBlock *anthropicBlock `json:"content_block"`
	Delta        *struct {
		Type        string       `json:"type"`
		Text        string       `json:"text"`
		Thinking    string       `json:"thinking"`
		StopReason  string       `json:"stop_reason"`
		StopDetails *stopDetails `json:"stop_details"`
	} `json:"delta"`
	Usage *anthropicUsage `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// readStream reads a streamed reply to its message_stop: text deltas are
// the reply and thinking deltas the reasoning, each passed on as it comes.
func (b *AnthropicBackend) readStream(body io.Reader) (anthropicReply, string, *stopDetails, error) {
	var reply anthropicReply
	var text, reasoning strings.Builder
	var usage anthropicUsage
	var stop string
	var details *stopDetails
	finish := func(err error) (anthropicReply, string, *stopDetails, error) {
		reply.text, reply.reasoning, reply.usage = text.String(), reasoning.String(), usage.counts()
		return reply, stop, details, err
	}
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64<<10), 8<<20)
	for scanner.Scan() {
		data, ok := strings.CutPrefix(scanner.Text(), "data: ")
		if !ok {
			continue
		}
		var event streamEvent
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			continue
		}
		switch event.Type {
		case "message_start":
			if event.Message != nil {
				reply.model, usage, reply.counted = event.Message.Model, event.Message.Usage, true
			}
		case "content_block_start":
			if event.ContentBlock != nil && event.ContentBlock.Type == "thinking" && reasoning.Len() > 0 {
				reasoning.WriteString("\n")
			}
		case "content_block_delta":
			if event.Delta == nil {
				continue
			}
			switch event.Delta.Type {
			case "text_delta":
				b.emit(StreamEvent{Kind: StreamContent, Text: event.Delta.Text})
				text.WriteString(event.Delta.Text)
			case "thinking_delta":
				b.emit(StreamEvent{Kind: StreamThinking, Text: event.Delta.Thinking})
				reasoning.WriteString(event.Delta.Thinking)
			}
		case "message_delta":
			if event.Delta != nil && event.Delta.StopReason != "" {
				stop, details = event.Delta.StopReason, event.Delta.StopDetails
			}
			if event.Usage != nil {
				// Counts here are cumulative; a zero means not repeated.
				usage.OutputTokens = event.Usage.OutputTokens
				usage.InputTokens = max(usage.InputTokens, event.Usage.InputTokens)
				usage.CacheReadInputTokens = max(usage.CacheReadInputTokens, event.Usage.CacheReadInputTokens)
				usage.CacheCreationInputTokens = max(usage.CacheCreationInputTokens, event.Usage.CacheCreationInputTokens)
				reply.counted = true
			}
		case "error":
			failure := &AnthropicError{Status: "stream error"}
			if event.Error != nil {
				failure.Type, failure.Message = event.Error.Type, event.Error.Message
			}
			return finish(failure)
		case "message_stop":
			b.emit(StreamEvent{Kind: StreamDone})
			return finish(nil)
		}
	}
	if err := scanner.Err(); err != nil {
		return finish(err)
	}
	return finish(fmt.Errorf("%w: the stream ended before message_stop", ErrMalformedModelResponse))
}

// emit passes a stream event to OnStreamEvent, if set.
func (b *AnthropicBackend) emit(event StreamEvent) {
	if b.onStreamEvent != nil {
		b.onStreamEvent(event)
	}
}
