package agentflow

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ErrPreflight is a workflow whose host paths cannot work on this machine.
var ErrPreflight = errors.New("preflight")

var placeholder = regexp.MustCompile(`__[A-Z][A-Z0-9_]*__`)

// PreflightHostPaths refuses, before anything is spawned, a workflow whose
// host paths cannot work here: a skill root the spec names, a read-only
// mount or a read path that does not exist, or a writable mount whose
// parent does not exist (aos run creates one whose parent does). skillRoots are the spec's own, resolved against its directory;
// the default roots are not checked. Validate is host-independent; this is
// the check that is not. aos run and aos-tui run it before they make a
// state directory.
func PreflightHostPaths(spec WorkflowSpec, skillRoots []string) error {
	var missing []string
	exists := func(path string) bool {
		_, err := os.Stat(path)
		return err == nil
	}
	note := func(format string, args ...any) { missing = append(missing, fmt.Sprintf(format, args...)) }
	for i, root := range skillRoots {
		if !exists(root) {
			note("skill_roots[%d]: %s does not exist", i, root)
		}
	}
	for i, agent := range spec.Agents {
		where := fmt.Sprintf("agents[%d] (%s)", i, agent.ID)
		for j, mount := range agent.Mounts {
			switch {
			case mount.HostPath == "":
			case mount.ReadOnly && !exists(mount.HostPath):
				note("%s.mounts[%d] %q, read-only: %s does not exist", where, j, mount.Name, mount.HostPath)
			case !mount.ReadOnly && !exists(mount.HostPath) && !exists(filepath.Dir(mount.HostPath)):
				note("%s.mounts[%d] %q, writable: neither %s nor its parent exists", where, j, mount.Name, mount.HostPath)
			}
		}
		for j, path := range agent.Capabilities.ReadPaths {
			if !exists(path) {
				note("%s.capabilities.read_paths[%d]: %s does not exist", where, j, path)
			}
		}
	}
	if len(missing) == 0 {
		return nil
	}
	hint := "Point them at paths on this machine, or run the workflow where they exist."
	if placeholder.MatchString(strings.Join(missing, " ")) {
		hint += " A path still holds a placeholder such as __RUN_DIR__, which the workflow's run script fills in."
	}
	return fmt.Errorf("%w: workflow %s names host paths that do not exist on this machine; nothing was run:\n  - %s\n%s", ErrPreflight, spec.Name, strings.Join(missing, "\n  - "), hint)
}
