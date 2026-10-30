package agentflow

import (
	"os"
	"path/filepath"
	"testing"

	"aos/internal/capability"
	"aos/internal/runtime"
)

// goldenRequest exercises every part of the prompt the recall aids touch:
// the system prompt, the working-memory snapshot and the archive index,
// in both the transcript and the single-prompt forms.
func goldenRequest() (PromptBackend, runtime.GenerateRequest) {
	backend := PromptBackend{Workflow: WorkflowSpec{Name: "golden"}, Agent: AgentSpec{ID: "writer", SystemPrompt: "Write.", Capabilities: capability.Set{Tools: []string{"read_file", "run"}}}}
	memory := &runtime.WorkingMemory{Version: 3, ArtifactID: "art-000099", Sections: []runtime.MemorySection{
		{Name: "decisions", Entries: []runtime.MemoryEntry{{Key: "leaf2", Content: "rejected: biased build", Evidence: "236-243"}}},
	}}
	request := runtime.GenerateRequest{
		WorkingMemory: memory,
		MemoryRefs: []runtime.MemoryRef{
			{ArtifactID: "art-000042", MessageCount: 120, FromSeq: 1, ToSeq: 120, Archived: "2026-10-29T04:05:00Z",
				Tools: []runtime.ToolCount{{Name: "run", Count: 40}}, Paths: []string{"/work/pt/src/bvh.cpp"}},
		},
		History: []runtime.Message{{Role: "user", Content: "goal", Seq: 121}, {Role: "notice", Content: "runtime notice: x", Seq: 122}},
	}
	return backend, request
}

func goldenPrompts(backend PromptBackend, request runtime.GenerateRequest) map[string]string {
	header := backend.PromptMessages(request)[1].Content
	return map[string]string{
		"system.txt": backend.systemPrompt(),
		"header.txt": header,
		"user.txt":   backend.userPrompt(request),
	}
}

// With every recall aid off, the prompts are byte for byte what they were
// before the aids (testdata/prompts-before-recall, written by this test at
// ccb6e8b with AOS_WRITE_GOLDEN set), so frozen probe arms stay valid.
func TestRecallOffKeepsThePromptsAsBefore(t *testing.T) {
	backend, request := goldenRequest()
	backend.Recall = runtime.RecallOff()
	dir := filepath.Join("testdata", "prompts-before-recall")
	for name, got := range goldenPrompts(backend, request) {
		path := filepath.Join(dir, name)
		if os.Getenv("AOS_WRITE_GOLDEN") != "" {
			os.MkdirAll(dir, 0o755)
			if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if got != string(want) {
			t.Fatalf("%s differs from before the recall aids:\n--- got\n%s\n--- want\n%s", name, got, want)
		}
	}
}
