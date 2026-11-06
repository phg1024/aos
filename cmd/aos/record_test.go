package main

import (
	"testing"
	"time"

	"aos/internal/inference"
)

// Sampling left at zero is not sent, so the record says the server's
// default applies rather than showing a zero that was never used.
func TestBackendRecordShowsEffectiveSampling(t *testing.T) {
	record := backendRecord(inference.BackendConfig{Kind: inference.BackendOpenAICompatible, BaseURL: "http://phg-gpu.local:8080", Model: "m", APIKey: "secret", Timeout: 2 * time.Minute,
		Sampling: inference.Sampling{Temperature: 0.6, MaxTokens: 8192}})
	want := map[string]string{"temperature": "0.6", "top_p": "server default", "max_tokens": "8192", "thinking_budget": "server default", "context_window": "unknown", "timeout": "2m0s"}
	for name, value := range want {
		if record[name] != value {
			t.Errorf("%s = %q, want %q", name, record[name], value)
		}
	}
	for name, value := range record {
		if value == "secret" {
			t.Fatalf("the key is recorded as %s", name)
		}
	}
	if set := backendRecord(inference.BackendConfig{ContextWindow: 262144, Sampling: inference.Sampling{TopP: 0.95, ThinkingBudget: 2048}}); set["top_p"] != "0.95" || set["thinking_budget"] != "2048" || set["context_window"] != "262144" {
		t.Errorf("set values = %v", set)
	}
}

// The run shows the client timeout it used, whatever became of the value. The value is seconds
// with an "s" added, so 2m is two milliseconds, and what cannot be parsed is the default.
func TestBackendRecordShowsTheTimeoutFromTheEnvironment(t *testing.T) {
	t.Setenv("AOS_VLLM_TIMEOUT_SECONDS", "")
	for value, want := range map[string]string{"1200": "20m0s", "": "2m0s", "2m": "2ms", "0": "2m0s", "soon": "2m0s"} {
		t.Setenv("AOS_LLM_TIMEOUT_SECONDS", value)
		if got := backendRecord(inference.LoadBackendConfig(120 * time.Second))["timeout"]; got != want {
			t.Errorf("AOS_LLM_TIMEOUT_SECONDS=%q recorded %s, want %s", value, got, want)
		}
	}
	t.Setenv("AOS_LLM_TIMEOUT_SECONDS", "1200")
	t.Setenv("AOS_VLLM_TIMEOUT_SECONDS", "30")
	if got := backendRecord(inference.LoadBackendConfig(120 * time.Second))["timeout"]; got != "20m0s" {
		t.Errorf("the older variable won: %s", got)
	}
}
