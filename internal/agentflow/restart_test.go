package agentflow_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"aos/internal/agentflow"
	"aos/internal/artifact"
	"aos/internal/capability"
	"aos/internal/kernel"
	"aos/internal/lease"
	"aos/internal/runtime"
	"aos/internal/supervisor"
	"aos/internal/tools"
)

// script is one agent's scripted backend: replies in order, each built
// from the request, so a step can see the process and its history. It
// keeps every request's history. The same script answers the agent's
// successors, continuing where it was.
type script struct {
	mu      sync.Mutex
	replies []func(runtime.GenerateRequest) runtime.GenerateResult
	seen    []runtime.GenerateRequest
}

func (s *script) Generate(_ context.Context, request runtime.GenerateRequest) (runtime.GenerateResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.seen)
	s.seen = append(s.seen, request)
	if n >= len(s.replies) {
		return runtime.GenerateResult{}, errors.New("script exhausted")
	}
	return s.replies[n](request), nil
}

func (s *script) requests() []runtime.GenerateRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]runtime.GenerateRequest(nil), s.seen...)
}

func reply(result runtime.GenerateResult) func(runtime.GenerateRequest) runtime.GenerateResult {
	return func(runtime.GenerateRequest) runtime.GenerateResult { return result }
}

func toolStep(name string, args map[string]string) func(runtime.GenerateRequest) runtime.GenerateResult {
	return reply(runtime.GenerateResult{Action: runtime.GenerateAction{Type: "tool", ToolCall: &runtime.ToolCall{Name: name, Args: args}}})
}

func exitStep(code int, retryable bool, reason string) func(runtime.GenerateRequest) runtime.GenerateResult {
	return reply(runtime.GenerateResult{Action: runtime.GenerateAction{Type: "exit", ExitStatus: &kernel.ExitStatus{Code: code, Reason: reason, Retryable: retryable}}})
}

func delegateStep(agentID string) func(runtime.GenerateRequest) runtime.GenerateResult {
	return reply(runtime.GenerateResult{Action: runtime.GenerateAction{Type: "delegate", Delegate: &runtime.DelegateCall{AgentID: agentID, Goal: "do the work"}}})
}

type scripts map[string]*script

func (s scripts) BackendFor(agent agentflow.AgentSpec, _ agentflow.WorkflowSpec) (runtime.Backend, error) {
	if backend, ok := s[agent.ID]; ok {
		return backend, nil
	}
	return nil, errors.New("no script for " + agent.ID)
}

type restartFixture struct {
	kernel    *kernel.Kernel
	store     *artifact.Store
	leases    *lease.Manager
	workspace string
}

func newRestartFixture(t *testing.T) restartFixture {
	t.Helper()
	root := t.TempDir()
	k, err := kernel.New(filepath.Join(root, "kernel"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := artifact.New(filepath.Join(root, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	leases, err := lease.New(filepath.Join(root, "leases"), k)
	if err != nil {
		t.Fatal(err)
	}
	workspace, _ := filepath.EvalSymlinks(t.TempDir())
	return restartFixture{kernel: k, store: store, leases: leases, workspace: workspace}
}

// spec is a coordinator that delegates to worker, which has policy.
func (f restartFixture) spec(policy *supervisor.RestartPolicy, maxSteps int) agentflow.WorkflowSpec {
	worker := capability.Set{Tools: []string{"write_file", "read_file", "acquire_lease"}, ReadPaths: []string{f.workspace}, WritePaths: []string{f.workspace}}
	return agentflow.WorkflowSpec{
		Name:       "restarts",
		EntryAgent: "coordinator",
		Agents: []agentflow.AgentSpec{
			{ID: "coordinator", SystemPrompt: "Delegate.", AllowedAgents: []string{"worker"}, Capabilities: capability.Union(capability.Set{Tools: []string{"spawn_agent"}}, worker),
				Mounts: []capability.Mount{{Name: tools.WorkspaceMount, HostPath: f.workspace}}},
			{ID: "worker", SystemPrompt: "Work.", MaxSteps: maxSteps, Capabilities: worker, Restart: policy,
				Mounts: []capability.Mount{{Name: tools.WorkspaceMount, HostPath: f.workspace}}},
		},
	}
}

func (f restartFixture) run(t *testing.T, ctx context.Context, spec agentflow.WorkflowSpec, backends scripts) (agentflow.RunSummary, []agentflow.TraceEvent) {
	t.Helper()
	var mu sync.Mutex
	var trace []agentflow.TraceEvent
	engine := agentflow.Engine{Kernel: f.kernel, Artifacts: f.store, Leases: f.leases, Tools: tools.NewRuntime(), Factory: backends,
		Trace: func(event agentflow.TraceEvent) {
			mu.Lock()
			trace = append(trace, event)
			mu.Unlock()
		}}
	summary, err := engine.Run(ctx, spec, agentflow.RunRequest{Goal: "go"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return summary, trace
}

func (f restartFixture) events(t *testing.T, eventType string) []kernel.Event {
	t.Helper()
	events, err := f.kernel.Events()
	if err != nil {
		t.Fatal(err)
	}
	var out []kernel.Event
	for _, event := range events {
		if event.Type == eventType {
			out = append(out, event)
		}
	}
	return out
}

func (f restartFixture) workers(t *testing.T) []kernel.Process {
	t.Helper()
	processes, _ := f.kernel.Processes()
	var out []kernel.Process
	for _, process := range processes {
		if process.Role == "agent:worker" {
			out = append(out, process)
		}
	}
	return out
}

func historyText(request runtime.GenerateRequest) string {
	var b strings.Builder
	for _, message := range request.History {
		b.WriteString(message.Content)
		b.WriteString("\n")
	}
	return b.String()
}

// The coordinator's delegate result, as its next request shows it.
func delegateNote(t *testing.T, coordinator *script) string {
	t.Helper()
	requests := coordinator.requests()
	if len(requests) < 2 {
		t.Fatalf("the coordinator made %d calls", len(requests))
	}
	return historyText(requests[1])
}

// A worker that exits retryably once is recovered from the checkpoint its
// exit names, through the exclusive Recover, and the coordinator sees one
// success. supervisor.restart names the predecessor, the attempt, the
// checkpoint and the backoff.
func TestTransientChildRestartsFromCheckpoint(t *testing.T) {
	f := newRestartFixture(t)
	target := filepath.Join(f.workspace, "notes.txt")
	worker := &script{replies: []func(runtime.GenerateRequest) runtime.GenerateResult{
		toolStep("write_file", map[string]string{"path": target, "content": "step one"}),
		exitStep(1, true, "flaky dependency"),
		exitStep(0, false, "done after restart"),
	}}
	coordinator := &script{replies: []func(runtime.GenerateRequest) runtime.GenerateResult{delegateStep("worker"), exitStep(0, false, "coordinated")}}
	summary, trace := f.run(t, context.Background(), f.spec(&supervisor.RestartPolicy{Max: 2, Within: "1m", Backoff: "0s"}, 8), scripts{"worker": worker, "coordinator": coordinator})
	if summary.ExitStatus.Code != 0 {
		t.Fatalf("summary = %+v", summary.ExitStatus)
	}
	workers := f.workers(t)
	if len(workers) != 2 {
		t.Fatalf("%d worker processes", len(workers))
	}
	first, second := workers[0], workers[1]
	if first.PID > second.PID {
		first, second = second, first
	}
	restarts := f.events(t, agentflow.RestartEvent)
	if len(restarts) != 1 || restarts[0].ProcessID != second.PID || restarts[0].Data["predecessor"] != first.PID || restarts[0].Data["attempt"] != "1" ||
		restarts[0].Data["checkpoint"] != first.ExitStatus.Checkpoint || restarts[0].Data["backoff"] != "0s" || restarts[0].Data["reason"] != "flaky dependency" {
		t.Fatalf("supervisor.restart = %+v (first %+v)", restarts, first.ExitStatus)
	}
	if first.SupersededBy != second.PID || second.Metadata["resumed_from"] != first.PID {
		t.Fatalf("superseded_by %q, resumed_from %q", first.SupersededBy, second.Metadata["resumed_from"])
	}
	// The successor continued from the checkpoint: its first request has
	// the write it did before.
	if requests := worker.requests(); len(requests) != 3 || !strings.Contains(historyText(requests[2]), "\"content\":\"step one\"") || requests[2].Process.PID != second.PID {
		t.Fatalf("the restarted request did not continue the checkpoint: %q", historyText(worker.requests()[2]))
	}
	if note := delegateNote(t, coordinator); !strings.Contains(note, "done after restart") || strings.Contains(note, "flaky") {
		t.Fatalf("the coordinator saw: %s", note)
	}
	kinds := ""
	for _, event := range trace {
		if event.AgentID == "worker" {
			kinds += event.Kind + ","
		}
	}
	if !strings.Contains(kinds, "exit,restart,") {
		t.Fatalf("trace: %s", kinds)
	}
}

// A worker that always fails retryably is restarted max times within the
// window, then ends with a final exit naming the intensity, which the
// coordinator sees; the engine does not restart the coordinator.
func TestRestartIntensityEscalates(t *testing.T) {
	f := newRestartFixture(t)
	worker := &script{}
	for i := 0; i < 10; i++ {
		worker.replies = append(worker.replies, exitStep(1, true, "always fails"))
	}
	coordinator := &script{replies: []func(runtime.GenerateRequest) runtime.GenerateResult{delegateStep("worker"), exitStep(0, false, "handled the failure")}}
	f.run(t, context.Background(), f.spec(&supervisor.RestartPolicy{Max: 2, Within: "1m", Backoff: "0s"}, 8), scripts{"worker": worker, "coordinator": coordinator})
	if n := len(f.workers(t)); n != 3 {
		t.Fatalf("%d worker processes, want 3 (2 restarts)", n)
	}
	if n := len(f.events(t, agentflow.RestartEvent)); n != 2 {
		t.Fatalf("%d restarts", n)
	}
	escalated := f.events(t, agentflow.EscalatedEvent)
	if len(escalated) != 1 || escalated[0].Data["restarts"] != "2" || escalated[0].Data["max"] != "2" {
		t.Fatalf("supervisor.escalated = %+v", escalated)
	}
	note := delegateNote(t, coordinator)
	if !strings.Contains(note, agentflow.RestartIntensityReason) || !strings.Contains(note, "exit_retryable=false") {
		t.Fatalf("the coordinator saw: %s", note)
	}
	coordinators := 0
	processes, _ := f.kernel.Processes()
	for _, process := range processes {
		if process.Role == "agent:coordinator" {
			coordinators++
		}
	}
	if coordinators != 1 {
		t.Fatalf("%d coordinator processes", coordinators)
	}
}

// An empty policy still bounds restarts: the defaults allow 3.
func TestDefaultPolicyIsBounded(t *testing.T) {
	f := newRestartFixture(t)
	worker := &script{}
	for i := 0; i < 10; i++ {
		worker.replies = append(worker.replies, exitStep(1, true, "always fails"))
	}
	coordinator := &script{replies: []func(runtime.GenerateRequest) runtime.GenerateResult{delegateStep("worker"), exitStep(0, false, "done")}}
	engine := agentflow.Engine{Kernel: f.kernel, Artifacts: f.store, Leases: f.leases, Tools: tools.NewRuntime(), Factory: scripts{"worker": worker, "coordinator": coordinator},
		Sleep: func(context.Context, time.Duration) {}}
	if _, err := engine.Run(context.Background(), f.spec(&supervisor.RestartPolicy{}, 8), agentflow.RunRequest{Goal: "go"}); err != nil {
		t.Fatal(err)
	}
	if n := len(f.workers(t)); n != supervisor.DefaultMax+1 {
		t.Fatalf("%d worker processes, want %d", n, supervisor.DefaultMax+1)
	}
}

// Restarts continue the predecessor's usage: the step count goes on from
// the checkpoint, so a worker that used its steps is not restarted, and
// the coordinator sees its exit unchanged.
func TestRestartsChargeTheSameAccount(t *testing.T) {
	f := newRestartFixture(t)
	target := filepath.Join(f.workspace, "a.txt")
	worker := &script{replies: []func(runtime.GenerateRequest) runtime.GenerateResult{
		toolStep("write_file", map[string]string{"path": target, "content": "1"}),
		exitStep(1, true, "first failure"),
		exitStep(1, true, "second failure"),
		exitStep(0, false, "never reached"),
	}}
	coordinator := &script{replies: []func(runtime.GenerateRequest) runtime.GenerateResult{delegateStep("worker"), exitStep(0, false, "done")}}
	f.run(t, context.Background(), f.spec(&supervisor.RestartPolicy{Max: 5, Backoff: "0s"}, 3), scripts{"worker": worker, "coordinator": coordinator})
	if n := len(worker.requests()); n != 3 {
		t.Fatalf("the worker made %d calls, want 3: steps continue across the restart", n)
	}
	workers := f.workers(t)
	if len(workers) != 2 {
		t.Fatalf("%d worker processes, want 2", len(workers))
	}
	skipped := f.events(t, agentflow.NotRestartedEvent)
	if len(skipped) != 1 || skipped[0].Data["cause"] != "budget_spent" || !strings.Contains(skipped[0].Data["detail"], "3 of 3 steps") {
		t.Fatalf("supervisor.not_restarted = %+v", skipped)
	}
	if note := delegateNote(t, coordinator); !strings.Contains(note, "second failure") || !strings.Contains(note, "exit_retryable=true") {
		t.Fatalf("the coordinator saw: %s", note)
	}
}

// A non-retryable exit is final: no restart, no restart events.
func TestNonRetryableExitIsFinal(t *testing.T) {
	f := newRestartFixture(t)
	worker := &script{replies: []func(runtime.GenerateRequest) runtime.GenerateResult{exitStep(2, false, "bad input"), exitStep(0, false, "never")}}
	coordinator := &script{replies: []func(runtime.GenerateRequest) runtime.GenerateResult{delegateStep("worker"), exitStep(0, false, "done")}}
	f.run(t, context.Background(), f.spec(&supervisor.RestartPolicy{Max: 3, Backoff: "0s"}, 8), scripts{"worker": worker, "coordinator": coordinator})
	if len(f.workers(t)) != 1 || len(f.events(t, agentflow.RestartEvent))+len(f.events(t, agentflow.NotRestartedEvent))+len(f.events(t, agentflow.EscalatedEvent)) != 0 {
		t.Fatal("a final exit was supervised")
	}
	if note := delegateNote(t, coordinator); !strings.Contains(note, "bad input") {
		t.Fatalf("the coordinator saw: %s", note)
	}
}

// Without a policy nothing changes: the retryable exit goes to the
// coordinator as it is.
func TestNoPolicyNoRestart(t *testing.T) {
	f := newRestartFixture(t)
	worker := &script{replies: []func(runtime.GenerateRequest) runtime.GenerateResult{exitStep(1, true, "flaky"), exitStep(0, false, "never")}}
	coordinator := &script{replies: []func(runtime.GenerateRequest) runtime.GenerateResult{delegateStep("worker"), exitStep(0, false, "done")}}
	f.run(t, context.Background(), f.spec(nil, 8), scripts{"worker": worker, "coordinator": coordinator})
	if len(f.workers(t)) != 1 || len(f.events(t, agentflow.RestartEvent)) != 0 {
		t.Fatal("restarted without a policy")
	}
	if note := delegateNote(t, coordinator); !strings.Contains(note, "flaky") || !strings.Contains(note, "exit_retryable=true") {
		t.Fatalf("the coordinator saw: %s", note)
	}
}

// Steps 2 and 4: a call the failed attempt started and never finished
// reaches the restarted worker before its first step.
func TestOpenIntentReachesRestartedChild(t *testing.T) {
	f := newRestartFixture(t)
	worker := &script{replies: []func(runtime.GenerateRequest) runtime.GenerateResult{
		func(request runtime.GenerateRequest) runtime.GenerateResult {
			// As if the process recorded an intent and died in the command.
			if _, err := f.kernel.RecordEvent(request.Process.PID, runtime.ToolStartedEvent, map[string]string{"tool": "run", "target": "make deploy"}); err != nil {
				t.Error(err)
			}
			return runtime.GenerateResult{Action: runtime.GenerateAction{Type: "exit", ExitStatus: &kernel.ExitStatus{Code: 1, Reason: "lost the command", Retryable: true}}}
		},
		exitStep(0, false, "checked"),
	}}
	coordinator := &script{replies: []func(runtime.GenerateRequest) runtime.GenerateResult{delegateStep("worker"), exitStep(0, false, "done")}}
	f.run(t, context.Background(), f.spec(&supervisor.RestartPolicy{Backoff: "0s"}, 8), scripts{"worker": worker, "coordinator": coordinator})
	requests := worker.requests()
	if len(requests) != 2 || !strings.Contains(historyText(requests[1]), `tool_interrupted name=run target="make deploy"`) {
		t.Fatalf("the restarted worker's first request: %s", historyText(requests[len(requests)-1]))
	}
}

// Steps 3 and 4: the lease the failed attempt held moves to the restarted
// worker, whose write under it is not fenced.
func TestLeasesTransferOnRestart(t *testing.T) {
	f := newRestartFixture(t)
	target := filepath.Join(f.workspace, "a.txt")
	worker := &script{replies: []func(runtime.GenerateRequest) runtime.GenerateResult{
		toolStep("acquire_lease", map[string]string{"resource": "a.txt", "ttl": "1h"}),
		exitStep(1, true, "flaky"),
		toolStep("write_file", map[string]string{"path": target, "content": "after restart"}),
		exitStep(0, false, "done"),
	}}
	coordinator := &script{replies: []func(runtime.GenerateRequest) runtime.GenerateResult{delegateStep("worker"), exitStep(0, false, "done")}}
	f.run(t, context.Background(), f.spec(&supervisor.RestartPolicy{Backoff: "0s"}, 8), scripts{"worker": worker, "coordinator": coordinator})
	if data, _ := os.ReadFile(target); string(data) != "after restart" {
		t.Fatalf("file = %q", data)
	}
	if fenced := f.events(t, "lease.fenced"); len(fenced) != 0 {
		t.Fatalf("the successor's write was fenced: %+v", fenced)
	}
	if transferred := f.events(t, "lease.transferred"); len(transferred) != 1 || transferred[0].Data["token"] != "2" {
		t.Fatalf("lease.transferred = %+v", transferred)
	}
}

// The backoff is waited before the restart, on the delegating agent's
// time (children run one at a time, inside the delegate call); a run
// stopped during the backoff is not restarted, and its exit stands.
func TestBackoffWaitsAndStopsWithTheRun(t *testing.T) {
	f := newRestartFixture(t)
	worker := &script{replies: []func(runtime.GenerateRequest) runtime.GenerateResult{exitStep(1, true, "flaky"), exitStep(0, false, "done")}}
	coordinator := &script{replies: []func(runtime.GenerateRequest) runtime.GenerateResult{delegateStep("worker"), exitStep(0, false, "done")}}
	start := time.Now()
	f.run(t, context.Background(), f.spec(&supervisor.RestartPolicy{Backoff: "300ms"}, 8), scripts{"worker": worker, "coordinator": coordinator})
	if elapsed := time.Since(start); elapsed < 300*time.Millisecond {
		t.Fatalf("the run took %s; the backoff was not waited", elapsed)
	}
	if restarts := f.events(t, agentflow.RestartEvent); len(restarts) != 1 || restarts[0].Data["backoff"] != "300ms" {
		t.Fatalf("supervisor.restart = %+v", restarts)
	}

	g := newRestartFixture(t)
	worker = &script{replies: []func(runtime.GenerateRequest) runtime.GenerateResult{exitStep(1, true, "flaky"), exitStep(0, false, "done")}}
	coordinator = &script{replies: []func(runtime.GenerateRequest) runtime.GenerateResult{delegateStep("worker"), exitStep(0, false, "done")}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	time.AfterFunc(200*time.Millisecond, cancel)
	engine := agentflow.Engine{Kernel: g.kernel, Artifacts: g.store, Leases: g.leases, Tools: tools.NewRuntime(), Factory: scripts{"worker": worker, "coordinator": coordinator}}
	start = time.Now()
	engine.Run(ctx, g.spec(&supervisor.RestartPolicy{Backoff: "30s"}, 8), agentflow.RunRequest{Goal: "go"})
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("a cancelled run waited out its backoff: %s", elapsed)
	}
	if len(g.workers(t)) != 1 || len(g.events(t, agentflow.RestartEvent)) != 0 {
		t.Fatal("restarted after the run was stopped")
	}
}

// An invalid restart policy is refused with the spec.
func TestInvalidRestartPolicyIsRefused(t *testing.T) {
	f := newRestartFixture(t)
	spec := f.spec(&supervisor.RestartPolicy{Within: "soon"}, 8)
	if err := spec.Validate(); !errors.Is(err, agentflow.ErrWorkflowSpecInvalid) || !strings.Contains(err.Error(), "worker") {
		t.Fatalf("validate: %v", err)
	}
}
