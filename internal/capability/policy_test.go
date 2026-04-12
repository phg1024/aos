package capability_test

import (
	"path/filepath"
	"testing"

	"aos/internal/capability"
)

func TestEvaluatorReadWriteAndToolScopes(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")

	evaluator := capability.Evaluator{}
	mounts := []capability.Mount{
		{Name: "workspace", HostPath: workspace},
	}
	set := capability.Set{
		ReadPaths:  []string{workspace},
		WritePaths: []string{workspace},
		Tools:      []string{"read_file", "write_file"},
	}

	if !evaluator.CanRead(filepath.Join(workspace, "a.txt"), mounts, set) {
		t.Fatal("expected read to be allowed")
	}
	if !evaluator.CanWrite(filepath.Join(workspace, "b.txt"), mounts, set) {
		t.Fatal("expected write to be allowed")
	}
	if evaluator.CanWrite(filepath.Join(root, "outside.txt"), mounts, set) {
		t.Fatal("expected outside write to be denied")
	}
	if evaluator.ToolAllowed("echo", set) {
		t.Fatal("echo should not be allowed")
	}
}

func TestDeriveChildSetRestrictsCapabilities(t *testing.T) {
	parent := capability.Set{
		ReadPaths:    []string{"/workspace", "/shared"},
		WritePaths:   []string{"/workspace"},
		Tools:        []string{"read_file", "write_file", "echo"},
		AllowNetwork: true,
	}
	requested := capability.Set{
		ReadPaths:    []string{"/workspace"},
		WritePaths:   []string{"/workspace", "/other"},
		Tools:        []string{"read_file", "echo"},
		AllowNetwork: false,
	}

	derived := capability.DeriveChildSet(parent, requested)
	if len(derived.ReadPaths) != 1 || derived.ReadPaths[0] != "/workspace" {
		t.Fatalf("unexpected derived read paths %#v", derived.ReadPaths)
	}
	if len(derived.WritePaths) != 1 || derived.WritePaths[0] != "/workspace" {
		t.Fatalf("unexpected derived write paths %#v", derived.WritePaths)
	}
	if len(derived.Tools) != 2 {
		t.Fatalf("unexpected derived tools %#v", derived.Tools)
	}
	if derived.AllowNetwork {
		t.Fatal("expected child network access to be narrowed")
	}
}
