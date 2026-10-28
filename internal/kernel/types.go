package kernel

import (
	"errors"
	"fmt"
	"time"

	"aos/internal/capability"
)

type ProcessState string

const (
	StateReady    ProcessState = "ready"
	StateRunning  ProcessState = "running"
	StateBlocked  ProcessState = "blocked"
	StateSleeping ProcessState = "sleeping"
	StateExited   ProcessState = "exited"
)

var ErrInvalidTransition = errors.New("invalid process state transition")
var ErrProcessNotFound = errors.New("process not found")
var ErrProcessNotExited = errors.New("process has not exited")

// ErrProcessAlreadyExited is returned when Exit would change the status of a
// process that has already exited, or Transition would move it; exit is
// terminal.
var ErrProcessAlreadyExited = fmt.Errorf("%w: process already exited", ErrInvalidTransition)

// ErrExitRequiresStatus: only Exit, which records an exit status, may move a
// process to exited, so every exited process has a record Await can return.
var ErrExitRequiresStatus = fmt.Errorf("%w: only Exit can move a process to exited", ErrInvalidTransition)

// ErrReservedEventType: event types under ReservedEventPrefix are written by
// the kernel alone, so callers cannot log a false lifecycle event.
var ErrReservedEventType = errors.New("event type is reserved for the kernel")

// ReservedEventPrefix marks the kernel's own lifecycle events.
const ReservedEventPrefix = "process."

type ExitStatus struct {
	Code      int    `json:"code"`
	Reason    string `json:"reason"`
	Retryable bool   `json:"retryable"`
	// Origin says who decided the exit: ExitOriginAgent for an agent's own
	// exit action; empty for exits the runtime, engine or scheduler recorded
	// (step limit, crash, cancel, errors). An agent cannot set it: the
	// runtime overwrites it at the one place it executes exit actions.
	Origin string `json:"origin,omitempty"`
	// Checkpoint is the process's latest checkpoint when it exited, if any:
	// where a Recover would resume from.
	Checkpoint string `json:"checkpoint,omitempty"`
}

// ExitOriginAgent marks an exit the agent chose itself.
const ExitOriginAgent = "agent"

type ProcessSpec struct {
	ParentPID     string             `json:"parent_pid,omitempty"`
	Role          string             `json:"role"`
	Metadata      map[string]string  `json:"metadata,omitempty"`
	Mounts        []capability.Mount `json:"mounts,omitempty"`
	CapabilitySet capability.Set     `json:"capability_set"`
	// Delegable is authority the process may grant to children but not use.
	Delegable capability.Set `json:"delegable"`
}

type Process struct {
	PID           string             `json:"pid"`
	ParentPID     string             `json:"parent_pid,omitempty"`
	Role          string             `json:"role"`
	State         ProcessState       `json:"state"`
	Metadata      map[string]string  `json:"metadata,omitempty"`
	Mounts        []capability.Mount `json:"mounts,omitempty"`
	CapabilitySet capability.Set     `json:"capability_set"`
	Delegable     capability.Set     `json:"delegable"`
	CreatedAt     time.Time          `json:"created_at"`
	UpdatedAt     time.Time          `json:"updated_at"`
	ExitStatus    *ExitStatus        `json:"exit_status,omitempty"`
	// LastEventID is the event that produced this record; the event log is
	// authoritative and the table is repaired from events after it.
	LastEventID string `json:"last_event_id,omitempty"`
}

type Event struct {
	ID        string            `json:"id"`
	ProcessID string            `json:"process_id"`
	Type      string            `json:"type"`
	Timestamp time.Time         `json:"timestamp"`
	Data      map[string]string `json:"data,omitempty"`
}

func ValidateTransition(from, to ProcessState) error {
	if from == to {
		return nil
	}

	allowed := map[ProcessState]map[ProcessState]bool{
		StateReady: {
			StateRunning:  true,
			StateBlocked:  true,
			StateSleeping: true,
			StateExited:   true,
		},
		StateRunning: {
			StateReady:    true,
			StateBlocked:  true,
			StateSleeping: true,
			StateExited:   true,
		},
		StateBlocked: {
			StateReady:   true,
			StateRunning: true,
			StateExited:  true,
		},
		StateSleeping: {
			StateReady:   true,
			StateRunning: true,
			StateExited:  true,
		},
		StateExited: {},
	}

	if !allowed[from][to] {
		return ErrInvalidTransition
	}

	return nil
}
