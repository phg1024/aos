package runtime_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"aos/internal/artifact"
	"aos/internal/capability"
	"aos/internal/kernel"
	"aos/internal/runtime"
	"aos/internal/tools"
)

type recoveryFixture struct {
	kernel    *kernel.Kernel
	store     *artifact.Store
	workspace string
	scratch   string
}

func newRecoveryFixture(t *testing.T) recoveryFixture {
	t.Helper()
	root := t.TempDir()
	k, err := kernel.New(filepath.Join(root, "kernel"))
	if err != nil {
		t.Fatalf("new kernel: %v", err)
	}
	store, err := artifact.New(filepath.Join(root, "artifacts"))
	if err != nil {
		t.Fatalf("new artifact store: %v", err)
	}
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	return recoveryFixture{kernel: k, store: store, workspace: workspace, scratch: filepath.Join(root, "scratch")}
}

func (f recoveryFixture) spawn(t *testing.T) kernel.Process {
	t.Helper()
	process, err := f.kernel.Spawn(kernel.ProcessSpec{
		Role:          "worker",
		Mounts:        []capability.Mount{{Name: "workspace", HostPath: f.workspace}},
		CapabilitySet: capability.Set{Tools: []string{"run", "write_file", "read_file"}, ReadPaths: []string{f.workspace}, WritePaths: []string{f.workspace}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return process
}

func (f recoveryFixture) runtime(backend runtime.Backend) *runtime.AgentRuntime {
	toolRuntime := tools.NewRuntime()
	toolRuntime.ScratchRoot = f.scratch
	return &runtime.AgentRuntime{Kernel: f.kernel, Artifacts: f.store, Tools: toolRuntime, Backend: backend, MaxSteps: 8, BackendRetry: runtime.BackendRetryPolicy{Attempts: -1}}
}

// steps replies with results in order, then exits.
func steps(results ...runtime.GenerateResult) *funcBackend {
	return &funcBackend{fn: func(call int, _ context.Context, _ runtime.GenerateRequest) (runtime.GenerateResult, error) {
		if call <= len(results) {
			return results[call-1], nil
		}
		return exitResult("done"), nil
	}}
}

// failed runs a process through two checkpointed messages to a retryable
// exit, and returns it with its checkpoints, oldest first.
func (f recoveryFixture) failed(t *testing.T) (kernel.Process, []string) {
	t.Helper()
	process := f.spawn(t)
	backend := &funcBackend{fn: func(call int, _ context.Context, _ runtime.GenerateRequest) (runtime.GenerateResult, error) {
		if call <= 2 {
			return messageResult("progress"), nil
		}
		return runtime.GenerateResult{}, errors.New("connection reset")
	}}
	result, err := f.runtime(backend).Run(context.Background(), process.PID, []runtime.Message{{Role: "user", Content: "work"}})
	if err == nil || result.ExitStatus.Code == 0 || !result.ExitStatus.Retryable {
		t.Fatalf("failed run = %+v, %v", result, err)
	}
	process, _ = f.kernel.Get(process.PID)
	return process, f.checkpoints(t, process.PID)
}

func (f recoveryFixture) checkpoints(t *testing.T, pid string) []string {
	t.Helper()
	records, err := f.store.List()
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, record := range records {
		if record.Type == "checkpoint" && record.ProducerPID == pid {
			ids = append(ids, record.ID)
		}
	}
	slices.Sort(ids)
	return ids
}

func (f recoveryFixture) successors(t *testing.T, pid string) int {
	t.Helper()
	processes, err := f.kernel.Processes()
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, process := range processes {
		if process.Metadata["resumed_from"] == pid {
			n++
		}
	}
	return n
}

func (f recoveryFixture) events(t *testing.T, eventType string) []kernel.Event {
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

// An exited process is recovered at most once: a second Recover, one after
// the other or many at once, is refused and names the successor, which is
// the one to recover next.
func TestRecoverIsExclusive(t *testing.T) {
	f := newRecoveryFixture(t)
	failed, _ := f.failed(t)
	again := &funcBackend{fn: func(int, context.Context, runtime.GenerateRequest) (runtime.GenerateResult, error) {
		return runtime.GenerateResult{}, errors.New("connection reset")
	}}
	first, err := f.runtime(again).Recover(context.Background(), failed.PID, "", nil, runtime.RunOptions{})
	if err == nil || first.PID == "" || first.PID == failed.PID || !first.ExitStatus.Retryable {
		t.Fatalf("first recover = %+v, %v", first, err)
	}
	backend := steps()
	_, err = f.runtime(backend).Recover(context.Background(), failed.PID, "", nil, runtime.RunOptions{})
	if !errors.Is(err, runtime.ErrNotRecoverable) || !errors.Is(err, kernel.ErrSuperseded) || !strings.Contains(err.Error(), first.PID) {
		t.Fatalf("second recover: %v", err)
	}
	if backend.calls != 0 || f.successors(t, failed.PID) != 1 {
		t.Fatalf("a refused recover ran (%d calls) or spawned (%d successors)", backend.calls, f.successors(t, failed.PID))
	}
	previous, _ := f.kernel.Get(failed.PID)
	if previous.SupersededBy != first.PID {
		t.Fatalf("superseded_by = %q, want %s", previous.SupersededBy, first.PID)
	}
	if next, err := f.runtime(steps()).Recover(context.Background(), first.PID, "", nil, runtime.RunOptions{}); err != nil || next.ExitStatus.Reason != "done" {
		t.Fatalf("recovering the successor = %+v, %v", next, err)
	}

	g := newRecoveryFixture(t)
	target, _ := g.failed(t)
	var wg sync.WaitGroup
	var mu sync.Mutex
	won, refused := 0, 0
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := g.runtime(steps()).Recover(context.Background(), target.PID, "", nil, runtime.RunOptions{})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				won++
			case errors.Is(err, kernel.ErrSuperseded):
				refused++
			default:
				t.Errorf("recover: %v", err)
			}
		}()
	}
	wg.Wait()
	if won != 1 || refused != 19 || g.successors(t, target.PID) != 1 {
		t.Fatalf("20 concurrent recovers: %d won, %d refused, %d successors", won, refused, g.successors(t, target.PID))
	}
}

// Recover starts from the checkpoint the exit names unless a rollback is
// asked for, and records a rollback as one.
func TestRecoverUsesTheExitsCheckpoint(t *testing.T) {
	f := newRecoveryFixture(t)
	failed, checkpoints := f.failed(t)
	named := failed.ExitStatus.Checkpoint
	if len(checkpoints) < 2 || named != checkpoints[len(checkpoints)-1] {
		t.Fatalf("checkpoints %v, exit names %s", checkpoints, named)
	}
	older := checkpoints[0]
	backend := steps()
	_, err := f.runtime(backend).Recover(context.Background(), failed.PID, older, nil, runtime.RunOptions{})
	if !errors.Is(err, runtime.ErrNotRecoverable) || !strings.Contains(err.Error(), "rollback") {
		t.Fatalf("recovering from an older checkpoint without asking: %v", err)
	}
	if backend.calls != 0 || f.successors(t, failed.PID) != 0 {
		t.Fatal("a refused rollback ran or spawned")
	}
	if _, err := f.runtime(steps()).Recover(context.Background(), failed.PID, named, nil, runtime.RunOptions{}); err != nil {
		t.Fatalf("recovering from the named checkpoint: %v", err)
	}
	if superseded := f.events(t, "process.superseded"); len(superseded) != 1 || superseded[0].Data["checkpoint"] != named || superseded[0].Data["rollback_from"] != "" {
		t.Fatalf("process.superseded = %+v", superseded)
	}

	g := newRecoveryFixture(t)
	failed, checkpoints = g.failed(t)
	var first []runtime.Message
	backend = &funcBackend{fn: func(call int, _ context.Context, request runtime.GenerateRequest) (runtime.GenerateResult, error) {
		first = request.History
		return exitResult("done"), nil
	}}
	if _, err := g.runtime(backend).Recover(context.Background(), failed.PID, checkpoints[0], nil, runtime.RunOptions{Rollback: true}); err != nil {
		t.Fatalf("an explicit rollback: %v", err)
	}
	superseded := g.events(t, "process.superseded")
	if len(superseded) != 1 || superseded[0].Data["checkpoint"] != checkpoints[0] || superseded[0].Data["rollback_from"] != failed.ExitStatus.Checkpoint {
		t.Fatalf("a rollback must be recorded as one: %+v", superseded)
	}
	if progress := strings.Count(joinContents(first), "progress"); progress != 1 {
		t.Fatalf("the rollback should start from the first checkpoint (one message), saw %d", progress)
	}

	h := newRecoveryFixture(t)
	none := h.spawn(t)
	if _, err := h.kernel.Exit(none.PID, kernel.ExitStatus{Code: 1, Reason: "transient", Retryable: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.runtime(steps()).Recover(context.Background(), none.PID, "", nil, runtime.RunOptions{}); !errors.Is(err, runtime.ErrNotRecoverable) {
		t.Fatalf("an exit with no checkpoint and none given: %v", err)
	}
}

func joinContents(messages []runtime.Message) string {
	var b strings.Builder
	for _, message := range messages {
		b.WriteString(message.Content)
		b.WriteString("\n")
	}
	return b.String()
}

// A world tool records its intent before it acts, and its outcome names
// the intent; a read records none.
func TestWorldToolRecordsIntentFirst(t *testing.T) {
	f := newRecoveryFixture(t)
	process := f.spawn(t)
	target := filepath.Join(f.workspace, "out.txt")
	agent := f.runtime(steps(
		toolAction("write_file", map[string]string{"path": target, "content": "x"}),
		toolAction("read_file", map[string]string{"path": target}),
		toolAction("run", map[string]string{"command": "true"}),
		toolAction("write_file", map[string]string{"path": "/elsewhere/denied", "content": "x"}),
	))
	var seen []int
	real := agent.Tools
	recorded := tools.NewRuntime()
	recorded.Recorded = func(ctx context.Context, req tools.Request) (tools.Result, error) {
		// What the kernel holds when the tool is about to act.
		seen = append(seen, len(f.events(t, runtime.ToolStartedEvent)))
		return real.ExecuteContext(ctx, req)
	}
	agent.Tools = recorded
	if _, err := agent.RunWithOptions(context.Background(), process.PID, nil, runtime.RunOptions{ContinueOnToolError: true}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(seen, []int{1, 1, 2, 3}) {
		t.Fatalf("intents on record as each tool acted: %v, want [1 1 2 3]", seen)
	}
	intents := f.events(t, runtime.ToolStartedEvent)
	if len(intents) != 3 || intents[0].Data["tool"] != "write_file" || intents[0].Data["target"] != target || intents[1].Data["target"] != "true" || intents[0].Data["args_sha256"] == "" {
		t.Fatalf("intents = %+v", intents)
	}
	records, _ := f.store.List()
	outcomes := map[string]string{}
	for _, record := range records {
		if record.Type == "tool_result" || record.Type == "tool_error" {
			outcomes[record.Metadata["tool"]+" "+record.Type] = record.Metadata["intent"]
		}
	}
	want := map[string]string{"write_file tool_result": intents[0].ID, "read_file tool_result": "", "run tool_result": intents[1].ID, "write_file tool_error": intents[2].ID}
	if !mapsEqual(outcomes, want) {
		t.Fatalf("outcome intents = %v, want %v", outcomes, want)
	}
	events, _ := f.kernel.Events()
	if open := runtime.OpenIntents(events, records); len(open) != 0 {
		t.Fatalf("every intent has an outcome: %+v", open)
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if got, ok := b[key]; !ok || got != value {
			return false
		}
	}
	return true
}

// A cancelled command records a tool_cancelled outcome with the files it
// changed, the agent is told it may have acted, and a resume does not
// report it again as interrupted.
func TestCancelledCommandRecordsItsChanges(t *testing.T) {
	f := newRecoveryFixture(t)
	process := f.spawn(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	agent := f.runtime(steps(toolAction("run", map[string]string{"command": "echo once >> " + filepath.Join(f.workspace, "effects") + "; sleep 30", "timeout": "60s"})))
	time.AfterFunc(500*time.Millisecond, cancel)
	result, err := agent.RunWithOptions(ctx, process.PID, nil, runtime.RunOptions{ContinueOnToolError: true})
	if !errors.Is(err, runtime.ErrRunCancelled) {
		t.Fatalf("expected a cancel, got %+v %v", result, err)
	}
	records, _ := f.store.List()
	var cancelled *artifact.Artifact
	for i := range records {
		if records[i].Type == runtime.ToolCancelledType {
			cancelled = &records[i]
		}
	}
	intents := f.events(t, runtime.ToolStartedEvent)
	if cancelled == nil || len(intents) != 1 || cancelled.Metadata["intent"] != intents[0].ID || cancelled.Metadata["tool"] != "run" {
		t.Fatalf("tool_cancelled = %+v, intents %+v", cancelled, intents)
	}
	var payload struct {
		Reason  string             `json:"reason"`
		Changes *tools.FileChanges `json:"changes"`
	}
	if err := f.store.ReadJSON(cancelled.ID, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Reason != "interrupted_by_user" || payload.Changes == nil || !slices.Equal(payload.Changes.Created, []string{"effects"}) {
		t.Fatalf("tool_cancelled payload = %+v", payload)
	}
	last := result.Session.Messages[len(result.Session.Messages)-1].Content
	if !strings.Contains(last, "may_have_taken_effect=true") || !strings.Contains(last, `changes="created effects"`) {
		t.Fatalf("the agent's note: %q", last)
	}
	var resumed []runtime.Message
	agent.Backend = &funcBackend{fn: func(_ int, _ context.Context, request runtime.GenerateRequest) (runtime.GenerateResult, error) {
		resumed = request.History
		return exitResult("done"), nil
	}}
	if _, err := agent.Resume(context.Background(), process.PID, result.CheckpointID, nil, runtime.RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if text := joinContents(resumed); strings.Contains(text, "tool_interrupted") || !strings.Contains(text, "may_have_taken_effect=true") {
		t.Fatalf("resumed history: %s", text)
	}
}

// A command cut off by its timeout records its changes on the tool_error.
func TestTimedOutCommandRecordsItsChanges(t *testing.T) {
	f := newRecoveryFixture(t)
	process := f.spawn(t)
	agent := f.runtime(steps(toolAction("run", map[string]string{"command": "echo once >> " + filepath.Join(f.workspace, "effects") + "; sleep 30", "timeout": "300ms"})))
	if _, err := agent.RunWithOptions(context.Background(), process.PID, nil, runtime.RunOptions{ContinueOnToolError: true}); err != nil {
		t.Fatal(err)
	}
	records, _ := f.store.List()
	for _, record := range records {
		if record.Type != "tool_error" {
			continue
		}
		var payload struct {
			Error   string             `json:"error"`
			Changes *tools.FileChanges `json:"changes"`
		}
		if err := f.store.ReadJSON(record.ID, &payload); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(payload.Error, "timed out") || payload.Changes == nil || !slices.Equal(payload.Changes.Created, []string{"effects"}) || record.Metadata["intent"] == "" {
			t.Fatalf("tool_error = %+v %+v", record, payload)
		}
		return
	}
	t.Fatal("no tool_error")
}

// A call whose process died during it (an intent with no outcome) is
// reported to the recovered agent before its first step, recorded as a
// tool_interrupted outcome by the new process, and reported once.
func TestRecoverReportsInterruptedCalls(t *testing.T) {
	f := newRecoveryFixture(t)
	process := f.spawn(t)
	result, err := f.runtime(steps(messageResult("about to run"))).RunWithOptions(context.Background(), process.PID, nil, runtime.RunOptions{StopOnAssistantMessage: true})
	if err != nil || !result.Yielded {
		t.Fatalf("park = %+v %v", result, err)
	}
	// The process records its intent and dies in the command: nothing after
	// the intent is written, and a reap closes it at its checkpoint.
	intent, err := f.kernel.RecordEvent(process.PID, runtime.ToolStartedEvent, map[string]string{"tool": "run", "target": "echo once >> effects"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.kernel.Exit(process.PID, kernel.ExitStatus{Code: 1, Reason: "orphaned", Retryable: true, Checkpoint: result.CheckpointID}); err != nil {
		t.Fatal(err)
	}
	events, _ := f.kernel.Events()
	records, _ := f.store.List()
	if open := runtime.OpenIntents(events, records); len(open) != 1 || open[0].EventID != intent.ID || open[0].PID != process.PID {
		t.Fatalf("open intents = %+v", open)
	}
	var first []runtime.Message
	backend := &funcBackend{fn: func(call int, _ context.Context, request runtime.GenerateRequest) (runtime.GenerateResult, error) {
		if call == 1 {
			first = request.History
			return messageResult("checked"), nil
		}
		return runtime.GenerateResult{}, errors.New("connection reset")
	}}
	recovered, err := f.runtime(backend).Recover(context.Background(), process.PID, "", []runtime.Message{{Role: "user", Content: "continue"}}, runtime.RunOptions{})
	if err == nil {
		t.Fatal("the recovered run should fail on its second call")
	}
	text := joinContents(first)
	note := strings.Index(text, "tool_interrupted name=run target=\"echo once >> effects\"")
	if note < 0 || !strings.Contains(text, "may have taken effect") || note > strings.Index(text, "continue") {
		t.Fatalf("the recovered agent's first request should report the call before the appended message:\n%s", text)
	}
	records, _ = f.store.List()
	var interrupted []artifact.Artifact
	for _, record := range records {
		if record.Type == runtime.ToolInterruptedType {
			interrupted = append(interrupted, record)
		}
	}
	if len(interrupted) != 1 || interrupted[0].ProducerPID != recovered.PID || interrupted[0].Metadata["intent"] != intent.ID {
		t.Fatalf("tool_interrupted = %+v", interrupted)
	}
	events, _ = f.kernel.Events()
	if open := runtime.OpenIntents(events, records); len(open) != 0 {
		t.Fatalf("the interruption resolves the intent: %+v", open)
	}
	// Recovering the successor does not report the call again.
	var again []runtime.Message
	if _, err := f.runtime(&funcBackend{fn: func(_ int, _ context.Context, request runtime.GenerateRequest) (runtime.GenerateResult, error) {
		again = request.History
		return exitResult("done"), nil
	}}).Recover(context.Background(), recovered.PID, "", nil, runtime.RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(joinContents(again), "tool_interrupted"); n != 1 {
		t.Fatalf("the note appears %d times after a second recovery, want 1 (carried in history)", n)
	}
}

// NoToolIntents runs world tools as AOS did before intents: no event, no
// intent metadata, the old cancel note.
func TestNoToolIntents(t *testing.T) {
	f := newRecoveryFixture(t)
	process := f.spawn(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	agent := f.runtime(steps(
		toolAction("write_file", map[string]string{"path": filepath.Join(f.workspace, "a"), "content": "x"}),
		toolAction("run", map[string]string{"command": "sleep 30", "timeout": "60s"}),
	))
	agent.NoToolIntents = true
	time.AfterFunc(500*time.Millisecond, cancel)
	result, err := agent.RunWithOptions(ctx, process.PID, nil, runtime.RunOptions{ContinueOnToolError: true})
	if !errors.Is(err, runtime.ErrRunCancelled) {
		t.Fatalf("expected a cancel: %v", err)
	}
	if intents := f.events(t, runtime.ToolStartedEvent); len(intents) != 0 {
		t.Fatalf("intents recorded: %+v", intents)
	}
	records, _ := f.store.List()
	for _, record := range records {
		if record.Metadata["intent"] != "" || record.Type == runtime.ToolCancelledType {
			t.Fatalf("%s %+v", record.Type, record.Metadata)
		}
	}
	if last := result.Session.Messages[len(result.Session.Messages)-1].Content; last != "tool_cancelled name=run reason=interrupted_by_user" {
		t.Fatalf("cancel note = %q", last)
	}
}
