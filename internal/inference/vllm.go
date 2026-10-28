package inference

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"aos/internal/kernel"
	"aos/internal/runtime"
)

var ErrMalformedModelResponse = errors.New("malformed model response")

type VLLMConfig struct {
	BaseURL       string
	Model         string
	APIKey        string
	SystemPrompt  string
	Timeout       time.Duration
	MaxRetries    int
	Sampling      Sampling
	HTTPClient    *http.Client
	OnStreamEvent func(StreamEvent)
}

type VLLMBackend struct {
	baseURL       string
	model         string
	apiKey        string
	client        *http.Client
	system        string
	maxRetries    int
	onStreamEvent func(StreamEvent)
	sampling      Sampling

	usageMu sync.Mutex
	usage   TokenUsage
}

// TokenUsage totals what the server reported across completed requests.
type TokenUsage struct {
	Requests         int `json:"requests"`
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	CachedTokens     int `json:"cached_tokens"`
}

// Usage returns the token totals so far.
func (b *VLLMBackend) Usage() TokenUsage {
	b.usageMu.Lock()
	defer b.usageMu.Unlock()
	return b.usage
}

func (b *VLLMBackend) recordUsage(usage *responseUsage) {
	b.usageMu.Lock()
	defer b.usageMu.Unlock()
	b.usage.Requests++
	if usage == nil {
		return
	}
	b.usage.PromptTokens += usage.PromptTokens
	b.usage.CompletionTokens += usage.CompletionTokens
	if usage.PromptTokensDetails != nil {
		b.usage.CachedTokens += usage.PromptTokensDetails.CachedTokens
	}
}

type StreamKind string

const (
	StreamThinking StreamKind = "thinking"
	StreamContent  StreamKind = "content"
	StreamDone     StreamKind = "done"
)

type StreamEvent struct {
	Kind StreamKind `json:"kind"`
	Text string     `json:"text,omitempty"`
}

// Sampling holds optional generation controls. Zero values leave the
// server default in place, except Temperature, which is always sent.
type Sampling struct {
	Temperature float64
	TopP        float64
	MaxTokens   int
	// ThinkingBudget caps reasoning tokens for servers that accept
	// reasoning_budget_tokens (llama.cpp-style servers such as Strata).
	ThinkingBudget int
}

type chatCompletionRequest struct {
	Model                 string         `json:"model"`
	Messages              []chatMessage  `json:"messages"`
	Temperature           float64        `json:"temperature"`
	TopP                  float64        `json:"top_p,omitempty"`
	MaxTokens             int            `json:"max_tokens,omitempty"`
	ReasoningBudgetTokens int            `json:"reasoning_budget_tokens,omitempty"`
	Stream                bool           `json:"stream,omitempty"`
	StreamOptions         *streamOptions `json:"stream_options,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type responseUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details,omitempty"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatCompletionResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
	Usage *responseUsage `json:"usage,omitempty"`
}

func NewVLLMBackend(config VLLMConfig) *VLLMBackend {
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
	return &VLLMBackend{
		baseURL:       strings.TrimRight(config.BaseURL, "/"),
		model:         config.Model,
		apiKey:        config.APIKey,
		system:        system,
		client:        client,
		maxRetries:    max(1, config.MaxRetries),
		onStreamEvent: config.OnStreamEvent,
		sampling:      config.Sampling,
	}
}

func (b *VLLMBackend) Generate(ctx context.Context, request runtime.GenerateRequest) (runtime.GenerateResult, error) {
	content, err := b.complete(ctx, b.messages(request))
	if err != nil {
		return runtime.GenerateResult{}, err
	}
	content = extractTrailingJSONObject(content)
	result, err := normalizeGenerateResult(content)
	if err != nil {
		return runtime.GenerateResult{}, ErrMalformedModelResponse
	}
	return result, nil
}

func (b *VLLMBackend) CompleteJSON(ctx context.Context, systemPrompt, userPrompt string, target any) error {
	content, err := b.CompleteText(ctx, systemPrompt, userPrompt)
	if err != nil {
		return err
	}
	content = extractTrailingJSONObject(content)
	if err := json.Unmarshal([]byte(content), target); err != nil {
		return ErrMalformedModelResponse
	}
	return nil
}

// CompleteMessages sends a whole chat transcript unchanged.
func (b *VLLMBackend) CompleteMessages(ctx context.Context, messages []runtime.Message) (string, error) {
	chat := make([]chatMessage, 0, len(messages))
	for _, message := range messages {
		chat = append(chat, chatMessage{Role: message.Role, Content: message.Content})
	}
	return b.complete(ctx, chat)
}

func (b *VLLMBackend) CompleteText(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	messages := []chatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userPrompt},
	}
	return b.complete(ctx, messages)
}

type chatCompletionChunk struct {
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			Reasoning        string `json:"reasoning"`
			Thinking         string `json:"thinking"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *responseUsage `json:"usage,omitempty"`
}

func (b *VLLMBackend) complete(ctx context.Context, messages []chatMessage) (string, error) {
	payload := chatCompletionRequest{
		Model:                 b.model,
		Messages:              messages,
		Temperature:           b.sampling.Temperature,
		TopP:                  b.sampling.TopP,
		MaxTokens:             b.sampling.MaxTokens,
		ReasoningBudgetTokens: b.sampling.ThinkingBudget,
		Stream:                b.onStreamEvent != nil,
	}
	if payload.Stream {
		payload.StreamOptions = &streamOptions{IncludeUsage: true}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	var lastErr error
	for attempt := 0; attempt < b.maxRetries; attempt++ {
		httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, b.baseURL+"/v1/chat/completions", bytes.NewReader(body))
		if err != nil {
			return "", err
		}
		httpRequest.Header.Set("Content-Type", "application/json")
		if b.apiKey != "" {
			httpRequest.Header.Set("Authorization", "Bearer "+b.apiKey)
		}
		if b.onStreamEvent != nil {
			httpRequest.Header.Set("Accept", "text/event-stream")
		}

		response, err := b.client.Do(httpRequest)
		if err != nil {
			lastErr = err
			if isRetryableVLLMError(err) && attempt < b.maxRetries-1 {
				continue
			}
			return "", err
		}

		if response.StatusCode >= 500 {
			lastErr = fmt.Errorf("vllm request failed: %s", response.Status)
			response.Body.Close()
			if attempt < b.maxRetries-1 {
				continue
			}
			return "", lastErr
		}
		if response.StatusCode >= 400 {
			lastErr = fmt.Errorf("vllm request failed: %s", response.Status)
			response.Body.Close()
			return "", lastErr
		}

		if b.onStreamEvent != nil {
			var fullResponse strings.Builder
			scanner := bufio.NewScanner(response.Body)
			var streamUsage *responseUsage
			for scanner.Scan() {
				line := scanner.Text()
				if strings.HasPrefix(line, "data: ") {
					data := strings.TrimPrefix(line, "data: ")
					if data == "[DONE]" {
						break
					}
					var chunk chatCompletionChunk
					if err := json.Unmarshal([]byte(data), &chunk); err != nil {
						continue
					}
					if chunk.Usage != nil {
						streamUsage = chunk.Usage
					}
					if len(chunk.Choices) > 0 {
						delta := chunk.Choices[0].Delta
						for _, reasoning := range []string{delta.ReasoningContent, delta.Reasoning, delta.Thinking} {
							if reasoning != "" {
								b.onStreamEvent(StreamEvent{Kind: StreamThinking, Text: reasoning})
								fullResponse.WriteString(reasoning)
							}
						}
						if delta.Content != "" {
							b.onStreamEvent(StreamEvent{Kind: StreamContent, Text: delta.Content})
							fullResponse.WriteString(delta.Content)
						}
					}
				}
			}
			response.Body.Close()
			if err := scanner.Err(); err != nil {
				lastErr = err
				if isRetryableVLLMError(err) && attempt < b.maxRetries-1 {
					continue
				}
				return fullResponse.String(), err
			}
			b.recordUsage(streamUsage)
			b.onStreamEvent(StreamEvent{Kind: StreamDone})
			return fullResponse.String(), nil
		}

		var parsed chatCompletionResponse
		err = json.NewDecoder(response.Body).Decode(&parsed)
		response.Body.Close()
		if err != nil {
			lastErr = err
			if attempt < b.maxRetries-1 {
				continue
			}
			return "", err
		}
		if len(parsed.Choices) == 0 {
			lastErr = ErrMalformedModelResponse
			if attempt < b.maxRetries-1 {
				continue
			}
			return "", ErrMalformedModelResponse
		}
		b.recordUsage(parsed.Usage)
		return parsed.Choices[0].Message.Content, nil
	}
	if lastErr != nil {
		return "", lastErr
	}
	return "", ErrMalformedModelResponse
}

func isRetryableVLLMError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "timeout") || strings.Contains(lower, "deadline exceeded")
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func (b *VLLMBackend) messages(request runtime.GenerateRequest) []chatMessage {
	messages := []chatMessage{
		{Role: "system", Content: b.system},
		{Role: "user", Content: fmt.Sprintf("Process role=%s pid=%s", request.Process.Role, request.Process.PID)},
	}
	for _, message := range request.History {
		messages = append(messages, chatMessage{
			Role:    message.Role,
			Content: message.Content,
		})
	}
	return messages
}

func extractTrailingJSONObject(input string) string {
	start := strings.LastIndex(input, "{")
	if start == -1 {
		return input
	}
	for candidateStart := start; candidateStart >= 0; candidateStart = previousBrace(input, candidateStart) {
		if candidateStart < 0 {
			break
		}
		candidate := strings.TrimSpace(input[candidateStart:])
		if json.Valid([]byte(candidate)) {
			return candidate
		}
	}
	return strings.TrimSpace(input)
}

func previousBrace(input string, before int) int {
	for i := before - 1; i >= 0; i-- {
		if input[i] == '{' {
			if utf8.ValidString(input[i:]) {
				return i
			}
			return i
		}
	}
	return -1
}

func normalizeGenerateResult(raw string) (runtime.GenerateResult, error) {
	var nested runtime.GenerateResult
	if err := json.Unmarshal([]byte(raw), &nested); err == nil && nested.Action.Type != "" {
		return nested, nil
	}

	var flat struct {
		Action    string `json:"action"`
		Reason    string `json:"reason"`
		Retryable bool   `json:"retryable"`
		Code      int    `json:"code"`
	}
	if err := json.Unmarshal([]byte(raw), &flat); err == nil && flat.Action != "" {
		code := flat.Code
		if code == 0 && flat.Action != "exit" {
			code = 0
		}
		status := runtimeExitStatus(flat.Action, code, flat.Reason, flat.Retryable)
		return runtime.GenerateResult{
			Action: runtime.GenerateAction{
				Type:       flat.Action,
				ExitStatus: &status,
			},
		}, nil
	}

	return runtime.GenerateResult{}, ErrMalformedModelResponse
}

func runtimeExitStatus(action string, code int, reason string, retryable bool) kernel.ExitStatus {
	if action != "exit" {
		return kernel.ExitStatus{
			Code:      code,
			Reason:    reason,
			Retryable: retryable,
		}
	}
	return kernel.ExitStatus{
		Code:      code,
		Reason:    reason,
		Retryable: retryable,
	}
}
