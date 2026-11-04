package runtime

import (
	"fmt"
	"slices"
	"strings"

	"aos/internal/framing"
	"aos/internal/ipc"
	"aos/internal/kernel"
)

// maxNoticeIDs is how many message IDs one undelivered notice lists.
const maxNoticeIDs = 20

func unshown(session Session, id string) bool {
	return slices.ContainsFunc(session.MailboxUnshown, func(mail UnshownMail) bool { return mail.ID == id })
}

// markShown is what is left unshown after a model call returned: every message
// the request held the body of is shown, which leaves those compaction
// archived. A call that failed or never returned shows nothing: no agent acted
// on it, and the next one sends the same history.
func markShown(mail []UnshownMail) []UnshownMail {
	var left []UnshownMail
	for _, item := range mail {
		if item.Segment != "" {
			left = append(left, item)
		}
	}
	return left
}

// bodiesRead brings archived mail back into what the next request holds, when a
// read_memory result has its whole body. A body split across pages stays
// unshown, as does one a search only quoted: consumed means the agent could act
// on the content. An unreadable segment leaves it unshown too.
func (r *AgentRuntime) bodiesRead(mail []UnshownMail, result string) []UnshownMail {
	out := slices.Clone(mail)
	for i, item := range out {
		if item.Segment == "" {
			continue
		}
		var segment memorySegment
		if r.Artifacts.ReadJSON(item.Segment, &segment) != nil {
			continue
		}
		at := slices.IndexFunc(segment.Messages, func(m Message) bool { return m.Mail == item.ID })
		// As the segment's text has it, so a short body met in passing does not
		// count.
		if at >= 0 && strings.Contains(result, segment.Messages[at].Role+": "+segment.Messages[at].Content) {
			out[i].Segment, out[i].Seq = "", 0
		}
	}
	return out
}

// noteArchivedUnread adds a line for each message compaction just moved out of
// the history before any model had its body: all that is left in the request
// is the segment's reference, which says nothing about mail.
func (r *AgentRuntime) noteArchivedUnread(pid string, session Session, older []Message, segment string) Session {
	for _, message := range older {
		if message.Mail == "" {
			continue
		}
		i := slices.IndexFunc(session.MailboxUnshown, func(mail UnshownMail) bool { return mail.ID == message.Mail })
		if i < 0 {
			continue
		}
		session.MailboxUnshown = slices.Clone(session.MailboxUnshown)
		session.MailboxUnshown[i].Segment, session.MailboxUnshown[i].Seq = segment, message.Seq
		if r.Bus != nil {
			_ = r.Bus.MarkArchived(pid, message.Mail, segment)
		}
		note := fmt.Sprintf("%s %s from %s, seq %d in %s; read_memory to read it", framing.ArchivedUnread, message.Mail, r.senderLabel(session.MailboxUnshown[i].From), message.Seq, segment)
		session.Messages = append(session.Messages, runtimeNote("notice", framing.ArchivedUnread, note))
	}
	return numberMessages(session)
}

// undeliveredNotes sweeps up any chain that ended with nobody there to do it,
// then returns a line for each chain that ended with this process's chain's
// mail unread: one per ended chain, at most maxNoticeIDs IDs. It reads the
// kernel's log, not the bus, so a sender in another OS process hears the same,
// and they are not mail: no bound applies, and nothing can bounce them. Mail a
// sender did not address, through a topic, and the operator's, get the event
// alone. A failure to read is tried again at the next step.
func (r *AgentRuntime) undeliveredNotes(pid string, session *Session) []Message {
	if r.NoSenderOutcomes {
		return nil
	}
	_ = r.Bus.Sweep()
	events, latest, err := r.Bus.DeadLetters(session.NoticesSeen)
	if err != nil || latest == session.NoticesSeen {
		return nil
	}
	lineage, err := r.lineage(pid)
	if err != nil {
		return nil
	}
	session.NoticesSeen = latest
	var chains []string
	byChain := map[string][]kernel.Event{}
	for _, event := range events {
		if !slices.Contains(lineage, event.Data["from"]) || event.Data["topic"] != "" {
			continue
		}
		to := event.Data["to"]
		if _, ok := byChain[to]; !ok {
			chains = append(chains, to)
		}
		byChain[to] = append(byChain[to], event)
	}
	var notes []Message
	for _, chain := range chains {
		notes = append(notes, runtimeNote("notice", framing.Undelivered, undeliveredNote(byChain[chain])))
	}
	return notes
}

// undeliveredNote is the line for one ended chain's dead letters. The exit
// reason is an agent's, so it is cut short and quoted, which keeps it on the
// one line. It says what is known: no record that a model read them, which is
// not to say none did.
func undeliveredNote(events []kernel.Event) string {
	var ids []string
	for _, event := range events[:min(len(events), maxNoticeIDs)] {
		ids = append(ids, framing.Inline(event.Data["id"]))
	}
	list := strings.Join(ids, ", ")
	if more := len(events) - len(ids); more > 0 {
		list += fmt.Sprintf(" and %d more", more)
	}
	first := events[0].Data
	recipient := framing.Inline(first["to"])
	if first["agent"] != "" {
		recipient = framing.Inline(fmt.Sprintf("%s (%s)", first["agent"], first["to"]))
	}
	closed := ""
	if first["closed"] == "true" {
		closed = ", closed"
	}
	return fmt.Sprintf("%s %s to %s: no record that a model read this; its chain ended with exit code %s %q%s",
		framing.Undelivered, list, recipient, framing.Inline(first["exit_code"]), ipc.CutReason(first["exit_reason"]), closed)
}
