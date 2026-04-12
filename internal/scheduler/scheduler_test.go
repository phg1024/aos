package scheduler_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"aos/internal/kernel"
	"aos/internal/scheduler"
)

type fakeRunner struct {
	steps map[string][]scheduler.Decision
	errs  map[string]error
}

func (f *fakeRunner) Step(_ context.Context, pid string) (scheduler.Decision, error) {
	if err := f.errs[pid]; err != nil {
		delete(f.errs, pid)
		return scheduler.Decision{}, err
	}
	queue := f.steps[pid]
	if len(queue) == 0 {
		return scheduler.Decision{ExitStatus: &kernel.ExitStatus{Code: 0, Reason: "done"}}, nil
	}
	step := queue[0]
	f.steps[pid] = queue[1:]
	return step, nil
}

func TestSchedulerRoundRobinAndBlockedWakeFlow(t *testing.T) {
	root := t.TempDir()
	k, err := kernel.New(filepath.Join(root, "kernel"))
	if err != nil {
		t.Fatalf("new kernel: %v", err)
	}
	a, _ := k.Spawn(kernel.ProcessSpec{Role: "worker-a"})
	b, _ := k.Spawn(kernel.ProcessSpec{Role: "worker-b"})

	runner := &fakeRunner{
		steps: map[string][]scheduler.Decision{
			a.PID: {
				{NextState: kernel.StateReady, Requeue: true},
				{ExitStatus: &kernel.ExitStatus{Code: 0, Reason: "a complete"}},
			},
			b.PID: {
				{NextState: kernel.StateBlocked, Requeue: false},
				{ExitStatus: &kernel.ExitStatus{Code: 0, Reason: "b complete"}},
			},
		},
		errs: map[string]error{},
	}

	s := scheduler.New(k, runner)
	s.Enqueue(a.PID)
	s.Enqueue(b.PID)
	if err := s.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	aprocess, _ := k.Get(a.PID)
	if aprocess.State != kernel.StateExited {
		t.Fatalf("expected a exited, got %s", aprocess.State)
	}
	bprocess, _ := k.Get(b.PID)
	if bprocess.State != kernel.StateBlocked {
		t.Fatalf("expected b blocked, got %s", bprocess.State)
	}

	if err := s.Wake(b.PID); err != nil {
		t.Fatalf("wake: %v", err)
	}
	if err := s.Run(context.Background()); err != nil {
		t.Fatalf("run after wake: %v", err)
	}
	bprocess, _ = k.Get(b.PID)
	if bprocess.State != kernel.StateExited {
		t.Fatalf("expected b exited after wake, got %s", bprocess.State)
	}

	trace := s.Trace()
	if len(trace) < 6 {
		t.Fatalf("expected scheduler trace entries, got %d", len(trace))
	}
}

func TestSchedulerExitsRetryableOnRunnerError(t *testing.T) {
	root := t.TempDir()
	k, err := kernel.New(filepath.Join(root, "kernel"))
	if err != nil {
		t.Fatalf("new kernel: %v", err)
	}
	process, _ := k.Spawn(kernel.ProcessSpec{Role: "worker"})
	s := scheduler.New(k, &fakeRunner{
		steps: map[string][]scheduler.Decision{},
		errs:  map[string]error{process.PID: errors.New("temporary failure")},
	})
	s.Enqueue(process.PID)
	if err := s.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	exit, err := k.Await(process.PID)
	if err != nil {
		t.Fatalf("await: %v", err)
	}
	if !exit.Retryable {
		t.Fatalf("expected retryable exit %#v", exit)
	}
}
