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

type selfExitingRunner struct {
	kernel *kernel.Kernel
	status kernel.ExitStatus
}

func (r selfExitingRunner) Step(_ context.Context, pid string) (scheduler.Decision, error) {
	if _, err := r.kernel.Exit(pid, r.status); err != nil {
		return scheduler.Decision{}, err
	}
	return scheduler.Decision{}, errors.New(r.status.Reason)
}

func TestSchedulerKeepsExitRecordedByRunner(t *testing.T) {
	k, err := kernel.New(filepath.Join(t.TempDir(), "kernel"))
	if err != nil {
		t.Fatalf("new kernel: %v", err)
	}
	process, _ := k.Spawn(kernel.ProcessSpec{Role: "worker"})
	status := kernel.ExitStatus{Code: 1, Reason: "malformed backend output", Retryable: false}

	s := scheduler.New(k, selfExitingRunner{kernel: k, status: status})
	s.Enqueue(process.PID)
	if err := s.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	got, err := k.Await(process.PID)
	if err != nil {
		t.Fatalf("await: %v", err)
	}
	if got != status {
		t.Fatalf("expected runner exit %#v to be kept, got %#v", status, got)
	}
}

// A runner that asks for exited without a status must not leave a process
// that is exited but has no exit record.
func TestSchedulerRecordsExitWhenRunnerOmitsStatus(t *testing.T) {
	k, err := kernel.New(filepath.Join(t.TempDir(), "kernel"))
	if err != nil {
		t.Fatalf("new kernel: %v", err)
	}
	process, _ := k.Spawn(kernel.ProcessSpec{Role: "worker"})
	runner := &fakeRunner{steps: map[string][]scheduler.Decision{process.PID: {{NextState: kernel.StateExited}}}}

	s := scheduler.New(k, runner)
	s.Enqueue(process.PID)
	if err := s.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	status, err := k.Await(process.PID)
	if err != nil {
		t.Fatalf("await: %v", err)
	}
	if status.Code != 1 || status.Retryable || status.Reason != "runner returned exited without an exit status" {
		t.Fatalf("unexpected exit record %#v", status)
	}
}

// A runner that records its own exit and then reports exited keeps that exit.
func TestSchedulerKeepsRunnerExitWhenDecisionSaysExited(t *testing.T) {
	k, err := kernel.New(filepath.Join(t.TempDir(), "kernel"))
	if err != nil {
		t.Fatalf("new kernel: %v", err)
	}
	process, _ := k.Spawn(kernel.ProcessSpec{Role: "worker"})
	status := kernel.ExitStatus{Code: 0, Reason: "finished by runner"}

	s := scheduler.New(k, exitedDecisionRunner{kernel: k, status: status})
	s.Enqueue(process.PID)
	if err := s.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got, err := k.Await(process.PID); err != nil || got != status {
		t.Fatalf("expected runner exit %#v, got %#v (err %v)", status, got, err)
	}
}

type exitedDecisionRunner struct {
	kernel *kernel.Kernel
	status kernel.ExitStatus
}

func (r exitedDecisionRunner) Step(_ context.Context, pid string) (scheduler.Decision, error) {
	if _, err := r.kernel.Exit(pid, r.status); err != nil {
		return scheduler.Decision{}, err
	}
	return scheduler.Decision{NextState: kernel.StateExited}, nil
}
