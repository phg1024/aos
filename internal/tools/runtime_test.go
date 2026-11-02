package tools_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"aos/internal/artifact"
	"aos/internal/capability"
	"aos/internal/tools"
)

func TestToolRuntimeHonorsCapabilities(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	runtime := tools.NewRuntime()

	req := tools.Request{
		Tool: "write_file",
		Args: map[string]string{
			"path":    filepath.Join(workspace, "note.txt"),
			"content": "hello",
		},
		Mounts: []capability.Mount{
			{Name: "workspace", HostPath: workspace},
		},
		CapabilitySet: capability.Set{
			WritePaths: []string{workspace},
			ReadPaths:  []string{workspace},
			Tools:      []string{"write_file", "read_file"},
		},
	}

	if _, err := runtime.Execute(req); err != nil {
		t.Fatalf("write file: %v", err)
	}

	readResult, err := runtime.Execute(tools.Request{
		Tool:          "read_file",
		Args:          map[string]string{"path": filepath.Join(workspace, "note.txt")},
		Mounts:        req.Mounts,
		CapabilitySet: req.CapabilitySet,
	})
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if readResult.Output["content"] != "hello" {
		t.Fatalf("unexpected read content %q", readResult.Output["content"])
	}

	listResult, err := runtime.Execute(tools.Request{
		Tool:          "list_dir",
		Args:          map[string]string{"path": workspace},
		Mounts:        req.Mounts,
		CapabilitySet: capability.Set{ReadPaths: []string{workspace}, Tools: []string{"list_dir"}},
	})
	if err != nil {
		t.Fatalf("list dir: %v", err)
	}
	if got := listResult.Output["entries"]; got != "note.txt" {
		t.Fatalf("unexpected dir listing %q", got)
	}

	findResult, err := runtime.Execute(tools.Request{
		Tool:          "find_files",
		Args:          map[string]string{"root": workspace, "pattern": "note"},
		Mounts:        req.Mounts,
		CapabilitySet: capability.Set{ReadPaths: []string{workspace}, Tools: []string{"find_files"}},
	})
	if err != nil {
		t.Fatalf("find files: %v", err)
	}
	if got := findResult.Output["matches"]; got != filepath.Join(workspace, "note.txt") {
		t.Fatalf("unexpected matches %q", got)
	}

	if _, err := runtime.Execute(tools.Request{
		Tool: "write_file",
		Args: map[string]string{
			"path":    filepath.Join(root, "outside.txt"),
			"content": "nope",
		},
		Mounts:        req.Mounts,
		CapabilitySet: req.CapabilitySet,
	}); !errors.Is(err, tools.ErrPathDenied) || tools.DenialReason(err) != tools.DeniedOutside {
		t.Fatalf("expected path denied, got %v", err)
	}

	if _, err := runtime.Execute(tools.Request{
		Tool:          "find_files",
		Args:          map[string]string{"root": workspace},
		Mounts:        req.Mounts,
		CapabilitySet: capability.Set{ReadPaths: []string{workspace}, Tools: []string{"find_files"}},
	}); err == nil {
		t.Fatal("expected missing pattern error")
	}
}

func TestExecuteAndRecordStoresStructuredArtifact(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	notePath := filepath.Join(workspace, "note.txt")
	if err := os.WriteFile(notePath, []byte("hello"), 0o644); err != nil {
		t.Fatalf("seed note: %v", err)
	}
	store, err := artifact.New(filepath.Join(root, "artifacts"))
	if err != nil {
		t.Fatalf("new artifact store: %v", err)
	}
	runtime := tools.NewRuntime()

	result, record, err := runtime.ExecuteAndRecord(store, "pid-1", tools.Request{
		Tool: "read_file",
		Args: map[string]string{"path": notePath},
		Mounts: []capability.Mount{
			{Name: "workspace", HostPath: workspace},
		},
		CapabilitySet: capability.Set{
			ReadPaths: []string{workspace},
			Tools:     []string{"read_file"},
		},
	})
	if err != nil {
		t.Fatalf("execute and record: %v", err)
	}
	if result.Tool != "read_file" {
		t.Fatalf("unexpected tool result %#v", result)
	}

	meta, err := store.Metadata(record.ID)
	if err != nil {
		t.Fatalf("artifact metadata: %v", err)
	}
	if meta.Metadata["tool"] != "read_file" {
		t.Fatalf("unexpected artifact metadata %#v", meta.Metadata)
	}
}

func TestRunTool(t *testing.T) {
	root := t.TempDir()
	runtime := tools.NewRuntime()

	// Test basic command
	result, err := runtime.Execute(tools.Request{
		Tool: "run",
		Args: map[string]string{
			"command": "echo 'hello from shell'",
		},
		Mounts: []capability.Mount{
			{Name: "workspace", HostPath: root},
		},
		CapabilitySet: capability.Set{
			Tools:      []string{"run"},
			ReadPaths:  []string{root},
			WritePaths: []string{root},
		},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Output["exit_code"] != "0" {
		t.Fatalf("unexpected exit code: %s", result.Output["exit_code"])
	}
	if !strings.Contains(result.Output["stdout"], "hello from shell") {
		t.Fatalf("unexpected stdout: %s", result.Output["stdout"])
	}

	// Test command with non-zero exit
	result, err = runtime.Execute(tools.Request{
		Tool: "run",
		Args: map[string]string{
			"command": "exit 42",
		},
		Mounts: []capability.Mount{
			{Name: "workspace", HostPath: root},
		},
		CapabilitySet: capability.Set{
			Tools:      []string{"run"},
			ReadPaths:  []string{root},
			WritePaths: []string{root},
		},
	})
	if err != nil {
		t.Fatalf("run should not error on non-zero exit: %v", err)
	}
	if result.Output["exit_code"] != "42" {
		t.Fatalf("expected exit code 42, got: %s", result.Output["exit_code"])
	}

	// Test missing command
	_, err = runtime.Execute(tools.Request{
		Tool:          "run",
		Args:          map[string]string{},
		Mounts:        []capability.Mount{{Name: "workspace", HostPath: root}},
		CapabilitySet: capability.Set{Tools: []string{"run"}, ReadPaths: []string{root}, WritePaths: []string{root}},
	})
	if err == nil {
		t.Fatal("expected error for missing command")
	}

	// Test pwd command to verify working directory
	result, err = runtime.Execute(tools.Request{
		Tool: "run",
		Args: map[string]string{
			"command": "pwd",
		},
		Mounts: []capability.Mount{
			{Name: "workspace", HostPath: root},
		},
		CapabilitySet: capability.Set{
			Tools:      []string{"run"},
			ReadPaths:  []string{root},
			WritePaths: []string{root},
		},
	})
	if err != nil {
		t.Fatalf("run pwd: %v", err)
	}
	if !strings.Contains(result.Output["stdout"], root) {
		t.Fatalf("expected stdout to contain %s, got: %s", root, result.Output["stdout"])
	}

	result, err = runtime.Execute(tools.Request{
		Tool: "run",
		Args: map[string]string{
			"command": "python3 -c \"print('hello from python')\"",
		},
		Mounts: []capability.Mount{
			{Name: "workspace", HostPath: root},
		},
		CapabilitySet: capability.Set{
			Tools:      []string{"run"},
			ReadPaths:  []string{root},
			WritePaths: []string{root},
		},
	})
	if err != nil {
		t.Fatalf("run python: %v", err)
	}
	if result.Output["exit_code"] != "0" || !strings.Contains(result.Output["stdout"], "hello from python") {
		t.Fatalf("unexpected python run result %#v", result.Output)
	}
}

func TestEditFileTool(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	target := filepath.Join(workspace, "note.txt")
	if err := os.WriteFile(target, []byte("alpha beta alpha"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	runtime := tools.NewRuntime()
	req := tools.Request{
		Tool: "edit_file",
		Args: map[string]string{
			"path":        target,
			"old_text":    "alpha",
			"new_text":    "omega",
			"replace_all": "true",
		},
		Mounts: []capability.Mount{{Name: "workspace", HostPath: workspace}},
		CapabilitySet: capability.Set{
			ReadPaths:  []string{workspace},
			WritePaths: []string{workspace},
			Tools:      []string{"edit_file"},
		},
	}
	result, err := runtime.Execute(req)
	if err != nil {
		t.Fatalf("edit file: %v", err)
	}
	if got := result.Output["occurrences"]; got != "2" {
		t.Fatalf("unexpected occurrence count %q", got)
	}
	contents, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if got := string(contents); got != "omega beta omega" {
		t.Fatalf("unexpected contents %q", got)
	}

	_, err = runtime.Execute(tools.Request{
		Tool: "edit_file",
		Args: map[string]string{
			"path":     target,
			"old_text": "missing",
			"new_text": "value",
		},
		Mounts:        req.Mounts,
		CapabilitySet: req.CapabilitySet,
	})
	if err == nil {
		t.Fatal("expected missing old_text match error")
	}
}

func TestLargeToolOutputIsCapped(t *testing.T) {
	root := t.TempDir()
	runtime := tools.NewRuntime()
	mounts := []capability.Mount{{Name: "workspace", HostPath: root}}

	small := filepath.Join(root, "small.txt")
	if err := os.WriteFile(small, []byte("hello"), 0o644); err != nil {
		t.Fatalf("write small: %v", err)
	}
	// 'é' is two bytes, so the cap falls inside a rune and must back off.
	big := filepath.Join(root, "image.ppm")
	bigContent := "P3\n" + strings.Repeat("é", tools.MaxToolOutputBytes)
	if err := os.WriteFile(big, []byte(bigContent), 0o644); err != nil {
		t.Fatalf("write big: %v", err)
	}
	set := capability.Set{ReadPaths: []string{root}, Tools: []string{"read_file", "run"}}

	result, err := runtime.Execute(tools.Request{Tool: "read_file", Args: map[string]string{"path": small}, Mounts: mounts, CapabilitySet: set})
	if err != nil {
		t.Fatalf("read small: %v", err)
	}
	if result.Output["content"] != "hello" || result.Output["truncated"] != "" {
		t.Fatalf("expected small file verbatim, got %#v", result.Output)
	}

	result, err = runtime.Execute(tools.Request{Tool: "read_file", Args: map[string]string{"path": big}, Mounts: mounts, CapabilitySet: set})
	if err != nil {
		t.Fatalf("read big: %v", err)
	}
	content := result.Output["content"]
	if result.Output["truncated"] != "true" || result.Output["size_bytes"] != strconv.Itoa(len(bigContent)) {
		t.Fatalf("expected truncation metadata, got truncated=%q size=%q", result.Output["truncated"], result.Output["size_bytes"])
	}
	if len(content) > tools.MaxToolOutputBytes+200 || !strings.Contains(content, "[truncated: showing") {
		t.Fatalf("expected capped content with notice, got %d bytes", len(content))
	}
	if !utf8.ValidString(content) {
		t.Fatal("expected truncation on a UTF-8 boundary")
	}

	result, err = runtime.Execute(tools.Request{Tool: "run", Args: map[string]string{"command": "cat image.ppm"}, Mounts: mounts, CapabilitySet: set})
	if err != nil {
		t.Fatalf("run cat: %v", err)
	}
	if len(result.Output["stdout"]) > tools.MaxToolOutputBytes+200 || !strings.Contains(result.Output["stdout"], "[truncated: showing") {
		t.Fatalf("expected capped stdout, got %d bytes", len(result.Output["stdout"]))
	}
}

func TestWriteFileRequiresContent(t *testing.T) {
	root := t.TempDir()
	runtime := tools.NewRuntime()
	mounts := []capability.Mount{{Name: "workspace", HostPath: root}}
	set := capability.Set{ReadPaths: []string{root}, WritePaths: []string{root}, Tools: []string{"write_file"}}
	target := filepath.Join(root, "main.cpp")
	if err := os.WriteFile(target, []byte("int main() { return 0; }\n"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	write := func(args map[string]string) error {
		_, err := runtime.Execute(tools.Request{Tool: "write_file", Args: args, Mounts: mounts, CapabilitySet: set})
		return err
	}

	if err := write(map[string]string{"path": target}); err == nil {
		t.Fatal("expected write_file without content to fail instead of truncating the file")
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "int main() { return 0; }\n" {
		t.Fatalf("expected the file to be untouched, got %q, %v", data, err)
	}
	if err := write(map[string]string{"path": target, "content": ""}); err != nil {
		t.Fatalf("expected an explicit empty write to succeed: %v", err)
	}
}

// A command's stdout and stderr share one cap, and its echoed command is
// capped too, so a whole run result stays within MaxToolResultBytes.
func TestRunResultFitsTheResultBound(t *testing.T) {
	root := t.TempDir()
	runtime := tools.NewRuntime()
	mounts := []capability.Mount{{Name: "workspace", HostPath: root}}
	set := capability.Set{ReadPaths: []string{root}, Tools: []string{"run"}}
	command := "head -c 70000 /dev/zero | tr '\\0' o; head -c 70000 /dev/zero | tr '\\0' e >&2 # " + strings.Repeat("c", 5000)
	result, err := runtime.Execute(tools.Request{Tool: "run", Args: map[string]string{"command": command}, Mounts: mounts, CapabilitySet: set})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	stdout, stderr := result.Output["stdout"], result.Output["stderr"]
	if !strings.Contains(stdout, "[truncated: showing") || !strings.Contains(stderr, "[truncated: showing") {
		t.Fatalf("both streams should be truncated with a notice: %d and %d bytes", len(stdout), len(stderr))
	}
	if len(stdout)+len(stderr) > tools.MaxToolOutputBytes+400 {
		t.Fatalf("stdout and stderr together are %d bytes, over the shared cap", len(stdout)+len(stderr))
	}
	if echoed := result.Output["command"]; len(echoed) > 2200 || !strings.Contains(echoed, "the full command is in your action") {
		t.Fatalf("echoed command is %d bytes", len(echoed))
	}
	if formatted := fmt.Sprintf("%s:%v", result.Tool, result.Output); len(formatted) > tools.MaxToolResultBytes {
		t.Fatalf("formatted result is %d bytes, over %d", len(formatted), tools.MaxToolResultBytes)
	}

	// A quiet stream leaves its share to the other.
	result, err = runtime.Execute(tools.Request{Tool: "run", Args: map[string]string{"command": "head -c 50000 /dev/zero | tr '\\0' o; echo short >&2"}, Mounts: mounts, CapabilitySet: set})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(result.Output["stdout"]) != 50000 {
		t.Fatalf("stdout under the shared cap should be whole, got %d bytes", len(result.Output["stdout"]))
	}
}

// A missing or blank path is an argument error naming the tool, never a
// path denial; list_dir and find_files take an empty path as the mount
// root.
func TestMissingPathIsAnArgumentError(t *testing.T) {
	workspace := t.TempDir()
	runtime := tools.NewRuntime()
	mounts := []capability.Mount{{Name: "workspace", HostPath: workspace}}
	set := capability.Set{
		ReadPaths:  []string{workspace},
		WritePaths: []string{workspace},
		Tools:      []string{"write_file", "read_file", "edit_file", "list_dir", "find_files"},
	}
	for _, tc := range []struct {
		tool string
		args map[string]string
	}{
		{"write_file", map[string]string{"content": "x"}},
		{"write_file", map[string]string{"path": "", "content": "x"}},
		{"read_file", map[string]string{}},
		{"read_file", map[string]string{"path": "  "}},
		{"edit_file", map[string]string{"old_text": "a", "new_text": "b"}},
	} {
		_, err := runtime.Execute(tools.Request{Tool: tc.tool, Args: tc.args, Mounts: mounts, CapabilitySet: set})
		if err == nil || err.Error() != tc.tool+" requires path" || errors.Is(err, tools.ErrPathDenied) || tools.DenialReason(err) != "" {
			t.Fatalf("%s %v: got %v, want an argument error", tc.tool, tc.args, err)
		}
	}
	if err := os.WriteFile(filepath.Join(workspace, "note.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	listed, err := runtime.Execute(tools.Request{Tool: "list_dir", Args: map[string]string{}, Mounts: mounts, CapabilitySet: set})
	if err != nil || !strings.Contains(listed.Output["entries"], "note.txt") {
		t.Fatalf("list_dir without a path lists the mount root: %+v %v", listed, err)
	}
	found, err := runtime.Execute(tools.Request{Tool: "find_files", Args: map[string]string{"pattern": "note"}, Mounts: mounts, CapabilitySet: set})
	if err != nil || !strings.Contains(found.Output["matches"], "note.txt") {
		t.Fatalf("find_files without a root searches the mount root: %+v %v", found, err)
	}
}
