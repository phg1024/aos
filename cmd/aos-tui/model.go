package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"aos/internal/inference"
	"aos/internal/kernel"
	"aos/internal/runtime"
	"aos/internal/tools"
	"aos/internal/tui/render"
)

// modelSwitchedEvent is the kernel event a /model switch records, on the
// root process. The latest one in the state directory is the session's
// model, which a resumed TUI goes back to.
const modelSwitchedEvent = "model.switched"

// defaultModelTimeout is the model call timeout where a profile sets none,
// as for the environment's backend.
const defaultModelTimeout = 180 * time.Second

// swapCompleter is the model client the prompt backends call. /model
// replaces the client inside it, so the root agent and every delegate
// built on it use the new model from their next call.
type swapCompleter struct {
	mu      sync.RWMutex
	backend inference.Backend
}

func (s *swapCompleter) current() inference.Backend {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.backend
}

func (s *swapCompleter) set(backend inference.Backend) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.backend = backend
}

func (s *swapCompleter) CompleteJSON(ctx context.Context, systemPrompt, userPrompt string, target any) error {
	return s.current().CompleteJSON(ctx, systemPrompt, userPrompt, target)
}

func (s *swapCompleter) CompleteText(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	return s.current().CompleteText(ctx, systemPrompt, userPrompt)
}

func (s *swapCompleter) CompleteMessages(ctx context.Context, messages []runtime.Message) (string, error) {
	return s.current().CompleteMessages(ctx, messages)
}

// modelSet is the profiles /model offers: the environment's backend first,
// then the profiles file's. Keys are read once, at start; see scrubKeys.
type modelSet struct {
	profiles []inference.Profile
	keys     map[string]profileKey
	current  string
}

type profileKey struct {
	value string
	// set is whether the profile's key variable was set; a profile that
	// names one it cannot read is not offered a switch.
	set bool
}

// loadModels builds the set from the environment's backend, named envName,
// and the profiles file at path. A file profile of the same name as the
// environment's is replaced by it: the environment is what was launched.
func loadModels(envName string, env inference.BackendConfig, path string) (modelSet, error) {
	fileProfiles, err := inference.LoadProfiles(path)
	if err != nil {
		return modelSet{}, err
	}
	set := modelSet{
		profiles: []inference.Profile{inference.EnvProfile(envName, env)},
		keys:     map[string]profileKey{envName: {value: env.APIKey, set: true}},
		current:  envName,
	}
	for _, profile := range fileProfiles {
		if profile.Name == envName {
			continue
		}
		set.profiles = append(set.profiles, profile)
		key := profileKey{set: profile.APIKeyEnv == ""}
		if profile.APIKeyEnv != "" {
			key.value, key.set = os.LookupEnv(profile.APIKeyEnv)
		}
		set.keys[profile.Name] = key
	}
	return set, nil
}

// keyVariables are the environment variables keys were read from.
func (m modelSet) keyVariables() []string {
	names := []string{"AOS_LLM_API_KEY", "AOS_VLLM_API_KEY"}
	for _, profile := range m.profiles {
		if profile.APIKeyEnv != "" {
			names = append(names, profile.APIKeyEnv)
		}
	}
	return names
}

// scrubKeys removes the key variables from this process's environment
// once they are read, so commands the agent runs do not inherit them.
func scrubKeys(names []string) {
	for _, name := range names {
		os.Unsetenv(name)
	}
}

// modelHosts are the hosts of every profile's endpoint, which web_fetch
// refuses unless allowlisted, whichever model is current.
func (m modelSet) modelHosts() []string {
	var hosts []string
	for _, profile := range m.profiles {
		if parsed, err := url.Parse(profile.BaseURL); err == nil && parsed.Hostname() != "" {
			hosts = appendUnique(hosts, parsed.Hostname())
		}
	}
	return hosts
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

// find is the profile named name, or numbered so in the list (from 1).
func (m modelSet) find(name string) (inference.Profile, bool) {
	for _, profile := range m.profiles {
		if profile.Name == name {
			return profile, true
		}
	}
	if n, err := strconv.Atoi(name); err == nil && n >= 1 && n <= len(m.profiles) {
		return m.profiles[n-1], true
	}
	return inference.Profile{}, false
}

// modelSwitch is a planned switch: the new backend's configuration and
// compaction policy, and what the user should know about it.
type modelSwitch struct {
	profile    inference.Profile
	config     inference.BackendConfig
	compaction runtime.CompactionPolicy
	notice     string
}

// configure works out the backend and compaction profile would get.
// Compaction follows the profile's window.
func (m modelSet) configure(profile inference.Profile) (modelSwitch, error) {
	key := m.keys[profile.Name]
	if !key.set {
		return modelSwitch{}, fmt.Errorf("profile %s reads its key from %s, which is not set; set it before starting the TUI", profile.Name, profile.APIKeyEnv)
	}
	config := profile.Config(key.value, defaultModelTimeout)
	compaction := config.CompactionPolicy()
	if err := compaction.Validate(tools.MaxToolResultBytes); err != nil {
		return modelSwitch{}, fmt.Errorf("profile %s: %w", profile.Name, err)
	}
	return modelSwitch{profile: profile, config: config, compaction: compaction}, nil
}

// planSwitch also checks that the session's live history can move to
// profile. History over the new compaction trigger is compacted at the
// start of the next turn, after a handoff on the new model; history that
// would not fit the new window at all is refused, since even that handoff
// call could not be sent.
func (m modelSet) planSwitch(profile inference.Profile, session runtime.Session) (modelSwitch, error) {
	planned, err := m.configure(profile)
	if err != nil || planned.config.ContextWindow <= 0 {
		return planned, err
	}
	window := planned.config.ContextWindow
	whole := runtime.CompactionPolicy{MaxChars: window * runtime.EstimatedCharsPerToken}
	if whole.ShouldCompact(session.Messages) {
		return modelSwitch{}, fmt.Errorf("the live history is larger than %s's %d-token window, so not even the handoff before compaction would fit; stay on this model", profile.Name, window)
	}
	if planned.compaction.ShouldCompact(session.Messages) {
		planned.notice = fmt.Sprintf("the live history is over %s's compaction trigger: your next message first gets a handoff on %s, then the history is compacted", profile.Name, profile.Name)
	}
	return planned, nil
}

// switchedData is the model.switched event's data. It names the key's
// variable, never the key.
func switchedData(from string, planned modelSwitch) map[string]string {
	data := map[string]string{
		"from":     from,
		"to":       planned.profile.Name,
		"backend":  string(planned.config.Kind),
		"base_url": planned.config.BaseURL,
		"model":    planned.config.Model,
	}
	if planned.profile.APIKeyEnv != "" {
		data["api_key_env"] = planned.profile.APIKeyEnv
	}
	if planned.config.ContextWindow > 0 {
		data["context_window"] = strconv.Itoa(planned.config.ContextWindow)
		data["compact_at_chars"] = strconv.Itoa(planned.compaction.MaxChars)
		data["compact_to_chars"] = strconv.Itoa(planned.compaction.MaxChars / 2)
	}
	return data
}

// lastSwitched is the profile the latest model.switched event chose, or ""
// when the session never switched.
func lastSwitched(events []kernel.Event) string {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == modelSwitchedEvent {
			return events[i].Data["to"]
		}
	}
	return ""
}

// modelRow is the i-th profile as the list and the picker show it.
func modelRow(ui render.Palette, i int, profile inference.Profile, current string) string {
	mark := " "
	if profile.Name == current {
		mark = "*"
	}
	window := ""
	if profile.ContextWindow > 0 {
		window = fmt.Sprintf("%dk", profile.ContextWindow/1024)
	}
	return fmt.Sprintf("%s%2d  %-18s %-40s %6s  %s", mark, i+1, profile.Name, profile.Model, window, ui.Subtle(profile.BaseURL))
}

// modelList renders the profiles, numbered for /model N, where the picker
// is not available.
func modelList(ui render.Palette, m modelSet, path string) string {
	var builder strings.Builder
	builder.WriteString(ui.Header(" Models ") + ui.Subtle("  "+path) + "\n")
	for i, profile := range m.profiles {
		builder.WriteString("  " + modelRow(ui, i, profile, m.current) + "\n")
	}
	builder.WriteString(ui.Subtle("  * current · /model NAME or /model N switches"))
	return builder.String()
}
