package runtime_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"aos/internal/runtime"
	"aos/internal/tools"
)

func TestClockNote(t *testing.T) {
	start := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	now := start
	clock := &runtime.Clock{Deadline: start.Add(15 * time.Minute), Budget: 15 * time.Minute, Now: func() time.Time { return now }}

	now = start.Add(4*time.Minute + 10*time.Second)
	if got := clock.Note(); got != "clock time_budget=15m0s elapsed=4m10s remaining=10m50s" {
		t.Fatalf("unexpected note %q", got)
	}
	now = start.Add(12 * time.Minute)
	if got := clock.Note(); !strings.HasSuffix(got, "remaining=3m0s warning=less_than_20_percent_left_save_a_complete_answer_now") {
		t.Fatalf("expected a warning at 20%% left, got %q", got)
	}
	now = start.Add(20 * time.Minute)
	if got := clock.Note(); !strings.Contains(got, "elapsed=15m0s remaining=0s warning=") {
		t.Fatalf("past the deadline should clamp to zero remaining, got %q", got)
	}
}

func TestAgentRuntimeAppendsClockNoteEachStep(t *testing.T) {
	k, store, process := newCancelFixture(t)
	start := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	now := start
	var seen [][]runtime.Message
	backend := &funcBackend{fn: func(call int, ctx context.Context, request runtime.GenerateRequest) (runtime.GenerateResult, error) {
		seen = append(seen, request.History)
		now = now.Add(time.Minute)
		if call == 1 {
			return toolAction("run", map[string]string{"command": "true"}), nil
		}
		return exitResult("done"), nil
	}}
	agentRuntime := &runtime.AgentRuntime{
		Kernel: k, Artifacts: store, Tools: tools.NewRuntime(), Backend: backend, MaxSteps: 5,
		Clock: &runtime.Clock{Deadline: start.Add(10 * time.Minute), Budget: 10 * time.Minute, Now: func() time.Time { return now }},
	}
	if _, err := agentRuntime.RunWithOptions(context.Background(), process.PID, []runtime.Message{{Role: "user", Content: "go"}}, runtime.RunOptions{}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(seen) != 2 {
		t.Fatalf("expected 2 model calls, got %d", len(seen))
	}
	first, second := seen[0], seen[1]
	if last := first[len(first)-1]; last.Content != "clock time_budget=10m0s elapsed=0s remaining=10m0s" {
		t.Fatalf("first step should end with the starting clock note, got %q", last.Content)
	}
	if last := second[len(second)-1]; last.Content != "clock time_budget=10m0s elapsed=1m0s remaining=9m0s" {
		t.Fatalf("second step should end with an updated clock note, got %q", last.Content)
	}
	notes := 0
	for _, message := range second {
		if strings.HasPrefix(message.Content, "clock ") {
			notes++
		}
	}
	if notes != 2 {
		t.Fatalf("expected one note per step kept in history, got %d", notes)
	}
}
