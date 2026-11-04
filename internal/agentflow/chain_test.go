package agentflow_test

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"aos/internal/agentflow"
	"aos/internal/capability"
	"aos/internal/ipc"
	"aos/internal/kernel"
	"aos/internal/runtime"
	"aos/internal/supervisor"
	"aos/internal/tools"
)

// chainSpec is a coordinator delegating to a worker with policy, each able
// to subscribe.
func chainSpec(policy *supervisor.RestartPolicy, maxSteps int) agentflow.WorkflowSpec {
	mail := capability.Set{Tools: []string{"message_agent", "spawn_agent"}}
	return agentflow.WorkflowSpec{
		Name:       "chains",
		EntryAgent: "coordinator",
		Agents: []agentflow.AgentSpec{
			{ID: "coordinator", SystemPrompt: "Delegate.", AllowedAgents: []string{"worker"}, Capabilities: mail},
			{ID: "worker", SystemPrompt: "Work.", MaxSteps: maxSteps, Capabilities: capability.Set{Tools: []string{"message_agent"}}, Restart: policy},
		},
	}
}

func subscribeStep(topic string) func(runtime.GenerateRequest) runtime.GenerateResult {
	return reply(runtime.GenerateResult{Action: runtime.GenerateAction{Type: "subscribe", Subscribe: &runtime.SubscribeCall{Topic: topic}}})
}

type chainFixture struct {
	restartFixture
	bus *ipc.Bus
}

func newChainFixture(t *testing.T) chainFixture {
	t.Helper()
	f := newRestartFixture(t)
	bus, err := ipc.New(filepath.Join(t.TempDir(), "bus"), f.kernel)
	if err != nil {
		t.Fatal(err)
	}
	return chainFixture{restartFixture: f, bus: bus}
}

func (f chainFixture) engine(backends scripts) agentflow.Engine {
	return agentflow.Engine{Kernel: f.kernel, Artifacts: f.store, Leases: f.leases, Bus: f.bus, Tools: tools.NewRuntime(), Factory: backends,
		Sleep: func(context.Context, time.Duration) {}}
}

func (f chainFixture) lastWorker(t *testing.T) kernel.Process {
	t.Helper()
	workers := f.workers(t)
	slices.SortFunc(workers, func(a, b kernel.Process) int { return strings.Compare(a.PID, b.PID) })
	if len(workers) == 0 {
		t.Fatal("no worker")
	}
	return workers[len(workers)-1]
}

// Who closes: the engine, as the worker's supervisor, when it escalates. The
// worker's chain ends, and only the worker's: its subscriptions go, its unread
// mail stays, and its delegator is told and goes on with its own.
func TestEscalationClosesTheChildsChain(t *testing.T) {
	f := newChainFixture(t)
	var lastAttempt string
	worker := &script{replies: []func(runtime.GenerateRequest) runtime.GenerateResult{
		subscribeStep("news"),
		exitStep(1, true, "fails"),
		func(request runtime.GenerateRequest) runtime.GenerateResult {
			lastAttempt = request.Process.PID
			// Arrives in this step, is never received.
			f.bus.Send("pid-000001", request.Process.PID, "unread at the end")
			return exitStep(1, true, "fails again")(request)
		},
	}}
	coordinator := &script{replies: []func(runtime.GenerateRequest) runtime.GenerateResult{subscribeStep("news"), delegateStep("worker"), exitStep(0, false, "done")}}
	engine := f.engine(scripts{"worker": worker, "coordinator": coordinator})
	if _, err := engine.Run(context.Background(), chainSpec(&supervisor.RestartPolicy{Max: 1, Within: "1m", Backoff: "0s"}, 30), agentflow.RunRequest{Goal: "go"}); err != nil {
		t.Fatal(err)
	}
	if len(f.events(t, agentflow.EscalatedEvent)) != 1 {
		t.Fatalf("supervisor.escalated = %+v", f.events(t, agentflow.EscalatedEvent))
	}
	last, err := f.kernel.Get(lastAttempt)
	if err != nil {
		t.Fatal(err)
	}
	if !last.Closed || !last.ChainEnded() {
		t.Fatalf("the escalated process = %+v", last)
	}
	closed := f.events(t, "process.closed")
	if len(closed) != 1 || closed[0].ProcessID != lastAttempt || !strings.Contains(closed[0].Data["reason"], agentflow.RestartIntensityReason) {
		t.Fatalf("process.closed = %+v", closed)
	}
	if topics, _ := f.bus.Subscriptions(lastAttempt); len(topics) != 0 {
		t.Fatalf("its topics = %v", topics)
	}
	pending, _ := f.bus.Pending()
	if box := pending[lastAttempt]; len(box) != 1 || box[0].Body != "unread at the end" {
		t.Fatalf("its mail should stay for step 3's dead letter: %+v", pending)
	}
	// A send to any PID of the chain is refused from now on, and Recover too.
	first := f.workers(t)[0].PID
	if _, err := f.bus.Send("pid-000001", first, "late"); !errors.Is(err, ipc.ErrRecipientExited) || !strings.Contains(err.Error(), lastAttempt) {
		t.Fatalf("send = %v", err)
	}
	agent := &runtime.AgentRuntime{Kernel: f.kernel, Artifacts: f.store, Bus: f.bus, Backend: worker}
	if _, err := agent.Recover(context.Background(), lastAttempt, "", nil, runtime.RunOptions{}); !errors.Is(err, runtime.ErrNotRecoverable) {
		t.Fatalf("recover = %v", err)
	}
	// The delegator's run is not the child's: it was closed by nobody, its own
	// exit 0 ended its own chain.
	processes, _ := f.kernel.Processes()
	for _, process := range processes {
		if process.Role == "agent:coordinator" && (process.Closed || process.ExitStatus == nil || process.ExitStatus.Code != 0) {
			t.Fatalf("coordinator = %+v", process)
		}
	}
}

// A budget-stopped child is closed by the same supervisor. Its budget is spent,
// so nothing could run it again anyway.
func TestSpentBudgetClosesTheChain(t *testing.T) {
	f := newChainFixture(t)
	worker := &script{replies: []func(runtime.GenerateRequest) runtime.GenerateResult{
		subscribeStep("news"), exitStep(1, true, "fails"),
	}}
	coordinator := &script{replies: []func(runtime.GenerateRequest) runtime.GenerateResult{delegateStep("worker"), exitStep(0, false, "done")}}
	engine := f.engine(scripts{"worker": worker, "coordinator": coordinator})
	if _, err := engine.Run(context.Background(), chainSpec(&supervisor.RestartPolicy{Max: 5, Backoff: "0s"}, 2), agentflow.RunRequest{Goal: "go"}); err != nil {
		t.Fatal(err)
	}
	skipped := f.events(t, agentflow.NotRestartedEvent)
	if len(skipped) != 1 || skipped[0].Data["cause"] != "budget_spent" {
		t.Fatalf("supervisor.not_restarted = %+v", skipped)
	}
	process, _ := f.kernel.Get(skipped[0].ProcessID)
	if !process.Closed {
		t.Fatalf("the worker = %+v", process)
	}
	if topics, _ := f.bus.Subscriptions(process.PID); len(topics) != 0 {
		t.Fatalf("its topics = %v", topics)
	}
}

// Every decline of the policy closes, unless a successor exists. The hazard
// named in ipc-design.md: an exit the engine will not restart must not leave
// its chain open.
func TestNotRecoverableClosesOnlyAChainWithNoSuccessor(t *testing.T) {
	cases := []struct {
		name   string
		during func(t *testing.T, f chainFixture, worker string, backend *script)
		closed bool
	}{
		{"another process recovered it first", func(t *testing.T, f chainFixture, worker string, backend *script) {
			if _, err := f.kernel.Supersede(worker, kernel.ProcessSpec{Role: "agent:worker", Metadata: map[string]string{"resumed_from": worker}}, nil); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"an operator closed it in the backoff", func(t *testing.T, f chainFixture, worker string, backend *script) {
			if _, err := f.kernel.Close(worker, "operator"); err != nil {
				t.Fatal(err)
			}
		}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newChainFixture(t)
			worker := &script{replies: []func(runtime.GenerateRequest) runtime.GenerateResult{exitStep(1, true, "fails")}}
			coordinator := &script{replies: []func(runtime.GenerateRequest) runtime.GenerateResult{delegateStep("worker"), exitStep(0, false, "done")}}
			engine := f.engine(scripts{"worker": worker, "coordinator": coordinator})
			engine.Sleep = func(context.Context, time.Duration) { c.during(t, f, f.workers(t)[0].PID, worker) }
			if _, err := engine.Run(context.Background(), chainSpec(&supervisor.RestartPolicy{Max: 5, Backoff: "1s"}, 30), agentflow.RunRequest{Goal: "go"}); err != nil {
				t.Fatal(err)
			}
			skipped := f.events(t, agentflow.NotRestartedEvent)
			if len(skipped) != 1 || skipped[0].Data["cause"] != "not_recoverable" {
				t.Fatalf("supervisor.not_restarted = %+v", skipped)
			}
			process, _ := f.kernel.Get(skipped[0].ProcessID)
			if process.Closed != c.closed {
				t.Fatalf("worker = %+v, want closed=%t", process, c.closed)
			}
		})
	}
}

// With no restart policy nothing declined the exit, so the chain stays open
// for an operator's Recover, as does a child abandoned when its delegator
// finished. Its subscriptions are kept and its mail is held: the known limit.
func TestAnExitNobodyDeclinedStaysOpen(t *testing.T) {
	f := newChainFixture(t)
	worker := &script{replies: []func(runtime.GenerateRequest) runtime.GenerateResult{
		subscribeStep("news"), exitStep(1, true, "fails"),
	}}
	coordinator := &script{replies: []func(runtime.GenerateRequest) runtime.GenerateResult{delegateStep("worker"), exitStep(0, false, "done")}}
	engine := f.engine(scripts{"worker": worker, "coordinator": coordinator})
	if _, err := engine.Run(context.Background(), chainSpec(nil, 30), agentflow.RunRequest{Goal: "go"}); err != nil {
		t.Fatal(err)
	}
	process := f.lastWorker(t)
	if process.Closed || process.ChainEnded() {
		t.Fatalf("worker = %+v", process)
	}
	if topics, _ := f.bus.Subscriptions(process.PID); !slices.Equal(topics, []string{"news"}) {
		t.Fatalf("its topics = %v", topics)
	}
	if message, err := f.bus.Send("pid-000001", process.PID, "held"); err != nil || message.To != process.PID {
		t.Fatalf("send = %+v, %v", message, err)
	}
	if closed := f.events(t, "process.closed"); len(closed) != 0 {
		t.Fatalf("process.closed = %+v", closed)
	}
}

// A retryable exit the policy restarts keeps the chain's subscription, and the
// publish in its backoff reaches the successor.
func TestRestartKeepsSubscriptionsAcrossBackoff(t *testing.T) {
	f := newChainFixture(t)
	worker := &script{replies: []func(runtime.GenerateRequest) runtime.GenerateResult{
		subscribeStep("news"), exitStep(1, true, "fails"), exitStep(0, false, "done"),
	}}
	coordinator := &script{replies: []func(runtime.GenerateRequest) runtime.GenerateResult{delegateStep("worker"), exitStep(0, false, "done")}}
	engine := f.engine(scripts{"worker": worker, "coordinator": coordinator})
	published := 0
	engine.Sleep = func(context.Context, time.Duration) {
		created, err := f.bus.Publish("pid-000001", "news", "in the backoff")
		if err != nil {
			t.Error(err)
		}
		published = len(created)
	}
	if _, err := engine.Run(context.Background(), chainSpec(&supervisor.RestartPolicy{Max: 5, Backoff: "1s"}, 30), agentflow.RunRequest{Goal: "go"}); err != nil {
		t.Fatal(err)
	}
	if published != 1 {
		t.Fatalf("the publish in the backoff reached %d", published)
	}
	requests := worker.requests()
	if !strings.Contains(historyText(requests[2]), "in the backoff") {
		t.Fatalf("the successor's first request: %s", historyText(requests[2]))
	}
}
