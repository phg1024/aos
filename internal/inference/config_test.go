package inference_test

import (
	"testing"
	"time"

	"aos/internal/inference"
)

func TestLoadBackendConfigDefaultsToVLLM(t *testing.T) {
	t.Setenv("AOS_LLM_BACKEND", "")
	t.Setenv("AOS_LLM_BASE_URL", "")
	t.Setenv("AOS_LLM_MODEL", "")
	t.Setenv("AOS_LLM_API_KEY", "")
	t.Setenv("AOS_LLM_TIMEOUT_SECONDS", "")
	t.Setenv("AOS_VLLM_BASE_URL", "")
	t.Setenv("AOS_VLLM_MODEL", "")
	t.Setenv("AOS_VLLM_API_KEY", "")
	t.Setenv("AOS_VLLM_TIMEOUT_SECONDS", "")

	config := inference.LoadBackendConfig(45 * time.Second)
	if config.Kind != inference.BackendVLLM {
		t.Fatalf("unexpected kind %q", config.Kind)
	}
	if config.BaseURL != "http://lenovo-px.local:8001" {
		t.Fatalf("unexpected base URL %q", config.BaseURL)
	}
	if config.Model != "Qwen/Qwen3.5-27B" {
		t.Fatalf("unexpected model %q", config.Model)
	}
	if config.APIKey != "dummy" {
		t.Fatalf("unexpected api key %q", config.APIKey)
	}
	if config.Timeout != 45*time.Second {
		t.Fatalf("unexpected timeout %s", config.Timeout)
	}
}

func TestLoadBackendConfigSupportsOllamaOverrides(t *testing.T) {
	t.Setenv("AOS_LLM_BACKEND", "ollama")
	t.Setenv("AOS_LLM_BASE_URL", "http://lenovo-px.local:11434/v1")
	t.Setenv("AOS_LLM_MODEL", "gemma4:26b")
	t.Setenv("AOS_LLM_API_KEY", "")
	t.Setenv("AOS_LLM_TIMEOUT_SECONDS", "180")
	t.Setenv("AOS_VLLM_BASE_URL", "")
	t.Setenv("AOS_VLLM_MODEL", "")
	t.Setenv("AOS_VLLM_API_KEY", "")
	t.Setenv("AOS_VLLM_TIMEOUT_SECONDS", "")

	config := inference.LoadBackendConfig(30 * time.Second)
	if config.Kind != inference.BackendOllama {
		t.Fatalf("unexpected kind %q", config.Kind)
	}
	if config.BaseURL != "http://lenovo-px.local:11434" {
		t.Fatalf("unexpected base URL %q", config.BaseURL)
	}
	if config.Model != "gemma4:26b" {
		t.Fatalf("unexpected model %q", config.Model)
	}
	if config.APIKey != "" {
		t.Fatalf("unexpected api key %q", config.APIKey)
	}
	if config.Timeout != 180*time.Second {
		t.Fatalf("unexpected timeout %s", config.Timeout)
	}
}

func TestLoadBackendConfigFallsBackToLegacyVLLMEnv(t *testing.T) {
	t.Setenv("AOS_LLM_BACKEND", "")
	t.Setenv("AOS_LLM_BASE_URL", "")
	t.Setenv("AOS_LLM_MODEL", "")
	t.Setenv("AOS_LLM_API_KEY", "")
	t.Setenv("AOS_LLM_TIMEOUT_SECONDS", "")
	t.Setenv("AOS_VLLM_BASE_URL", "http://legacy.local:9000/v1")
	t.Setenv("AOS_VLLM_MODEL", "legacy-model")
	t.Setenv("AOS_VLLM_API_KEY", "legacy-key")
	t.Setenv("AOS_VLLM_TIMEOUT_SECONDS", "99")

	config := inference.LoadBackendConfig(10 * time.Second)
	if config.Kind != inference.BackendVLLM {
		t.Fatalf("unexpected kind %q", config.Kind)
	}
	if config.BaseURL != "http://legacy.local:9000" {
		t.Fatalf("unexpected base URL %q", config.BaseURL)
	}
	if config.Model != "legacy-model" {
		t.Fatalf("unexpected model %q", config.Model)
	}
	if config.APIKey != "legacy-key" {
		t.Fatalf("unexpected api key %q", config.APIKey)
	}
	if config.Timeout != 99*time.Second {
		t.Fatalf("unexpected timeout %s", config.Timeout)
	}
}

func TestLoadBackendConfigReadsContextWindow(t *testing.T) {
	t.Setenv("AOS_LLM_CONTEXT_WINDOW", "262144")
	if got := inference.LoadBackendConfig(time.Minute).ContextWindow; got != 262144 {
		t.Fatalf("expected context window 262144, got %d", got)
	}
	for _, value := range []string{"", "abc", "-5"} {
		t.Setenv("AOS_LLM_CONTEXT_WINDOW", value)
		if got := inference.LoadBackendConfig(time.Minute).ContextWindow; got != 0 {
			t.Fatalf("expected unknown window for %q, got %d", value, got)
		}
	}
}
