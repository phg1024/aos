package capability

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
)

type Mount struct {
	Name     string `json:"name"`
	HostPath string `json:"host_path"`
	ReadOnly bool   `json:"read_only"`
}

type Set struct {
	ReadPaths    []string `json:"read_paths,omitempty"`
	WritePaths   []string `json:"write_paths,omitempty"`
	Tools        []string `json:"tools,omitempty"`
	AllowNetwork bool     `json:"allow_network"`
}

type Evaluator struct{}

func (Evaluator) CanRead(path string, mounts []Mount, set Set) bool {
	return withinAllowed(path, mounts, set.ReadPaths, false)
}

func (Evaluator) CanWrite(path string, mounts []Mount, set Set) bool {
	return withinAllowed(path, mounts, set.WritePaths, true)
}

func (Evaluator) ToolAllowed(tool string, set Set) bool {
	return slices.Contains(set.Tools, tool)
}

// DeriveChildSet narrows requested to what parent may grant: tools both
// name, path scopes that lie within both (a requested scope inside a
// parent scope is kept as requested; a wider one is cut down to the parent
// scope), and network only if both allow it.
func DeriveChildSet(parent, requested Set) Set {
	return Set{
		ReadPaths:    intersectScopes(parent.ReadPaths, requested.ReadPaths),
		WritePaths:   intersectScopes(parent.WritePaths, requested.WritePaths),
		Tools:        intersectStrings(parent.Tools, requested.Tools),
		AllowNetwork: parent.AllowNetwork && requested.AllowNetwork,
	}
}

// withinAllowed judges where a path really points: symlinks in the path,
// in mounts and in scopes are resolved first, so a link inside a mount that
// leads outside it is refused.
func withinAllowed(path string, mounts []Mount, scopes []string, write bool) bool {
	resolved := resolvePath(path)
	for _, mount := range mounts {
		if write && mount.ReadOnly && within(resolvePath(mount.HostPath), resolved) {
			return false
		}
	}
	for _, scope := range scopes {
		if within(resolvePath(scope), resolved) && withinAnyMount(resolved, mounts) {
			return true
		}
	}
	return false
}

func withinAnyMount(path string, mounts []Mount) bool {
	for _, mount := range mounts {
		if within(resolvePath(mount.HostPath), path) {
			return true
		}
	}
	return false
}

// maxSymlinkHops bounds link chains (and cycles) followed by resolvePath.
const maxSymlinkHops = 40

// resolvePath returns path with symlinks followed. The longest existing
// prefix is resolved; a final component that is a dangling link is followed
// to its target, since writing through it would create the target; any
// remaining, not yet existing components are appended unchanged.
func resolvePath(path string) string {
	return resolveHops(filepath.Clean(path), 0)
}

func resolveHops(path string, hops int) string {
	existing, rest := path, ""
	for {
		if resolved, err := filepath.EvalSymlinks(existing); err == nil {
			return filepath.Join(resolved, rest)
		}
		if target, err := os.Readlink(existing); err == nil && hops < maxSymlinkHops {
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(existing), target)
			}
			return filepath.Join(resolveHops(filepath.Clean(target), hops+1), rest)
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return path
		}
		rest = filepath.Join(filepath.Base(existing), rest)
		existing = parent
	}
}

func within(base, target string) bool {
	rel, err := filepath.Rel(filepath.Clean(base), filepath.Clean(target))
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, "..") && rel != "..")
}

func intersectStrings(left, right []string) []string {
	if len(left) == 0 || len(right) == 0 {
		return nil
	}
	var out []string
	for _, candidate := range right {
		if slices.Contains(left, candidate) {
			out = append(out, candidate)
		}
	}
	return out
}
