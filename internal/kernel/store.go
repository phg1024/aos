package kernel

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

type storeState struct {
	NextProcessID int `json:"next_process_id"`
	NextEventID   int `json:"next_event_id"`
}

type FileStore struct {
	root string
	mu   sync.Mutex
}

func NewFileStore(root string) (*FileStore, error) {
	store := &FileStore{root: root}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	if err := store.ensureFiles(); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *FileStore) ensureFiles() error {
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

func (s *FileStore) statePath() string     { return filepath.Join(s.root, "kernel_state.json") }
func (s *FileStore) processesPath() string { return filepath.Join(s.root, "processes.json") }
func (s *FileStore) eventsPath() string    { return filepath.Join(s.root, "events.json") }

func (s *FileStore) AllocatePID() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	state, err := s.readState()
	if err != nil {
		return "", err
	}
	pid := fmt.Sprintf("pid-%06d", state.NextProcessID)
	state.NextProcessID++
	if err := s.writeJSON(s.statePath(), state); err != nil {
		return "", err
	}
	return pid, nil
}

func (s *FileStore) AllocateEventID() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	state, err := s.readState()
	if err != nil {
		return "", err
	}
	id := fmt.Sprintf("evt-%06d", state.NextEventID)
	state.NextEventID++
	if err := s.writeJSON(s.statePath(), state); err != nil {
		return "", err
	}
	return id, nil
}

func (s *FileStore) SaveProcess(process Process) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	processes, err := s.readProcesses()
	if err != nil {
		return err
	}
	processes[process.PID] = process
	return s.writeJSON(s.processesPath(), processes)
}

func (s *FileStore) LoadProcess(pid string) (Process, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	processes, err := s.readProcesses()
	if err != nil {
		return Process{}, err
	}
	process, ok := processes[pid]
	if !ok {
		return Process{}, ErrProcessNotFound
	}
	return process, nil
}

func (s *FileStore) ListProcesses() ([]Process, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	processes, err := s.readProcesses()
	if err != nil {
		return nil, err
	}
	out := make([]Process, 0, len(processes))
	for _, process := range processes {
		out = append(out, process)
	}
	return out, nil
}

func (s *FileStore) AppendEvent(event Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	events, err := s.readEvents()
	if err != nil {
		return err
	}
	events = append(events, event)
	return s.writeJSON(s.eventsPath(), events)
}

func (s *FileStore) ListEvents() ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readEvents()
}

func (s *FileStore) readState() (storeState, error) {
	var state storeState
	err := s.readJSON(s.statePath(), &state)
	return state, err
}

func (s *FileStore) readProcesses() (map[string]Process, error) {
	var processes map[string]Process
	err := s.readJSON(s.processesPath(), &processes)
	return processes, err
}

func (s *FileStore) readEvents() ([]Event, error) {
	var events []Event
	err := s.readJSON(s.eventsPath(), &events)
	return events, err
}

func (s *FileStore) readJSON(path string, target any) error {
	bytes, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(bytes, target)
}

func (s *FileStore) writeJSON(path string, value any) error {
	bytes, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, bytes, 0o644)
}
