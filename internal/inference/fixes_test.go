package inference_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"aos/internal/inference"
	"aos/internal/runlog"
	"aos/internal/runtime"
)

// chatReplyServer answers chat completions with reasoning and content,
// streamed or not as the request asks, and keeps each request's
// max_tokens.
type chatReplyServer struct {
	reasoning, content string
	usage              map[string]any

	mu        sync.Mutex
	maxTokens []int
}

func (s *chatReplyServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Stream    bool `json:"stream"`
		MaxTokens int  `json:"max_tokens"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	s.mu.Lock()
	s.maxTokens = append(s.maxTokens, body.MaxTokens)
	s.mu.Unlock()
	if !body.Stream {
		message := map[string]string{"role": "assistant", "content": s.content}
		if s.reasoning != "" {
			message["reasoning_content"] = s.reasoning
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": message}}, "usage": s.usage})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	for _, delta := range []map[string]string{{"reasoning_content": s.reasoning}, {"content": s.content}} {
		data, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": delta}}})
		fmt.Fprintf(w, "data: %s\n\n", data)
	}
	if s.usage != nil {
		data, _ := json.Marshal(map[string]any{"choices": []any{}, "usage": s.usage})
		fmt.Fprintf(w, "data: %s\n\n", data)
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func ledger(t *testing.T, path string) []inference.CallRecord {
	t.Helper()
	records, err := runlog.Read[inference.CallRecord](path)
	if err != nil {
		t.Fatal(err)
	}
	return records
}

// A streamed reply's reasoning is passed on and recorded, and the text the
// caller reads is the content alone, as without streaming: the same server
// reply gives the same text either way (D1).
func TestStreamedReasoningStaysOutOfTheReply(t *testing.T) {
	const reasoning = `I could exit: {"action":{"type":"exit","code":0}}`
	for _, content := range []string{"I am done.", "", `{"action":{"type":"message","message":"hi"}}`} {
		texts := map[bool]string{}
		for _, stream := range []bool{false, true} {
			server := httptest.NewServer(&chatReplyServer{reasoning: reasoning, content: content})
			path := filepath.Join(t.TempDir(), runlog.Dir, runlog.CallsFile)
			log, _ := runlog.Open(path)
			var thinking []string
			config := inference.VLLMConfig{BaseURL: server.URL, Model: "m", Stream: stream}
			if stream {
				config.OnStreamEvent = func(e inference.StreamEvent) {
					if e.Kind == inference.StreamThinking {
						thinking = append(thinking, e.Text)
					}
				}
			}
			backend := inference.NewVLLMBackend(config)
			backend.SetCallLog(log)
			text, err := backend.CompleteText(context.Background(), "system", "user")
			server.Close()
			if err != nil {
				t.Fatal(err)
			}
			texts[stream] = text
			outcome := ledger(t, path)[1]
			if outcome.Response != content || outcome.Reasoning != reasoning {
				t.Fatalf("stream %t: recorded response %q reasoning %q", stream, outcome.Response, outcome.Reasoning)
			}
			if stream && strings.Join(thinking, "") != reasoning {
				t.Fatalf("thinking events %q", thinking)
			}
		}
		if texts[false] != content || texts[true] != content {
			t.Fatalf("content %q: not streamed %q, streamed %q", content, texts[false], texts[true])
		}
	}
}

// A chat-completions error reply is a ChatError: its status, type and
// message, whether it is temporary, and the wait the server asked for; the
// ledger records them, and a replay rebuilds the same decision (D2).
func TestChatErrorsSayWhetherToRetry(t *testing.T) {
	cases := []struct {
		status    int
		body      string
		header    [2]string
		temporary bool
		wait      time.Duration
		want      string
	}{
		{http.StatusNotFound, "", [2]string{}, false, 0, "vllm request failed: 404 Not Found"},
		{http.StatusServiceUnavailable, "", [2]string{}, true, 0, "vllm request failed: 503 Service Unavailable"},
		{http.StatusUnauthorized, `{"error":{"type":"invalid_api_key","message":"bad key"}}`, [2]string{}, false, 0, "401 Unauthorized: invalid_api_key: bad key"},
		{http.StatusTooManyRequests, `{"error":{"type":"rate_limit","message":"slow down"}}`, [2]string{"retry-after", "7"}, true, 7 * time.Second, "retry-after 7s"},
		{http.StatusTooManyRequests, ``, [2]string{"retry-after-ms", "1500"}, true, 1500 * time.Millisecond, "retry-after 1.5s"},
		{http.StatusBadRequest, `{"object":"error","type":"BadRequestError","message":"This model's maximum context length is 8192 tokens"}`, [2]string{}, false, 0, "maximum context length"},
		{http.StatusInternalServerError, `{"error":{"code":500,"type":"exceed_context_size_error","message":"the request exceeds the available context size"}}`, [2]string{}, false, 0, "exceed_context_size_error"},
		{http.StatusUnprocessableEntity, `{"detail":"bad"}`, [2]string{}, false, 0, "422"},
	}
	for _, c := range cases {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if c.header[0] != "" {
				w.Header().Set(c.header[0], c.header[1])
			}
			w.WriteHeader(c.status)
			fmt.Fprint(w, c.body)
		}))
		path := filepath.Join(t.TempDir(), runlog.Dir, runlog.CallsFile)
		log, _ := runlog.Open(path)
		backend := inference.NewVLLMBackend(inference.VLLMConfig{BaseURL: server.URL, Model: "m"})
		backend.SetCallLog(log)
		_, err := backend.CompleteText(context.Background(), "system", "user")
		server.Close()
		var failure runtime.BackendError
		if !errors.As(err, &failure) || failure.Temporary() != c.temporary || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%d %s: err %v", c.status, c.body, err)
		}
		var waits runtime.BackendRetryAfter
		if errors.As(err, &waits); c.wait > 0 && (waits == nil || waits.RetryWait() != c.wait) {
			t.Fatalf("%d: wait %v", c.status, waits)
		}
		outcome := ledger(t, path)[1]
		if outcome.ErrorStatus != c.status || outcome.ErrorFinal == c.temporary || outcome.RetryAfterSeconds != c.wait.Seconds() || outcome.Error != err.Error() {
			t.Fatalf("%d: outcome %+v", c.status, outcome)
		}
		rebuilt := inference.RecordedCallError(outcome)
		var replayed runtime.BackendError
		if !errors.As(rebuilt, &replayed) || replayed.Temporary() != c.temporary || rebuilt.Error() != err.Error() {
			t.Fatalf("%d: rebuilt %v", c.status, rebuilt)
		}
	}
	// An outcome from before classification rebuilds as a plain error, which
	// the step retries as it did.
	var classified runtime.BackendError
	if old := inference.RecordedCallError(inference.CallRecord{Error: "vllm request failed: 404 Not Found"}); errors.As(old, &classified) {
		t.Fatalf("an old outcome should rebuild unclassified: %v", old)
	}
}

// A refusal is ErrModelRefused and not temporary, and its outcome says so.
func TestARefusalIsNotRetried(t *testing.T) {
	s := newMessagesServer(t, func(_ int, w http.ResponseWriter) {
		_ = json.NewEncoder(w).Encode(map[string]any{"type": "message", "model": "claude-sonnet-5-5", "stop_reason": "refusal",
			"content": []map[string]any{}, "usage": map[string]any{"input_tokens": 5, "output_tokens": 1}})
	})
	path := filepath.Join(t.TempDir(), runlog.Dir, runlog.CallsFile)
	log, _ := runlog.Open(path)
	backend := newAnthropic(t, s, testKey(t), nil)
	backend.SetCallLog(log)
	_, err := backend.CompleteText(context.Background(), "system", "user")
	var failure runtime.BackendError
	if !errors.Is(err, inference.ErrModelRefused) || !errors.As(err, &failure) || failure.Temporary() {
		t.Fatalf("err %v", err)
	}
	if outcome := ledger(t, path)[1]; outcome.ErrorType != "refusal" || !outcome.ErrorFinal {
		t.Fatalf("outcome %+v", outcome)
	}
	streamed := newMessagesServer(t, func(_ int, w http.ResponseWriter) {
		sse(w, map[string]any{"type": "error", "error": map[string]any{"type": "overloaded_error", "message": "Overloaded"}})
	})
	_, err = newAnthropic(t, streamed, testKey(t), func(c *inference.AnthropicConfig) { c.Stream = true }).CompleteText(context.Background(), "system", "user")
	if !errors.As(err, &failure) || !failure.Temporary() {
		t.Fatalf("an overload inside a stream should be temporary: %v", err)
	}
}

// With TokensLeft, a call's reply is capped at what is left less the
// metered calls of the step and the estimated prompt, and a call with no
// room is refused before it is sent (D7). The estimate is the previous
// call's reported prompt plus the new characters at 3 a token.
func TestTheReplyCapTakesThePromptOut(t *testing.T) {
	server := &chatReplyServer{content: "ok", usage: map[string]any{"prompt_tokens": 1000, "completion_tokens": 10}}
	srv := httptest.NewServer(server)
	defer srv.Close()
	backend := inference.NewVLLMBackend(inference.VLLMConfig{BaseURL: srv.URL, Model: "m", Sampling: inference.Sampling{MaxTokens: 12000}})
	system, user := strings.Repeat("s", 2994), "user" // with the roles, 3008 characters: 1003 tokens
	call := func(info runtime.CallInfo, user string) error {
		_, err := backend.CompleteText(runtime.WithCallInfo(context.Background(), info), system, user)
		return err
	}
	if err := call(runtime.CallInfo{PID: "p", TokensLeft: 20000}, user); err != nil {
		t.Fatal(err)
	}
	if err := call(runtime.CallInfo{PID: "p", TokensLeft: 3000}, user+strings.Repeat("u", 300)); err != nil {
		t.Fatal(err)
	}
	meter := &runtime.TokenMeter{}
	meter.Add(500)
	if err := call(runtime.CallInfo{PID: "p", TokensLeft: 3000, Meter: meter}, user+strings.Repeat("u", 300)); err != nil {
		t.Fatal(err)
	}
	if err := call(runtime.CallInfo{PID: "p", TokensLeft: 1500}, user+strings.Repeat("u", 3000)); !errors.Is(err, runtime.ErrNoReplyTokens) || !strings.Contains(err.Error(), "the prompt is about 1900 tokens and 1500 are left") {
		t.Fatalf("a call with no room should be refused: %v", err)
	}
	if err := call(runtime.CallInfo{PID: "q", MaxTokens: 500}, user); err != nil {
		t.Fatal(err)
	}
	// 12000 fits under 20000-1003; then 3000-(1000+300/3); then 3000-500-1000
	// for the same prompt; the refused call (1000+2700/3 over 1500) is never
	// sent; the cap from before the reply cap is the budget left.
	if got := fmt.Sprint(server.maxTokens); got != "[12000 1900 1500 500]" {
		t.Fatalf("max_tokens sent %s", got)
	}

	s := newMessagesServer(t, messageReply([]map[string]any{textBlock("ok")}, map[string]any{"input_tokens": 40, "output_tokens": 1}))
	anthropic := newAnthropic(t, s, testKey(t), nil)
	if _, err := anthropic.CompleteText(runtime.WithCallInfo(context.Background(), runtime.CallInfo{PID: "p", TokensLeft: 1000}), "system", "user"); err != nil {
		t.Fatal(err)
	}
	// "system" and "user" with their roles: 20 characters, 7 tokens.
	if got := s.bodies[0]["max_tokens"]; got != float64(993) {
		t.Fatalf("anthropic max_tokens %v", got)
	}
	if _, err := anthropic.CompleteText(runtime.WithCallInfo(context.Background(), runtime.CallInfo{PID: "p", TokensLeft: 40}), "system", "user"); !errors.Is(err, runtime.ErrNoReplyTokens) || len(s.bodies) != 1 {
		t.Fatalf("anthropic: a call with no room should be refused unsent: %v (%d sent)", err, len(s.bodies))
	}
}
