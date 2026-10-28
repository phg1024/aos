//go:build unix

package filelock

import (
	"os"
	"syscall"
)

func lock(file *os.File, wait bool) error {
	how := syscall.LOCK_EX
	if !wait {
		how |= syscall.LOCK_NB
	}
	for {
		err := syscall.Flock(int(file.Fd()), how)
		switch err {
		case nil:
			return nil
		case syscall.EINTR:
			continue
		case syscall.EWOULDBLOCK:
			return ErrLocked
		default:
			return err
		}
	}
}

func unlock(file *os.File) {
	_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
}

// Supported reports whether locks coordinate separate processes here.
const Supported = true
