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
