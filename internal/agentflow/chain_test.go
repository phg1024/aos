package agentflow_test

import (
	"context"
	"errors"
	"fmt"
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
// mail becomes dead letters, and its delegator is told and goes on with its own.
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
	if len(pending) != 0 {
		t.Fatalf("nothing should be left: %+v", pending)
	}
	letters := f.events(t, ipc.DeadLetterEvent)
	if len(letters) != 1 || letters[0].ProcessID != lastAttempt || letters[0].Data["reason"] != ipc.ReasonUnread || letters[0].Data["closed"] != "true" {
		t.Fatalf("ipc.dead_letter = %+v", letters)
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
		created, _, err := f.bus.Publish("pid-000001", "news", "in the backoff")
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

// A parent's mail a delegated child never read: the child's exit 0 ends its
// chain, and the parent hears at its next step, after the delegate result it
// was waiting on. Nothing is left, and the child's exit is untouched.
func TestParentHearsOfUnreadMailAfterTheDelegateResult(t *testing.T) {
	f := newChainFixture(t)
	var parent string
	worker := &script{replies: []func(runtime.GenerateRequest) runtime.GenerateResult{
		func(request runtime.GenerateRequest) runtime.GenerateResult {
			if _, err := f.bus.Send(parent, request.Process.PID, "arrives as the child finishes"); err != nil {
				t.Error(err)
			}
			return exitStep(0, false, "finished")(request)
		},
	}}
	coordinator := &script{replies: []func(runtime.GenerateRequest) runtime.GenerateResult{
		func(request runtime.GenerateRequest) runtime.GenerateResult {
			parent = request.Process.PID
			return delegateStep("worker")(request)
		},
		exitStep(0, false, "done"),
	}}
	engine := f.engine(scripts{"worker": worker, "coordinator": coordinator})
	if _, err := engine.Run(context.Background(), chainSpec(nil, 30), agentflow.RunRequest{Goal: "go"}); err != nil {
		t.Fatal(err)
	}
	requests := coordinator.requests()
	history := historyText(requests[1])
	result, notice := strings.Index(history, "delegate_result"), strings.Index(history, "undelivered:")
	if result < 0 || notice < result {
		t.Fatalf("the parent's next request should hold the delegate result, then the notice:\n%s", history)
	}
	if !strings.Contains(history, `its chain ended with exit code 0 "finished"`) {
		t.Fatalf("history:\n%s", history)
	}
	if pending, _ := f.bus.Pending(); len(pending) != 0 {
		t.Fatalf("left pending: %+v", pending)
	}
}

// A chain nobody closes has no outcome: a sender's held mail just waits, and
// the sender has only the held on its send result. This is the stated limit.
func TestAChainNobodyClosesProducesNoOutcome(t *testing.T) {
	f := newChainFixture(t)
	worker := &script{replies: []func(runtime.GenerateRequest) runtime.GenerateResult{exitStep(1, true, "fails")}}
	coordinator := &script{replies: []func(runtime.GenerateRequest) runtime.GenerateResult{delegateStep("worker"), exitStep(0, false, "done")}}
	engine := f.engine(scripts{"worker": worker, "coordinator": coordinator})
	if _, err := engine.Run(context.Background(), chainSpec(nil, 30), agentflow.RunRequest{Goal: "go"}); err != nil {
		t.Fatal(err)
	}
	process := f.lastWorker(t)
	message, err := f.bus.Send("pid-000001", process.PID, "held")
	if err != nil || !message.Held {
		t.Fatalf("send = %+v, %v", message, err)
	}
	if err := f.bus.Sweep(); err != nil {
		t.Fatal(err)
	}
	if letters := f.events(t, ipc.DeadLetterEvent); len(letters) != 0 {
		t.Fatalf("ipc.dead_letter = %+v", letters)
	}
	if pending, _ := f.bus.Pending(); len(pending[process.PID]) != 1 {
		t.Fatalf("pending = %+v", pending)
	}
}

// handleSpec is a lead delegating to a worker and a verifier, siblings of each other.
func handleSpec() agentflow.WorkflowSpec {
	return agentflow.WorkflowSpec{
		Name:       "handles",
		EntryAgent: "lead",
		Agents: []agentflow.AgentSpec{
			{ID: "lead", SystemPrompt: "Lead.", AllowedAgents: []string{"worker", "verifier"}, Capabilities: capability.Set{Tools: []string{"message_agent", "spawn_agent", "publish_artifact", "list_artifacts"}}},
			{ID: "worker", SystemPrompt: "Work.", Capabilities: capability.Set{Tools: []string{"publish_artifact"}}},
			{ID: "verifier", SystemPrompt: "Check.", Capabilities: capability.Set{Tools: []string{"list_artifacts"}}},
		},
	}
}

func handleTool(name string, args func(runtime.GenerateRequest) map[string]string) func(runtime.GenerateRequest) runtime.GenerateResult {
	return func(request runtime.GenerateRequest) runtime.GenerateResult {
		return runtime.GenerateResult{Action: runtime.GenerateAction{Type: "tool", ToolCall: &runtime.ToolCall{Name: name, Args: args(request)}}}
	}
}

func artifactIn(request runtime.GenerateRequest, opening string) string {
	for i := len(request.History) - 1; i >= 0; i-- {
		if strings.HasPrefix(request.History[i].Content, opening) {
			if at := strings.Index(request.History[i].Content, "art-"); at >= 0 {
				return request.History[i].Content[at : at+len("art-000000")]
			}
		}
	}
	return ""
}

// Chapter 11's verifier is a sibling of the worker, so no read rule covers it.
// Today's route: the lead reads the worker's evidence as its parent, and has a PID
// for the verifier only once it parks, since delegate takes no handle. So the
// verifier waits, the lead attaches the handle, and re-delegating resumes it. A
// child reads a parent's artifact the same way, and a child that never parks has
// no route.
func TestAVerifierGetsEvidenceThroughAWait(t *testing.T) {
	f := newChainFixture(t)
	var evidence, verifierPID string
	worker := &script{replies: []func(runtime.GenerateRequest) runtime.GenerateResult{
		handleTool("publish_artifact", func(runtime.GenerateRequest) map[string]string {
			return map[string]string{"type": "test_log", "body": "all 12 tests passed"}
		}),
		exitStep(0, false, "work done"),
	}}
	var unattachedRead, attachedRead string
	verifier := &script{replies: []func(runtime.GenerateRequest) runtime.GenerateResult{
		func(request runtime.GenerateRequest) runtime.GenerateResult {
			verifierPID = request.Process.PID
			return handleTool("read_artifact", func(runtime.GenerateRequest) map[string]string { return map[string]string{"id": evidence} })(request)
		},
		func(request runtime.GenerateRequest) runtime.GenerateResult {
			unattachedRead = historyText(request)
			return runtime.GenerateResult{Action: runtime.GenerateAction{Type: "wait", Message: "send the evidence"}}
		},
		handleTool("read_artifact", func(runtime.GenerateRequest) map[string]string { return map[string]string{"id": evidence} }),
		func(request runtime.GenerateRequest) runtime.GenerateResult {
			attachedRead = historyText(request)
			return exitStep(0, false, "verified")(request)
		},
	}}
	lead := &script{replies: []func(runtime.GenerateRequest) runtime.GenerateResult{
		delegateStep("worker"),
		handleTool("list_artifacts", func(runtime.GenerateRequest) map[string]string {
			return map[string]string{"type": "published.test_log"}
		}),
		func(request runtime.GenerateRequest) runtime.GenerateResult {
			evidence = artifactIn(request, "list_artifacts:")
			return handleTool("read_artifact", func(runtime.GenerateRequest) map[string]string { return map[string]string{"id": evidence} })(request)
		},
		delegateStep("verifier"),
		handleTool("message_agent", func(runtime.GenerateRequest) map[string]string {
			return map[string]string{"to": verifierPID, "body": "the test log", "artifacts": evidence}
		}),
		delegateStep("verifier"),
		exitStep(0, false, "done"),
	}}
	engine := f.engine(scripts{"worker": worker, "verifier": verifier, "lead": lead})
	if _, err := engine.Run(context.Background(), handleSpec(), agentflow.RunRequest{Goal: "go"}); err != nil {
		t.Fatal(err)
	}
	if evidence == "" {
		t.Fatal("the lead could not list its child's evidence")
	}
	if requests := lead.requests(); !strings.Contains(historyText(requests[3]), "all 12 tests passed") {
		t.Fatalf("the lead could not read it:\n%s", historyText(requests[3]))
	}
	if !strings.Contains(unattachedRead, "tool_error name=read_artifact") || strings.Contains(unattachedRead, "all 12 tests passed") {
		t.Fatalf("a sibling read it with no handle:\n%s", unattachedRead)
	}
	if !strings.Contains(attachedRead, "all 12 tests passed") {
		t.Fatalf("the verifier could not read the handle:\n%s", attachedRead)
	}
}

// failingFor refuses a backend for one agent once it has been asked count times,
// and otherwise is scripts.
type failingFor struct {
	scripts
	agent string
	after int
	asked *int
}

func (f failingFor) BackendFor(agent agentflow.AgentSpec, spec agentflow.WorkflowSpec) (runtime.Backend, error) {
	if agent.ID == f.agent {
		*f.asked++
		if *f.asked > f.after {
			return nil, errors.New("the model is not configured")
		}
	}
	return f.scripts.BackendFor(agent, spec)
}

// reDelegate is a coordinator delegating to the worker twice, then exiting. between
// runs at its second step, with the first worker's PID.
func reDelegate(between func(first string)) *script {
	return &script{replies: []func(runtime.GenerateRequest) runtime.GenerateResult{
		delegateStep("worker"),
		func(request runtime.GenerateRequest) runtime.GenerateResult {
			if between != nil {
				between(lastDelegatedPID(request))
			}
			return delegateStep("worker")(request)
		},
		exitStep(0, false, "done"),
	}}
}

func lastDelegatedPID(request runtime.GenerateRequest) string {
	text := historyText(request)
	at := strings.LastIndex(text, "pid=")
	if at < 0 {
		return ""
	}
	return strings.Fields(text[at+len("pid="):])[0]
}

// The case the user decided: a re-delegation after a retryable exit, no restart
// policy. The old chain closes before the new child's first step, its mail becomes
// dead letters and does not move, and its senders hear of it.
func TestReDelegationClosesTheOldChain(t *testing.T) {
	f := newChainFixture(t)
	var first, parent, second string
	var closedAtFirstStep, firstRequest string
	sweptAtFirstStep := 0
	worker := &script{replies: []func(runtime.GenerateRequest) runtime.GenerateResult{
		subscribeStep("news"),
		exitStep(1, true, "fails"),
		func(request runtime.GenerateRequest) runtime.GenerateResult {
			second = request.Process.PID
			firstRequest = historyText(request)
			old, err := f.kernel.Get(first)
			if err != nil {
				t.Error(err)
			}
			closedAtFirstStep = fmt.Sprint(old.Closed)
			sweptAtFirstStep = len(f.events(t, ipc.DeadLetterEvent))
			return exitStep(0, false, "second worker done")(request)
		},
	}}
	coordinator := reDelegate(func(pid string) {
		first = pid
		if _, err := f.bus.Send(parent, pid, "for the first worker"); err != nil {
			t.Error(err)
		}
	})
	inner := coordinator.replies[0]
	coordinator.replies[0] = func(request runtime.GenerateRequest) runtime.GenerateResult {
		parent = request.Process.PID
		return inner(request)
	}
	engine := f.engine(scripts{"worker": worker, "coordinator": coordinator})
	if _, err := engine.Run(context.Background(), chainSpec(nil, 30), agentflow.RunRequest{Goal: "go"}); err != nil {
		t.Fatal(err)
	}
	if closedAtFirstStep != "true" {
		t.Fatalf("at the new child's first step the old chain closed = %q", closedAtFirstStep)
	}
	// The new child's first step sweeps, as any process's step does.
	if sweptAtFirstStep != 1 {
		t.Fatalf("dead letters at the new child's first step = %d", sweptAtFirstStep)
	}
	old, err := f.kernel.Get(first)
	if err != nil {
		t.Fatal(err)
	}
	if !old.Closed || !old.ChainEnded() {
		t.Fatalf("old = %+v", old)
	}
	closed := f.events(t, "process.closed")
	if len(closed) != 1 || closed[0].ProcessID != first || closed[0].Data["reason"] != "re-delegated as "+second {
		t.Fatalf("process.closed = %+v", closed)
	}
	// Not a Transfer: the new child never holds it.
	if strings.Contains(firstRequest, "for the first worker") {
		t.Fatalf("the mail moved to the new child:\n%s", firstRequest)
	}
	letters := f.events(t, ipc.DeadLetterEvent)
	if len(letters) != 1 || letters[0].Data["to"] != first || letters[0].Data["closed"] != "true" {
		t.Fatalf("ipc.dead_letter = %+v", letters)
	}
	if topics, _ := f.bus.Subscriptions(first); len(topics) != 0 {
		t.Fatalf("its subscriptions stayed: %v", topics)
	}
	if topics, _ := f.bus.Subscriptions(second); len(topics) != 0 {
		t.Fatalf("its subscriptions moved: %v", topics)
	}
	// The delegator hears at its next step, after the new child's result.
	requests := coordinator.requests()
	history := historyText(requests[2])
	result, notice := strings.LastIndex(history, "delegate_result"), strings.Index(history, "undelivered:")
	if notice < 0 || notice < result || !strings.Contains(history, `"fails", closed`) {
		t.Fatalf("the delegator's next request:\n%s", history)
	}
	if pending, _ := f.bus.Pending(); len(pending) != 0 {
		t.Fatalf("pending: %+v", pending)
	}
	// An address is a chain, not an agent: the old PID is refused, not rerouted, and
	// an agent's name is no address.
	if _, err := f.bus.Send(parent, first, "too late"); !errors.Is(err, ipc.ErrRecipientExited) || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("send to the old PID = %v", err)
	}
	if _, err := f.bus.Send(parent, "worker", "by name"); !errors.Is(err, ipc.ErrUnknownRecipient) {
		t.Fatalf("send to the name = %v", err)
	}
	// An operator's Recover of that exit is refused.
	agent := &runtime.AgentRuntime{Kernel: f.kernel, Artifacts: f.store, Bus: f.bus, Tools: tools.NewRuntime(), Backend: worker}
	if _, err := agent.Recover(context.Background(), first, old.ExitStatus.Checkpoint, nil, runtime.RunOptions{}); !errors.Is(err, kernel.ErrChainClosed) || !errors.Is(err, runtime.ErrNotRecoverable) {
		t.Fatalf("Recover = %v", err)
	}
}

// A new child that cannot start has replaced nothing, so the old chain stays open.
func TestAFailedStartLeavesTheOldChainOpen(t *testing.T) {
	f := newChainFixture(t)
	worker := &script{replies: []func(runtime.GenerateRequest) runtime.GenerateResult{exitStep(1, true, "fails")}}
	asked := 0
	engine := f.engine(nil)
	engine.Factory = failingFor{scripts: scripts{"worker": worker, "coordinator": reDelegate(nil)}, agent: "worker", after: 1, asked: &asked}
	// The delegation's failure ends the delegator's run, as any such error does.
	if _, err := engine.Run(context.Background(), chainSpec(nil, 30), agentflow.RunRequest{Goal: "go"}); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("run = %v", err)
	}
	if asked != 2 {
		t.Fatalf("asked for a backend %d times", asked)
	}
	if closed := f.events(t, "process.closed"); len(closed) != 0 {
		t.Fatalf("process.closed = %+v", closed)
	}
	workers := f.workers(t)
	slices.SortFunc(workers, func(a, b kernel.Process) int { return strings.Compare(a.PID, b.PID) })
	if workers[0].Closed || workers[0].ChainEnded() {
		t.Fatalf("the old worker = %+v", workers[0])
	}
}

// Only this case. Each of these leaves the first worker's chain as it was.
func TestWhatADelegationDoesNotClose(t *testing.T) {
	noneClosed := func(t *testing.T, f chainFixture) {
		t.Helper()
		if closed := f.events(t, "process.closed"); len(closed) != 0 {
			t.Fatalf("process.closed = %+v", closed)
		}
	}
	t.Run("the first delegation", func(t *testing.T) {
		f := newChainFixture(t)
		worker := &script{replies: []func(runtime.GenerateRequest) runtime.GenerateResult{exitStep(1, true, "fails")}}
		coordinator := &script{replies: []func(runtime.GenerateRequest) runtime.GenerateResult{delegateStep("worker"), exitStep(0, false, "done")}}
		engine := f.engine(scripts{"worker": worker, "coordinator": coordinator})
		if _, err := engine.Run(context.Background(), chainSpec(nil, 30), agentflow.RunRequest{Goal: "go"}); err != nil {
			t.Fatal(err)
		}
		noneClosed(t, f)
	})
	t.Run("a final exit", func(t *testing.T) {
		f := newChainFixture(t)
		worker := &script{replies: []func(runtime.GenerateRequest) runtime.GenerateResult{exitStep(1, false, "fails for good"), exitStep(0, false, "done")}}
		engine := f.engine(scripts{"worker": worker, "coordinator": reDelegate(nil)})
		if _, err := engine.Run(context.Background(), chainSpec(nil, 30), agentflow.RunRequest{Goal: "go"}); err != nil {
			t.Fatal(err)
		}
		noneClosed(t, f)
	})
	t.Run("a retryable exit already recovered", func(t *testing.T) {
		f := newChainFixture(t)
		worker := &script{replies: []func(runtime.GenerateRequest) runtime.GenerateResult{
			reply(runtime.GenerateResult{Action: runtime.GenerateAction{Type: "message", Message: "a checkpoint"}}),
			exitStep(1, true, "fails"),
			exitStep(0, false, "recovered and done"),
			exitStep(0, false, "second done"),
		}}
		var recovered runtime.RunResult
		coordinator := reDelegate(func(pid string) {
			old, err := f.kernel.Get(pid)
			if err != nil {
				t.Error(err)
				return
			}
			agent := &runtime.AgentRuntime{Kernel: f.kernel, Artifacts: f.store, Bus: f.bus, Tools: tools.NewRuntime(), Backend: worker}
			if recovered, err = agent.Recover(context.Background(), pid, old.ExitStatus.Checkpoint, nil, runtime.RunOptions{}); err != nil {
				t.Error(err)
			}
		})
		engine := f.engine(scripts{"worker": worker, "coordinator": coordinator})
		if _, err := engine.Run(context.Background(), chainSpec(nil, 30), agentflow.RunRequest{Goal: "go"}); err != nil {
			t.Fatal(err)
		}
		if recovered.ExitStatus.Code != 0 {
			t.Fatalf("recovered = %+v", recovered.ExitStatus)
		}
		noneClosed(t, f)
	})
	t.Run("another agent", func(t *testing.T) {
		f := newChainFixture(t)
		spec := chainSpec(nil, 30)
		spec.Agents[0].AllowedAgents = []string{"worker", "other"}
		spec.Agents = append(spec.Agents, agentflow.AgentSpec{ID: "other", SystemPrompt: "Other."})
		worker := &script{replies: []func(runtime.GenerateRequest) runtime.GenerateResult{exitStep(1, true, "fails")}}
		other := &script{replies: []func(runtime.GenerateRequest) runtime.GenerateResult{exitStep(0, false, "done")}}
		coordinator := &script{replies: []func(runtime.GenerateRequest) runtime.GenerateResult{delegateStep("worker"), delegateStep("other"), exitStep(0, false, "done")}}
		engine := f.engine(scripts{"worker": worker, "other": other, "coordinator": coordinator})
		if _, err := engine.Run(context.Background(), spec, agentflow.RunRequest{Goal: "go"}); err != nil {
			t.Fatal(err)
		}
		noneClosed(t, f)
	})
	t.Run("another delegator's child", func(t *testing.T) {
		f := newChainFixture(t)
		spec := chainSpec(nil, 30)
		spec.Agents[0].AllowedAgents = []string{"worker", "middle"}
		spec.Agents = append(spec.Agents, agentflow.AgentSpec{ID: "middle", SystemPrompt: "Middle.", AllowedAgents: []string{"worker"}, Capabilities: capability.Set{Tools: []string{"message_agent", "spawn_agent"}}})
		worker := &script{replies: []func(runtime.GenerateRequest) runtime.GenerateResult{exitStep(1, true, "fails"), exitStep(0, false, "done")}}
		middle := &script{replies: []func(runtime.GenerateRequest) runtime.GenerateResult{delegateStep("worker"), exitStep(0, false, "done")}}
		coordinator := &script{replies: []func(runtime.GenerateRequest) runtime.GenerateResult{delegateStep("middle"), delegateStep("worker"), exitStep(0, false, "done")}}
		engine := f.engine(scripts{"worker": worker, "middle": middle, "coordinator": coordinator})
		if _, err := engine.Run(context.Background(), spec, agentflow.RunRequest{Goal: "go"}); err != nil {
			t.Fatal(err)
		}
		noneClosed(t, f)
	})
	t.Run("the marker off", func(t *testing.T) {
		f := newChainFixture(t)
		worker := &script{replies: []func(runtime.GenerateRequest) runtime.GenerateResult{exitStep(1, true, "fails"), exitStep(0, false, "done")}}
		engine := f.engine(scripts{"worker": worker, "coordinator": reDelegate(nil)})
		engine.NoRedelegationClose = true
		if _, err := engine.Run(context.Background(), chainSpec(nil, 30), agentflow.RunRequest{Goal: "go"}); err != nil {
			t.Fatal(err)
		}
		noneClosed(t, f)
	})
}

// runThenRecover has a delegator's first run end with a worker's retryable exit
// left open, then recovers the delegator under spec, and it delegates again.
func runThenRecover(t *testing.T, f chainFixture, spec agentflow.WorkflowSpec, firstWorker runtime.GenerateResult) kernel.Process {
	t.Helper()
	var worker string
	workers := &script{replies: []func(runtime.GenerateRequest) runtime.GenerateResult{
		func(request runtime.GenerateRequest) runtime.GenerateResult {
			worker = request.Process.PID
			return firstWorker
		},
		exitStep(0, false, "second done"),
	}}
	lead := &script{replies: []func(runtime.GenerateRequest) runtime.GenerateResult{
		delegateStep("worker"),
		exitStep(1, true, "the delegator fails"),
		delegateStep("worker"),
		exitStep(0, false, "done"),
	}}
	engine := f.engine(scripts{"worker": workers, "coordinator": lead})
	if first, err := engine.Run(context.Background(), chainSpec(nil, 30), agentflow.RunRequest{Goal: "go"}); err == nil && first.ExitStatus.Code == 0 {
		t.Fatalf("the first run = %+v", first)
	}
	var delegator kernel.Process
	processes, _ := f.kernel.Processes()
	for _, process := range processes {
		if process.Metadata["agent_id"] == "coordinator" {
			delegator = process
		}
	}
	coordinator, _ := spec.Agent("coordinator")
	runner := &runtime.AgentRuntime{Kernel: f.kernel, Artifacts: f.store, Bus: f.bus, Tools: tools.NewRuntime(), Backend: lead}
	runner.DelegateHandler = engine.DelegateHandler(spec, agentflow.RunRequest{Goal: "go"}, coordinator, nil)
	if _, err := runner.Recover(context.Background(), delegator.PID, delegator.ExitStatus.Checkpoint, nil, runtime.RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := len(lead.requests()); got != 4 {
		t.Fatalf("the delegator took %d steps", got)
	}
	old, err := f.kernel.Get(worker)
	if err != nil {
		t.Fatal(err)
	}
	return old
}

// A delegator recovered after the first worker's exit still replaces it: the old
// child's ParentPID names the delegator's old process.
func TestARecoveredDelegatorStillClosesItsOldChild(t *testing.T) {
	f := newChainFixture(t)
	if old := runThenRecover(t, f, chainSpec(nil, 30), exitStep(1, true, "fails")(runtime.GenerateRequest{})); !old.Closed {
		t.Fatalf("the old child = %+v", old)
	}
}

// An agent with a restart policy is left to it, and an exit the policy left open
// keeps an operator's Recover.
func TestARestartPolicyKeepsTheOldChainOpen(t *testing.T) {
	f := newChainFixture(t)
	if old := runThenRecover(t, f, chainSpec(&supervisor.RestartPolicy{Max: 1, Backoff: "0s"}, 30), exitStep(1, true, "fails")(runtime.GenerateRequest{})); old.Closed {
		t.Fatalf("the old child = %+v", old)
	}
}

// An abandoned child, whose delegator ended before it could answer, stays open too:
// an operator's Recover is why it is retryable.
func TestAnAbandonedChildStaysOpen(t *testing.T) {
	f := newChainFixture(t)
	waits := runtime.GenerateResult{Action: runtime.GenerateAction{Type: "wait", Message: "a question"}}
	old := runThenRecover(t, f, chainSpec(nil, 30), waits)
	if old.ExitStatus == nil || old.ExitStatus.Reason != agentflow.AbandonedReason || old.Closed {
		t.Fatalf("the old child = %+v", old)
	}
}

// A crash just after the close. The state directory holds a closed chain, its mail
// and a new child that took no step; the next run's first step sweeps the mail, and
// nothing is lost or moved. Simulated: the new child's backend panics at its first
// call, which the sweep precedes, so this also shows the kept body.
func TestACrashAfterTheCloseLeavesAStateTheNextRunHandles(t *testing.T) {
	f := newChainFixture(t)
	var first string
	worker := &script{replies: []func(runtime.GenerateRequest) runtime.GenerateResult{
		func(request runtime.GenerateRequest) runtime.GenerateResult {
			first = request.Process.PID
			return exitStep(1, true, "fails")(request)
		},
		func(runtime.GenerateRequest) runtime.GenerateResult { panic("killed") },
	}}
	coordinator := reDelegate(func(pid string) {
		if _, err := f.bus.Send("pid-000001", pid, "held for the first worker"); err != nil {
			t.Error(err)
		}
	})
	engine := f.engine(scripts{"worker": worker, "coordinator": coordinator})
	func() {
		defer func() { recover() }()
		engine.Run(context.Background(), chainSpec(nil, 30), agentflow.RunRequest{Goal: "go"})
	}()
	old, err := f.kernel.Get(first)
	if err != nil || !old.Closed {
		t.Fatalf("old = %+v, %v", old, err)
	}
	// Nothing moved to the new child, and the next run goes on.
	next := &script{replies: []func(runtime.GenerateRequest) runtime.GenerateResult{exitStep(0, false, "done")}}
	again := f.engine(scripts{"coordinator": next})
	// The coordinator alone: the spec contract refuses an agent nobody
	// reaches, and spawn_agent with no edge.
	spec := chainSpec(nil, 30)
	spec.Agents = spec.Agents[:1]
	spec.Agents[0].AllowedAgents = nil
	spec.Agents[0].Capabilities = capability.Set{Tools: []string{"message_agent"}}
	if _, err := again.Run(context.Background(), spec, agentflow.RunRequest{Goal: "again"}); err != nil {
		t.Fatal(err)
	}
	letters := f.events(t, ipc.DeadLetterEvent)
	if len(letters) != 1 || letters[0].Data["to"] != first {
		t.Fatalf("ipc.dead_letter = %+v", letters)
	}
	body, err := f.bus.DeadLetterBody(letters[0].Data["id"])
	if err != nil || body.Body != "held for the first worker" {
		t.Fatalf("the kept body = %+v, %v", body, err)
	}
}
