package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"aos/internal/capability"
	"aos/internal/sandbox"
)

// runCommand builds the command for run: confined by the OS sandbox to the
// process's scopes, or, only for a process granted unconfined_run, a plain
// shell. It names which in confinement.
func (r *Runtime) runCommand(ctx context.Context, req Request, command string) (*exec.Cmd, string, error) {
	dir := mountRoot(req.Mounts)
	if req.CapabilitySet.UnconfinedRun {
		cmd := exec.CommandContext(ctx, "sh", "-c", command)
		cmd.Dir = dir
		return cmd, "unconfined (unconfined_run)", nil
	}
	policy, err := r.runPolicy(req)
	if err != nil {
		return nil, "", err
	}
	confine := r.Confine
	if confine == nil {
		confine = sandbox.Command
	}
	cmd, err := confine(ctx, policy, dir, command)
	if err != nil {
		if errors.Is(err, sandbox.ErrUnavailable) {
			return nil, "", fmt.Errorf("%w: %v", ErrRunUnconfined, err)
		}
		return nil, "", err
	}
	return cmd, "sandbox", nil
}

// runPolicy is what a confined run of req's process may touch: its read
// and write scopes within its mounts, its own scratch directory, network
// only with allow_network, and never the protected paths.
func (r *Runtime) runPolicy(req Request) (sandbox.Policy, error) {
	read, write, readOnly := capability.RunScopes(req.CapabilitySet, req.Mounts)
	temp, err := r.scratchDir(req.PID)
	if err != nil {
		return sandbox.Policy{}, err
	}
	var hidden []string
	for _, path := range r.Protected {
		hidden = append(hidden, capability.ResolvePath(path))
	}
	return sandbox.Policy{
		Read:     read,
		Write:    write,
		ReadOnly: readOnly,
		Hidden:   hidden,
		Network:  req.CapabilitySet.AllowNetwork,
		Temp:     temp,
	}, nil
}

// scratchDir is pid's own writable directory for run, kept across its
// calls (so build caches stay warm) and shared with no other process.
func (r *Runtime) scratchDir(pid string) (string, error) {
	r.scratchMu.Lock()
	defer r.scratchMu.Unlock()
	if r.ScratchRoot == "" {
		root, err := os.MkdirTemp("", "aos-scratch-")
		if err != nil {
			return "", err
		}
		r.ScratchRoot = root
	}
	name := strings.Map(func(c rune) rune {
		if c == '/' || c == '\\' || c == '.' || c == 0 {
			return '_'
		}
		return c
	}, pid)
	if name == "" {
		name = "anonymous"
	}
	dir := filepath.Join(r.ScratchRoot, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return capability.ResolvePath(dir), nil
}
