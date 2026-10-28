package lease_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aos/internal/kernel"
	"aos/internal/lease"
)

func TestLeaseAcquireConflictAndRelease(t *testing.T) {
	manager, err := lease.New(filepath.Join(t.TempDir(), "leases"), nil)
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
	k, err := kernel.New(filepath.Join(root, "kernel"))
	if err != nil {
		t.Fatalf("new kernel: %v", err)
	}
	manager, err := lease.New(filepath.Join(root, "leases"), k)
	if err != nil {
		t.Fatalf("new manager: %v", err)
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

func newKernelManager(t *testing.T) (*kernel.Kernel, *lease.Manager) {
	t.Helper()
	root := t.TempDir()
	k, err := kernel.New(filepath.Join(root, "kernel"))
	if err != nil {
		t.Fatalf("new kernel: %v", err)
	}
	manager, err := lease.New(filepath.Join(root, "leases"), k)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	return k, manager
}

func spawn(t *testing.T, k *kernel.Kernel) string {
	t.Helper()
	process, err := k.Spawn(kernel.ProcessSpec{Role: "worker"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	return process.PID
}

func TestOwnerRenewsKeepingToken(t *testing.T) {
	k, manager := newKernelManager(t)
	owner := spawn(t, k)
	first, err := manager.Acquire("a.txt", owner, time.Minute)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	renewed, err := manager.Acquire("a.txt", owner, time.Hour)
	if err != nil {
		t.Fatalf("renew: %v", err)
	}
	if renewed.Token != first.Token || !renewed.ExpiresAt.After(first.ExpiresAt.Add(30*time.Minute)) {
		t.Fatalf("renewal should keep token %d and extend the expiry: %+v", first.Token, renewed)
	}
}

func TestExitedOwnerFreesLease(t *testing.T) {
	k, manager := newKernelManager(t)
	owner, next := spawn(t, k), spawn(t, k)
	first, err := manager.Acquire("a.txt", owner, time.Hour)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if _, err := k.Exit(owner, kernel.ExitStatus{Code: 1, Reason: "crashed"}); err != nil {
		t.Fatalf("exit: %v", err)
	}
	if _, held, err := manager.Get("a.txt"); err != nil || held {
		t.Fatalf("a lease of an exited owner should read as free: held=%v err=%v", held, err)
	}
	taken, err := manager.Acquire("a.txt", next, time.Hour)
	if err != nil {
		t.Fatalf("takeover: %v", err)
	}
	if taken.OwnerPID != next || taken.Token <= first.Token {
		t.Fatalf("takeover = %+v, first token %d", taken, first.Token)
	}
	if err := manager.Release("a.txt", owner); err != lease.ErrLeaseOwnerMismatch {
		t.Fatalf("the old owner cannot release the new lease: %v", err)
	}
	events, _ := k.Events()
	var leaseEvents []string
	for _, event := range events {
		if strings.HasPrefix(event.Type, "lease.") {
			leaseEvents = append(leaseEvents, event.ProcessID+":"+event.Type+":"+event.Data["reason"])
		}
	}
	want := owner + ":lease.acquired:," + next + ":lease.taken_over:owner_exited"
	if strings.Join(leaseEvents, ",") != want {
		t.Fatalf("lease events = %v\nwant %s", leaseEvents, want)
	}
}

func TestExpiredLeaseIsTakenOver(t *testing.T) {
	_, manager := newKernelManager(t)
	if _, err := manager.Acquire("a.txt", "pid-x", time.Millisecond); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	time.Sleep(5 * time.Millisecond)
	taken, err := manager.Acquire("a.txt", "pid-y", time.Minute)
	if err != nil || taken.Token != 2 {
		t.Fatalf("takeover = %+v, %v", taken, err)
	}
}

func TestTransferMovesLiveLeases(t *testing.T) {
	k, manager := newKernelManager(t)
	old, other := spawn(t, k), spawn(t, k)
	if _, err := manager.Acquire("a.txt", old, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Acquire("b.txt", old, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Acquire("c.txt", other, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Exit(old, kernel.ExitStatus{Code: 1, Reason: "crashed", Retryable: true}); err != nil {
		t.Fatal(err)
	}
	recovered := spawn(t, k)
	moved, err := manager.Transfer(old, recovered)
	if err != nil || len(moved) != 2 {
		t.Fatalf("transfer = %+v, %v", moved, err)
	}
	for _, resource := range []string{"a.txt", "b.txt"} {
		current, held, err := manager.Get(resource)
		if err != nil || !held || current.OwnerPID != recovered || current.Token != 2 {
			t.Fatalf("%s = %+v held=%v err=%v", resource, current, held, err)
		}
		if err := manager.Release(resource, recovered); err != nil {
			t.Fatalf("the new owner should release %s: %v", resource, err)
		}
	}
	if current, _, _ := manager.Get("c.txt"); current.OwnerPID != other {
		t.Fatalf("another process's lease moved: %+v", current)
	}
}

func TestReadsUnversionedLeaseFile(t *testing.T) {
	root := t.TempDir()
	legacy := `{"a.txt":{"resource":"a.txt","owner_pid":"pid-9","expires_at":"` + time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"}}`
	if err := os.WriteFile(filepath.Join(root, "leases.json"), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	manager, err := lease.New(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if current, held, err := manager.Get("a.txt"); err != nil || !held || current.OwnerPID != "pid-9" {
		t.Fatalf("legacy lease = %+v held=%v err=%v", current, held, err)
	}
	if _, err := manager.Acquire("a.txt", "pid-1", time.Minute); err != lease.ErrLeaseHeld {
		t.Fatalf("legacy lease should still bind: %v", err)
	}
}
