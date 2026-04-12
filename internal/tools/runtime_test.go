package tools_test

import (
	"path/filepath"
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
	store, err := artifact.New(filepath.Join(root, "artifacts"))
	if err != nil {
		t.Fatalf("new artifact store: %v", err)
	}
	runtime := tools.NewRuntime()

	result, record, err := runtime.ExecuteAndRecord(store, "pid-1", tools.Request{
		Tool: "echo",
		Args: map[string]string{"message": "hello"},
		Mounts: []capability.Mount{
			{Name: "workspace", HostPath: workspace},
		},
		CapabilitySet: capability.Set{
			Tools: []string{"echo"},
		},
	})
	if err != nil {
		t.Fatalf("execute and record: %v", err)
	}
	if result.Tool != "echo" {
		t.Fatalf("unexpected tool result %#v", result)
	}

	meta, err := store.Metadata(record.ID)
	if err != nil {
		t.Fatalf("artifact metadata: %v", err)
	}
	if meta.Metadata["tool"] != "echo" {
		t.Fatalf("unexpected artifact metadata %#v", meta.Metadata)
	}
}
