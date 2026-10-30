package agentflow

import (
	"context"
	"encoding/json"
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
// labelled as the agent's own and checked only for its numbers; newest
// first within the cap, older ones falling back to how to read theirs.
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
	if !strings.Contains(header, "(your index of this segment is over the header's cap: read_memory artifact_id=art-000101 shows it)") ||
		strings.Contains(header, "artifact_id=art-000105 shows it") || len(backend.archiveIndex(many)) > MaxAgentIndexBytes+2048 {
		t.Fatalf("over the cap, the oldest segments should fall back:\n%s", header)
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
}
