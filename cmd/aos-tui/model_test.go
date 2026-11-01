package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"aos/internal/agentflow"
	"aos/internal/inference"
	"aos/internal/runlog"
	"aos/internal/runtime"
)

// fakeModel is a chat-completions server that answers every call with one
// message and remembers what it was sent.
type fakeModel struct {
	server *httptest.Server
	mu     sync.Mutex
	bodies []string
	auth   []string
}

func newFakeModel(t *testing.T, reply string) *fakeModel {
	t.Helper()
	model := &fakeModel{}
	content, _ := json.Marshal(map[string]any{"action": map[string]string{"type": "message", "message": reply}})
	model.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		model.mu.Lock()
		model.bodies = append(model.bodies, string(body))
		model.auth = append(model.auth, r.Header.Get("Authorization"))
		model.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"role": "assistant", "content": string(content)}}},
			"usage":   map[string]int{"prompt_tokens": 100, "completion_tokens": 10},
		})
	}))
	t.Cleanup(model.server.Close)
	return model
}

func writeModels(t *testing.T, profiles ...inference.Profile) string {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"profiles": profiles})
	path := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// The environment's backend is the first profile and the current one, and
// replaces a file profile of its name. Keys are read from the variables the
// profiles name, those variables are then dropped from the environment, and
// a profile whose variable was not set cannot be switched to.
func TestModelsComeFromTheEnvironmentAndTheFile(t *testing.T) {
	t.Setenv("TEST_KEY_REMOTE", "secret-remote")
	t.Setenv("AOS_LLM_API_KEY", "secret-env")
	path := writeModels(t,
		inference.Profile{Name: "strata-phg", BaseURL: "http://elsewhere:1", Model: "stale"},
		inference.Profile{Name: "remote", BaseURL: "http://remote.local:8080/v1", Model: "m", APIKeyEnv: "TEST_KEY_REMOTE", ContextWindow: 131072},
		inference.Profile{Name: "nokey", BaseURL: "http://nokey.local:8080", Model: "m", APIKeyEnv: "TEST_KEY_UNSET"},
	)
	env := inference.BackendConfig{Kind: inference.BackendOpenAICompatible, BaseURL: "http://phg-gpu.local:8080", Model: "phg", APIKey: "secret-env", Timeout: time.Hour}
	models, err := loadModels("strata-phg", env, path)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, profile := range models.profiles {
		names = append(names, profile.Name)
	}
	if strings.Join(names, ",") != "strata-phg,remote,nokey" || models.current != "strata-phg" || models.profiles[0].Model != "phg" {
		t.Fatalf("profiles %v, current %s, first %+v", names, models.current, models.profiles[0])
	}
	if hosts := strings.Join(models.modelHosts(), ","); hosts != "phg-gpu.local,remote.local,nokey.local" {
		t.Fatalf("web_fetch should refuse every profile's host: %s", hosts)
	}

	scrubKeys(models.keyVariables())
	if os.Getenv("TEST_KEY_REMOTE") != "" || os.Getenv("AOS_LLM_API_KEY") != "" {
		t.Fatal("key variables should be dropped once read")
	}
	remote, _ := models.find("remote")
	planned, err := models.planSwitch(remote, runtime.Session{})
	if err != nil || planned.config.APIKey != "secret-remote" || planned.config.BaseURL != "http://remote.local:8080" {
		t.Fatalf("switch to remote = %+v, %v", planned.config.BaseURL, err)
	}
	if byNumber, ok := models.find("3"); !ok || byNumber.Name != "nokey" {
		t.Fatalf("find 3 = %+v", byNumber)
	}
	nokey, _ := models.find("nokey")
	if _, err := models.planSwitch(nokey, runtime.Session{}); err == nil || !strings.Contains(err.Error(), "TEST_KEY_UNSET") {
		t.Fatalf("a profile without its key should be refused, naming the variable: %v", err)
	}

	data := switchedData("strata-phg", planned)
	for _, value := range data {
		if strings.Contains(value, "secret") {
			t.Fatalf("the event holds a key: %v", data)
		}
	}
	want := map[string]string{"from": "strata-phg", "to": "remote", "base_url": "http://remote.local:8080", "model": "m", "api_key_env": "TEST_KEY_REMOTE",
		"context_window": "131072", "compact_at_chars": "314571", "compact_to_chars": "157285", "backend": "vllm"}
	for name, value := range want {
		if data[name] != value {
			t.Errorf("event %s = %q, want %q", name, data[name], value)
		}
	}
}

// Compaction follows the new model's window. History over the new trigger
// is accepted with a notice (the next turn compacts it, after a handoff);
// history over the whole new window is refused.
func TestSwitchFollowsTheNewWindow(t *testing.T) {
	small := inference.Profile{Name: "small", BaseURL: "http://small:1", Model: "m", ContextWindow: 65536}
	models, _ := loadModels("env", inference.BackendConfig{BaseURL: "http://big:1", Model: "m", ContextWindow: 262144}, writeModels(t, small))
	history := func(chars int) runtime.Session {
		return runtime.Session{Messages: []runtime.Message{{Role: "user", Content: strings.Repeat("x", chars)}}}
	}
	planned, err := models.planSwitch(small, history(1000))
	if err != nil || planned.notice != "" || planned.compaction.MaxChars != 52428*3 || !planned.compaction.Handoff {
		t.Fatalf("a short history = %+v, %v", planned.compaction, err)
	}
	if planned, err = models.planSwitch(small, history(170_000)); err != nil || !strings.Contains(planned.notice, "compaction trigger") {
		t.Fatalf("history over the trigger = %q, %v", planned.notice, err)
	}
	if _, err = models.planSwitch(small, history(200_000)); err == nil || !strings.Contains(err.Error(), "65536-token window") {
		t.Fatalf("history over the window should be refused: %v", err)
	}
	unknown := inference.Profile{Name: "unknown", BaseURL: "http://u:1", Model: "m"}
	models, _ = loadModels("env", inference.BackendConfig{BaseURL: "http://big:1", Model: "m"}, writeModels(t, unknown))
	if planned, err := models.planSwitch(unknown, history(200_000)); err != nil || planned.compaction.MaxChars != 0 {
		t.Fatalf("an unknown window keeps the runtime's default compaction: %+v, %v", planned.compaction, err)
	}
}

// A switch between turns: the next call goes to the new server with the
// whole conversation and the new key, the ledger records which endpoint
// answered each call, the event records the switch without the key, and
// a resumed TUI finds the profile from the events.
func TestSwitchingModelsMidSession(t *testing.T) {
	first, second := newFakeModel(t, "answer from the first"), newFakeModel(t, "answer from the second")
	t.Setenv("TEST_KEY_SECOND", "secret-second")
	path := writeModels(t, inference.Profile{Name: "second", Backend: "openai-compatible", BaseURL: second.server.URL, Model: "model-2", APIKeyEnv: "TEST_KEY_SECOND", ContextWindow: 262144})
	models, err := loadModels("first", inference.BackendConfig{Kind: inference.BackendOpenAICompatible, BaseURL: first.server.URL, Model: "model-1", APIKey: "secret-first", Timeout: time.Minute}, path)
	if err != nil {
		t.Fatal(err)
	}
	scrubKeys(models.keyVariables())

	f := newRootFixture(t)
	stateDir := t.TempDir()
	callLog, _ := runlog.Open(runlog.Path(stateDir, runlog.CallsFile))
	newBackend := func(config inference.BackendConfig) *inference.VLLMBackend {
		backend := inference.NewBackendFromConfig(config, inference.BackendOptions{})
		backend.SetCallLog(callLog)
		return backend
	}
	current, _ := models.find("first")
	active, err := models.configure(current)
	if err != nil {
		t.Fatal(err)
	}
	completer := &swapCompleter{backend: newBackend(active.config)}
	spec, agent := loadInteractiveSpec("", "", "You are a test agent.", t.TempDir(), nil, nil, nil)
	r := f.runtime(agentflow.PromptBackend{Completer: completer, Workflow: spec, Agent: agent})
	process, _ := f.spawn()
	root := tuiRoot{process: process}

	if _, result, err := f.say(t, r, &root, "what is the first question?"); err != nil || !result.Yielded {
		t.Fatalf("turn 1: %+v %v", result.ExitStatus, err)
	}
	target, _ := models.find("second")
	planned, err := models.planSwitch(target, root.session)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.kernel.RecordEvent(root.process.PID, modelSwitchedEvent, switchedData(models.current, planned)); err != nil {
		t.Fatal(err)
	}
	completer.set(newBackend(planned.config))
	r.Compaction = planned.compaction
	if _, result, err := f.say(t, r, &root, "and the second?"); err != nil || !result.Yielded {
		t.Fatalf("turn 2: %+v %v", result.ExitStatus, err)
	}

	if len(first.bodies) != 1 || len(second.bodies) != 1 {
		t.Fatalf("calls: first %d, second %d; want one each", len(first.bodies), len(second.bodies))
	}
	for _, want := range []string{"what is the first question?", "answer from the first", "and the second?", `"model":"model-2"`} {
		if !strings.Contains(second.bodies[0], want) {
			t.Fatalf("the second model should get the whole conversation; missing %q", want)
		}
	}
	if first.auth[0] != "Bearer secret-first" || second.auth[0] != "Bearer secret-second" {
		t.Fatalf("each model gets its own key: %q, %q", first.auth[0], second.auth[0])
	}
	if last := root.session.Messages[len(root.session.Messages)-1]; last.Content != "answer from the second" {
		t.Fatalf("the reply = %+v", last)
	}

	records, _ := runlog.Read[inference.CallRecord](callLog.Path())
	var calls []inference.CallRecord
	for _, record := range records {
		if record.Finished() {
			calls = append(calls, record)
		}
	}
	if len(calls) != 2 || calls[0].Backend != first.server.URL || calls[0].Model != "model-1" || calls[1].Backend != second.server.URL || calls[1].Model != "model-2" {
		t.Fatalf("the ledger should say which model answered each call: %+v", calls)
	}
	events, _ := f.kernel.Events()
	if lastSwitched(events) != "second" {
		t.Fatalf("a resumed TUI should find profile second, got %q", lastSwitched(events))
	}
	ledger, _ := os.ReadFile(callLog.Path())
	rawEvents, _ := json.Marshal(events)
	for _, record := range [][]byte{ledger, rawEvents} {
		if strings.Contains(string(record), "secret-") {
			t.Fatal("a key reached the ledger or the event log")
		}
	}
}
