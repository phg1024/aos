package inference

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"aos/internal/runtime"
)

// ChatError is an error reply from a chat-completions server: its status,
// the error type, code and message it gave, in the OpenAI shape
// ({"error":{...}}) or vLLM's flat one, and its retry-after. It holds
// nothing from the request.
type ChatError struct {
	StatusCode int
	Status     string
	Type       string
	Code       string
	Message    string
	// RetryAfter is the wait the server asked for: retry-after-ms or
	// retry-after, zero when neither was sent or readable.
	RetryAfter time.Duration
}

func (e *ChatError) Error() string {
	text := "vllm request failed: " + e.Status
	if e.Type != "" {
		text += ": " + e.Type
	}
	if e.Message != "" {
		text += ": " + e.Message
	}
	if e.RetryAfter > 0 {
		text += " (retry-after " + e.RetryAfter.String() + ")"
	}
	return text
}

// Temporary reports whether the same request may succeed later, as
// temporaryFailure decides.
func (e *ChatError) Temporary() bool { return temporaryFailure(e.StatusCode, e.Type, e.Message) }

// RetryWait is the wait the server asked for.
func (e *ChatError) RetryWait() time.Duration { return e.RetryAfter }

// temporaryFailure is the one rule both drivers use: a timeout (408), a
// conflict (409), a rate limit (429) and a server error or overload (5xx,
// 529 included) may pass, and so may an error event inside a stream unless
// its type says the request was refused; any other 4xx (400, 401, 403, 404,
// 413, 422, ...) will not, and neither will a server error that says the
// prompt is over the context window (a 400 already says so on most
// servers).
func temporaryFailure(status int, kind, message string) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusConflict, http.StatusTooManyRequests:
		return true
	case 0:
		// An error event inside a stream has a type and no status: an
		// overload or a server error passes, a request the server refused
		// does not.
		switch kind {
		case "invalid_request_error", "authentication_error", "permission_error", "not_found_error", "request_too_large":
			return false
		}
	}
	return (status == 0 || status >= 500) && !overContextWindow(kind, message)
}

// overContextWindow reports whether an error says the prompt does not fit
// the model's context window, in the words vLLM, llama.cpp, OpenAI and
// Anthropic use.
func overContextWindow(kind, message string) bool {
	text := strings.ToLower(kind + " " + message)
	for _, phrase := range []string{"context_length_exceeded", "exceed_context_size", "maximum context length", "context window", "context length", "prompt is too long"} {
		if strings.Contains(text, phrase) {
			return true
		}
	}
	return false
}

// parseRetryAfter reads retry-after-ms, then retry-after in seconds or as
// an HTTP date (relative to now); zero when neither is readable.
func parseRetryAfter(header http.Header, now time.Time) time.Duration {
	if ms, err := strconv.ParseFloat(strings.TrimSpace(header.Get("retry-after-ms")), 64); err == nil && ms > 0 {
		return time.Duration(ms * float64(time.Millisecond))
	}
	value := strings.TrimSpace(header.Get("retry-after"))
	if value == "" {
		return 0
	}
	if seconds, err := strconv.ParseFloat(value, 64); err == nil {
		if seconds <= 0 {
			return 0
		}
		return time.Duration(seconds * float64(time.Second))
	}
	if at, err := http.ParseTime(value); err == nil && at.After(now) {
		return at.Sub(now).Round(time.Second)
	}
	return 0
}

func readChatError(response *http.Response) error {
	failure := &ChatError{StatusCode: response.StatusCode, Status: response.Status, RetryAfter: parseRetryAfter(response.Header, time.Now())}
	raw, _ := io.ReadAll(io.LimitReader(response.Body, errorBodyLimit))
	var parsed struct {
		Error *struct {
			Type    string `json:"type"`
			Code    any    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Type    string `json:"type"`
		Code    any    `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &parsed) == nil {
		if parsed.Error != nil {
			failure.Type, failure.Message, failure.Code = parsed.Error.Type, parsed.Error.Message, codeString(parsed.Error.Code)
		} else {
			failure.Type, failure.Message, failure.Code = parsed.Type, parsed.Message, codeString(parsed.Code)
		}
	}
	return failure
}

// codeString is an error code as the server sent it: a string, or a
// number such as llama.cpp's.
func codeString(code any) string {
	switch c := code.(type) {
	case string:
		return c
	case float64:
		return strconv.FormatFloat(c, 'f', -1, 64)
	}
	return ""
}

// refusalError is a reply that stopped with stop_reason "refusal": it is
// ErrModelRefused, and not temporary.
type refusalError struct{ text string }

func (e *refusalError) Error() string        { return e.text }
func (e *refusalError) Is(target error) bool { return target == ErrModelRefused }
func (e *refusalError) Temporary() bool      { return false }

// callFailure is what a ledger outcome records of a call's error beyond
// its text: the HTTP status and the provider's error type where the server
// gave them, whether the driver said not to retry it, and the wait the
// server asked for.
type callFailure struct {
	status     int
	kind       string
	final      bool
	retryAfter time.Duration
}

func failureOf(err error) callFailure {
	var failure callFailure
	var chat *ChatError
	var anthropic *AnthropicError
	var recorded *RecordedError
	switch {
	case errors.As(err, &recorded):
		return callFailure{status: recorded.StatusCode, kind: recorded.Type, final: recorded.Final, retryAfter: recorded.Wait}
	case errors.As(err, &chat):
		failure = callFailure{status: chat.StatusCode, kind: chat.Type, retryAfter: chat.RetryAfter}
	case errors.As(err, &anthropic):
		failure = callFailure{status: anthropic.StatusCode, kind: anthropic.Type, retryAfter: anthropic.Wait}
	case errors.Is(err, ErrModelRefused):
		failure.kind = "refusal"
	}
	var classified runtime.BackendError
	if errors.As(err, &classified) {
		failure.final = !classified.Temporary()
	}
	return failure
}

// RecordedError is a ledger outcome's error as a replay returns it: the
// recorded text, and the status, type, retry decision and wait the driver
// gave it, so the step retries it or not as the run did.
type RecordedError struct {
	Text       string
	StatusCode int
	Type       string
	Final      bool
	Wait       time.Duration
}

func (e *RecordedError) Error() string { return e.Text }

// Temporary is the recorded decision.
func (e *RecordedError) Temporary() bool { return !e.Final }

// RetryWait is the recorded wait.
func (e *RecordedError) RetryWait() time.Duration { return e.Wait }

// RecordedCallError rebuilds the error an outcome records. An outcome from
// before drivers classified errors carries no status, type or decision:
// its error is plain, and the step retries it, as it did.
func RecordedCallError(outcome CallRecord) error {
	if outcome.ErrorStatus == 0 && outcome.ErrorType == "" && !outcome.ErrorFinal && outcome.RetryAfterSeconds == 0 {
		return errors.New(outcome.Error)
	}
	return &RecordedError{Text: outcome.Error, StatusCode: outcome.ErrorStatus, Type: outcome.ErrorType, Final: outcome.ErrorFinal,
		Wait: time.Duration(outcome.RetryAfterSeconds * float64(time.Second))}
}

// promptEstimator estimates the prompt tokens of a call before it is sent,
// for the reply cap: the same process's previous call's reported prompt
// tokens plus the new characters at runtime.EstimatedCharsPerToken when the
// transcript grew since, or every character at that ratio otherwise (the
// process's first call, or one after compaction).
type promptEstimator struct {
	mu   sync.Mutex
	last map[string]promptSize
}

type promptSize struct{ chars, tokens int }

func transcriptChars(messages []chatMessage) int {
	chars := 0
	for _, message := range messages {
		chars += utf8.RuneCountInString(message.Role) + utf8.RuneCountInString(message.Content)
	}
	return chars
}

func (e *promptEstimator) estimate(pid string, messages []chatMessage) int {
	chars := transcriptChars(messages)
	e.mu.Lock()
	previous, ok := e.last[pid]
	e.mu.Unlock()
	if ok && pid != "" && previous.tokens > 0 && chars >= previous.chars {
		return previous.tokens + charsToTokens(chars-previous.chars)
	}
	return charsToTokens(chars)
}

// observe keeps what the server reported for pid's call over messages.
func (e *promptEstimator) observe(pid string, messages []chatMessage, promptTokens int) {
	if pid == "" || promptTokens <= 0 {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.last == nil {
		e.last = map[string]promptSize{}
	}
	e.last[pid] = promptSize{chars: transcriptChars(messages), tokens: promptTokens}
}

func charsToTokens(chars int) int {
	return int(math.Ceil(float64(chars) / runtime.EstimatedCharsPerToken))
}

// replyCap is the max_tokens a call sends: configured (0: none), capped at
// info.MaxTokens (the cap from before the reply cap), then at what is left
// of info.TokensLeft once this step's metered calls and the estimated
// prompt are taken out. A call with nothing left for a reply is refused
// with runtime.ErrNoReplyTokens.
func (e *promptEstimator) replyCap(info runtime.CallInfo, messages []chatMessage, configured int) (int, error) {
	limit := configured
	if info.MaxTokens > 0 && (limit == 0 || limit > info.MaxTokens) {
		limit = info.MaxTokens
	}
	if info.TokensLeft <= 0 {
		return limit, nil
	}
	left := info.TokensLeft
	if info.Meter != nil {
		left -= info.Meter.Total()
	}
	prompt := e.estimate(info.PID, messages)
	room := left - prompt
	if room <= 0 {
		return 0, fmt.Errorf("%w: the prompt is about %d tokens and %d are left", runtime.ErrNoReplyTokens, prompt, left)
	}
	if limit == 0 || limit > room {
		limit = room
	}
	return limit, nil
}
