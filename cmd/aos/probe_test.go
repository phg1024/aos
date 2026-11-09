package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aos/internal/agentflow"
	"aos/internal/artifact"
	"aos/internal/kernel"
	"aos/internal/memprobe"
	"aos/internal/runtime"
)

// probe-memory reads the workflow and probes, writes every sample as a
// JSON line and prints the summary table.
func TestRunProbesWritesSamplesAndTable(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	k, err := kernel.New(filepath.Join(stateDir, "kernel"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := artifact.New(filepath.Join(stateDir, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	solver, err := k.Spawn(kernel.ProcessSpec{Role: "agent:solver", Metadata: map[string]string{"agent_id": "solver"}})
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := store.CreateJSON("checkpoint", solver.PID, runtime.Session{Messages: []runtime.Message{{Role: "user", Content: "go", Seq: 1}}, NextSeq: 1}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	probes, _ := json.Marshal([]memprobe.Probe{{ID: "answer", Kind: "live", Checkpoint: checkpoint.ID, Question: "What is the answer?", Expect: "forty-two"}})
	probesPath := filepath.Join(dir, "probes.json")
	if err := os.WriteFile(probesPath, probes, 0o644); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(dir, "samples.jsonl")
	var table bytes.Buffer
	err = runProbes(context.Background(), exitCompleter{kernel.ExitStatus{Code: 0, Reason: "forty-two"}}, runtime.CompactionPolicy{},
		stateDir, writeSoloWorkflow(t, dir), probesPath, filepath.Join(dir, "work"), outPath, 2, 4, memprobe.Config{}, &table)
	if err != nil {
		t.Fatal(err)
	}
	lines, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(lines), "\n"); n != 2 || !strings.Contains(string(lines), `"correct":true`) {
		t.Fatalf("expected 2 correct samples, got:\n%s", lines)
	}
	if !strings.Contains(table.String(), "2/2") {
		t.Fatalf("table:\n%s", table.String())
	}
}

// probe-memory checks the workflow as aos run does: a misspelled field and
// a spec that breaks the contract are refused before any probe is asked.
func TestProbeMemoryRefusesAnInvalidWorkflow(t *testing.T) {
	dir := t.TempDir()
	for name, c := range map[string]struct{ spec, want string }{
		"misspelled": {`{"name":"w","entry_agent":"a","agents":[{"id":"a","system_prompt":"A.","max_step":3}]}`,
			"agents[0].max_step: unknown field; did you mean max_steps?"},
		"contract": {`{"name":"w","entry_agent":"a","agents":[{"id":"a","system_prompt":"A."},{"id":"b","system_prompt":"B."}]}`,
			`agent "b" is not reachable from entry agent "a" through allowed_agents`},
	} {
		workflowPath := filepath.Join(dir, name+".json")
		if err := os.WriteFile(workflowPath, []byte(c.spec), 0o644); err != nil {
			t.Fatal(err)
		}
		var table bytes.Buffer
		err := runProbes(context.Background(), exitCompleter{kernel.ExitStatus{Code: 0}}, runtime.CompactionPolicy{},
			filepath.Join(dir, "state"), workflowPath, filepath.Join(dir, "missing-probes.json"), filepath.Join(dir, "work"), "", 1, 4, memprobe.Config{}, &table)
		if !errors.Is(err, agentflow.ErrWorkflowSpecInvalid) || !strings.Contains(err.Error(), workflowPath+": ") || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: err %v", name, err)
		}
	}
}
