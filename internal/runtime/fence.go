package runtime

import (
	"errors"
	"path/filepath"
	"strconv"
	"strings"

	"aos/internal/capability"
	"aos/internal/kernel"
	"aos/internal/lease"
	"aos/internal/tools"
)

// LeaseFencedEvent is the kernel event of a write a lease refused.
const LeaseFencedEvent = "lease.fenced"

// fencedWrite reports whether call is a file write the leases guard: the
// tool writes one named path, a lease manager is bound, and fencing is on.
// run is not fenced: what it writes is known only after it ran.
func (r *AgentRuntime) fencedWrite(call ToolCall) bool {
	if r.NoLeaseFencing || r.Leases == nil {
		return false
	}
	return (call.Name == "write_file" || call.Name == "edit_file") && strings.TrimSpace(call.Args["path"]) != ""
}

// writers is pid and the processes it was recovered from: a lease any of
// them was granted counts as the writer's own.
func (r *AgentRuntime) writers(pid string) []string {
	writers := []string{pid}
	current := pid
	for hops := 0; hops < maxResumeChain; hops++ {
		process, err := r.Kernel.Get(current)
		if err != nil || process.Metadata["resumed_from"] == "" {
			break
		}
		current = process.Metadata["resumed_from"]
		writers = append(writers, current)
	}
	return writers
}

// leaseCovers returns whether a lease's resource covers target, a path the
// process writes. A resource names a path: an absolute one as is, a
// relative one under the workspace mount (else the first mount). It covers
// target when that path is target or a directory above it, and lies in one
// of the process's mounts. A resource such as "db-migration" names
// <workspace>/db-migration, so it covers only writes there.
func leaseCovers(process kernel.Process, target string) func(resource string) bool {
	target = capability.ResolvePath(filepath.Clean(target))
	base := ""
	for _, mount := range process.Mounts {
		if mount.Name == tools.WorkspaceMount {
			base = mount.HostPath
			break
		}
	}
	if base == "" && len(process.Mounts) > 0 {
		base = process.Mounts[0].HostPath
	}
	return func(resource string) bool {
		resource = strings.TrimSpace(resource)
		if !filepath.IsAbs(resource) {
			if base == "" {
				return false
			}
			resource = filepath.Join(base, resource)
		}
		path := capability.ResolvePath(filepath.Clean(resource))
		inMount := false
		for _, mount := range process.Mounts {
			if capability.Within(capability.ResolvePath(mount.HostPath), path) {
				inMount = true
				break
			}
		}
		return inMount && capability.Within(path, target)
	}
}

// logFence records a refused write in the kernel log.
func (r *AgentRuntime) logFence(pid string, call ToolCall, err error) {
	var fence *lease.FenceError
	if !errors.As(err, &fence) {
		return
	}
	data := map[string]string{
		"tool":     call.Name,
		"path":     call.Args["path"],
		"resource": fence.Resource,
		"reason":   fence.Reason,
		"token":    strconv.FormatInt(fence.Token, 10),
	}
	if fence.Holder != "" {
		data["holder"] = fence.Holder
	}
	if fence.HeldToken != 0 {
		data["held_token"] = strconv.FormatInt(fence.HeldToken, 10)
	}
	_, _ = r.Kernel.RecordEvent(pid, LeaseFencedEvent, data)
}
