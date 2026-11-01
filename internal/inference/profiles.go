package inference

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Profile is a named model endpoint with the driver settings that go with
// it. It names where its API key comes from (APIKeyEnv, an environment
// variable), never the key itself.
type Profile struct {
	Name           string          `json:"name"`
	Description    string          `json:"description,omitempty"`
	Backend        BackendKind     `json:"backend,omitempty"`
	BaseURL        string          `json:"base_url"`
	Model          string          `json:"model"`
	APIKeyEnv      string          `json:"api_key_env,omitempty"`
	ContextWindow  int             `json:"context_window,omitempty"`
	CompactRatio   float64         `json:"compact_ratio,omitempty"`
	TimeoutSeconds int             `json:"timeout_seconds,omitempty"`
	Sampling       ProfileSampling `json:"sampling,omitzero"`
}

// ProfileSampling is a profile's sampling and reasoning settings; zero
// fields are left to the server.
type ProfileSampling struct {
	Temperature    float64 `json:"temperature,omitempty"`
	TopP           float64 `json:"top_p,omitempty"`
	MaxTokens      int     `json:"max_tokens,omitempty"`
	ThinkingBudget int     `json:"thinking_budget,omitempty"`
}

// ProfilesFileEnv names the profiles file when no flag does.
const ProfilesFileEnv = "AOS_MODELS_FILE"

// DefaultProfilesFile is where profiles live when neither a flag nor
// AOS_MODELS_FILE says: models.json in the AOS config directory
// ($XDG_CONFIG_HOME/aos, else ~/.config/aos).
func DefaultProfilesFile() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "aos", "models.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "aos", "models.json")
}

// LoadProfiles reads a profiles file: {"profiles": [...]}. A missing file
// has no profiles. A profile must have a unique name, a base URL and a
// model, and may not carry a key.
func LoadProfiles(path string) ([]Profile, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var keyCheck struct {
		Profiles []map[string]json.RawMessage `json:"profiles"`
	}
	if err := json.Unmarshal(raw, &keyCheck); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for i, fields := range keyCheck.Profiles {
		if _, ok := fields["api_key"]; ok {
			return nil, fmt.Errorf("%s: profile %d holds an api_key; name its environment variable in api_key_env instead", path, i+1)
		}
	}
	var file struct {
		Profiles []Profile `json:"profiles"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&file); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	seen := map[string]bool{}
	for i, profile := range file.Profiles {
		switch {
		case strings.TrimSpace(profile.Name) == "" || strings.ContainsAny(profile.Name, " \t"):
			return nil, fmt.Errorf("%s: profile %d needs a name without spaces", path, i+1)
		case seen[profile.Name]:
			return nil, fmt.Errorf("%s: profile %q is defined twice", path, profile.Name)
		case profile.BaseURL == "" || profile.Model == "":
			return nil, fmt.Errorf("%s: profile %q needs base_url and model", path, profile.Name)
		case profile.CompactRatio < 0 || profile.CompactRatio > 1:
			return nil, fmt.Errorf("%s: profile %q: compact_ratio must be within 0 and 1", path, profile.Name)
		}
		seen[profile.Name] = true
		file.Profiles[i].Backend = normalizeBackendKind(string(profile.Backend))
		file.Profiles[i].BaseURL = normalizeBaseURL(profile.BaseURL)
	}
	return file.Profiles, nil
}

// EnvProfile describes the backend that the AOS_LLM_* environment selects,
// so it can be listed and switched back to like any other profile.
func EnvProfile(name string, config BackendConfig) Profile {
	return Profile{
		Name:           name,
		Description:    "from the AOS_LLM_* environment",
		Backend:        config.Kind,
		BaseURL:        config.BaseURL,
		Model:          config.Model,
		APIKeyEnv:      "AOS_LLM_API_KEY",
		ContextWindow:  config.ContextWindow,
		CompactRatio:   config.CompactRatio,
		TimeoutSeconds: int(config.Timeout / time.Second),
		Sampling: ProfileSampling{
			Temperature:    config.Sampling.Temperature,
			TopP:           config.Sampling.TopP,
			MaxTokens:      config.Sampling.MaxTokens,
			ThinkingBudget: config.Sampling.ThinkingBudget,
		},
	}
}

// Config is the backend configuration for the profile, with apiKey (read
// from APIKeyEnv by the caller) and defaultTimeout where the profile sets
// none.
func (p Profile) Config(apiKey string, defaultTimeout time.Duration) BackendConfig {
	timeout := defaultTimeout
	if p.TimeoutSeconds > 0 {
		timeout = time.Duration(p.TimeoutSeconds) * time.Second
	}
	ratio := p.CompactRatio
	if ratio == 0 {
		ratio = defaultCompactRatio
	}
	return BackendConfig{
		Kind:          p.Backend,
		BaseURL:       p.BaseURL,
		Model:         p.Model,
		APIKey:        apiKey,
		Timeout:       timeout,
		ContextWindow: p.ContextWindow,
		CompactRatio:  ratio,
		Sampling: Sampling{
			Temperature:    p.Sampling.Temperature,
			TopP:           p.Sampling.TopP,
			MaxTokens:      p.Sampling.MaxTokens,
			ThinkingBudget: p.Sampling.ThinkingBudget,
		},
	}
}
