package agentflow

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"aos/internal/artifact"
	"aos/internal/capability"
	"aos/internal/kernel"
	"aos/internal/runtime"
	"aos/internal/tools"
)

// The pointer wording and the index guidance are in the prompt unless
// their switch turns them off.
func TestPromptRecallWording(t *testing.T) {
	backend := PromptBackend{Agent: AgentSpec{ID: "writer", SystemPrompt: "Write.", Capabilities: capability.Set{Tools: []string{"run"}}}}
	memory := &runtime.WorkingMemory{Version: 1, ArtifactID: "art-000001", Sections: []runtime.MemorySection{{Name: "open", Entries: []runtime.MemoryEntry{{Key: "a", Content: "b"}}}}}
	const (
		snapshotPointer = "Each entry records a conclusion and points at its evidence: when a question needs more than an entry says, such as a mechanism, a number or a condition, read its evidence with read_memory instead of filling it in."
		writeMechanism  = "Write into an entry the mechanism or condition its conclusion rests on, not only the conclusion"
		pointer         = "An entry is a pointer, not the whole record: when you need more than it says, read its evidence with read_memory rather than filling in."
		deleteStale     = "A new key replaces no other entry: when an entry no longer holds, delete it."
		indexGuidance   = "When a handoff notice asks for an index, call index_memory with one line per thing in the messages being archived that you might need to find again"
		indexTool       = "- index_memory(index) (available after a handoff notice: indexes the messages it archives)"
	)
	header := func() string {
		return backend.PromptMessages(runtime.GenerateRequest{WorkingMemory: memory})[1].Content
	}
	for _, want := range []string{writeMechanism, pointer, deleteStale, indexGuidance, indexTool} {
		if !strings.Contains(backend.systemPrompt(), want) {
			t.Fatalf("the prompt lacks %q", want)
		}
	}
	if !strings.Contains(header(), snapshotPointer) {
		t.Fatal("the snapshot should say entries point at their evidence")
	}
	backend.Recall = runtime.RecallPolicy{NoPointerPrompt: true}
	for _, gone := range []string{writeMechanism, pointer, deleteStale} {
		if strings.Contains(backend.systemPrompt(), gone) {
			t.Fatalf("NoPointerPrompt should drop %q", gone)
		}
	}
	if strings.Contains(header(), snapshotPointer) || !strings.Contains(backend.systemPrompt(), indexTool) {
		t.Fatal("NoPointerPrompt should drop only the pointer wording")
	}
	backend.Recall = runtime.RecallPolicy{NoAgentIndex: true}
	if s := backend.systemPrompt(); strings.Contains(s, "index_memory") || !strings.Contains(s, pointer) {
		t.Fatal("NoAgentIndex should drop only the index guidance and tool")
	}
}

// The header shows each segment's agent index under its runtime line,
// labelled as the agent's own and checked only for its numbers, within the
// header's budget for indexes.
func TestArchiveIndexShowsTheAgentsIndex(t *testing.T) {
	backend := PromptBackend{Agent: AgentSpec{ID: "writer", SystemPrompt: "Write."}}
	ref := func(id string, from, to int, index ...runtime.IndexLine) runtime.MemoryRef {
		return runtime.MemoryRef{ArtifactID: id, MessageCount: to - from + 1, FromSeq: from, ToSeq: to, Tools: []runtime.ToolCount{{Name: "run", Count: 3}}, Index: index}
	}
	refs := []runtime.MemoryRef{
		ref("art-000010", 1, 40, runtime.IndexLine{From: 33, To: 33, Text: "measured occupancy, max_threads_per_group=1024"}),
		ref("art-000020", 41, 80),
		ref("art-000030", 81, 120, runtime.IndexLine{From: 64, To: 69, Text: "why a different BVH breaks bit-identity (tie-breaks)"}),
	}
	header := backend.PromptMessages(runtime.GenerateRequest{MemoryRefs: refs})[1].Content
	for _, want := range []string{
		"Indented lines under a segment are your own index of it, written when it was archived. The runtime checked only that their message numbers are in the segment",
		"- messages 1-40 (art-000010): run x3\n  33: measured occupancy, max_threads_per_group=1024\n- messages 41-80 (art-000020): run x3\n- messages 81-120",
		"  64-69: why a different BVH breaks bit-identity (tie-breaks)\n",
	} {
		if !strings.Contains(header, want) {
			t.Fatalf("header lacks %q:\n%s", want, header)
		}
	}
	sizes := backend.HeaderSizes(runtime.GenerateRequest{MemoryRefs: refs})
	if sizes["agent_index_segments"] != 2 || sizes["agent_index_bytes"] == 0 || sizes["archive_index_bytes"] != len(backend.archiveIndex(refs)) {
		t.Fatalf("header sizes = %v", sizes)
	}

	long := strings.Repeat("x", 1400)
	var many []runtime.MemoryRef
	for i := range 6 {
		many = append(many, ref("art-00010"+string(rune('0'+i)), i*10+1, i*10+10, runtime.IndexLine{From: i*10 + 1, To: i*10 + 1, Text: long}))
	}
	header = backend.PromptMessages(runtime.GenerateRequest{MemoryRefs: many})[1].Content
	if !strings.Contains(header, "  1: "+long) || !strings.Contains(header, "(+1 more lines of this index: read_memory artifact_id=art-000105)") ||
		len(backend.archiveIndex(many)) > MaxAgentIndexBytes+2048 {
		t.Fatalf("over the cap, the oldest segments should keep their lines and the newest say how to read theirs:\n%s", header)
	}

	backend.Recall = runtime.RecallPolicy{NoAgentIndex: true}
	if header := backend.PromptMessages(runtime.GenerateRequest{MemoryRefs: refs})[1].Content; strings.Contains(header, "max_threads_per_group") || strings.Contains(header, "your own index") {
		t.Fatal("NoAgentIndex should not show the agent's index")
	}
}

// promptRecorder replies with scripted actions and records system prompts.
type promptRecorder struct {
	actions []map[string]any
	systems []string
}

func (p *promptRecorder) CompleteJSON(_ context.Context, system, _ string, target any) error {
	p.systems = append(p.systems, system)
	action := map[string]any{"type": "exit", "exit_status": map[string]any{"code": 0, "reason": "done"}}
	if len(p.actions) > 0 {
		action, p.actions = p.actions[0], p.actions[1:]
	}
	bytes, _ := json.Marshal(map[string]any{"action": action})
	return json.Unmarshal(bytes, target)
}

// A workflow spec's recall policy reaches both the prompt and the runtime
// of every agent.
func TestWorkflowRecallReachesPromptAndRuntime(t *testing.T) {
	run := func(t *testing.T, recall runtime.RecallPolicy) (*promptRecorder, []tools.Result) {
		root := t.TempDir()
		k, err := kernel.New(filepath.Join(root, "kernel"))
		if err != nil {
			t.Fatal(err)
		}
		store, err := artifact.New(filepath.Join(root, "artifacts"))
		if err != nil {
			t.Fatal(err)
		}
		set := func(content string) map[string]any {
			return map[string]any{"type": "tool", "tool_call": map[string]any{"name": "update_memory", "args": map[string]string{"section": "open", "key": "plan", "content": content}}}
		}
		recorder := &promptRecorder{actions: []map[string]any{set("draft at 30.9 ms"), set("draft")}}
		engine := Engine{Kernel: k, Artifacts: store, Tools: tools.NewRuntime(), Factory: StaticBackendFactory{Completer: recorder}}
		spec := WorkflowSpec{Name: "recall", EntryAgent: "writer", Recall: recall,
			Agents: []AgentSpec{{ID: "writer", SystemPrompt: "Write.", Capabilities: capability.Set{Tools: []string{"run"}}}}}
		if _, err := engine.Run(context.Background(), spec, RunRequest{Goal: "plan"}); err != nil {
			t.Fatal(err)
		}
		records, _ := store.List()
		results := make([]tools.Result, 2)
		for _, record := range records {
			if record.Type == "tool_result" {
				var result tools.Result
				store.ReadJSON(record.ID, &result)
				if version, _ := strconv.Atoi(result.Output["version"]); version >= 1 && version <= 2 {
					results[version-1] = result
				}
			}
		}
		return recorder, results
	}
	recorder, results := run(t, runtime.RecallPolicy{})
	if !strings.Contains(recorder.systems[0], "index_memory") || len(results) != 2 || results[1].Output["notice"] == "" {
		t.Fatalf("with the aids on, the prompt and the runtime should have them: %+v", results)
	}
	recorder, results = run(t, runtime.RecallPolicy{NoAgentIndex: true, NoOverwriteWarning: true})
	if strings.Contains(recorder.systems[0], "index_memory") || len(results) != 2 || results[1].Output["notice"] != "" {
		t.Fatalf("the spec's switches should reach the prompt and the runtime: %+v", results)
	}

	var spec WorkflowSpec
	if err := json.Unmarshal([]byte(`{"recall":{"no_agent_index":true,"no_pointer_prompt":true}}`), &spec); err != nil || spec.Recall != (runtime.RecallPolicy{NoAgentIndex: true, NoPointerPrompt: true}) {
		t.Fatalf("recall should decode from a workflow file: %+v %v", spec.Recall, err)
	}
	spec = WorkflowSpec{}
	if err := json.Unmarshal([]byte(`{"recall":{"no_partial_index_note":true}}`), &spec); err != nil || spec.Recall != (runtime.RecallPolicy{NoPartialIndexNote: true}) {
		t.Fatalf("no_partial_index_note should decode from a workflow file: %+v %v", spec.Recall, err)
	}
}

// indexRef is a segment whose agent index has lines lines of about width
// bytes each.
func indexRef(id string, from, lines, width int) runtime.MemoryRef {
	ref := runtime.MemoryRef{ArtifactID: id, MessageCount: lines, FromSeq: from, ToSeq: from + lines - 1}
	for n := range lines {
		text := fmt.Sprintf("%s line %d ", id, n)
		ref.Index = append(ref.Index, runtime.IndexLine{From: from + n, To: from + n, Text: text + strings.Repeat("t", max(width-len(text)-8, 1))})
	}
	return ref
}

// shownTotal is the bytes of agent index the header shows.
func shownTotal(shown []string) int {
	total := 0
	for _, text := range shown {
		total += len(text)
	}
	return total
}

// The header shares its budget for agent indexes by whole lines, every
// segment's next line before any segment's line after it, oldest first:
// the round-2 shape that hid art-000414's index now shows its first
// lines; a short index leaves its share to the others; a shortened index
// says how many lines it hides; the total never exceeds the budget, which
// RecallPolicy.HeaderIndexBytes sets; and the same refs give the same
// header.
func TestHeaderSharesTheIndexBudget(t *testing.T) {
	backend := PromptBackend{Agent: AgentSpec{ID: "writer", SystemPrompt: "Write."}}

	// Round 2: four indexes, about 8 KB, the oldest holding the truth.
	round2 := []runtime.MemoryRef{
		indexRef("art-000414", 1, 24, 100),
		indexRef("art-000503", 60, 20, 100),
		indexRef("art-000560", 120, 20, 100),
		indexRef("art-000611", 180, 16, 100),
	}
	header := backend.archiveIndex(round2)
	shown, counts := backend.shownAgentIndexes(round2)
	for n := range 5 {
		if !strings.Contains(header, round2[0].Index[n].String()) {
			t.Fatalf("the oldest segment's line %d should be shown:\n%s", n, header)
		}
	}
	if total := shownTotal(shown); total > MaxAgentIndexBytes {
		t.Fatalf("shown %d bytes, over the %d budget", total, MaxAgentIndexBytes)
	}
	for i, count := range counts {
		if count.Shown == 0 || count.Shown+count.Hidden != len(round2[i].Index) {
			t.Fatalf("segment %s: %+v", count.ArtifactID, count)
		}
		if more := fmt.Sprintf("(+%d more lines of this index: read_memory artifact_id=%s)", count.Hidden, count.ArtifactID); strings.Contains(header, more) != (count.Hidden > 0) ||
			strings.Count(shown[i], "\n") != count.Shown+min(count.Hidden, 1) {
			t.Fatalf("segment %s should end with %q: %+v\n%s", count.ArtifactID, more, count, shown[i])
		}
		if count.Shown < counts[len(counts)-1].Shown-1 {
			t.Fatalf("lines should be shared round by round: %+v", counts)
		}
	}
	if again := backend.archiveIndex(append([]runtime.MemoryRef(nil), round2...)); again != header {
		t.Fatal("the same refs should give the same header")
	}
	t.Logf("round 2: %+v, %d of %d bytes", counts, shownTotal(shown), MaxAgentIndexBytes)

	// A short index leaves its share to a long one.
	short := []runtime.MemoryRef{indexRef("art-000001", 1, 2, 80), indexRef("art-000002", 10, 80, 80)}
	shown, counts = backend.shownAgentIndexes(short)
	if counts[0].Hidden != 0 || len(shown[1]) <= MaxAgentIndexBytes/2 || shownTotal(shown) > MaxAgentIndexBytes {
		t.Fatalf("the short index's unused share should go to the long one: %+v, %d bytes", counts, len(shown[1]))
	}

	// Many segments: each still shows its first line, within the budget.
	var many []runtime.MemoryRef
	for i := range 30 {
		many = append(many, indexRef(fmt.Sprintf("art-%06d", 100+i), i*20+1, 10, 90))
	}
	shown, counts = backend.shownAgentIndexes(many)
	for _, count := range counts {
		if count.Shown == 0 {
			t.Fatalf("every segment should show its first line: %+v", counts)
		}
	}
	if shownTotal(shown) > MaxAgentIndexBytes {
		t.Fatalf("over the budget: %d", shownTotal(shown))
	}

	// The budget is the policy's, and holds even when it cannot fit every
	// segment's "+K more" line: then the newest show nothing, as reported.
	for _, budget := range []int{1, 100, 700, 2048, 12 * 1024} {
		backend.Recall = runtime.RecallPolicy{HeaderIndexBytes: budget}
		for _, refs := range [][]runtime.MemoryRef{round2, short, many} {
			shown, counts = backend.shownAgentIndexes(refs)
			if shownTotal(shown) > budget {
				t.Fatalf("budget %d: shown %d bytes", budget, shownTotal(shown))
			}
			for i, count := range counts {
				if i > 0 && count.Shown > 0 && counts[0].Shown == 0 {
					t.Fatalf("budget %d: a newer segment shows lines while the oldest shows none: %+v", budget, counts)
				}
			}
		}
	}
	backend.Recall = runtime.RecallPolicy{HeaderIndexBytes: 700}
	if _, counts = backend.shownAgentIndexes(round2); counts[3].Shown != 1 {
		t.Fatalf("700 bytes should still show every segment's first line: %+v", counts)
	}
	backend.Recall = runtime.RecallPolicy{HeaderIndexBytes: 250}
	shown, counts = backend.shownAgentIndexes(round2)
	if !strings.Contains(shown[0], "(+24 more lines") || shown[3] != "" || counts[3].Hidden != 16 {
		t.Fatalf("with 250 bytes the newest should show nothing and be reported hidden: %q %+v", shown, counts)
	}

	backend.Recall = runtime.RecallPolicy{}
	sizes := backend.HeaderSizes(runtime.GenerateRequest{MemoryRefs: round2})
	_, counts = backend.shownAgentIndexes(round2)
	lines := 0
	for _, count := range counts {
		if sizes["agent_index_shown."+count.ArtifactID] != count.Shown || sizes["agent_index_hidden."+count.ArtifactID] != count.Hidden {
			t.Fatalf("the trace should say what each segment showed: %v %+v", sizes, count)
		}
		lines += count.Shown
	}
	if sizes["agent_index_lines_shown"] != lines || sizes["agent_index_budget"] != MaxAgentIndexBytes || sizes["agent_index_segments"] != 4 {
		t.Fatalf("header sizes = %v", sizes)
	}
}

// The header says the agent index is partial whenever it shows any of it,
// and, when a segment shows only its first lines, how to read the rest.
// With the note off, the header is 6c55bdb's byte for byte
// (testdata/header-6c55bdb, written by that commit's code).
func TestHeaderSaysTheIndexIsPartial(t *testing.T) {
	const (
		intro     = "they are a map, not the record: read the messages they cite before relying on details."
		partial   = " This index is partial: a fact missing from it may still be in the archive. Before you answer that something was never done, measured or seen, search_memory for it."
		shortened = " Some segments show only their first lines; \"(+K more lines ...)\" marks the rest, and read_memory artifact_id=<segment> returns the whole index."
	)
	golden := func(name string) string {
		bytes, err := os.ReadFile(filepath.Join("testdata", "header-6c55bdb", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(bytes)
	}
	backend := PromptBackend{Agent: AgentSpec{ID: "writer", SystemPrompt: "Write."}}
	refs := []runtime.MemoryRef{indexRef("art-000101", 1, 2, 40), indexRef("art-000102", 10, 3, 40)}
	cases := []struct {
		name   string
		budget int
		note   string
	}{
		{"whole.txt", 0, partial},
		{"shortened.txt", 220, partial + shortened},
	}
	for _, c := range cases {
		backend.Recall = runtime.RecallPolicy{HeaderIndexBytes: c.budget}
		before := golden(c.name)
		if got, want := backend.archiveIndex(refs), strings.Replace(before, intro+"\n", intro+c.note+"\n", 1); got != want || !strings.Contains(before, intro+"\n") {
			t.Fatalf("%s: header with the note =\n%s\nwant\n%s", c.name, got, want)
		}
		sizes := backend.HeaderSizes(runtime.GenerateRequest{MemoryRefs: refs})
		if sizes["agent_index_partial_note"] != 1 || sizes["agent_index_shortened_note"] != boolInt(c.budget > 0) {
			t.Fatalf("%s: the trace should say which notes were shown: %v", c.name, sizes)
		}
		backend.Recall.NoPartialIndexNote = true
		if got := backend.archiveIndex(refs); got != before {
			t.Fatalf("%s: with the note off the header should be 6c55bdb's:\n%s", c.name, got)
		}
		if sizes := backend.HeaderSizes(runtime.GenerateRequest{MemoryRefs: refs}); sizes["agent_index_partial_note"] != 0 || sizes["agent_index_shortened_note"] != 0 {
			t.Fatalf("%s: the trace should say no note was shown: %v", c.name, sizes)
		}
	}

	// No note without an agent index to qualify; none with the index off.
	backend.Recall = runtime.RecallPolicy{}
	bare := []runtime.MemoryRef{{ArtifactID: "art-000103", MessageCount: 4, FromSeq: 1, ToSeq: 4}}
	if header := backend.archiveIndex(bare); strings.Contains(header, "partial") || strings.Contains(header, "first lines") {
		t.Fatalf("a header with no agent index should have no note:\n%s", header)
	}
	backend.Recall = runtime.RecallPolicy{NoAgentIndex: true}
	if header := backend.archiveIndex(refs); strings.Contains(header, "partial") || strings.Contains(header, "first lines") {
		t.Fatalf("NoAgentIndex should drop the note:\n%s", header)
	}
}
