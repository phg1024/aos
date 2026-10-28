//go:build unix

package kernel

import (
	"os"
	"syscall"
)

// lockFile holds an exclusive flock on path until the returned function is
// called. flock locks belong to the open file, so separate processes, and
// separate Kernel instances in one process, exclude each other.
func lockFile(path string) (func(), error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	for {
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX)
		if err != syscall.EINTR {
			break
		}
	}
	if err != nil {
		file.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		file.Close()
	}, nil
}
