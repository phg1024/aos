package tools

import (
	"io/fs"
	"path/filepath"
	"sort"
	"time"

	"aos/internal/capability"
)

// FileChanges is what a run command did to the files under the workspace
// mount, found by comparing each path's size and modification time before
// and after the command. It is recorded with the tool result, not shown to
// the agent, so prompts do not change. Writes outside the workspace mount
// are not covered, and a write that keeps a file's size and modification
// time is not seen.
type FileChanges struct {
	// Root is the directory compared; the paths are relative to it.
	Root     string   `json:"root"`
	Created  []string `json:"created,omitempty"`
	Modified []string `json:"modified,omitempty"`
	Deleted  []string `json:"deleted,omitempty"`
	// More counts changes beyond the MaxListedChanges listed.
	More int `json:"more,omitempty"`
	// Unknown says why nothing could be compared, such as a tree larger
	// than MaxSnapshotEntries.
	Unknown string `json:"unknown,omitempty"`
}

// Bounds on the comparison: the entries walked in each snapshot, and the
// changes listed.
const (
	MaxSnapshotEntries = 50000
	MaxListedChanges   = 200
)

// WorkspaceMount names the mount whose files a run command's changes are
// recorded for.
const WorkspaceMount = "workspace"

// fileState is what a snapshot keeps of one entry. Directories keep only
// that they exist: their modification time changes with their contents.
type fileState struct {
	dir     bool
	size    int64
	modTime time.Time
}

// snapshot walks root, without following links, up to limit entries; ok
// is false when the tree is larger.
func snapshot(root string, limit int) (map[string]fileState, bool) {
	states := map[string]fileState{}
	complete := true
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || path == root {
			return nil
		}
		if len(states) >= limit {
			complete = false
			return filepath.SkipAll
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		if entry.IsDir() {
			states[rel] = fileState{dir: true}
			return nil
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return nil
		}
		states[rel] = fileState{size: info.Size(), modTime: info.ModTime()}
		return nil
	})
	return states, complete
}

// compareSnapshots lists what changed between before and after, sorted
// and capped.
func compareSnapshots(root string, before, after map[string]fileState) *FileChanges {
	changes := &FileChanges{Root: root}
	var created, modified, deleted []string
	for path, state := range after {
		previous, existed := before[path]
		switch {
		case !existed:
			created = append(created, path)
		case !state.dir && (previous.dir || state.size != previous.size || !state.modTime.Equal(previous.modTime)):
			modified = append(modified, path)
		}
	}
	for path := range before {
		if _, exists := after[path]; !exists {
			deleted = append(deleted, path)
		}
	}
	listed := 0
	keep := func(paths []string) []string {
		sort.Strings(paths)
		room := max(0, MaxListedChanges-listed)
		if len(paths) > room {
			changes.More += len(paths) - room
			paths = paths[:room]
		}
		listed += len(paths)
		return paths
	}
	changes.Created, changes.Modified, changes.Deleted = keep(created), keep(modified), keep(deleted)
	return changes
}

// workspaceRoot is the run's workspace mount, resolved, or "".
func workspaceRoot(mounts []capability.Mount) string {
	for _, mount := range mounts {
		if mount.Name == WorkspaceMount {
			return capability.ResolvePath(mount.HostPath)
		}
	}
	return ""
}

// watchChanges snapshots the workspace mount and returns a function that
// compares it with a second snapshot; both are no-ops without one.
func watchChanges(mounts []capability.Mount) func() *FileChanges {
	root := workspaceRoot(mounts)
	if root == "" {
		return func() *FileChanges { return nil }
	}
	before, complete := snapshot(root, MaxSnapshotEntries)
	return func() *FileChanges {
		if !complete {
			return &FileChanges{Root: root, Unknown: "the workspace has more than 50000 entries"}
		}
		after, complete := snapshot(root, MaxSnapshotEntries)
		if !complete {
			return &FileChanges{Root: root, Unknown: "the workspace has more than 50000 entries"}
		}
		return compareSnapshots(root, before, after)
	}
}
