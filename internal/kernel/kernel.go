package kernel

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

type Kernel struct {
	store *fileStore
	// now is the clock that stamps events, read once per transaction;
	// peek reads what now would return next without advancing it.
	now  func() time.Time
	peek func() time.Time
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
		peek:  time.Now,
	}, nil
}

// SetClock makes the kernel stamp records with now instead of the wall
// clock, as a replay does with the recorded times. The kernel reads it
// once per event.
func (k *Kernel) SetClock(now func() time.Time) {
	if now != nil {
		k.now, k.peek = now, now
	}
}

// SetClocks is SetClock for a clock that advances on every reading, such
// as a replay's recorded times: next is read once per event, and peek
// returns what next would return without advancing it.
func (k *Kernel) SetClocks(next, peek func() time.Time) {
	if next != nil && peek != nil {
		k.now, k.peek = next, peek
	}
}

// Now is the kernel's clock as the next event would read it, without
// advancing it: the wall clock in a live run, the next recorded time in a
// replay. Decisions that depend on time but record no event read it.
func (k *Kernel) Now() time.Time {
	return k.peek().UTC()
}

func (k *Kernel) Spawn(spec ProcessSpec) (Process, error) {
	var process Process
	err := k.store.update(func(t *tx) error {
		var err error
		process, err = t.spawn(spec, k.now().UTC())
		return err
	})
	if err != nil {
		return Process{}, err
	}
	return process, nil
}

// Supersede spawns spec as the successor of pid, an exited process, and
// records it as pid's superseded_by, in one transaction: of any number of
// callers, sequential or concurrent, in this OS process or another, one
// gets the successor and the rest get ErrSuperseded. data is added to the
// process.superseded event, e.g. the checkpoint the successor starts from.
func (k *Kernel) Supersede(pid string, spec ProcessSpec, data map[string]string) (Process, error) {
	var successor Process
	err := k.store.update(func(t *tx) error {
		previous, err := t.process(pid)
		if err != nil {
			return err
		}
		if previous.State != StateExited || previous.ExitStatus == nil {
			return fmt.Errorf("%w: %s is %s; only an exited process can be superseded", ErrInvalidTransition, pid, previous.State)
		}
		if previous.SupersededBy != "" {
			return fmt.Errorf("%w: %s was superseded by %s", ErrSuperseded, pid, previous.SupersededBy)
		}
		now := k.now().UTC()
		if successor, err = t.spawn(spec, now); err != nil {
			return err
		}
		eventData := cloneMap(data)
		if eventData == nil {
			eventData = map[string]string{}
		}
		eventData["successor"] = successor.PID
		event := t.appendEvent(pid, "process.superseded", eventData, now)
		previous.SupersededBy = successor.PID
		previous.UpdatedAt = now
		previous.LastEventID = event.ID
		t.putProcess(previous)
		return nil
	})
	if err != nil {
		return Process{}, err
	}
	return successor, nil
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
		if status.Checkpoint != "" {
			data["checkpoint"] = status.Checkpoint
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

// RecordEventFunc records an event decided at one reading of the
// kernel's clock: decide gets that reading, and a lookup of the process
// table as of the same transaction, and returns the event to record,
// stamped with the same reading. Returning an empty type records nothing;
// an error records nothing and is returned. A caller whose state depends
// on the time, such as a lease's expiry, so derives it from the event's
// own timestamp, which a replay reproduces. decide runs under the
// kernel's lock and must not call the kernel.
func (k *Kernel) RecordEventFunc(pid string, decide func(now time.Time, lookup func(pid string) (Process, error)) (string, map[string]string, error)) (Event, error) {
	var event Event
	err := k.store.update(func(t *tx) error {
		now := k.peek().UTC()
		eventType, data, err := decide(now, t.process)
		if err != nil || eventType == "" {
			return err
		}
		if strings.HasPrefix(eventType, ReservedEventPrefix) {
			return fmt.Errorf("%w: %s", ErrReservedEventType, eventType)
		}
		// Advance the clock past the reading the event takes.
		_ = k.now()
		event = t.appendEvent(pid, eventType, data, now)
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
