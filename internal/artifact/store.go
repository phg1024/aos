package artifact

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"aos/internal/filelock"
)

// The store's contract:
//   - Payloads are content-addressed: the index records each payload's
//     sha256 and every read verifies it, so a payload changed on disk is
//     detected (ErrArtifactTampered). Payload files are written read-only.
//   - Payloads and index.json are written via temp file plus rename, so a
//     crash never leaves a torn file, and every operation holds an
//     in-memory mutex plus a filelock on artifacts.lock, so several
//     processes may share a store.
//   - Lineage is checked: parents must exist, and once a producer check is
//     bound (BindProducers) the producer must be a known process.
//
// Compatibility: index entries written before hashing have no sha256 and
// are read without verification.

var (
	// ErrArtifactTampered: a payload no longer matches its recorded hash.
	ErrArtifactTampered = errors.New("artifact payload does not match its recorded sha256")
	// ErrUnknownParent: a parent ID is not in the store.
	ErrUnknownParent = errors.New("unknown parent artifact")
	// ErrUnknownProducer: the bound producer check does not know the PID.
	ErrUnknownProducer = errors.New("unknown producer process")
	// ErrReservedType: agents may not publish artifacts of a reserved type.
	ErrReservedType = errors.New("artifact type is reserved for the runtime")
)

// reservedTypes are produced only by the runtime, engine and contract
// harness. Agents publish under PublishedPrefix instead.
var reservedTypes = map[string]bool{
	"checkpoint":           true,
	"tool_result":          true,
	"tool_error":           true,
	"backend_error":        true,
	"mailbox_message":      true,
	"mailbox_publish":      true,
	"mailbox_subscription": true,
	"delegate_result":      true,
	"agent_run":            true,
	"workflow_summary":     true,
	"verification":         true,
	"memory_segment":       true,
	"task_plan":            true,
	"worker_output":        true,
}

// PublishedPrefix starts the type of every artifact an agent publishes.
const PublishedPrefix = "published."

// IsReserved reports whether artifactType belongs to the runtime.
func IsReserved(artifactType string) bool {
	return reservedTypes[artifactType]
}

type Artifact struct {
	ID          string            `json:"id"`
	Type        string            `json:"type"`
	ProducerPID string            `json:"producer_pid"`
	ParentIDs   []string          `json:"parent_ids,omitempty"`
	Path        string            `json:"path"`
	SHA256      string            `json:"sha256,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
}

type storeState struct {
	NextArtifactID int `json:"next_artifact_id"`
}

type Store struct {
	root      string
	mu        sync.Mutex
	producers atomic.Pointer[func(pid string) error]
}

func New(root string) (*Store, error) {
	store := &Store{root: root}
	if err := os.MkdirAll(filepath.Join(root, "payloads"), 0o755); err != nil {
		return nil, err
	}
	unlock, err := store.lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	for _, path := range []string{store.statePath(), store.indexPath()} {
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			initial := []byte(`{}`)
			if path == store.statePath() {
				initial = []byte(`{"next_artifact_id":1}`)
			}
			if err := writeFileAtomic(path, initial, 0o644); err != nil {
				return nil, err
			}
		}
	}
	return store, nil
}

// BindProducers installs the check every later CreateJSON applies to its
// producer PID. The first binding wins; callers holding the kernel bind it
// so artifacts cannot name a process the kernel does not know.
func (s *Store) BindProducers(check func(pid string) error) {
	s.producers.CompareAndSwap(nil, &check)
}

func (s *Store) CreateJSON(artifactType, producerPID string, payload any, parentIDs []string, metadata map[string]string) (Artifact, error) {
	if check := s.producers.Load(); check != nil {
		if err := (*check)(producerPID); err != nil {
			return Artifact{}, fmt.Errorf("%w: %q: %v", ErrUnknownProducer, producerPID, err)
		}
	}
	bytes, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return Artifact{}, err
	}

	unlock, err := s.lock()
	if err != nil {
		return Artifact{}, err
	}
	defer unlock()
	state, err := s.readState()
	if err != nil {
		return Artifact{}, err
	}
	index, err := s.readIndex()
	if err != nil {
		return Artifact{}, err
	}
	for _, parent := range parentIDs {
		if _, ok := index[parent]; !ok {
			return Artifact{}, fmt.Errorf("%w: %q", ErrUnknownParent, parent)
		}
	}

	next := state.NextArtifactID
	for id := range index {
		if seq := sequence(id); seq >= next {
			next = seq + 1
		}
	}
	id := fmt.Sprintf("art-%06d", next)
	payloadPath := filepath.Join(s.root, "payloads", id+".json")
	if err := writeFileAtomic(payloadPath, bytes, 0o444); err != nil {
		return Artifact{}, err
	}
	sum := sha256.Sum256(bytes)
	artifact := Artifact{
		ID:          id,
		Type:        artifactType,
		ProducerPID: producerPID,
		ParentIDs:   append([]string(nil), parentIDs...),
		Path:        payloadPath,
		SHA256:      hex.EncodeToString(sum[:]),
		Metadata:    clone(metadata),
		CreatedAt:   time.Now().UTC(),
	}
	index[id] = artifact
	// Index before counter: a crash in between only leaves a counter that
	// allocation already corrects from the index.
	if err := s.writeJSON(s.indexPath(), index); err != nil {
		return Artifact{}, err
	}
	if err := s.writeJSON(s.statePath(), storeState{NextArtifactID: next + 1}); err != nil {
		return Artifact{}, err
	}
	return artifact, nil
}

func (s *Store) Metadata(id string) (Artifact, error) {
	unlock, err := s.lock()
	if err != nil {
		return Artifact{}, err
	}
	defer unlock()
	index, err := s.readIndex()
	if err != nil {
		return Artifact{}, err
	}
	artifact, ok := index[id]
	if !ok {
		return Artifact{}, os.ErrNotExist
	}
	return artifact, nil
}

// ReadJSON decodes an artifact's payload after verifying its hash.
func (s *Store) ReadJSON(id string, target any) error {
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()
	index, err := s.readIndex()
	if err != nil {
		return err
	}
	artifact, ok := index[id]
	if !ok {
		return os.ErrNotExist
	}
	bytes, err := os.ReadFile(artifact.Path)
	if err != nil {
		return err
	}
	if artifact.SHA256 != "" {
		sum := sha256.Sum256(bytes)
		if hex.EncodeToString(sum[:]) != artifact.SHA256 {
			return fmt.Errorf("%w: %s", ErrArtifactTampered, id)
		}
	}
	return json.Unmarshal(bytes, target)
}

func (s *Store) List() ([]Artifact, error) {
	unlock, err := s.lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	index, err := s.readIndex()
	if err != nil {
		return nil, err
	}
	out := make([]Artifact, 0, len(index))
	for _, artifact := range index {
		out = append(out, artifact)
	}
	return out, nil
}

func (s *Store) statePath() string { return filepath.Join(s.root, "state.json") }
func (s *Store) indexPath() string { return filepath.Join(s.root, "index.json") }
func (s *Store) lockPath() string  { return filepath.Join(s.root, "artifacts.lock") }

func (s *Store) lock() (func(), error) {
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

func (s *Store) readState() (storeState, error) {
	var state storeState
	err := s.readJSON(s.statePath(), &state)
	return state, err
}

func (s *Store) readIndex() (map[string]Artifact, error) {
	var index map[string]Artifact
	if err := s.readJSON(s.indexPath(), &index); err != nil {
		return nil, err
	}
	if index == nil {
		index = map[string]Artifact{}
	}
	return index, nil
}

func (s *Store) readJSON(path string, target any) error {
	bytes, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(bytes, target)
}

func (s *Store) writeJSON(path string, value any) error {
	bytes, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, bytes, 0o644)
}

// writeFileAtomic writes a sibling temp file and renames it over path, so
// readers never see a torn file.
func writeFileAtomic(path string, bytes []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(bytes); err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Chmod(tmpPath, mode)
	}
	if err == nil {
		err = os.Rename(tmpPath, path)
	}
	if err != nil {
		os.Remove(tmpPath)
	}
	return err
}

// sequence parses the number in an ID such as "art-000042"; -1 if absent.
func sequence(id string) int {
	n, err := strconv.Atoi(strings.TrimPrefix(id, "art-"))
	if err != nil {
		return -1
	}
	return n
}

func clone(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
