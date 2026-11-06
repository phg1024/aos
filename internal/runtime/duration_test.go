package runtime_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"aos/internal/runtime"
	"aos/internal/tools"
)

// acquire_lease reads ttl as run reads timeout: a bare number is seconds, and what
// it cannot read reaches the model as a tool error, with no lease taken.
func TestAcquireLeaseReadsItsTTL(t *testing.T) {
	f := newFenceFixture(t)
	process := f.spawn(t)
	var outputs []map[string]string
	agent := f.runtime(steps(
		toolAction("acquire_lease", map[string]string{"resource": "bare", "ttl": "300"}),
		toolAction("acquire_lease", map[string]string{"resource": "go", "ttl": "5m"}),
		toolAction("acquire_lease", map[string]string{"resource": "plain"}),
		toolAction("acquire_lease", map[string]string{"resource": "words", "ttl": "soon"}),
		toolAction("acquire_lease", map[string]string{"resource": "zero", "ttl": "0"}),
		toolAction("acquire_lease", map[string]string{"resource": "negative", "ttl": "-5s"}),
	))
	start := time.Now()
	result, err := agent.RunWithOptions(context.Background(), process.PID, nil, runtime.RunOptions{ContinueOnToolError: true, OnToolResult: func(result tools.Result) {
		outputs = append(outputs, result.Output)
	}})
	if err != nil {
		t.Fatal(err)
	}
	errs := toolErrors(result)
	if len(errs) != 3 {
		t.Fatalf("tool errors: %v", errs)
	}
	for _, text := range errs {
		if !strings.Contains(text, "acquire_lease ttl") || !strings.Contains(text, "whole number of seconds") {
			t.Fatalf("tool error %q", text)
		}
	}
	if got := len(f.events(t, "lease.acquired")); got != 3 {
		t.Fatalf("%d leases taken, want 3", got)
	}
	var expires []time.Duration
	for _, output := range outputs {
		if output["expires_at"] == "" {
			continue
		}
		at, err := time.Parse(time.RFC3339, output["expires_at"])
		if err != nil {
			t.Fatal(err)
		}
		expires = append(expires, at.Sub(start))
	}
	want := []time.Duration{5 * time.Minute, 5 * time.Minute, 10 * time.Minute}
	if len(expires) != len(want) {
		t.Fatalf("outputs = %v", outputs)
	}
	for i, left := range expires {
		if left < want[i]-2*time.Second || left > want[i]+time.Minute {
			t.Fatalf("lease %d expires %s after the start, want about %s", i, left, want[i])
		}
	}
}
