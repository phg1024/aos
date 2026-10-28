package inference_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aos/internal/inference"
	"aos/internal/kernel"
	"aos/internal/runtime"
)

func TestVLLMBackendNormalizesOpenAIStyleResponse(t *testing.T) {
	backend := inference.NewVLLMBackend(inference.VLLMConfig{
		BaseURL: "http://vllm.local",
		Model:   "local-model",
		APIKey:  "dummy",
		Timeout: 2 * time.Second,
		HTTPClient: &http.Client{
			Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path != "/v1/chat/completions" {
					t.Fatalf("unexpected path %s", r.URL.Path)
				}
				if got := r.Header.Get("Authorization"); got != "Bearer dummy" {
					t.Fatalf("unexpected auth header %q", got)
				}
				var payload map[string]any
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Fatalf("decode request: %v", err)
				}
				return jsonResponse(http.StatusOK, map[string]any{
					"choices": []map[string]any{
						{
							"message": map[string]any{
								"content": `{"action":{"type":"exit","exit_status":{"code":0,"reason":"done","retryable":false}}}`,
							},
						},
					},
				}), nil
			}),
		},
	})

	result, err := backend.Generate(context.Background(), runtime.GenerateRequest{
		Process: kernel.Process{PID: "pid-1", Role: "worker"},
		History: []runtime.Message{{Role: "user", Content: "say done"}},
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if result.Action.Type != "exit" || result.Action.ExitStatus == nil || result.Action.ExitStatus.Code != 0 {
		t.Fatalf("unexpected result %#v", result)
	}
}

func TestVLLMBackendRejectsMalformedJSONContent(t *testing.T) {
	backend := inference.NewVLLMBackend(inference.VLLMConfig{
		BaseURL: "http://vllm.local",
		Model:   "local-model",
		HTTPClient: &http.Client{
			Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				return jsonResponse(http.StatusOK, map[string]any{
					"choices": []map[string]any{{"message": map[string]any{"content": `not-json`}}},
				}), nil
			}),
		},
	})
	_, err := backend.Generate(context.Background(), runtime.GenerateRequest{
		Process: kernel.Process{PID: "pid-1", Role: "worker"},
	})
	if err != inference.ErrMalformedModelResponse {
		t.Fatalf("expected malformed response error, got %v", err)
	}
}

func TestVLLMBackendTimeout(t *testing.T) {
	backend := inference.NewVLLMBackend(inference.VLLMConfig{
		BaseURL: "http://vllm.local",
		Model:   "local-model",
		Timeout: 10 * time.Millisecond,
		HTTPClient: &http.Client{
			Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				return nil, context.DeadlineExceeded
			}),
		},
	})
	_, err := backend.Generate(context.Background(), runtime.GenerateRequest{
		Process: kernel.Process{PID: "pid-1", Role: "worker"},
	})
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestVLLMBackendRetriesTimeoutOnce(t *testing.T) {
	attempts := 0
	backend := inference.NewVLLMBackend(inference.VLLMConfig{
		BaseURL:    "http://vllm.local",
		Model:      "local-model",
		Timeout:    10 * time.Millisecond,
		MaxRetries: 2,
		HTTPClient: &http.Client{
			Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				attempts++
				if attempts == 1 {
					return nil, context.DeadlineExceeded
				}
				return jsonResponse(http.StatusOK, map[string]any{
					"choices": []map[string]any{
						{"message": map[string]any{
							"content": `{"action":{"type":"exit","exit_status":{"code":0,"reason":"retried","retryable":false}}}`,
						}},
					},
				}), nil
			}),
		},
	})
	result, err := backend.Generate(context.Background(), runtime.GenerateRequest{
		Process: kernel.Process{PID: "pid-1", Role: "worker"},
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("expected 2 attempts, got %d", attempts)
	}
	if result.Action.ExitStatus == nil || result.Action.ExitStatus.Reason != "retried" {
		t.Fatalf("unexpected result %#v", result)
	}
}

func TestVLLMBackendSupportsEndToEndSingleAgentFlowAgainstMockServer(t *testing.T) {
	root := t.TempDir()
	k, err := kernel.New(filepath.Join(root, "kernel"))
	if err != nil {
		t.Fatalf("new kernel: %v", err)
	}
	process, err := k.Spawn(kernel.ProcessSpec{Role: "worker"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	backend := inference.NewVLLMBackend(inference.VLLMConfig{
		BaseURL: "http://vllm.local",
		Model:   "local-model",
		Timeout: time.Second,
		HTTPClient: &http.Client{
			Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				return jsonResponse(http.StatusOK, map[string]any{
					"choices": []map[string]any{
						{"message": map[string]any{
							"content": `{"action":{"type":"exit","exit_status":{"code":0,"reason":"mock-live","retryable":false}}}`,
						}},
					},
				}), nil
			}),
		},
	})

	result, err := backend.Generate(context.Background(), runtime.GenerateRequest{
		Process: process,
		History: []runtime.Message{{Role: "user", Content: "finish"}},
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if result.Action.ExitStatus == nil || result.Action.ExitStatus.Reason != "mock-live" {
		t.Fatalf("unexpected result %#v", result)
	}
}

func TestVLLMBackendExtractsTrailingJSONFromReasoningWrappedContent(t *testing.T) {
	backend := inference.NewVLLMBackend(inference.VLLMConfig{
		BaseURL: "http://vllm.local",
		Model:   "local-model",
		HTTPClient: &http.Client{
			Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				return jsonResponse(http.StatusOK, map[string]any{
					"choices": []map[string]any{
						{"message": map[string]any{
							"content": "thinking\n</think>\n{\"action\":{\"type\":\"exit\",\"exit_status\":{\"code\":0,\"reason\":\"wrapped\",\"retryable\":false}}}",
						}},
					},
				}), nil
			}),
		},
	})

	result, err := backend.Generate(context.Background(), runtime.GenerateRequest{
		Process: kernel.Process{PID: "pid-1", Role: "worker"},
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if result.Action.ExitStatus == nil || result.Action.ExitStatus.Reason != "wrapped" {
		t.Fatalf("unexpected result %#v", result)
	}
}

func TestVLLMBackendNormalizesFlatExitShape(t *testing.T) {
	backend := inference.NewVLLMBackend(inference.VLLMConfig{
		BaseURL: "http://vllm.local",
		Model:   "local-model",
		HTTPClient: &http.Client{
			Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				return jsonResponse(http.StatusOK, map[string]any{
					"choices": []map[string]any{
						{"message": map[string]any{
							"content": `{"action":"exit","reason":"flat-shape","retryable":false}`,
						}},
					},
				}), nil
			}),
		},
	})

	result, err := backend.Generate(context.Background(), runtime.GenerateRequest{
		Process: kernel.Process{PID: "pid-1", Role: "worker"},
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if result.Action.Type != "exit" || result.Action.ExitStatus == nil || result.Action.ExitStatus.Reason != "flat-shape" {
		t.Fatalf("unexpected result %#v", result)
	}
}

func TestVLLMBackendCompleteJSON(t *testing.T) {
	backend := inference.NewVLLMBackend(inference.VLLMConfig{
		BaseURL: "http://vllm.local",
		Model:   "local-model",
		HTTPClient: &http.Client{
			Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				return jsonResponse(http.StatusOK, map[string]any{
					"choices": []map[string]any{
						{"message": map[string]any{
							"content": "thinking\n</think>\n{\"approved\":true,\"reason\":\"ok\"}",
						}},
					},
				}), nil
			}),
		},
	})

	var out struct {
		Approved bool   `json:"approved"`
		Reason   string `json:"reason"`
	}
	if err := backend.CompleteJSON(context.Background(), "Return JSON", "verify", &out); err != nil {
		t.Fatalf("complete json: %v", err)
	}
	if !out.Approved || out.Reason != "ok" {
		t.Fatalf("unexpected json result %#v", out)
	}
}

func TestVLLMBackendStreamsReasoningAndContentEvents(t *testing.T) {
	var events []inference.StreamEvent
	backend := inference.NewVLLMBackend(inference.VLLMConfig{
		BaseURL: "http://vllm.local",
		Model:   "local-model",
		OnStreamEvent: func(event inference.StreamEvent) {
			events = append(events, event)
		},
		HTTPClient: &http.Client{
			Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				var payload map[string]any
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Fatalf("decode request: %v", err)
				}
				if got, _ := payload["stream"].(bool); !got {
					t.Fatalf("expected stream=true, got %#v", payload["stream"])
				}
				body := strings.Join([]string{
					`data: {"choices":[{"delta":{"reasoning_content":"thinking step"}}]}`,
					``,
					`data: {"choices":[{"delta":{"content":"{\"action\":{\"type\":\"exit\",\"exit_status\":{\"code\":0,\"reason\":\"streamed\",\"retryable\":false}}}"}}]}`,
					``,
					`data: [DONE]`,
					``,
				}, "\n")
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(body)),
				}, nil
			}),
		},
	})

	result, err := backend.Generate(context.Background(), runtime.GenerateRequest{
		Process: kernel.Process{PID: "pid-1", Role: "worker"},
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if result.Action.ExitStatus == nil || result.Action.ExitStatus.Reason != "streamed" {
		t.Fatalf("unexpected result %#v", result)
	}
	if len(events) != 3 {
		t.Fatalf("expected 3 stream events, got %d", len(events))
	}
	if events[0].Kind != inference.StreamThinking || events[0].Text != "thinking step" {
		t.Fatalf("unexpected first event %#v", events[0])
	}
	if events[1].Kind != inference.StreamContent {
		t.Fatalf("unexpected second event %#v", events[1])
	}
	if events[2].Kind != inference.StreamDone {
		t.Fatalf("unexpected final event %#v", events[2])
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (fn roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return fn(r)
}

func jsonResponse(status int, payload map[string]any) *http.Response {
	bytes, _ := json.Marshal(payload)
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(string(bytes))),
	}
}

func TestVLLMBackendSendsSamplingControls(t *testing.T) {
	capture := func(sampling inference.Sampling) map[string]any {
		var body map[string]any
		backend := inference.NewVLLMBackend(inference.VLLMConfig{
			BaseURL:  "http://vllm.local",
			Model:    "local-model",
			Sampling: sampling,
			HTTPClient: &http.Client{
				Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Fatalf("decode request: %v", err)
					}
					return jsonResponse(http.StatusOK, map[string]any{
						"choices": []map[string]any{{"message": map[string]any{"content": "ok"}}},
					}), nil
				}),
			},
		})
		if _, err := backend.CompleteText(context.Background(), "system", "user"); err != nil {
			t.Fatalf("complete text: %v", err)
		}
		return body
	}

	body := capture(inference.Sampling{})
	if body["temperature"] != 0.0 {
		t.Fatalf("expected default temperature 0, got %#v", body["temperature"])
	}
	for _, key := range []string{"top_p", "max_tokens", "reasoning_budget_tokens"} {
		if _, ok := body[key]; ok {
			t.Fatalf("expected %s to be omitted by default, got %#v", key, body)
		}
	}

	body = capture(inference.Sampling{Temperature: 0.6, TopP: 0.95, MaxTokens: 32768, ThinkingBudget: 16384})
	want := map[string]any{"temperature": 0.6, "top_p": 0.95, "max_tokens": 32768.0, "reasoning_budget_tokens": 16384.0}
	for key, value := range want {
		if body[key] != value {
			t.Fatalf("expected %s=%v, got %#v", key, value, body[key])
		}
	}
}

func TestVLLMBackendCompleteMessagesSendsTranscript(t *testing.T) {
	var body struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	backend := inference.NewVLLMBackend(inference.VLLMConfig{
		BaseURL: "http://vllm.local",
		Model:   "local-model",
		HTTPClient: &http.Client{
			Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatalf("decode request: %v", err)
				}
				return jsonResponse(http.StatusOK, map[string]any{
					"choices": []map[string]any{{"message": map[string]any{"content": "ok"}}},
				}), nil
			}),
		},
	})
	transcript := []runtime.Message{
		{Role: "system", Content: "rules"},
		{Role: "user", Content: "task"},
		{Role: "assistant", Content: "notes: next"},
		{Role: "user", Content: "read_file:map[path:/w/a]"},
	}
	out, err := backend.CompleteMessages(context.Background(), transcript)
	if err != nil || out != "ok" {
		t.Fatalf("complete messages: %q, %v", out, err)
	}
	if len(body.Messages) != len(transcript) {
		t.Fatalf("expected %d messages, got %#v", len(transcript), body.Messages)
	}
	for i, message := range transcript {
		if body.Messages[i].Role != message.Role || body.Messages[i].Content != message.Content {
			t.Fatalf("message %d changed: %#v", i, body.Messages[i])
		}
	}
}

func TestVLLMBackendAccumulatesTokenUsage(t *testing.T) {
	exit := `{"action":{"type":"exit","exit_status":{"code":0,"reason":"done"}}}`
	plain := inference.NewVLLMBackend(inference.VLLMConfig{
		BaseURL: "http://vllm.local",
		HTTPClient: &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, map[string]any{
				"choices": []map[string]any{{"message": map[string]any{"content": exit}}},
				"usage":   map[string]any{"prompt_tokens": 1200, "completion_tokens": 80, "prompt_tokens_details": map[string]any{"cached_tokens": 1000}},
			}), nil
		})},
	})
	for i := 0; i < 2; i++ {
		if _, err := plain.Generate(context.Background(), runtime.GenerateRequest{Process: kernel.Process{PID: "pid-1"}}); err != nil {
			t.Fatalf("generate: %v", err)
		}
	}
	if got, want := plain.Usage(), (inference.TokenUsage{Requests: 2, PromptTokens: 2400, CompletionTokens: 160, CachedTokens: 2000}); got != want {
		t.Fatalf("non-streamed usage = %+v, want %+v", got, want)
	}

	streamed := inference.NewVLLMBackend(inference.VLLMConfig{
		BaseURL:       "http://vllm.local",
		OnStreamEvent: func(inference.StreamEvent) {},
		HTTPClient: &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			var payload struct {
				StreamOptions map[string]bool `json:"stream_options"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode request: %v", err)
			}
			if !payload.StreamOptions["include_usage"] {
				t.Fatalf("streaming request should ask for usage, got %v", payload.StreamOptions)
			}
			content, _ := json.Marshal(exit)
			body := "data: {\"choices\":[{\"delta\":{\"content\":" + string(content) + "}}]}\n\n" +
				"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":300,\"completion_tokens\":20}}\n\n" +
				"data: [DONE]\n\n"
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
		})},
	})
	if _, err := streamed.Generate(context.Background(), runtime.GenerateRequest{Process: kernel.Process{PID: "pid-1"}}); err != nil {
		t.Fatalf("generate streamed: %v", err)
	}
	if got, want := streamed.Usage(), (inference.TokenUsage{Requests: 1, PromptTokens: 300, CompletionTokens: 20}); got != want {
		t.Fatalf("streamed usage = %+v, want %+v", got, want)
	}
}
