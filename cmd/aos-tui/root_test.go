package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"aos/internal/artifact"
	"aos/internal/ipc"
	"aos/internal/kernel"
	"aos/internal/runtime"
	"aos/internal/tools"
	"aos/internal/tui/render"
)

type rootFixture struct {
	dir    string
	kernel *kernel.Kernel
	store  *artifact.Store
	bus    *ipc.Bus
	spawn  func(continues string) (kernel.Process, error)
}

func newRootFixture(t *testing.T) rootFixture {
	t.Helper()
	dir := t.TempDir()
	k, err := kernel.New(filepath.Join(dir, "kernel"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := artifact.New(filepath.Join(dir, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	bus, err := ipc.New(filepath.Join(dir, "bus"), k)
	if err != nil {
		t.Fatal(err)
	}
	spawn := func(continues string) (kernel.Process, error) {
		metadata := map[string]string{"agent_id": "interactive"}
		if continues != "" {
			metadata[runtime.ContinuesMetadata] = continues
		}
		return k.Spawn(kernel.ProcessSpec{Role: "agent:interactive", Metadata: metadata})
	}
	return rootFixture{dir: dir, kernel: k, store: store, bus: bus, spawn: spawn}
}

func (f rootFixture) runtime(backend runtime.Backend) *runtime.AgentRuntime {
	return &runtime.AgentRuntime{Kernel: f.kernel, Artifacts: f.store, Bus: f.bus, Tools: tools.NewRuntime(), Backend: backend, MaxSteps: 6, KeepMailAtExit: true}
}

// say runs one TUI turn for line, as the loop does.
func (f rootFixture) say(t *testing.T, r *runtime.AgentRuntime, root *tuiRoot, line string) (string, runtime.RunResult, error) {
	t.Helper()
	note, err := continueEnded(f.kernel, f.store, f.bus, root, f.spawn)
	if err != nil {
		t.Fatalf("continueEnded: %v", err)
	}
	appended, err := deliver(f.bus, f.store, root, line)
	if err != nil {
		t.Fatalf("deliver: %v", err)
	}
	result, err := runTurn(context.Background(), r, root, appended, runtime.RunOptions{StopOnAssistantMessage: true, NewTurn: true})
	root.record(f.store, result)
	return note, result, err
}

func message(text string) runtime.GenerateResult {
	return runtime.GenerateResult{Action: runtime.GenerateAction{Type: "message", Message: text}}
}

func exitDone() runtime.GenerateResult {
	return runtime.GenerateResult{Action: runtime.GenerateAction{Type: "exit", ExitStatus: &kernel.ExitStatus{Code: 0, Reason: "done", Retryable: false}}}
}

// After the agent exits with code 0, the next line continues the
// conversation in a new agent, which sees what was said and the new line.
// Recover and Resume still refuse the exited agent.
func TestExitedRootContinuesInANewAgent(t *testing.T) {
	f := newRootFixture(t)
	backend := &runtime.FakeBackend{Steps: []runtime.GenerateResult{message("Uptime Kuma runs on 10.0.0.5"), exitDone(), message("port 3001")}}
	r := f.runtime(backend)
	first, err := f.spawn("")
	if err != nil {
		t.Fatal(err)
	}
	root := tuiRoot{process: first}

	if _, _, err := f.say(t, r, &root, "find the host running Uptime Kuma"); err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	_, exited, err := f.say(t, r, &root, "thanks")
	if err != nil || exited.ExitStatus.Reason != "done" {
		t.Fatalf("turn 2 should exit: %+v %v", exited.ExitStatus, err)
	}
	ended := root.checkpointID

	note, result, err := f.say(t, r, &root, "and its port?")
	if err != nil {
		t.Fatalf("turn 3: %v", err)
	}
	if root.process.PID == first.PID || !strings.Contains(note, "new agent "+root.process.PID) || !strings.Contains(note, first.PID+" exited (done)") {
		t.Fatalf("turn 3 should run in a new agent: pid %s, note %q", root.process.PID, note)
	}
	if root.checkpointID == "" || root.checkpointID == ended || root.seed != nil {
		t.Fatalf("the new agent's checkpoint should replace the ended one: %q (ended %q), seed %d", root.checkpointID, ended, len(root.seed))
	}
	last := backend.Requests[len(backend.Requests)-1]
	var said []string
	for _, m := range last.History {
		if m.Role == "user" || m.Role == "assistant" {
			said = append(said, m.Content)
		}
	}
	got := strings.Join(said, " | ")
	for _, want := range []string{"find the host running Uptime Kuma", "Uptime Kuma runs on 10.0.0.5", "thanks", "and its port?"} {
		if !strings.Contains(got, want) {
			t.Fatalf("the new agent should see %q; it saw %s", want, got)
		}
	}
	if result.Session.Messages[len(result.Session.Messages)-1].Content != "port 3001" {
		t.Fatalf("the new agent should answer: %+v", result.Session.Messages)
	}

	// The final exit stays final outside the TUI.
	if _, err := r.Recover(context.Background(), first.PID, ended, nil, runtime.RunOptions{}); !errors.Is(err, runtime.ErrNotRecoverable) {
		t.Fatalf("Recover of a final exit = %v", err)
	}
	if _, err := r.Resume(context.Background(), first.PID, ended, nil, runtime.RunOptions{}); !errors.Is(err, runtime.ErrNotRecoverable) {
		t.Fatalf("Resume of a final exit = %v", err)
	}
}

// Each turn has its own step allowance, and the step note and warning
// count against it: the second turn is warned again at its own last step,
// not at the first turn's.
func TestStepWarningCountsAgainstTheTurn(t *testing.T) {
	f := newRootFixture(t)
	search := runtime.GenerateResult{Action: runtime.GenerateAction{Type: "tool", ToolCall: &runtime.ToolCall{Name: "search_memory", Args: map[string]string{"query": "port"}}}}
	var steps []runtime.GenerateResult
	for turn := 1; turn <= 2; turn++ {
		for range 5 {
			steps = append(steps, search)
		}
		steps = append(steps, message(fmt.Sprintf("turn %d done", turn)))
	}
	backend := &runtime.FakeBackend{Steps: steps}
	r := f.runtime(backend)
	first, _ := f.spawn("")
	root := tuiRoot{process: first}
	for turn := 1; turn <= 2; turn++ {
		if _, result, err := f.say(t, r, &root, fmt.Sprintf("turn %d", turn)); err != nil || !result.Yielded {
			t.Fatalf("turn %d: %+v %v", turn, result.ExitStatus, err)
		}
	}
	if len(backend.Requests) != 12 {
		t.Fatalf("calls = %d, want 6 per turn", len(backend.Requests))
	}
	// Compaction may archive the first turn's notes before the last call,
	// so they are gathered from every call, once each.
	var budget []string
	seen := map[int]bool{}
	for _, request := range backend.Requests {
		for _, m := range request.History {
			if strings.HasPrefix(m.Content, "budget ") && !seen[m.Seq] {
				seen[m.Seq] = true
				budget = append(budget, m.Content)
			}
		}
	}
	note := "budget steps=6 used=5 left=1 (strict: the run is stopped after the last step)"
	warning := "budget warning: 1 of 6 steps are left, this one included. Commit your best answer now: write or submit what you have, then exit."
	if strings.Join(budget, "\n") != strings.Join([]string{note, warning, note, warning}, "\n") {
		t.Fatalf("each turn's last step should carry the note and the warning:\n%s", strings.Join(budget, "\n"))
	}
	if root.session.Usage.Steps != 12 || root.session.StepBase != 6 {
		t.Fatalf("usage %+v, step base %d", root.session.Usage, root.session.StepBase)
	}
}

// A turn that fails with a zero result keeps the last good checkpoint,
// session and process, and a line its recovery was to carry is reported as
// not delivered.
func TestFailedTurnKeepsTheLastGoodState(t *testing.T) {
	f := newRootFixture(t)
	r := f.runtime(&runtime.FakeBackend{Steps: []runtime.GenerateResult{message("hi")}})
	first, _ := f.spawn("")
	root := tuiRoot{process: first}
	if _, _, err := f.say(t, r, &root, "hello"); err != nil {
		t.Fatal(err)
	}
	f.kernel.Exit(first.PID, kernel.ExitStatus{Code: 0, Reason: "done"})
	good := root

	// Without continueEnded (the old loop), the line goes with a recovery
	// that Recover refuses.
	appended, err := deliver(f.bus, f.store, &root, "are you there?")
	if err != nil || len(appended) != 1 {
		t.Fatalf("deliver to an exited agent: %v %v", appended, err)
	}
	result, err := runTurn(context.Background(), r, &root, appended, runtime.RunOptions{})
	if !errors.Is(err, runtime.ErrNotRecoverable) || result.CheckpointID != "" {
		t.Fatalf("resume of a final exit: %+v %v", result, err)
	}
	root.record(f.store, result)
	if root.checkpointID != good.checkpointID || root.process.PID != good.process.PID || len(root.session.Messages) != len(good.session.Messages) || len(root.session.Messages) == 0 {
		t.Fatalf("a failed turn changed the root: %+v, was %+v", root, good)
	}
	if !lostLine(appended, good.checkpointID, result) {
		t.Fatal("the line should be reported as not delivered")
	}
	if lostLine(nil, good.checkpointID, result) {
		t.Fatal("a line in the mailbox is not lost")
	}
	// A recovery that failed after spawning reports the old checkpoint.
	if !lostLine(appended, good.checkpointID, runtime.RunResult{PID: "pid-9", CheckpointID: good.checkpointID}) {
		t.Fatal("a line that reached no new checkpoint should be reported as not delivered")
	}
	if lostLine(appended, good.checkpointID, runtime.RunResult{CheckpointID: "art-new"}) {
		t.Fatal("a line that reached a new checkpoint is delivered")
	}

	// A new root keeps its seed until its first checkpoint.
	seeded := tuiRoot{process: first, seed: []runtime.Message{{Role: "user", Content: "hello"}}}
	seeded.record(f.store, runtime.RunResult{})
	if len(seeded.seed) != 1 || seeded.checkpointID != "" {
		t.Fatalf("a failed first turn lost the seed: %+v", seeded)
	}
}

// continueEnded leaves live and recoverable roots alone, and moves mail an
// ended root never read to the new one.
func TestContinueEndedOnlyReplacesFinalExits(t *testing.T) {
	f := newRootFixture(t)
	live, _ := f.spawn("")
	root := tuiRoot{process: live, checkpointID: "art-x"}
	if note, err := continueEnded(f.kernel, f.store, f.bus, &root, f.spawn); note != "" || err != nil || root.process.PID != live.PID {
		t.Fatalf("live root replaced: %q %v", note, err)
	}
	f.kernel.Exit(live.PID, kernel.ExitStatus{Code: 1, Reason: "backend down", Retryable: true})
	if note, err := continueEnded(f.kernel, f.store, f.bus, &root, f.spawn); note != "" || err != nil || root.process.PID != live.PID {
		t.Fatalf("recoverable root replaced: %q %v", note, err)
	}

	// No checkpoint yet: even a retryable exit cannot be recovered.
	unread, _ := f.spawn("")
	if _, err := f.bus.SendAsOperator(unread.PID, "first line"); err != nil {
		t.Fatal(err)
	}
	f.kernel.Exit(unread.PID, kernel.ExitStatus{Code: 1, Reason: "crashed", Retryable: true})
	root = tuiRoot{process: unread, seed: []runtime.Message{{Role: "user", Content: "earlier"}}}
	note, err := continueEnded(f.kernel, f.store, f.bus, &root, f.spawn)
	if err != nil || note == "" || root.process.PID == unread.PID || len(root.seed) != 1 {
		t.Fatalf("ended root without a checkpoint: %q %v %+v", note, err, root)
	}
	pending, _ := f.bus.Pending()
	if len(pending[unread.PID]) != 0 || len(pending[root.process.PID]) != 1 || pending[root.process.PID][0].Body != "first line" {
		t.Fatalf("unread mail should move to the new root: %+v", pending)
	}
}

// The TUI runs its root with ExitEndsTurn: an exit ends the turn, and the
// next line goes to the same agent, with no new one taking over.
func TestExitEndsTheTurnInTheSameAgent(t *testing.T) {
	ui := testPalette()
	options := interactiveRunOptions(ui, render.NewStreamRenderer(ui), render.NewFooterBar(ui), func(string, string) {})
	if !options.ExitEndsTurn || !options.StopOnAssistantMessage {
		t.Fatalf("interactive options: ExitEndsTurn=%t StopOnAssistantMessage=%t", options.ExitEndsTurn, options.StopOnAssistantMessage)
	}
	f := newRootFixture(t)
	backend := &runtime.FakeBackend{Steps: []runtime.GenerateResult{message("Uptime Kuma runs on 10.0.0.5"), exitDone(), message("port 3001")}}
	r := f.runtime(backend)
	first, _ := f.spawn("")
	root := tuiRoot{process: first}
	turn := func(line string) (string, runtime.RunResult) {
		t.Helper()
		note, err := continueEnded(f.kernel, f.store, f.bus, &root, f.spawn)
		if err != nil {
			t.Fatal(err)
		}
		appended, err := deliver(f.bus, f.store, &root, line)
		if err != nil || appended != nil {
			t.Fatalf("deliver %q: %v %v", line, appended, err)
		}
		result, err := runTurn(context.Background(), r, &root, appended, runtime.RunOptions{StopOnAssistantMessage: true, NewTurn: true, ExitEndsTurn: options.ExitEndsTurn})
		if err != nil {
			t.Fatalf("turn %q: %v", line, err)
		}
		root.record(f.store, result)
		return note, result
	}
	turn("find the host running Uptime Kuma")
	if _, ended := turn("thanks"); ended.TurnExit == nil || ended.TurnExit.Reason != "done" {
		t.Fatalf("exit should end the turn: %+v", ended)
	}
	note, result := turn("and its port?")
	if note != "" || root.process.PID != first.PID || result.PID != first.PID {
		t.Fatalf("the same agent should continue: note %q, pid %s", note, root.process.PID)
	}
	if last := result.Session.Messages[len(result.Session.Messages)-1]; last.Content != "port 3001" {
		t.Fatalf("answer: %+v", last)
	}
	text := captureFooter(t, func(footer *render.FooterBar) {
		printResult(ui, footer, runtime.RunResult{Yielded: true, TurnExit: &kernel.ExitStatus{Reason: "done"}})
	})
	if !strings.Contains(text, "agent ended its turn: done") || strings.Contains(text, "agent exited") {
		t.Fatalf("turn end printed as %q", text)
	}
}

// captureFooter returns what fn printed through a footer bar, which writes
// to standard output.
func captureFooter(t *testing.T, fn func(*render.FooterBar)) string {
	t.Helper()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = write
	fn(render.NewFooterBar(testPalette()))
	os.Stdout = stdout
	write.Close()
	out, _ := io.ReadAll(read)
	return string(out)
}

// A root that exited retryably holds a line for its recovery, in the bus, so
// a turn that fails before a checkpoint does not lose it. A root whose chain
// has ended cannot hold one, and the next turn carries it.
func TestDeliverFollowsTheChain(t *testing.T) {
	f := newRootFixture(t)
	first, _ := f.spawn("")
	root := tuiRoot{process: first}
	f.kernel.Exit(first.PID, kernel.ExitStatus{Code: 1, Reason: "crashed", Retryable: true})
	appended, err := deliver(f.bus, f.store, &root, "still there?")
	if err != nil || appended != nil {
		t.Fatalf("deliver to an open chain = %v, %v", appended, err)
	}
	if pending, _ := f.bus.Pending(); len(pending[first.PID]) != 1 {
		t.Fatalf("pending = %+v", pending)
	}
	f.kernel.Close(first.PID, "declined")
	appended, err = deliver(f.bus, f.store, &root, "and now?")
	if err != nil || len(appended) != 1 || appended[0].Content != "and now?" {
		t.Fatalf("deliver to a closed chain = %v, %v", appended, err)
	}
}

// A root replaced by a new one is a chain that nothing will recover, so it is
// closed, after its mail and subscriptions have moved.
func TestContinueEndedClosesTheChainItReplaces(t *testing.T) {
	f := newRootFixture(t)
	first, _ := f.spawn("")
	root := tuiRoot{process: first}
	f.bus.Subscribe(first.PID, "news")
	f.bus.SendAsOperator(first.PID, "unread")
	// Retryable, but with no checkpoint to recover from.
	f.kernel.Exit(first.PID, kernel.ExitStatus{Code: 1, Reason: "crashed", Retryable: true})
	note, err := continueEnded(f.kernel, f.store, f.bus, &root, f.spawn)
	if err != nil || note == "" || root.process.PID == first.PID {
		t.Fatalf("continueEnded = %q, %v, root %s", note, err, root.process.PID)
	}
	old, _ := f.kernel.Get(first.PID)
	if !old.Closed {
		t.Fatalf("the replaced root = %+v", old)
	}
	if topics, _ := f.bus.Subscriptions(root.process.PID); len(topics) != 1 {
		t.Fatalf("the new root's topics = %v", topics)
	}
	if pending, _ := f.bus.Pending(); len(pending[root.process.PID]) != 1 || len(pending[first.PID]) != 0 {
		t.Fatalf("pending = %+v", pending)
	}
}

// The old root is closed before its mail moves, so a send between the two is
// refused rather than stranded. A Transfer that fails shows the order, and the
// next attempt still moves the mail.
func TestContinueEndedClosesBeforeItMovesTheMail(t *testing.T) {
	f := newRootFixture(t)
	first, _ := f.spawn("")
	root := tuiRoot{process: first}
	f.bus.SendAsOperator(first.PID, "unread")
	f.kernel.Exit(first.PID, kernel.ExitStatus{Code: 1, Reason: "crashed", Retryable: true})
	mailboxes := filepath.Join(f.dir, "bus", "mailboxes.json")
	good, err := os.ReadFile(mailboxes)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mailboxes, []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := continueEnded(f.kernel, f.store, f.bus, &root, f.spawn); err == nil {
		t.Fatal("the transfer should fail")
	}
	if old, _ := f.kernel.Get(first.PID); !old.Closed {
		t.Fatalf("the old root should be closed before the transfer: %+v", old)
	}
	if err := os.WriteFile(mailboxes, good, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.bus.SendAsOperator(first.PID, "after the close"); !errors.Is(err, ipc.ErrRecipientExited) {
		t.Fatalf("a send after the close = %v", err)
	}
	if _, err := continueEnded(f.kernel, f.store, f.bus, &root, f.spawn); err != nil {
		t.Fatal(err)
	}
	if pending, _ := f.bus.Pending(); len(pending[first.PID]) != 0 || len(pending[root.process.PID]) != 1 {
		t.Fatalf("pending = %+v", pending)
	}
}

// sendsThenExits delivers a line while the root takes its last step.
type sendsThenExits struct {
	bus  *ipc.Bus
	line string
}

func (b sendsThenExits) Generate(_ context.Context, request runtime.GenerateRequest) (runtime.GenerateResult, error) {
	if _, err := b.bus.SendAsOperator(request.Process.PID, b.line); err != nil {
		return runtime.GenerateResult{}, err
	}
	return exitDone(), nil
}

// A root's final exit leaves its mail for the new root that continues the
// conversation, so an unread line of the user's reaches a model, not a dead
// letter.
func TestAFinalExitLeavesMailForTheNewRoot(t *testing.T) {
	f := newRootFixture(t)
	first, _ := f.spawn("")
	root := tuiRoot{process: first}
	backend := sendsThenExits{bus: f.bus, line: "typed during the last step"}
	if _, err := f.runtime(backend).RunWithOptions(context.Background(), first.PID, []runtime.Message{{Role: "user", Content: "hi"}}, runtime.RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if pending, _ := f.bus.Pending(); len(pending[first.PID]) != 1 {
		t.Fatalf("pending = %+v", pending)
	}
	if _, err := continueEnded(f.kernel, f.store, f.bus, &root, f.spawn); err != nil {
		t.Fatal(err)
	}
	pending, _ := f.bus.Pending()
	if len(pending[root.process.PID]) != 1 || pending[root.process.PID][0].Body != "typed during the last step" {
		t.Fatalf("pending = %+v", pending)
	}
	events, _ := f.kernel.Events()
	for _, event := range events {
		if event.Type == ipc.DeadLetterEvent {
			t.Fatalf("ipc.dead_letter = %+v", event)
		}
	}
}

// The path that matters: a line typed as the root exits, then the user quits. A
// new TUI on that state directory moves it to the new root before any step, so
// the first request holds it and nothing is swept.
func TestAStartingTUIMovesAnEndedRootsMail(t *testing.T) {
	f := newRootFixture(t)
	first, _ := f.spawn("")
	backend := sendsThenExits{bus: f.bus, line: "typed as it exited"}
	result, err := f.runtime(backend).RunWithOptions(context.Background(), first.PID, []runtime.Message{{Role: "user", Content: "hi"}}, runtime.RunOptions{})
	if err != nil || result.ExitStatus.Code != 0 {
		t.Fatalf("run = %+v, %v", result, err)
	}
	if pending, _ := f.bus.Pending(); len(pending[first.PID]) != 1 {
		t.Fatalf("pending = %+v", pending)
	}

	root, note, resumed, err := startRoot(f.kernel, f.bus, f.dir, "interactive", f.spawn)
	if err != nil || !resumed || root.process.PID == first.PID || note == "" {
		t.Fatalf("startRoot = %+v, %q, %t, %v", root.process, note, resumed, err)
	}
	if pending, _ := f.bus.Pending(); len(pending[first.PID]) != 0 || len(pending[root.process.PID]) != 1 {
		t.Fatalf("pending = %+v", pending)
	}
	var history []runtime.Message
	reading := readingBackend{history: &history}
	if _, err := runTurn(context.Background(), f.runtime(reading), &root, nil, runtime.RunOptions{}); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, message := range history {
		found = found || (message.Role == "user" && message.Content == "typed as it exited")
	}
	if !found {
		t.Fatalf("the first request: %+v", history)
	}
	events, _ := f.kernel.Events()
	for _, event := range events {
		if event.Type == ipc.DeadLetterEvent {
			t.Fatalf("ipc.dead_letter = %+v", event)
		}
	}
}

// A root still able to go on keeps its mail: startRoot replaces nothing.
func TestAStartingTUILeavesARecoverableRootsMail(t *testing.T) {
	f := newRootFixture(t)
	first, _ := f.spawn("")
	record, err := f.store.CreateJSON("checkpoint", first.PID, runtime.Session{Messages: []runtime.Message{{Role: "user", Content: "hi"}}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.bus.SendAsOperator(first.PID, "held")
	f.kernel.Exit(first.PID, kernel.ExitStatus{Code: 1, Reason: "crashed", Retryable: true, Checkpoint: record.ID})
	root, _, resumed, err := startRoot(f.kernel, f.bus, f.dir, "interactive", f.spawn)
	if err != nil || !resumed || root.process.PID != first.PID {
		t.Fatalf("startRoot = %+v, %t, %v", root.process, resumed, err)
	}
	if old, _ := f.kernel.Get(first.PID); old.Closed {
		t.Fatalf("it should not be closed: %+v", old)
	}
	if pending, _ := f.bus.Pending(); len(pending[first.PID]) != 1 {
		t.Fatalf("pending = %+v", pending)
	}
}

// readingBackend keeps its first request and exits.
type readingBackend struct{ history *[]runtime.Message }

func (b readingBackend) Generate(_ context.Context, request runtime.GenerateRequest) (runtime.GenerateResult, error) {
	if len(*b.history) == 0 {
		*b.history = request.History
	}
	return exitDone(), nil
}

// readBack runs a root that reads id whole, as a model following the message
// would, and returns what the tool gave it.
func (f rootFixture) readBack(t *testing.T, pid, id string) string {
	t.Helper()
	var got strings.Builder
	offset := "0"
	backend := &scriptedBackend{fn: func(call int, request runtime.GenerateRequest) runtime.GenerateResult {
		if call > 1 {
			last := request.History[len(request.History)-1].Content
			if strings.HasPrefix(last, "tool_error") {
				t.Fatalf("read_artifact %s: %s", id, last)
			}
			match := regexp.MustCompile(`(?s)^read_artifact:map\[artifact_id:\S+ body:(.*?)(?: next_offset:(\d+))? offset:\d+ total_bytes:\d+\]$`).FindStringSubmatch(last)
			if match == nil {
				t.Fatalf("not a page: %.200s", last)
			}
			got.WriteString(match[1])
			if match[2] == "" {
				return runtime.GenerateResult{Action: runtime.GenerateAction{Type: "exit", ExitStatus: &kernel.ExitStatus{Code: 1, Reason: "read", Retryable: true}}}
			}
			offset = match[2]
		}
		return runtime.GenerateResult{Action: runtime.GenerateAction{Type: "tool", ToolCall: &runtime.ToolCall{Name: "read_artifact", Args: map[string]string{"id": id, "offset": offset}}}}
	}}
	agent := f.runtime(backend)
	agent.MaxSteps = 40
	if _, err := agent.RunWithOptions(context.Background(), pid, []runtime.Message{{Role: "user", Content: "read"}}, runtime.RunOptions{ContinueOnToolError: true}); err != nil {
		t.Fatal(err)
	}
	return got.String()
}

type scriptedBackend struct {
	calls int
	fn    func(call int, request runtime.GenerateRequest) runtime.GenerateResult
}

func (b *scriptedBackend) Generate(_ context.Context, request runtime.GenerateRequest) (runtime.GenerateResult, error) {
	b.calls++
	return b.fn(b.calls, request), nil
}

// A paste over the bus's cap is not refused and not lost: the mail holds its start
// and a handle, and the root reads the whole of it, by the path the loop uses.
func TestAnOverLongLineStaysWhole(t *testing.T) {
	f := newRootFixture(t)
	first, _ := f.spawn("")
	root := tuiRoot{process: first}
	line := strings.Repeat("a line of a long paste, 世界\n", 5000)
	if len(line) <= ipc.MaxBodyBytes {
		t.Fatal("the line should be over the cap")
	}
	appended, err := deliver(f.bus, f.store, &root, line)
	if err != nil || appended != nil {
		t.Fatalf("deliver = %+v, %v", appended, err)
	}
	pending, _ := f.bus.Pending()
	box := pending[first.PID]
	if len(box) != 1 || len(box[0].Artifacts) != 1 || len(box[0].Body) > ipc.MaxBodyBytes {
		t.Fatalf("mailbox: %d messages, %v, %d bytes", len(box), box[0].Artifacts, len(box[0].Body))
	}
	id := box[0].Artifacts[0]
	if !strings.HasPrefix(box[0].Body, "a line of a long paste") || !strings.Contains(box[0].Body, "read_artifact id="+id) || !utf8.ValidString(box[0].Body) {
		t.Fatalf("body: %.100s ... %s", box[0].Body, box[0].Body[len(box[0].Body)-120:])
	}
	if got := f.readBack(t, first.PID, id); got != line {
		t.Fatalf("read back %d bytes of %d", len(got), len(line))
	}
}

func TestALineAtTheCapIsSentAsItIs(t *testing.T) {
	f := newRootFixture(t)
	first, _ := f.spawn("")
	root := tuiRoot{process: first}
	line := strings.Repeat("x", ipc.MaxBodyBytes)
	if _, err := deliver(f.bus, f.store, &root, line); err != nil {
		t.Fatal(err)
	}
	pending, _ := f.bus.Pending()
	if box := pending[first.PID]; len(box) != 1 || box[0].Body != line || box[0].Artifacts != nil {
		t.Fatalf("mailbox: %+v", box)
	}
}

// A store that cannot keep the paste sends nothing and says so, which the loop
// prints as "not delivered". The text is then only in the terminal: the stated
// limit, on a state directory that cannot write anyway.
func TestAnOverLongLineThatCannotBeKeptIsNotDelivered(t *testing.T) {
	f := newRootFixture(t)
	root := tuiRoot{process: kernel.Process{PID: "pid-gone"}}
	f.store.BindProducers(func(pid string) error { return errors.New("unknown") })
	if _, err := deliver(f.bus, f.store, &root, strings.Repeat("x", ipc.MaxBodyBytes+1)); err == nil {
		t.Fatal("expected an error")
	}
	if pending, _ := f.bus.Pending(); len(pending) != 0 {
		t.Fatalf("pending: %+v", pending)
	}
}

// A person reaches an agent whose mailbox peers have filled.
func TestALineReachesAFullMailbox(t *testing.T) {
	f := newRootFixture(t)
	first, _ := f.spawn("")
	peer, _ := f.spawn("")
	// As a flood of sends would have left it, without their 5 seconds.
	var flood []ipc.Message
	for i := 0; i < ipc.MaxMailboxMessages; i++ {
		flood = append(flood, ipc.Message{ID: fmt.Sprintf("msg-%06d", i+1), From: peer.PID, To: first.PID, Body: "flood", Timestamp: time.Now().UTC()})
	}
	raw, err := json.Marshal(map[string][]ipc.Message{first.PID: flood})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.dir, "bus", "mailboxes.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.bus.Send(peer.PID, first.PID, "one more"); !errors.Is(err, ipc.ErrMailboxFull) {
		t.Fatalf("a peer's send = %v", err)
	}
	root := tuiRoot{process: first}
	if _, err := deliver(f.bus, f.store, &root, "stop what you are doing"); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	pending, _ := f.bus.Pending()
	box := pending[first.PID]
	if len(box) != ipc.MaxMailboxMessages+1 || box[len(box)-1].Body != "stop what you are doing" {
		t.Fatalf("%d messages", len(box))
	}
	// It comes after the flood, which is read in batches: the stated cost. Keep
	// a run short by asking only that it gets there.
	var seen string
	backend := &scriptedBackend{fn: func(call int, request runtime.GenerateRequest) runtime.GenerateResult {
		for _, message := range request.History {
			if message.Role == "user" {
				seen += message.Content + "\n"
			}
		}
		if strings.Contains(seen, "stop what") {
			return runtime.GenerateResult{Action: runtime.GenerateAction{Type: "exit", ExitStatus: &kernel.ExitStatus{Code: 0, Reason: "stopped"}}}
		}
		return runtime.GenerateResult{Action: runtime.GenerateAction{Type: "message", Message: "reading"}}
	}}
	agent := f.runtime(backend)
	agent.MaxSteps = 60
	if _, err := agent.RunWithOptions(context.Background(), first.PID, []runtime.Message{{Role: "user", Content: "start"}}, runtime.RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(seen, "stop what you are doing") {
		t.Fatal("the line never reached a request")
	}
}

// A new root continues an ended one's conversation, so it can read what that one
// could: its own publications, and handles in its history. The mail it had not
// read comes over with its handle. Both ways of taking over.
func TestANewRootHoldsWhatTheOldOneHeld(t *testing.T) {
	setup := func(t *testing.T) (rootFixture, kernel.Process, string, string) {
		f := newRootFixture(t)
		old, _ := f.spawn("")
		peer, _ := f.kernel.Spawn(kernel.ProcessSpec{Role: "agent:peer", Metadata: map[string]string{"agent_id": "peer"}})
		own, err := f.store.CreateJSON(artifact.PublishedPrefix+"note", old.PID, map[string]string{"body": "published by the old root"}, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		attached, err := f.store.CreateJSON(artifact.PublishedPrefix+"note", peer.PID, map[string]string{"body": "attached to mail"}, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.bus.SendWithArtifacts(peer.PID, old.PID, "unread with a handle", []string{attached.ID}); err != nil {
			t.Fatal(err)
		}
		// The old root exits for good with that mail unread.
		if _, err := f.kernel.Exit(old.PID, kernel.ExitStatus{Code: 0, Reason: "done"}); err != nil {
			t.Fatal(err)
		}
		return f, old, own.ID, attached.ID
	}
	check := func(t *testing.T, f rootFixture, pid, own, attached string) {
		t.Helper()
		pending, _ := f.bus.Pending()
		if box := pending[pid]; len(box) != 1 || !slices.Equal(box[0].Artifacts, []string{attached}) {
			t.Fatalf("the mail did not come over with its handle: %+v", box)
		}
		if got := f.readBack(t, pid, own); got != "published by the old root" {
			t.Fatalf("own: %q", got)
		}
		// That run took the mail in and exited retryably; the successor holds the grant.
		end, err := f.kernel.ChainEnd(pid)
		if err != nil {
			t.Fatal(err)
		}
		next, err := f.kernel.Supersede(end.PID, kernel.ProcessSpec{Role: end.Role, Metadata: map[string]string{"agent_id": "interactive", "resumed_from": end.PID}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := f.readBack(t, next.PID, attached); got != "attached to mail" {
			t.Fatalf("attached: %q", got)
		}
	}
	t.Run("continueEnded", func(t *testing.T) {
		f, old, own, attached := setup(t)
		root := tuiRoot{process: old, seed: []runtime.Message{{Role: "user", Content: "before"}}}
		if _, err := continueEnded(f.kernel, f.store, f.bus, &root, f.spawn); err != nil {
			t.Fatal(err)
		}
		if root.process.Metadata[runtime.ContinuesMetadata] != old.PID {
			t.Fatalf("metadata: %+v", root.process.Metadata)
		}
		check(t, f, root.process.PID, own, attached)
	})
	t.Run("startRoot", func(t *testing.T) {
		f, old, own, attached := setup(t)
		if _, err := f.store.CreateJSON("checkpoint", old.PID, runtime.Session{Messages: []runtime.Message{{Role: "user", Content: "before"}}}, nil, nil); err != nil {
			t.Fatal(err)
		}
		root, _, resumed, err := startRoot(f.kernel, f.bus, f.dir, "interactive", f.spawn)
		if err != nil || !resumed {
			t.Fatalf("startRoot: %v, resumed %t", err, resumed)
		}
		if root.process.Metadata[runtime.ContinuesMetadata] != old.PID {
			t.Fatalf("metadata: %+v", root.process.Metadata)
		}
		check(t, f, root.process.PID, own, attached)
	})
	t.Run("an unrelated root does not", func(t *testing.T) {
		f, _, own, _ := setup(t)
		other, _ := f.spawn("")
		backend := &scriptedBackend{fn: func(call int, request runtime.GenerateRequest) runtime.GenerateResult {
			if call == 1 {
				return runtime.GenerateResult{Action: runtime.GenerateAction{Type: "tool", ToolCall: &runtime.ToolCall{Name: "read_artifact", Args: map[string]string{"id": own}}}}
			}
			if last := request.History[len(request.History)-1].Content; !strings.Contains(last, runtime.ErrArtifactNotReadable.Error()) {
				t.Errorf("read: %s", last)
			}
			return runtime.GenerateResult{Action: runtime.GenerateAction{Type: "exit", ExitStatus: &kernel.ExitStatus{Code: 0, Reason: "done"}}}
		}}
		if _, err := f.runtime(backend).RunWithOptions(context.Background(), other.PID, []runtime.Message{{Role: "user", Content: "read"}}, runtime.RunOptions{ContinueOnToolError: true}); err != nil {
			t.Fatal(err)
		}
	})
}
