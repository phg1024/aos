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
// With a kernel bound, messages to unknown or exited processes are
// refused, and sends, deliveries, acknowledgements, refusals and transfers
// are logged as ipc.* kernel events.
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
	"aos/internal/kernel"
)

// ErrUnknownRecipient: the recipient is not a process the kernel knows.
var ErrUnknownRecipient = errors.New("message recipient is not a known process")

// ErrRecipientExited: the recipient has exited and will never read it.
var ErrRecipientExited = errors.New("message recipient has exited")

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
}

type busState struct {
	NextMessageID int `json:"next_message_id"`
}

type Bus struct {
	root   string
	kernel *kernel.Kernel
	mu     sync.Mutex
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

// Send queues body for to. A recipient the kernel does not know, or that
// has exited, is refused.
func (b *Bus) Send(from, to, body string) (Message, error) {
	var sent Message
	err := b.update(func(t *tx) error {
		if err := b.deliverable(to); err != nil {
			b.record(from, "ipc.rejected", map[string]string{"to": to, "reason": err.Error()})
			return err
		}
		sent = t.enqueue(Message{From: from, To: to, Body: body})
		return nil
	})
	if err != nil {
		return Message{}, err
	}
	b.record(from, "ipc.sent", map[string]string{"id": sent.ID, "to": sent.To})
	return sent, nil
}

// Subscribe adds pid to topic's subscribers.
func (b *Bus) Subscribe(pid, topic string) error {
	return b.update(func(t *tx) error {
		if !slices.Contains(t.subscriptions[topic], pid) {
			t.subscriptions[topic] = append(t.subscriptions[topic], pid)
			t.changed = true
		}
		return nil
	})
}

// Unsubscribe removes pid from every topic, as when it exits.
func (b *Bus) Unsubscribe(pid string) error {
	return b.update(func(t *tx) error {
		for topic, pids := range t.subscriptions {
			if kept := slices.DeleteFunc(slices.Clone(pids), func(p string) bool { return p == pid }); len(kept) != len(pids) {
				t.setSubscribers(topic, kept)
			}
		}
		return nil
	})
}

// Publish queues body for every live subscriber of topic. Subscribers that
// have exited, or that the kernel does not know, are dropped.
func (b *Bus) Publish(from, topic, body string) ([]Message, error) {
	var created []Message
	err := b.update(func(t *tx) error {
		var live []string
		for _, pid := range t.subscriptions[topic] {
			if b.deliverable(pid) != nil {
				continue
			}
			live = append(live, pid)
			created = append(created, t.enqueue(Message{From: from, To: pid, Topic: topic, Body: body}))
		}
		if len(live) != len(t.subscriptions[topic]) {
			t.setSubscribers(topic, live)
		}
		return nil
	})
	for _, message := range created {
		b.record(from, "ipc.sent", map[string]string{"id": message.ID, "to": message.To, "topic": topic})
	}
	return created, err
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
// to toPID, as Recover does for the process it replaces.
func (b *Bus) Transfer(fromPID, toPID string) (int, error) {
	moved := 0
	err := b.update(func(t *tx) error {
		for _, message := range t.mailboxes[fromPID] {
			message.To = toPID
			t.mailboxes[toPID] = append(t.mailboxes[toPID], message)
			moved++
		}
		if moved > 0 {
			t.setMailbox(fromPID, nil)
		}
		for topic, pids := range t.subscriptions {
			if i := slices.Index(pids, fromPID); i >= 0 {
				next := slices.Clone(pids)
				next[i] = toPID
				t.setSubscribers(topic, slices.Compact(next))
			}
		}
		return nil
	})
	if moved > 0 {
		b.record(toPID, "ipc.transferred", map[string]string{"from_pid": fromPID, "count": strconv.Itoa(moved)})
	}
	return moved, err
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

// deliverable refuses a recipient that cannot read its mail.
func (b *Bus) deliverable(pid string) error {
	if b.kernel == nil {
		return nil
	}
	process, err := b.kernel.Get(pid)
	if errors.Is(err, kernel.ErrProcessNotFound) {
		return fmt.Errorf("%w: %s", ErrUnknownRecipient, pid)
	}
	if err != nil {
		return err
	}
	if process.State == kernel.StateExited {
		return fmt.Errorf("%w: %s", ErrRecipientExited, pid)
	}
	return nil
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
			if n, err := strconv.Atoi(strings.TrimPrefix(message.ID, "msg-")); err == nil && n >= t.state.NextMessageID {
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
