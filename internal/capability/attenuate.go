package capability

import (
	"fmt"
	"path/filepath"
	"slices"
)

// Union is the authority of both sets: an agent's own capabilities plus
// what it may only delegate.
func Union(a, b Set) Set {
	return Set{
		ReadPaths:    unionStrings(a.ReadPaths, b.ReadPaths),
		WritePaths:   unionStrings(a.WritePaths, b.WritePaths),
		Tools:        unionStrings(a.Tools, b.Tools),
		AllowNetwork: a.AllowNetwork || b.AllowNetwork,
	}
}

// Excess lists what requested asks for beyond grantable: tools it lacks,
// path scopes outside every granted scope, and network. Empty means
// requested is within grantable.
func Excess(grantable, requested Set) []string {
	var excess []string
	for _, tool := range requested.Tools {
		if !slices.Contains(grantable.Tools, tool) {
			excess = append(excess, "tool "+tool)
		}
	}
	for _, path := range requested.ReadPaths {
		if !withinAnyScope(grantable.ReadPaths, path) {
			excess = append(excess, "read path "+path)
		}
	}
	for _, path := range requested.WritePaths {
		if !withinAnyScope(grantable.WritePaths, path) {
			excess = append(excess, "write path "+path)
		}
	}
	if requested.AllowNetwork && !grantable.AllowNetwork {
		excess = append(excess, "network")
	}
	return excess
}

// AttenuateMounts keeps the requested mounts that fit inside a parent
// mount, narrows a requested mount that encloses a parent mount down to
// it, and drops the rest. A mount is read-only if the request or any parent
// mount covering it is.
func AttenuateMounts(parent, requested []Mount) []Mount {
	var out []Mount
	for _, mount := range requested {
		if covering := coveringMounts(parent, mount.HostPath); len(covering) > 0 {
			mount.ReadOnly = mount.ReadOnly || anyReadOnly(covering)
			out = append(out, mount)
			continue
		}
		for _, outer := range parent {
			if within(mount.HostPath, outer.HostPath) {
				out = append(out, Mount{Name: mount.Name, HostPath: outer.HostPath, ReadOnly: mount.ReadOnly || outer.ReadOnly})
			}
		}
	}
	return out
}

// MountExcess lists requested mounts that no parent mount covers, and
// writable mounts under a read-only parent mount.
func MountExcess(parent, requested []Mount) []string {
	var excess []string
	for _, mount := range requested {
		covering := coveringMounts(parent, mount.HostPath)
		switch {
		case len(covering) == 0:
			excess = append(excess, fmt.Sprintf("mount %s (%s) outside the parent's mounts", mount.Name, mount.HostPath))
		case !mount.ReadOnly && anyReadOnly(covering):
			excess = append(excess, fmt.Sprintf("writable mount %s (%s) under a read-only parent mount", mount.Name, mount.HostPath))
		}
	}
	return excess
}

func coveringMounts(mounts []Mount, path string) []Mount {
	var out []Mount
	for _, mount := range mounts {
		if within(mount.HostPath, path) {
			out = append(out, mount)
		}
	}
	return out
}

func anyReadOnly(mounts []Mount) bool {
	for _, mount := range mounts {
		if mount.ReadOnly {
			return true
		}
	}
	return false
}

func withinAnyScope(scopes []string, path string) bool {
	for _, scope := range scopes {
		if within(scope, path) {
			return true
		}
	}
	return false
}

// intersectScopes keeps each requested scope inside a parent scope, and cuts
// a requested scope that encloses parent scopes down to those.
func intersectScopes(parent, requested []string) []string {
	var out []string
	for _, scope := range requested {
		if withinAnyScope(parent, scope) {
			out = appendUnique(out, filepath.Clean(scope))
			continue
		}
		for _, inner := range parent {
			if within(scope, inner) {
				out = appendUnique(out, filepath.Clean(inner))
			}
		}
	}
	return out
}

func unionStrings(a, b []string) []string {
	out := append([]string(nil), a...)
	for _, value := range b {
		out = appendUnique(out, value)
	}
	return out
}

func appendUnique(values []string, value string) []string {
	if slices.Contains(values, value) {
		return values
	}
	return append(values, value)
}
