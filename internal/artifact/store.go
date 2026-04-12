package artifact

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Artifact struct {
	ID          string            `json:"id"`
	Type        string            `json:"type"`
	ProducerPID string            `json:"producer_pid"`
	ParentIDs   []string          `json:"parent_ids,omitempty"`
	Path        string            `json:"path"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
}

type storeState struct {
	NextArtifactID int `json:"next_artifact_id"`
}

type Store struct {
	root string
	mu   sync.Mutex
}

func New(root string) (*Store, error) {
	store := &Store{root: root}
	if err := os.MkdirAll(filepath.Join(root, "payloads"), 0o755); err != nil {
		return nil, err
	}
	for _, path := range []string{store.statePath(), store.indexPath()} {
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			initial := []byte(`{}`)
			if path == store.statePath() {
				initial = []byte(`{"next_artifact_id":1}`)
			}
			if err := os.WriteFile(path, initial, 0o644); err != nil {
				return nil, err
			}
		}
	}
	return store, nil
}

func (s *Store) CreateJSON(artifactType, producerPID string, payload any, parentIDs []string, metadata map[string]string) (Artifact, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	state, err := s.readState()
	if err != nil {
		return Artifact{}, err
	}
	index, err := s.readIndex()
	if err != nil {
		return Artifact{}, err
	}

	id := fmt.Sprintf("art-%06d", state.NextArtifactID)
	state.NextArtifactID++
	payloadPath := filepath.Join(s.root, "payloads", id+".json")
	bytes, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return Artifact{}, err
	}
	if err := os.WriteFile(payloadPath, bytes, 0o644); err != nil {
		return Artifact{}, err
	}

	artifact := Artifact{
		ID:          id,
		Type:        artifactType,
		ProducerPID: producerPID,
		ParentIDs:   append([]string(nil), parentIDs...),
		Path:        payloadPath,
		Metadata:    clone(metadata),
		CreatedAt:   time.Now().UTC(),
	}
	index[id] = artifact
	if err := s.writeJSON(s.statePath(), state); err != nil {
		return Artifact{}, err
	}
	if err := s.writeJSON(s.indexPath(), index); err != nil {
		return Artifact{}, err
	}
	return artifact, nil
}

func (s *Store) Metadata(id string) (Artifact, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

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

func (s *Store) ReadJSON(id string, target any) error {
	s.mu.Lock()
	defer s.mu.Unlock()

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
	return json.Unmarshal(bytes, target)
}

func (s *Store) List() ([]Artifact, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

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

func (s *Store) readState() (storeState, error) {
	var state storeState
	err := s.readJSON(s.statePath(), &state)
	return state, err
}

func (s *Store) readIndex() (map[string]Artifact, error) {
	var index map[string]Artifact
	err := s.readJSON(s.indexPath(), &index)
	return index, err
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
	return os.WriteFile(path, bytes, 0o644)
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
