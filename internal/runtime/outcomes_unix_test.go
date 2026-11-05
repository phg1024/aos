//go:build unix

package runtime_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"aos/internal/framing"
	"aos/internal/ipc"
	"aos/internal/kernel"
	"aos/internal/runtime"
)

func exitAction(code int, retryable bool, reason string) runtime.GenerateResult {
	return runtime.GenerateResult{Action: runtime.GenerateAction{Type: "exit", ExitStatus: &kernel.ExitStatus{Code: code, Reason: reason, Retryable: retryable}}}
}

func sayAction(text string) runtime.GenerateResult {
	return runtime.GenerateResult{Action: runtime.GenerateAction{Type: "message", Message: text}}
}

func notesIn(messages []runtime.Message, prefix string) []string {
	var out []string
	for _, message := range messages {
		if strings.HasPrefix(message.Content, prefix) {
			out = append(out, message.Content)
		}
	}
	return out
}

func (s mailState) pendingFor(t *testing.T, pid string) []ipc.Message {
	t.Helper()
	pending, err := s.bus.Pending()
	if err != nil {
		t.Fatal(err)
	}
	return pending[pid]
}

// The story this step exists for: a sender is told held, the chain is closed,
// and the sender's next request says what became of it.
func TestHeldThenClosedTellsTheSender(t *testing.T) {
	s := openMailState(t, t.TempDir())
	worker := s.spawn(t, "worker")
	s.kernel.Exit(worker.PID, kernel.ExitStatus{Code: 1, Reason: "crashed", Retryable: true})
	sender := s.spawn(t, "lead", "message_agent")
	var histories [][]runtime.Message
	backend := &funcBackend{fn: func(call int, _ context.Context, request runtime.GenerateRequest) (runtime.GenerateResult, error) {
		histories = append(histories, request.History)
		switch call {
		case 1:
			return runtime.GenerateResult{Action: runtime.GenerateAction{Type: "send", Send: &runtime.SendCall{To: worker.PID, Body: "are you there"}}}, nil
		case 2:
			if _, err := s.kernel.Close(worker.PID, "declined"); err != nil {
				t.Error(err)
			}
			if err := s.bus.Sweep(); err != nil {
				t.Error(err)
			}
			return sayAction("waiting"), nil
		}
		return exitResult("done"), nil
	}}
	if _, err := s.agent(backend).Run(context.Background(), sender.PID, []runtime.Message{{Role: "user", Content: "go"}}); err != nil {
		t.Fatal(err)
	}
	if got := notesIn(histories[1], "send:"); len(got) != 1 || !strings.Contains(got[0], "held=true") {
		t.Fatalf("the send's result: %v", got)
	}
	if got := notesIn(histories[1], "undelivered:"); len(got) != 0 {
		t.Fatalf("too early: %v", got)
	}
	want := fmt.Sprintf(`undelivered: msg-000001 to worker (%s): no record that a model read this; its chain ended with exit code 1 "crashed", closed`, worker.PID)
	if got := notesIn(histories[2], "undelivered:"); !slices.Equal(got, []string{want}) {
		t.Fatalf("the next request: %v, want %q", got, want)
	}
	letters := s.events(t, ipc.DeadLetterEvent)
	if len(letters) != 1 || letters[0].ProcessID != worker.PID || letters[0].Data["id"] != "msg-000001" || letters[0].Data["from"] != sender.PID ||
		letters[0].Data["reason"] != ipc.ReasonUnread || letters[0].Data["exit_code"] != "1" || letters[0].Data["exit_reason"] != "crashed" {
		t.Fatalf("ipc.dead_letter = %+v", letters)
	}
	if pending, _ := s.bus.Pending(); len(pending) != 0 {
		t.Fatalf("left pending: %+v", pending)
	}
}

// A retryable exit is not an end. Mail waits for a successor, or for a Close.
func TestRetryableExitKeepsMailUntilClosed(t *testing.T) {
	s := openMailState(t, t.TempDir())
	worker, lead := s.spawn(t, "worker"), s.spawn(t, "lead")
	sent, _ := s.bus.Send(lead.PID, worker.PID, "unread")
	s.kernel.Exit(worker.PID, kernel.ExitStatus{Code: 1, Reason: "crashed", Retryable: true})
	if err := s.bus.Sweep(); err != nil {
		t.Fatal(err)
	}
	if box := s.pendingFor(t, worker.PID); len(box) != 1 || box[0].ID != sent.ID || len(s.events(t, ipc.DeadLetterEvent)) != 0 {
		t.Fatalf("a sweep took mail from an open chain: %+v", box)
	}
	s.kernel.Close(worker.PID, "declined")
	if err := s.bus.Sweep(); err != nil {
		t.Fatal(err)
	}
	if box := s.pendingFor(t, worker.PID); len(box) != 0 || len(s.events(t, ipc.DeadLetterEvent)) != 1 {
		t.Fatalf("after the close: %+v", box)
	}
}

// A Sweep can repeat, from anywhere, and after a crash between its event and
// its mailbox write: no second event, and nothing left.
func TestSweepIsRepeatable(t *testing.T) {
	root := t.TempDir()
	s := openMailState(t, root)
	worker, lead := s.spawn(t, "worker"), s.spawn(t, "lead")
	s.bus.Send(lead.PID, worker.PID, "one")
	s.bus.Send(lead.PID, worker.PID, "two")
	s.kernel.Exit(worker.PID, kernel.ExitStatus{Code: 0, Reason: "done"})
	mailboxes := filepath.Join(root, "bus", "mailboxes.json")
	before, err := os.ReadFile(mailboxes)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := s.bus.Sweep(); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(mailboxes, before, 0o644); err != nil { // as if the crash came before this write
		t.Fatal(err)
	}
	statePath := filepath.Join(root, "bus", "state.json")
	if err := os.WriteFile(statePath, []byte(`{"next_message_id":3}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.bus.Sweep(); err != nil {
		t.Fatal(err)
	}
	if letters := s.events(t, ipc.DeadLetterEvent); len(letters) != 2 {
		t.Fatalf("ipc.dead_letter = %+v", letters)
	}
	if pending, _ := s.bus.Pending(); len(pending) != 0 {
		t.Fatalf("left pending: %+v", pending)
	}
	// The counter caught up, so a sender still hears.
	if events, latest, err := s.bus.DeadLetters(0); err != nil || len(events) != 2 || latest == 0 {
		t.Fatalf("DeadLetters = %+v, %d, %v", events, latest, err)
	}
}

// An exit nobody swept after, as the scheduler's or a process that died at the
// wrong moment, is swept up by the next step of any process on the state.
func TestAnotherProcessStepSweepsAnUnsweptExit(t *testing.T) {
	s := openMailState(t, t.TempDir())
	worker := s.spawn(t, "worker")
	sender := s.spawn(t, "lead", "message_agent")
	s.bus.Send(sender.PID, worker.PID, "unread")
	s.kernel.Exit(worker.PID, kernel.ExitStatus{Code: 0, Reason: "done"})
	var history []runtime.Message
	backend := &funcBackend{fn: func(_ int, _ context.Context, request runtime.GenerateRequest) (runtime.GenerateResult, error) {
		history = request.History
		return exitResult("done"), nil
	}}
	if _, err := s.agent(backend).Run(context.Background(), sender.PID, []runtime.Message{{Role: "user", Content: "go"}}); err != nil {
		t.Fatal(err)
	}
	if len(notesIn(history, "undelivered: msg-000001")) != 1 {
		t.Fatalf("history: %+v", history)
	}
}

// A sweep reads no checkpoint, so one that cannot be read stops neither the
// exit nor the sweep. The message's reason comes from the bus.
func TestUnreadableCheckpointDoesNotStopTheSweep(t *testing.T) {
	root := t.TempDir()
	s := openMailState(t, root)
	worker, lead := s.spawn(t, "worker"), s.spawn(t, "lead")
	s.park(t, worker)
	sent, _ := s.bus.Send(lead.PID, worker.PID, "unread")
	for _, id := range s.recordIDs(t, "checkpoint") {
		damage(t, root, id, false)
	}
	s.kernel.Exit(worker.PID, kernel.ExitStatus{Code: 0, Reason: "done"})
	if err := s.bus.Sweep(); err != nil {
		t.Fatal(err)
	}
	letters := s.events(t, ipc.DeadLetterEvent)
	if len(letters) != 1 || letters[0].Data["id"] != sent.ID || letters[0].Data["reason"] != ipc.ReasonUnread {
		t.Fatalf("ipc.dead_letter = %+v", letters)
	}
}

// A swept message stays on disk whole, however it was sent. For the operator's,
// nothing else holds it. Repeating a sweep after a crash writes the same file.
func TestSweepKeepsEveryBody(t *testing.T) {
	root := t.TempDir()
	s := openMailState(t, root)
	worker := s.spawn(t, "worker")
	if err := s.bus.Subscribe(worker.PID, "news"); err != nil {
		t.Fatal(err)
	}
	sender := s.spawn(t, "lead", "message_agent")
	backend := &funcBackend{fn: func(call int, _ context.Context, _ runtime.GenerateRequest) (runtime.GenerateResult, error) {
		switch call {
		case 1:
			return runtime.GenerateResult{Action: runtime.GenerateAction{Type: "send", Send: &runtime.SendCall{To: worker.PID, Body: "BODY-SEND"}}}, nil
		case 2:
			return toolAction("message_agent", map[string]string{"to": worker.PID, "body": "BODY-MESSAGE-AGENT"}), nil
		case 3:
			return runtime.GenerateResult{Action: runtime.GenerateAction{Type: "publish", Publish: &runtime.PublishCall{Topic: "news", Body: "BODY-PUBLISH"}}}, nil
		}
		return sayAction("parked"), nil
	}}
	if _, err := s.agent(backend).RunWithOptions(context.Background(), sender.PID, []runtime.Message{{Role: "user", Content: "go"}}, runtime.RunOptions{StopOnAssistantMessage: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.bus.SendAsOperator(worker.PID, "BODY-OPERATOR\nsecond line"); err != nil {
		t.Fatal(err)
	}
	box := s.pendingFor(t, worker.PID)
	if len(box) != 4 {
		t.Fatalf("mailbox = %+v", box)
	}
	mailboxes := filepath.Join(root, "bus", "mailboxes.json")
	before, err := os.ReadFile(mailboxes)
	if err != nil {
		t.Fatal(err)
	}
	s.kernel.Exit(worker.PID, kernel.ExitStatus{Code: 0, Reason: "done"})
	check := func() {
		t.Helper()
		if err := s.bus.Sweep(); err != nil {
			t.Fatal(err)
		}
		if len(s.pendingFor(t, worker.PID)) != 0 || len(s.events(t, ipc.DeadLetterEvent)) != 4 {
			t.Fatalf("pending %+v, letters %+v", s.pendingFor(t, worker.PID), s.events(t, ipc.DeadLetterEvent))
		}
		for _, message := range box {
			kept, err := s.bus.DeadLetterBody(message.ID)
			if err != nil || kept.Body != message.Body || kept.From != message.From || kept.ID != message.ID {
				t.Fatalf("%s: kept %+v, %v", message.ID, kept, err)
			}
		}
		files, err := os.ReadDir(filepath.Join(root, "bus", "dead_letters"))
		if err != nil || len(files) != 4 {
			t.Fatalf("dead_letters holds %v, %v", files, err)
		}
	}
	check()
	if err := os.WriteFile(mailboxes, before, 0o644); err != nil { // as if the crash came before this write
		t.Fatal(err)
	}
	check()
	if _, err := s.bus.DeadLetterBody("msg-999999"); !errors.Is(err, ipc.ErrNoDeadLetter) {
		t.Fatalf("an unknown ID = %v", err)
	}
	if _, err := s.bus.DeadLetterBody("../mailboxes"); !errors.Is(err, ipc.ErrNoDeadLetter) {
		t.Fatalf("an ID outside the directory = %v", err)
	}
	// Not in the kernel's log: it is rewritten on every append.
	if raw, _ := os.ReadFile(filepath.Join(root, "kernel", "events.json")); strings.Contains(string(raw), "BODY-OPERATOR") {
		t.Fatal("a body reached the kernel's log")
	}
}

// A body that cannot be kept stops the sweep before anything is lost: the mail
// is still there, with no event, and the next sweep finishes.
func TestSweepThatCannotKeepABodyLeavesTheMail(t *testing.T) {
	root := t.TempDir()
	s := openMailState(t, root)
	worker := s.spawn(t, "worker")
	s.bus.SendAsOperator(worker.PID, "BODY-OPERATOR")
	s.kernel.Exit(worker.PID, kernel.ExitStatus{Code: 0, Reason: "done"})
	blocked := filepath.Join(root, "bus", "dead_letters")
	if err := os.WriteFile(blocked, []byte("in the way"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.bus.Sweep(); err == nil {
		t.Fatal("the sweep should fail")
	}
	if len(s.pendingFor(t, worker.PID)) != 1 || len(s.events(t, ipc.DeadLetterEvent)) != 0 {
		t.Fatalf("pending %+v, letters %+v", s.pendingFor(t, worker.PID), s.events(t, ipc.DeadLetterEvent))
	}
	if err := os.Remove(blocked); err != nil {
		t.Fatal(err)
	}
	if err := s.bus.Sweep(); err != nil {
		t.Fatal(err)
	}
	if kept, err := s.bus.DeadLetterBody("msg-000001"); err != nil || kept.Body != "BODY-OPERATOR" {
		t.Fatalf("kept %+v, %v", kept, err)
	}
}

// One notice per ended chain, however much mail: 20 IDs, then a count. The
// next step does not repeat it, and a bounce is not mail.
func TestOneNoticePerEndedChain(t *testing.T) {
	s := openMailState(t, t.TempDir())
	first, second := s.spawn(t, "first"), s.spawn(t, "second")
	sender := s.spawn(t, "lead", "message_agent")
	for i := 0; i < 25; i++ {
		s.bus.Send(sender.PID, first.PID, fmt.Sprintf("to first %d", i))
	}
	s.bus.Send(sender.PID, second.PID, "to second")
	s.kernel.Exit(first.PID, kernel.ExitStatus{Code: 0, Reason: "done"})
	s.kernel.Exit(second.PID, kernel.ExitStatus{Code: 0, Reason: "also done"})
	var histories [][]runtime.Message
	backend := &funcBackend{fn: func(call int, _ context.Context, request runtime.GenerateRequest) (runtime.GenerateResult, error) {
		histories = append(histories, request.History)
		if call < 3 {
			return sayAction("next"), nil
		}
		return exitResult("done"), nil
	}}
	if _, err := s.agent(backend).Run(context.Background(), sender.PID, []runtime.Message{{Role: "user", Content: "go"}}); err != nil {
		t.Fatal(err)
	}
	notes := notesIn(histories[2], "undelivered:")
	if len(notes) != 2 {
		t.Fatalf("notices = %q", notes)
	}
	if !strings.Contains(notes[0], "msg-000020 and 5 more to first") || strings.Contains(notes[0], "msg-000021") {
		t.Fatalf("first chain's notice = %q", notes[0])
	}
	if !strings.Contains(notes[1], "msg-000026 to second") {
		t.Fatalf("second chain's notice = %q", notes[1])
	}
	if pending, _ := s.bus.Pending(); len(pending) != 0 {
		t.Fatalf("a notice went through the bus: %+v", pending)
	}
}

// A bounce is not mail, and nothing happens to one whose sender has ended:
// that sender's own unread mail makes an event and no further notice.
func TestBouncesAreNotBounced(t *testing.T) {
	s := openMailState(t, t.TempDir())
	worker, gone := s.spawn(t, "worker"), s.spawn(t, "gone")
	s.bus.Send(gone.PID, worker.PID, "from a sender that ends")
	s.bus.Send(worker.PID, gone.PID, "to it, also unread")
	s.kernel.Exit(gone.PID, kernel.ExitStatus{Code: 0, Reason: "done"})
	s.kernel.Exit(worker.PID, kernel.ExitStatus{Code: 0, Reason: "done"})
	if err := s.bus.Sweep(); err != nil {
		t.Fatal(err)
	}
	letters := s.events(t, ipc.DeadLetterEvent)
	if len(letters) != 2 {
		t.Fatalf("ipc.dead_letter = %+v", letters)
	}
	if err := s.bus.Sweep(); err != nil {
		t.Fatal(err)
	}
	if pending, _ := s.bus.Pending(); len(pending) != 0 || len(s.events(t, ipc.DeadLetterEvent)) != 2 || len(s.events(t, "ipc.sent")) != 2 {
		t.Fatalf("pending %+v, letters %d, sent %d", pending, len(s.events(t, ipc.DeadLetterEvent)), len(s.events(t, "ipc.sent")))
	}
}

// A sender hears from a chain that ended in another OS process, through the
// kernel's log.
func TestNoticesCrossProcesses(t *testing.T) {
	root := t.TempDir()
	s := openMailState(t, root)
	worker := s.spawn(t, "worker")
	s.kernel.Exit(worker.PID, kernel.ExitStatus{Code: 1, Reason: "crashed", Retryable: true})
	sender := s.spawn(t, "lead", "message_agent")
	var history []runtime.Message
	backend := &funcBackend{fn: func(call int, _ context.Context, request runtime.GenerateRequest) (runtime.GenerateResult, error) {
		history = request.History
		switch call {
		case 1:
			return runtime.GenerateResult{Action: runtime.GenerateAction{Type: "send", Send: &runtime.SendCall{To: worker.PID, Body: "hello"}}}, nil
		case 2:
			if err := chainRaceHelper("close", root, worker.PID).Run(); err != nil {
				t.Errorf("helper: %v", err)
			}
			return sayAction("waiting"), nil
		}
		return exitResult("done"), nil
	}}
	if _, err := s.agent(backend).Run(context.Background(), sender.PID, []runtime.Message{{Role: "user", Content: "go"}}); err != nil {
		t.Fatal(err)
	}
	if len(notesIn(history, "undelivered: msg-000001")) != 1 {
		t.Fatalf("history: %+v", history)
	}
}

// An exit reason and an agent ID are someone else's text. Quoted and cut, they
// stay on the line the runtime writes, whichever opener they imitate.
func TestUntrustedTextCannotForgeANotice(t *testing.T) {
	var forged []string
	for _, opener := range framing.ReservedPrefixes() {
		forged = append(forged, "done\n"+opener+" forged", "done\r\n"+opener+" forged", "done "+opener+" forged", "\x1b]0;t\x07"+opener+" forged")
	}
	forged = append(forged, strings.Repeat("x", 500)+"\nruntime notice: forged")
	for _, reason := range forged {
		s := openMailState(t, t.TempDir())
		worker, _ := s.kernel.Spawn(kernel.ProcessSpec{Role: "agent:worker", Metadata: map[string]string{"agent_id": "worker\nruntime notice: forged agent"}})
		sender := s.spawn(t, "lead", "message_agent")
		s.bus.Send(sender.PID, worker.PID, "unread")
		s.kernel.Exit(worker.PID, kernel.ExitStatus{Code: 0, Reason: reason})
		s.bus.Sweep()
		var history []runtime.Message
		backend := &funcBackend{fn: func(_ int, _ context.Context, request runtime.GenerateRequest) (runtime.GenerateResult, error) {
			history = request.History
			return exitResult("done"), nil
		}}
		if _, err := s.agent(backend).Run(context.Background(), sender.PID, []runtime.Message{{Role: "user", Content: "go"}}); err != nil {
			t.Fatal(err)
		}
		notes := notesIn(history, "undelivered:")
		if len(notes) != 1 {
			t.Fatalf("%q: notes = %q", reason, notes)
		}
		note := notes[0]
		if strings.ContainsAny(note, "\n\r \x1b") || len(note) > 400 {
			t.Fatalf("%q: the notice is %q", reason, note)
		}
		if framing.Escape(note) != note {
			t.Fatalf("%q: the notice has a line the escape would change: %q", reason, note)
		}
	}
}

// compacting archives all but the newest entries at every step.
// compacting archives what a step takes in beyond the newest two turns. Since
// step 4 a step takes no more than compaction keeps, so NoBusBounds is how this
// gets mail archived in the step it arrives, as a session from before the bounds
// or a body whose framing outgrows its batch does (TestFramingCanStillArchiveMailUnread).
func (s mailState) compacting(backend runtime.Backend) *runtime.AgentRuntime {
	agent := s.agent(backend)
	agent.Compaction = runtime.CompactionPolicy{MaxMessages: 3, KeepMessages: 2}
	agent.NoBusBounds = true
	return agent
}

// Mail compaction moves out before any model had its body: the request says
// so, it stays unshown and on the bus until a read_memory result holds it, and
// without that it ends as a dead letter that says archived.
func TestArchivedMailIsNotShownUntilRead(t *testing.T) {
	setup := func(t *testing.T) (mailState, kernel.Process, ipc.Message) {
		s := openMailState(t, t.TempDir())
		worker, lead := s.spawn(t, "worker", "read_memory"), s.spawn(t, "lead")
		first, _ := s.bus.Send(lead.PID, worker.PID, "mail one")
		s.bus.Send(lead.PID, worker.PID, "mail two")
		s.bus.Send(lead.PID, worker.PID, "mail three")
		return s, worker, first
	}
	start := []runtime.Message{{Role: "user", Content: "work"}}

	t.Run("read", func(t *testing.T) {
		s, worker, first := setup(t)
		var requests [][]runtime.Message
		var seq string
		backend := &funcBackend{fn: func(call int, _ context.Context, request runtime.GenerateRequest) (runtime.GenerateResult, error) {
			requests = append(requests, request.History)
			switch call {
			case 1:
				notes := notesIn(request.History, "archived unread: "+first.ID)
				if len(notes) != 1 {
					t.Fatalf("first request: %+v", request.History)
				}
				fields := strings.Fields(notes[0])
				seq = fields[slices.Index(fields, "seq")+1]
				if !strings.Contains(notes[0], "from lead (") {
					t.Fatalf("the notice = %q", notes[0])
				}
				if box := s.pendingFor(t, worker.PID); !slices.ContainsFunc(box, func(m ipc.Message) bool { return m.ID == first.ID && m.Archived != "" }) {
					t.Fatalf("it should still be on the bus: %+v", box)
				}
				return toolAction("read_memory", map[string]string{"from": seq, "to": seq}), nil
			case 2:
				if box := s.pendingFor(t, worker.PID); !slices.ContainsFunc(box, func(m ipc.Message) bool { return m.ID == first.ID }) {
					t.Fatalf("the request with the result has not gone out yet: %+v", box)
				}
				return sayAction("noted"), nil
			case 3:
				if box := s.pendingFor(t, worker.PID); len(box) != 0 {
					t.Fatalf("shown now, so acknowledged: %+v", box)
				}
			}
			return exitResult("done"), nil
		}}
		result, err := s.compacting(backend).Run(context.Background(), worker.PID, start)
		if err != nil || result.ExitStatus.Code != 0 {
			t.Fatalf("run = %+v, %v", result, err)
		}
		if letters := s.events(t, ipc.DeadLetterEvent); len(letters) != 0 {
			t.Fatalf("ipc.dead_letter = %+v", letters)
		}
	})
	t.Run("not read", func(t *testing.T) {
		s, worker, first := setup(t)
		backend := &funcBackend{fn: func(call int, _ context.Context, _ runtime.GenerateRequest) (runtime.GenerateResult, error) {
			return exitResult("done"), nil
		}}
		if _, err := s.compacting(backend).Run(context.Background(), worker.PID, start); err != nil {
			t.Fatal(err)
		}
		var found []kernel.Event
		for _, letter := range s.events(t, ipc.DeadLetterEvent) {
			if letter.Data["id"] == first.ID {
				found = append(found, letter)
			}
		}
		if len(found) != 1 || found[0].Data["reason"] != ipc.ReasonNotShown || found[0].Data["detail"] != ipc.DetailArchived || found[0].Data["segment"] == "" {
			t.Fatalf("ipc.dead_letter = %+v", found)
		}
		if pending, _ := s.bus.Pending(); len(pending) != 0 {
			t.Fatalf("left pending: %+v", pending)
		}
	})
	t.Run("a search only quotes it", func(t *testing.T) {
		s, worker, first := setup(t)
		backend := &funcBackend{fn: func(call int, _ context.Context, _ runtime.GenerateRequest) (runtime.GenerateResult, error) {
			if call == 1 {
				return toolAction("search_memory", map[string]string{"query": "mail one"}), nil
			}
			return exitResult("done"), nil
		}}
		if _, err := s.compacting(backend).Run(context.Background(), worker.PID, start); err != nil {
			t.Fatal(err)
		}
		if !slices.ContainsFunc(s.events(t, ipc.DeadLetterEvent), func(e kernel.Event) bool { return e.Data["id"] == first.ID && e.Data["detail"] == ipc.DetailArchived }) {
			t.Fatalf("ipc.dead_letter = %+v", s.events(t, ipc.DeadLetterEvent))
		}
	})
}

// A call that returned shows the mail, and only then is it acknowledged. A call
// that failed does not, and the next one shows it.
func TestOnlyACallThatReturnedShowsMail(t *testing.T) {
	s := openMailState(t, t.TempDir())
	worker, lead := s.spawn(t, "worker"), s.spawn(t, "lead")
	c1 := s.park(t, worker)
	s.bus.Send(lead.PID, worker.PID, "mail a")
	failing := &funcBackend{fn: func(int, context.Context, runtime.GenerateRequest) (runtime.GenerateResult, error) {
		return runtime.GenerateResult{}, errors.New("connection reset")
	}}
	// Retries running out saves a checkpoint, which is when an ack would go.
	agent := s.agent(failing)
	agent.BackendRetry = runtime.BackendRetryPolicy{Attempts: 1}
	first, _ := agent.Resume(context.Background(), worker.PID, c1, nil, runtime.RunOptions{})
	if first.CheckpointID == c1 || first.CheckpointID == "" {
		t.Fatalf("the failed run = %+v", first)
	}
	if box := s.pendingFor(t, first.PID); len(box) != 1 {
		t.Fatalf("after a failed call: %+v", box)
	}
	var history string
	if _, err := s.agent(listening(&history)).Recover(context.Background(), first.PID, "", nil, runtime.RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if bodiesIn(history) != "mail a" || len(s.events(t, ipc.DeadLetterEvent)) != 0 {
		t.Fatalf("history %q, letters %+v", history, s.events(t, ipc.DeadLetterEvent))
	}
	if pending, _ := s.bus.Pending(); len(pending) != 0 {
		t.Fatalf("left pending: %+v", pending)
	}
}

// After a crash in the gap between a call and its checkpoint, no record shows
// the mail was shown. If no successor comes, it is not_shown, although a model
// may have read it. The error is in that direction on purpose, and neither the
// event nor the notice says more than the record does.
func TestCrashAfterACallIsNotShown(t *testing.T) {
	s := openMailState(t, t.TempDir())
	worker := s.spawn(t, "worker")
	sender := s.spawn(t, "lead", "message_agent")
	s.bus.Send(sender.PID, worker.PID, "handed over")
	if _, err := s.bus.Receive(worker.PID, false); err != nil {
		t.Fatal(err)
	}
	s.kernel.Exit(worker.PID, kernel.ExitStatus{Code: 1, Reason: "killed", Retryable: true})
	s.kernel.Close(worker.PID, "declined")
	s.bus.Sweep()
	letters := s.events(t, ipc.DeadLetterEvent)
	if len(letters) != 1 || letters[0].Data["reason"] != ipc.ReasonNotShown {
		t.Fatalf("ipc.dead_letter = %+v", letters)
	}
	var history []runtime.Message
	backend := &funcBackend{fn: func(_ int, _ context.Context, request runtime.GenerateRequest) (runtime.GenerateResult, error) {
		history = request.History
		return exitResult("done"), nil
	}}
	if _, err := s.agent(backend).Run(context.Background(), sender.PID, []runtime.Message{{Role: "user", Content: "go"}}); err != nil {
		t.Fatal(err)
	}
	notes := notesIn(history, "undelivered:")
	if len(notes) != 1 || !strings.Contains(notes[0], "no record that a model read this") || strings.Contains(notes[0], "was not read") {
		t.Fatalf("notes = %q", notes)
	}
}

// A crash is not the only way. Mail read in a call that returned, then a denied
// tool or a malformed action with no ContinueOnToolError, exits through a path
// that saves no checkpoint. It reads the same, and says no more than that.
func TestExitWithNoCheckpointAfterTheCallIsNotShown(t *testing.T) {
	for name, action := range map[string]runtime.GenerateResult{
		"denied tool":      toolAction("write_file", map[string]string{"path": "x", "content": "y"}),
		"malformed action": {Action: runtime.GenerateAction{Type: "send"}},
	} {
		t.Run(name, func(t *testing.T) {
			s := openMailState(t, t.TempDir())
			worker, lead := s.spawn(t, "worker"), s.spawn(t, "lead")
			sent, _ := s.bus.Send(lead.PID, worker.PID, "read in a call that returned")
			backend := &funcBackend{fn: func(int, context.Context, runtime.GenerateRequest) (runtime.GenerateResult, error) {
				return action, nil
			}}
			result, _ := s.agent(backend).Run(context.Background(), worker.PID, []runtime.Message{{Role: "user", Content: "go"}})
			if result.ExitStatus.Code == 0 || result.ExitStatus.Retryable {
				t.Fatalf("the exit = %+v", result.ExitStatus)
			}
			letters := s.events(t, ipc.DeadLetterEvent)
			if len(letters) != 1 || letters[0].Data["id"] != sent.ID || letters[0].Data["reason"] != ipc.ReasonNotShown {
				t.Fatalf("ipc.dead_letter = %+v", letters)
			}
		})
	}
}

// Shown is per session. Mail shown only in steps a rollback undid goes back; if
// the chain then ends before a model has it, the reason is what this session
// knows, and the detail says a model may have read it in the steps undone.
func TestRollbackAndDeadLetters(t *testing.T) {
	setup := func(t *testing.T) (mailState, kernel.Process, string, ipc.Message) {
		s := openMailState(t, t.TempDir())
		worker, lead := s.spawn(t, "worker"), s.spawn(t, "lead")
		c1 := s.park(t, worker)
		a, _ := s.bus.Send(lead.PID, worker.PID, "mail a")
		first, err := s.agent(crashing()).Resume(context.Background(), worker.PID, c1, nil, runtime.RunOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if pending, _ := s.bus.Pending(); len(pending) != 0 {
			t.Fatalf("shown, so acknowledged: %+v, %+v", pending, first)
		}
		return s, worker, c1, a
	}
	t.Run("handed to the successor, not shown", func(t *testing.T) {
		s, worker, c1, a := setup(t)
		failing := &funcBackend{fn: func(int, context.Context, runtime.GenerateRequest) (runtime.GenerateResult, error) {
			return runtime.GenerateResult{}, errors.New("connection reset")
		}}
		next, _ := s.agent(failing).Recover(context.Background(), worker.PID, c1, nil, runtime.RunOptions{Rollback: true})
		s.kernel.Close(next.PID, "declined")
		s.bus.Sweep()
		letters := s.events(t, ipc.DeadLetterEvent)
		if len(letters) != 1 || letters[0].Data["id"] != a.ID || letters[0].Data["reason"] != ipc.ReasonNotShown || letters[0].Data["detail"] != ipc.DetailRolledBack {
			t.Fatalf("ipc.dead_letter = %+v", letters)
		}
	})
	t.Run("never handed to the successor", func(t *testing.T) {
		s, worker, _, a := setup(t)
		next, err := s.kernel.Supersede(worker.PID, kernel.ProcessSpec{Role: worker.Role}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.bus.Redeliver(next.PID, []ipc.Redelivery{{Message: a, Record: "art-x"}}, "rollback"); err != nil {
			t.Fatal(err)
		}
		s.kernel.Exit(next.PID, kernel.ExitStatus{Code: 1, Reason: "killed", Retryable: true})
		s.kernel.Close(next.PID, "declined")
		s.bus.Sweep()
		letters := s.events(t, ipc.DeadLetterEvent)
		if len(letters) != 1 || letters[0].Data["reason"] != ipc.ReasonUnread || letters[0].Data["detail"] != ipc.DetailRolledBack {
			t.Fatalf("ipc.dead_letter = %+v", letters)
		}
	})
	t.Run("shown again, no letter", func(t *testing.T) {
		s, worker, c1, _ := setup(t)
		var history string
		if _, err := s.agent(listening(&history)).Recover(context.Background(), worker.PID, c1, nil, runtime.RunOptions{Rollback: true}); err != nil {
			t.Fatal(err)
		}
		if letters := s.events(t, ipc.DeadLetterEvent); len(letters) != 0 {
			t.Fatalf("ipc.dead_letter = %+v", letters)
		}
	})
}

// NoticesSeen is per session too. A rollback past the notice tells the session
// again, since its history has not heard, once. A plain Recover does not.
func TestRollbackRepeatsANoticeOnce(t *testing.T) {
	s := openMailState(t, t.TempDir())
	gone := s.spawn(t, "gone")
	sender := s.spawn(t, "lead", "message_agent")
	c1 := s.park(t, sender)
	s.bus.Send(sender.PID, gone.PID, "unread")
	s.kernel.Exit(gone.PID, kernel.ExitStatus{Code: 0, Reason: "done"})
	s.bus.Sweep()
	first, err := s.agent(crashing()).Resume(context.Background(), sender.PID, c1, nil, runtime.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.agent(nil).RestoreCheckpoint(first.ExitStatus.Checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if got := notesIn(session.Messages, "undelivered:"); len(got) != 1 || session.NoticesSeen == 0 {
		t.Fatalf("the first run heard: %v, seen %d", got, session.NoticesSeen)
	}
	var plain []runtime.Message
	second, err := s.agent(&funcBackend{fn: func(_ int, _ context.Context, request runtime.GenerateRequest) (runtime.GenerateResult, error) {
		plain = request.History
		return exitAction(1, true, "again"), nil
	}}).Recover(context.Background(), first.PID, "", nil, runtime.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := notesIn(plain, "undelivered:"); len(got) != 1 {
		t.Fatalf("a plain Recover continues the same history: %v", got)
	}
	var rolled []runtime.Message
	if _, err := s.agent(&funcBackend{fn: func(_ int, _ context.Context, request runtime.GenerateRequest) (runtime.GenerateResult, error) {
		rolled = request.History
		return exitResult("done"), nil
	}}).Recover(context.Background(), second.PID, c1, nil, runtime.RunOptions{Rollback: true}); err != nil {
		t.Fatal(err)
	}
	if got := notesIn(rolled, "undelivered:"); len(got) != 1 {
		t.Fatalf("after the rollback: %v", got)
	}
}

// A publisher never named the recipient, so its topic mail going unread makes
// the event and no notice. The direct send from the same sender still bounces.
func TestTopicMailGetsTheEventOnly(t *testing.T) {
	s := openMailState(t, t.TempDir())
	worker := s.spawn(t, "worker")
	sender := s.spawn(t, "lead", "message_agent")
	s.bus.Subscribe(worker.PID, "news")
	s.bus.Publish(sender.PID, "news", "published")
	s.bus.Send(sender.PID, worker.PID, "sent")
	s.kernel.Exit(worker.PID, kernel.ExitStatus{Code: 0, Reason: "done"})
	var history []runtime.Message
	backend := &funcBackend{fn: func(_ int, _ context.Context, request runtime.GenerateRequest) (runtime.GenerateResult, error) {
		history = request.History
		return exitResult("done"), nil
	}}
	if _, err := s.agent(backend).Run(context.Background(), sender.PID, []runtime.Message{{Role: "user", Content: "go"}}); err != nil {
		t.Fatal(err)
	}
	letters := s.events(t, ipc.DeadLetterEvent)
	if len(letters) != 2 || letters[0].Data["topic"] != "news" {
		t.Fatalf("ipc.dead_letter = %+v", letters)
	}
	notes := notesIn(history, "undelivered:")
	if len(notes) != 1 || !strings.Contains(notes[0], "msg-000002") || strings.Contains(notes[0], "msg-000001") {
		t.Fatalf("notes = %q", notes)
	}
}

// The operator has no chain to tell: its unread mail makes the event alone.
func TestOperatorMailGetsTheEventOnly(t *testing.T) {
	s := openMailState(t, t.TempDir())
	worker := s.spawn(t, "worker")
	s.bus.SendAsOperator(worker.PID, "are you there")
	s.kernel.Exit(worker.PID, kernel.ExitStatus{Code: 0, Reason: "done"})
	s.bus.Sweep()
	letters := s.events(t, ipc.DeadLetterEvent)
	if len(letters) != 1 || letters[0].Data["from"] != ipc.OperatorSender {
		t.Fatalf("ipc.dead_letter = %+v", letters)
	}
}

// Mail is delivered to a checkpoint from before this step: it has no unshown
// list, so it all counts as shown, and is acknowledged as before.
func TestACheckpointWithoutTheListCountsAsShown(t *testing.T) {
	s := openMailState(t, t.TempDir())
	worker, lead := s.spawn(t, "worker"), s.spawn(t, "lead")
	a, _ := s.bus.Send(lead.PID, worker.PID, "recorded by an old build")
	record, err := s.store.CreateJSON("checkpoint", worker.PID, runtime.Session{Messages: []runtime.Message{{Role: "user", Content: "work"}}, MailboxSeen: []string{a.ID}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.agent(&funcBackend{fn: func(int, context.Context, runtime.GenerateRequest) (runtime.GenerateResult, error) {
		return exitResult("done"), nil
	}}).Resume(context.Background(), worker.PID, record.ID, nil, runtime.RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if letters := s.events(t, ipc.DeadLetterEvent); len(letters) != 0 {
		t.Fatalf("ipc.dead_letter = %+v", letters)
	}
	if pending, _ := s.bus.Pending(); len(pending) != 0 {
		t.Fatalf("left pending: %+v", pending)
	}
}

// N OS processes send to a live process while another OS process ends its
// chain and two sweep. Every send that succeeded is a dead letter, once, every
// other was refused, and nothing is left in the mailbox.
func TestSendRacesAChainsEnd(t *testing.T) {
	trials := 3
	if raw := os.Getenv("AOS_IPC_TRIALS"); raw != "" {
		trials, _ = strconv.Atoi(raw)
	}
	for trial := 0; trial < trials; trial++ {
		chainEndRace(t, trial)
	}
}

func chainEndRace(t *testing.T, trial int) {
	const senders = 4
	root := t.TempDir()
	s := openMailState(t, root)
	worker := s.spawn(t, "worker")
	scratch := t.TempDir()
	var cmds []*exec.Cmd
	var outs []string
	for i := 0; i < senders; i++ {
		from := s.spawn(t, fmt.Sprintf("sender%d", i)).PID
		out := filepath.Join(scratch, fmt.Sprintf("sent-%d.tsv", i))
		outs = append(outs, out)
		cmds = append(cmds, chainRaceHelper("send-until-refused", root, from, worker.PID, out))
	}
	for _, cmd := range cmds {
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
	}
	watchdog := time.AfterFunc(90*time.Second, func() {
		for _, cmd := range cmds {
			cmd.Process.Signal(syscall.SIGKILL)
		}
	})
	defer watchdog.Stop()
	for deadline := time.Now().Add(60 * time.Second); len(s.pendingFor(t, worker.PID)) < 2*senders; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("trial %d: no mail arrived", trial)
		}
	}
	ending := chainRaceHelper("end", root, worker.PID)
	sweeping := chainRaceHelper("sweep", root)
	if err := ending.Start(); err != nil {
		t.Fatal(err)
	}
	if err := sweeping.Start(); err != nil {
		t.Fatal(err)
	}
	cmds = append(cmds, ending, sweeping)
	for _, cmd := range cmds {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("trial %d: helper: %v", trial, err)
		}
	}
	sent := map[string]bool{}
	refused := 0
	for _, path := range outs {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			if line == "refused" {
				refused++
			} else if line != "" {
				sent[line] = true
			}
		}
	}
	letters := map[string]int{}
	for _, event := range s.events(t, ipc.DeadLetterEvent) {
		letters[event.Data["id"]]++
	}
	lost, doubled := 0, 0
	for id := range sent {
		if letters[id] == 0 {
			lost++
		}
		if letters[id] > 1 {
			doubled++
		}
	}
	pending, _ := s.bus.Pending()
	t.Logf("trial %d: %d sent, %d refused, %d dead letters, stranded %d, doubled %d, left pending %d", trial, len(sent), refused, len(letters), lost, doubled, len(pending))
	if lost != 0 || doubled != 0 || len(pending) != 0 || len(letters) != len(sent) || refused != senders {
		t.Fatalf("trial %d: stranded %d, doubled %d, pending %+v, %d letters for %d sends, %d refusals", trial, lost, doubled, pending, len(letters), len(sent), refused)
	}
}
