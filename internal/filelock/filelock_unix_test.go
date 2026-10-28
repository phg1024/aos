//go:build unix

package filelock_test

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"aos/internal/filelock"
)

func TestTryAcquireSeesHeldLockAndAcquireWaits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.lock")
	held, err := filelock.Acquire(path)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if _, err := filelock.TryAcquire(path); !errors.Is(err, filelock.ErrLocked) {
		t.Fatalf("a second holder should get ErrLocked, got %v", err)
	}
	acquired := make(chan *filelock.Lock)
	go func() {
		second, err := filelock.Acquire(path)
		if err != nil {
			t.Errorf("blocking acquire: %v", err)
		}
		acquired <- second
	}()
	select {
	case <-acquired:
		t.Fatal("Acquire returned while the lock was held")
	case <-time.After(100 * time.Millisecond):
	}
	held.Release()
	second := <-acquired
	second.Release()
}
