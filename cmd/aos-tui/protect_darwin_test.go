//go:build darwin

package main

import (
	"os"
	"path/filepath"
	"testing"

	"aos/internal/sandbox"
)

// Under scripts/tui.sh's layout a confined run cannot write or read an
// earlier session either, and still writes the workspace.
func TestRunCannotReachEarlierSessions(t *testing.T) {
	if err := sandbox.Available(); err != nil {
		t.Skip(err)
	}
	l := newTUILayout(t)
	target := filepath.Join(l.earlier, "kernel", "probe-sibling-run.txt")
	for _, command := range []string{
		"echo forged > " + target,
		"echo '{}' > " + filepath.Join(l.earlier, "kernel", "processes.json"),
		"cat " + filepath.Join(l.earlier, "kernel", "processes.json"),
		"mkdir -p " + filepath.Join(l.sessions, "planted", "kernel"),
	} {
		result, err := l.runtime.Execute(l.request("run", map[string]string{"command": command}))
		if err != nil {
			t.Fatal(err)
		}
		if result.Output["exit_code"] == "0" || result.Output["confinement"] != "sandbox" {
			t.Errorf("%q: exit %s, confinement %q, stdout %q", command, result.Output["exit_code"], result.Output["confinement"], result.Output["stdout"])
		}
	}
	if _, err := os.Stat(target); err == nil {
		t.Error("run wrote into the earlier session")
	}
	if _, ok := interactiveRoot(l.earlier, ""); !ok {
		t.Error("the earlier session no longer loads")
	}
	ok, err := l.runtime.Execute(l.request("run", map[string]string{"command": "echo ok > " + filepath.Join(l.root, "run-note.txt") + " && cat " + filepath.Join(l.root, "run-note.txt")}))
	if err != nil || ok.Output["exit_code"] != "0" || ok.Output["stdout"] != "ok\n" {
		t.Fatalf("run in the workspace = %+v, %v", ok.Output, err)
	}
}
