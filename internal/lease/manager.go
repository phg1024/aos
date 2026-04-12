package lease

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"aos/internal/kernel"
)

var ErrLeaseHeld = errors.New("resource lease already held")
var ErrLeaseOwnerMismatch = errors.New("lease owner mismatch")

type Lease struct {
	Resource  string    `json:"resource"`
	OwnerPID  string    `json:"owner_pid"`
	ExpiresAt time.Time `json:"expires_at"`
}

type Manager struct {
	path string
	mu   sync.Mutex
}

func New(root string) (*Manager, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(root, "leases.json")
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(path, []byte(`{}`), 0o644); err != nil {
			return nil, err
		}
	}
	return &Manager{path: path}, nil
}

func (m *Manager) Acquire(resource, ownerPID string, ttl time.Duration) (Lease, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	leases, err := m.read()
	if err != nil {
		return Lease{}, err
	}

	if existing, ok := leases[resource]; ok && time.Now().UTC().Before(existing.ExpiresAt) {
		return Lease{}, ErrLeaseHeld
	}

	lease := Lease{
		Resource:  resource,
		OwnerPID:  ownerPID,
		ExpiresAt: time.Now().UTC().Add(ttl),
	}
	leases[resource] = lease
	if err := m.write(leases); err != nil {
		return Lease{}, err
	}
	return lease, nil
}

func (m *Manager) AcquireForProcess(k *kernel.Kernel, resource, ownerPID string, ttl time.Duration) (Lease, error) {
	lease, err := m.Acquire(resource, ownerPID, ttl)
	if err == ErrLeaseHeld {
		if _, transitionErr := k.Transition(ownerPID, kernel.StateBlocked); transitionErr != nil && transitionErr != kernel.ErrInvalidTransition {
			return Lease{}, transitionErr
		}
	}
	return lease, err
}

func (m *Manager) Release(resource, ownerPID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	leases, err := m.read()
	if err != nil {
		return err
	}
	existing, ok := leases[resource]
	if !ok {
		return nil
	}
	if existing.OwnerPID != ownerPID {
		return ErrLeaseOwnerMismatch
	}
	delete(leases, resource)
	return m.write(leases)
}

func (m *Manager) ReleaseAndWake(k *kernel.Kernel, resource, ownerPID, wakePID string) error {
	if err := m.Release(resource, ownerPID); err != nil {
		return err
	}
	if wakePID == "" {
		return nil
	}
	_, err := k.Transition(wakePID, kernel.StateReady)
	return err
}

func (m *Manager) Get(resource string) (Lease, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	leases, err := m.read()
	if err != nil {
		return Lease{}, false, err
	}
	lease, ok := leases[resource]
	if !ok {
		return Lease{}, false, nil
	}
	if time.Now().UTC().After(lease.ExpiresAt) {
		delete(leases, resource)
		if err := m.write(leases); err != nil {
			return Lease{}, false, err
		}
		return Lease{}, false, nil
	}
	return lease, true, nil
}

func (m *Manager) read() (map[string]Lease, error) {
	var leases map[string]Lease
	bytes, err := os.ReadFile(m.path)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(bytes, &leases); err != nil {
		return nil, err
	}
	return leases, nil
}

func (m *Manager) write(leases map[string]Lease) error {
	bytes, err := json.MarshalIndent(leases, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(m.path, bytes, 0o644)
}
