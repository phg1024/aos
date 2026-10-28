package capability

import (
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

func withinAllowed(path string, mounts []Mount, scopes []string, write bool) bool {
	cleanPath := filepath.Clean(path)
	for _, mount := range mounts {
		if write && mount.ReadOnly && within(mount.HostPath, cleanPath) {
			return false
		}
	}
	for _, scope := range scopes {
		if within(scope, cleanPath) && withinAnyMount(cleanPath, mounts) {
			return true
		}
	}
	return false
}

func withinAnyMount(path string, mounts []Mount) bool {
	for _, mount := range mounts {
		if within(mount.HostPath, path) {
			return true
		}
	}
	return false
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
