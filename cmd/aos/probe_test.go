package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
		stateDir, writeSoloWorkflow(t, dir), probesPath, filepath.Join(dir, "work"), outPath, 2, 4, &table)
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
