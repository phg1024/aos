package runtime_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"aos/internal/artifact"
	"aos/internal/kernel"
	"aos/internal/runtime"
	"aos/internal/tools"
)

// fastRetry retries quickly enough for tests.
var fastRetry = runtime.BackendRetryPolicy{Base: time.Millisecond, Max: 2 * time.Millisecond}

func kernelEvents(t *testing.T, k *kernel.Kernel, eventType string) []kernel.Event {
	t.Helper()
	events, err := k.Events()
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

// noticeItems counts memory.notice events by item.
func noticeItems(t *testing.T, k *kernel.Kernel) map[string]int {
	t.Helper()
	counts := map[string]int{}
	for _, event := range kernelEvents(t, k, "memory.notice") {
		counts[event.Data["item"]]++
	}
	return counts
}

// A failed model call is retried within its step: an outage spends no
// steps and puts nothing in the history. Once retries run out, the failure
// takes a step as before, and says it was the endpoint, not the agent.
func TestBackendRetriesDoNotSpendSteps(t *testing.T) {
	t.Run("recovers", func(t *testing.T) {
		k, store, process := newMemoryFixture(t)
		backend := &funcBackend{fn: func(call int, _ context.Context, _ runtime.GenerateRequest) (runtime.GenerateResult, error) {
			if call <= 5 {
				return runtime.GenerateResult{}, errors.New("connection refused")
			}
			return exitResult("done"), nil
		}}
		agent := &runtime.AgentRuntime{Kernel: k, Artifacts: store, Tools: tools.NewRuntime(), Backend: backend, MaxSteps: 1, BackendRetry: fastRetry}
		result, err := agent.RunWithOptions(context.Background(), process.PID, []runtime.Message{{Role: "user", Content: "goal"}}, runtime.RunOptions{ContinueOnToolError: true})
		if err != nil || result.ExitStatus.Reason != "done" {
			t.Fatalf("the run should survive the outage within one step: %+v %v", result.ExitStatus, err)
		}
		if result.Session.Usage.Steps != 1 || len(kernelEvents(t, k, "backend.retry")) != 5 {
			t.Fatalf("steps %d, retries %d", result.Session.Usage.Steps, len(kernelEvents(t, k, "backend.retry")))
		}
		for _, message := range result.Session.Messages {
			if strings.Contains(message.Content, "tool_error name=backend") {
				t.Fatalf("a retried failure should leave no trace in the history: %q", message.Content)
			}
		}
	})
	// Once retries run out, another step cannot help: the process exits
	// retryable with a checkpoint, to be recovered when the endpoint is back.
	t.Run("gives up", func(t *testing.T) {
		k, store, process := newMemoryFixture(t)
		backend := &funcBackend{fn: func(int, context.Context, runtime.GenerateRequest) (runtime.GenerateResult, error) {
			return runtime.GenerateResult{}, errors.New("connection refused")
		}}
		retry := fastRetry
		retry.Attempts = 3
		agent := &runtime.AgentRuntime{Kernel: k, Artifacts: store, Tools: tools.NewRuntime(), Backend: backend, MaxSteps: 5, BackendRetry: retry}
		result, err := agent.RunWithOptions(context.Background(), process.PID, []runtime.Message{{Role: "user", Content: "goal"}}, runtime.RunOptions{ContinueOnToolError: true})
		if err != nil || backend.calls != 3 {
			t.Fatalf("three attempts, then an exit: %d calls, %v", backend.calls, err)
		}
		status := result.ExitStatus
		if status.Reason != "model endpoint failed 3 times in a row: connection refused" || !status.Retryable || status.Checkpoint == "" {
			t.Fatalf("exit = %+v", status)
		}
		if session := checkpointMessages(t, store, status.Checkpoint); session.Usage.Steps != 1 || len(session.Messages) != 1 {
			t.Fatalf("the outage should cost one step and leave the history as it was: %+v", session)
		}
	})
	// A failure its driver says will not pass is not retried: the process
	// exits at once, retryable with a checkpoint, so the operator can fix
	// the cause and recover.
	t.Run("not temporary", func(t *testing.T) {
		k, store, process := newMemoryFixture(t)
		backend := &funcBackend{fn: func(int, context.Context, runtime.GenerateRequest) (runtime.GenerateResult, error) {
			return runtime.GenerateResult{}, &driverError{text: "404 Not Found"}
		}}
		agent := &runtime.AgentRuntime{Kernel: k, Artifacts: store, Tools: tools.NewRuntime(), Backend: backend, MaxSteps: 5, BackendRetry: fastRetry}
		result, err := agent.RunWithOptions(context.Background(), process.PID, []runtime.Message{{Role: "user", Content: "goal"}}, runtime.RunOptions{ContinueOnToolError: true})
		status := result.ExitStatus
		if err != nil || backend.calls != 1 || len(kernelEvents(t, k, "backend.retry")) != 0 {
			t.Fatalf("one attempt, no retry: %d calls, %v", backend.calls, err)
		}
		if status.Reason != "model endpoint refused the call, not retried: 404 Not Found" || !status.Retryable || status.Checkpoint == "" {
			t.Fatalf("exit = %+v", status)
		}
	})
	// A temporary failure with a retry-after waits that long, capped at the
	// policy's Max, and the event says so.
	t.Run("retry-after", func(t *testing.T) {
		k, store, process := newMemoryFixture(t)
		backend := &funcBackend{fn: func(call int, _ context.Context, _ runtime.GenerateRequest) (runtime.GenerateResult, error) {
			switch call {
			case 1:
				return runtime.GenerateResult{}, &driverError{text: "429", temporary: true, wait: time.Millisecond}
			case 2:
				return runtime.GenerateResult{}, &driverError{text: "429", temporary: true, wait: time.Hour}
			}
			return exitResult("done"), nil
		}}
		var waits []time.Duration
		agent := &runtime.AgentRuntime{Kernel: k, Artifacts: store, Tools: tools.NewRuntime(), Backend: backend, MaxSteps: 5,
			BackendRetry: runtime.BackendRetryPolicy{Base: time.Second, Max: 2 * time.Second}, Sleep: func(_ context.Context, d time.Duration) { waits = append(waits, d) }}
		if result, err := agent.RunWithOptions(context.Background(), process.PID, []runtime.Message{{Role: "user", Content: "goal"}}, runtime.RunOptions{}); err != nil || result.ExitStatus.Reason != "done" {
			t.Fatalf("%+v %v", result.ExitStatus, err)
		}
		events := kernelEvents(t, k, "backend.retry")
		if fmt.Sprint(waits) != "[1ms 2s]" || len(events) != 2 || events[1].Data["retry_after"] != "1h0m0s" || events[1].Data["delay"] != "2s" {
			t.Fatalf("waits %v, events %+v", waits, events)
		}
	})
	// A call the driver refused for want of tokens ends the step as a spent
	// token budget does, without a retry.
	t.Run("no tokens for a reply", func(t *testing.T) {
		k, store, process := newMemoryFixture(t)
		backend := &funcBackend{fn: func(int, context.Context, runtime.GenerateRequest) (runtime.GenerateResult, error) {
			return runtime.GenerateResult{}, fmt.Errorf("%w: the prompt is about 900 tokens and 800 are left", runtime.ErrNoReplyTokens)
		}}
		agent := &runtime.AgentRuntime{Kernel: k, Artifacts: store, Tools: tools.NewRuntime(), Backend: backend, MaxSteps: 5, BackendRetry: fastRetry}
		result, _ := agent.RunWithOptions(context.Background(), process.PID, []runtime.Message{{Role: "user", Content: "goal"}}, runtime.RunOptions{})
		if status := result.ExitStatus; backend.calls != 1 || !strings.HasPrefix(status.Reason, "token budget exceeded: no tokens left for a reply") || !status.Retryable || status.Checkpoint == "" {
			t.Fatalf("%d calls, exit %+v", backend.calls, status)
		}
	})
}

// driverError is a BackendError, as a driver's error says how to retry it.
type driverError struct {
	text      string
	temporary bool
	wait      time.Duration
}

func (e *driverError) Error() string            { return e.text }
func (e *driverError) Temporary() bool          { return e.temporary }
func (e *driverError) RetryWait() time.Duration { return e.wait }

// Evidence may cite messages the way the history shows them; it is stored
// in the canonical form, and a bad form is refused with the forms accepted.
func TestEvidenceAcceptsTheFormsTheHistoryShows(t *testing.T) {
	k, store, process := newMemoryFixture(t)
	agent := &runtime.AgentRuntime{Kernel: k, Artifacts: store, Tools: tools.NewRuntime(), MaxSteps: 20}
	results, errs := memoryRun(t, agent, process.PID,
		setMemory("decisions", "a", "x", "#1"),
		setMemory("decisions", "b", "x", "#1-#2"),
		setMemory("decisions", "c", "x", "#1-2, msg 2"),
		setMemory("decisions", "d", "x", "messages 1-2"),
		setMemory("decisions", "e", "x", "# 1 - 2"),
		setMemory("decisions", "f", "x", "#one"),
	)
	if len(results) != 5 || len(errs) != 1 {
		t.Fatalf("results %d, errors %v", len(results), errs)
	}
	if !strings.Contains(errs[0].Error(), "#12, #12-#15, msg 12 or messages 12-15") {
		t.Fatalf("the refusal should list the accepted forms: %v", errs[0])
	}
	memory := readMemory(t, store, results[len(results)-1].Output["artifact_id"])
	for key, want := range map[string]string{"a": "1", "b": "1-2", "c": "1-2, 2", "d": "1-2", "e": "1-2"} {
		if entry, _ := memory.Entry("decisions", key); entry.Evidence != want {
			t.Fatalf("%s: evidence %q, want %q", key, entry.Evidence, want)
		}
	}
}

// Replacing an entry shows the content it had, the evidence and the
// numbers and identifiers the new one dropped; adding a key lists the keys
// it joins. With the aid off, the result is what it was.
func TestOverwriteShowsWhatItDrops(t *testing.T) {
	actions := []runtime.GenerateResult{
		setMemory("measurements", "occupancy", "max_threads_per_group=1024 measured with MTLComputePipelineState at 30.9 ms", "1"),
		setMemory("measurements", "occupancy", "occupancy checked, fine", "2"),
		setMemory("measurements", "occupancy", "occupancy checked, fine", "2"),
		setMemory("measurements", "bvh", "stackless", ""),
	}
	k, store, process := newMemoryFixture(t)
	agent := &runtime.AgentRuntime{Kernel: k, Artifacts: store, Tools: tools.NewRuntime(), MaxSteps: 20}
	results, errs := memoryRun(t, agent, process.PID, actions...)
	if len(results) != 4 || len(errs) != 0 {
		t.Fatalf("results %d, errors %v", len(results), errs)
	}
	if _, ok := results[0].Output["notice"]; ok {
		t.Fatal("a first write replaces nothing")
	}
	notice := results[1].Output["notice"]
	for _, want := range []string{
		"runtime notice: this replaced measurements/occupancy",
		`"max_threads_per_group=1024 measured with MTLComputePipelineState at 30.9 ms"`,
		"no longer cites evidence 1",
		"it no longer holds max_threads_per_group, 1024, MTLComputePipelineState, 30.9",
	} {
		if !strings.Contains(notice, want) {
			t.Fatalf("notice lacks %q: %s", want, notice)
		}
	}
	if _, ok := results[2].Output["notice"]; ok {
		t.Fatal("setting an entry to what it holds drops nothing")
	}
	if keys := results[3].Output["section_keys"]; !strings.HasPrefix(keys, "occupancy (a new key replaces none of these") {
		t.Fatalf("a new key should list the section's keys: %q", keys)
	}
	if items := noticeItems(t, k); items[runtime.NoticeOverwrite] != 1 {
		t.Fatalf("one w1 notice should be recorded: %v", items)
	}

	k, store, process = newMemoryFixture(t)
	agent = &runtime.AgentRuntime{Kernel: k, Artifacts: store, Tools: tools.NewRuntime(), MaxSteps: 20, Recall: runtime.RecallPolicy{NoOverwriteWarning: true}}
	results, _ = memoryRun(t, agent, process.PID, actions...)
	for _, result := range results {
		var keys []string
		for key := range result.Output {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		if !reflect.DeepEqual(keys, []string{"adopted", "artifact_id", "bytes", "entry", "version"}) {
			t.Fatalf("with the aid off the result should be as before: %v", keys)
		}
	}
	if items := noticeItems(t, k); len(items) != 0 {
		t.Fatalf("no notices with the aid off: %v", items)
	}
}

var indexRange = regexp.MustCompile(`index messages (\d+)-(\d+) with index_memory`)

// handoffScript writes 1000-character messages; at each handoff notice it
// queues the actions plan returns for that notice (numbered from 1, with
// the range the notice asks to index, or 0-0), and exits after the last
// notice once its queue is empty.
type handoffScript struct {
	requests []runtime.GenerateRequest
	queue    []runtime.GenerateResult
	notices  int
}

func (s *handoffScript) backend(last int, plan func(notice, from, to int) []runtime.GenerateResult) *funcBackend {
	return &funcBackend{fn: func(call int, _ context.Context, request runtime.GenerateRequest) (runtime.GenerateResult, error) {
		s.requests = append(s.requests, request)
		latest := runtime.Message{}
		if len(request.History) > 0 {
			latest = request.History[len(request.History)-1]
		}
		if latest.Role == "notice" && strings.Contains(latest.Content, "will move to the archive") {
			s.notices++
			from, to := 0, 0
			if m := indexRange.FindStringSubmatch(latest.Content); m != nil {
				from, _ = strconv.Atoi(m[1])
				to, _ = strconv.Atoi(m[2])
			}
			s.queue = append(s.queue, plan(s.notices, from, to)...)
		}
		if len(s.queue) > 0 {
			next := s.queue[0]
			s.queue = s.queue[1:]
			return next, nil
		}
		if s.notices >= last {
			return exitResult("done"), nil
		}
		return markedMessage(call, 1000), nil
	}}
}

func say(text string) runtime.GenerateResult {
	return runtime.GenerateResult{Action: runtime.GenerateAction{Type: "message", Message: text}}
}

func indexAction(text string) runtime.GenerateResult {
	return toolAction("index_memory", map[string]string{"index": text})
}

// lastNotice is the newest runtime notice a request shows, if its last
// message is one.
func lastNotice(request runtime.GenerateRequest) string {
	latest := request.History[len(request.History)-1]
	if latest.Role != "notice" {
		return ""
	}
	return latest.Content
}

// An ignored handoff notice is repeated once, when the agent has no
// working memory; then compaction follows whatever the agent does. With
// the aid off, or with working memory already kept, it compacts at once.
func TestIgnoredHandoffIsRepeatedOnce(t *testing.T) {
	policy := runtime.CompactionPolicy{MaxChars: 12000, Handoff: true}
	run := func(t *testing.T, recall runtime.RecallPolicy, keep bool) (*kernel.Kernel, *handoffScript) {
		k, store, process := newMemoryFixture(t)
		script := &handoffScript{}
		backend := script.backend(1, func(int, int, int) []runtime.GenerateResult {
			return []runtime.GenerateResult{say("busy"), say("still busy")}
		})
		agent := &runtime.AgentRuntime{Kernel: k, Artifacts: store, Tools: tools.NewRuntime(), Backend: backend, MaxSteps: 40, Compaction: policy, Recall: recall}
		initial := []runtime.Message{{Role: "user", Content: "goal"}}
		if keep {
			backend.fn = wrapFirst(backend.fn, setMemory("open", "plan", "draft", ""))
		}
		if _, err := agent.Run(context.Background(), process.PID, initial); err != nil {
			t.Fatal(err)
		}
		return k, script
	}
	reminders := func(script *handoffScript) (n int, refsAfter int) {
		for i, request := range script.requests {
			if strings.HasPrefix(lastNotice(request), "runtime notice: Reminder: history through message ") {
				n++
				refsAfter = len(script.requests[i+1].MemoryRefs)
			}
		}
		return n, refsAfter
	}

	k, script := run(t, runtime.RecallPolicy{NoAgentIndex: true}, false)
	if n, refs := reminders(script); n != 1 || refs != 1 {
		t.Fatalf("one reminder, then compaction at the next action: %d reminders, %d refs after", n, refs)
	}
	events := compactionEvents(t, k)
	if len(events) != 1 || events[0].Data["handoff_renoticed"] != "true" || noticeItems(t, k)[runtime.NoticeRenotice] != 1 {
		t.Fatalf("the reminder should be recorded: %+v %v", events, noticeItems(t, k))
	}

	// A failed memory write after the reminder neither compacts at once
	// nor earns a second reminder.
	{
		k, store, process := newMemoryFixture(t)
		script := &handoffScript{}
		backend := script.backend(1, func(int, int, int) []runtime.GenerateResult {
			return []runtime.GenerateResult{say("busy"), indexAction("not an index"), say("still busy")}
		})
		agent := &runtime.AgentRuntime{Kernel: k, Artifacts: store, Tools: tools.NewRuntime(), Backend: backend, MaxSteps: 40, Compaction: policy}
		if _, err := agent.RunWithOptions(context.Background(), process.PID, []runtime.Message{{Role: "user", Content: "goal"}}, runtime.RunOptions{ContinueOnToolError: true}); err != nil {
			t.Fatal(err)
		}
		if n, _ := reminders(script); n != 1 || len(compactionEvents(t, k)) != 1 {
			t.Fatalf("one reminder at most: %d", n)
		}
	}

	for name, tc := range map[string]struct {
		recall runtime.RecallPolicy
		keep   bool
	}{
		"aid off":               {runtime.RecallPolicy{NoAgentIndex: true, NoHandoffRenotice: true}, false},
		"working memory exists": {runtime.RecallPolicy{NoAgentIndex: true}, true},
	} {
		k, script := run(t, tc.recall, tc.keep)
		if n, _ := reminders(script); n != 0 || noticeItems(t, k)[runtime.NoticeRenotice] != 0 {
			t.Fatalf("%s: no reminder expected, got %d", name, n)
		}
		if len(compactionEvents(t, k)) != 1 {
			t.Fatalf("%s: one compaction expected", name)
		}
	}
}

// wrapFirst makes fn's first call return first instead.
func wrapFirst(fn func(int, context.Context, runtime.GenerateRequest) (runtime.GenerateResult, error), first runtime.GenerateResult) func(int, context.Context, runtime.GenerateRequest) (runtime.GenerateResult, error) {
	return func(call int, ctx context.Context, request runtime.GenerateRequest) (runtime.GenerateResult, error) {
		if call == 1 {
			return first, nil
		}
		return fn(call, ctx, request)
	}
}

// At handoff the agent indexes the messages being archived. The index is
// checked against that range, kept through the handoff steps it takes to
// write memory, stored with its segment, which a later handoff never
// changes, shown in the next prompt's refs and by read_memory.
func TestAgentIndexIsStoredWithItsSegment(t *testing.T) {
	k, store, process := newMemoryFixture(t)
	var errs []error
	var reads []tools.Result
	ranges := map[int][2]int{}
	script := &handoffScript{}
	backend := script.backend(2, func(notice, from, to int) []runtime.GenerateResult {
		ranges[notice] = [2]int{from, to}
		if notice == 1 {
			return []runtime.GenerateResult{
				indexAction(fmt.Sprintf("#%d: the goal\n- %d-%d: filler messages", from, from+1, to)),
				indexAction(fmt.Sprintf("%d: past the range", to+5)),
				setMemory("open", "plan", "draft", strconv.Itoa(from)),
				say("back to work"),
			}
		}
		return []runtime.GenerateResult{
			indexAction(fmt.Sprintf("%d-%d: second stretch", from, to)),
			say("back to work"),
		}
	})
	agent := &runtime.AgentRuntime{Kernel: k, Artifacts: store, Tools: tools.NewRuntime(), Backend: backend, MaxSteps: 80,
		Compaction: runtime.CompactionPolicy{MaxChars: 12000, Handoff: true}}
	options := runtime.RunOptions{
		ContinueOnToolError: true,
		OnToolError:         func(_ runtime.ToolCall, err error) { errs = append(errs, err) },
	}
	if _, err := agent.RunWithOptions(context.Background(), process.PID, []runtime.Message{{Role: "user", Content: "goal"}}, options); err != nil {
		t.Fatal(err)
	}
	if script.notices != 2 {
		t.Fatalf("expected two handoffs, got %d", script.notices)
	}
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), fmt.Sprintf("cites messages outside %d-%d, the ones being archived", ranges[1][0], ranges[1][1])) {
		t.Fatalf("the out-of-range line should be refused with the range: %v", errs)
	}

	events := compactionEvents(t, k)
	if len(events) != 2 {
		t.Fatalf("compactions: %d", len(events))
	}
	first, second := events[0].Data["segment"], events[1].Data["segment"]
	want := []runtime.IndexLine{{From: ranges[1][0], To: ranges[1][0], Text: "the goal"}, {From: ranges[1][0] + 1, To: ranges[1][1], Text: "filler messages"}}
	if got, err := runtime.SegmentIndex(store, first); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("first segment's index = %+v %v, want %+v", got, err, want)
	}
	if got, _ := runtime.SegmentIndex(store, second); len(got) != 1 || got[0].Text != "second stretch" {
		t.Fatalf("second segment's index = %+v", got)
	}
	if events[0].Data["agent_index_lines"] != "2" || events[0].Data["handoff_steps"] != "3" {
		t.Fatalf("the compaction should record the index and the handoff steps: %+v", events[0].Data)
	}
	last := script.requests[len(script.requests)-1]
	if len(last.MemoryRefs) != 2 || !reflect.DeepEqual(last.MemoryRefs[0].Index, want) {
		t.Fatalf("the refs should carry each segment's index: %+v", last.MemoryRefs)
	}

	// read_memory shows a segment's whole index, on every page: the
	// header's "+K more lines" points here for the lines it hides.
	agent.Backend = &funcBackend{fn: func(call int, _ context.Context, _ runtime.GenerateRequest) (runtime.GenerateResult, error) {
		switch call {
		case 1:
			return toolAction("read_memory", map[string]string{"artifact_id": first}), nil
		case 2:
			return toolAction("read_memory", map[string]string{"artifact_id": first, "offset": "40"}), nil
		}
		return exitResult("done"), nil
	}}
	reader, _ := k.Spawn(kernel.ProcessSpec{Role: "agent:writer", Metadata: map[string]string{"resumed_from": process.PID}})
	checkpoint := latestOwnCheckpoint(t, store, process.PID)
	options.OnToolResult = func(result tools.Result) { reads = append(reads, result) }
	if _, err := agent.Resume(context.Background(), reader.PID, checkpoint, nil, options); err != nil {
		t.Fatal(err)
	}
	if len(reads) != 2 || reads[0].Output["agent_index"] != runtime.RenderIndex(want) {
		t.Fatalf("read_memory should show the index: %+v", reads)
	}
	if reads[1].Output["offset"] != "40" || reads[1].Output["agent_index"] != runtime.RenderIndex(want) {
		t.Fatalf("a later page should show the whole index too: %+v", reads[1].Output)
	}

	// It reads the index from the segment's ref, where the header shows it
	// from: a checkpoint whose ref carries an index the segment lacks, as
	// memprobe gives one, reads back the ref's.
	var session runtime.Session
	records, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	given := []runtime.IndexLine{{From: ranges[2][0], To: ranges[2][0], Text: "given line one"}, {From: ranges[2][1], To: ranges[2][1], Text: "given line two"}}
	var base artifact.Artifact
	for _, record := range records {
		if record.Type != "checkpoint" || record.ProducerPID != process.PID || base.ID != "" {
			continue
		}
		if err := store.ReadJSON(record.ID, &session); err != nil {
			t.Fatal(err)
		}
		for i := range session.MemoryRefs {
			if session.MemoryRefs[i].ArtifactID == second {
				session.MemoryRefs[i].Index, base = given, record
			}
		}
	}
	if base.ID == "" {
		t.Fatal("no checkpoint holds the second segment")
	}
	cp, err := store.CreateJSON("checkpoint", base.ProducerPID, session, []string{base.ID}, map[string]string{"kind": "intermediate"})
	if err != nil {
		t.Fatal(err)
	}
	agent.Backend = &funcBackend{fn: func(call int, _ context.Context, _ runtime.GenerateRequest) (runtime.GenerateResult, error) {
		if call == 1 {
			return toolAction("read_memory", map[string]string{"artifact_id": second}), nil
		}
		return exitResult("done"), nil
	}}
	reads = nil
	again, _ := k.Spawn(kernel.ProcessSpec{Role: "agent:writer", Metadata: map[string]string{"resumed_from": process.PID}})
	if _, err := agent.Resume(context.Background(), again.PID, cp.ID, nil, options); err != nil {
		t.Fatal(err)
	}
	if len(reads) != 1 || reads[0].Output["agent_index"] != runtime.RenderIndex(given) {
		t.Fatalf("read_memory should show the index the segment's ref carries: %+v", reads)
	}
	asked := 0
	for _, event := range kernelEvents(t, k, "memory.notice") {
		if event.ProcessID == process.PID && event.Data["item"] == runtime.NoticeIndex {
			asked++
		}
	}
	if asked != 2 {
		t.Fatalf("each handoff asking for an index should be recorded: %d", asked)
	}
}

func latestOwnCheckpoint(t *testing.T, store *artifact.Store, pid string) string {
	t.Helper()
	records, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	id := ""
	for _, record := range records {
		if record.Type == "checkpoint" && record.ProducerPID == pid {
			id = record.ID
		}
	}
	return id
}

// index_memory refuses an index with no handoff pending, over its cap, a
// malformed line, and any call when the aid is off; the notice then does
// not ask for one.
func TestIndexMemoryRefusals(t *testing.T) {
	k, store, process := newMemoryFixture(t)
	agent := &runtime.AgentRuntime{Kernel: k, Artifacts: store, Tools: tools.NewRuntime(), MaxSteps: 20}
	_, errs := memoryRun(t, agent, process.PID, indexAction("1: goal"))
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "no handoff pending") {
		t.Fatalf("an index needs a pending handoff: %v", errs)
	}

	refused := func(recall runtime.RecallPolicy, index string) []error {
		k, store, process := newMemoryFixture(t)
		script := &handoffScript{}
		backend := script.backend(1, func(_, from, to int) []runtime.GenerateResult {
			if index == "" {
				return []runtime.GenerateResult{indexAction("1: goal")}
			}
			return []runtime.GenerateResult{indexAction(fmt.Sprintf(index, from, to))}
		})
		agent := &runtime.AgentRuntime{Kernel: k, Artifacts: store, Tools: tools.NewRuntime(), Backend: backend, MaxSteps: 40,
			Compaction: runtime.CompactionPolicy{MaxChars: 12000, Handoff: true}, Recall: recall}
		var errs []error
		if _, err := agent.RunWithOptions(context.Background(), process.PID, []runtime.Message{{Role: "user", Content: "goal"}}, runtime.RunOptions{
			ContinueOnToolError: true,
			OnToolError:         func(_ runtime.ToolCall, err error) { errs = append(errs, err) },
		}); err != nil {
			t.Fatal(err)
		}
		for _, request := range script.requests {
			if recall.NoAgentIndex && strings.Contains(lastNotice(request), "index_memory") {
				t.Fatal("with the aid off the notice should not ask for an index")
			}
		}
		return errs
	}
	if errs := refused(runtime.RecallPolicy{}, "%d-%d: "+strings.Repeat("x", runtime.MaxSegmentIndexBytes)); len(errs) != 1 || !strings.Contains(errs[0].Error(), "over the index's 1536-byte cap") {
		t.Fatalf("an index over the cap should be refused: %v", errs)
	}
	if errs := refused(runtime.RecallPolicy{}, "%d to %d the goal"); len(errs) != 1 || !strings.Contains(errs[0].Error(), `not "N: what is there"`) {
		t.Fatalf("a line without its numbers should be refused: %v", errs)
	}
	if errs := refused(runtime.RecallPolicy{NoAgentIndex: true}, ""); len(errs) != 1 || !strings.Contains(errs[0].Error(), "not enabled") {
		t.Fatalf("index_memory should be refused with the aid off: %v", errs)
	}
}

// historySize is a request's live history as compaction counts it.
func historySize(request runtime.GenerateRequest) int {
	total := 0
	for _, message := range request.History {
		total += len(message.Role) + len(message.Content)
	}
	return total
}

// The handoff steps the recall aids add show the model no more history
// than before them, or than 16 KB (maxHandoffExtraChars) over the trigger
// plus one memory write: a later handoff step is only given while the
// history is within that, and one that would not write memory is compacted
// before its action runs. Two scenarios: worst-case tool results around an
// agent that ignores the notice, then writes the largest entries it can;
// and small messages, where the extra steps are given.
func TestHandoffOverrunIsBounded(t *testing.T) {
	policy := runtime.ContextCompactionPolicy(65536, 0.8)
	big := func(key string) runtime.GenerateResult {
		return setMemory("open", key, strings.Repeat("w ", 1900), "")
	}
	outputs := true
	fill := func(call int) runtime.GenerateResult {
		if outputs {
			return worstRun(5000)
		}
		return markedMessage(call, 6000)
	}
	peak := func(t *testing.T, recall runtime.RecallPolicy) (int, map[string]string) {
		k, store, process := newCancelFixture(t)
		script := &handoffScript{}
		backend := script.backend(1, func(_, from, to int) []runtime.GenerateResult {
			return []runtime.GenerateResult{fill(0), big("a"), indexAction(fmt.Sprintf("%d-%d: %s", from, to, strings.Repeat("i", 1400))), big("b"), big("c"), fill(0), fill(0)}
		})
		inner := backend.fn
		backend.fn = func(call int, ctx context.Context, request runtime.GenerateRequest) (runtime.GenerateResult, error) {
			result, err := inner(call, ctx, request)
			if script.notices == 0 && result.Action.Type == "message" {
				result = fill(call)
			}
			return result, err
		}
		agent := &runtime.AgentRuntime{Kernel: k, Artifacts: store, Tools: tools.NewRuntime(), Backend: backend, MaxSteps: 30, Compaction: policy, Recall: recall}
		if _, err := agent.RunWithOptions(context.Background(), process.PID, []runtime.Message{{Role: "user", Content: "goal"}}, runtime.RunOptions{ContinueOnToolError: true}); err != nil {
			t.Fatal(err)
		}
		most := 0
		for _, request := range script.requests {
			most = max(most, historySize(request))
		}
		events := compactionEvents(t, k)
		if len(events) == 0 {
			t.Fatal("expected a compaction")
		}
		return most, events[0].Data
	}
	// The notice asking for an index is up to 1 KB longer than before.
	bound := func(before int) int {
		return max(before+1024, policy.MaxChars+16*1024+2*runtime.MaxWorkingMemoryBytes+2*runtime.MaxSegmentIndexBytes)
	}
	for _, worst := range []bool{true, false} {
		outputs = worst
		before, _ := peak(t, runtime.RecallOff())
		after, data := peak(t, runtime.RecallPolicy{})
		t.Logf("worst-case outputs %t: peak history shown %d chars with the aids off, %d with them on (trigger %d); first compaction %v",
			worst, before, after, policy.MaxChars, data)
		if after > bound(before) {
			t.Fatalf("the aids showed %d chars, over the bound of %d", after, bound(before))
		}
		if steps, _ := strconv.Atoi(data["handoff_steps"]); !worst && (data["handoff_renoticed"] != "true" || steps < 3 || data["agent_index_lines"] != "1") {
			t.Fatalf("with small outputs the extra steps should be given: %v", data)
		}
	}
}

// A HandoffOnly run asks a process resumed from a checkpoint for its
// handoff and parks once the history is compacted, before any other action
// runs; the checkpoint it started from is untouched.
func TestHandoffOnlyRunIndexesACheckpoint(t *testing.T) {
	k, store, process := newMemoryFixture(t)
	writer := &funcBackend{fn: func(call int, _ context.Context, _ runtime.GenerateRequest) (runtime.GenerateResult, error) {
		if call > 10 {
			return exitResult("done"), nil
		}
		return markedMessage(call, 1000), nil
	}}
	agent := &runtime.AgentRuntime{Kernel: k, Artifacts: store, Tools: tools.NewRuntime(), Backend: writer, MaxSteps: 20,
		Compaction: runtime.CompactionPolicy{MaxChars: 1 << 20, Handoff: true}}
	ran, err := agent.Run(context.Background(), process.PID, []runtime.Message{{Role: "user", Content: "goal"}})
	if err != nil {
		t.Fatal(err)
	}
	before := checkpointMessages(t, store, ran.CheckpointID)

	probe, _ := k.Spawn(kernel.ProcessSpec{Role: "agent:writer", Metadata: map[string]string{"resumed_from": process.PID}})
	script := &handoffScript{}
	agent.Backend = script.backend(1, func(_, from, to int) []runtime.GenerateResult {
		return []runtime.GenerateResult{
			indexAction(fmt.Sprintf("%d-%d: marked filler", from, to)),
			setMemory("open", "plan", "draft", ""),
			toolAction("run", map[string]string{"command": "touch should-not-run"}),
		}
	})
	agent.Compaction = runtime.CompactionPolicy{MaxChars: 8000, Handoff: true}
	var ran2 []string
	result, err := agent.Resume(context.Background(), probe.PID, ran.CheckpointID, nil, runtime.RunOptions{
		HandoffOnly:         true,
		ContinueOnToolError: true,
		OnToolCall:          func(call runtime.ToolCall) { ran2 = append(ran2, call.Name) },
	})
	if err != nil || !result.Yielded {
		t.Fatalf("the run should park after its handoff: %+v %v", result, err)
	}
	if got, _ := k.Get(probe.PID); got.State != kernel.StateReady {
		t.Fatalf("the process should be ready, is %s", got.State)
	}
	if !reflect.DeepEqual(ran2, []string{"index_memory", "update_memory"}) {
		t.Fatalf("only the memory tools should have run: %v", ran2)
	}
	session := checkpointMessages(t, store, result.CheckpointID)
	if len(session.MemoryRefs) != 1 || len(session.MemoryRefs[0].Index) != 1 || session.MemoryRefs[0].Index[0].Text != "marked filler" {
		t.Fatalf("the handoff should leave one indexed segment: %+v", session.MemoryRefs)
	}
	if after := checkpointMessages(t, store, ran.CheckpointID); !reflect.DeepEqual(before, after) {
		t.Fatal("the source checkpoint changed")
	}

	short, _ := k.Spawn(kernel.ProcessSpec{Role: "agent:writer"})
	if _, err := agent.RunWithOptions(context.Background(), short.PID, []runtime.Message{{Role: "user", Content: "goal"}}, runtime.RunOptions{HandoffOnly: true}); !errors.Is(err, runtime.ErrNothingToArchive) {
		t.Fatalf("a history with nothing to archive should be refused: %v", err)
	}
}

// Each No field turns off only its own aid: a policy that sets one keeps
// the others on.
func TestRecallSwitchesAreIndependent(t *testing.T) {
	k, store, process := newMemoryFixture(t)
	agent := &runtime.AgentRuntime{Kernel: k, Artifacts: store, Tools: tools.NewRuntime(), MaxSteps: 20, Recall: runtime.RecallPolicy{NoAgentIndex: true, NoHandoffRenotice: true}}
	results, _ := memoryRun(t, agent, process.PID, setMemory("open", "a", "v1 at 3", ""), setMemory("open", "a", "v2", ""))
	if !strings.Contains(results[1].Output["notice"], "it no longer holds v1, 3") {
		t.Fatalf("w1 should stay on: %+v", results[1].Output)
	}
	if off := runtime.RecallOff(); !off.NoOverwriteWarning || !off.NoPointerPrompt || !off.NoHandoffRenotice || !off.NoAgentIndex || !off.NoPartialIndexNote {
		t.Fatalf("RecallOff should turn every aid off: %+v", off)
	}
}

// handoffThrough returns the through_seq of each memory.handoff event.
func handoffThrough(t *testing.T, k *kernel.Kernel) []string {
	var out []string
	for _, event := range kernelEvents(t, k, "memory.handoff") {
		out = append(out, event.Data["through_seq"])
	}
	return out
}

// With the index on, a handoff archives exactly the messages its notice
// named, which are the ones the agent indexed, however much the history
// grew during the handoff; with the aids off the cut is recomputed then and
// can go past the notice, as before. A reminder for a missing index names
// index_memory and the range.
func TestHandoffArchivesWhatTheNoticeNamed(t *testing.T) {
	policy := runtime.CompactionPolicy{MaxChars: 12000, Handoff: true}
	run := func(t *testing.T, recall runtime.RecallPolicy) (*kernel.Kernel, *handoffScript, [2]int) {
		k, store, process := newMemoryFixture(t)
		script := &handoffScript{}
		var named [2]int
		backend := script.backend(1, func(notice, from, to int) []runtime.GenerateResult {
			if notice > 1 {
				return nil
			}
			named = [2]int{from, to}
			return []runtime.GenerateResult{
				say("busy " + strings.Repeat("g", 6000)),
				indexAction(fmt.Sprintf("%d-%d: everything so far", from, to)),
				say("back to work"),
			}
		})
		agent := &runtime.AgentRuntime{Kernel: k, Artifacts: store, Tools: tools.NewRuntime(), Backend: backend, MaxSteps: 40, Compaction: policy, Recall: recall}
		if _, err := agent.RunWithOptions(context.Background(), process.PID, []runtime.Message{{Role: "user", Content: "goal"}}, runtime.RunOptions{ContinueOnToolError: true}); err != nil {
			t.Fatal(err)
		}
		return k, script, named
	}

	k, script, named := run(t, runtime.RecallPolicy{})
	through := handoffThrough(t, k)
	events := compactionEvents(t, k)
	// What grew during the handoff is archived by the next one, under its
	// own notice and index.
	if len(through) == 0 || len(events) == 0 {
		t.Fatalf("expected a compaction per handoff: %v %d", through, len(events))
	}
	data := events[0].Data
	if data["from_seq"] != strconv.Itoa(named[0]) || data["to_seq"] != strconv.Itoa(named[1]) || data["to_seq"] != through[0] {
		t.Fatalf("archived %s-%s, the notice named %d-%d (through %s)", data["from_seq"], data["to_seq"], named[0], named[1], through[0])
	}
	if data["agent_index_lines"] != "1" {
		t.Fatalf("the index of the named range should be kept whole: %v", data)
	}
	if size, _ := strconv.Atoi(data["live_chars"]); size <= policy.MaxChars/2 {
		t.Fatalf("the scenario should have grown the history past the target (%d), or it proves nothing", size)
	}
	reminded := false
	for _, request := range script.requests {
		if notice := lastNotice(request); strings.HasPrefix(notice, "runtime notice: Reminder:") {
			reminded = true
			if !strings.Contains(notice, fmt.Sprintf("you have not indexed it. Call index_memory now for messages %d-%d", named[0], named[1])) ||
				!strings.Contains(notice, "in at most 1536 bytes") || !strings.Contains(notice, "Then update your working memory") {
				t.Fatalf("the reminder should name index_memory, the range and the cap: %q", notice)
			}
		}
	}
	if !reminded {
		t.Fatal("the ignored notice should have been repeated")
	}

	k, _, _ = run(t, runtime.RecallOff())
	through, events = handoffThrough(t, k), compactionEvents(t, k)
	if len(events) == 0 || events[0].Data["to_seq"] == through[0] {
		t.Fatalf("with the aids off the cut should still be recomputed past the notice: %v %v", through, events)
	}
}

// The notice asks for the index first and states its range and cap; the
// cap grows with the messages archived, within its floor and ceiling.
func TestIndexCapScalesWithTheArchive(t *testing.T) {
	for messages, want := range map[int]int{1: 1536, 24: 1536, 40: 2560, 64: 4096, 500: 4096} {
		if got := runtime.SegmentIndexCap(messages); got != want {
			t.Fatalf("SegmentIndexCap(%d) = %d, want %d", messages, got, want)
		}
	}
	k, store, process := newMemoryFixture(t)
	script := &handoffScript{}
	backend := script.backend(1, func(int, int, int) []runtime.GenerateResult { return nil })
	agent := &runtime.AgentRuntime{Kernel: k, Artifacts: store, Tools: tools.NewRuntime(), Backend: backend, MaxSteps: 40,
		Compaction: runtime.CompactionPolicy{MaxChars: 12000, Handoff: true}}
	if _, err := agent.Run(context.Background(), process.PID, []runtime.Message{{Role: "user", Content: "goal"}}); err != nil {
		t.Fatal(err)
	}
	for _, request := range script.requests {
		notice := lastNotice(request)
		if !strings.Contains(notice, "will move to the archive") {
			continue
		}
		index, memory := strings.Index(notice, "First index messages"), strings.Index(notice, "Then update your working memory")
		if index < 0 || memory < index || !strings.Contains(notice, "in at most 1536 bytes") {
			t.Fatalf("the notice should ask for the index first, with its cap: %q", notice)
		}
		return
	}
	t.Fatal("no handoff notice")
}

// An index with bad lines keeps the good ones and reports the rest, with
// reasons; the write earns a retry step, and the retry's index replaces
// the first. Lines past the cap are rejected the same way.
func TestIndexKeepsGoodLinesAndRetries(t *testing.T) {
	k, store, process := newMemoryFixture(t)
	var results []tools.Result
	script := &handoffScript{}
	backend := script.backend(1, func(_, from, to int) []runtime.GenerateResult {
		return []runtime.GenerateResult{
			indexAction(fmt.Sprintf("%d: the goal\n%d: past the range\nno numbers here\n%d-%d: filler\n%d: %s", from, to+5, from+1, to, from, strings.Repeat("c", 1600))),
			indexAction(fmt.Sprintf("%d: the goal\n%d-%d: filler\n%d: the last one", from, from+1, to-1, to)),
			say("back to work"),
		}
	})
	agent := &runtime.AgentRuntime{Kernel: k, Artifacts: store, Tools: tools.NewRuntime(), Backend: backend, MaxSteps: 40,
		Compaction: runtime.CompactionPolicy{MaxChars: 12000, Handoff: true}}
	if _, err := agent.RunWithOptions(context.Background(), process.PID, []runtime.Message{{Role: "user", Content: "goal"}}, runtime.RunOptions{
		ContinueOnToolError: true,
		OnToolResult: func(result tools.Result) {
			if result.Tool == "index_memory" {
				results = append(results, result)
			}
		},
	}); err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("index_memory results: %d", len(results))
	}
	first := results[0].Output
	if first["lines"] != "2" || strings.Count(first["rejected"], "\n") != 2 ||
		!strings.Contains(first["rejected"], "cites messages outside") || !strings.Contains(first["rejected"], `"no numbers here": not "N: what is there"`) ||
		!strings.Contains(first["rejected"], "over the index's 1536-byte cap") || !strings.Contains(first["notice"], "resend every line") {
		t.Fatalf("the first index should keep two lines and report three: %v", first)
	}
	if results[1].Output["replaced"] != "true" || results[1].Output["rejected"] != "" {
		t.Fatalf("the retry should replace the index: %v", results[1].Output)
	}
	events := compactionEvents(t, k)
	if len(events) != 1 {
		t.Fatalf("compactions: %d", len(events))
	}
	if data := events[0].Data; data["agent_index_lines"] != "3" || data["handoff_retried"] != "true" || data["handoff_end"] != "other_action" {
		t.Fatalf("the segment should keep the retry's index: %v", data)
	}
}

// A refused index earns exactly one retry step, even past the 16 KB bound
// on later handoff steps; that step sees only the refused call and its
// bounded report beyond what the refused step saw. A second refusal ends
// the handoff.
func TestRefusedIndexGetsOneRetry(t *testing.T) {
	policy := runtime.CompactionPolicy{MaxChars: 12000, Handoff: true}
	huge := strings.Repeat("this line has no number\n", 1000)
	run := func(t *testing.T, retry func(from, to int) runtime.GenerateResult) (*kernel.Kernel, *handoffScript, []error) {
		k, store, process := newMemoryFixture(t)
		var errs []error
		script := &handoffScript{}
		backend := script.backend(1, func(notice, from, to int) []runtime.GenerateResult {
			if notice > 1 {
				return nil
			}
			return []runtime.GenerateResult{indexAction(huge), retry(from, to), say("back to work")}
		})
		agent := &runtime.AgentRuntime{Kernel: k, Artifacts: store, Tools: tools.NewRuntime(), Backend: backend, MaxSteps: 40, Compaction: policy}
		if _, err := agent.RunWithOptions(context.Background(), process.PID, []runtime.Message{{Role: "user", Content: "goal"}}, runtime.RunOptions{
			ContinueOnToolError: true,
			OnToolError:         func(_ runtime.ToolCall, err error) { errs = append(errs, err) },
		}); err != nil {
			t.Fatal(err)
		}
		return k, script, errs
	}
	k, script, errs := run(t, func(from, to int) runtime.GenerateResult {
		return indexAction(fmt.Sprintf("%d-%d: everything so far", from, to))
	})
	if len(errs) != 1 || len(errs[0].Error()) > 1200 || !strings.Contains(errs[0].Error(), "... and ") {
		t.Fatalf("the refusal should report within its bound: %d %v", len(errs), errs)
	}
	refusedAt := -1
	for i, request := range script.requests {
		if strings.Contains(lastNotice(request), "will move to the archive") {
			refusedAt = i
			break
		}
	}
	shown, retried := historySize(script.requests[refusedAt]), historySize(script.requests[refusedAt+1])
	if shown+len(huge) <= policy.MaxChars+16*1024 {
		t.Fatal("the refused call should take the history past the 16 KB bound, or this proves nothing")
	}
	added := script.requests[refusedAt+1].History[len(script.requests[refusedAt].History):]
	if len(added) != 2 || !strings.Contains(added[0].Content, `"index_memory"`) || added[1].Role != "tool" || len(added[1].Content) > 1300 {
		t.Fatalf("the retry step should see only the refused call and its report beyond the refused step: %d messages", len(added))
	}
	t.Logf("refused step shown %d chars, retry step %d: the call's record %d and its report %d", shown, retried, len(added[0].Content), len(added[1].Content))
	data := compactionEvents(t, k)[0].Data
	if data["handoff_retried"] != "true" || data["agent_index_lines"] != "1" || data["handoff_end"] != "overrun" {
		t.Fatalf("the retry's index should be kept, then the overrun compacts: %v", data)
	}

	k, _, errs = run(t, func(int, int) runtime.GenerateResult { return indexAction(huge) })
	data = compactionEvents(t, k)[0].Data
	if len(errs) != 2 || data["agent_index_lines"] != "0" || data["handoff_steps"] != "2" {
		t.Fatalf("a second refusal should end the handoff: %d errors, %v", len(errs), data)
	}
}

// An index written on one line ("59: a 61: b") is split into its entries
// at each later number within the archived range and after the entry
// before it; a number in an entry's text is left alone.
func TestRunOnIndexIsSplit(t *testing.T) {
	k, store, process := newMemoryFixture(t)
	var results []tools.Result
	script := &handoffScript{}
	backend := script.backend(1, func(notice, from, to int) []runtime.GenerateResult {
		if notice > 1 {
			return nil
		}
		return []runtime.GenerateResult{
			indexAction(fmt.Sprintf("- #%d: the goal %d-%d: filler; %d: the last one, ratio 3: 1 kept %d: not in range", from, from+1, to-1, to, to+40)),
			say("back to work"),
		}
	})
	agent := &runtime.AgentRuntime{Kernel: k, Artifacts: store, Tools: tools.NewRuntime(), Backend: backend, MaxSteps: 40,
		Compaction: runtime.CompactionPolicy{MaxChars: 12000, Handoff: true}}
	if _, err := agent.RunWithOptions(context.Background(), process.PID, []runtime.Message{{Role: "user", Content: "goal"}}, runtime.RunOptions{
		ContinueOnToolError: true,
		OnToolResult: func(result tools.Result) {
			if result.Tool == "index_memory" {
				results = append(results, result)
			}
		},
	}); err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("index_memory results: %d", len(results))
	}
	from, to := 0, 0
	fmt.Sscanf(results[0].Output["messages"], "%d-%d", &from, &to)
	want := fmt.Sprintf("%d: the goal\n%d-%d: filler;\n%d: the last one, ratio 3: 1 kept %d: not in range\n", from, from+1, to-1, to, to+40)
	if results[0].Output["index"] != want || results[0].Output["rejected"] != "" {
		t.Fatalf("index = %q, want %q (rejected %q)", results[0].Output["index"], want, results[0].Output["rejected"])
	}
	if data := compactionEvents(t, k)[0].Data; data["agent_index_lines"] != "3" {
		t.Fatalf("the segment should keep the three entries: %v", data)
	}
}
