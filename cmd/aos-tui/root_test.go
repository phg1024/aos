package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aos/internal/artifact"
	"aos/internal/ipc"
	"aos/internal/kernel"
	"aos/internal/runtime"
	"aos/internal/tools"
	"aos/internal/tui/render"
)

type rootFixture struct {
	kernel *kernel.Kernel
	store  *artifact.Store
	bus    *ipc.Bus
	spawn  func() (kernel.Process, error)
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
	spawn := func() (kernel.Process, error) {
		return k.Spawn(kernel.ProcessSpec{Role: "agent:interactive", Metadata: map[string]string{"agent_id": "interactive"}})
	}
	return rootFixture{kernel: k, store: store, bus: bus, spawn: spawn}
}

func (f rootFixture) runtime(backend runtime.Backend) *runtime.AgentRuntime {
	return &runtime.AgentRuntime{Kernel: f.kernel, Artifacts: f.store, Bus: f.bus, Tools: tools.NewRuntime(), Backend: backend, MaxSteps: 6}
}

// say runs one TUI turn for line, as the loop does.
func (f rootFixture) say(t *testing.T, r *runtime.AgentRuntime, root *tuiRoot, line string) (string, runtime.RunResult, error) {
	t.Helper()
	note, err := continueEnded(f.kernel, f.store, f.bus, root, f.spawn)
	if err != nil {
		t.Fatalf("continueEnded: %v", err)
	}
	appended, err := deliver(f.bus, root, line)
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
	first, err := f.spawn()
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
	first, _ := f.spawn()
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
	first, _ := f.spawn()
	root := tuiRoot{process: first}
	if _, _, err := f.say(t, r, &root, "hello"); err != nil {
		t.Fatal(err)
	}
	f.kernel.Exit(first.PID, kernel.ExitStatus{Code: 0, Reason: "done"})
	good := root

	// Without continueEnded (the old loop), the line goes with a recovery
	// that Recover refuses.
	appended, err := deliver(f.bus, &root, "are you there?")
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
	live, _ := f.spawn()
	root := tuiRoot{process: live, checkpointID: "art-x"}
	if note, err := continueEnded(f.kernel, f.store, f.bus, &root, f.spawn); note != "" || err != nil || root.process.PID != live.PID {
		t.Fatalf("live root replaced: %q %v", note, err)
	}
	f.kernel.Exit(live.PID, kernel.ExitStatus{Code: 1, Reason: "backend down", Retryable: true})
	if note, err := continueEnded(f.kernel, f.store, f.bus, &root, f.spawn); note != "" || err != nil || root.process.PID != live.PID {
		t.Fatalf("recoverable root replaced: %q %v", note, err)
	}

	// No checkpoint yet: even a retryable exit cannot be recovered.
	unread, _ := f.spawn()
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
	first, _ := f.spawn()
	root := tuiRoot{process: first}
	turn := func(line string) (string, runtime.RunResult) {
		t.Helper()
		note, err := continueEnded(f.kernel, f.store, f.bus, &root, f.spawn)
		if err != nil {
			t.Fatal(err)
		}
		appended, err := deliver(f.bus, &root, line)
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
