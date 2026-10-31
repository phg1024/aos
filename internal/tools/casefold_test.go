package tools_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aos/internal/capability"
	"aos/internal/tools"
)

// skipUnlessCaseInsensitive skips a test unless dir's volume opens a path
// in any case, as macOS's default volume does.
func skipUnlessCaseInsensitive(t *testing.T, dir string) {
	t.Helper()
	probe := filepath.Join(dir, "case-probe")
	if err := os.WriteFile(probe, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(probe)
	if _, err := os.Stat(filepath.Join(dir, "CASE-PROBE")); err != nil {
		t.Skip("the temporary volume is case-sensitive")
	}
}

// On a case-insensitive volume a path in another case names the same
// file, so it gets the same answer: every file tool refuses the protected
// state directory and writes into the read-only mount nested in the
// writable one however they are spelled, as it refuses them in lower
// case, and a case variant inside the scopes still works.
func TestCaseVariantsGetTheSameAnswer(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	skipUnlessCaseInsensitive(t, root)
	ws := filepath.Join(root, "ws")
	state, ref := filepath.Join(ws, ".aos-state"), filepath.Join(ws, "ref")
	events := filepath.Join(state, "kernel", "events.json")
	for _, dir := range []string{filepath.Join(state, "kernel"), ref, filepath.Join(ws, "sub")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(events, []byte(`[{"id":"evt-000001"}]`), 0o644)
	os.WriteFile(filepath.Join(ref, "reference.txt"), []byte("reference image"), 0o644)
	runtime := tools.NewRuntime()
	runtime.Protected = []string{state}
	request := func(tool string, args map[string]string) tools.Request {
		return tools.Request{Tool: tool, Args: args,
			Mounts:        []capability.Mount{{Name: "workspace", HostPath: ws}, {Name: "reference", HostPath: ref, ReadOnly: true}},
			CapabilitySet: capability.Set{Tools: []string{tool}, ReadPaths: []string{ws}, WritePaths: []string{ws}}}
	}

	for _, spelling := range []string{".aos-state", ".AOS-STATE", ".Aos-State"} {
		dir := filepath.Join(ws, spelling)
		file := filepath.Join(dir, "kernel", "events.json")
		attempts := map[string]tools.Request{
			"write a new file":  request("write_file", map[string]string{"path": filepath.Join(dir, "probe.txt"), "content": "x"}),
			"overwrite events":  request("write_file", map[string]string{"path": file, "content": `[{"id":"evt-999999"}]`}),
			"append to events":  request("edit_file", map[string]string{"path": file, "old_text": "}]", "new_text": `},{"id":"evt-999999"}]`}),
			"read events":       request("read_file", map[string]string{"path": file}),
			"list the state":    request("list_dir", map[string]string{"path": dir}),
			"find in the state": request("find_files", map[string]string{"root": dir, "pattern": "events"}),
		}
		for name, attempt := range attempts {
			result, err := runtime.Execute(attempt)
			if !errors.Is(err, tools.ErrPathDenied) || tools.DenialReason(err) != tools.DeniedProtected {
				t.Errorf("%s: %s via %s = %+v, %v; want refused as protected", spelling, name, attempt.Tool, result.Output, err)
			}
		}
	}
	// A search from the workspace skips the state directory too.
	found, err := runtime.Execute(request("find_files", map[string]string{"root": ws, "pattern": "events"}))
	if err != nil || found.Output["matches"] != "" {
		t.Errorf("find_files from the workspace = %+v, %v", found.Output, err)
	}

	for _, spelling := range []string{"ref", "REF", "Ref"} {
		dir := filepath.Join(ws, spelling)
		attempts := map[string]tools.Request{
			"write a new file":    request("write_file", map[string]string{"path": filepath.Join(dir, "probe-ro.txt"), "content": "x"}),
			"overwrite reference": request("write_file", map[string]string{"path": filepath.Join(dir, "reference.txt"), "content": "tampered"}),
			"edit reference":      request("edit_file", map[string]string{"path": filepath.Join(dir, "reference.txt"), "old_text": "image", "new_text": "tampered"}),
		}
		for name, attempt := range attempts {
			result, err := runtime.Execute(attempt)
			if !errors.Is(err, tools.ErrPathDenied) || tools.DenialReason(err) != tools.DeniedReadOnly {
				t.Errorf("%s: %s via %s = %+v, %v; want refused as read-only", spelling, name, attempt.Tool, result.Output, err)
			}
		}
		// Reading the read-only mount is granted in any case.
		if read, err := runtime.Execute(request("read_file", map[string]string{"path": filepath.Join(dir, "reference.txt")})); err != nil || read.Output["content"] != "reference image" {
			t.Errorf("%s: read the reference = %+v, %v", spelling, read.Output, err)
		}
	}

	if raw, _ := os.ReadFile(events); string(raw) != `[{"id":"evt-000001"}]` {
		t.Errorf("events changed to %s", raw)
	}
	if raw, _ := os.ReadFile(filepath.Join(ref, "reference.txt")); string(raw) != "reference image" {
		t.Errorf("reference changed to %q", raw)
	}
	entries, _ := os.ReadDir(state)
	refEntries, _ := os.ReadDir(ref)
	if len(entries) != 1 || len(refEntries) != 1 {
		t.Errorf("files were created: state %v, ref %v", entries, refEntries)
	}

	// Inside the scopes a case variant works as before.
	if _, err := runtime.Execute(request("write_file", map[string]string{"path": filepath.Join(ws, "SUB", "note.txt"), "content": "ok"})); err != nil {
		t.Fatalf("write in scope, in another case: %v", err)
	}
	if raw, _ := os.ReadFile(filepath.Join(ws, "sub", "note.txt")); string(raw) != "ok" {
		t.Fatalf("note = %q", raw)
	}
	if listed, err := runtime.Execute(request("list_dir", map[string]string{"path": filepath.Join(strings.ToUpper(root[:2])+root[2:], "ws")})); err != nil || !strings.Contains(listed.Output["entries"], "sub/") {
		t.Fatalf("list the workspace = %+v, %v", listed.Output, err)
	}
}
