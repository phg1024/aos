package agentflow

import (
	"testing"
	"time"

	"aos/internal/capability"
	"aos/internal/runtime"
)

// run.started records settings as they take effect, defaults filled in,
// says which were defaults, and reads back to the same setup.
func TestRunStartedRecordsEffectiveSettings(t *testing.T) {
	spec := WorkflowSpec{Agents: []AgentSpec{{ID: "solver"}, {ID: "writer", MaxSteps: 300}}}
	defaults := (&Engine{RunRecord: map[string]string{"model": "m", "retry_attempts": "99"}}).runStartedData(spec, "art-000001", "sum")
	want := map[string]string{
		"budget_tokens": "0", "budget_seconds": "0", "budget_strict_time": "false", "budget_time": "none",
		"compaction_source": "default", "compact_max_messages": "24", "compact_max_chars": "8000", "compact_keep": "12", "compact_target_chars": "4000", "compact_handoff": "false",
		"retry_source": "default", "retry_attempts": "12", "retry_base": "1s", "retry_max": "2m0s",
		"max_steps_solver": "24", "max_steps_writer": "300", "model": "m",
	}
	for name, value := range want {
		if defaults[name] != value {
			t.Errorf("defaults: %s = %q, want %q", name, defaults[name], value)
		}
	}

	configured := (&Engine{
		Budget:       capability.Budget{Seconds: 879, StrictTime: true, Tokens: 5000},
		Compaction:   runtime.ContextCompactionPolicy(262144, 0.8),
		BackendRetry: runtime.BackendRetryPolicy{Attempts: 3},
	}).runStartedData(spec, "art-000001", "sum")
	want = map[string]string{
		"budget_time": "strict", "budget_seconds": "879", "budget_tokens": "5000",
		"compaction_source": "configured", "compact_max_chars": "629145", "compact_target_chars": "314572", "compact_handoff": "true", "compact_max_messages": "0",
		"retry_source": "configured", "retry_attempts": "3", "retry_base": "1s", "retry_max": "2m0s",
	}
	for name, value := range want {
		if configured[name] != value {
			t.Errorf("configured: %s = %q, want %q", name, configured[name], value)
		}
	}
	if kind := timeBudgetKind(capability.Budget{Seconds: 60}); kind != "advisory" {
		t.Errorf("advisory budget = %q", kind)
	}

	// Defaults read back as defaults, configured settings as their effect,
	// and an engine set up from either records the same data again.
	for name, data := range map[string]map[string]string{"defaults": defaults, "configured": configured} {
		settings, err := ParseRunStarted(data)
		if err != nil {
			t.Fatal(err)
		}
		again := (&Engine{Budget: settings.Budget, Compaction: settings.Compaction, BackendRetry: settings.BackendRetry}).runStartedData(spec, "art-000001", "sum")
		for key, value := range again {
			if data[key] != value {
				t.Errorf("%s: %s records %q again, was %q", name, key, value, data[key])
			}
		}
	}
	if settings, _ := ParseRunStarted(defaults); settings.Compaction != (runtime.CompactionPolicy{}) || settings.BackendRetry != (runtime.BackendRetryPolicy{}) {
		t.Errorf("defaults should read back as the zero settings: %+v", settings)
	}
	if settings, _ := ParseRunStarted(configured); settings.BackendRetry != (runtime.BackendRetryPolicy{Attempts: 3, Base: time.Second, Max: 2 * time.Minute}) {
		t.Errorf("configured retry = %+v", settings.BackendRetry)
	}
}
