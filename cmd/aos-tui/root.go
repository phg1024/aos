package main

import (
	"context"
	"errors"
	"fmt"

	"aos/internal/artifact"
	"aos/internal/ipc"
	"aos/internal/kernel"
	"aos/internal/runtime"
)

// tuiRoot is the interactive process the user's lines go to, and the last
// checkpoint and session its conversation reached. A turn that fails never
// replaces them with a zero result's, so the next line still has them.
type tuiRoot struct {
	process      kernel.Process
	checkpointID string
	session      runtime.Session
	// seed is the conversation a new root starts from when it continues an
	// ended one; it is kept until the new root's first checkpoint.
	seed []runtime.Message
}

// continueEnded replaces a root whose exit is final with a new one that
// continues its conversation, as resumePoint does at startup: Recover
// refuses a final exit, so finished work never runs again, but the user
// can still talk on. Mail the old root never read moves to the new one. It
// returns a note for the user, or "" when the root can take the line.
func continueEnded(k *kernel.Kernel, store *artifact.Store, bus *ipc.Bus, root *tuiRoot, spawn func() (kernel.Process, error)) (string, error) {
	current, err := k.Get(root.process.PID)
	if err != nil {
		return "", err
	}
	if current.State != kernel.StateExited {
		return "", nil
	}
	if root.checkpointID != "" && current.ExitStatus != nil && current.ExitStatus.Retryable {
		// Resume recovers it from its checkpoint.
		return "", nil
	}
	seed := root.seed
	if root.checkpointID != "" {
		var session runtime.Session
		if err := store.ReadJSON(root.checkpointID, &session); err != nil {
			return "", err
		}
		root.session = session
		seed = session.Messages
	}
	next, err := spawn()
	if err != nil {
		return "", err
	}
	if _, err := bus.Transfer(current.PID, next.PID); err != nil {
		return "", err
	}
	reason := ""
	if current.ExitStatus != nil {
		reason = current.ExitStatus.Reason
	}
	root.process, root.checkpointID, root.seed = next, "", seed
	return fmt.Sprintf("the agent %s exited (%s); a new agent %s continues its conversation", current.PID, reason, next.PID), nil
}

// deliver puts line in the root's mailbox. A root that exited cannot take
// mail; its recovery carries the line instead, as appended.
func deliver(bus *ipc.Bus, root *tuiRoot, line string) (appended []runtime.Message, err error) {
	if _, err := bus.SendAsOperator(root.process.PID, line); errors.Is(err, ipc.ErrRecipientExited) {
		return []runtime.Message{{Role: "user", Content: line}}, nil
	} else if err != nil {
		return nil, err
	}
	return nil, nil
}

// runTurn runs the root until it next parks or exits: a new root from its
// seed, otherwise from its checkpoint.
func runTurn(ctx context.Context, r *runtime.AgentRuntime, root *tuiRoot, appended []runtime.Message, options runtime.RunOptions) (runtime.RunResult, error) {
	if root.checkpointID == "" {
		return r.RunWithOptions(ctx, root.process.PID, root.seed, options)
	}
	return r.Resume(ctx, root.process.PID, root.checkpointID, appended, options)
}

// record keeps what a turn reached. A result without a checkpoint (a turn
// that failed before one) leaves the last good checkpoint and session.
func (root *tuiRoot) record(store *artifact.Store, result runtime.RunResult) {
	if result.PID != "" {
		// Resuming an exited process recovers it as a new one.
		root.process.PID = result.PID
	}
	if result.CheckpointID == "" {
		return
	}
	root.checkpointID = result.CheckpointID
	root.seed = nil
	root.session = result.Session
	if len(result.Session.Messages) == 0 {
		var session runtime.Session
		if store.ReadJSON(result.CheckpointID, &session) == nil {
			root.session = session
		}
	}
}

// lostLine reports whether a line a recovery was to carry (appended) never
// reached a checkpoint: the turn failed before the recovered process saved
// one, so the user must send it again.
func lostLine(appended []runtime.Message, before string, result runtime.RunResult) bool {
	return len(appended) > 0 && (result.CheckpointID == "" || result.CheckpointID == before)
}
