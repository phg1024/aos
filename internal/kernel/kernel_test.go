package kernel_test

import (
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
