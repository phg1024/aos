package tools_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	}); err != tools.ErrPathDenied {
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
			Tools: []string{"run"},
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
			Tools: []string{"run"},
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
		CapabilitySet: capability.Set{Tools: []string{"run"}},
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
			Tools: []string{"run"},
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
			Tools: []string{"run"},
		},
	})
	if err != nil {
		t.Fatalf("run python: %v", err)
	}
	if result.Output["exit_code"] != "0" || !strings.Contains(result.Output["stdout"], "hello from python") {
		t.Fatalf("unexpected python run result %#v", result.Output)
	}
}
