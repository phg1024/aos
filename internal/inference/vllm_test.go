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
