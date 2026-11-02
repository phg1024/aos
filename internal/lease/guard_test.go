package lease_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"aos/internal/lease"
)

// guard runs Guard for writers on a target only resource "a" covers, and
// reports whether the write ran.
func guard(t *testing.T, manager *lease.Manager, writers ...string) (bool, error) {
	t.Helper()
	wrote := false
	err := manager.Guard(writers, func(resource string) bool { return resource == "a" }, func() error {
		wrote = true
		return nil
	})
	if wrote && err != nil {
		t.Fatalf("the write ran and Guard failed: %v", err)
	}
	return wrote, err
}

func fence(t *testing.T, err error) *lease.FenceError {
	t.Helper()
	var fenced *lease.FenceError
	if !errors.Is(err, lease.ErrLeaseFenced) || !errors.As(err, &fenced) {
		t.Fatalf("expected a fence, got %v", err)
	}
	return fenced
}

// Guard refuses a write under another process's live lease and a write by
// a former holder whose token went stale, and allows the holder, a writer
// whose lapsed lease no one took, and writes no lease covers.
func TestGuardFencesWrites(t *testing.T) {
	manager, err := lease.New(filepath.Join(t.TempDir(), "leases"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if wrote, err := guard(t, manager, "pid-1"); !wrote || err != nil {
		t.Fatalf("no lease: %v %v", wrote, err)
	}
	if _, err := manager.Acquire("a", "pid-1", 50*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if wrote, err := guard(t, manager, "pid-1"); !wrote || err != nil {
		t.Fatalf("the holder: %v %v", wrote, err)
	}
	wrote, err := guard(t, manager, "pid-2")
	if held := fence(t, err); wrote || held.Reason != lease.FenceHeldByOther || held.Holder != "pid-1" || held.Token != 1 {
		t.Fatalf("another process's live lease: %+v", held)
	}
	if err := manager.Guard([]string{"pid-2"}, func(string) bool { return false }, func() error { return nil }); err != nil {
		t.Fatalf("a lease that does not cover the target: %v", err)
	}
	time.Sleep(80 * time.Millisecond)
	if wrote, err := guard(t, manager, "pid-1"); !wrote || err != nil {
		t.Fatalf("a lapsed lease no one took still has the current token: %v %v", wrote, err)
	}
	if _, err := manager.Acquire("a", "pid-2", time.Hour); err != nil {
		t.Fatal(err)
	}
	wrote, err = guard(t, manager, "pid-1")
	if held := fence(t, err); wrote || held.Reason != lease.FenceHeldByOther || held.Holder != "pid-2" || held.Token != 2 {
		t.Fatalf("after a takeover: %+v", held)
	}
	if err := manager.Release("a", "pid-2"); err != nil {
		t.Fatal(err)
	}
	wrote, err = guard(t, manager, "pid-1")
	stale := fence(t, err)
	if wrote || stale.Reason != lease.FenceStaleToken || stale.HeldToken != 1 || stale.Token != 2 || stale.Holder != "" {
		t.Fatalf("a former holder after another's grant: %+v", stale)
	}
	if want := `write fenced by a lease: your token 1 for "a" is stale; it was granted again with token 2 (no one holds it now); acquire_lease again before writing`; err.Error() != want {
		t.Fatalf("message %q", err.Error())
	}
	if wrote, err := guard(t, manager, "pid-3"); !wrote || err != nil {
		t.Fatalf("a process that never held it, with the lease free: %v %v", wrote, err)
	}
	if _, err := manager.Acquire("a", "pid-1", time.Hour); err != nil {
		t.Fatal(err)
	}
	if wrote, err := guard(t, manager, "pid-1"); !wrote || err != nil {
		t.Fatalf("acquiring again makes the token current: %v %v", wrote, err)
	}
}

// A lease transferred to a recovered process counts for it, and the
// predecessor in writers does not make the successor's token stale.
func TestGuardFollowsTransfers(t *testing.T) {
	manager, err := lease.New(filepath.Join(t.TempDir(), "leases"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Acquire("a", "pid-1", time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Transfer("pid-1", "pid-2"); err != nil {
		t.Fatal(err)
	}
	if wrote, err := guard(t, manager, "pid-2", "pid-1"); !wrote || err != nil {
		t.Fatalf("the successor: %v %v", wrote, err)
	}
	wrote, err := guard(t, manager, "pid-1")
	if held := fence(t, err); wrote || held.Reason != lease.FenceHeldByOther {
		t.Fatalf("the predecessor alone: %+v", held)
	}
}

// A lease file written before holders were kept still fences: the current
// owner holds the current token, and another process is refused.
func TestGuardReadsOlderLeaseFiles(t *testing.T) {
	root := filepath.Join(t.TempDir(), "leases")
	os.MkdirAll(root, 0o755)
	expires := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	if err := os.WriteFile(filepath.Join(root, "leases.json"), []byte(`{"version":2,"leases":{"a":{"resource":"a","owner_pid":"pid-1","expires_at":"`+expires+`","token":3}},"tokens":{"a":3}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	manager, _ := lease.New(root, nil)
	if wrote, err := guard(t, manager, "pid-1"); !wrote || err != nil {
		t.Fatalf("the owner: %v %v", wrote, err)
	}
	if _, err := guard(t, manager, "pid-2"); fence(t, err).Token != 3 {
		t.Fatal("token")
	}
}

// The write runs under the lease lock: a takeover started during it waits
// until the write is done, so nothing comes between check and write.
func TestGuardHoldsTheLockThroughTheWrite(t *testing.T) {
	root := filepath.Join(t.TempDir(), "leases")
	holder, _ := lease.New(root, nil)
	other, _ := lease.New(root, nil) // a second manager, as another OS process would have
	if _, err := holder.Acquire("a", "pid-1", 30*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond) // lapsed: free to take over
	var tookOverDuringWrite bool
	err := holder.Guard([]string{"pid-1"}, func(resource string) bool { return resource == "a" }, func() error {
		done := make(chan struct{})
		go func() {
			other.Acquire("a", "pid-2", time.Hour)
			close(done)
		}()
		select {
		case <-done:
			tookOverDuringWrite = true
		case <-time.After(200 * time.Millisecond):
		}
		return nil
	})
	if err != nil || tookOverDuringWrite {
		t.Fatalf("guard = %v, takeover during the write: %v", err, tookOverDuringWrite)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if current, ok, _ := other.Get("a"); ok && current.OwnerPID == "pid-2" && current.Token == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the takeover never completed after the write")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
