//go:build linux || darwin

package sandbox_test

import (
	"os/exec"
	"testing"
)

type sleeper struct {
	Pid    int
	Exited chan struct{}
}

// startSleeper is a process of this user outside any sandbox.
func startSleeper(t *testing.T) *sleeper {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	s := &sleeper{Pid: cmd.Process.Pid, Exited: make(chan struct{})}
	go func() {
		cmd.Wait()
		close(s.Exited)
	}()
	t.Cleanup(func() { cmd.Process.Kill(); <-s.Exited })
	return s
}
