//go:build unix

package tools_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
		CapabilitySet: capability.Set{Tools: []string{"run"}, ReadPaths: []string{root}, WritePaths: []string{root}},
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
		CapabilitySet: capability.Set{Tools: []string{"run"}, ReadPaths: []string{root}, WritePaths: []string{root}},
	})
	if !errors.Is(err, tools.ErrToolCancelled) || !errors.Is(err, context.Canceled) {
		t.Fatalf("expected ErrToolCancelled wrapping context.Canceled, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("cancel took %s", elapsed)
	}
}

// A run asked to hash its outputs records each file it wrote by > or >>,
// as it left it; one outside the grammar, or not asked, records none.
func TestRunHashesWhatItWroteByRedirection(t *testing.T) {
	root := t.TempDir()
	run := func(command string, hash bool) tools.Result {
		t.Helper()
		result, err := tools.NewRuntime().Execute(tools.Request{
			Tool:          "run",
			Args:          map[string]string{"command": command},
			Mounts:        []capability.Mount{{Name: "workspace", HostPath: root}},
			CapabilitySet: capability.Set{Tools: []string{"run"}, ReadPaths: []string{root}, WritePaths: []string{root}},
			HashOutputs:   hash,
		})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	out, log := filepath.Join(root, "out.txt"), filepath.Join(root, "log.txt")
	result := run("echo frame_ms=1 > out.txt; echo done >> "+log+" 2>&1", true)
	sum := sha256.Sum256([]byte("frame_ms=1\n"))
	if len(result.Wrote) != 2 || result.Wrote[out] != hex.EncodeToString(sum[:]) || len(result.Wrote[log]) != 64 {
		t.Fatalf("wrote %v", result.Wrote)
	}
	if result := run("echo x > out.txt", false); result.Wrote != nil {
		t.Fatalf("not asked: wrote %v", result.Wrote)
	}
	if result := run("echo $HOME > out.txt", true); result.Wrote != nil {
		t.Fatalf("outside the grammar: wrote %v", result.Wrote)
	}
}
