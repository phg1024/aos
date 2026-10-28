package kernel

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"aos/internal/capability"
)

type Kernel struct {
	store *fileStore
	now   func() time.Time
}

// New opens the kernel state under root, the only way to reach it. Opening
// replays any events the process table has not caught up with, e.g. after a
// crash between an event and its table write.
func New(root string) (*Kernel, error) {
	store, err := newFileStore(root)
	if err != nil {
		return nil, err
	}
	if err := store.update(func(*tx) error { return nil }); err != nil {
		return nil, err
	}
	return &Kernel{
		store: store,
		now:   time.Now,
	}, nil
}

func (k *Kernel) Spawn(spec ProcessSpec) (Process, error) {
	var process Process
	err := k.store.update(func(t *tx) error {
		now := k.now().UTC()
		process = Process{
			PID:           t.allocatePID(),
			ParentPID:     spec.ParentPID,
			Role:          spec.Role,
			State:         StateReady,
			Metadata:      cloneMap(spec.Metadata),
			Mounts:        append([]capability.Mount(nil), spec.Mounts...),
			CapabilitySet: spec.CapabilitySet,
			Delegable:     spec.Delegable,
			CreatedAt:     now,
			UpdatedAt:     now,
		}
		// The event carries the whole record so the table can be rebuilt.
		encoded, err := json.Marshal(process)
		if err != nil {
			return err
		}
		event := t.appendEvent(process.PID, "process.spawned", map[string]string{
			"role":    process.Role,
			"process": string(encoded),
		}, now)
		process.LastEventID = event.ID
		t.putProcess(process)
		return nil
	})
	if err != nil {
		return Process{}, err
	}
	return process, nil
}

func (k *Kernel) Transition(pid string, next ProcessState) (Process, error) {
	if next == StateExited {
		return Process{}, ErrExitRequiresStatus
	}
	var process Process
	err := k.store.update(func(t *tx) error {
		var err error
		process, err = t.process(pid)
		if err != nil {
			return err
		}
		if process.State == StateExited {
			return ErrProcessAlreadyExited
		}
		if err := ValidateTransition(process.State, next); err != nil {
			return err
		}
		now := k.now().UTC()
		event := t.appendEvent(pid, "process.transition", map[string]string{
			"state": string(next),
		}, now)
		process.State = next
		process.UpdatedAt = now
		process.LastEventID = event.ID
		t.putProcess(process)
		return nil
	})
	if err != nil {
		return Process{}, err
	}
	return process, nil
}

func (k *Kernel) Exit(pid string, status ExitStatus) (Process, error) {
	var process Process
	err := k.store.update(func(t *tx) error {
		var err error
		process, err = t.process(pid)
		if err != nil {
			return err
		}
		if process.State == StateExited {
			// Exit is terminal: repeating the same status is a no-op, any other
			// status is refused so a recorded exit can never be rewritten.
			if process.ExitStatus != nil && *process.ExitStatus == status {
				return nil
			}
			return ErrProcessAlreadyExited
		}
		if err := ValidateTransition(process.State, StateExited); err != nil {
			return err
		}
		now := k.now().UTC()
		data := map[string]string{
			"code":      strconv.Itoa(status.Code),
			"reason":    status.Reason,
			"retryable": strconv.FormatBool(status.Retryable),
		}
		if status.Origin != "" {
			data["origin"] = status.Origin
		}
		event := t.appendEvent(pid, "process.exited", data, now)
		process.State = StateExited
		process.ExitStatus = &status
		process.UpdatedAt = now
		process.LastEventID = event.ID
		t.putProcess(process)
		return nil
	})
	if err != nil {
		return Process{}, err
	}
	return process, nil
}

func (k *Kernel) Await(pid string) (ExitStatus, error) {
	process, err := k.Get(pid)
	if err != nil {
		return ExitStatus{}, err
	}
	if process.State != StateExited || process.ExitStatus == nil {
		return ExitStatus{}, ErrProcessNotExited
	}
	return *process.ExitStatus, nil
}

func (k *Kernel) Get(pid string) (Process, error) {
	var process Process
	err := k.store.view(func(t *tx) error {
		var err error
		process, err = t.process(pid)
		return err
	})
	return process, err
}

func (k *Kernel) Processes() ([]Process, error) {
	var out []Process
	err := k.store.view(func(t *tx) error {
		out = make([]Process, 0, len(t.processes))
		for _, process := range t.processes {
			out = append(out, process)
		}
		return nil
	})
	return out, err
}

func (k *Kernel) Events() ([]Event, error) {
	var events []Event
	err := k.store.view(func(t *tx) error {
		events = t.events
		return nil
	})
	return events, err
}

// LogEvent records a caller event. Types starting with ReservedEventPrefix
// belong to the kernel and are refused.
func (k *Kernel) LogEvent(pid, eventType string, data map[string]string) error {
	_, err := k.RecordEvent(pid, eventType, data)
	return err
}

// RecordEvent is LogEvent returning the recorded event, e.g. so a trace can
// name its kernel event ID.
func (k *Kernel) RecordEvent(pid, eventType string, data map[string]string) (Event, error) {
	if strings.HasPrefix(eventType, ReservedEventPrefix) {
		return Event{}, fmt.Errorf("%w: %s", ErrReservedEventType, eventType)
	}
	var event Event
	err := k.store.update(func(t *tx) error {
		event = t.appendEvent(pid, eventType, data, k.now().UTC())
		return nil
	})
	return event, err
}

func (k *Kernel) Children(parentPID string) ([]Process, error) {
	processes, err := k.Processes()
	if err != nil {
		return nil, err
	}
	var children []Process
	for _, process := range processes {
		if process.ParentPID == parentPID {
			children = append(children, process)
		}
	}
	return children, nil
}

func cloneMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
