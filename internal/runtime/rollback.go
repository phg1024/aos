package runtime

import (
	"slices"
	"strings"

	"aos/internal/artifact"
	"aos/internal/framing"
	"aos/internal/ipc"
	"aos/internal/statedir"
)

// restoreRolledBack makes a successor's start true to what its chain was
// given, when the checkpoint it starts from is not the chain's latest. Mail
// in the latest checkpoint and not in this one was acknowledged off the bus,
// and this session never held it: it goes back in pid's mailbox from its
// mailbox_message record, under its own ID, so the session takes it and
// anything holding it already drops the repeat. Subscriptions stay with the
// chain, and the returned notes tell the session about those its history
// does not show.
//
// It looks at the chain's records, not at whether this Recover asked for a
// rollback, so a successor that died before its first checkpoint hands on
// what it was still owed: its exit names the earlier checkpoint as if nothing
// had been rolled back.
//
// A message with no readable record cannot go back, and that is recorded as
// ipc.redelivery_failed rather than stopping every later Recover of the chain.
func (r *AgentRuntime) restoreRolledBack(pid string, lineage []string, targetID string, target Session) ([]Message, error) {
	if r.Bus == nil {
		return nil, nil
	}
	records, err := r.Artifacts.List()
	if err != nil {
		return nil, err
	}
	chain := func(record artifact.Artifact) bool { return slices.Contains(lineage, record.ProducerPID) }
	latestID := targetID
	for _, record := range records {
		if record.Type == "checkpoint" && chain(record) && statedir.Newer(record.ID, latestID) {
			latestID = record.ID
		}
	}
	if latestID == targetID {
		return nil, nil
	}
	var latest Session
	if err := r.Artifacts.ReadJSON(latestID, &latest); err != nil {
		return nil, err
	}
	var owed []string
	for _, id := range latest.MailboxSeen {
		if !slices.Contains(target.MailboxSeen, id) {
			owed = append(owed, id)
		}
	}
	if len(owed) > 0 {
		if err := r.redeliver(pid, owed, records, chain); err != nil {
			return nil, err
		}
	}
	return r.subscriptionNotes(pid, target, records)
}

// redeliver finds each owed message's record and puts them back.
func (r *AgentRuntime) redeliver(pid string, owed []string, records []artifact.Artifact, chain func(artifact.Artifact) bool) error {
	found := map[string]ipc.Redelivery{}
	var unreadable []string
	for _, record := range records {
		if record.Type != "mailbox_message" || record.Metadata["direction"] != "receive" || !chain(record) {
			continue
		}
		var message ipc.Message
		if err := r.Artifacts.ReadJSON(record.ID, &message); err != nil {
			unreadable = append(unreadable, record.ID)
			continue
		}
		if !slices.Contains(owed, message.ID) {
			continue
		}
		// A message delivered twice has two records; its first names the
		// original delivery.
		if earlier, ok := found[message.ID]; !ok || statedir.Newer(earlier.Record, record.ID) {
			found[message.ID] = ipc.Redelivery{Message: message, Record: record.ID}
		}
	}
	slices.Sort(unreadable)
	var back []ipc.Redelivery
	for _, id := range owed {
		item, ok := found[id]
		if !ok {
			data := map[string]string{"id": id, "cause": "rollback", "reason": "no readable mailbox_message record", "unreadable": strings.Join(unreadable, ",")}
			if _, err := r.Kernel.RecordEvent(pid, "ipc.redelivery_failed", data); err != nil {
				return err
			}
			continue
		}
		back = append(back, item)
	}
	_, err := r.Bus.Redeliver(pid, back, "rollback")
	return err
}

// subscriptionNotes tells a session of the chain's subscriptions its history
// does not show: one line each, unless it holds that line from an earlier
// rollback.
func (r *AgentRuntime) subscriptionNotes(pid string, target Session, records []artifact.Artifact) ([]Message, error) {
	topics, err := r.Bus.Subscriptions(pid)
	if err != nil {
		return nil, err
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
	return notes, nil
}
