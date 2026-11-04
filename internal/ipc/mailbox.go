// Package ipc is the mailbox bus agents message each other through: direct
// sends, and publishes to the subscribers of a topic. Its files live in one
// directory, read and written under a file lock and replaced atomically,
// so buses in separate processes sharing it never lose or duplicate a
// message.
//
// Delivery is at least once. Receive marks a process's messages delivered
// but keeps them; the receiver acknowledges them (Ack) only once a
// checkpoint records them, and until then they are delivered again, after
// a crash or a resume. Every message has a stable ID, so a receiver that
// records the IDs it has seen can drop the repeats and see each message
// exactly once.
//
// With a kernel bound, a PID names its process chain (superseded_by), and
// mail and subscriptions belong to the chain. A message goes to the chain's
// last process: forwarded if the PID was superseded, held in the mailbox if
// that process exited retryably and may yet be recovered, and refused if the
// chain has ended or the process is unknown. Sends, deliveries,
// acknowledgements, refusals, transfers and subscriptions are logged as
// ipc.* kernel events.
//
// Forwarding reads the kernel under the bus's lock, and Transfer takes that
// lock after Supersede, so a send racing a recovery either sees the successor
// or lands in time to be moved. The kernel's lock may be taken inside the
// bus's, and never the reverse, so the two cannot deadlock.
package ipc

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"aos/internal/filelock"
	"aos/internal/framing"
	"aos/internal/kernel"
)

// ErrUnknownRecipient: the recipient is not a process the kernel knows.
var ErrUnknownRecipient = errors.New("message recipient is not a known process")

// ErrRecipientExited: the recipient has exited and will never read it.
var ErrRecipientExited = errors.New("message recipient has exited")

// MaxTopicBytes is the longest topic name.
const MaxTopicBytes = 128

// ErrInvalidTopic: a topic reaches its receivers' message headers, so it must
// be one plain line, of at most MaxTopicBytes.
var ErrInvalidTopic = errors.New("a topic must be one plain line of at most 128 bytes, with no control or invisible characters")

// checkTopic refuses a topic that could not go in a header as it is.
func checkTopic(topic string) error {
	if len(topic) > MaxTopicBytes || !framing.PlainLine(topic) {
		shown := topic
		if len(shown) > 64 {
			shown = shown[:64]
		}
		return fmt.Errorf("%w: %q", ErrInvalidTopic, shown)
	}
	return nil
}

// OperatorSender is the sender of mail from whoever runs AOS: it reaches
// the agent as a user turn. Only SendAsOperator sends as it.
const OperatorSender = "user"

// ErrReservedSender: only SendAsOperator may send as OperatorSender.
var ErrReservedSender = errors.New("the sender name is reserved for the operator")

type Message struct {
	ID        string    `json:"id"`
	From      string    `json:"from"`
	To        string    `json:"to"`
	Topic     string    `json:"topic,omitempty"`
	Body      string    `json:"body"`
	Timestamp time.Time `json:"timestamp"`
	// Deliveries counts how often Receive handed the message out without
	// it being acknowledged; 0 means pending. Mailboxes written before
	// acknowledgements lack it, so their messages load as pending.
	Deliveries  int        `json:"deliveries,omitempty"`
	DeliveredAt *time.Time `json:"delivered_at,omitempty"`
	// Held is set on what Send and Publish return, for the sender: see
	// HeldMeaning. It is about that moment, so it is not stored.
	Held bool `json:"-"`
}

// HeldMeaning is what a sender is told when a message went to a process that
// exited retryably. It is not a delivery: nothing says a successor will come,
// and until that chain ends nobody will say otherwise.
const HeldMeaning = "the process is not running and may never read this"

type busState struct {
	NextMessageID int `json:"next_message_id"`
}

type Bus struct {
	root   string
	kernel *kernel.Kernel
	// NoSubscriptionEvents leaves out ipc.subscribed and ipc.unsubscribed, for
	// replaying a record made before them.
	NoSubscriptionEvents bool
	mu                   sync.Mutex
}

// Why a subscription came or went, on its ipc.subscribed or ipc.unsubscribed
// event.
const (
	CauseSubscribe  = "subscribe"
	CauseTransfer   = "transfer"
	CauseChainEnded = "chain_ended"
)

// Redelivery is a message going back in a mailbox, and the mailbox_message
// artifact it came from.
type Redelivery struct {
	Message Message
	Record  string
}

// tx is one locked read-modify-write of the bus files.
type tx struct {
	state         busState
	mailboxes     map[string][]Message
	subscriptions map[string][]string
	changed       bool
}

// New opens the bus in root. k, if not nil, is the kernel whose processes
// send and receive: it validates recipients and receives the ipc.* events.
func New(root string, k *kernel.Kernel) (*Bus, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return &Bus{root: root, kernel: k}, nil
}

// Send queues body for to, which may be any PID of a process chain. The
// returned message names the process it reached. An unknown process, or a
// chain that has ended, is refused.
func (b *Bus) Send(from, to, body string) (Message, error) {
	if from == OperatorSender {
		return Message{}, fmt.Errorf("%w: %s", ErrReservedSender, from)
	}
	return b.send(from, to, body)
}

// SendAsOperator queues body for to, from the operator. It is for a program
// that holds the state directory, such as aos-tui, and so already has the
// operator's reach; the runtime, which sends for agents, never calls it.
func (b *Bus) SendAsOperator(to, body string) (Message, error) {
	return b.send(OperatorSender, to, body)
}

func (b *Bus) send(from, to, body string) (Message, error) {
	var sent Message
	err := b.update(func(t *tx) error {
		end, err := b.chainEnd(to)
		if err != nil {
			b.record(from, "ipc.rejected", map[string]string{"to": to, "reason": err.Error()})
			return err
		}
		sent = t.enqueue(Message{From: from, To: end.PID, Body: body})
		sent.Held = end.State == kernel.StateExited
		return nil
	})
	if err != nil {
		return Message{}, err
	}
	if sent.To != to {
		b.record(from, "ipc.forwarded", map[string]string{"id": sent.ID, "from_pid": to, "to_pid": sent.To})
	}
	b.record(from, "ipc.sent", sentEvent(sent))
	return sent, nil
}

// Subscribe adds pid to topic's subscribers.
func (b *Bus) Subscribe(pid, topic string) error {
	if err := checkTopic(topic); err != nil {
		b.record(pid, "ipc.rejected", map[string]string{"reason": "invalid_topic"})
		return err
	}
	added := false
	err := b.update(func(t *tx) error {
		if !slices.Contains(t.subscriptions[topic], pid) {
			t.subscriptions[topic] = append(t.subscriptions[topic], pid)
			t.changed = true
			added = true
		}
		return nil
	})
	if added {
		b.recordSubscription("ipc.subscribed", pid, topic, CauseSubscribe)
	}
	return err
}

// Unsubscribe removes pid from every topic. A chain's subscriptions outlive
// its processes, so this is for the end of the chain (CauseChainEnded); a
// Recover moves them with Transfer instead.
func (b *Bus) Unsubscribe(pid, cause string) error {
	var left []string
	err := b.update(func(t *tx) error {
		for topic, pids := range t.subscriptions {
			if kept := slices.DeleteFunc(slices.Clone(pids), func(p string) bool { return p == pid }); len(kept) != len(pids) {
				t.setSubscribers(topic, kept)
				left = append(left, topic)
			}
		}
		return nil
	})
	slices.Sort(left)
	for _, topic := range left {
		b.recordSubscription("ipc.unsubscribed", pid, topic, cause)
	}
	return err
}

// Subscriptions lists the topics pid subscribes to, in order.
func (b *Bus) Subscriptions(pid string) ([]string, error) {
	var topics []string
	err := b.view(func(t *tx) error {
		for topic, pids := range t.subscriptions {
			if slices.Contains(pids, pid) {
				topics = append(topics, topic)
			}
		}
		return nil
	})
	slices.Sort(topics)
	return topics, err
}

// Publish queues body for the last process of every subscriber's chain, once
// per chain. One that exited retryably holds it for its successor. A
// subscriber whose chain has ended, or that the kernel does not know, is
// dropped; one whose chain is broken gets nothing and is kept.
func (b *Bus) Publish(from, topic, body string) ([]Message, error) {
	if from == OperatorSender {
		return nil, fmt.Errorf("%w: %s", ErrReservedSender, from)
	}
	if err := checkTopic(topic); err != nil {
		b.record(from, "ipc.rejected", map[string]string{"reason": "invalid_topic"})
		return nil, err
	}
	var created []Message
	var dropped []string
	var moved [][2]string
	err := b.update(func(t *tx) error {
		subscribers := t.subscriptions[topic]
		kept := make([]string, 0, len(subscribers))
		for _, pid := range subscribers {
			end, err := b.chainEnd(pid)
			switch {
			case errors.Is(err, ErrUnknownRecipient), errors.Is(err, ErrRecipientExited):
				dropped = append(dropped, pid)
				continue
			case errors.Is(err, kernel.ErrBrokenChain):
				b.record(from, "ipc.rejected", map[string]string{"to": pid, "topic": topic, "reason": err.Error()})
				kept = append(kept, pid)
				continue
			case err != nil:
				return err
			}
			if end.PID != pid {
				moved = append(moved, [2]string{pid, end.PID})
			}
			if slices.Contains(kept, end.PID) {
				continue
			}
			kept = append(kept, end.PID)
			message := t.enqueue(Message{From: from, To: end.PID, Topic: topic, Body: body})
			message.Held = end.State == kernel.StateExited
			created = append(created, message)
		}
		if !slices.Equal(kept, subscribers) {
			t.setSubscribers(topic, kept)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, pid := range dropped {
		b.recordSubscription("ipc.unsubscribed", pid, topic, CauseChainEnded)
	}
	for _, move := range moved {
		b.recordSubscription("ipc.unsubscribed", move[0], topic, CauseTransfer)
		b.recordSubscription("ipc.subscribed", move[1], topic, CauseTransfer)
	}
	for _, message := range created {
		data := sentEvent(message)
		data["topic"] = topic
		b.record(from, "ipc.sent", data)
	}
	return created, nil
}

// Receive hands out pid's messages and marks them delivered. They stay in
// the mailbox until acknowledged. With redeliver, messages already handed
// out are included again, as a receiver starting from a checkpoint needs;
// without it, only messages not yet delivered.
func (b *Bus) Receive(pid string, redeliver bool) ([]Message, error) {
	var out []Message
	now := time.Now().UTC()
	err := b.update(func(t *tx) error {
		for i, message := range t.mailboxes[pid] {
			if message.Deliveries > 0 && !redeliver {
				continue
			}
			message.Deliveries++
			message.DeliveredAt = &now
			t.mailboxes[pid][i] = message
			out = append(out, message)
			t.changed = true
		}
		return nil
	})
	for _, message := range out {
		eventType := "ipc.delivered"
		if message.Deliveries > 1 {
			eventType = "ipc.redelivered"
		}
		b.record(pid, eventType, map[string]string{"id": message.ID, "from": message.From, "deliveries": strconv.Itoa(message.Deliveries)})
	}
	return out, err
}

// Ack removes pid's messages with the given IDs: their receiver has
// recorded them durably. Unknown IDs are ignored.
func (b *Bus) Ack(pid string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	var acked []string
	err := b.update(func(t *tx) error {
		kept := t.mailboxes[pid][:0:0]
		for _, message := range t.mailboxes[pid] {
			if slices.Contains(ids, message.ID) {
				acked = append(acked, message.ID)
				continue
			}
			kept = append(kept, message)
		}
		if len(acked) > 0 {
			t.setMailbox(pid, kept)
		}
		return nil
	})
	if len(acked) > 0 {
		b.record(pid, "ipc.acked", map[string]string{"ids": strings.Join(acked, ",")})
	}
	return err
}

// Transfer moves every unacknowledged message and subscription of fromPID
// to toPID, as Recover does for the process it replaces. A message forwarded
// to toPID before this keeps its place by send order.
func (b *Bus) Transfer(fromPID, toPID string) (int, error) {
	moved := 0
	var topics []string
	err := b.update(func(t *tx) error {
		for _, message := range t.mailboxes[fromPID] {
			message.To = toPID
			t.mailboxes[toPID] = append(t.mailboxes[toPID], message)
			moved++
		}
		if moved > 0 {
			inSendOrder(t.mailboxes[toPID])
			t.setMailbox(fromPID, nil)
		}
		for topic, pids := range t.subscriptions {
			if i := slices.Index(pids, fromPID); i >= 0 {
				next := slices.Clone(pids)
				next[i] = toPID
				if slices.Contains(pids, toPID) {
					next = slices.Delete(next, i, i+1)
				}
				t.setSubscribers(topic, next)
				topics = append(topics, topic)
			}
		}
		return nil
	})
	if moved > 0 {
		b.record(toPID, "ipc.transferred", map[string]string{"from_pid": fromPID, "count": strconv.Itoa(moved)})
	}
	slices.Sort(topics)
	for _, topic := range topics {
		b.recordSubscription("ipc.unsubscribed", fromPID, topic, CauseTransfer)
		b.recordSubscription("ipc.subscribed", toPID, topic, CauseTransfer)
	}
	return moved, err
}

// Redeliver puts messages a rollback took back from pid's chain in pid's
// mailbox: same IDs, pending again, in send order, which puts them ahead of
// the mail that came after them. A message already there stays as it is, so
// a Recover repeated after a crash adds nothing twice. Each is logged before
// it is written, like a kernel state change: after a crash, the log may name
// a redelivery the next Recover repeats, and never misses one.
func (b *Bus) Redeliver(pid string, messages []Redelivery, cause string) (int, error) {
	if len(messages) == 0 {
		return 0, nil
	}
	added := 0
	err := b.update(func(t *tx) error {
		for _, item := range messages {
			if slices.ContainsFunc(t.mailboxes[pid], func(m Message) bool { return m.ID == item.Message.ID }) {
				continue
			}
			message := item.Message
			message.To, message.Deliveries, message.DeliveredAt = pid, 0, nil
			t.mailboxes[pid] = append(t.mailboxes[pid], message)
			b.record(pid, "ipc.redelivered", map[string]string{"id": message.ID, "from": message.From, "cause": cause, "record": item.Record})
			added++
		}
		if added > 0 {
			inSendOrder(t.mailboxes[pid])
			t.changed = true
		}
		return nil
	})
	return added, err
}

// Pending returns every unacknowledged message, by recipient.
func (b *Bus) Pending() (map[string][]Message, error) {
	var out map[string][]Message
	err := b.view(func(t *tx) error {
		out = t.mailboxes
		return nil
	})
	return out, err
}

func sentEvent(message Message) map[string]string {
	data := map[string]string{"id": message.ID, "to": message.To}
	if message.Held {
		data["held"] = "true"
	}
	return data
}

// chainEnd is the process mail to pid goes to: the last of pid's chain, live
// or holding it for a successor. Without a kernel, pid itself. It refuses an
// unknown process (ErrUnknownRecipient), a chain that has ended
// (ErrRecipientExited), and a broken chain (kernel.ErrBrokenChain).
func (b *Bus) chainEnd(pid string) (kernel.Process, error) {
	if b.kernel == nil {
		return kernel.Process{PID: pid}, nil
	}
	end, err := b.kernel.ChainEnd(pid)
	if errors.Is(err, kernel.ErrProcessNotFound) {
		return kernel.Process{}, fmt.Errorf("%w: %s", ErrUnknownRecipient, pid)
	}
	if err != nil {
		return kernel.Process{}, err
	}
	if end.ChainEnded() {
		return kernel.Process{}, fmt.Errorf("%w: %s", ErrRecipientExited, finalExit(pid, end))
	}
	return end, nil
}

// finalExit names where a chain ended, for the sender it refuses. The reason
// may be an agent's, so it is quoted and cut short.
func finalExit(pid string, end kernel.Process) string {
	where := end.PID
	if pid != end.PID {
		where = fmt.Sprintf("%s, whose chain ended at %s", pid, end.PID)
	}
	if end.ExitStatus == nil {
		return where
	}
	reason := end.ExitStatus.Reason
	if len(reason) > 200 {
		reason = strings.ToValidUTF8(reason[:200], "") + "..."
	}
	closed := ""
	if end.Closed {
		closed = ", closed"
	}
	return fmt.Sprintf("%s (exit code %d %q%s)", where, end.ExitStatus.Code, reason, closed)
}

// inSendOrder sorts a mailbox by ID. The bus gives out IDs under its lock, so
// that is the order the messages were sent in.
func inSendOrder(messages []Message) {
	slices.SortStableFunc(messages, func(a, b Message) int { return messageNumber(a.ID) - messageNumber(b.ID) })
}

func messageNumber(id string) int {
	n, err := strconv.Atoi(strings.TrimPrefix(id, "msg-"))
	if err != nil {
		return -1
	}
	return n
}

func (b *Bus) recordSubscription(eventType, pid, topic, cause string) {
	if !b.NoSubscriptionEvents {
		b.record(pid, eventType, map[string]string{"topic": topic, "pid": pid, "cause": cause})
	}
}

func (b *Bus) record(pid, eventType string, data map[string]string) {
	if b.kernel != nil {
		_, _ = b.kernel.RecordEvent(pid, eventType, data)
	}
}

func (t *tx) enqueue(message Message) Message {
	message.ID = fmt.Sprintf("msg-%06d", t.state.NextMessageID)
	t.state.NextMessageID++
	message.Timestamp = time.Now().UTC()
	t.mailboxes[message.To] = append(t.mailboxes[message.To], message)
	t.changed = true
	return message
}

func (t *tx) setMailbox(pid string, messages []Message) {
	if len(messages) == 0 {
		delete(t.mailboxes, pid)
	} else {
		t.mailboxes[pid] = messages
	}
	t.changed = true
}

func (t *tx) setSubscribers(topic string, pids []string) {
	if len(pids) == 0 {
		delete(t.subscriptions, topic)
	} else {
		t.subscriptions[topic] = pids
	}
	t.changed = true
}

func (b *Bus) statePath() string         { return filepath.Join(b.root, "state.json") }
func (b *Bus) mailboxesPath() string     { return filepath.Join(b.root, "mailboxes.json") }
func (b *Bus) subscriptionsPath() string { return filepath.Join(b.root, "subscriptions.json") }
func (b *Bus) lockPath() string          { return filepath.Join(b.root, "bus.lock") }

// update runs fn under the lock and writes the files back if it changed
// them: the ID counter first, so a crash between writes never reuses an
// ID. The kernel's lock may be taken inside (never the reverse).
func (b *Bus) update(fn func(*tx) error) error {
	return b.locked(func() error {
		t, err := b.load()
		if err != nil {
			return err
		}
		if err := fn(t); err != nil {
			return err
		}
		if !t.changed {
			return nil
		}
		if err := writeJSON(b.statePath(), t.state); err != nil {
			return err
		}
		if err := writeJSON(b.mailboxesPath(), t.mailboxes); err != nil {
			return err
		}
		return writeJSON(b.subscriptionsPath(), t.subscriptions)
	})
}

func (b *Bus) view(fn func(*tx) error) error {
	return b.locked(func() error {
		t, err := b.load()
		if err != nil {
			return err
		}
		return fn(t)
	})
}

func (b *Bus) locked(fn func() error) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	held, err := filelock.Acquire(b.lockPath())
	if err != nil {
		return err
	}
	defer held.Release()
	return fn()
}

func (b *Bus) load() (*tx, error) {
	t := &tx{state: busState{NextMessageID: 1}, mailboxes: map[string][]Message{}, subscriptions: map[string][]string{}}
	for path, target := range map[string]any{b.statePath(): &t.state, b.mailboxesPath(): &t.mailboxes, b.subscriptionsPath(): &t.subscriptions} {
		bytes, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(bytes, target); err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
		}
	}
	if t.mailboxes == nil {
		t.mailboxes = map[string][]Message{}
	}
	if t.subscriptions == nil {
		t.subscriptions = map[string][]string{}
	}
	// Never reuse an ID, even if the counter fell behind the mailboxes.
	for _, messages := range t.mailboxes {
		for _, message := range messages {
			if n := messageNumber(message.ID); n >= t.state.NextMessageID {
				t.state.NextMessageID = n + 1
			}
		}
	}
	if t.state.NextMessageID < 1 {
		t.state.NextMessageID = 1
	}
	return t, nil
}

// writeJSON replaces path through a synced temp file, so a reader or a
// crash never sees a partial file.
func writeJSON(path string, value any) error {
	bytes, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	_, err = tmp.Write(bytes)
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Chmod(tmpPath, 0o644)
	}
	if err == nil {
		err = os.Rename(tmpPath, path)
	}
	if err != nil {
		os.Remove(tmpPath)
	}
	return err
}
