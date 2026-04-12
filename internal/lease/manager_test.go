package lease_test

import (
	"path/filepath"
	"testing"
	"time"

	"aos/internal/kernel"
	"aos/internal/lease"
)

func TestLeaseAcquireConflictAndRelease(t *testing.T) {
	manager, err := lease.New(filepath.Join(t.TempDir(), "leases"))
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	first, err := manager.Acquire("src/router.go", "pid-1", time.Minute)
	if err != nil {
		t.Fatalf("acquire first: %v", err)
	}
	if first.OwnerPID != "pid-1" {
		t.Fatalf("unexpected owner %q", first.OwnerPID)
	}

	if _, err := manager.Acquire("src/router.go", "pid-2", time.Minute); err != lease.ErrLeaseHeld {
		t.Fatalf("expected held error, got %v", err)
	}

	if err := manager.Release("src/router.go", "pid-2"); err != lease.ErrLeaseOwnerMismatch {
		t.Fatalf("expected owner mismatch, got %v", err)
	}

	if err := manager.Release("src/router.go", "pid-1"); err != nil {
		t.Fatalf("release: %v", err)
	}

	if _, err := manager.Acquire("src/router.go", "pid-2", time.Minute); err != nil {
		t.Fatalf("reacquire: %v", err)
	}
}

func TestAcquireForProcessBlocksAndReleaseWakes(t *testing.T) {
	root := t.TempDir()
	manager, err := lease.New(filepath.Join(root, "leases"))
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	k, err := kernel.New(filepath.Join(root, "kernel"))
	if err != nil {
		t.Fatalf("new kernel: %v", err)
	}
	owner, err := k.Spawn(kernel.ProcessSpec{Role: "worker"})
	if err != nil {
		t.Fatalf("spawn owner: %v", err)
	}
	waiter, err := k.Spawn(kernel.ProcessSpec{Role: "worker"})
	if err != nil {
		t.Fatalf("spawn waiter: %v", err)
	}

	if _, err := manager.AcquireForProcess(k, "shared.txt", owner.PID, time.Minute); err != nil {
		t.Fatalf("owner acquire: %v", err)
	}
	if _, err := manager.AcquireForProcess(k, "shared.txt", waiter.PID, time.Minute); err != lease.ErrLeaseHeld {
		t.Fatalf("expected held error, got %v", err)
	}

	blocked, err := k.Get(waiter.PID)
	if err != nil {
		t.Fatalf("get waiter: %v", err)
	}
	if blocked.State != kernel.StateBlocked {
		t.Fatalf("expected waiter to be blocked, got %s", blocked.State)
	}

	if err := manager.ReleaseAndWake(k, "shared.txt", owner.PID, waiter.PID); err != nil {
		t.Fatalf("release and wake: %v", err)
	}
	woken, err := k.Get(waiter.PID)
	if err != nil {
		t.Fatalf("get woken waiter: %v", err)
	}
	if woken.State != kernel.StateReady {
		t.Fatalf("expected waiter to be ready, got %s", woken.State)
	}
}
