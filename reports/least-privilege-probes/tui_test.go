//go:build darwin

package lpprobes

import (
	"os"

	"aos/internal/capability"
	"aos/internal/statedir"
)

// tuiProtectedPaths is cmd/aos-tui's protectedPaths (sessions.go, b3c53b0)
// for the layout scripts/tui.sh makes, where the sessions directory holds
// neither the workdir nor the temporary directory: then it protects this
// session's state directory, its lock and the whole sessions directory.
// The other branch, which lists sessions one by one through the TUI's own
// interactiveRoot, is not copied; ok is false if the layout would take it.
func tuiProtectedPaths(stateDir, sessionsDir, workdir string) (paths []string, ok bool) {
	paths = []string{stateDir, statedir.LockPath(stateDir)}
	holds := func(dir string) bool {
		dir = capability.ResolvePath(dir)
		return capability.Within(dir, capability.ResolvePath(workdir)) || capability.Within(dir, capability.ResolvePath(os.TempDir()))
	}
	if holds(sessionsDir) {
		return nil, false
	}
	return append(paths, sessionsDir), true
}
