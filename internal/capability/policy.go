package capability

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
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
	// NetworkAllow names hosts, IP addresses or CIDR ranges web_fetch may
	// reach even though they are loopback, private or link-local, or the
	// model endpoint. It only matters with AllowNetwork.
	NetworkAllow []string `json:"network_allow,omitempty"`
	// UnconfinedRun lets run execute without the OS sandbox, for agents
	// isolated some other way, such as inside a disposable container.
	// Without it, run is confined or refused.
	UnconfinedRun bool `json:"unconfined_run,omitempty"`
	// MaxRunSeconds bounds the timeout of a run command; 0 means
	// DefaultMaxRunSeconds.
	MaxRunSeconds int `json:"max_run_seconds,omitempty"`
	// Budget limits what the process, together with everything it
	// delegates to, may consume.
	Budget Budget `json:"budget,omitzero"`
}

// Budget limits model tokens and active time, for a process and its
// descendants together (their use counts against it). Zero is unlimited.
// Tokens count prompt and completion tokens of every model call, cached
// prompt tokens included, since the server still processes them. Time is
// wall-clock time spent running, not time parked between turns. A time
// budget is advisory unless StrictTime: the agent sees how much is left
// and is told to commit its answer near the end, but only a strict budget
// stops it.
type Budget struct {
	Tokens     int  `json:"tokens,omitempty"`
	Seconds    int  `json:"seconds,omitempty"`
	StrictTime bool `json:"strict_time,omitempty"`
}

// tighter is the stricter of two limits, 0 meaning unlimited.
func tighter(a, b int) int {
	switch {
	case a == 0:
		return b
	case b == 0:
		return a
	}
	return min(a, b)
}

// DefaultMaxRunSeconds bounds a run command's timeout when the set does
// not say.
const DefaultMaxRunSeconds = 600

// RunTimeoutBound is the longest timeout a run command may ask for.
func (s Set) RunTimeoutBound() time.Duration {
	return time.Duration(effectiveRunSeconds(s.MaxRunSeconds)) * time.Second
}

func effectiveRunSeconds(seconds int) int {
	if seconds <= 0 {
		return DefaultMaxRunSeconds
	}
	return seconds
}

// RunScopes is where a confined run may read and write: each read scope
// (write scope) that lies within a mount (writable mount), cut down to the
// mount where the scope is wider, with symlinks resolved. readOnly lists
// read-only mounts, which stay unwritable even inside a write root.
func RunScopes(set Set, mounts []Mount) (read, write, readOnly []string) {
	for _, mount := range mounts {
		root := resolvePath(mount.HostPath)
		for _, scope := range set.ReadPaths {
			if shared, ok := overlap(resolvePath(scope), root); ok {
				read = appendUnique(read, shared)
			}
		}
		if mount.ReadOnly {
			readOnly = appendUnique(readOnly, root)
			continue
		}
		for _, scope := range set.WritePaths {
			if shared, ok := overlap(resolvePath(scope), root); ok {
				write = appendUnique(write, shared)
			}
		}
	}
	return read, write, readOnly
}

// overlap is the narrower of two nested paths.
func overlap(a, b string) (string, bool) {
	switch {
	case within(b, a):
		return a, true
	case within(a, b):
		return b, true
	}
	return "", false
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
	derived := Set{
		ReadPaths:     intersectScopes(parent.ReadPaths, requested.ReadPaths),
		WritePaths:    intersectScopes(parent.WritePaths, requested.WritePaths),
		Tools:         intersectStrings(parent.Tools, requested.Tools),
		AllowNetwork:  parent.AllowNetwork && requested.AllowNetwork,
		NetworkAllow:  intersectStrings(parent.NetworkAllow, requested.NetworkAllow),
		UnconfinedRun: parent.UnconfinedRun && requested.UnconfinedRun,
	}
	if parent.MaxRunSeconds > 0 || requested.MaxRunSeconds > 0 {
		derived.MaxRunSeconds = min(effectiveRunSeconds(parent.MaxRunSeconds), effectiveRunSeconds(requested.MaxRunSeconds))
	}
	// A child's budget never exceeds its parent's, and a strict parent
	// makes the child strict.
	derived.Budget = Budget{
		Tokens:  tighter(parent.Budget.Tokens, requested.Budget.Tokens),
		Seconds: tighter(parent.Budget.Seconds, requested.Budget.Seconds),
	}
	derived.Budget.StrictTime = derived.Budget.Seconds > 0 && (parent.Budget.StrictTime || requested.Budget.StrictTime)
	return derived
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

// ResolvePath returns path with symlinks followed, as the capability checks
// judge it.
func ResolvePath(path string) string { return resolvePath(path) }

// Within reports whether target is base or lies inside it.
func Within(base, target string) bool { return within(base, target) }
