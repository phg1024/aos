//go:build !unix

package main

import (
	"os"
	"os/exec"
)

// restart runs the TUI on args as a child and exits with its status, where
// a process cannot replace itself.
func restart(args []string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(self, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err = cmd.Run()
	if exitErr, ok := err.(*exec.ExitError); ok {
		os.Exit(exitErr.ExitCode())
	}
	if err == nil {
		os.Exit(0)
	}
	return err
}
