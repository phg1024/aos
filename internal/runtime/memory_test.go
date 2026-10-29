package runtime_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"aos/internal/artifact"
	"aos/internal/kernel"
	"aos/internal/runtime"
	"aos/internal/tools"
)

// Reading compacted memory back must not undo the compaction: one call
// returns at most a tool output's worth, and says how to page on.
func TestReadMemoryPagesALargeSegment(t *testing.T) {
	root := t.TempDir()
	k, _ := kernel.New(filepath.Join(root, "kernel"))
	store, _ := artifact.New(filepath.Join(root, "artifacts"))
	process, _ := k.Spawn(kernel.ProcessSpec{Role: "agent:writer"})
	big := strings.Repeat("x", 9000)
	var refs []runtime.MemoryRef
	var reads []string
	backend := &funcBackend{fn: func(call int, _ context.Context, request runtime.GenerateRequest) (runtime.GenerateResult, error) {
		refs = request.MemoryRefs
		if n := len(request.History); n > 0 && strings.HasPrefix(request.History[n-1].Content, "read_memory:") {
			reads = append(reads, request.History[n-1].Content)
		}
		switch {
		case call <= 40:
			return runtime.GenerateResult{Action: runtime.GenerateAction{Type: "message", Message: big}}, nil
		case call == 41:
			return toolAction("read_memory", map[string]string{"artifact_id": refs[0].ArtifactID}), nil
		case call == 42:
			return toolAction("read_memory", map[string]string{"artifact_id": refs[0].ArtifactID, "offset": "65536"}), nil
		}
		return exitResult("done"), nil
	}}
	agent := &runtime.AgentRuntime{Kernel: k, Artifacts: store, Tools: tools.NewRuntime(), Backend: backend, MaxSteps: 50,
		Compaction: runtime.CompactionPolicy{MaxChars: 200000, KeepMessages: 4}}
	if _, err := agent.RunWithOptions(context.Background(), process.PID, nil, runtime.RunOptions{ContinueOnToolError: true}); err != nil {
		t.Fatal(err)
	}
	if len(refs) == 0 || refs[0].MessageCount < 20 {
		t.Fatalf("expected a compacted segment of many messages, refs = %+v", refs)
	}
	if len(reads) != 2 {
		t.Fatalf("expected two read_memory results, got %d", len(reads))
	}
	for i, read := range reads {
		if len(read) > tools.MaxToolOutputBytes+2048 {
			t.Fatalf("read %d put %d chars back into the history", i, len(read))
		}
	}
	if !strings.Contains(reads[0], "next_offset:65536") || !strings.Contains(reads[1], "offset:65536") {
		t.Fatalf("paging is not explained:\n%.300s\n…\n%.300s", reads[0], reads[1])
	}
}
