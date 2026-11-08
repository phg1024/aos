package inference

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeProfiles(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadProfilesNormalizesAndChecks(t *testing.T) {
	path := writeProfiles(t, `{"profiles": [
		{"name": "remote", "backend": "openai", "base_url": "http://gpu.local:8080/v1/", "model": "m1", "api_key_env": "REMOTE_KEY",
		 "context_window": 131072, "compact_ratio": 0.7, "timeout_seconds": 90, "sampling": {"temperature": 0.6, "thinking_budget": 2048}},
		{"name": "local", "base_url": "http://127.0.0.1:8080", "model": "m2"}
	]}`)
	profiles, err := LoadProfiles(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 2 || profiles[0].BaseURL != "http://gpu.local:8080" || profiles[0].Backend != BackendOpenAICompatible || profiles[1].Backend != BackendVLLM {
		t.Fatalf("profiles = %+v", profiles)
	}
	config := profiles[0].Config("k", time.Minute)
	if config.APIKey != "k" || config.Timeout != 90*time.Second || config.ContextWindow != 131072 || config.CompactRatio != 0.7 || config.Sampling.ThinkingBudget != 2048 {
		t.Fatalf("config = %+v", config)
	}
	if policy := config.CompactionPolicy(); policy.MaxChars != 91750*3 {
		t.Fatalf("compaction should follow the profile's window: %+v", policy)
	}
	if local := profiles[1].Config("", time.Minute); local.Timeout != time.Minute || local.CompactRatio != defaultCompactRatio || local.CompactionPolicy().MaxChars != 0 {
		t.Fatalf("defaults = %+v", local)
	}

	if profiles, err := LoadProfiles(filepath.Join(t.TempDir(), "absent.json")); err != nil || profiles != nil {
		t.Fatalf("a missing file has no profiles: %v %v", profiles, err)
	}
	for name, body := range map[string]string{
		"a literal key":    `{"profiles": [{"name": "x", "base_url": "http://h", "model": "m", "api_key": "sk-secret"}]}`,
		"a repeated name":  `{"profiles": [{"name": "x", "base_url": "http://h", "model": "m"}, {"name": "x", "base_url": "http://h", "model": "m"}]}`,
		"no model":         `{"profiles": [{"name": "x", "base_url": "http://h"}]}`,
		"no name":          `{"profiles": [{"base_url": "http://h", "model": "m"}]}`,
		"an unknown field": `{"profiles": [{"name": "x", "base_url": "http://h", "model": "m", "thinking": 1}]}`,
		"a bad ratio":      `{"profiles": [{"name": "x", "base_url": "http://h", "model": "m", "compact_ratio": 2}]}`,
	} {
		_, err := LoadProfiles(writeProfiles(t, body))
		if err == nil {
			t.Errorf("%s: accepted", name)
		} else if strings.Contains(err.Error(), "sk-secret") {
			t.Errorf("%s: the error repeats the key: %v", name, err)
		}
	}
}

// The environment's backend, as a profile, configures the same backend.
func TestEnvProfileRoundTrips(t *testing.T) {
	env := BackendConfig{Kind: BackendOpenAICompatible, BaseURL: "http://phg-gpu.local:8080", Model: "m", APIKey: "k",
		Timeout: 3600 * time.Second, ContextWindow: 262144, CompactRatio: 0.8,
		Sampling: Sampling{Temperature: 0.6, TopP: 0.95, MaxTokens: 32768, ThinkingBudget: 16384}}
	profile := EnvProfile("strata-phg", env)
	if got := profile.Config("k", time.Minute); got != env {
		t.Fatalf("config %+v, want %+v", got, env)
	}
	if profile.APIKeyEnv != "AOS_LLM_API_KEY" {
		t.Fatalf("key variable %q", profile.APIKeyEnv)
	}
}

// The checked-in presets load, include both Stratas, and carry no key.
func TestCheckedInProfiles(t *testing.T) {
	path := filepath.Join("..", "..", "scripts", "models.json")
	profiles, err := LoadProfiles(path)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]Profile{}
	for _, profile := range profiles {
		byName[profile.Name] = profile
	}
	if byName["strata-phg"].BaseURL != "http://phg-gpu.local:8080" || byName["strata-phg"].APIKeyEnv == "" {
		t.Fatalf("strata-phg = %+v", byName["strata-phg"])
	}
	if byName["strata"].BaseURL != "http://127.0.0.1:8080" || byName["strata"].APIKeyEnv != "" || byName["strata"].ContextWindow != 262144 {
		t.Fatalf("strata = %+v", byName["strata"])
	}
	if claude := byName["claude"]; claude.Backend != BackendAnthropic || claude.BaseURL != defaultAnthropicBaseURL || claude.Model != "claude-sonnet-5-5" || claude.APIKeyEnv != AnthropicKeyEnv {
		t.Fatalf("claude = %+v", claude)
	}
}
