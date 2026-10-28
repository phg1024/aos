package scheduler

import (
	"context"
	"strconv"
	"sync"
	"time"

	"aos/internal/kernel"
)

type Decision struct {
	NextState  kernel.ProcessState
	ExitStatus *kernel.ExitStatus
	Requeue    bool
}

type Runner interface {
	Step(ctx context.Context, pid string) (Decision, error)
}

type TraceEntry struct {
	At      time.Time `json:"at"`
	PID     string    `json:"pid"`
	Event   string    `json:"event"`
	Details string    `json:"details,omitempty"`
}

type Scheduler struct {
	Kernel  *kernel.Kernel
	Runner  Runner
	now     func() time.Time
	mu      sync.Mutex
	queue   []string
	traces  []TraceEntry
	maxRuns int
}

func New(k *kernel.Kernel, runner Runner) *Scheduler {
	return &Scheduler{
		Kernel: k,
		Runner: runner,
		now:    time.Now,
	}
}

func (s *Scheduler) Enqueue(pid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queue = append(s.queue, pid)
	s.recordTrace(pid, "enqueue", "")
}

func (s *Scheduler) Wake(pid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	process, err := s.Kernel.Get(pid)
	if err != nil {
		return err
	}
	if process.State == kernel.StateBlocked || process.State == kernel.StateSleeping {
		if _, err := s.Kernel.Transition(pid, kernel.StateReady); err != nil {
			return err
		}
	}
	s.queue = append(s.queue, pid)
	s.recordTrace(pid, "wake", "")
	return nil
}

func (s *Scheduler) Run(ctx context.Context) error {
	for {
		pid, ok := s.next()
		if !ok {
			return nil
		}
		if err := s.dispatch(ctx, pid); err != nil {
			return err
		}
	}
}

func (s *Scheduler) Trace() []TraceEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]TraceEntry, len(s.traces))
	copy(out, s.traces)
	return out
}

func (s *Scheduler) QueueLength() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.queue)
}

func (s *Scheduler) next() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queue) == 0 {
		return "", false
	}
	pid := s.queue[0]
	s.queue = s.queue[1:]
	return pid, true
}

func (s *Scheduler) dispatch(ctx context.Context, pid string) error {
	process, err := s.Kernel.Get(pid)
	if err != nil {
		return err
	}
	if process.State != kernel.StateReady {
		s.log(pid, "skip", "state="+string(process.State))
		return nil
	}

	if _, err := s.Kernel.Transition(pid, kernel.StateRunning); err != nil {
		return err
	}
	s.log(pid, "dispatch", "")

	decision, err := s.Runner.Step(ctx, pid)
	if err != nil {
		// A runner may record its own exit before failing; that exit is final.
		if current, getErr := s.Kernel.Get(pid); getErr == nil && current.State == kernel.StateExited {
			s.log(pid, "exit", "error")
			return nil
		}
		exitStatus := kernel.ExitStatus{
			Code:      1,
			Reason:    err.Error(),
			Retryable: true,
		}
		if _, exitErr := s.Kernel.Exit(pid, exitStatus); exitErr != nil {
			return exitErr
		}
		s.log(pid, "exit", "error")
		return nil
	}

	if decision.ExitStatus != nil {
		if _, err := s.Kernel.Exit(pid, *decision.ExitStatus); err != nil {
			return err
		}
		s.log(pid, "exit", decision.ExitStatus.Reason)
		return nil
	}

	nextState := decision.NextState
	if nextState == kernel.StateExited {
		// Only Exit may end a process. Keep an exit the runner recorded
		// itself; otherwise record that it gave no status.
		if current, getErr := s.Kernel.Get(pid); getErr == nil && current.State == kernel.StateExited {
			s.log(pid, "exit", "recorded by runner")
			return nil
		}
		missing := kernel.ExitStatus{Code: 1, Reason: "runner returned exited without an exit status", Retryable: false}
		if _, err := s.Kernel.Exit(pid, missing); err != nil {
			return err
		}
		s.log(pid, "exit", missing.Reason)
		return nil
	}
	if nextState == "" {
		nextState = kernel.StateReady
	}
	if nextState != kernel.StateRunning {
		if _, err := s.Kernel.Transition(pid, nextState); err != nil {
			return err
		}
	}
	s.log(pid, "transition", string(nextState))

	if decision.Requeue && nextState == kernel.StateReady {
		s.Enqueue(pid)
	}
	return nil
}

func (s *Scheduler) log(pid, event, details string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recordTrace(pid, event, details)
}

func (s *Scheduler) recordTrace(pid, event, details string) {
	entry := TraceEntry{
		At:      s.now().UTC(),
		PID:     pid,
		Event:   event,
		Details: details,
	}
	s.traces = append(s.traces, entry)
	_ = s.Kernel.LogEvent(pid, "scheduler."+event, map[string]string{
		"details": details,
		"trace":   strconv.Itoa(len(s.traces)),
	})
}
