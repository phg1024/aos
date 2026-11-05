package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

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
func continueEnded(k *kernel.Kernel, store *artifact.Store, bus *ipc.Bus, root *tuiRoot, spawn func(continues string) (kernel.Process, error)) (string, error) {
	current, err := k.Get(root.process.PID)
	if err != nil {
		return "", err
	}
	if current.State != kernel.StateExited {
		return "", nil
	}
	if root.checkpointID != "" && !current.ChainEnded() {
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
	next, err := spawn(current.PID)
	if err != nil {
		return "", err
	}
	if err := takeOver(k, bus, current.PID, next.PID); err != nil {
		return "", err
	}
	reason := ""
	if current.ExitStatus != nil {
		reason = current.ExitStatus.Reason
	}
	root.process, root.checkpointID, root.seed = next, "", seed
	return fmt.Sprintf("the agent %s exited (%s); a new agent %s continues its conversation", current.PID, reason, next.PID), nil
}

// takeOver hands an ended root's mail and subscriptions to the new root that
// continues its conversation. The new root is a new chain, so nothing will
// recover the old one. Close it first: mail sent before this is moved below, and
// mail sent after is refused, so none is left behind on the old PID.
func takeOver(k *kernel.Kernel, bus *ipc.Bus, old, next string) error {
	if _, err := k.Close(old, "continued by "+next); err != nil && !errors.Is(err, kernel.ErrSuperseded) {
		return err
	}
	_, err := bus.Transfer(old, next)
	return err
}

// startRoot finds the interactive process a TUI starting on stateDir talks to.
// A root that cannot continue is replaced, and what it left unread goes to the
// new one before any step, so a line typed just before it exited is read rather
// than swept.
func startRoot(k *kernel.Kernel, bus *ipc.Bus, stateDir, agentID string, spawn func(continues string) (kernel.Process, error)) (root tuiRoot, note string, resumed bool, err error) {
	previous, hadRoot := interactiveRoot(stateDir, agentID)
	root.process, root.checkpointID, root.session, root.seed, note, resumed = resumePoint(k, stateDir, agentID)
	if root.process.PID != "" {
		return root, note, resumed, nil
	}
	continues := ""
	if hadRoot && resumed {
		continues = previous.PID
	}
	if root.process, err = spawn(continues); err != nil {
		return root, note, resumed, err
	}
	if continues != "" {
		err = takeOver(k, bus, previous.PID, root.process.PID)
	}
	return root, note, resumed, err
}

// operatorPreviewBytes is how much of an over-long line stays in the message, so
// the agent sees what it is being asked before it reads the rest.
const operatorPreviewBytes = 4 * 1024

// operatorBody fits line to the bus's cap. A person's paste is neither refused
// nor sent whole: they would have to retype it elsewhere, and one larger than the
// model's window would fail every request. It is published under the root, whose
// chain may read it, and the message opens with its start and attaches the rest.
func operatorBody(store *artifact.Store, pid, line string) (string, []string, error) {
	if len(line) <= ipc.MaxBodyBytes {
		return line, nil, nil
	}
	record, err := store.CreateJSON(artifact.PublishedPrefix+"operator_text", pid, map[string]string{"body": line}, nil, map[string]string{"kind": "published", "from": ipc.OperatorSender})
	if err != nil {
		return "", nil, err
	}
	preview := strings.ToValidUTF8(line[:operatorPreviewBytes], "")
	return fmt.Sprintf("%s\n\n[This is the start of a longer message, %d bytes in all. read_artifact id=%s reads all of it.]", preview, len(line), record.ID), []string{record.ID}, nil
}

// deliver puts line in the root's mailbox, where a root that exited
// retryably holds it for its recovery. A chain that has ended cannot take
// mail, so the next turn carries the line instead, as appended.
func deliver(bus *ipc.Bus, store *artifact.Store, root *tuiRoot, line string) (appended []runtime.Message, err error) {
	body, handles, err := operatorBody(store, root.process.PID, line)
	if err != nil {
		return nil, err
	}
	if _, err := bus.SendAsOperator(root.process.PID, body, handles...); errors.Is(err, ipc.ErrRecipientExited) {
		return []runtime.Message{{Role: "user", Content: body}}, nil
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
