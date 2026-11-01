//go:build unix

package main

import (
	"os"
	"syscall"
)

// restart replaces this process with the TUI started on args and env. The
// state directory lock is released on exec, since its file is
// close-on-exec.
func restart(args, env []string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	return syscall.Exec(self, append([]string{os.Args[0]}, args...), env)
}
