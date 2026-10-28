package kernel

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// crashAfterEvent appends event straight to the log file, as the first of a
// transaction's writes, and stops there, as if the process died before
// writing the table. It deliberately bypasses update, which would replay
// and repair.
func crashAfterEvent(t *testing.T, root, pid, eventType string, data map[string]string) Event {
	t.Helper()
	store := &fileStore{root: root}
	var events []Event
	if err := store.readJSON(store.eventsPath(), &events); err != nil {
		t.Fatalf("read log: %v", err)
	}
	next := 1
	if len(events) > 0 {
		next = sequence(events[len(events)-1].ID, "evt-") + 1
	}
	event := Event{ID: fmt.Sprintf("evt-%06d", next), ProcessID: pid, Type: eventType, Timestamp: time.Now().UTC(), Data: data}
	if err := store.writeJSON(store.eventsPath(), append(events, event)); err != nil {
		t.Fatalf("write log: %v", err)
	}
	return event
}

func tableOnDisk(t *testing.T, root string) map[string]Process {
	t.Helper()
	var processes map[string]Process
	raw, err := os.ReadFile(filepath.Join(root, "processes.json"))
	if err != nil {
		t.Fatalf("read table: %v", err)
	}
	if err := json.Unmarshal(raw, &processes); err != nil {
		t.Fatalf("parse table: %v", err)
	}
	return processes
}

func TestNewRepairsTableFromEventsAfterCrash(t *testing.T) {
	root := t.TempDir()
	k, err := New(root)
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	running, err := k.Spawn(ProcessSpec{Role: "worker"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	exiting, err := k.Spawn(ProcessSpec{Role: "worker"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}

	// Spawn: the event exists, the record does not.
	lost := Process{PID: "pid-000099", Role: "worker", State: StateReady, CreatedAt: time.Now().UTC()}
	encoded, _ := json.Marshal(lost)
	spawned := crashAfterEvent(t, root, lost.PID, "process.spawned", map[string]string{"role": "worker", "process": string(encoded)})
	// Transition: the record still says ready.
	transitioned := crashAfterEvent(t, root, running.PID, "process.transition", map[string]string{"state": string(StateRunning)})
	// Exit: the record has no exit status.
	exited := crashAfterEvent(t, root, exiting.PID, "process.exited", map[string]string{"code": "3", "reason": "tests failed", "retryable": "true"})

	if table := tableOnDisk(t, root); table[lost.PID].PID != "" || table[running.PID].State != StateReady || table[exiting.PID].ExitStatus != nil {
		t.Fatalf("the simulated crash should leave the table behind the log: %+v", table)
	}

	repaired, err := New(root)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	table := tableOnDisk(t, root)
	if got := table[lost.PID]; got.Role != "worker" || got.State != StateReady || got.LastEventID != spawned.ID {
		t.Fatalf("spawned record not rebuilt: %+v", got)
	}
	if got := table[running.PID]; got.State != StateRunning || got.LastEventID != transitioned.ID {
		t.Fatalf("transition not replayed: %+v", got)
	}
	status, err := repaired.Await(exiting.PID)
	if err != nil || status != (ExitStatus{Code: 3, Reason: "tests failed", Retryable: true}) {
		t.Fatalf("exit not replayed: %+v %v", status, err)
	}
	if got := table[exiting.PID]; got.LastEventID != exited.ID {
		t.Fatalf("repaired record should name its event: %+v", got)
	}

	// A live instance opened before the repair sees the same view.
	if got, err := k.Get(running.PID); err != nil || got.State != StateRunning {
		t.Fatalf("older instance should read the repaired state: %+v %v", got, err)
	}
}

// A stale counter file (crash before it was written) must not lead to
// reused process or event IDs.
func TestAllocationIgnoresStaleCounters(t *testing.T) {
	root := t.TempDir()
	k, err := New(root)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	first, err := k.Spawn(ProcessSpec{Role: "worker"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "kernel_state.json"), []byte(`{"next_process_id":1,"next_event_id":1}`), 0o644); err != nil {
		t.Fatalf("reset counters: %v", err)
	}
	second, err := k.Spawn(ProcessSpec{Role: "worker"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	events, err := k.Events()
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if second.PID == first.PID || len(events) != 2 || events[0].ID == events[1].ID {
		t.Fatalf("IDs reused after a stale counter: %s %s %+v", first.PID, second.PID, events)
	}
}

// A state directory written before the write-ahead order (records without
// last_event_id, spawn events without the record) still loads unchanged.
func TestLegacyStateDirectoryLoads(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"kernel_state.json": `{"next_process_id":3,"next_event_id":4}`,
		"processes.json": `{"pid-000001":{"pid":"pid-000001","role":"agent:coordinator","state":"exited","capability_set":{"allow_network":false},"created_at":"2026-10-01T00:00:00Z","updated_at":"2026-10-01T00:01:00Z","exit_status":{"code":0,"reason":"done","retryable":false}},` +
			`"pid-000002":{"pid":"pid-000002","role":"agent:writer","state":"ready","capability_set":{"allow_network":false},"created_at":"2026-10-01T00:00:00Z","updated_at":"2026-10-01T00:00:00Z"}}`,
		"events.json": `[{"id":"evt-000001","process_id":"pid-000001","type":"process.spawned","timestamp":"2026-10-01T00:00:00Z","data":{"role":"agent:coordinator"}},` +
			`{"id":"evt-000002","process_id":"pid-000002","type":"process.spawned","timestamp":"2026-10-01T00:00:00Z","data":{"role":"agent:writer"}},` +
			`{"id":"evt-000003","process_id":"pid-000001","type":"process.exited","timestamp":"2026-10-01T00:01:00Z","data":{"code":"0","reason":"done","retryable":"false"}}]`,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	k, err := New(root)
	if err != nil {
		t.Fatalf("legacy dir should load: %v", err)
	}
	if status, err := k.Await("pid-000001"); err != nil || status.Reason != "done" {
		t.Fatalf("legacy exit: %+v %v", status, err)
	}
	if _, err := k.Transition("pid-000002", StateRunning); err != nil {
		t.Fatalf("legacy process should keep working: %v", err)
	}
	next, err := k.Spawn(ProcessSpec{Role: "worker"})
	if err != nil || next.PID != "pid-000003" {
		t.Fatalf("new spawn after legacy state: %+v %v", next, err)
	}
	events, _ := k.Events()
	if last := events[len(events)-1].ID; last != "evt-000005" {
		t.Fatalf("event IDs should continue the legacy sequence, last is %s", last)
	}
}

// Origin is part of the exit record: it survives replay, and an exit cannot
// be repeated with a different origin.
func TestExitOriginIsRecordedAndReplayed(t *testing.T) {
	root := t.TempDir()
	k, err := New(root)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	process, err := k.Spawn(ProcessSpec{Role: "verifier"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	status := ExitStatus{Code: 1, Reason: "FAIL", Retryable: true, Origin: ExitOriginAgent}
	if _, err := k.Exit(process.PID, status); err != nil {
		t.Fatalf("exit: %v", err)
	}
	if _, err := k.Exit(process.PID, status); err != nil {
		t.Fatalf("repeating the same exit is a no-op: %v", err)
	}
	if _, err := k.Exit(process.PID, ExitStatus{Code: 1, Reason: "FAIL", Retryable: true}); err == nil {
		t.Fatal("an exit differing only in origin must be refused")
	}

	other, err := k.Spawn(ProcessSpec{Role: "verifier"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	crashAfterEvent(t, root, other.PID, "process.exited", map[string]string{"code": "1", "reason": "FAIL", "retryable": "false", "origin": ExitOriginAgent})
	reopened, err := New(root)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got, err := reopened.Await(other.PID); err != nil || got.Origin != ExitOriginAgent {
		t.Fatalf("replayed exit lost its origin: %+v %v", got, err)
	}
}
