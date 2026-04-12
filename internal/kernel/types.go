package kernel

import (
	"errors"
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

type ExitStatus struct {
	Code      int    `json:"code"`
	Reason    string `json:"reason"`
	Retryable bool   `json:"retryable"`
}

type ProcessSpec struct {
	ParentPID     string             `json:"parent_pid,omitempty"`
	Role          string             `json:"role"`
	Metadata      map[string]string  `json:"metadata,omitempty"`
	Mounts        []capability.Mount `json:"mounts,omitempty"`
	CapabilitySet capability.Set     `json:"capability_set"`
}

type Process struct {
	PID           string             `json:"pid"`
	ParentPID     string             `json:"parent_pid,omitempty"`
	Role          string             `json:"role"`
	State         ProcessState       `json:"state"`
	Metadata      map[string]string  `json:"metadata,omitempty"`
	Mounts        []capability.Mount `json:"mounts,omitempty"`
	CapabilitySet capability.Set     `json:"capability_set"`
	CreatedAt     time.Time          `json:"created_at"`
	UpdatedAt     time.Time          `json:"updated_at"`
	ExitStatus    *ExitStatus        `json:"exit_status,omitempty"`
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
