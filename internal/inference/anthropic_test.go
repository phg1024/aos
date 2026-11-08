package inference_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"aos/internal/inference"
	"aos/internal/kernel"
	"aos/internal/runlog"
	"aos/internal/runtime"
)

// testKey is a key made up for the test run, so no key is in a fixture.
func testKey(t *testing.T) string {
	t.Helper()
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "test-" + hex.EncodeToString(b)
}

// messagesServer is an httptest server for /v1/messages: it records each
// request and answers with reply, a JSON body or an event stream.
type messagesServer struct {
	t      *testing.T
	server *httptest.Server

	mu       sync.Mutex
	bodies   []map[string]any
	raw      [][]byte
	headers  []http.Header
	reply    func(n int, w http.ResponseWriter)
	requests int
}

func newMessagesServer(t *testing.T, reply func(n int, w http.ResponseWriter)) *messagesServer {
	t.Helper()
	s := &messagesServer{t: t, reply: reply}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		s.mu.Lock()
		s.requests++
		n := s.requests
		s.bodies, s.raw, s.headers = append(s.bodies, body), append(s.raw, raw), append(s.headers, r.Header.Clone())
		s.mu.Unlock()
		s.reply(n, w)
	}))
	t.Cleanup(s.server.Close)
	return s
}

func messageReply(content []map[string]any, usage map[string]any) func(int, http.ResponseWriter) {
	return func(_ int, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": "msg_1", "type": "message", "role": "assistant", "model": "claude-sonnet-5-5",
			"content": content, "stop_reason": "end_turn", "usage": usage,
		})
	}
}

func textBlock(text string) map[string]any { return map[string]any{"type": "text", "text": text} }

func thinkingBlock(text string) map[string]any {
	return map[string]any{"type": "thinking", "thinking": text, "signature": "sig"}
}

func newAnthropic(t *testing.T, s *messagesServer, key string, mutate func(*inference.AnthropicConfig)) *inference.AnthropicBackend {
	t.Helper()
	config := inference.AnthropicConfig{BaseURL: s.server.URL, Model: "claude-sonnet-5-5", APIKey: key, Timeout: 5 * time.Second}
	if mutate != nil {
		mutate(&config)
	}
	return inference.NewAnthropicBackend(config)
}

func TestAnthropicBackendSendsAMessagesRequest(t *testing.T) {
	key := testKey(t)
	s := newMessagesServer(t, messageReply([]map[string]any{textBlock("ok")}, map[string]any{"input_tokens": 10, "output_tokens": 2}))
	backend := newAnthropic(t, s, key, nil)
	reply, err := backend.CompleteMessages(context.Background(), []runtime.Message{
		{Role: "system", Content: "You are AOS."},
		{Role: "user", Content: "task header"},
		{Role: "tool", Content: "tool result"},
		{Role: "assistant", Content: "an action"},
		{Role: "user", Content: ""},
		{Role: "user", Content: " \n"},
		{Role: "user", Content: "next result"},
	})
	if err != nil || reply != "ok" {
		t.Fatalf("reply %q, err %v", reply, err)
	}
	header, body := s.headers[0], s.bodies[0]
	if header.Get("x-api-key") != key || header.Get("anthropic-version") != "2026-10-21" || header.Get("Authorization") != "" {
		t.Fatalf("headers: version %q, key sent %t, authorization %q", header.Get("anthropic-version"), header.Get("x-api-key") == key, header.Get("Authorization"))
	}
	if header.Get("anthropic-beta") != "server-side-fallback-2026-07-01" || body["fallbacks"] != "default" {
		t.Fatalf("fallbacks: beta %q, field %v", header.Get("anthropic-beta"), body["fallbacks"])
	}
	if body["model"] != "claude-sonnet-5-5" || body["max_tokens"] != float64(inference.DefaultAnthropicMaxTokens) || body["system"] != "You are AOS." {
		t.Fatalf("body: %v", body)
	}
	for _, field := range []string{"temperature", "top_p", "top_k", "reasoning_budget_tokens", "stream"} {
		if _, ok := body[field]; ok {
			t.Fatalf("%s should not be sent: %v", field, body)
		}
	}
	thinking, _ := json.Marshal(body["thinking"])
	cache, _ := json.Marshal(body["cache_control"])
	if string(thinking) != `{"display":"summarized","type":"adaptive"}` || string(cache) != `{"type":"ephemeral"}` {
		t.Fatalf("thinking %s, cache_control %s", thinking, cache)
	}
	messages, _ := json.Marshal(body["messages"])
	want := `[{"content":"task header\n\ntool result","role":"user"},{"content":"an action","role":"assistant"},{"content":"next result","role":"user"}]`
	if string(messages) != want {
		t.Fatalf("messages:\n got %s\nwant %s", messages, want)
	}
}

func TestAnthropicBackendSendsNoFallbacksForOtherModels(t *testing.T) {
	s := newMessagesServer(t, messageReply([]map[string]any{textBlock("ok")}, nil))
	backend := newAnthropic(t, s, testKey(t), func(c *inference.AnthropicConfig) { c.Model = "claude-haiku-5-5"; c.MaxTokens = 4000 })
	if _, err := backend.CompleteText(context.Background(), "system", "user"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.bodies[0]["fallbacks"]; ok || s.headers[0].Get("anthropic-beta") != "" || s.bodies[0]["max_tokens"] != float64(4000) {
		t.Fatalf("body %v, beta %q", s.bodies[0], s.headers[0].Get("anthropic-beta"))
	}
}

// A thinking block that holds an action is never the action: the reply is
// the text blocks, and the thinking is recorded as reasoning.
func TestAnthropicBackendKeepsThinkingApartFromTheReply(t *testing.T) {
	s := newMessagesServer(t, messageReply([]map[string]any{
		thinkingBlock(`{"action":{"type":"exit","exit_status":{"code":0,"reason":"from thinking"}}}`),
		textBlock(`{"action":{"type":"message","message":"from the reply"}}`),
	}, map[string]any{"input_tokens": 10, "output_tokens": 30}))
	backend := newAnthropic(t, s, testKey(t), nil)
	path := filepath.Join(t.TempDir(), runlog.Dir, runlog.CallsFile)
	log, err := runlog.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	backend.SetCallLog(log)
	result, err := backend.Generate(context.Background(), runtime.GenerateRequest{
		Process: kernel.Process{PID: "pid-1", Role: "worker"},
		History: []runtime.Message{{Role: "user", Content: "go"}},
	})
	if err != nil || result.Action.Type != "message" || result.Action.Message != "from the reply" {
		t.Fatalf("result %#v, err %v", result, err)
	}
	records, err := runlog.Read[inference.CallRecord](path)
	if err != nil {
		t.Fatal(err)
	}
	outcome := records[len(records)-1]
	if outcome.Response != `{"action":{"type":"message","message":"from the reply"}}` || !strings.Contains(outcome.Reasoning, "from thinking") {
		t.Fatalf("outcome response %q reasoning %q", outcome.Response, outcome.Reasoning)
	}
}

func sse(w http.ResponseWriter, events ...map[string]any) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, event := range events {
		data, _ := json.Marshal(event)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], data)
	}
}

func delta(index int, kind, field, text string) map[string]any {
	return map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]any{"type": kind, field: text}}
}

// Streamed, thinking deltas go to OnStreamEvent as thinking and are kept
// out of the reply, unlike the chat-completions driver, which writes
// reasoning into the text it returns.
func TestAnthropicBackendStreamsThinkingApart(t *testing.T) {
	s := newMessagesServer(t, func(_ int, w http.ResponseWriter) {
		sse(w,
			map[string]any{"type": "message_start", "message": map[string]any{"model": "claude-sonnet-5-5", "usage": map[string]any{"input_tokens": 5, "cache_read_input_tokens": 40, "output_tokens": 1}}},
			map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "thinking", "thinking": ""}},
			delta(0, "thinking_delta", "thinking", `{"action":{"type":"exit","exit_status":{"code":0}}}`),
			delta(0, "signature_delta", "signature", "sig"),
			map[string]any{"type": "content_block_stop", "index": 0},
			map[string]any{"type": "ping"},
			map[string]any{"type": "content_block_start", "index": 1, "content_block": map[string]any{"type": "text", "text": ""}},
			delta(1, "text_delta", "text", "I am done "),
			delta(1, "text_delta", "text", "with the task."),
			map[string]any{"type": "content_block_stop", "index": 1},
			map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn"}, "usage": map[string]any{"output_tokens": 25}},
			map[string]any{"type": "message_stop"},
		)
	})
	var events []inference.StreamEvent
	backend := newAnthropic(t, s, testKey(t), func(c *inference.AnthropicConfig) {
		c.OnStreamEvent = func(e inference.StreamEvent) { events = append(events, e) }
	})
	reply, err := backend.CompleteText(context.Background(), "system", "user")
	if err != nil || reply != "I am done with the task." {
		t.Fatalf("reply %q, err %v", reply, err)
	}
	if s.bodies[0]["stream"] != true || s.headers[0].Get("Accept") != "text/event-stream" {
		t.Fatalf("not a streamed request: %v", s.bodies[0])
	}
	var kinds []string
	for _, e := range events {
		kinds = append(kinds, string(e.Kind))
	}
	if got := strings.Join(kinds, ","); got != "thinking,content,content,done" {
		t.Fatalf("events %s", got)
	}
	if got, want := backend.Usage(), (inference.TokenUsage{Requests: 1, PromptTokens: 45, CompletionTokens: 25, CachedTokens: 40}); got != want {
		t.Fatalf("usage %+v, want %+v", got, want)
	}
}

func TestAnthropicBackendCountsCacheReadsAndWrites(t *testing.T) {
	s := newMessagesServer(t, messageReply([]map[string]any{textBlock("ok")},
		map[string]any{"input_tokens": 100, "output_tokens": 20, "cache_read_input_tokens": 900, "cache_creation_input_tokens": 50}))
	backend := newAnthropic(t, s, testKey(t), nil)
	path := filepath.Join(t.TempDir(), runlog.Dir, runlog.CallsFile)
	log, _ := runlog.Open(path)
	backend.SetCallLog(log)
	meter := &runtime.TokenMeter{}
	ctx := runtime.WithCallInfo(context.Background(), runtime.CallInfo{PID: "pid-1", Meter: meter})
	for range 2 {
		if _, err := backend.CompleteText(ctx, "system", "user"); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := backend.Usage(), (inference.TokenUsage{Requests: 2, PromptTokens: 2100, CompletionTokens: 40, CachedTokens: 1800, CacheCreationTokens: 100}); got != want {
		t.Fatalf("usage %+v, want %+v", got, want)
	}
	if meter.Total() != 2*1070 {
		t.Fatalf("meter %d", meter.Total())
	}
	records, _ := runlog.Read[inference.CallRecord](path)
	outcome := records[1]
	if outcome.PromptTokens != 1050 || outcome.CachedTokens != 900 || outcome.CacheCreationTokens != 50 || outcome.CompletionTokens != 20 || outcome.Model != "claude-sonnet-5-5" {
		t.Fatalf("outcome %+v", outcome)
	}
}

// Each error is one request: retrying is the runtime's.
func TestAnthropicBackendErrors(t *testing.T) {
	key := testKey(t)
	cases := []struct {
		status            int
		kind, retryAfter  string
		temporary         bool
		wantInErrorString []string
	}{
		{http.StatusBadRequest, "invalid_request_error", "", false, []string{"400 Bad Request", "invalid_request_error", "prompt is too long"}},
		{http.StatusTooManyRequests, "rate_limit_error", "17", true, []string{"429 Too Many Requests", "rate_limit_error", "retry-after 17", "request-id req_test"}},
		{529, "overloaded_error", "", true, []string{"529", "overloaded_error"}},
	}
	for _, c := range cases {
		s := newMessagesServer(t, func(_ int, w http.ResponseWriter) {
			w.Header().Set("request-id", "req_test")
			if c.retryAfter != "" {
				w.Header().Set("retry-after", c.retryAfter)
			}
			w.WriteHeader(c.status)
			json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": map[string]any{"type": c.kind, "message": "prompt is too long"}})
		})
		backend := newAnthropic(t, s, key, nil)
		path := filepath.Join(t.TempDir(), runlog.Dir, runlog.CallsFile)
		log, _ := runlog.Open(path)
		backend.SetCallLog(log)
		_, err := backend.CompleteText(context.Background(), "system", "user")
		var failure *inference.AnthropicError
		if !errors.As(err, &failure) || failure.StatusCode != c.status || failure.Type != c.kind || failure.Temporary() != c.temporary {
			t.Fatalf("%d: err %v", c.status, err)
		}
		for _, want := range c.wantInErrorString {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("%d: %q lacks %q", c.status, err, want)
			}
		}
		if s.requests != 1 {
			t.Fatalf("%d: %d requests, want 1", c.status, s.requests)
		}
		ledger, _ := os.ReadFile(path)
		if strings.Contains(err.Error(), key) || strings.Contains(string(ledger), key) || !strings.Contains(string(ledger), c.kind) {
			t.Fatalf("%d: the key reached the error or the ledger, or the ledger lacks the error", c.status)
		}
	}
}

func TestAnthropicBackendStreamError(t *testing.T) {
	s := newMessagesServer(t, func(_ int, w http.ResponseWriter) {
		sse(w,
			map[string]any{"type": "message_start", "message": map[string]any{"model": "claude-sonnet-5-5", "usage": map[string]any{"input_tokens": 5}}},
			map[string]any{"type": "error", "error": map[string]any{"type": "overloaded_error", "message": "Overloaded"}},
		)
	})
	backend := newAnthropic(t, s, testKey(t), func(c *inference.AnthropicConfig) { c.Stream = true })
	_, err := backend.CompleteText(context.Background(), "system", "user")
	var failure *inference.AnthropicError
	if !errors.As(err, &failure) || failure.Type != "overloaded_error" {
		t.Fatalf("err %v", err)
	}
	if usage := backend.Usage(); usage != (inference.TokenUsage{}) {
		t.Fatalf("a failed stream should count nothing: %+v", usage)
	}

	cut := newMessagesServer(t, func(_ int, w http.ResponseWriter) {
		sse(w, map[string]any{"type": "message_start", "message": map[string]any{"model": "claude-sonnet-5-5"}}, delta(0, "text_delta", "text", "partial"))
	})
	backend = newAnthropic(t, cut, testKey(t), func(c *inference.AnthropicConfig) { c.Stream = true })
	if _, err := backend.CompleteText(context.Background(), "system", "user"); !errors.Is(err, inference.ErrMalformedModelResponse) {
		t.Fatalf("a stream without message_stop: err %v", err)
	}
}

func TestAnthropicBackendRefusal(t *testing.T) {
	s := newMessagesServer(t, func(_ int, w http.ResponseWriter) {
		json.NewEncoder(w).Encode(map[string]any{
			"type": "message", "model": "claude-sonnet-5-5", "content": []any{}, "stop_reason": "refusal",
			"stop_details": map[string]any{"type": "refusal", "category": "cyber", "explanation": "declined"},
			"usage":        map[string]any{"input_tokens": 10, "output_tokens": 0},
		})
	})
	backend := newAnthropic(t, s, testKey(t), nil)
	_, err := backend.CompleteText(context.Background(), "system", "user")
	if !errors.Is(err, inference.ErrModelRefused) || !strings.Contains(err.Error(), `"cyber"`) {
		t.Fatalf("err %v", err)
	}
	if backend.Usage().Requests != 1 {
		t.Fatalf("a refusal is still a counted request: %+v", backend.Usage())
	}
}

// max_tokens is the configured value, lowered to what is left of the
// token budget.
func TestAnthropicBackendMaxTokens(t *testing.T) {
	s := newMessagesServer(t, messageReply([]map[string]any{textBlock("ok")}, nil))
	cases := []struct{ configured, left, want int }{
		{0, 0, inference.DefaultAnthropicMaxTokens},
		{0, 500, 500},
		{4000, 20000, 4000},
		{4000, 1200, 1200},
	}
	for i, c := range cases {
		backend := newAnthropic(t, s, testKey(t), func(config *inference.AnthropicConfig) { config.MaxTokens = c.configured })
		ctx := runtime.WithCallInfo(context.Background(), runtime.CallInfo{MaxTokens: c.left})
		if _, err := backend.CompleteText(ctx, "system", "user"); err != nil {
			t.Fatal(err)
		}
		if got := s.bodies[i]["max_tokens"]; got != float64(c.want) {
			t.Fatalf("configured %d, left %d: sent %v, want %d", c.configured, c.left, got, c.want)
		}
	}
}

func TestAnthropicBackendRefusesTranscriptsTheAPIWould(t *testing.T) {
	s := newMessagesServer(t, messageReply([]map[string]any{textBlock("ok")}, nil))
	backend := newAnthropic(t, s, testKey(t), nil)
	for _, transcript := range [][]runtime.Message{
		{{Role: "system", Content: "s"}, {Role: "assistant", Content: "first"}, {Role: "user", Content: "u"}},
		{{Role: "system", Content: "s"}, {Role: "user", Content: "u"}, {Role: "assistant", Content: "a prefill"}},
		{{Role: "system", Content: "s"}},
	} {
		if _, err := backend.CompleteMessages(context.Background(), transcript); err == nil {
			t.Fatalf("accepted %v", transcript)
		}
	}
	if s.requests != 0 {
		t.Fatalf("%d requests sent", s.requests)
	}
}

// The ledger rebuilds every Anthropic body byte for byte, through the base
// chain, and a replayer answering from it is sent the same bytes.
func TestAnthropicCallLedgerRebuildsEachRequest(t *testing.T) {
	s := newMessagesServer(t, messageReply([]map[string]any{thinkingBlock("plan"), textBlock("reply")},
		map[string]any{"input_tokens": 10, "output_tokens": 3, "cache_read_input_tokens": 7}))
	backend := newAnthropic(t, s, testKey(t), nil)
	path := filepath.Join(t.TempDir(), runlog.Dir, runlog.CallsFile)
	log, _ := runlog.Open(path)
	backend.SetCallLog(log)
	ctx := runtime.WithCallInfo(context.Background(), runtime.CallInfo{PID: "pid-1"})
	history := []runtime.Message{{Role: "system", Content: "s"}, {Role: "user", Content: "first"}}
	for range 3 {
		reply, err := backend.CompleteMessages(ctx, history)
		if err != nil {
			t.Fatal(err)
		}
		history = append(history, runtime.Message{Role: "assistant", Content: reply}, runtime.Message{Role: "user", Content: "more"})
	}
	records, err := runlog.Read[inference.CallRecord](path)
	if err != nil {
		t.Fatal(err)
	}
	var starts []inference.CallRecord
	for i, record := range records {
		if record.Phase != inference.PhaseStart {
			continue
		}
		starts = append(starts, record)
		if record.Params.API != inference.APIAnthropicMessages {
			t.Fatalf("api %q", record.Params.API)
		}
		body, err := inference.RebuildRequestBody(record.Params, inference.RequestMessages(records, i))
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(body)
		if hex.EncodeToString(digest[:]) != record.RequestSHA256 || string(body) != string(s.raw[len(starts)-1]) {
			t.Fatalf("call %d: the rebuilt body differs:\n%s\n%s", len(starts), body, s.raw[len(starts)-1])
		}
	}
	if len(starts) != 3 || starts[1].Base != starts[0].Seq || starts[2].Base != starts[1].Seq {
		t.Fatalf("calls do not extend each other: %+v", starts)
	}

	player := &ledgerPlayer{records: records}
	replayed := inference.NewAnthropicBackend(inference.AnthropicConfig{BaseURL: starts[0].Backend, Model: starts[0].Params.Model, MaxTokens: starts[0].Params.MaxTokens, Replayer: player})
	history = []runtime.Message{{Role: "system", Content: "s"}, {Role: "user", Content: "first"}}
	for range 3 {
		reply, err := replayed.CompleteMessages(ctx, history)
		if err != nil || reply != "reply" {
			t.Fatalf("replayed reply %q, err %v", reply, err)
		}
		history = append(history, runtime.Message{Role: "assistant", Content: reply}, runtime.Message{Role: "user", Content: "more"})
	}
	if got := replayed.Usage(); got != backend.Usage() {
		t.Fatalf("replayed usage %+v, recorded %+v", got, backend.Usage())
	}
}

// ledgerPlayer answers each call from the ledger once its body hashes as
// recorded, as aos replay's player does.
type ledgerPlayer struct {
	records []inference.CallRecord
	next    int
}

func (p *ledgerPlayer) Reply(_ context.Context, body []byte) (inference.RecordedReply, error) {
	for p.next < len(p.records) && p.records[p.next].Phase != inference.PhaseStart {
		p.next++
	}
	if p.next >= len(p.records) {
		return inference.RecordedReply{}, errors.New("no more calls")
	}
	start := p.records[p.next]
	p.next++
	digest := sha256.Sum256(body)
	if hex.EncodeToString(digest[:]) != start.RequestSHA256 {
		return inference.RecordedReply{}, fmt.Errorf("call %d diverged", start.Seq)
	}
	for _, record := range p.records {
		if record.Phase == inference.PhaseOutcome && record.Start == start.Seq {
			return inference.RecordedReply{Content: record.Response, Reasoning: record.Reasoning, PromptTokens: record.PromptTokens,
				CompletionTokens: record.CompletionTokens, CachedTokens: record.CachedTokens, CacheCreationTokens: record.CacheCreationTokens}, nil
		}
	}
	return inference.RecordedReply{}, errors.New("no outcome")
}

func TestLoadBackendConfigSelectsAnthropic(t *testing.T) {
	key := testKey(t)
	for _, name := range []string{"AOS_LLM_BASE_URL", "AOS_VLLM_BASE_URL", "AOS_LLM_MODEL", "AOS_VLLM_MODEL", "AOS_VLLM_API_KEY"} {
		t.Setenv(name, "")
	}
	t.Setenv("AOS_LLM_BACKEND", "anthropic")
	t.Setenv("AOS_LLM_API_KEY", "meant-for-a-local-server")
	t.Setenv("ANTHROPIC_API_KEY", key)
	config := inference.LoadBackendConfig(time.Minute)
	if config.Kind != inference.BackendAnthropic || config.BaseURL != "https://api.anthropic.com" || config.Model != "claude-sonnet-5-5" || config.APIKey != key {
		t.Fatalf("config kind %q base %q model %q, key from ANTHROPIC_API_KEY %t", config.Kind, config.BaseURL, config.Model, config.APIKey == key)
	}
	if _, ok := inference.NewBackendFromConfig(config, inference.BackendOptions{}).(*inference.AnthropicBackend); !ok {
		t.Fatal("not the anthropic driver")
	}
	if profile := inference.EnvProfile("env", config); profile.APIKeyEnv != "ANTHROPIC_API_KEY" {
		t.Fatalf("env profile reads %s", profile.APIKeyEnv)
	}
	t.Setenv("ANTHROPIC_API_KEY", "")
	if config := inference.LoadBackendConfig(time.Minute); config.APIKey != "" {
		t.Fatal("AOS_LLM_API_KEY was used for the Anthropic API")
	}
	t.Setenv("AOS_LLM_BACKEND", "vllm")
	if _, ok := inference.NewBackendFromConfig(inference.LoadBackendConfig(time.Minute), inference.BackendOptions{}).(*inference.VLLMBackend); !ok {
		t.Fatal("vllm should build the chat-completions driver")
	}
}

func TestLoadProfilesGivesAnAnthropicProfileItsBaseURL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	os.WriteFile(path, []byte(`{"profiles":[{"name":"claude","backend":"anthropic","model":"claude-sonnet-5-5","api_key_env":"ANTHROPIC_API_KEY"}]}`), 0o600)
	profiles, err := inference.LoadProfiles(path)
	if err != nil {
		t.Fatal(err)
	}
	if profiles[0].Backend != inference.BackendAnthropic || profiles[0].BaseURL != "https://api.anthropic.com" {
		t.Fatalf("profile %+v", profiles[0])
	}
	os.WriteFile(path, []byte(`{"profiles":[{"name":"local","backend":"vllm","model":"m"}]}`), 0o600)
	if _, err := inference.LoadProfiles(path); err == nil {
		t.Fatal("a vllm profile without base_url should be refused")
	}
}

// The recorded params are the sent body's fields other than its messages,
// for each wire format: no field the request did not carry, and none it
// carried left out. An outcome records no params.
func TestRecordedParamsAreWhatWasSent(t *testing.T) {
	for _, name := range []string{"AOS_LLM_API_KEY", "AOS_VLLM_API_KEY", "ANTHROPIC_API_KEY"} {
		t.Setenv(name, "")
	}
	var mu sync.Mutex
	var bodies []map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		if r.URL.Path == "/v1/messages" {
			json.NewEncoder(w).Encode(map[string]any{"model": "claude-sonnet-5-5", "content": []any{textBlock("ok")}, "stop_reason": "end_turn"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": "ok"}}}})
	}))
	defer srv.Close()
	cases := []struct {
		name    string
		backend inference.Backend
	}{
		{"chat completions, temperature 0", inference.NewVLLMBackend(inference.VLLMConfig{BaseURL: srv.URL, Model: "m"})},
		{"chat completions, sampling set", inference.NewVLLMBackend(inference.VLLMConfig{BaseURL: srv.URL, Model: "m", Stream: true,
			Sampling: inference.Sampling{Temperature: 0.6, TopP: 0.95, MaxTokens: 512, ThinkingBudget: 256}})},
		{"anthropic", inference.NewAnthropicBackend(inference.AnthropicConfig{BaseURL: srv.URL, Model: "claude-sonnet-5-5", MaxTokens: 4000})},
		{"anthropic, streamed", inference.NewAnthropicBackend(inference.AnthropicConfig{BaseURL: srv.URL, Model: "claude-haiku-5-5", Stream: true})},
	}
	for i, c := range cases {
		path := filepath.Join(t.TempDir(), runlog.Dir, runlog.CallsFile)
		log, _ := runlog.Open(path)
		c.backend.SetCallLog(log)
		ctx := runtime.WithCallInfo(context.Background(), runtime.CallInfo{PID: "pid-1"})
		_, _ = c.backend.CompleteText(ctx, "system", "user")
		lines, _ := os.ReadFile(path)
		var start, outcome map[string]json.RawMessage
		parts := strings.Split(strings.TrimSpace(string(lines)), "\n")
		json.Unmarshal([]byte(parts[0]), &start)
		json.Unmarshal([]byte(parts[len(parts)-1]), &outcome)
		var recorded map[string]json.RawMessage
		json.Unmarshal(start["params"], &recorded)
		delete(recorded, "api")
		sent := map[string]json.RawMessage{}
		for key, value := range bodies[i] {
			if key != "messages" && key != "system" {
				sent[key] = value
			}
		}
		got, _ := json.Marshal(recorded)
		want, _ := json.Marshal(sent)
		if string(got) != string(want) {
			t.Fatalf("%s: recorded params %s, sent %s", c.name, got, want)
		}
		if _, ok := outcome["params"]; ok {
			t.Fatalf("%s: the outcome records params: %s", c.name, outcome["params"])
		}
	}
}

// Ledgers written before temperature was recorded only when sent still
// rebuild byte for byte: a chat call with its temperature, and an
// Anthropic call that recorded a temperature it never sent.
func TestRecordsFromBeforeTheParamsFixRebuild(t *testing.T) {
	chat := `{"seq":1,"phase":"start","pid":"pid-1","backend":"http://x","model":"m","prompt_tokens":0,"cached_tokens":0,"completion_tokens":0,"latency_ms":0,` +
		`"params":{"model":"m","temperature":0.6,"max_tokens":512},"messages":[{"role":"system","content":"s"},{"role":"user","content":"u"}],"request_sha256":""}`
	anthropic := `{"seq":1,"phase":"start","pid":"pid-1","backend":"http://x","model":"claude-sonnet-5-5","prompt_tokens":0,"cached_tokens":0,"completion_tokens":0,"latency_ms":0,` +
		`"params":{"api":"anthropic-messages","model":"claude-sonnet-5-5","temperature":0,"max_tokens":4000,"thinking":{"type":"adaptive","display":"summarized"},"cache_control":{"type":"ephemeral"},"fallbacks":"default"},` +
		`"messages":[{"role":"system","content":"s"},{"role":"user","content":"u"}],"request_sha256":""}`
	for _, c := range []struct{ line, want string }{
		{chat, `{"model":"m","messages":[{"role":"system","content":"s"},{"role":"user","content":"u"}],"temperature":0.6,"max_tokens":512}`},
		{anthropic, `{"model":"claude-sonnet-5-5","max_tokens":4000,"system":"s","messages":[{"role":"user","content":"u"}],"thinking":{"type":"adaptive","display":"summarized"},"cache_control":{"type":"ephemeral"},"fallbacks":"default"}`},
	} {
		var record inference.CallRecord
		if err := json.Unmarshal([]byte(c.line), &record); err != nil {
			t.Fatal(err)
		}
		body, err := inference.RebuildRequestBody(record.Params, record.Messages)
		if err != nil || string(body) != c.want {
			t.Fatalf("rebuilt %s, want %s (err %v)", body, c.want, err)
		}
	}
	var outcome inference.CallRecord
	if err := json.Unmarshal([]byte(`{"seq":2,"phase":"outcome","start":1,"params":{"model":"","temperature":0},"request_sha256":""}`), &outcome); err != nil || outcome.Start != 1 {
		t.Fatalf("an old outcome line: %+v, %v", outcome, err)
	}
}
