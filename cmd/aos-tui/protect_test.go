package main

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"aos/internal/agentflow"
	"aos/internal/statedir"
	"aos/internal/tools"
)

// tuiLayout is scripts/tui.sh's layout: the workspace is the checkout,
// mounted writable, and each session's state directory is in
// .aos-sessions inside it, with an earlier session /resume can load.
type tuiLayout struct {
	root, sessions, current, earlier string
	runtime                          *tools.Runtime
}

func newTUILayout(t *testing.T) tuiLayout {
	t.Helper()
	root, _ := filepath.EvalSymlinks(t.TempDir())
	l := tuiLayout{root: root, sessions: filepath.Join(root, ".aos-sessions")}
	l.current, l.earlier = filepath.Join(l.sessions, "20261031-100000-strata"), filepath.Join(l.sessions, "20261030-090000-strata")
	makeSession(t, l.earlier, "hello")
	if err := os.MkdirAll(l.current, 0o755); err != nil {
		t.Fatal(err)
	}
	l.runtime = tools.NewRuntime()
	l.runtime.Protected = protectedPaths(l.current, l.sessions, root)
	l.runtime.ScratchRoot = filepath.Join(t.TempDir(), "scratch")
	return l
}

func (l tuiLayout) request(tool string, args map[string]string) tools.Request {
	agent := agentflow.AgentSpec{}
	set := agentCapabilities(agent, l.root)
	set.Tools = []string{tool}
	return tools.Request{Tool: tool, Args: args, PID: "pid-000001", Mounts: agentMounts(agent, l.root), CapabilitySet: set}
}

// Under scripts/tui.sh's layout no file tool reaches an earlier session,
// or one made after this one started, while the rest of the workspace
// stays writable.
func TestEarlierSessionsAreProtected(t *testing.T) {
	l := newTUILayout(t)
	if !slices.Contains(l.runtime.Protected, l.sessions) {
		t.Fatalf("the sessions directory should be protected: %v", l.runtime.Protected)
	}
	later := filepath.Join(l.sessions, "20261031-110000-strata")
	for _, path := range []string{
		filepath.Join(l.earlier, "kernel", "probe-sibling.txt"),
		filepath.Join(l.earlier, "kernel", "processes.json"),
		filepath.Join(later, "kernel", "events.json"),
		filepath.Join(l.current, "probe-current.txt"),
		statedir.LockPath(l.earlier),
	} {
		_, err := l.runtime.Execute(l.request("write_file", map[string]string{"path": path, "content": "{}"}))
		if !errors.Is(err, tools.ErrPathDenied) || tools.DenialReason(err) != tools.DeniedProtected {
			t.Errorf("write_file %s: %v, want refused as protected", path, err)
		}
	}
	if _, err := l.runtime.Execute(l.request("read_file", map[string]string{"path": filepath.Join(l.earlier, "kernel", "processes.json")})); !errors.Is(err, tools.ErrPathDenied) {
		t.Errorf("read_file of the earlier session: %v", err)
	}
	if _, ok := interactiveRoot(l.earlier, ""); !ok {
		t.Fatal("the earlier session should still load")
	}
	if _, err := l.runtime.Execute(l.request("write_file", map[string]string{"path": filepath.Join(l.root, "notes.txt"), "content": "ok"})); err != nil {
		t.Fatalf("the workspace should stay writable: %v", err)
	}
}

// When the sessions directory holds the workspace, as the state
// directory's default parent can, protecting it would leave the tools
// nothing: then each session in it is protected, and the workspace is not.
func TestSessionsDirectoryHoldingTheWorkspace(t *testing.T) {
	parent, _ := filepath.EvalSymlinks(t.TempDir())
	workspace, current, earlier := filepath.Join(parent, "ws"), filepath.Join(parent, "current"), filepath.Join(parent, "earlier")
	makeSession(t, earlier, "hello")
	os.MkdirAll(workspace, 0o755)
	os.MkdirAll(current, 0o755)
	os.MkdirAll(filepath.Join(parent, "plain"), 0o755)
	got := protectedPaths(current, parent, workspace)
	want := []string{current, statedir.LockPath(current), earlier, statedir.LockPath(earlier)}
	if !slices.Equal(got, want) {
		t.Fatalf("protected = %v, want %v", got, want)
	}
	// The same when the workspace is the sessions directory itself.
	if got := protectedPaths(current, parent, parent); !slices.Equal(got, want) {
		t.Fatalf("protected = %v, want %v", got, want)
	}
	// And under the temporary directory, which run's scratch is in.
	if got := protectedPaths(filepath.Join(os.TempDir(), "aos-tui-1"), os.TempDir(), workspace); slices.Contains(got, os.TempDir()) {
		t.Fatalf("the temporary directory should not be protected: %v", got)
	}
}
