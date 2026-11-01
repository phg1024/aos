package agentflow

import (
	"fmt"
	"maps"
	"strconv"
	"time"

	"aos/internal/capability"
	"aos/internal/runtime"
)

// RunStartedEvent is the kernel event a workflow run records on its root
// process before the entry agent starts: what the run ran under, so the
// record states it and a replay can set it up again. Its data holds the
// run's budget, the engine's compaction and retry settings, the run_spec
// artifact holding the workflow and request, and whatever the caller puts
// in Engine.RunRecord (the build and the backend).
const RunStartedEvent = "run.started"

// RunSpecArtifact is the artifact type holding a run's workflow spec, as
// the engine ran it, and its request.
const RunSpecArtifact = "run_spec"

// RunSpec is a run_spec artifact's payload.
type RunSpec struct {
	Spec    WorkflowSpec `json:"spec"`
	Request RunRequest   `json:"request"`
}

// runStartedData is the run.started event's data.
func (e *Engine) runStartedData(specID, specSHA string) map[string]string {
	data := map[string]string{
		"run_spec":             specID,
		"run_spec_sha256":      specSHA,
		"budget_tokens":        strconv.Itoa(e.Budget.Tokens),
		"budget_seconds":       strconv.Itoa(e.Budget.Seconds),
		"budget_strict_time":   strconv.FormatBool(e.Budget.StrictTime),
		"compact_max_messages": strconv.Itoa(e.Compaction.MaxMessages),
		"compact_max_chars":    strconv.Itoa(e.Compaction.MaxChars),
		"compact_keep":         strconv.Itoa(e.Compaction.KeepMessages),
		"compact_target_chars": strconv.Itoa(e.Compaction.TargetChars),
		"compact_handoff":      strconv.FormatBool(e.Compaction.Handoff),
		"retry_attempts":       strconv.Itoa(e.BackendRetry.Attempts),
		"retry_base":           e.BackendRetry.Base.String(),
		"retry_max":            e.BackendRetry.Max.String(),
	}
	for name, value := range e.RunRecord {
		if _, own := data[name]; !own {
			data[name] = value
		}
	}
	return data
}

// RunSettings are the engine settings a run.started event records.
type RunSettings struct {
	Budget       capability.Budget
	Compaction   runtime.CompactionPolicy
	BackendRetry runtime.BackendRetryPolicy
	// Record is the event's data, for the fields the caller added.
	Record map[string]string
}

// ParseRunStarted reads the settings back from a run.started event's data.
func ParseRunStarted(data map[string]string) (RunSettings, error) {
	var settings RunSettings
	var err error
	integer := func(name string) int {
		if err != nil {
			return 0
		}
		var value int
		if value, err = strconv.Atoi(data[name]); err != nil {
			err = fmt.Errorf("run.started %s: %w", name, err)
		}
		return value
	}
	boolean := func(name string) bool {
		if err != nil {
			return false
		}
		var value bool
		if value, err = strconv.ParseBool(data[name]); err != nil {
			err = fmt.Errorf("run.started %s: %w", name, err)
		}
		return value
	}
	duration := func(name string) time.Duration {
		if err != nil {
			return 0
		}
		var value time.Duration
		if value, err = time.ParseDuration(data[name]); err != nil {
			err = fmt.Errorf("run.started %s: %w", name, err)
		}
		return value
	}
	settings.Budget = capability.Budget{Tokens: integer("budget_tokens"), Seconds: integer("budget_seconds"), StrictTime: boolean("budget_strict_time")}
	settings.Compaction = runtime.CompactionPolicy{
		MaxMessages:  integer("compact_max_messages"),
		MaxChars:     integer("compact_max_chars"),
		KeepMessages: integer("compact_keep"),
		TargetChars:  integer("compact_target_chars"),
		Handoff:      boolean("compact_handoff"),
	}
	settings.BackendRetry = runtime.BackendRetryPolicy{Attempts: integer("retry_attempts"), Base: duration("retry_base"), Max: duration("retry_max")}
	settings.Record = maps.Clone(data)
	return settings, err
}
