package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aos/internal/capability"
)

// A run records what it did to the workspace mount's files, including a
// write no command text shows (a heredoc script, as in Terminal-Bench seq
// 27), and the result the agent sees is unchanged by it.
func TestRunRecordsFileChanges(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	ws := filepath.Join(root, "ws")
	os.MkdirAll(filepath.Join(ws, "old"), 0o755)
	os.WriteFile(filepath.Join(ws, "keep.txt"), []byte("same"), 0o644)
	os.WriteFile(filepath.Join(ws, "edit.txt"), []byte("before"), 0o644)
	os.WriteFile(filepath.Join(ws, "old", "gone.txt"), []byte("x"), 0o644)
	runtime := NewRuntime()
	request := Request{Tool: "run", Args: map[string]string{"command": "sh -s <<'EOF'\nprintf '{}' > results.json\nprintf 'after!' > edit.txt\nrm -r old\nmkdir build && touch build/cmain\nEOF"},
		Mounts:        []capability.Mount{{Name: WorkspaceMount, HostPath: ws}, {Name: "root", HostPath: "/"}},
		CapabilitySet: capability.Set{Tools: []string{"run"}, ReadPaths: []string{"/"}, WritePaths: []string{"/"}, UnconfinedRun: true}}
	result, err := runtime.Execute(request)
	if err != nil || result.Output["exit_code"] != "0" {
		t.Fatalf("run = %+v, %v", result, err)
	}
	changes := result.Changes
	if changes == nil || changes.Root != ws {
		t.Fatalf("changes = %+v", changes)
	}
	if got := strings.Join(changes.Created, ","); got != "build,build/cmain,results.json" {
		t.Fatalf("created %s", got)
	}
	if got := strings.Join(changes.Modified, ","); got != "edit.txt" {
		t.Fatalf("modified %s", got)
	}
	if got := strings.Join(changes.Deleted, ","); got != "old,old/gone.txt" {
		t.Fatalf("deleted %s", got)
	}
	for _, value := range result.Output {
		if strings.Contains(value, "results.json") && !strings.Contains(value, "printf") {
			t.Fatalf("the changes should not be in the output the agent sees: %v", result.Output)
		}
	}

	// A run without a workspace mount records nothing.
	request.Mounts = []capability.Mount{{Name: "root", HostPath: "/"}}
	request.Args = map[string]string{"command": "true"}
	if result, err := runtime.Execute(request); err != nil || result.Changes != nil {
		t.Fatalf("without a workspace mount: %+v, %v", result.Changes, err)
	}
}

// Changes are capped, and a tree too large to walk is reported as unknown.
func TestFileChangesAreBounded(t *testing.T) {
	before := map[string]fileState{}
	after := map[string]fileState{}
	for i := 0; i < MaxListedChanges+5; i++ {
		after[fmt.Sprintf("f%03d", i)] = fileState{size: 1}
	}
	changes := compareSnapshots("/ws", before, after)
	if len(changes.Created) != MaxListedChanges || changes.More != 5 {
		t.Fatalf("listed %d, more %d", len(changes.Created), changes.More)
	}
	dir := t.TempDir()
	for i := 0; i < 5; i++ {
		os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%d", i)), nil, 0o644)
	}
	if _, complete := snapshot(dir, 3); complete {
		t.Fatal("a tree over the limit should not count as walked")
	}
	if states, complete := snapshot(dir, 10); !complete || len(states) != 5 {
		t.Fatalf("snapshot = %d entries, complete %v", len(states), complete)
	}
}
