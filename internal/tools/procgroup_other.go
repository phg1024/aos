//go:build !unix

package tools

import "os/exec"

func isolateProcessGroup(cmd *exec.Cmd) {}
