// Package filelock provides exclusive locks on files that coordinate
// separate processes (and separate opens within one process). On unix it
// uses flock; elsewhere a lock only keeps the file open and coordinates
// nothing, so callers must still serialise within their own process.
package filelock

import (
	"errors"
	"os"
)

// ErrLocked is returned by TryAcquire when another holder has the lock.
var ErrLocked = errors.New("file is locked by another holder")

// Lock is a held lock on a file. Release it exactly once.
type Lock struct {
	file *os.File
}

// Acquire blocks until it holds an exclusive lock on path, creating the
// file if needed.
func Acquire(path string) (*Lock, error) {
	return acquire(path, true)
}

// TryAcquire takes the lock only if it is free, else returns ErrLocked.
func TryAcquire(path string) (*Lock, error) {
	return acquire(path, false)
}

func acquire(path string, wait bool) (*Lock, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := lock(file, wait); err != nil {
		file.Close()
		return nil, err
	}
	return &Lock{file: file}, nil
}

// File is the open lock file, e.g. to record who holds it.
func (l *Lock) File() *os.File { return l.file }

// Release drops the lock and closes the file.
func (l *Lock) Release() {
	unlock(l.file)
	l.file.Close()
}
