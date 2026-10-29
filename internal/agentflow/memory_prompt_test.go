package agentflow

import (
	"strings"
	"testing"

	"aos/internal/capability"
	"aos/internal/runtime"
)

// search_memory and read_memory are always available, so the prompt must
// say so where tools are listed: an agent told to "use 'tool' only for
// allowed tools" would otherwise never call them.
func TestPromptListsMemoryToolsAsAllowed(t *testing.T) {
	backend := PromptBackend{Agent: AgentSpec{ID: "writer", SystemPrompt: "Write.", Capabilities: capability.Set{Tools: []string{"read_file", "run"}}}}
	system := backend.systemPrompt()
	allowed := system[strings.Index(system, "Allowed tools"):]
	for _, want := range []string{"- search_memory(query, limit?)", "- read_memory(from, to? | artifact_id, offset?)"} {
		if !strings.Contains(allowed, want) {
			t.Fatalf("%s missing from the allowed tools:\n%s", want, allowed)
		}
	}
}

// When history was compacted, the header indexes the archive: message
// numbers, tools and paths per segment, what the index leaves out, and how
// to search and read it. It carries no summary, even from an old ref.
func TestTaskHeaderIndexesTheArchive(t *testing.T) {
	backend := PromptBackend{Agent: AgentSpec{ID: "writer", SystemPrompt: "Write."}}
	request := runtime.GenerateRequest{MemoryRefs: []runtime.MemoryRef{
		{ArtifactID: "art-000042", MessageCount: 120, FromSeq: 1, ToSeq: 120, Archived: "2026-10-29T04:05:00Z", Summary: "built the BVH",
			Tools: []runtime.ToolCount{{Name: "run", Count: 40}, {Name: "read_file", Count: 3}},
			Paths: []string{"/work/pt/src/bvh.cpp", "/work/pt/shaders/bvh.metal"}, MorePaths: 2},
		{ArtifactID: "art-000077", MessageCount: 30, FromSeq: 121, ToSeq: 150, Archived: "2026-10-29T05:00:00Z",
			Paths: []string{"/work/pt/notes.txt"}},
	}}
	header := backend.PromptMessages(request)[1].Content
	for _, want := range []string{
		"Archive: your messages 1-150 are in 2 segments",
		"search_memory", "read_memory from=N to=M",
		"not a summary", "files that run commands touched are not listed", "paths are under /work/pt/",
		"- messages 1-120 (art-000042, archived 2026-10-29 04:05:00): run x40, read_file x3; paths src/bvh.cpp, shaders/bvh.metal +2 more",
		"- messages 121-150 (art-000077, archived 2026-10-29 05:00:00); paths notes.txt",
	} {
		if !strings.Contains(header, want) {
			t.Fatalf("header lacks %q:\n%s", want, header)
		}
	}
	if strings.Contains(header, "built the BVH") || strings.Contains(header, "memory_refs") {
		t.Fatalf("the header should index the archive, not summarize it:\n%s", header)
	}
	plain := backend.PromptMessages(runtime.GenerateRequest{})[1].Content
	if strings.Contains(plain, "search_memory") || strings.Contains(plain, "Archive:") {
		t.Fatal("without compacted history the header should not mention the archive")
	}
}
