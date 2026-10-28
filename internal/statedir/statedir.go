// Package statedir guards a state directory shared by aos runs. One run at
// a time (aos run --workflow, aos-tui) holds its lock; a run that is gone
// may have left processes with no exit record, which Reap closes so they
// can be recovered instead of looking live forever.
package statedir

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"aos/internal/artifact"
	"aos/internal/filelock"
	"aos/internal/kernel"
)

// ErrInUse means another run holds the state directory's lock.
var ErrInUse = errors.New("state directory is in use by another aos run")

// ErrLockUnsupported means file locks do not coordinate processes on this
// platform, so whether a run is alive cannot be told.
var ErrLockUnsupported = errors.New("state directory locks are not supported on this platform")

// OrphanReason is the exit reason Reap records.
const OrphanReason = "orphaned: owning run is gone"

// LockPath is the lock file of dir, beside it.
func LockPath(dir string) string { return filepath.Clean(dir) + ".lock" }

// Lock takes dir's lock for a run, or fails with ErrInUse, and notes who
// holds it in the lock file.
func Lock(dir string) (func(), error) {
	held, err := filelock.TryAcquire(LockPath(dir))
	if errors.Is(err, filelock.ErrLocked) {
		return nil, fmt.Errorf("%w: %s", ErrInUse, dir)
	}
	if err != nil {
		return nil, err
	}
	if file := held.File(); file.Truncate(0) == nil {
		_, _ = file.WriteString(fmt.Sprintf("pid=%d\nstarted_at=%s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339)))
		_, _ = file.Seek(0, 0)
	}
	return held.Release, nil
}

// InUse reports whether a run holds dir's lock now. It is false where locks
// are unsupported, since nothing can be known.
func InUse(dir string) (bool, error) {
	if !filelock.Supported {
		return false, nil
	}
	if _, err := os.Stat(LockPath(dir)); errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	held, err := filelock.TryAcquire(LockPath(dir))
	if errors.Is(err, filelock.ErrLocked) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	held.Release()
	return false, nil
}

// LatestCheckpoints maps each producer PID to its newest checkpoint.
func LatestCheckpoints(dir string) (map[string]string, error) {
	latest := map[string]string{}
	artifactsDir := filepath.Join(dir, "artifacts")
	if _, err := os.Stat(filepath.Join(artifactsDir, "index.json")); errors.Is(err, os.ErrNotExist) {
		return latest, nil
	}
	store, err := artifact.New(artifactsDir)
	if err != nil {
		return nil, err
	}
	records, err := store.List()
	if err != nil {
		return nil, err
	}
	for _, record := range records {
		if record.Type == "checkpoint" && Newer(record.ID, latest[record.ProducerPID]) {
			latest[record.ProducerPID] = record.ID
		}
	}
	return latest, nil
}

// Newer compares IDs such as "art-000042" by number.
func Newer(id, than string) bool {
	if than == "" {
		return true
	}
	return idNumber(id) > idNumber(than)
}

func idNumber(id string) int {
	if i := strings.LastIndex(id, "-"); i >= 0 {
		n, _ := strconv.Atoi(id[i+1:])
		return n
	}
	return 0
}

// Reap exits every process in dir that has no exit record, provided no run
// holds dir: each gets a retryable runtime-origin exit naming its latest
// checkpoint, so Recover can pick it up. It returns the PIDs it exited,
// children before parents.
func Reap(dir string) ([]string, error) {
	if !filelock.Supported {
		return nil, ErrLockUnsupported
	}
	release, err := Lock(dir)
	if err != nil {
		return nil, err
	}
	defer release()
	kernelDir := filepath.Join(dir, "kernel")
	if _, err := os.Stat(filepath.Join(kernelDir, "processes.json")); err != nil {
		return nil, fmt.Errorf("%s is not an aos state directory: %w", dir, err)
	}
	k, err := kernel.New(kernelDir)
	if err != nil {
		return nil, err
	}
	processes, err := k.Processes()
	if err != nil {
		return nil, err
	}
	checkpoints, err := LatestCheckpoints(dir)
	if err != nil {
		return nil, err
	}
	sort.Slice(processes, func(i, j int) bool { return processes[i].PID > processes[j].PID })
	var reaped []string
	for _, process := range processes {
		if process.State == kernel.StateExited {
			continue
		}
		status := kernel.ExitStatus{Code: 1, Reason: OrphanReason, Retryable: true, Checkpoint: checkpoints[process.PID]}
		if _, err := k.Exit(process.PID, status); err != nil {
			return reaped, fmt.Errorf("reap %s: %w", process.PID, err)
		}
		reaped = append(reaped, process.PID)
	}
	return reaped, nil
}
