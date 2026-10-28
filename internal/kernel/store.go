package kernel

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"aos/internal/filelock"
)

// The kernel's files are written only through fileStore, which only the
// Kernel holds. Write order is the contract:
//
//  1. The event log (events.json) is the source of truth. Every state change
//     appends its event first, with enough data to rebuild the change.
//  2. The process table (processes.json) is derived from the log. Each
//     record names the event that produced it (last_event_id); a table that
//     fell behind, after a crash between the two writes, is repaired by
//     replaying the newer events whenever the store is read.
//  3. kernel_state.json only caches the next IDs; allocation also looks at
//     the log and table, so a stale counter can never reuse an ID.
//
// Every read-modify-write holds an exclusive lock: an in-memory mutex plus a
// filelock on kernel.lock, so several processes may share a state
// directory. Where filelock cannot lock (non-unix), only the mutex applies.

type storeState struct {
	NextProcessID int `json:"next_process_id"`
	NextEventID   int `json:"next_event_id"`
}

type fileStore struct {
	root string
	mu   sync.Mutex
}

func newFileStore(root string) (*fileStore, error) {
	store := &fileStore{root: root}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	if err := store.ensureFiles(); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *fileStore) ensureFiles() error {
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()
	for _, path := range []string{s.statePath(), s.processesPath(), s.eventsPath()} {
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			var contents []byte
			switch path {
			case s.statePath():
				contents = []byte(`{"next_process_id":1,"next_event_id":1}`)
			case s.processesPath():
				contents = []byte(`{}`)
			default:
				contents = []byte(`[]`)
			}
			if err := os.WriteFile(path, contents, 0o644); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *fileStore) statePath() string     { return filepath.Join(s.root, "kernel_state.json") }
func (s *fileStore) processesPath() string { return filepath.Join(s.root, "processes.json") }
func (s *fileStore) eventsPath() string    { return filepath.Join(s.root, "events.json") }
func (s *fileStore) lockPath() string      { return filepath.Join(s.root, "kernel.lock") }

// lock takes the store's exclusive lock for one operation.
func (s *fileStore) lock() (func(), error) {
	s.mu.Lock()
	held, err := filelock.Acquire(s.lockPath())
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	return func() {
		held.Release()
		s.mu.Unlock()
	}, nil
}

// tx is one locked read-modify-write of the kernel files, already repaired
// from the event log.
type tx struct {
	state     storeState
	processes map[string]Process
	events    []Event
	appended  int
	// tableChanged is set when the process table differs from disk,
	// including repairs made while loading.
	tableChanged bool
	nextPID      int
	nextEvent    int
}

// update runs fn in a transaction and writes events before the table.
func (s *fileStore) update(fn func(*tx) error) error {
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()
	t, err := s.load()
	if err != nil {
		return err
	}
	if err := fn(t); err != nil {
		return err
	}
	if t.appended > 0 {
		if err := s.writeJSON(s.eventsPath(), t.events); err != nil {
			return err
		}
	}
	if t.tableChanged {
		if err := s.writeJSON(s.processesPath(), t.processes); err != nil {
			return err
		}
	}
	if t.nextPID != t.state.NextProcessID || t.nextEvent != t.state.NextEventID {
		return s.writeJSON(s.statePath(), storeState{NextProcessID: t.nextPID, NextEventID: t.nextEvent})
	}
	return nil
}

// view runs fn on a repaired snapshot without writing anything.
func (s *fileStore) view(fn func(*tx) error) error {
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()
	t, err := s.load()
	if err != nil {
		return err
	}
	return fn(t)
}

func (s *fileStore) load() (*tx, error) {
	t := &tx{}
	if err := s.readJSON(s.statePath(), &t.state); err != nil {
		return nil, err
	}
	if err := s.readJSON(s.processesPath(), &t.processes); err != nil {
		return nil, err
	}
	if t.processes == nil {
		t.processes = map[string]Process{}
	}
	if err := s.readJSON(s.eventsPath(), &t.events); err != nil {
		return nil, err
	}
	if err := t.replay(); err != nil {
		return nil, err
	}
	t.nextPID, t.nextEvent = t.state.NextProcessID, t.state.NextEventID
	for _, event := range t.events {
		if seq := sequence(event.ID, "evt-"); seq >= t.nextEvent {
			t.nextEvent = seq + 1
		}
	}
	for pid := range t.processes {
		if seq := sequence(pid, "pid-"); seq >= t.nextPID {
			t.nextPID = seq + 1
		}
	}
	return t, nil
}

// replay applies process.* events the table has not caught up with. A
// record without last_event_id predates the write-ahead order (the table
// was written first then), so it is taken as is.
func (t *tx) replay() error {
	for _, event := range t.events {
		if !strings.HasPrefix(event.Type, ReservedEventPrefix) {
			continue
		}
		record, known := t.processes[event.ProcessID]
		if known && (record.LastEventID == "" || sequence(event.ID, "evt-") <= sequence(record.LastEventID, "evt-")) {
			continue
		}
		next, ok, err := applyEvent(record, known, event)
		if err != nil {
			return fmt.Errorf("replay %s: %w", event.ID, err)
		}
		if ok {
			t.processes[event.ProcessID] = next
			t.tableChanged = true
		}
	}
	return nil
}

// applyEvent returns the record after event, and whether the event could be
// applied (events from before the write-ahead order lack the data).
func applyEvent(record Process, known bool, event Event) (Process, bool, error) {
	switch event.Type {
	case "process.spawned":
		encoded, ok := event.Data["process"]
		if !ok {
			return record, false, nil
		}
		var spawned Process
		if err := json.Unmarshal([]byte(encoded), &spawned); err != nil {
			return record, false, err
		}
		record = spawned
	case "process.transition":
		if !known {
			return record, false, nil
		}
		record.State = ProcessState(event.Data["state"])
	case "process.exited":
		if !known {
			return record, false, nil
		}
		code, err := strconv.Atoi(event.Data["code"])
		if err != nil {
			return record, false, err
		}
		record.State = StateExited
		record.ExitStatus = &ExitStatus{Code: code, Reason: event.Data["reason"], Retryable: event.Data["retryable"] == "true", Origin: event.Data["origin"]}
	default:
		return record, false, nil
	}
	record.LastEventID = event.ID
	record.UpdatedAt = event.Timestamp
	return record, true, nil
}

func (t *tx) allocatePID() string {
	pid := fmt.Sprintf("pid-%06d", t.nextPID)
	t.nextPID++
	return pid
}

// appendEvent adds the next event to the log; update writes it before the
// table so the log never trails a state change.
func (t *tx) appendEvent(pid, eventType string, data map[string]string, now time.Time) Event {
	event := Event{
		ID:        fmt.Sprintf("evt-%06d", t.nextEvent),
		ProcessID: pid,
		Type:      eventType,
		Timestamp: now,
		Data:      cloneMap(data),
	}
	t.nextEvent++
	t.events = append(t.events, event)
	t.appended++
	return event
}

func (t *tx) putProcess(process Process) {
	t.processes[process.PID] = process
	t.tableChanged = true
}

func (t *tx) process(pid string) (Process, error) {
	process, ok := t.processes[pid]
	if !ok {
		return Process{}, ErrProcessNotFound
	}
	return process, nil
}

// sequence parses the number in an ID such as "evt-000042"; -1 if absent.
func sequence(id, prefix string) int {
	n, err := strconv.Atoi(strings.TrimPrefix(id, prefix))
	if err != nil {
		return -1
	}
	return n
}

func (s *fileStore) readJSON(path string, target any) error {
	bytes, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(bytes, target)
}

func (s *fileStore) writeJSON(path string, value any) error {
	bytes, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	// Write a sibling temp file and rename it so readers never see a torn file.
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(bytes); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return nil
}
