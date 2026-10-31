package memprobe_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"aos/internal/artifact"
	"aos/internal/kernel"
	"aos/internal/memprobe"
	"aos/internal/runtime"
)

// scriptedCompleter answers with its actions in turn, then with a message,
// and records every prompt it was given.
type scriptedCompleter struct {
	actions []map[string]any
	calls   int
	systems []string
	users   []string
}

func (c *scriptedCompleter) CompleteJSON(_ context.Context, system, user string, target any) error {
	c.systems, c.users = append(c.systems, system), append(c.users, user)
	action := map[string]any{"type": "message", "message": "TG4."}
	if c.calls < len(c.actions) {
		action = c.actions[c.calls]
	}
	c.calls++
	encoded, _ := json.Marshal(map[string]any{"action": action})
	return json.Unmarshal(encoded, target)
}

// CompleteMessages makes the completer take the chat-messages path, as the
// vLLM backend does: the system message and the rest are recorded apart.
func (c *scriptedCompleter) CompleteMessages(ctx context.Context, messages []runtime.Message) (string, error) {
	var system, user []string
	for _, message := range messages {
		if message.Role == "system" {
			system = append(system, message.Content)
		} else {
			user = append(user, message.Content)
		}
	}
	var out map[string]any
	if err := c.CompleteJSON(ctx, strings.Join(system, "\n"), strings.Join(user, "\n"), &out); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(out)
	return string(encoded), err
}

func memoryTool(name string, args map[string]string) map[string]any {
	return map[string]any{"type": "tool", "tool_call": map[string]any{"name": name, "args": args}}
}

// The runtime's zero recall policy turns every aid on, so memprobe never
// passes it through: a Config that names no aid runs with the prompts from
// before the aids, and each named aid is turned on alone.
func TestProbesRunWithEveryAidOffUnlessNamed(t *testing.T) {
	stateDir, checkpoint, _ := probedRun(t)
	probes := []memprobe.Probe{{ID: "tg", Kind: "archive", Checkpoint: checkpoint, Question: "Which threadgroup size did you keep?", Expect: `TG4`,
		WorkingMemory: []memprobe.ProbeMemoryEntry{{Section: "decisions", Key: "tg", Content: "TG4 kept", Evidence: "1"}}}}
	run := func(recall []string) (*scriptedCompleter, memprobe.Sample) {
		t.Helper()
		completer := &scriptedCompleter{}
		samples, err := memprobe.Run(context.Background(), memprobe.Config{
			StateDir: stateDir, WorkDir: t.TempDir(), Workflow: probeWorkflow(), Completer: completer, Recall: recall,
		}, probes)
		if err != nil || len(samples) != 1 || samples[0].Error != "" {
			t.Fatalf("samples = %+v, %v", samples, err)
		}
		return completer, samples[0]
	}

	off, sample := run(nil)
	if strings.Contains(off.systems[0], "index_memory") || strings.Contains(off.users[0], "points at its evidence") {
		t.Fatalf("with no aid named, the prompts should be those from before the aids:\n%s\n%s", off.systems[0], off.users[0])
	}
	if sample.Condition != "memory,check-rule" || sample.AgentIndexSegments != 0 || sample.BackendErrors != 0 {
		t.Fatalf("the condition should read as before the aids: %+v", sample)
	}

	on, sample := run([]string{"a1", "w2"})
	if !strings.Contains(on.systems[0], "index_memory") || !strings.Contains(on.users[0], "points at its evidence") {
		t.Fatalf("a1 and w2 should be on:\n%s\n%s", on.systems[0], on.users[0])
	}
	if sample.Condition != "memory,check-rule,recall=w2+a1" {
		t.Fatalf("the condition should name the aids turned on: %q", sample.Condition)
	}

	if _, err := memprobe.Run(context.Background(), memprobe.Config{StateDir: stateDir, WorkDir: t.TempDir(), Workflow: probeWorkflow(),
		Completer: &scriptedCompleter{}, Recall: []string{"w3"}}, probes); err == nil {
		t.Fatal("an unknown aid should be refused")
	}
	if policy, _ := memprobe.RecallPolicy(nil); policy != runtime.RecallOff() {
		t.Fatalf("no aid named should be RecallOff: %+v", policy)
	}
}

// handoffRun is a state directory left by a writer whose live history is
// long enough to compact, after more steps than one answer may take.
func handoffRun(t *testing.T) (stateDir, checkpoint string) {
	t.Helper()
	stateDir = t.TempDir()
	k, err := kernel.New(filepath.Join(stateDir, "kernel"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := artifact.New(filepath.Join(stateDir, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	writer, err := k.Spawn(kernel.ProcessSpec{Role: "agent:writer", Metadata: map[string]string{"agent_id": "writer"}})
	if err != nil {
		t.Fatal(err)
	}
	var messages []runtime.Message
	for seq := 1; seq <= 12; seq++ {
		role := "assistant"
		if seq%2 == 1 {
			role = "user"
		}
		messages = append(messages, runtime.Message{Role: role, Content: strings.Repeat("sweep result ", 80), Seq: seq})
	}
	cp, err := store.CreateJSON("checkpoint", writer.PID, runtime.Session{Messages: messages, NextSeq: 13, Usage: runtime.Usage{Steps: 50}}, nil, map[string]string{"kind": "intermediate"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k.Exit(writer.PID, kernel.ExitStatus{Code: 0, Reason: "done", Checkpoint: cp.ID}); err != nil {
		t.Fatal(err)
	}
	return stateDir, cp.ID
}

// A handoff on a checkpoint runs on a copy: the agent indexes and records
// what is archived, nothing else runs, and the run's state is unchanged.
func TestHandoffsIndexACheckpointOnACopy(t *testing.T) {
	stateDir, checkpoint := handoffRun(t)
	before := map[string][]byte{}
	for _, rel := range []string{"artifacts/index.json", "kernel/processes.json", "kernel/events.json"} {
		before[rel], _ = os.ReadFile(filepath.Join(stateDir, rel))
	}
	completer := &scriptedCompleter{actions: []map[string]any{
		memoryTool("index_memory", map[string]string{"index": "1-4: the first sweep"}),
		memoryTool("update_memory", map[string]string{"section": "decisions", "key": "tg", "content": "TG4 kept", "evidence": "2"}),
		memoryTool("run", map[string]string{"command": "touch should-not-run"}),
	}}
	work := t.TempDir()
	handoffs, err := memprobe.RunHandoffs(context.Background(), memprobe.Config{
		StateDir: stateDir, WorkDir: work, Workflow: probeWorkflow(), Completer: completer, Recall: []string{"a1"},
		Compaction: runtime.CompactionPolicy{MaxChars: 6000, Handoff: true},
	}, []string{checkpoint})
	if err != nil || len(handoffs) != 1 {
		t.Fatalf("handoffs = %+v, %v", handoffs, err)
	}
	h := handoffs[0]
	if h.Error != "" || h.Segment == "" || h.Result == "" || h.WorkingMemory == "" || h.Condition != "handoff,recall=a1" {
		t.Fatalf("the handoff should archive a segment with working memory: %+v", h)
	}
	if len(h.Index) != 1 || h.Index[0].Text != "the first sweep" {
		t.Fatalf("the segment should hold the agent's index: %+v", h.Index)
	}
	if slices.Contains(h.Tools, "run") || !slices.Contains(h.Tools, "index_memory") || !slices.Contains(h.Tools, "update_memory") {
		t.Fatalf("only the memory tools should have run: %v", h.Tools)
	}
	if _, err := os.Stat(filepath.Join(work, "should-not-run")); err == nil {
		t.Fatal("the probe ran a command")
	}
	for rel, want := range before {
		got, _ := os.ReadFile(filepath.Join(stateDir, rel))
		if !bytes.Equal(got, want) {
			t.Fatalf("the handoff changed the original %s", rel)
		}
	}

	if _, err := memprobe.RunHandoffs(context.Background(), memprobe.Config{StateDir: stateDir, WorkDir: t.TempDir(), Workflow: probeWorkflow(),
		Completer: &scriptedCompleter{}, NoWorkingMemory: true}, []string{checkpoint}); err == nil {
		t.Fatal("a handoff without working memory should be refused")
	}
}

// failingOnce fails its first call, then answers.
type failingOnce struct{ calls int }

func (c *failingOnce) CompleteJSON(_ context.Context, _, _ string, target any) error {
	c.calls++
	if c.calls == 1 {
		return errors.New("endpoint down")
	}
	return json.Unmarshal([]byte(`{"action":{"type":"message","message":"TG4."}}`), target)
}

// Without BackendRetry a failed model call takes a step, as before d65b94f,
// and nothing is retried; with it the call is retried within the step.
func TestProbesRetryModelCallsOnlyWhenAsked(t *testing.T) {
	stateDir, checkpoint, _ := probedRun(t)
	probes := []memprobe.Probe{{ID: "tg", Kind: "archive", Checkpoint: checkpoint, Question: "Which threadgroup size did you keep?", Expect: `TG4`}}
	run := func(retry *runtime.BackendRetryPolicy) (memprobe.Sample, string) {
		t.Helper()
		work := t.TempDir()
		samples, err := memprobe.Run(context.Background(), memprobe.Config{
			StateDir: stateDir, WorkDir: work, Workflow: probeWorkflow(), Completer: &failingOnce{}, BackendRetry: retry,
		}, probes)
		if err != nil || len(samples) != 1 {
			t.Fatalf("samples = %+v, %v", samples, err)
		}
		events, _ := os.ReadFile(filepath.Join(work, "kernel", "events.json"))
		return samples[0], string(events)
	}

	sample, events := run(nil)
	if sample.Error != "" || !sample.Correct || sample.Steps != 2 || sample.BackendErrors != 1 || strings.Contains(events, "backend.retry") {
		t.Fatalf("without retry the failure should take a step and nothing be retried: %+v", sample)
	}
	sample, events = run(&runtime.BackendRetryPolicy{Attempts: 3, Base: time.Millisecond})
	if sample.Error != "" || !sample.Correct || sample.BackendErrors != 1 || !strings.Contains(events, "backend.retry") {
		t.Fatalf("with retry the failed call should be retried: %+v", sample)
	}
}

// indexedRun is probedRun with its segment's message range on the
// checkpoint's reference, as the runtime writes it.
func indexedRun(t *testing.T) (stateDir, checkpoint, segment string) {
	t.Helper()
	stateDir, checkpoint, segment = probedRun(t)
	store, err := artifact.New(filepath.Join(stateDir, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	var session runtime.Session
	if err := store.ReadJSON(checkpoint, &session); err != nil {
		t.Fatal(err)
	}
	session.MemoryRefs[0].FromSeq, session.MemoryRefs[0].ToSeq = 1, 1
	record, err := store.Metadata(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	cp, err := store.CreateJSON("checkpoint", record.ProducerPID, session, []string{segment}, map[string]string{"kind": "intermediate"})
	if err != nil {
		t.Fatal(err)
	}
	return stateDir, cp.ID, segment
}

// Agent indexes given by segment are shown with their segments when a1 is
// on, and refused when a line lies outside its segment. Each answer
// records its memory reads with their arguments.
func TestProbesShowGivenAgentIndexes(t *testing.T) {
	stateDir, checkpoint, segment := indexedRun(t)
	probes := []memprobe.Probe{{ID: "tg", Kind: "archive", Checkpoint: checkpoint, Question: "Which threadgroup size did you keep?", Expect: `TG4`}}
	indexes := map[string][]runtime.IndexLine{segment: {{From: 1, To: 1, Text: "threadgroup sweep: TG4 kept over TG2"}}}
	budget := 6144
	run := func(recall []string, indexes map[string][]runtime.IndexLine) (*scriptedCompleter, memprobe.Sample, error) {
		t.Helper()
		completer := &scriptedCompleter{actions: []map[string]any{memoryTool("read_memory", map[string]string{"artifact_id": segment})}}
		samples, err := memprobe.Run(context.Background(), memprobe.Config{
			StateDir: stateDir, WorkDir: t.TempDir(), Workflow: probeWorkflow(), Completer: completer, Recall: recall, AgentIndexes: indexes,
			HeaderIndexBytes: budget,
		}, probes)
		if err != nil {
			return completer, memprobe.Sample{}, err
		}
		return completer, samples[0], nil
	}

	on, sample, err := run([]string{"a1"}, indexes)
	if err != nil || !strings.Contains(on.users[0], "threadgroup sweep: TG4 kept over TG2") || sample.AgentIndexSegments != 1 || sample.AgentIndexBytes == 0 {
		t.Fatalf("with a1 the header should show the given index: %+v %v\n%s", sample, err, on.users[0])
	}
	if sample.AgentIndexBudget != 6144 {
		t.Fatalf("the sample should record the header's index budget: %d", sample.AgentIndexBudget)
	}
	// A budget too small for even the "+K more" line leaves the index out.
	budget = 8
	if _, small, err := run([]string{"a1"}, indexes); err != nil || small.AgentIndexBudget != 8 || small.AgentIndexShown[segment] != 0 || small.AgentIndexSegments != 0 {
		t.Fatalf("a budget of 8 bytes should show no line: %+v %v", small, err)
	}
	budget = 6144
	if sample.AgentIndexShown[segment] != 1 || len(sample.AgentIndexShown) != 1 || sample.AgentIndexHidden[segment] != 0 || len(sample.AgentIndexHidden) != 1 {
		t.Fatalf("the sample should record the lines shown and hidden by segment: %v %v", sample.AgentIndexShown, sample.AgentIndexHidden)
	}
	if !slices.Equal(sample.Reads, []string{"read_memory artifact_id=" + segment}) {
		t.Fatalf("the read should be recorded with its arguments: %v", sample.Reads)
	}
	off, offSample, err := run(nil, indexes)
	if err != nil || strings.Contains(off.users[0], "threadgroup sweep") || offSample.AgentIndexShown != nil {
		t.Fatalf("without a1 the index should not be shown: %v\n%s", err, off.users[0])
	}
	if _, _, err := run([]string{"a1"}, map[string][]runtime.IndexLine{segment: {{From: 1, To: 5, Text: "past the segment"}}}); err == nil {
		t.Fatal("a line outside its segment should be refused")
	}
}

// a1p adds the header's note that the agent index is partial, and, when
// an index is shortened, the note on reading the rest; each sample records
// which it was shown. read_memory of the segment returns the given index,
// so the note's pointer holds in probes too.
func TestProbesNoteThatTheIndexIsPartial(t *testing.T) {
	stateDir, checkpoint, segment := indexedRun(t)
	probes := []memprobe.Probe{{ID: "tg", Kind: "archive", Checkpoint: checkpoint, Question: "Which threadgroup size did you keep?", Expect: `TG4`}}
	given := []runtime.IndexLine{{From: 1, To: 1, Text: "threadgroup sweep: TG4 kept over TG2"}, {From: 1, To: 1, Text: "TG8 never ran: out of registers, so the sweep stopped at TG4 and was not extended"}}
	run := func(recall []string, budget int) (*scriptedCompleter, memprobe.Sample) {
		t.Helper()
		completer := &scriptedCompleter{actions: []map[string]any{memoryTool("read_memory", map[string]string{"artifact_id": segment})}}
		samples, err := memprobe.Run(context.Background(), memprobe.Config{
			StateDir: stateDir, WorkDir: t.TempDir(), Workflow: probeWorkflow(), Completer: completer, Recall: recall,
			AgentIndexes: map[string][]runtime.IndexLine{segment: given}, HeaderIndexBytes: budget,
		}, probes)
		if err != nil {
			t.Fatal(err)
		}
		return completer, samples[0]
	}
	const partial, shortened = "This index is partial: a fact missing from it may still be in the archive.", "Some segments show only their first lines"

	off, sample := run([]string{"a1"}, 0)
	if strings.Contains(off.users[0], partial) || sample.AgentIndexPartialNote || sample.Condition != "no-memory,check-rule,recall=a1" {
		t.Fatalf("a1 alone should not add the note: %+v\n%s", sample, off.users[0])
	}
	on, sample := run([]string{"a1", "a1p"}, 0)
	if !strings.Contains(on.users[0], partial) || strings.Contains(on.users[0], shortened) || !sample.AgentIndexPartialNote || sample.AgentIndexShortenedNote ||
		sample.Condition != "no-memory,check-rule,recall=a1+a1p" {
		t.Fatalf("a1p should add the note, without the shortened part when nothing is hidden: %+v\n%s", sample, on.users[0])
	}
	if strings.Count(on.users[1], given[1].Text) != 2 {
		t.Fatalf("read_memory should return the given index after the header's:\n%s", on.users[1])
	}
	short, sample := run([]string{"a1", "a1p"}, 120)
	if !strings.Contains(short.users[0], shortened) || !sample.AgentIndexShortenedNote || sample.AgentIndexHidden[segment] != 1 {
		t.Fatalf("a shortened index should add the note on reading the rest: %+v\n%s", sample, short.users[0])
	}
	if !strings.Contains(short.users[1], given[1].Text) {
		t.Fatalf("read_memory should return the line the header hid:\n%s", short.users[1])
	}
	if _, err := memprobe.RecallPolicy([]string{"a1p"}); err != nil {
		t.Fatal(err)
	}
}
