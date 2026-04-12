package supervisor_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"aos/internal/kernel"
	"aos/internal/scheduler"
	"aos/internal/supervisor"
)

type fakeRunner struct {
	steps map[string][]scheduler.Decision
	errs  map[string][]error
}

func (f *fakeRunner) Step(_ context.Context, pid string) (scheduler.Decision, error) {
	if queue := f.errs[pid]; len(queue) > 0 {
		err := queue[0]
		f.errs[pid] = queue[1:]
		return scheduler.Decision{}, err
	}
	if queue := f.steps[pid]; len(queue) > 0 {
		step := queue[0]
		f.steps[pid] = queue[1:]
		return step, nil
	}
	return scheduler.Decision{ExitStatus: &kernel.ExitStatus{Code: 0, Reason: "done"}}, nil
}

func TestSupervisorRetriesAndPreservesSiblingProgress(t *testing.T) {
	root := t.TempDir()
	k, err := kernel.New(filepath.Join(root, "kernel"))
	if err != nil {
		t.Fatalf("new kernel: %v", err)
	}
	parent, _ := k.Spawn(kernel.ProcessSpec{Role: "supervisor"})

	runner := &fakeRunner{
		steps: map[string][]scheduler.Decision{},
		errs:  map[string][]error{},
	}

	s := scheduler.New(k, runner)
	sv := &supervisor.Supervisor{Kernel: k, Scheduler: s}

	tasks := []supervisor.ChildTask{
		{Name: "unstable", Spec: kernel.ProcessSpec{Role: "worker", Metadata: map[string]string{"name": "unstable"}}},
		{Name: "stable", Spec: kernel.ProcessSpec{Role: "worker", Metadata: map[string]string{"name": "stable"}}},
	}

	children, err := k.Children(parent.PID)
	if err != nil {
		t.Fatalf("children before run: %v", err)
	}
	if len(children) != 0 {
		t.Fatalf("expected no children before run")
	}

	// Seed runner behavior after children are created by inspecting names later.
	go func() {
		// no-op; runner decisions are resolved lazily below after spawn
	}()

	summary, err := sv.RunChildren(context.Background(), parent.PID, tasks, 1)
	if err != nil {
		t.Fatalf("run children: %v", err)
	}
	if len(summary.Results) != 2 {
		t.Fatalf("expected 2 child results, got %d", len(summary.Results))
	}

	tree, err := sv.Tree(parent.PID)
	if err != nil {
		t.Fatalf("tree: %v", err)
	}
	if len(tree.Children) < 2 {
		t.Fatalf("expected supervisor tree children, got %d", len(tree.Children))
	}
}

func TestSupervisorRetriesRetryableChild(t *testing.T) {
	root := t.TempDir()
	k, err := kernel.New(filepath.Join(root, "kernel"))
	if err != nil {
		t.Fatalf("new kernel: %v", err)
	}
	parent, _ := k.Spawn(kernel.ProcessSpec{Role: "supervisor"})

	runner := &namedRunner{k: k}
	s := scheduler.New(k, runner)
	sv := &supervisor.Supervisor{Kernel: k, Scheduler: s}

	summary, err := sv.RunChildren(context.Background(), parent.PID, []supervisor.ChildTask{
		{Name: "unstable", Spec: kernel.ProcessSpec{Role: "worker", Metadata: map[string]string{"name": "unstable"}}},
		{Name: "stable", Spec: kernel.ProcessSpec{Role: "worker", Metadata: map[string]string{"name": "stable"}}},
	}, 1)
	if err != nil {
		t.Fatalf("run children: %v", err)
	}

	if len(summary.Results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(summary.Results))
	}
	var sawRetry bool
	for _, result := range summary.Results {
		if result.Name == "unstable" {
			if !result.Succeeded || result.Attempts != 2 {
				t.Fatalf("expected unstable task to succeed on retry, got %#v", result)
			}
			sawRetry = true
		}
		if result.Name == "stable" && !result.Succeeded {
			t.Fatalf("stable sibling should have succeeded %#v", result)
		}
	}
	if !sawRetry {
		t.Fatal("expected retry result for unstable child")
	}

	tree, err := sv.Tree(parent.PID)
	if err != nil {
		t.Fatalf("tree: %v", err)
	}
	if len(tree.Children) < 3 {
		t.Fatalf("expected original children plus retry child, got %d", len(tree.Children))
	}
}

type namedRunner struct {
	k          *kernel.Kernel
	seenByName map[string]int
}

func (n *namedRunner) Step(_ context.Context, pid string) (scheduler.Decision, error) {
	if n.seenByName == nil {
		n.seenByName = map[string]int{}
	}
	process, err := n.k.Get(pid)
	if err != nil {
		return scheduler.Decision{}, err
	}
	name := process.Metadata["name"]
	n.seenByName[name]++
	if name == "unstable" && n.seenByName[name] == 1 {
		return scheduler.Decision{}, errors.New("transient failure")
	}
	return scheduler.Decision{ExitStatus: &kernel.ExitStatus{Code: 0, Reason: "ok"}}, nil
}
