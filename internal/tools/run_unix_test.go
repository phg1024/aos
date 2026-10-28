//go:build unix

package tools_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"aos/internal/capability"
	"aos/internal/tools"
)

func TestRunToolTimeoutKillsDescendants(t *testing.T) {
	root := t.TempDir()
	pidFile := filepath.Join(root, "child.pid")
	runtime := tools.NewRuntime()

	start := time.Now()
	_, err := runtime.Execute(tools.Request{
		Tool: "run",
		Args: map[string]string{
			// The backgrounded sleep inherits stdout; before the fix it held the
			// pipe open and Execute blocked until it exited.
			"command": "sleep 30 & echo $! > " + pidFile + "; wait",
			"timeout": "300ms",
		},
		Mounts:        []capability.Mount{{Name: "workspace", HostPath: root}},
		CapabilitySet: capability.Set{Tools: []string{"run"}},
	})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected timeout error, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("run blocked for %s after timeout", elapsed)
	}
	raw, readErr := os.ReadFile(pidFile)
	if readErr != nil {
		t.Fatalf("read pid file: %v", readErr)
	}
	pid, convErr := strconv.Atoi(strings.TrimSpace(string(raw)))
	if convErr != nil {
		t.Fatalf("parse pid: %v", convErr)
	}
	deadline := time.Now().Add(2 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("background child %d survived the timeout", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
