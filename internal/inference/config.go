package inference

import (
	"os"
	"strings"
	"time"
)

type BackendKind string

const (
	BackendVLLM             BackendKind = "vllm"
	BackendOllama           BackendKind = "ollama"
	BackendOpenAICompatible BackendKind = "openai-compatible"
)

type BackendConfig struct {
	Kind    BackendKind
	BaseURL string
	Model   string
	APIKey  string
	Timeout time.Duration
}

type BackendOptions struct {
	SystemPrompt  string
	OnStreamEvent func(StreamEvent)
}

func LoadBackendConfig(defaultTimeout time.Duration) BackendConfig {
	kind := normalizeBackendKind(firstNonEmpty(
		os.Getenv("AOS_LLM_BACKEND"),
	))
	if kind == "" {
		kind = BackendVLLM
	}

	baseURL := normalizeBaseURL(firstNonEmpty(
		os.Getenv("AOS_LLM_BASE_URL"),
		os.Getenv("AOS_VLLM_BASE_URL"),
		defaultBaseURL(kind),
	))
	model := firstNonEmpty(
		os.Getenv("AOS_LLM_MODEL"),
		os.Getenv("AOS_VLLM_MODEL"),
		defaultModel(kind),
	)
	apiKey := firstNonEmpty(
		os.Getenv("AOS_LLM_API_KEY"),
		os.Getenv("AOS_VLLM_API_KEY"),
		defaultAPIKey(kind),
	)

	return BackendConfig{
		Kind:    kind,
		BaseURL: baseURL,
		Model:   model,
		APIKey:  apiKey,
		Timeout: parseTimeoutEnv(defaultTimeout),
	}
}

func NewBackendFromConfig(config BackendConfig, options BackendOptions) *VLLMBackend {
	return NewVLLMBackend(VLLMConfig{
		BaseURL:       config.BaseURL,
		Model:         config.Model,
		APIKey:        config.APIKey,
		SystemPrompt:  options.SystemPrompt,
		Timeout:       config.Timeout,
		OnStreamEvent: options.OnStreamEvent,
	})
}

func (c BackendConfig) Label() string {
	if c.Model == "" {
		return string(c.Kind)
	}
	return string(c.Kind) + " / " + c.Model
}

func normalizeBackendKind(value string) BackendKind {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "vllm":
		return BackendVLLM
	case "ollama":
		return BackendOllama
	case "openai", "openai-compatible", "openai_compatible", "compatible":
		return BackendOpenAICompatible
	default:
		return BackendKind(strings.ToLower(strings.TrimSpace(value)))
	}
}

func defaultBaseURL(kind BackendKind) string {
	switch kind {
	case BackendOllama:
		return "http://localhost:11434"
	default:
		return "http://lenovo-px.local:8001"
	}
}

func defaultModel(kind BackendKind) string {
	switch kind {
	case BackendOllama:
		return "qwen3.6:35b"
	default:
		return "Qwen/Qwen3.5-27B"
	}
}

func defaultAPIKey(kind BackendKind) string {
	switch kind {
	case BackendOllama:
		return ""
	default:
		return "dummy"
	}
}

func normalizeBaseURL(value string) string {
	value = strings.TrimSpace(strings.TrimRight(value, "/"))
	if strings.HasSuffix(value, "/v1") {
		return strings.TrimSuffix(value, "/v1")
	}
	return value
}

func parseTimeoutEnv(fallback time.Duration) time.Duration {
	if seconds := firstNonEmpty(os.Getenv("AOS_LLM_TIMEOUT_SECONDS"), os.Getenv("AOS_VLLM_TIMEOUT_SECONDS")); seconds != "" {
		if parsed, err := time.ParseDuration(strings.TrimSpace(seconds) + "s"); err == nil && parsed > 0 {
			return parsed
		}
	}
	return fallback
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}
