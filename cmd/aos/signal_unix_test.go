//go:build unix

package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"aos/internal/artifact"
	"aos/internal/capability"
	"aos/internal/inference"
	"aos/internal/kernel"
	"aos/internal/runlog"
	"aos/internal/runtime"
)

const signalHelperStateEnv = "AOS_SIGNAL_HELPER_STATE"

// TestSignalHelperProcess runs a one-agent workflow, as aos run does, that
// hangs on its second model call.
func TestSignalHelperProcess(t *testing.T) {
	stateDir := os.Getenv(signalHelperStateEnv)
	if stateDir == "" {
		t.Skip("subprocess helper")
	}
	server := scriptedServer([]map[string]any{
		{"type": "tool", "tool_call": map[string]any{"name": "list_dir", "args": map[string]any{"path": filepath.Dir(stateDir)}}},
	})
	// Not closed: the script's last call blocks for good, and the helper
	// exits when the run returns.
	workflow := filepath.Join(filepath.Dir(stateDir), "workflow.json")
	backend := inference.NewVLLMBackend(inference.VLLMConfig{BaseURL: server.URL, Model: "scripted", Timeout: time.Hour})
	ctx, stop := terminationContext(context.Background())
	defer stop()
	_ = runWorkflow(ctx, backend, runtime.CompactionPolicy{}, capability.Budget{Seconds: 600}, workflow, nil, "", "", stateDir, "go", false)
}

// SIGTERM during a model call ends the run with recorded exits: the agent
// exits retryably, naming the signal, with a final checkpoint; the call
// gets an outcome line with its error; the root exits and the summary is
// written. Nothing is left running.
func TestSIGTERMRecordsTheExit(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	spec := `{"name":"signalled","entry_agent":"solver","agents":[
		{"id":"solver","system_prompt":"Solve.","max_steps":6,"mounts":[{"name":"workspace","host_path":"` + root + `"}],
		 "capabilities":{"tools":["list_dir"],"read_paths":["` + root + `"]}}]}`
	if err := os.WriteFile(filepath.Join(root, "workflow.json"), []byte(spec), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestSignalHelperProcess$", "-test.count=1")
	cmd.Env = append(os.Environ(), signalHelperStateEnv+"="+stateDir)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	callsPath := runlog.Path(stateDir, runlog.CallsFile)
	deadline := time.Now().Add(30 * time.Second)
	for {
		records, _ := runlog.Read[inference.CallRecord](callsPath)
		if open := inference.InFlight(records); len(records) == 3 && len(open) == 1 {
			break
		}
		if time.Now().After(deadline) {
			cmd.Process.Kill()
			t.Fatalf("the second call never started: %d ledger lines", len(records))
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		cmd.Process.Kill()
		t.Fatal("aos did not stop on SIGTERM")
	}

	k, _ := kernel.New(filepath.Join(stateDir, "kernel"))
	processes, _ := k.Processes()
	if len(processes) != 2 {
		t.Fatalf("processes = %+v", processes)
	}
	for _, process := range processes {
		if process.State != kernel.StateExited {
			t.Fatalf("%s left %s", process.PID, process.State)
		}
	}
	solver := processes[1]
	status := solver.ExitStatus
	if status == nil || status.Code != 1 || !status.Retryable || status.Reason != "terminated by signal (SIGTERM)" || status.Checkpoint == "" {
		t.Fatalf("solver exit = %+v", status)
	}
	store, _ := artifact.New(filepath.Join(stateDir, "artifacts"))
	var session runtime.Session
	if err := store.ReadJSON(status.Checkpoint, &session); err != nil || session.Usage.Steps != 2 {
		t.Fatalf("final checkpoint: %+v %v", session.Usage, err)
	}
	records, _ := runlog.Read[inference.CallRecord](callsPath)
	if len(records) != 4 || inference.InFlight(records) != nil || !strings.Contains(records[3].Error, "terminated by signal (SIGTERM)") {
		t.Fatalf("the cut call should be closed with its error: %+v", records)
	}
	events, _ := k.Events()
	var types []string
	for _, event := range events {
		types = append(types, event.Type)
	}
	if !strings.Contains(strings.Join(types, ","), "run.started") {
		t.Fatalf("events = %v", types)
	}
	index, _ := store.List()
	var summary bool
	for _, record := range index {
		if record.Type == "workflow_summary" {
			raw, _ := os.ReadFile(record.Path)
			var decoded map[string]any
			json.Unmarshal(raw, &decoded)
			summary = strings.Contains(string(raw), "terminated by signal (SIGTERM)")
		}
	}
	if !summary {
		t.Fatal("the summary should record the terminated entry agent")
	}
}
