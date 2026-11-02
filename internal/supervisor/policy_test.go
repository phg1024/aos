package supervisor_test

import (
	"errors"
	"testing"
	"time"

	"aos/internal/supervisor"
)

func TestEffectiveAppliesBoundedDefaults(t *testing.T) {
	policy, err := supervisor.RestartPolicy{}.Effective()
	if err != nil || policy.Max != supervisor.DefaultMax || policy.Within != supervisor.DefaultWithin || policy.BackoffBase != time.Second || policy.BackoffMax != 30*time.Second {
		t.Fatalf("defaults = %+v, %v", policy, err)
	}
	policy, err = supervisor.RestartPolicy{Max: 2, Within: "1m", Backoff: "250ms"}.Effective()
	if err != nil || policy.Max != 2 || policy.Within != time.Minute || policy.BackoffBase != 250*time.Millisecond || policy.BackoffMax != 250*time.Millisecond {
		t.Fatalf("explicit = %+v, %v", policy, err)
	}
	if policy.Total != supervisor.DefaultTotal {
		t.Fatalf("explicit total = %d", policy.Total)
	}
	if policy, err = (supervisor.RestartPolicy{Max: 20, Within: "1h", Backoff: "0s"}).Effective(); err != nil || policy.Total != 20 {
		t.Fatalf("a max above the default total: %+v, %v", policy, err)
	}
	for _, bad := range []supervisor.RestartPolicy{{Max: -1}, {Within: "0s"}, {Within: "soon"}, {Backoff: "2s..1s"}, {Backoff: "x"}, {Backoff: "-1s"},
		// Windows too short to fill: under MinWithin, or not outlasting the
		// backoffs of max restarts (1s, 2s and 4s by default).
		{Within: "1ns", Backoff: "0s"}, {Within: "999ms", Backoff: "0s"}, {Within: "7s"}, {Max: 2, Within: "1m", Backoff: "40s"},
		// Restart counts out of bounds.
		{Max: supervisor.MaxRestarts + 1}, {Total: -1}, {Total: supervisor.MaxRestarts + 1}, {Max: 5, Total: 4}} {
		if _, err := bad.Effective(); !errors.Is(err, supervisor.ErrInvalidPolicy) {
			t.Fatalf("%+v: %v", bad, err)
		}
	}
}

func TestBackoffDoublesUpToTheMaximum(t *testing.T) {
	policy, _ := supervisor.RestartPolicy{Backoff: "1s..5s"}.Effective()
	var got []time.Duration
	for n := 1; n <= 5; n++ {
		got = append(got, policy.Backoff(n))
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 5 * time.Second, 5 * time.Second}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("backoff = %v, want %v", got, want)
		}
	}
	if zero, _ := (supervisor.RestartPolicy{Backoff: "0s"}).Effective(); zero.Backoff(3) != 0 {
		t.Fatal("a zero backoff waits")
	}
}

func TestIntensityCountsTheWindow(t *testing.T) {
	policy, _ := supervisor.RestartPolicy{Max: 2, Within: "1m"}.Effective()
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	old := now.Add(-2 * time.Minute)
	if policy.Exceeded([]time.Time{old, old, now.Add(-time.Second)}, now) {
		t.Fatal("restarts outside the window count")
	}
	if !policy.Exceeded([]time.Time{now.Add(-30 * time.Second), now.Add(-time.Second)}, now) {
		t.Fatal("max restarts within the window should exceed on the next")
	}
	total, _ := supervisor.RestartPolicy{Max: 2, Within: "1m", Total: 3}.Effective()
	if spread := []time.Time{now.Add(-time.Hour), now.Add(-30 * time.Minute)}; total.Exceeded(spread, now) {
		t.Fatal("2 of a total of 3 should not exceed")
	}
	if spread := []time.Time{now.Add(-time.Hour), now.Add(-30 * time.Minute), now.Add(-10 * time.Minute)}; !total.Exceeded(spread, now) {
		t.Fatal("the total should exceed with no restart in the window")
	}
}
