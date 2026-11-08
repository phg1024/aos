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

// The preflight names each host path that cannot work here, and passes a
// writable mount whose parent exists, which the run creates.
func TestPreflightNamesMissingHostPaths(t *testing.T) {
	root := t.TempDir()
	spec := agentflow.WorkflowSpec{Name: "w", Agents: []agentflow.AgentSpec{{ID: "a",
		Mounts: []capability.Mount{
			{Name: "ref", HostPath: filepath.Join(root, "missing-ref"), ReadOnly: true},
			{Name: "out", HostPath: filepath.Join(root, "new-out")},
			{Name: "deep", HostPath: filepath.Join(root, "no-parent", "out")},
		},
		Capabilities: capability.Set{ReadPaths: []string{root, filepath.Join(root, "missing-read")}}}}}
	err := preflightHostPaths(spec, []string{filepath.Join(root, "skills")})
	if !errors.Is(err, ErrPreflight) {
		t.Fatalf("err %v", err)
	}
	for _, want := range []string{
		"workflow w names host paths that do not exist on this machine; nothing was run:",
		"skill_roots[0]: " + filepath.Join(root, "skills") + " does not exist",
		`agents[0] (a).mounts[0] "ref", read-only: ` + filepath.Join(root, "missing-ref") + " does not exist",
		`agents[0] (a).mounts[2] "deep", writable: neither ` + filepath.Join(root, "no-parent", "out") + " nor its parent exists",
		"agents[0] (a).capabilities.read_paths[1]: " + filepath.Join(root, "missing-read") + " does not exist",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("%v\nlacks %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "new-out") || strings.Contains(err.Error(), "read_paths[0]") {
		t.Fatalf("a path that works was named: %v", err)
	}
	spec.Agents[0].Mounts[0].HostPath = "__RUN_DIR__/ref"
	if err := preflightHostPaths(spec, nil); !strings.Contains(err.Error(), "placeholder such as __RUN_DIR__") {
		t.Fatalf("placeholder hint: %v", err)
	}
}

// aos run --workflow refuses, before it makes a state directory or spawns
// anything, a checked-in workflow whose paths are another machine's, and
// a spec with a misspelled field.
func TestRunWorkflowChecksBeforeRunning(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	err := runWorkflow(context.Background(), nil, runtime.CompactionPolicy{}, capability.Budget{}, "../../workflows/rasterizer-bunny.json", nil, "", "", stateDir, "go", false)
	if !errors.Is(err, ErrPreflight) || !strings.Contains(err.Error(), "workflow rasterizer-bunny names host paths that do not exist on this machine") {
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
