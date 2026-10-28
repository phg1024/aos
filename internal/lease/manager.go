// Package lease grants time-limited exclusive leases on named resources to
// kernel processes. Leases live in leases.json, read and written under a
// file lock and replaced atomically, so managers in separate processes
// sharing a directory never both win a resource. A lease is held until it
// expires, its owner releases it, or its owner exits: with a kernel bound,
// a lease whose owner has exited is free.
//
// Every grant carries a fencing token, strictly increasing per resource
// across all managers, so a write site can reject a holder whose lease was
// since taken over. With a kernel bound, grants, renewals, releases,
// takeovers and transfers are logged as lease.* kernel events, each before
// the lease file changes.
package lease

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"aos/internal/filelock"
	"aos/internal/kernel"
)

var ErrLeaseHeld = errors.New("resource lease already held")
var ErrLeaseOwnerMismatch = errors.New("lease owner mismatch")

type Lease struct {
	Resource  string    `json:"resource"`
	OwnerPID  string    `json:"owner_pid"`
	ExpiresAt time.Time `json:"expires_at"`
	// Token is the fencing token of this grant. A renewal keeps it; any new
	// grant of the resource, including a takeover or transfer, gets a
	// larger one.
	Token int64 `json:"token"`
}

type Manager struct {
	path   string
	kernel *kernel.Kernel
	mu     sync.Mutex
}

// state is leases.json. Files without a version are the earlier format: a
// bare map of resource to lease, with no tokens.
type state struct {
	Version int              `json:"version"`
	Leases  map[string]Lease `json:"leases"`
	// Tokens is the last token issued per resource. It outlives the lease,
	// so a token is never issued twice.
	Tokens map[string]int64 `json:"tokens"`
}

// New opens the leases under root. k, if not nil, is the kernel whose
// processes own the leases: it decides whether an owner has exited and
// receives the lease.* events.
func New(root string, k *kernel.Kernel) (*Manager, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return &Manager{path: filepath.Join(root, "leases.json"), kernel: k}, nil
}

// Acquire grants resource to ownerPID for ttl. The owner of a live lease
// renews it: the expiry moves and the token stays. A lease that expired or
// whose owner exited is taken over. Any other live lease is ErrLeaseHeld.
func (m *Manager) Acquire(resource, ownerPID string, ttl time.Duration) (Lease, error) {
	var granted Lease
	err := m.update(func(s *state, now time.Time) error {
		existing, ok := s.Leases[resource]
		eventType := "lease.acquired"
		data := map[string]string{"resource": resource}
		if ok {
			free, why, err := m.free(existing, now)
			if err != nil {
				return err
			}
			switch {
			case !free && existing.OwnerPID == ownerPID:
				existing.ExpiresAt = now.Add(ttl)
				granted = existing
				data["token"] = strconv.FormatInt(granted.Token, 10)
				data["expires_at"] = granted.ExpiresAt.Format(time.RFC3339Nano)
				if err := m.record(ownerPID, "lease.renewed", data); err != nil {
					return err
				}
				s.Leases[resource] = granted
				return nil
			case !free:
				return ErrLeaseHeld
			default:
				eventType = "lease.taken_over"
				data["previous_owner"] = existing.OwnerPID
				data["previous_token"] = strconv.FormatInt(existing.Token, 10)
				data["reason"] = why
			}
		}
		granted = Lease{Resource: resource, OwnerPID: ownerPID, ExpiresAt: now.Add(ttl), Token: s.Tokens[resource] + 1}
		data["token"] = strconv.FormatInt(granted.Token, 10)
		data["expires_at"] = granted.ExpiresAt.Format(time.RFC3339Nano)
		if err := m.record(ownerPID, eventType, data); err != nil {
			return err
		}
		s.Leases[resource] = granted
		s.Tokens[resource] = granted.Token
		return nil
	})
	if err != nil {
		return Lease{}, err
	}
	return granted, nil
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

// Release drops ownerPID's lease on resource. Releasing a lease that does
// not exist is a no-op; releasing someone else's is ErrLeaseOwnerMismatch.
func (m *Manager) Release(resource, ownerPID string) error {
	return m.update(func(s *state, _ time.Time) error {
		existing, ok := s.Leases[resource]
		if !ok {
			return nil
		}
		if existing.OwnerPID != ownerPID {
			return ErrLeaseOwnerMismatch
		}
		if err := m.record(ownerPID, "lease.released", map[string]string{
			"resource": resource,
			"token":    strconv.FormatInt(existing.Token, 10),
		}); err != nil {
			return err
		}
		delete(s.Leases, resource)
		return nil
	})
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

// Transfer moves fromPID's unexpired leases to toPID, as Recover does for
// the process it replaces, each with a new token. Leases another process
// already took over are not touched.
func (m *Manager) Transfer(fromPID, toPID string) ([]Lease, error) {
	var moved []Lease
	err := m.update(func(s *state, now time.Time) error {
		for resource, existing := range s.Leases {
			if existing.OwnerPID != fromPID || !now.Before(existing.ExpiresAt) {
				continue
			}
			next := Lease{Resource: resource, OwnerPID: toPID, ExpiresAt: existing.ExpiresAt, Token: s.Tokens[resource] + 1}
			if err := m.record(toPID, "lease.transferred", map[string]string{
				"resource":       resource,
				"previous_owner": fromPID,
				"previous_token": strconv.FormatInt(existing.Token, 10),
				"token":          strconv.FormatInt(next.Token, 10),
				"expires_at":     next.ExpiresAt.Format(time.RFC3339Nano),
			}); err != nil {
				return err
			}
			s.Leases[resource] = next
			s.Tokens[resource] = next.Token
			moved = append(moved, next)
		}
		return nil
	})
	return moved, err
}

// Get returns the live lease on resource, if any. A lease that expired or
// whose owner exited is reported as not held.
func (m *Manager) Get(resource string) (Lease, bool, error) {
	var lease Lease
	var held bool
	err := m.view(func(s *state, now time.Time) error {
		existing, ok := s.Leases[resource]
		if !ok {
			return nil
		}
		free, _, err := m.free(existing, now)
		if err != nil {
			return err
		}
		lease, held = existing, !free
		return nil
	})
	if err != nil || !held {
		return Lease{}, false, err
	}
	return lease, true, nil
}

// List returns the live leases, by resource.
func (m *Manager) List() ([]Lease, error) {
	var live []Lease
	err := m.view(func(s *state, now time.Time) error {
		for _, existing := range s.Leases {
			free, _, err := m.free(existing, now)
			if err != nil {
				return err
			}
			if !free {
				live = append(live, existing)
			}
		}
		return nil
	})
	sort.Slice(live, func(i, j int) bool { return live[i].Resource < live[j].Resource })
	return live, err
}

// free reports whether a recorded lease no longer binds, and why.
func (m *Manager) free(lease Lease, now time.Time) (bool, string, error) {
	if !now.Before(lease.ExpiresAt) {
		return true, "expired", nil
	}
	if m.kernel == nil {
		return false, "", nil
	}
	owner, err := m.kernel.Get(lease.OwnerPID)
	if errors.Is(err, kernel.ErrProcessNotFound) {
		// Not a process this kernel knows; only expiry frees it.
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	if owner.State == kernel.StateExited {
		return true, "owner_exited", nil
	}
	return false, "", nil
}

func (m *Manager) record(pid, eventType string, data map[string]string) error {
	if m.kernel == nil {
		return nil
	}
	_, err := m.kernel.RecordEvent(pid, eventType, data)
	return err
}

// update runs fn on the leases under the lock and writes them back if fn
// succeeds. The kernel's lock may be taken inside (never the reverse).
func (m *Manager) update(fn func(*state, time.Time) error) error {
	return m.locked(func() error {
		s, err := m.read()
		if err != nil {
			return err
		}
		if err := fn(&s, time.Now().UTC()); err != nil {
			return err
		}
		return m.write(s)
	})
}

func (m *Manager) view(fn func(*state, time.Time) error) error {
	return m.locked(func() error {
		s, err := m.read()
		if err != nil {
			return err
		}
		return fn(&s, time.Now().UTC())
	})
}

func (m *Manager) locked(fn func() error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	held, err := filelock.Acquire(m.path + ".lock")
	if err != nil {
		return err
	}
	defer held.Release()
	return fn()
}

func (m *Manager) read() (state, error) {
	s := state{Version: 2, Leases: map[string]Lease{}, Tokens: map[string]int64{}}
	bytes, err := os.ReadFile(m.path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(bytes, &probe); err != nil {
		return s, err
	}
	if _, versioned := probe["version"]; !versioned {
		return s, json.Unmarshal(bytes, &s.Leases)
	}
	if err := json.Unmarshal(bytes, &s); err != nil {
		return s, err
	}
	if s.Leases == nil {
		s.Leases = map[string]Lease{}
	}
	if s.Tokens == nil {
		s.Tokens = map[string]int64{}
	}
	s.Version = 2
	return s, nil
}

// write replaces leases.json through a synced temp file, so a reader or a
// crash never sees a partial file.
func (m *Manager) write(s state) error {
	bytes, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(m.path), ".leases.json.tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	_, err = tmp.Write(bytes)
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Chmod(tmpPath, 0o644)
	}
	if err == nil {
		err = os.Rename(tmpPath, m.path)
	}
	if err != nil {
		os.Remove(tmpPath)
	}
	return err
}
