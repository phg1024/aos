package kernel

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"aos/internal/capability"
)

type Kernel struct {
	store *FileStore
	now   func() time.Time
}

func New(root string) (*Kernel, error) {
	store, err := NewFileStore(root)
	if err != nil {
		return nil, err
	}
	return &Kernel{
		store: store,
		now:   time.Now,
	}, nil
}

func (k *Kernel) Spawn(spec ProcessSpec) (Process, error) {
	pid, err := k.store.AllocatePID()
	if err != nil {
		return Process{}, err
	}

	now := k.now().UTC()
	process := Process{
		PID:           pid,
		ParentPID:     spec.ParentPID,
		Role:          spec.Role,
		State:         StateReady,
		Metadata:      cloneMap(spec.Metadata),
		Mounts:        append([]capability.Mount(nil), spec.Mounts...),
		CapabilitySet: spec.CapabilitySet,
		CreatedAt:     now,
		UpdatedAt:     now,
	}

	if err := k.store.SaveProcess(process); err != nil {
		return Process{}, err
	}
	if err := k.appendEvent(process.PID, "process.spawned", map[string]string{
		"role": process.Role,
	}); err != nil {
		return Process{}, err
	}
	return process, nil
}

func (k *Kernel) Transition(pid string, next ProcessState) (Process, error) {
	if next == StateExited {
		return Process{}, ErrExitRequiresStatus
	}
	process, err := k.store.LoadProcess(pid)
	if err != nil {
		return Process{}, err
	}
	if process.State == StateExited {
		return Process{}, ErrProcessAlreadyExited
	}
	if err := ValidateTransition(process.State, next); err != nil {
		return Process{}, err
	}
	process.State = next
	process.UpdatedAt = k.now().UTC()
	if err := k.store.SaveProcess(process); err != nil {
		return Process{}, err
	}
	if err := k.appendEvent(pid, "process.transition", map[string]string{
		"state": string(next),
	}); err != nil {
		return Process{}, err
	}
	return process, nil
}

func (k *Kernel) Exit(pid string, status ExitStatus) (Process, error) {
	process, err := k.store.LoadProcess(pid)
	if err != nil {
		return Process{}, err
	}
	if process.State == StateExited {
		// Exit is terminal: repeating the same status is a no-op, any other
		// status is refused so a recorded exit can never be rewritten.
		if process.ExitStatus != nil && *process.ExitStatus == status {
			return process, nil
		}
		return Process{}, ErrProcessAlreadyExited
	}
	if err := ValidateTransition(process.State, StateExited); err != nil {
		return Process{}, err
	}
	process.State = StateExited
	process.ExitStatus = &status
	process.UpdatedAt = k.now().UTC()
	if err := k.store.SaveProcess(process); err != nil {
		return Process{}, err
	}
	if err := k.appendEvent(pid, "process.exited", map[string]string{
		"code":      strconv.Itoa(status.Code),
		"reason":    status.Reason,
		"retryable": strconv.FormatBool(status.Retryable),
	}); err != nil {
		return Process{}, err
	}
	return process, nil
}

func (k *Kernel) Await(pid string) (ExitStatus, error) {
	process, err := k.store.LoadProcess(pid)
	if err != nil {
		return ExitStatus{}, err
	}
	if process.State != StateExited || process.ExitStatus == nil {
		return ExitStatus{}, ErrProcessNotExited
	}
	return *process.ExitStatus, nil
}

func (k *Kernel) Get(pid string) (Process, error) { return k.store.LoadProcess(pid) }
func (k *Kernel) Processes() ([]Process, error)   { return k.store.ListProcesses() }
func (k *Kernel) Events() ([]Event, error)        { return k.store.ListEvents() }

// LogEvent records a caller event. Types starting with ReservedEventPrefix
// belong to the kernel and are refused.
func (k *Kernel) LogEvent(pid, eventType string, data map[string]string) error {
	if strings.HasPrefix(eventType, ReservedEventPrefix) {
		return fmt.Errorf("%w: %s", ErrReservedEventType, eventType)
	}
	return k.appendEvent(pid, eventType, data)
}

func (k *Kernel) Children(parentPID string) ([]Process, error) {
	processes, err := k.store.ListProcesses()
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

func (k *Kernel) appendEvent(pid, eventType string, data map[string]string) error {
	id, err := k.store.AllocateEventID()
	if err != nil {
		return err
	}
	return k.store.AppendEvent(Event{
		ID:        id,
		ProcessID: pid,
		Type:      eventType,
		Timestamp: k.now().UTC(),
		Data:      cloneMap(data),
	})
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
