package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aos/internal/agentflow"
	"aos/internal/capability"
	"aos/internal/runtime"
)

// aos run --workflow refuses, before it makes a state directory or spawns
// anything, a checked-in workflow whose paths are another machine's, and
// a spec with a misspelled field.
func TestRunWorkflowChecksBeforeRunning(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	err := runWorkflow(context.Background(), nil, runtime.CompactionPolicy{}, capability.Budget{}, "../../workflows/rasterizer-bunny-metal.json", nil, "", "", stateDir, "go", false)
	if !errors.Is(err, agentflow.ErrPreflight) || !strings.Contains(err.Error(), "workflow rasterizer-bunny-metal names host paths that do not exist on this machine") {
		t.Fatalf("err %v", err)
	}
	if _, statErr := os.Stat(stateDir); statErr == nil {
		t.Fatal("the refused run made its state directory")
	}
	misspelled := filepath.Join(t.TempDir(), "w.json")
	os.WriteFile(misspelled, []byte(`{"name":"w","entry_agent":"a","verifcation":{"policy":"required","verifier":"b"},"agents":[{"id":"a","system_prompt":"A."}]}`), 0o644)
	err = runWorkflow(context.Background(), nil, runtime.CompactionPolicy{}, capability.Budget{}, misspelled, nil, "", "", stateDir, "go", false)
	if !errors.Is(err, agentflow.ErrWorkflowSpecInvalid) || !strings.Contains(err.Error(), "verifcation: unknown field; did you mean verification?") {
		t.Fatalf("err %v", err)
	}
}
