//go:build !darwin && !linux

package sandbox

import (
	"context"
	"os/exec"
)

// Available reports whether commands can be confined here: never, on this
// platform.
func Available() error { return ErrUnavailable }

// Command always fails: there is no sandbox here, so run must be refused
// unless the process may run unconfined.
func Command(context.Context, Policy, string, string) (*exec.Cmd, error) {
	return nil, ErrUnavailable
}
