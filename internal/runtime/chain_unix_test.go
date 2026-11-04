//go:build unix

package runtime_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"aos/internal/artifact"
	"aos/internal/capability"
	"aos/internal/ipc"
	"aos/internal/kernel"
	"aos/internal/runtime"
	"aos/internal/tools"
)

func (s mailState) agent(backend runtime.Backend) *runtime.AgentRuntime {
	return &runtime.AgentRuntime{Kernel: s.kernel, Artifacts: s.store, Bus: s.bus, Tools: tools.NewRuntime(), Backend: backend, MaxSteps: 16,
		BackendRetry: runtime.BackendRetryPolicy{Attempts: -1}}
}

func (s mailState) spawn(t *testing.T, name string, toolNames ...string) kernel.Process {
	t.Helper()
	process, err := s.kernel.Spawn(kernel.ProcessSpec{Role: "agent:" + name, Metadata: map[string]string{"agent_id": name}, CapabilitySet: capability.Set{Tools: toolNames}})
	if err != nil {
		t.Fatal(err)
	}
	return process
}

func (s mailState) events(t *testing.T, eventType string) []kernel.Event {
	t.Helper()
	all, err := s.kernel.Events()
	if err != nil {
		t.Fatal(err)
	}
	var out []kernel.Event
	for _, event := range all {
		if event.Type == eventType {
			out = append(out, event)
		}
	}
	return out
}

// park runs process to a yield and returns the checkpoint it parked at.
func (s mailState) park(t *testing.T, process kernel.Process) string {
	t.Helper()
	backend := &funcBackend{fn: func(int, context.Context, runtime.GenerateRequest) (runtime.GenerateResult, error) {
		return runtime.GenerateResult{Action: runtime.GenerateAction{Type: "message", Message: "parked"}}, nil
	}}
	result, err := s.agent(backend).RunWithOptions(context.Background(), process.PID, []runtime.Message{{Role: "user", Content: "work"}}, runtime.RunOptions{StopOnAssistantMessage: true})
	if err != nil || result.CheckpointID == "" {
		t.Fatalf("park = %+v, %v", result, err)
	}
	return result.CheckpointID
}

// crashing receives its mail and exits retryably in its first step.
func crashing() *funcBackend {
	return &funcBackend{fn: func(int, context.Context, runtime.GenerateRequest) (runtime.GenerateResult, error) {
		return runtime.GenerateResult{Action: runtime.GenerateAction{Type: "exit", ExitStatus: &kernel.ExitStatus{Code: 1, Reason: "transient failure", Retryable: true}}}, nil
	}}
}

// listening exits 0 at once and keeps its first request's history.
func listening(history *string) *funcBackend {
	return &funcBackend{fn: func(_ int, _ context.Context, request runtime.GenerateRequest) (runtime.GenerateResult, error) {
		if *history == "" {
			var lines []string
			for _, message := range request.History {
				lines = append(lines, message.Content)
			}
			*history = strings.Join(lines, "\n")
		}
		return exitResult("done"), nil
	}}
}

var mailBody = regexp.MustCompile(`(?m)^\| (mail \S+)$`)

// bodiesIn is the mail in a history, in order, by its marked body lines.
func bodiesIn(history string) string {
	var out []string
	for _, match := range mailBody.FindAllStringSubmatch(history, -1) {
		out = append(out, match[1])
	}
	return strings.Join(out, ", ")
}

func (s mailState) rollbackEvents(t *testing.T, id string) int {
	t.Helper()
	n := 0
	for _, event := range s.events(t, "ipc.redelivered") {
		if event.Data["id"] == id && event.Data["cause"] == "rollback" {
			n++
		}
	}
	return n
}

// The chain's latest checkpoint can be on a later successor: mail acked into
// it goes back too, each message once.
func TestRollbackAcrossTwoSuccessors(t *testing.T) {
	s := openMailState(t, t.TempDir())
	worker, lead := s.spawn(t, "worker"), s.spawn(t, "lead")
	c1 := s.park(t, worker)
	a, _ := s.bus.Send(lead.PID, worker.PID, "mail a")
	first, err := s.agent(crashing()).Resume(context.Background(), worker.PID, c1, nil, runtime.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	c2 := first.ExitStatus.Checkpoint
	b, _ := s.bus.Send(lead.PID, worker.PID, "mail b")
	second, err := s.agent(crashing()).Recover(context.Background(), worker.PID, "", nil, runtime.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if pending, _ := s.bus.Pending(); len(pending) != 0 {
		t.Fatalf("both should be acked by now: %+v", pending)
	}
	s.bus.Send(lead.PID, worker.PID, "mail p")

	var history string
	third, err := s.agent(listening(&history)).Recover(context.Background(), second.PID, c1, nil, runtime.RunOptions{Rollback: true})
	if err != nil || third.ExitStatus.Code != 0 {
		t.Fatalf("rollback = %+v, %v", third, err)
	}
	if got := bodiesIn(history); got != "mail a, mail b, mail p" {
		t.Fatalf("after a rollback to the first checkpoint: %q\n%s", got, history)
	}
	for _, id := range []string{a.ID, b.ID} {
		if !strings.Contains(history, "["+id+"]") {
			t.Fatalf("%s should keep its ID:\n%s", id, history)
		}
		if n := s.rollbackEvents(t, id); n != 1 {
			t.Fatalf("%s: %d rollback events", id, n)
		}
	}
	if pending, _ := s.bus.Pending(); len(pending) != 0 {
		t.Fatalf("left pending: %+v", pending)
	}

	// From the middle checkpoint, only what came after it goes back.
	u := openMailState(t, t.TempDir())
	worker, lead = u.spawn(t, "worker"), u.spawn(t, "lead")
	c1 = u.park(t, worker)
	u.bus.Send(lead.PID, worker.PID, "mail a")
	first, _ = u.agent(crashing()).Resume(context.Background(), worker.PID, c1, nil, runtime.RunOptions{})
	c2 = first.ExitStatus.Checkpoint
	u.bus.Send(lead.PID, worker.PID, "mail b")
	second, _ = u.agent(crashing()).Recover(context.Background(), worker.PID, "", nil, runtime.RunOptions{})
	history = ""
	if _, err := u.agent(listening(&history)).Recover(context.Background(), second.PID, c2, nil, runtime.RunOptions{Rollback: true}); err != nil {
		t.Fatal(err)
	}
	if got := bodiesIn(history); got != "mail a, mail b" {
		t.Fatalf("after a rollback to the middle checkpoint: %q", got)
	}
	if redelivered := u.events(t, "ipc.redelivered"); len(redelivered) != 1 {
		t.Fatalf("ipc.redelivered = %+v", redelivered)
	}
}

// Rolling back a rollback gives the same mail once more, under the same IDs.
func TestRollbackOfARollback(t *testing.T) {
	s := openMailState(t, t.TempDir())
	worker, lead := s.spawn(t, "worker"), s.spawn(t, "lead")
	c1 := s.park(t, worker)
	a, _ := s.bus.Send(lead.PID, worker.PID, "mail a")
	first, err := s.agent(crashing()).Resume(context.Background(), worker.PID, c1, nil, runtime.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.agent(crashing()).Recover(context.Background(), first.PID, c1, nil, runtime.RunOptions{Rollback: true})
	if err != nil {
		t.Fatal(err)
	}
	if second.ExitStatus.Checkpoint == c1 {
		t.Fatal("the rolled-back run should have reached a checkpoint of its own")
	}
	var history string
	if _, err := s.agent(listening(&history)).Recover(context.Background(), second.PID, c1, nil, runtime.RunOptions{Rollback: true}); err != nil {
		t.Fatal(err)
	}
	if got := bodiesIn(history); got != "mail a" {
		t.Fatalf("after the second rollback: %q\n%s", got, history)
	}
	if n := s.rollbackEvents(t, a.ID); n != 2 {
		t.Fatalf("%d rollback events, want one per rollback", n)
	}
	if pending, _ := s.bus.Pending(); len(pending) != 0 {
		t.Fatalf("left pending: %+v", pending)
	}
}

// A Recover can die anywhere between the kernel's supersede and the
// successor's first checkpoint. Its successor's exit names the checkpoint it
// was to start from, as if nothing had been rolled back, and the next Recover
// still delivers what was owed, once.
func TestRecoverAfterACrashDuringRedelivery(t *testing.T) {
	setup := func(t *testing.T) (mailState, kernel.Process, kernel.Process, string, ipc.Message) {
		s := openMailState(t, t.TempDir())
		worker, lead := s.spawn(t, "worker"), s.spawn(t, "lead")
		c1 := s.park(t, worker)
		a, _ := s.bus.Send(lead.PID, worker.PID, "mail a")
		if _, err := s.agent(crashing()).Resume(context.Background(), worker.PID, c1, nil, runtime.RunOptions{}); err != nil {
			t.Fatal(err)
		}
		return s, worker, lead, c1, a
	}
	check := func(t *testing.T, s mailState, pid string, a ipc.Message, wantEvents int) {
		t.Helper()
		var history string
		if _, err := s.agent(listening(&history)).Recover(context.Background(), pid, "", nil, runtime.RunOptions{}); err != nil {
			t.Fatal(err)
		}
		if got := bodiesIn(history); got != "mail a" {
			t.Fatalf("after the second Recover: %q\n%s", got, history)
		}
		if n := s.rollbackEvents(t, a.ID); n != wantEvents {
			t.Fatalf("%d rollback events, want %d", n, wantEvents)
		}
		if pending, _ := s.bus.Pending(); len(pending) != 0 {
			t.Fatalf("left pending: %+v", pending)
		}
	}

	t.Run("before anything was put back", func(t *testing.T) {
		s, worker, _, c1, a := setup(t)
		next, err := s.kernel.Supersede(worker.PID, kernel.ProcessSpec{Role: worker.Role, Metadata: map[string]string{"agent_id": "worker", "resumed_from": worker.PID}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		s.kernel.Exit(next.PID, kernel.ExitStatus{Code: 1, Reason: "orphaned", Retryable: true, Checkpoint: c1})
		check(t, s, next.PID, a, 1)
	})
	t.Run("put back, before the first step", func(t *testing.T) {
		s, worker, _, c1, a := setup(t)
		next, err := s.kernel.Supersede(worker.PID, kernel.ProcessSpec{Role: worker.Role, Metadata: map[string]string{"agent_id": "worker", "resumed_from": worker.PID}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.bus.Redeliver(next.PID, []ipc.Redelivery{{Message: a, Record: "art-x"}}, "rollback"); err != nil {
			t.Fatal(err)
		}
		s.kernel.Exit(next.PID, kernel.ExitStatus{Code: 1, Reason: "orphaned", Retryable: true, Checkpoint: c1})
		check(t, s, next.PID, a, 1)
	})
	t.Run("delivered, before the first checkpoint", func(t *testing.T) {
		s, worker, _, c1, a := setup(t)
		failing := &funcBackend{fn: func(int, context.Context, runtime.GenerateRequest) (runtime.GenerateResult, error) {
			return runtime.GenerateResult{}, errors.New("connection reset")
		}}
		next, _ := s.agent(failing).Recover(context.Background(), worker.PID, c1, nil, runtime.RunOptions{Rollback: true})
		if next.ExitStatus.Checkpoint != c1 {
			t.Fatalf("the failed successor's exit = %+v", next.ExitStatus)
		}
		pending, _ := s.bus.Pending()
		if box := pending[next.PID]; len(box) != 1 || box[0].Deliveries != 1 {
			t.Fatalf("it should have been handed out and not acked: %+v", pending)
		}
		check(t, s, next.PID, a, 1)
	})
}

// A message whose record is gone cannot go back; the loss is on the log, and
// the chain can still be recovered.
func TestRollbackWithoutARecordIsLogged(t *testing.T) {
	root := t.TempDir()
	s := openMailState(t, root)
	worker, lead := s.spawn(t, "worker"), s.spawn(t, "lead")
	c1 := s.park(t, worker)
	a, _ := s.bus.Send(lead.PID, worker.PID, "mail a")
	if _, err := s.agent(crashing()).Resume(context.Background(), worker.PID, c1, nil, runtime.RunOptions{}); err != nil {
		t.Fatal(err)
	}
	records, _ := s.store.List()
	for _, record := range records {
		if record.Type == "mailbox_message" {
			if err := os.Remove(filepath.Join(root, "artifacts", "payloads", record.ID+".json")); err != nil {
				t.Fatal(err)
			}
		}
	}
	var history string
	result, err := s.agent(listening(&history)).Recover(context.Background(), worker.PID, c1, nil, runtime.RunOptions{Rollback: true})
	if err != nil || result.ExitStatus.Code != 0 {
		t.Fatalf("recover = %+v, %v", result, err)
	}
	failed := s.events(t, "ipc.redelivery_failed")
	if len(failed) != 1 || failed[0].Data["id"] != a.ID || failed[0].Data["unreadable"] == "" {
		t.Fatalf("ipc.redelivery_failed = %+v", failed)
	}
	if strings.Contains(history, "mail a") {
		t.Fatalf("nothing could bring it back:\n%s", history)
	}
}

// A subscription made after the rollback target is kept and told once, while
// one the history shows, and a plain Recover, need no notice.
func TestRollbackTellsOfSubscriptions(t *testing.T) {
	s := openMailState(t, t.TempDir())
	worker := s.spawn(t, "worker", "message_agent")
	subscribe := func(topic string) runtime.GenerateResult {
		return runtime.GenerateResult{Action: runtime.GenerateAction{Type: "subscribe", Subscribe: &runtime.SubscribeCall{Topic: topic}}}
	}
	early := &funcBackend{fn: func(call int, _ context.Context, _ runtime.GenerateRequest) (runtime.GenerateResult, error) {
		if call == 1 {
			return subscribe("early"), nil
		}
		return runtime.GenerateResult{Action: runtime.GenerateAction{Type: "message", Message: "parked"}}, nil
	}}
	parked, err := s.agent(early).RunWithOptions(context.Background(), worker.PID, []runtime.Message{{Role: "user", Content: "work"}}, runtime.RunOptions{StopOnAssistantMessage: true})
	if err != nil {
		t.Fatal(err)
	}
	c1 := parked.CheckpointID
	late := &funcBackend{fn: func(call int, _ context.Context, _ runtime.GenerateRequest) (runtime.GenerateResult, error) {
		if call == 1 {
			return subscribe("late"), nil
		}
		return runtime.GenerateResult{Action: runtime.GenerateAction{Type: "exit", ExitStatus: &kernel.ExitStatus{Code: 1, Reason: "transient failure", Retryable: true}}}, nil
	}}
	first, err := s.agent(late).Resume(context.Background(), worker.PID, c1, nil, runtime.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// A plain Recover from the latest checkpoint: the history says it all.
	var request []runtime.Message
	second, err := s.agent(&funcBackend{fn: func(_ int, _ context.Context, r runtime.GenerateRequest) (runtime.GenerateResult, error) {
		request = r.History
		return runtime.GenerateResult{Action: runtime.GenerateAction{Type: "exit", ExitStatus: &kernel.ExitStatus{Code: 1, Reason: "again", Retryable: true}}}, nil
	}}).Recover(context.Background(), first.PID, "", nil, runtime.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	notes := func(messages []runtime.Message) (out []string) {
		for _, message := range messages {
			if strings.HasPrefix(message.Content, "subscribed to:") {
				out = append(out, message.Content)
			}
		}
		return out
	}
	if got := notes(request); len(got) != 0 {
		t.Fatalf("a plain Recover got notices: %v", got)
	}

	third, err := s.agent(&funcBackend{fn: func(_ int, _ context.Context, r runtime.GenerateRequest) (runtime.GenerateResult, error) {
		request = r.History
		return runtime.GenerateResult{Action: runtime.GenerateAction{Type: "exit", ExitStatus: &kernel.ExitStatus{Code: 1, Reason: "again", Retryable: true}}}, nil
	}}).Recover(context.Background(), second.PID, c1, nil, runtime.RunOptions{Rollback: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := notes(request); !slices.Equal(got, []string{"subscribed to: late"}) {
		t.Fatalf("after a rollback: %v", got)
	}
	if topics, _ := s.bus.Subscriptions(third.PID); !slices.Equal(topics, []string{"early", "late"}) {
		t.Fatalf("the chain's topics = %v", topics)
	}
	for _, message := range request {
		if strings.HasPrefix(message.Content, "subscribed to:") && message.Role != "notice" {
			t.Fatalf("the notice's role = %q", message.Role)
		}
	}

	// Rolling back to where the notice already is does not repeat it.
	if _, err := s.agent(&funcBackend{fn: func(_ int, _ context.Context, r runtime.GenerateRequest) (runtime.GenerateResult, error) {
		request = r.History
		return exitResult("done"), nil
	}}).Recover(context.Background(), third.PID, third.ExitStatus.Checkpoint, nil, runtime.RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := notes(request); !slices.Equal(got, []string{"subscribed to: late"}) {
		t.Fatalf("after recovering from it: %v", got)
	}
}

// A retryable exit keeps its subscriptions for a successor; the end of the
// chain, reached by the run itself, drops them.
func TestSubscriptionsLastAsLongAsTheChain(t *testing.T) {
	s := openMailState(t, t.TempDir())
	worker := s.spawn(t, "worker", "message_agent")
	backend := &funcBackend{fn: func(call int, _ context.Context, _ runtime.GenerateRequest) (runtime.GenerateResult, error) {
		if call == 1 {
			return runtime.GenerateResult{Action: runtime.GenerateAction{Type: "subscribe", Subscribe: &runtime.SubscribeCall{Topic: "news"}}}, nil
		}
		return runtime.GenerateResult{Action: runtime.GenerateAction{Type: "exit", ExitStatus: &kernel.ExitStatus{Code: 1, Reason: "transient failure", Retryable: true}}}, nil
	}}
	if _, err := s.agent(backend).Run(context.Background(), worker.PID, []runtime.Message{{Role: "user", Content: "listen"}}); err != nil {
		t.Fatal(err)
	}
	if topics, _ := s.bus.Subscriptions(worker.PID); !slices.Equal(topics, []string{"news"}) {
		t.Fatalf("after a retryable exit: %v", topics)
	}
	var history string
	next, err := s.agent(listening(&history)).Recover(context.Background(), worker.PID, "", nil, runtime.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if topics, _ := s.bus.Subscriptions(next.PID); len(topics) != 0 {
		t.Fatalf("after exit 0: %v", topics)
	}
	left := s.events(t, "ipc.unsubscribed")
	if len(left) != 2 || left[0].Data["cause"] != ipc.CauseTransfer || left[1].Data["cause"] != ipc.CauseChainEnded || left[1].Data["pid"] != next.PID {
		t.Fatalf("ipc.unsubscribed = %+v", left)
	}
}

// What an agent is told: the process its mail reached, or where the chain ended.
func TestSendResultNamesThePIDItReached(t *testing.T) {
	s := openMailState(t, t.TempDir())
	first := s.spawn(t, "worker")
	s.kernel.Exit(first.PID, kernel.ExitStatus{Code: 1, Reason: "crashed", Retryable: true})
	second, err := s.kernel.Supersede(first.PID, kernel.ProcessSpec{Role: first.Role}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ended := s.spawn(t, "ended")
	s.kernel.Exit(ended.PID, kernel.ExitStatus{Code: 1, Reason: "crashed", Retryable: true})
	s.kernel.Close(ended.PID, "declined")

	sender := s.spawn(t, "lead", "message_agent")
	var last []runtime.Message
	backend := &funcBackend{fn: func(call int, _ context.Context, request runtime.GenerateRequest) (runtime.GenerateResult, error) {
		last = request.History
		switch call {
		case 1:
			return runtime.GenerateResult{Action: runtime.GenerateAction{Type: "send", Send: &runtime.SendCall{To: first.PID, Body: "hello"}}}, nil
		case 2:
			return toolAction("message_agent", map[string]string{"to": first.PID, "body": "again"}), nil
		case 3:
			return runtime.GenerateResult{Action: runtime.GenerateAction{Type: "send", Send: &runtime.SendCall{To: ended.PID, Body: "late"}}}, nil
		}
		return exitResult("done"), nil
	}}
	if _, err := s.agent(backend).RunWithOptions(context.Background(), sender.PID, []runtime.Message{{Role: "user", Content: "go"}}, runtime.RunOptions{ContinueOnToolError: true}); err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, message := range last {
		lines = append(lines, message.Content)
	}
	history := strings.Join(lines, "\n")
	for _, want := range []string{
		fmt.Sprintf("send:to=%s id=msg-000001 forwarded_from=%s", second.PID, first.PID),
		"forwarded_from:" + first.PID,
		"to:" + second.PID,
		"tool_error name=send",
		ended.PID + ` (exit code 1 "crashed", closed)`,
	} {
		if !strings.Contains(history, want) {
			t.Fatalf("the history should hold %q:\n%s", want, history)
		}
	}
	if pending, _ := s.bus.Pending(); len(pending[second.PID]) != 2 || len(pending[ended.PID]) != 0 {
		t.Fatalf("pending = %+v", pending)
	}
}

func TestRecoverRefusesAnEndedChain(t *testing.T) {
	s := openMailState(t, t.TempDir())
	closed := s.spawn(t, "closed")
	c := s.park(t, closed)
	s.kernel.Exit(closed.PID, kernel.ExitStatus{Code: 1, Reason: "crashed", Retryable: true, Checkpoint: c})
	s.kernel.Close(closed.PID, "declined")
	_, err := s.agent(&funcBackend{}).Recover(context.Background(), closed.PID, "", nil, runtime.RunOptions{})
	if !errors.Is(err, runtime.ErrNotRecoverable) || !errors.Is(err, kernel.ErrChainClosed) {
		t.Fatalf("recover of a closed process = %v", err)
	}
	finished := s.spawn(t, "finished")
	c = s.park(t, finished)
	s.kernel.Exit(finished.PID, kernel.ExitStatus{Code: 0, Reason: "done", Retryable: true, Checkpoint: c})
	if _, err := s.agent(&funcBackend{}).Recover(context.Background(), finished.PID, "", nil, runtime.RunOptions{}); !errors.Is(err, runtime.ErrNotRecoverable) {
		t.Fatalf("recover of an exit 0 = %v", err)
	}
}

const chainRaceEnv = "AOS_CHAIN_RACE_HELPER"

func chainRaceHelper(args ...string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=^TestChainRaceHelper$", "-test.count=1")
	cmd.Env = append(os.Environ(), chainRaceEnv+"="+strings.Join(args, "\x1f"))
	cmd.Stderr = os.Stderr
	return cmd
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// TestChainRaceHelper is a subprocess, not a test. Modes:
//   - send root from to stop out: send numbered messages until stop exists,
//     writing "id<TAB>to<TAB>body" lines for each;
//   - publish root from topic stop out: the same, through a topic;
//   - recover root pid done rounds: Recover the chain's end rounds times, each
//     successor but the last crashing in its first step; the last takes mail
//     until done exists and nothing new is waiting, then exits 0.
func TestChainRaceHelper(t *testing.T) {
	raw := os.Getenv(chainRaceEnv)
	if raw == "" {
		t.Skip("subprocess helper")
	}
	// A parent that was killed never writes the stop file.
	time.AfterFunc(3*time.Minute, func() { os.Exit(2) })
	args := strings.Split(raw, "\x1f")
	s := openMailState(t, args[1])
	switch args[0] {
	case "send", "publish":
		out, err := os.Create(args[5])
		if err != nil {
			t.Fatal(err)
		}
		defer out.Close()
		for i := 0; !exists(args[4]); i++ {
			// Paced, so that the Recover gets its turn at the locks.
			time.Sleep(150 * time.Millisecond)
			body := fmt.Sprintf("%s#%05d", args[2], i)
			if args[0] == "send" {
				message, err := s.bus.Send(args[2], args[3], body)
				if err != nil {
					t.Fatalf("send %d: %v", i, err)
				}
				fmt.Fprintf(out, "%s\t%s\t%s\n", message.ID, message.To, body)
				continue
			}
			messages, err := s.bus.Publish(args[2], args[3], body)
			if err != nil || len(messages) != 1 {
				t.Fatalf("publish %d = %+v, %v", i, messages, err)
			}
			fmt.Fprintf(out, "%s\t%s\t%s\n", messages[0].ID, messages[0].To, body)
		}
	case "recover":
		rounds, _ := strconv.Atoi(args[4])
		for round := 1; round <= rounds; round++ {
			end, err := s.kernel.ChainEnd(args[2])
			if err != nil {
				t.Fatal(err)
			}
			backend := &funcBackend{fn: func(call int, _ context.Context, request runtime.GenerateRequest) (runtime.GenerateResult, error) {
				if round < rounds {
					// Leave a gap with the next process not there yet.
					time.Sleep(20 * time.Millisecond)
					return runtime.GenerateResult{Action: runtime.GenerateAction{Type: "exit", ExitStatus: &kernel.ExitStatus{Code: 1, Reason: "crashed", Retryable: true}}}, nil
				}
				for {
					pending, err := s.bus.Pending()
					if err != nil {
						return runtime.GenerateResult{}, err
					}
					if slices.ContainsFunc(pending[request.Process.PID], func(m ipc.Message) bool { return m.Deliveries == 0 }) {
						return runtime.GenerateResult{Action: runtime.GenerateAction{Type: "message", Message: "more"}}, nil
					}
					if exists(args[3]) {
						return exitResult("done"), nil
					}
					time.Sleep(20 * time.Millisecond)
				}
			}}
			agent := s.agent(backend)
			agent.MaxSteps = 1 << 20
			result, err := agent.Recover(context.Background(), end.PID, "", nil, runtime.RunOptions{})
			if err != nil {
				t.Fatalf("recover %d = %+v, %v", round, result, err)
			}
			if round == rounds && result.ExitStatus.Code != 0 {
				t.Fatalf("the last recover = %+v", result)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

// N OS processes send and publish to one process chain, always through its
// first PID, while another OS process recovers it over and over. Every message
// reaches the chain once and in order, whether it was held and transferred or
// forwarded, and nothing deadlocks.
func TestForwardingRacesRecover(t *testing.T) {
	trials := 3
	if raw := os.Getenv("AOS_IPC_TRIALS"); raw != "" {
		trials, _ = strconv.Atoi(raw)
	}
	for trial := 0; trial < trials; trial++ {
		forwardingRace(t, trial)
	}
}

func forwardingRace(t *testing.T, trial int) {
	const senders, rounds = 3, 4
	root := t.TempDir()
	s := openMailState(t, root)
	worker := s.spawn(t, "worker")
	checkpoint := s.park(t, worker)
	if err := s.bus.Subscribe(worker.PID, "news"); err != nil {
		t.Fatal(err)
	}
	s.kernel.Exit(worker.PID, kernel.ExitStatus{Code: 1, Reason: "crashed", Retryable: true, Checkpoint: checkpoint})

	scratch := t.TempDir()
	stop, done := filepath.Join(scratch, "stop"), filepath.Join(scratch, "done")
	var cmds []*exec.Cmd
	var outs []string
	for i := 0; i <= senders; i++ {
		from := s.spawn(t, fmt.Sprintf("sender%d", i)).PID
		out := filepath.Join(scratch, fmt.Sprintf("sent-%d.tsv", i))
		outs = append(outs, out)
		if i == senders {
			cmds = append(cmds, chainRaceHelper("publish", root, from, "news", stop, out))
		} else {
			cmds = append(cmds, chainRaceHelper("send", root, from, worker.PID, stop, out))
		}
	}
	for _, cmd := range cmds {
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
	}
	// A deadlock would show as a helper that never ends.
	watchdog := time.AfterFunc(90*time.Second, func() {
		for _, cmd := range cmds {
			cmd.Process.Signal(syscall.SIGKILL)
		}
	})
	defer watchdog.Stop()
	waitFor := func(what string, ready func() bool) {
		t.Helper()
		for deadline := time.Now().Add(60 * time.Second); !ready(); time.Sleep(100 * time.Millisecond) {
			if time.Now().After(deadline) {
				for _, cmd := range cmds {
					cmd.Process.Signal(syscall.SIGKILL)
				}
				t.Fatalf("trial %d: timed out waiting for %s", trial, what)
			}
		}
	}
	// Some mail is held before the Recover starts, and some is forwarded after.
	waitFor("held mail", func() bool {
		pending, _ := s.bus.Pending()
		return len(pending[worker.PID]) >= senders+1
	})
	recovering := chainRaceHelper("recover", root, worker.PID, done, strconv.Itoa(rounds))
	if err := recovering.Start(); err != nil {
		t.Fatal(err)
	}
	cmds = append(cmds, recovering)
	waitFor("every recovery", func() bool { return len(s.events(t, "process.superseded")) == rounds })
	waitFor("forwarded mail", func() bool { return len(s.events(t, "ipc.forwarded")) >= senders })
	os.WriteFile(stop, nil, 0o644)
	for _, cmd := range cmds[:senders+1] {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("trial %d: sender: %v", trial, err)
		}
	}
	os.WriteFile(done, nil, 0o644)
	if err := recovering.Wait(); err != nil {
		t.Fatalf("trial %d: recover: %v", trial, err)
	}

	sent := map[string]string{}
	for _, path := range outs {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			fields := strings.Split(line, "\t")
			if len(fields) != 3 {
				t.Fatalf("trial %d: bad line %q", trial, line)
			}
			sent[fields[0]] = fields[2]
		}
	}
	chain := []string{worker.PID}
	for _, event := range s.events(t, "process.superseded") {
		chain = append(chain, event.Data["successor"])
	}
	records, _ := s.store.List()
	slices.SortFunc(records, func(a, b artifact.Artifact) int { return strings.Compare(a.ID, b.ID) })
	received := map[string]int{}
	var order []ipc.Message
	for _, record := range records {
		if record.Type != "mailbox_message" || record.Metadata["direction"] != "receive" {
			continue
		}
		var message ipc.Message
		if err := s.store.ReadJSON(record.ID, &message); err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(chain[1:], record.ProducerPID) || message.To != record.ProducerPID {
			t.Fatalf("trial %d: %s went to %s (record of %s), not a successor in %v", trial, message.ID, message.To, record.ProducerPID, chain)
		}
		received[message.ID]++
		order = append(order, message)
	}
	lost, duplicated, disordered := 0, 0, 0
	for id := range sent {
		switch received[id] {
		case 0:
			lost++
		case 1:
		default:
			duplicated++
		}
	}
	last := map[string]int{}
	for _, message := range order {
		from, number, _ := strings.Cut(message.Body, "#")
		n, _ := strconv.Atoi(number)
		if previous, ok := last[from]; ok && n <= previous {
			disordered++
		}
		last[from] = n
	}
	pending, _ := s.bus.Pending()
	held := 0
	for _, event := range s.events(t, "ipc.sent") {
		if event.Data["held"] == "true" {
			held++
		}
	}
	forwarded := len(s.events(t, "ipc.forwarded"))
	t.Logf("trial %d: %d sent, %d received, lost %d, duplicated %d, out of order %d; %d held for a successor, %d forwarded; left pending %d", trial, len(sent), len(order), lost, duplicated, disordered, held, forwarded, len(pending))
	if lost != 0 || duplicated != 0 || disordered != 0 || len(order) != len(sent) || len(pending) != 0 {
		t.Fatalf("trial %d: lost %d, duplicated %d, out of order %d, %d of %d received, pending %+v", trial, lost, duplicated, disordered, len(order), len(sent), pending)
	}
	if held == 0 || forwarded == 0 {
		t.Fatalf("trial %d: the race was not exercised: %d held, %d forwarded", trial, held, forwarded)
	}
	if !slices.ContainsFunc(s.events(t, "ipc.sent"), func(event kernel.Event) bool { return event.Data["to"] == chain[rounds] }) {
		t.Fatalf("trial %d: nothing reached the last process %s", trial, chain[rounds])
	}
}

// damage makes an artifact unreadable: its payload rewritten, or gone.
func damage(t *testing.T, root, id string, missing bool) {
	t.Helper()
	path := filepath.Join(root, "artifacts", "payloads", id+".json")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if !missing {
		if err := os.WriteFile(path, []byte(`{"mailbox_seen":[]}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func (s mailState) recordIDs(t *testing.T, artifactType string) []string {
	t.Helper()
	records, err := s.store.List()
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, record := range records {
		if record.Type == artifactType {
			ids = append(ids, record.ID)
		}
	}
	slices.Sort(ids)
	return ids
}

// A damaged newest checkpoint is the main reason to roll back. It never stops
// the Recover, it is on the log, and the chain's records still say what it knew.
func TestRollbackPastAnUnreadableCheckpoint(t *testing.T) {
	for name, missing := range map[string]bool{"damaged": false, "missing": true} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			s := openMailState(t, root)
			worker, lead := s.spawn(t, "worker"), s.spawn(t, "lead")
			c1 := s.park(t, worker)
			s.bus.Send(lead.PID, worker.PID, "mail a")
			first, err := s.agent(crashing()).Resume(context.Background(), worker.PID, c1, nil, runtime.RunOptions{})
			if err != nil {
				t.Fatal(err)
			}
			c2 := first.ExitStatus.Checkpoint
			s.bus.Send(lead.PID, worker.PID, "mail b")
			second, err := s.agent(crashing()).Recover(context.Background(), first.PID, "", nil, runtime.RunOptions{})
			if err != nil {
				t.Fatal(err)
			}
			c3 := second.ExitStatus.Checkpoint
			damage(t, root, c3, missing)

			var history string
			result, err := s.agent(listening(&history)).Recover(context.Background(), second.PID, c1, nil, runtime.RunOptions{Rollback: true})
			if err != nil || result.ExitStatus.Code != 0 {
				t.Fatalf("recover = %+v, %v", result, err)
			}
			// mail a is in the readable c2; mail b only in the unreadable c3
			// and in its record.
			if got := bodiesIn(history); got != "mail a, mail b" {
				t.Fatalf("after the rollback: %q\n%s", got, history)
			}
			logged := s.events(t, "ipc.checkpoint_unreadable")
			if len(logged) != 1 || logged[0].Data["checkpoint"] != c3 || logged[0].Data["reason"] == "" {
				t.Fatalf("ipc.checkpoint_unreadable = %+v (c2 %s, c3 %s)", logged, c2, c3)
			}
			if failed := s.events(t, "ipc.redelivery_failed"); len(failed) != 0 {
				t.Fatalf("ipc.redelivery_failed = %+v", failed)
			}
		})
	}
}

// Every newer checkpoint gone, and the second Recover is no different from the
// first: no attempt leaves the chain worse off.
func TestUnreadableCheckpointDoesNotBlockRepeatedRecovers(t *testing.T) {
	root := t.TempDir()
	s := openMailState(t, root)
	worker, lead := s.spawn(t, "worker"), s.spawn(t, "lead")
	c1 := s.park(t, worker)
	s.bus.Send(lead.PID, worker.PID, "mail a")
	first, err := s.agent(crashing()).Resume(context.Background(), worker.PID, c1, nil, runtime.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	damage(t, root, first.ExitStatus.Checkpoint, false)
	second, err := s.agent(crashing()).Recover(context.Background(), worker.PID, c1, nil, runtime.RunOptions{Rollback: true})
	if err != nil || strings.Contains(second.ExitStatus.Reason, "re-delivery") {
		t.Fatalf("first attempt = %+v, %v", second, err)
	}
	var history string
	third, err := s.agent(listening(&history)).Recover(context.Background(), second.PID, c1, nil, runtime.RunOptions{Rollback: true})
	if err != nil || third.ExitStatus.Code != 0 {
		t.Fatalf("second attempt = %+v, %v", third, err)
	}
	if got := bodiesIn(history); got != "mail a" {
		t.Fatalf("after the second attempt: %q\n%s", got, history)
	}
}

// With the checkpoint and the record both gone, the message cannot come back.
// The process still runs, and the log says what could not be named.
func TestUnreadableCheckpointAndRecordIsLogged(t *testing.T) {
	root := t.TempDir()
	s := openMailState(t, root)
	worker, lead := s.spawn(t, "worker"), s.spawn(t, "lead")
	c1 := s.park(t, worker)
	s.bus.Send(lead.PID, worker.PID, "mail a")
	first, err := s.agent(crashing()).Resume(context.Background(), worker.PID, c1, nil, runtime.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	damage(t, root, first.ExitStatus.Checkpoint, true)
	record := s.recordIDs(t, "mailbox_message")[0]
	damage(t, root, record, true)
	var history string
	result, err := s.agent(listening(&history)).Recover(context.Background(), worker.PID, c1, nil, runtime.RunOptions{Rollback: true})
	if err != nil || result.ExitStatus.Code != 0 {
		t.Fatalf("recover = %+v, %v", result, err)
	}
	if strings.Contains(history, "mail a") {
		t.Fatalf("nothing could bring it back:\n%s", history)
	}
	failed := s.events(t, "ipc.redelivery_failed")
	if len(failed) != 1 || failed[0].Data["unreadable"] != record {
		t.Fatalf("ipc.redelivery_failed = %+v", failed)
	}
}

// A plain Recover reads no payload but the checkpoint it starts from, so
// damage elsewhere in the store adds no way for it to fail.
func TestPlainRecoverReadsNothingElse(t *testing.T) {
	root := t.TempDir()
	s := openMailState(t, root)
	worker, lead := s.spawn(t, "worker"), s.spawn(t, "lead")
	c1 := s.park(t, worker)
	s.bus.Send(lead.PID, worker.PID, "mail a")
	first, err := s.agent(crashing()).Resume(context.Background(), worker.PID, c1, nil, runtime.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	damage(t, root, c1, true)
	damage(t, root, s.recordIDs(t, "mailbox_message")[0], true)
	var history string
	result, err := s.agent(listening(&history)).Recover(context.Background(), first.PID, "", nil, runtime.RunOptions{})
	if err != nil || result.ExitStatus.Code != 0 {
		t.Fatalf("recover = %+v, %v", result, err)
	}
	if got := bodiesIn(history); got != "mail a" {
		t.Fatalf("history: %q", got)
	}
	if n := len(s.events(t, "ipc.checkpoint_unreadable")) + len(s.events(t, "ipc.redelivery_failed")); n != 0 {
		t.Fatalf("%d events for a store a plain Recover never read", n)
	}
}

// Subscribe, then exit naming the checkpoint before it: a plain Recover still
// says so, or topic mail would arrive with no history of asking for it.
func TestPlainRecoverTellsOfASubscriptionPastItsCheckpoint(t *testing.T) {
	s := openMailState(t, t.TempDir())
	worker := s.spawn(t, "worker", "message_agent")
	c1 := s.park(t, worker)
	if err := s.bus.Subscribe(worker.PID, "news"); err != nil {
		t.Fatal(err)
	}
	s.kernel.Transition(worker.PID, kernel.StateRunning)
	if _, err := s.kernel.Exit(worker.PID, kernel.ExitStatus{Code: 1, Reason: "crashed", Retryable: true, Checkpoint: c1}); err != nil {
		t.Fatal(err)
	}
	var history string
	if _, err := s.agent(listening(&history)).Recover(context.Background(), worker.PID, "", nil, runtime.RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if strings.Count(history, "subscribed to: news") != 1 {
		t.Fatalf("history:\n%s", history)
	}
}

// A send to a process that is not running says so, in each of the three
// places an agent can read it, and a live recipient says nothing of the kind.
func TestHeldIsToldToTheSender(t *testing.T) {
	s := openMailState(t, t.TempDir())
	worker := s.spawn(t, "worker")
	if err := s.bus.Subscribe(worker.PID, "news"); err != nil {
		t.Fatal(err)
	}
	s.kernel.Exit(worker.PID, kernel.ExitStatus{Code: 1, Reason: "crashed", Retryable: true})
	live := s.spawn(t, "live")
	sender := s.spawn(t, "lead", "message_agent")
	var last []runtime.Message
	backend := &funcBackend{fn: func(call int, _ context.Context, request runtime.GenerateRequest) (runtime.GenerateResult, error) {
		last = request.History
		switch call {
		case 1:
			return runtime.GenerateResult{Action: runtime.GenerateAction{Type: "send", Send: &runtime.SendCall{To: worker.PID, Body: "one"}}}, nil
		case 2:
			return toolAction("message_agent", map[string]string{"to": worker.PID, "body": "two"}), nil
		case 3:
			return runtime.GenerateResult{Action: runtime.GenerateAction{Type: "publish", Publish: &runtime.PublishCall{Topic: "news", Body: "three"}}}, nil
		case 4:
			return runtime.GenerateResult{Action: runtime.GenerateAction{Type: "send", Send: &runtime.SendCall{To: live.PID, Body: "four"}}}, nil
		}
		return exitResult("done"), nil
	}}
	if _, err := s.agent(backend).RunWithOptions(context.Background(), sender.PID, []runtime.Message{{Role: "user", Content: "go"}}, runtime.RunOptions{}); err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, message := range last {
		lines = append(lines, message.Content)
	}
	history := strings.Join(lines, "\n")
	for _, want := range []string{
		fmt.Sprintf("send:to=%s id=msg-000001 held=true (%s)", worker.PID, ipc.HeldMeaning),
		"held:true: " + ipc.HeldMeaning,
		fmt.Sprintf("publish:topic=news count=1 held=1 (%s)", ipc.HeldMeaning),
	} {
		if !strings.Contains(history, want) {
			t.Fatalf("the history should hold %q:\n%s", want, history)
		}
	}
	if want := fmt.Sprintf("send:to=%s id=msg-000004", live.PID); !strings.HasSuffix(history, want) {
		t.Fatalf("a send to a live process should end at %q:\n%s", want, history)
	}
	// It is about that moment, so the send's record does not say it.
	records, _ := s.store.List()
	checked := 0
	for _, record := range records {
		if record.Type == "mailbox_message" && record.Metadata["direction"] == "send" {
			var stored map[string]any
			if err := s.store.ReadJSON(record.ID, &stored); err != nil {
				t.Fatal(err)
			}
			if _, ok := stored["held"]; ok {
				t.Fatalf("record %s = %v", record.ID, stored)
			}
			checked++
		}
	}
	if checked != 2 {
		t.Fatalf("%d send records", checked)
	}
}

// What is owed is every newer checkpoint's list, not only the newest's. A
// chain rolled back before re-delivery existed has a newest checkpoint that
// lacks mail an older one held.
func TestOwedComesFromEveryNewerCheckpoint(t *testing.T) {
	s := openMailState(t, t.TempDir())
	worker, lead := s.spawn(t, "worker"), s.spawn(t, "lead")
	c1 := s.park(t, worker)
	s.bus.Send(lead.PID, worker.PID, "mail a")
	first, err := s.agent(crashing()).Resume(context.Background(), worker.PID, c1, nil, runtime.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.CreateJSON("checkpoint", first.PID, runtime.Session{Messages: []runtime.Message{{Role: "user", Content: "work"}}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	var history string
	if _, err := s.agent(listening(&history)).Recover(context.Background(), first.PID, c1, nil, runtime.RunOptions{Rollback: true}); err != nil {
		t.Fatal(err)
	}
	if got := bodiesIn(history); got != "mail a" {
		t.Fatalf("after the rollback: %q\n%s", got, history)
	}
}
