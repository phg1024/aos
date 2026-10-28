//go:build unix

package statedir_test

import (
	"errors"
	"path/filepath"
	"testing"

	"aos/internal/kernel"
	"aos/internal/statedir"
)

// A live run's processes are not orphans: while the lock is held, InUse
// says so and Reap refuses.
func TestReapRefusesWhileARunHoldsTheLock(t *testing.T) {
	dir := t.TempDir()
	k, err := kernel.New(filepath.Join(dir, "kernel"))
	if err != nil {
		t.Fatal(err)
	}
	live, _ := k.Spawn(kernel.ProcessSpec{Role: "worker"})
	if inUse, err := statedir.InUse(dir); err != nil || inUse {
		t.Fatalf("never-locked dir in use = %v, %v", inUse, err)
	}
	release, err := statedir.Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := statedir.Lock(dir); !errors.Is(err, statedir.ErrInUse) {
		t.Fatalf("second lock: %v", err)
	}
	if inUse, err := statedir.InUse(dir); err != nil || !inUse {
		t.Fatalf("locked dir in use = %v, %v", inUse, err)
	}
	if _, err := statedir.Reap(dir); !errors.Is(err, statedir.ErrInUse) {
		t.Fatalf("reap under a live run: %v", err)
	}
	if process, _ := k.Get(live.PID); process.State == kernel.StateExited {
		t.Fatal("a live run's process was reaped")
	}
	release()
	if inUse, _ := statedir.InUse(dir); inUse {
		t.Fatal("released dir still in use")
	}
	reaped, err := statedir.Reap(dir)
	if err != nil || len(reaped) != 1 || reaped[0] != live.PID {
		t.Fatalf("reap after release = %v, %v", reaped, err)
	}
}

func TestReapRejectsNonStateDir(t *testing.T) {
	if _, err := statedir.Reap(t.TempDir()); err == nil {
		t.Fatal("expected an error for a directory without a kernel")
	}
}
