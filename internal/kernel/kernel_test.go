package kernel_test

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aos/internal/capability"
	"aos/internal/kernel"
)

func TestKernelSpawnTransitionExitAndPersistence(t *testing.T) {
	root := t.TempDir()
	k, err := kernel.New(root)
	if err != nil {
		t.Fatalf("new kernel: %v", err)
	}

	process, err := k.Spawn(kernel.ProcessSpec{
		Role:      "planner",
		ParentPID: "root",
		Metadata: map[string]string{
			"story": "kernel bootstrap",
		},
		Mounts: []capability.Mount{
			{Name: "workspace", HostPath: filepath.Join(root, "workspace")},
		},
		CapabilitySet: capability.Set{},
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}

	if process.State != kernel.StateReady {
		t.Fatalf("expected ready state, got %s", process.State)
	}

	if _, err := k.Transition(process.PID, kernel.StateRunning); err != nil {
		t.Fatalf("transition to running: %v", err)
	}

	if _, err := k.Exit(process.PID, kernel.ExitStatus{Code: 0, Reason: "done"}); err != nil {
		t.Fatalf("exit: %v", err)
	}

	reloaded, err := kernel.New(root)
	if err != nil {
		t.Fatalf("reload kernel: %v", err)
	}

	stored, err := reloaded.Get(process.PID)
	if err != nil {
		t.Fatalf("get process: %v", err)
	}
	if stored.ParentPID != "root" {
		t.Fatalf("expected parent pid to persist, got %q", stored.ParentPID)
	}
	if stored.ExitStatus == nil || stored.ExitStatus.Reason != "done" {
		t.Fatalf("expected exit status to persist, got %#v", stored.ExitStatus)
	}

	events, err := reloaded.Events()
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d", len(events))
	}
}

func TestInvalidTransitionRejected(t *testing.T) {
	if err := kernel.ValidateTransition(kernel.StateReady, kernel.StateReady); err != nil {
		t.Fatalf("same-state transition should be allowed: %v", err)
	}

	if err := kernel.ValidateTransition(kernel.StateExited, kernel.StateRunning); err != kernel.ErrInvalidTransition {
		t.Fatalf("expected invalid transition error, got %v", err)
	}
}

func TestAwaitRequiresExitedProcess(t *testing.T) {
	root := t.TempDir()
	k, err := kernel.New(root)
	if err != nil {
		t.Fatalf("new kernel: %v", err)
	}
	process, err := k.Spawn(kernel.ProcessSpec{Role: "worker"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if _, err := k.Await(process.PID); err != kernel.ErrProcessNotExited {
		t.Fatalf("expected not exited error, got %v", err)
	}
}

func TestExitIsTerminal(t *testing.T) {
	k, err := kernel.New(t.TempDir())
	if err != nil {
		t.Fatalf("new kernel: %v", err)
	}
	process, err := k.Spawn(kernel.ProcessSpec{Role: "worker"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	first := kernel.ExitStatus{Code: 1, Reason: "malformed output", Retryable: false}
	if _, err := k.Exit(process.PID, first); err != nil {
		t.Fatalf("first exit: %v", err)
	}
	events, err := k.Events()
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	eventCount := len(events)

	_, err = k.Exit(process.PID, kernel.ExitStatus{Code: 1, Reason: "retry me", Retryable: true})
	if !errors.Is(err, kernel.ErrProcessAlreadyExited) || !errors.Is(err, kernel.ErrInvalidTransition) {
		t.Fatalf("expected already-exited error, got %v", err)
	}
	if _, err := k.Exit(process.PID, first); err != nil {
		t.Fatalf("expected repeating the same exit to be a no-op, got %v", err)
	}

	status, err := k.Await(process.PID)
	if err != nil {
		t.Fatalf("await: %v", err)
	}
	if status != first {
		t.Fatalf("expected exit status to stay %#v, got %#v", first, status)
	}
	events, err = k.Events()
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if len(events) != eventCount {
		t.Fatalf("expected no new events after repeated exit, got %d -> %d", eventCount, len(events))
	}
}

func TestStoreWritesLeaveNoTempFiles(t *testing.T) {
	root := t.TempDir()
	k, err := kernel.New(root)
	if err != nil {
		t.Fatalf("new kernel: %v", err)
	}
	process, err := k.Spawn(kernel.ProcessSpec{Role: "worker"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if _, err := k.Exit(process.PID, kernel.ExitStatus{Code: 0, Reason: "done"}); err != nil {
		t.Fatalf("exit: %v", err)
	}
	leftovers, err := filepath.Glob(filepath.Join(root, "*.tmp-*"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(leftovers) != 0 {
		t.Fatalf("expected no temp files, got %v", leftovers)
	}
	reloaded, err := kernel.New(root)
	if err != nil {
		t.Fatalf("reload kernel: %v", err)
	}
	if status, err := reloaded.Await(process.PID); err != nil || status.Reason != "done" {
		t.Fatalf("expected persisted exit after reload, got %#v, %v", status, err)
	}
}

// Only Exit may produce exited: a bare Transition to exited would leave a
// process that Await cannot read and Exit can no longer record.
func TestTransitionCannotReachOrLeaveExited(t *testing.T) {
	k, err := kernel.New(t.TempDir())
	if err != nil {
		t.Fatalf("new kernel: %v", err)
	}
	for _, from := range []kernel.ProcessState{kernel.StateReady, kernel.StateRunning, kernel.StateBlocked, kernel.StateSleeping} {
		process, err := k.Spawn(kernel.ProcessSpec{Role: "worker"})
		if err != nil {
			t.Fatalf("spawn: %v", err)
		}
		if from != kernel.StateReady {
			if _, err := k.Transition(process.PID, from); err != nil {
				t.Fatalf("transition to %s: %v", from, err)
			}
		}
		_, err = k.Transition(process.PID, kernel.StateExited)
		if !errors.Is(err, kernel.ErrExitRequiresStatus) || !errors.Is(err, kernel.ErrInvalidTransition) {
			t.Fatalf("%s -> exited: expected ErrExitRequiresStatus, got %v", from, err)
		}
		got, err := k.Get(process.PID)
		if err != nil || got.State != from || got.ExitStatus != nil {
			t.Fatalf("%s -> exited must leave the process untouched, got %+v (err %v)", from, got, err)
		}
		status := kernel.ExitStatus{Code: 0, Reason: "done"}
		if _, err := k.Exit(process.PID, status); err != nil {
			t.Fatalf("exit after refused transition from %s: %v", from, err)
		}
		if awaited, err := k.Await(process.PID); err != nil || awaited != status {
			t.Fatalf("await after exit from %s: %+v %v", from, awaited, err)
		}
	}

	process, err := k.Spawn(kernel.ProcessSpec{Role: "worker"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if _, err := k.Exit(process.PID, kernel.ExitStatus{Code: 0, Reason: "done"}); err != nil {
		t.Fatalf("exit: %v", err)
	}
	events, err := k.Events()
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	for _, next := range []kernel.ProcessState{kernel.StateExited, kernel.StateReady, kernel.StateRunning} {
		if _, err := k.Transition(process.PID, next); !errors.Is(err, kernel.ErrInvalidTransition) {
			t.Fatalf("exited -> %s: expected an invalid transition, got %v", next, err)
		}
	}
	after, err := k.Events()
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if len(after) != len(events) {
		t.Fatalf("refused transitions must not log events: %d -> %d", len(events), len(after))
	}
}

func TestLogEventRejectsKernelReservedTypes(t *testing.T) {
	k, err := kernel.New(t.TempDir())
	if err != nil {
		t.Fatalf("new kernel: %v", err)
	}
	process, err := k.Spawn(kernel.ProcessSpec{Role: "worker"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	for _, eventType := range []string{"process.exited", "process.spawned", "process.transition", "process.anything"} {
		if err := k.LogEvent(process.PID, eventType, map[string]string{"code": "0"}); !errors.Is(err, kernel.ErrReservedEventType) {
			t.Fatalf("LogEvent(%q): expected ErrReservedEventType, got %v", eventType, err)
		}
	}
	for _, eventType := range []string{"scheduler.dispatch", "supervisor.retry", "processing.note"} {
		if err := k.LogEvent(process.PID, eventType, nil); err != nil {
			t.Fatalf("LogEvent(%q) should be allowed: %v", eventType, err)
		}
	}
	events, err := k.Events()
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	for _, event := range events {
		if event.Type == "process.exited" {
			t.Fatalf("a refused caller event reached the log: %+v", event)
		}
	}
	if got, _ := k.Get(process.PID); got.State != kernel.StateReady {
		t.Fatalf("process state changed: %s", got.State)
	}
}

// Supersede spawns the successor and records it on the exited process in
// one step, refuses a second successor, and refuses a live process.
func TestSupersedeRecordsOneSuccessor(t *testing.T) {
	k, err := kernel.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	live, err := k.Spawn(kernel.ProcessSpec{Role: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k.Supersede(live.PID, kernel.ProcessSpec{Role: "worker"}, nil); !errors.Is(err, kernel.ErrInvalidTransition) {
		t.Fatalf("superseding a live process: %v", err)
	}
	if _, err := k.Exit(live.PID, kernel.ExitStatus{Code: 1, Reason: "transient", Retryable: true}); err != nil {
		t.Fatal(err)
	}
	successor, err := k.Supersede(live.PID, kernel.ProcessSpec{Role: "worker", Metadata: map[string]string{"resumed_from": live.PID}}, map[string]string{"checkpoint": "art-000003"})
	if err != nil {
		t.Fatal(err)
	}
	if successor.State != kernel.StateReady || successor.Metadata["resumed_from"] != live.PID {
		t.Fatalf("successor = %+v", successor)
	}
	previous, _ := k.Get(live.PID)
	if previous.SupersededBy != successor.PID || previous.State != kernel.StateExited || previous.ExitStatus == nil {
		t.Fatalf("previous = %+v", previous)
	}
	events, _ := k.Events()
	last := events[len(events)-1]
	if last.Type != "process.superseded" || last.ProcessID != live.PID || last.Data["successor"] != successor.PID || last.Data["checkpoint"] != "art-000003" {
		t.Fatalf("last event = %+v", last)
	}
	if events[len(events)-2].Type != "process.spawned" || events[len(events)-2].ProcessID != successor.PID {
		t.Fatalf("the successor's spawn should come first: %+v", events[len(events)-2])
	}
	before := len(events)
	_, err = k.Supersede(live.PID, kernel.ProcessSpec{Role: "worker"}, nil)
	if !errors.Is(err, kernel.ErrSuperseded) || !strings.Contains(err.Error(), successor.PID) {
		t.Fatalf("a second supersede: %v", err)
	}
	if events, _ := k.Events(); len(events) != before {
		t.Fatal("a refused supersede wrote events")
	}
	if err := k.LogEvent(live.PID, "process.superseded", nil); !errors.Is(err, kernel.ErrReservedEventType) {
		t.Fatalf("callers cannot log a supersession: %v", err)
	}
}

// RecordEventFunc decides at one reading of the clock and stamps the
// event with it; a decision that records nothing does not advance the
// clock. Now reads the clock without advancing it.
func TestRecordEventFuncUsesOneReading(t *testing.T) {
	k, err := kernel.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	n := 0
	k.SetClocks(func() time.Time { n++; return base.Add(time.Duration(n-1) * time.Second) }, func() time.Time { return base.Add(time.Duration(n) * time.Second) })
	if got := k.Now(); !got.Equal(base) || n != 0 {
		t.Fatalf("Now = %s, readings %d", got, n)
	}
	var seen time.Time
	event, err := k.RecordEventFunc("pid-000001", func(now time.Time, lookup func(string) (kernel.Process, error)) (string, map[string]string, error) {
		seen = now
		if _, err := lookup("pid-000001"); !errors.Is(err, kernel.ErrProcessNotFound) {
			t.Errorf("lookup: %v", err)
		}
		return "test.decided", map[string]string{"at": now.Format(time.RFC3339Nano)}, nil
	})
	if err != nil || !event.Timestamp.Equal(seen) || !seen.Equal(base) || n != 1 {
		t.Fatalf("event %+v, decided at %s, readings %d: %v", event, seen, n, err)
	}
	if _, err := k.RecordEventFunc("pid-000001", func(time.Time, func(string) (kernel.Process, error)) (string, map[string]string, error) {
		return "", nil, errors.New("refused")
	}); err == nil || n != 1 {
		t.Fatalf("a refused decision: %v, readings %d", err, n)
	}
	next, err := k.RecordEvent("pid-000001", "test.next", nil)
	if err != nil || !next.Timestamp.Equal(base.Add(time.Second)) {
		t.Fatalf("the next event should take the next reading: %+v %v", next, err)
	}
	if _, err := k.RecordEventFunc("pid-000001", func(time.Time, func(string) (kernel.Process, error)) (string, map[string]string, error) {
		return "process.spawned", nil, nil
	}); !errors.Is(err, kernel.ErrReservedEventType) {
		t.Fatalf("a reserved type: %v", err)
	}
}
