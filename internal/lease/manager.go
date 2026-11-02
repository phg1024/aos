// Package lease grants time-limited exclusive leases on named resources to
// kernel processes. Leases live in leases.json, read and written under a
// file lock and replaced atomically, so managers in separate processes
// sharing a directory never both win a resource. A lease is held until it
// expires, its owner releases it, or its owner exits: with a kernel bound,
// a lease whose owner has exited is free.
//
// Every grant carries a fencing token, strictly increasing per resource
// across all managers, so a write site can reject a holder whose lease was
// since taken over. Guard is that check, run under the lease lock with the
// write itself. With a kernel bound, grants, renewals, releases,
// takeovers and transfers are logged as lease.* kernel events, each before
// the lease file changes.
package lease

import (
	"encoding/json"
	"errors"
	"fmt"
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

// ErrLeaseFenced: a write was refused by a lease on its target. The error
// is a *FenceError saying which lease and why.
var ErrLeaseFenced = errors.New("write fenced by a lease")

// Why Guard refused a write.
const (
	// FenceHeldByOther: another process holds a live lease on the target.
	FenceHeldByOther = "held_by_other"
	// FenceStaleToken: the writer held a lease on the target, and the
	// resource has since been granted again, so its token is not current.
	FenceStaleToken = "stale_token"
)

// FenceError is a write Guard refused. It names no time, so the same
// refusal reads the same whenever it happens.
type FenceError struct {
	Resource string
	Reason   string
	// Holder holds the resource now, if anyone does; Token is the current
	// token. HeldToken is the writer's own, stale token.
	Holder    string
	Token     int64
	HeldToken int64
}

func (e *FenceError) Error() string {
	if e.Reason == FenceStaleToken {
		current := "no one holds it now"
		if e.Holder != "" {
			current = "held by " + e.Holder
		}
		return fmt.Sprintf("%s: your token %d for %q is stale; it was granted again with token %d (%s); acquire_lease again before writing", ErrLeaseFenced, e.HeldToken, e.Resource, e.Token, current)
	}
	return fmt.Sprintf("%s: %q, which covers this path, is held by %s (token %d); wait for its release or acquire it when free", ErrLeaseFenced, e.Resource, e.Holder, e.Token)
}

func (e *FenceError) Is(target error) bool { return target == ErrLeaseFenced }

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
	// Holders is, per resource, the last token each owner was granted. It
	// outlives the lease, so Guard can tell a former holder whose token
	// went stale. Files written before it have none.
	Holders map[string]map[string]int64 `json:"holders,omitempty"`
}

// granted records that owner holds resource with token.
func (s *state) granted(resource, owner string, token int64) {
	if s.Holders == nil {
		s.Holders = map[string]map[string]int64{}
	}
	if s.Holders[resource] == nil {
		s.Holders[resource] = map[string]int64{}
	}
	s.Holders[resource][owner] = token
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
// With a kernel bound, the decision and the new expiry come from one
// reading of the kernel's clock, the timestamp of the lease event itself,
// so a replay, which reproduces event times, decides the same.
func (m *Manager) Acquire(resource, ownerPID string, ttl time.Duration) (Lease, error) {
	var granted Lease
	err := m.update(func(s *state, _ time.Time) error {
		return m.decide(ownerPID, func(now time.Time, exited func(string) (bool, error)) (string, map[string]string, error) {
			existing, ok := s.Leases[resource]
			eventType := "lease.acquired"
			data := map[string]string{"resource": resource}
			if ok {
				free, why, err := freeAt(existing, now, exited)
				if err != nil {
					return "", nil, err
				}
				switch {
				case !free && existing.OwnerPID == ownerPID:
					existing.ExpiresAt = now.Add(ttl)
					granted = existing
					data["token"] = strconv.FormatInt(granted.Token, 10)
					data["expires_at"] = granted.ExpiresAt.Format(time.RFC3339Nano)
					s.Leases[resource] = granted
					s.granted(resource, ownerPID, granted.Token)
					return "lease.renewed", data, nil
				case !free:
					return "", nil, ErrLeaseHeld
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
			s.Leases[resource] = granted
			s.Tokens[resource] = granted.Token
			s.granted(resource, ownerPID, granted.Token)
			return eventType, data, nil
		})
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
			s.granted(resource, toPID, next.Token)
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

// Guard runs write under the lease lock if writers may write the target,
// so no grant, takeover or transfer can come between the check and the
// write. writers is the writing process and the processes it was recovered
// from; covers reports whether a resource's lease covers the target. Every
// covering resource is checked, in name order, and the first refusal is
// returned as a *FenceError without calling write:
//
//   - a live lease held by a process not among writers (FenceHeldByOther);
//   - a lease a writer was granted whose token is no longer the resource's
//     current one (FenceStaleToken). A writer whose own lease lapsed with
//     no one granted it since still has the current token.
//
// write's error is returned as is.
func (m *Manager) Guard(writers []string, covers func(resource string) bool, write func() error) error {
	return m.locked(func() error {
		s, err := m.read()
		if err != nil {
			return err
		}
		if err := m.fence(s, m.now(), writers, covers); err != nil {
			return err
		}
		return write()
	})
}

func (m *Manager) fence(s state, now time.Time, writers []string, covers func(string) bool) error {
	resources := make([]string, 0, len(s.Tokens))
	for resource := range s.Tokens {
		resources = append(resources, resource)
	}
	for resource := range s.Leases {
		if _, counted := s.Tokens[resource]; !counted {
			resources = append(resources, resource)
		}
	}
	sort.Strings(resources)
	isWriter := func(pid string) bool {
		for _, writer := range writers {
			if writer == pid {
				return true
			}
		}
		return false
	}
	for _, resource := range resources {
		if !covers(resource) {
			continue
		}
		current := s.Tokens[resource]
		holder := ""
		existing, ok := s.Leases[resource]
		if ok {
			free, _, err := m.free(existing, now)
			if err != nil {
				return err
			}
			if !free {
				holder = existing.OwnerPID
			}
			if existing.Token > current {
				current = existing.Token
			}
		}
		if holder != "" && !isWriter(holder) {
			return &FenceError{Resource: resource, Reason: FenceHeldByOther, Holder: holder, Token: current}
		}
		var held int64
		for _, writer := range writers {
			held = max(held, s.Holders[resource][writer])
		}
		if ok && isWriter(existing.OwnerPID) {
			held = max(held, existing.Token)
		}
		if held > 0 && held != current {
			return &FenceError{Resource: resource, Reason: FenceStaleToken, Holder: holder, Token: current, HeldToken: held}
		}
	}
	return nil
}

// free reports whether a recorded lease no longer binds, and why.
func (m *Manager) free(lease Lease, now time.Time) (bool, string, error) {
	return freeAt(lease, now, func(pid string) (bool, error) {
		if m.kernel == nil {
			return false, nil
		}
		return ownerExited(m.kernel.Get(pid))
	})
}

// freeAt is free with exited telling whether an owner has exited.
func freeAt(lease Lease, now time.Time, exited func(pid string) (bool, error)) (bool, string, error) {
	if !now.Before(lease.ExpiresAt) {
		return true, "expired", nil
	}
	gone, err := exited(lease.OwnerPID)
	if err != nil {
		return false, "", err
	}
	if gone {
		return true, "owner_exited", nil
	}
	return false, "", nil
}

// ownerExited reads a kernel lookup of a lease's owner. A process the
// kernel does not know never exits; only expiry frees its lease.
func ownerExited(owner kernel.Process, err error) (bool, error) {
	if errors.Is(err, kernel.ErrProcessNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return owner.State == kernel.StateExited, nil
}

// now is the lease clock for decisions that record no event: the kernel's
// clock without advancing it, or the wall clock with no kernel bound.
func (m *Manager) now() time.Time {
	if m.kernel == nil {
		return time.Now().UTC()
	}
	return m.kernel.Now()
}

// decide makes a decision that records a lease event at one reading of
// the clock: through the kernel, which stamps the event with that
// reading, or on the wall clock with no kernel bound, recording nothing.
func (m *Manager) decide(pid string, fn func(now time.Time, exited func(string) (bool, error)) (string, map[string]string, error)) error {
	if m.kernel == nil {
		_, _, err := fn(time.Now().UTC(), func(string) (bool, error) { return false, nil })
		return err
	}
	_, err := m.kernel.RecordEventFunc(pid, func(now time.Time, lookup func(string) (kernel.Process, error)) (string, map[string]string, error) {
		return fn(now, func(owner string) (bool, error) { return ownerExited(lookup(owner)) })
	})
	return err
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
		if err := fn(&s, m.now()); err != nil {
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
		return fn(&s, m.now())
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
