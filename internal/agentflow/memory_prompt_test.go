package agentflow

import (
	"strings"
	"testing"

	"aos/internal/capability"
	"aos/internal/runtime"
)

// read_memory is always available, so the prompt must say so where tools
// are listed: an agent told to "use 'tool' only for allowed tools" would
// otherwise never call it.
func TestPromptListsReadMemoryAsAllowed(t *testing.T) {
	backend := PromptBackend{Agent: AgentSpec{ID: "writer", SystemPrompt: "Write.", Capabilities: capability.Set{Tools: []string{"read_file", "run"}}}}
	system := backend.systemPrompt()
	allowed := system[strings.Index(system, "Allowed tools"):]
	if !strings.Contains(allowed, "- read_memory(artifact_id, offset?)") {
		t.Fatalf("read_memory missing from the allowed tools:\n%s", allowed)
	}
}

// When history was compacted, the header next to the refs says how to read
// them back.
func TestTaskHeaderExplainsCompactedHistory(t *testing.T) {
	backend := PromptBackend{Agent: AgentSpec{ID: "writer", SystemPrompt: "Write."}}
	request := runtime.GenerateRequest{MemoryRefs: []runtime.MemoryRef{{ArtifactID: "art-000042", Summary: "built the BVH", MessageCount: 120}}}
	header := backend.PromptMessages(request)[1].Content
	for _, want := range []string{"art-000042", "read_memory", "120 earlier messages"} {
		if !strings.Contains(header, want) {
			t.Fatalf("header lacks %q:\n%s", want, header)
		}
	}
	plain := backend.PromptMessages(runtime.GenerateRequest{})[1].Content
	if strings.Contains(plain, "read_memory") {
		t.Fatal("without compacted history the header should not mention read_memory")
	}
}
