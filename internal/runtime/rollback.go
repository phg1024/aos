package runtime

import (
	"slices"
	"strings"

	"aos/internal/artifact"
	"aos/internal/framing"
	"aos/internal/ipc"
	"aos/internal/statedir"
)

// restoreChain makes a successor's start true to what its chain was given.
// Mail the chain took after the checkpoint it now starts from was acknowledged
// off the bus, and this session never held it: it goes back in pid's mailbox
// under its own ID, so the session takes it and anything holding it already
// drops the repeat. Subscriptions stay with the chain, and the returned notes
// tell the session about those its history does not show.
//
// It looks at the chain's records, not at whether this Recover asked for a
// rollback, so a successor that died before its first checkpoint hands on
// what it was still owed: its exit names the earlier checkpoint as if nothing
// had been rolled back.
//
// What it cannot read never stops the Recover. A damaged checkpoint is often
// why a chain is being rolled back, and no later Recover of the chain would
// get past it either. It is recorded instead. The one error returned is the
// bus failing to take the mail back, which leaves it on record for the next
// Recover.
func (r *AgentRuntime) restoreChain(pid string, lineage []string, targetID string, target Session) ([]Message, error) {
	if r.Bus == nil {
		return nil, nil
	}
	records, err := r.Artifacts.List()
	if err != nil {
		r.recordUnrestored(pid, "ipc.redelivery_failed", map[string]string{"reason": "artifact index: " + err.Error()})
		return nil, nil
	}
	slices.SortFunc(records, func(a, b artifact.Artifact) int {
		switch {
		case statedir.Newer(a.ID, b.ID):
			return 1
		case statedir.Newer(b.ID, a.ID):
			return -1
		}
		return 0
	})
	inChain := func(record artifact.Artifact) bool { return slices.Contains(lineage, record.ProducerPID) }
	if err := r.redeliverOwed(pid, targetID, target, records, inChain); err != nil {
		return nil, err
	}
	return r.subscriptionNotes(pid, target, records), nil
}

// recordUnrestored logs what a restore could not do. A kernel that cannot
// record it cannot record the successor's run either.
func (r *AgentRuntime) recordUnrestored(pid, eventType string, data map[string]string) {
	data["cause"] = "rollback"
	_, _ = r.Kernel.RecordEvent(pid, eventType, data)
}

// redeliverOwed finds what the chain's checkpoints newer than the target hold
// and the target does not, and puts it back from the mailbox_message records.
// When one of them is unreadable, its list is gone, so every message the chain
// recorded receiving after the target counts as owed: mail still on the bus is
// not added twice. That leaves one way to lose a message, which is logged: a
// damaged checkpoint was the only list naming it and its record is unreadable
// too.
func (r *AgentRuntime) redeliverOwed(pid, targetID string, target Session, records []artifact.Artifact, inChain func(artifact.Artifact) bool) error {
	owed := []string{}
	damaged := false
	for _, record := range records {
		if record.Type != "checkpoint" || !inChain(record) || !statedir.Newer(record.ID, targetID) {
			continue
		}
		var later Session
		if err := r.Artifacts.ReadJSON(record.ID, &later); err != nil {
			damaged = true
			r.recordUnrestored(pid, "ipc.checkpoint_unreadable", map[string]string{"checkpoint": record.ID, "reason": err.Error()})
			continue
		}
		for _, id := range later.MailboxSeen {
			if !slices.Contains(target.MailboxSeen, id) && !slices.Contains(owed, id) {
				owed = append(owed, id)
			}
		}
	}
	if len(owed) == 0 && !damaged {
		return nil
	}
	found := map[string]ipc.Redelivery{}
	var unreadable []string
	for _, record := range records {
		if record.Type != "mailbox_message" || record.Metadata["direction"] != "receive" || !inChain(record) {
			continue
		}
		var message ipc.Message
		if err := r.Artifacts.ReadJSON(record.ID, &message); err != nil {
			unreadable = append(unreadable, record.ID)
			continue
		}
		if damaged && statedir.Newer(record.ID, targetID) && !slices.Contains(target.MailboxSeen, message.ID) && !slices.Contains(owed, message.ID) {
			owed = append(owed, message.ID)
		}
		// A message delivered twice has two records; records come oldest
		// first, and the first names the original delivery.
		if _, ok := found[message.ID]; !ok && slices.Contains(owed, message.ID) {
			found[message.ID] = ipc.Redelivery{Message: message, Record: record.ID}
		}
	}
	var back []ipc.Redelivery
	for _, id := range owed {
		item, ok := found[id]
		if !ok {
			r.recordUnrestored(pid, "ipc.redelivery_failed", map[string]string{"id": id, "reason": "no readable mailbox_message record", "unreadable": strings.Join(unreadable, ",")})
			continue
		}
		back = append(back, item)
	}
	if damaged && len(unreadable) > 0 {
		r.recordUnrestored(pid, "ipc.redelivery_failed", map[string]string{"reason": "a checkpoint and these mailbox_message records are unreadable, so mail only they knew of cannot be named", "unreadable": strings.Join(unreadable, ",")})
	}
	_, err := r.Bus.Redeliver(pid, back, "rollback")
	return err
}

// subscriptionNotes tells a session of the chain's subscriptions its history
// does not show: one line each, unless it holds that line from an earlier
// rollback. A subscription outlives the process that made it, so this covers
// one made after the last checkpoint as well as one rolled back past.
func (r *AgentRuntime) subscriptionNotes(pid string, target Session, records []artifact.Artifact) []Message {
	topics, err := r.Bus.Subscriptions(pid)
	if err != nil {
		r.recordUnrestored(pid, "ipc.redelivery_failed", map[string]string{"reason": "subscriptions: " + err.Error()})
		return nil
	}
	var notes []Message
	for _, topic := range topics {
		shown := slices.ContainsFunc(records, func(record artifact.Artifact) bool {
			return record.Type == "mailbox_subscription" && record.Metadata["topic"] == topic && slices.Contains(target.MailboxArtifactIDs, record.ID)
		})
		if shown {
			continue
		}
		note := runtimeNote("notice", framing.SubscribedTo, framing.SubscribedTo.String()+" "+framing.Inline(topic))
		if !slices.ContainsFunc(target.Messages, func(message Message) bool { return message.Content == note.Content }) {
			notes = append(notes, note)
		}
	}
	return notes
}
