//go:build unix

package tools_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"aos/internal/capability"
	"aos/internal/tools"
)

// symlinkFixture: a workspace mount with links that escape it (a directory,
// a file and a dangling link pointing outside) and one that stays inside.
func symlinkFixture(t *testing.T) (ws, outside string, request func(tool string, args map[string]string) error) {
	t.Helper()
	root := t.TempDir()
	ws = filepath.Join(root, "ws")
	outside = filepath.Join(root, "outside")
	for _, dir := range []string{filepath.Join(ws, "sub"), outside} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	for path, content := range map[string]string{filepath.Join(outside, "secret.txt"): "secret", filepath.Join(ws, "sub", "a.txt"): "inside"} {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	links := map[string]string{
		"dirlink":  outside,
		"filelink": filepath.Join(outside, "secret.txt"),
		"dangling": filepath.Join(outside, "created.txt"),
		"inner":    filepath.Join(ws, "sub"),
	}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(ws, name)); err != nil {
			t.Fatalf("symlink: %v", err)
		}
	}
	runtime := tools.NewRuntime()
	request = func(tool string, args map[string]string) error {
		_, err := runtime.Execute(tools.Request{
			Tool:          tool,
			Args:          args,
			Mounts:        []capability.Mount{{Name: "ws", HostPath: ws}},
			CapabilitySet: capability.Set{ReadPaths: []string{ws}, WritePaths: []string{ws}, Tools: []string{"read_file", "write_file", "edit_file", "list_dir", "find_files"}},
		})
		return err
	}
	return ws, outside, request
}

func TestSymlinkEscapeIsDeniedForEveryFileTool(t *testing.T) {
	ws, outside, request := symlinkFixture(t)
	for name, tc := range map[string]struct {
		tool string
		args map[string]string
	}{
		"read_file via file link":         {"read_file", map[string]string{"path": filepath.Join(ws, "filelink")}},
		"read_file via dir link":          {"read_file", map[string]string{"path": filepath.Join(ws, "dirlink", "secret.txt")}},
		"write_file via dir link":         {"write_file", map[string]string{"path": filepath.Join(ws, "dirlink", "new.txt"), "content": "x"}},
		"write_file via dangling link":    {"write_file", map[string]string{"path": filepath.Join(ws, "dangling"), "content": "x"}},
		"edit_file via file link":         {"edit_file", map[string]string{"path": filepath.Join(ws, "filelink"), "old_text": "secret", "new_text": "owned"}},
		"list_dir via dir link":           {"list_dir", map[string]string{"path": filepath.Join(ws, "dirlink")}},
		"find_files rooted at a dir link": {"find_files", map[string]string{"root": filepath.Join(ws, "dirlink"), "pattern": "secret"}},
	} {
		if err := request(tc.tool, tc.args); !errors.Is(err, tools.ErrPathDenied) {
			t.Fatalf("%s: got %v, want ErrPathDenied", name, err)
		}
	}
	if content, _ := os.ReadFile(filepath.Join(outside, "secret.txt")); string(content) != "secret" {
		t.Fatalf("the outside file was modified: %q", content)
	}
	for _, path := range []string{filepath.Join(outside, "new.txt"), filepath.Join(outside, "created.txt")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s was created outside the mount", path)
		}
	}
}

func TestSymlinkInsideMountStillWorks(t *testing.T) {
	ws, _, request := symlinkFixture(t)
	if err := request("read_file", map[string]string{"path": filepath.Join(ws, "inner", "a.txt")}); err != nil {
		t.Fatalf("read through an inside link: %v", err)
	}
	if err := request("write_file", map[string]string{"path": filepath.Join(ws, "inner", "b.txt"), "content": "ok"}); err != nil {
		t.Fatalf("write through an inside link: %v", err)
	}
	if err := request("list_dir", map[string]string{"path": filepath.Join(ws, "inner")}); err != nil {
		t.Fatalf("list through an inside link: %v", err)
	}
	if content, err := os.ReadFile(filepath.Join(ws, "sub", "b.txt")); err != nil || string(content) != "ok" {
		t.Fatalf("write did not land inside the mount: %q %v", content, err)
	}
}
