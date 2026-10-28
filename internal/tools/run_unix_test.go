//go:build unix

package tools_test

import (
	"context"
	"errors"
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

func TestRunToolCancelledByContext(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)

	start := time.Now()
	_, err := tools.NewRuntime().ExecuteContext(ctx, tools.Request{
		Tool:          "run",
		Args:          map[string]string{"command": "sleep 30", "timeout": "60s"},
		Mounts:        []capability.Mount{{Name: "workspace", HostPath: root}},
		CapabilitySet: capability.Set{Tools: []string{"run"}},
	})
	if !errors.Is(err, tools.ErrToolCancelled) || !errors.Is(err, context.Canceled) {
		t.Fatalf("expected ErrToolCancelled wrapping context.Canceled, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("cancel took %s", elapsed)
	}
}
