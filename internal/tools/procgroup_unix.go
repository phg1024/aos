//go:build unix

package tools

import (
	"os/exec"
	"syscall"
)

// isolateProcessGroup runs the command in its own process group and makes
// context cancellation kill the whole group, so grandchildren spawned by
// `sh -c` cannot outlive the timeout and hold the output pipes open.
func isolateProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
