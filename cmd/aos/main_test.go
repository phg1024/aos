package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"aos/internal/kernel"
	"aos/internal/runtime"
)

// exitCompleter answers every completion with one exit action.
type exitCompleter struct{ status kernel.ExitStatus }

func (c exitCompleter) CompleteJSON(_ context.Context, _ string, _ string, target any) error {
	bytes, err := json.Marshal(map[string]any{"action": map[string]any{"type": "exit", "exit_status": c.status}})
	if err != nil {
		return err
	}
	return json.Unmarshal(bytes, target)
}

func writeSoloWorkflow(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "solo.json")
	spec := `{"name":"solo","entry_agent":"solver","agents":[{"id":"solver","system_prompt":"Solve.","max_steps":3}]}`
	if err := os.WriteFile(path, []byte(spec), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
	return path
}

func TestRunWorkflowReportsEntryFailure(t *testing.T) {
	dir := t.TempDir()
	report := filepath.Join(dir, "report.html")
	err := runWorkflow(context.Background(), exitCompleter{kernel.ExitStatus{Code: 3, Reason: "tests failed"}}, runtime.CompactionPolicy{}, nil,
		writeSoloWorkflow(t, dir), nil, "", report, filepath.Join(dir, "state"), "go", false)
	var failed workflowFailed
	if !errors.As(err, &failed) {
		t.Fatalf("expected workflowFailed, got %v", err)
	}
	if failed.exitCode() != 3 || failed.Error() != "workflow failed: entry agent solver exited with code 3: tests failed" {
		t.Fatalf("unexpected failure %q with exit code %d", failed.Error(), failed.exitCode())
	}
	if _, statErr := os.Stat(report); statErr != nil {
		t.Fatalf("the HTML report should still be written on failure: %v", statErr)
	}
}

func TestRunWorkflowSucceedsWhenEntrySucceeds(t *testing.T) {
	dir := t.TempDir()
	err := runWorkflow(context.Background(), exitCompleter{kernel.ExitStatus{Code: 0, Reason: "done"}}, runtime.CompactionPolicy{}, nil,
		writeSoloWorkflow(t, dir), nil, "", "", filepath.Join(dir, "state"), "go", false)
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
}

func TestWorkflowFailedExitCode(t *testing.T) {
	for code, want := range map[int]int{1: 1, 3: 3, 125: 125, 126: 1, 130: 1, -1: 1} {
		if got := (workflowFailed{status: kernel.ExitStatus{Code: code}}).exitCode(); got != want {
			t.Fatalf("exitCode for root code %d = %d, want %d", code, got, want)
		}
	}
}
