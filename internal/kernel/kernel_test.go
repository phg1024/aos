package kernel_test

import (
	"errors"
	"path/filepath"
	"testing"

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
