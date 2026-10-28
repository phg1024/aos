// Package sandbox runs a shell command confined by the operating system to
// the paths and network a process's capabilities grant: sandbox-exec on
// macOS, Landlock on Linux. Where neither is available, or the policy is
// one the platform cannot express, Command fails with ErrUnavailable; the
// caller must refuse rather than run unconfined.
//
// What it does not cover: CPU, memory, process count and wall time (the
// caller's timeout bounds time); see the per-platform files for the rest.
package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// ErrUnavailable means a command cannot be confined here as asked.
var ErrUnavailable = errors.New("no OS sandbox can confine run here")

// Policy is what a confined command may touch. Paths are absolute and
// already resolved (no symlinks); a path grants its whole subtree.
type Policy struct {
	// Read roots are readable, Write roots readable and writable.
	Read  []string
	Write []string
	// ReadOnly roots stay unwritable even inside a Write root.
	ReadOnly []string
	// Hidden roots (the state directory) are neither readable nor
	// writable, whatever else grants.
	Hidden []string
	// Network allows outbound and inbound connections and name lookups.
	Network bool
	// Temp is a writable scratch directory for this process only; the
	// command gets it as TMPDIR, and toolchain caches are pointed there.
	Temp string
}

// Environment is the command's environment: the caller's, with temporary
// and cache directories moved into the scratch directory, since the usual
// per-user ones are shared with other processes and not writable.
func (p Policy) Environment(base []string) []string {
	overrides := map[string]string{
		"TMPDIR":   p.Temp,
		"TMP":      p.Temp,
		"TEMP":     p.Temp,
		"GOCACHE":  filepath.Join(p.Temp, "go-build"),
		"GOTMPDIR": "",
		// xcrun caches tool lookups in the per-user temp dir otherwise, and
		// prints a misleading "error" when it cannot.
		"xcrun_db": filepath.Join(p.Temp, "xcrun_db"),
	}
	var env []string
	for _, entry := range base {
		name, _, _ := strings.Cut(entry, "=")
		if _, replaced := overrides[name]; !replaced {
			env = append(env, entry)
		}
	}
	for name, value := range overrides {
		if value != "" {
			env = append(env, name+"="+value)
		}
	}
	return env
}

// ToolchainReadPaths are install prefixes of the commands on PATH (the
// parent of each .../bin), so a toolchain installed under the home
// directory, such as miniforge or a Go install, keeps working read-only,
// plus any extra paths in AOS_SANDBOX_READ (colon-separated). The home
// directory itself and anything above it are never included.
func ToolchainReadPaths() []string {
	home, _ := os.UserHomeDir()
	home = resolve(home)
	var out []string
	add := func(path string) {
		path = resolve(path)
		if path == "" || path == "/" || (home != "" && within(path, home)) {
			return
		}
		for _, existing := range out {
			if existing == path {
				return
			}
		}
		out = append(out, path)
	}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if !filepath.IsAbs(dir) {
			continue
		}
		if filepath.Base(dir) == "bin" {
			dir = filepath.Dir(dir)
		}
		add(dir)
	}
	for _, extra := range filepath.SplitList(os.Getenv("AOS_SANDBOX_READ")) {
		if filepath.IsAbs(extra) {
			add(extra)
		}
	}
	return out
}

func resolve(path string) string {
	if path == "" {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return filepath.Clean(path)
}

// within reports whether target is base or inside it.
func within(base, target string) bool {
	rel, err := filepath.Rel(base, target)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

// overlaps reports whether either path contains the other.
func overlaps(a, b string) bool { return within(a, b) || within(b, a) }
