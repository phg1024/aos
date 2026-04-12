package ipc

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Message struct {
	ID        string    `json:"id"`
	From      string    `json:"from"`
	To        string    `json:"to"`
	Topic     string    `json:"topic,omitempty"`
	Body      string    `json:"body"`
	Timestamp time.Time `json:"timestamp"`
}

type busState struct {
	NextMessageID int `json:"next_message_id"`
}

type Bus struct {
	root string
	mu   sync.Mutex
}

func New(root string) (*Bus, error) {
	bus := &Bus{root: root}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	for _, path := range []string{bus.statePath(), bus.mailboxesPath(), bus.subscriptionsPath()} {
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			initial := []byte(`{}`)
			if path == bus.statePath() {
				initial = []byte(`{"next_message_id":1}`)
			}
			if err := os.WriteFile(path, initial, 0o644); err != nil {
				return nil, err
			}
		}
	}
	return bus, nil
}

func (b *Bus) Send(from, to, body string) (Message, error) {
	return b.enqueue(Message{
		From: from,
		To:   to,
		Body: body,
	})
}

func (b *Bus) Subscribe(pid, topic string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	subscriptions, err := b.readSubscriptions()
	if err != nil {
		return err
	}
	list := subscriptions[topic]
	for _, existing := range list {
		if existing == pid {
			return nil
		}
	}
	subscriptions[topic] = append(list, pid)
	return b.writeJSON(b.subscriptionsPath(), subscriptions)
}

func (b *Bus) Publish(from, topic, body string) ([]Message, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	subscriptions, err := b.readSubscriptions()
	if err != nil {
		return nil, err
	}
	mailboxes, err := b.readMailboxes()
	if err != nil {
		return nil, err
	}
	state, err := b.readState()
	if err != nil {
		return nil, err
	}

	var created []Message
	for _, pid := range subscriptions[topic] {
		message := Message{
			ID:        fmt.Sprintf("msg-%06d", state.NextMessageID),
			From:      from,
			To:        pid,
			Topic:     topic,
			Body:      body,
			Timestamp: time.Now().UTC(),
		}
		state.NextMessageID++
		mailboxes[pid] = append(mailboxes[pid], message)
		created = append(created, message)
	}

	if err := b.writeJSON(b.statePath(), state); err != nil {
		return nil, err
	}
	if err := b.writeJSON(b.mailboxesPath(), mailboxes); err != nil {
		return nil, err
	}
	return created, nil
}

func (b *Bus) ReceiveAll(pid string) ([]Message, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	mailboxes, err := b.readMailboxes()
	if err != nil {
		return nil, err
	}
	messages := append([]Message(nil), mailboxes[pid]...)
	delete(mailboxes, pid)
	if err := b.writeJSON(b.mailboxesPath(), mailboxes); err != nil {
		return nil, err
	}
	return messages, nil
}

func (b *Bus) enqueue(message Message) (Message, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	mailboxes, err := b.readMailboxes()
	if err != nil {
		return Message{}, err
	}
	state, err := b.readState()
	if err != nil {
		return Message{}, err
	}
	message.ID = fmt.Sprintf("msg-%06d", state.NextMessageID)
	state.NextMessageID++
	message.Timestamp = time.Now().UTC()
	mailboxes[message.To] = append(mailboxes[message.To], message)
	if err := b.writeJSON(b.statePath(), state); err != nil {
		return Message{}, err
	}
	if err := b.writeJSON(b.mailboxesPath(), mailboxes); err != nil {
		return Message{}, err
	}
	return message, nil
}

func (b *Bus) statePath() string         { return filepath.Join(b.root, "state.json") }
func (b *Bus) mailboxesPath() string     { return filepath.Join(b.root, "mailboxes.json") }
func (b *Bus) subscriptionsPath() string { return filepath.Join(b.root, "subscriptions.json") }

func (b *Bus) readState() (busState, error) {
	var state busState
	err := b.readJSON(b.statePath(), &state)
	return state, err
}

func (b *Bus) readMailboxes() (map[string][]Message, error) {
	var mailboxes map[string][]Message
	err := b.readJSON(b.mailboxesPath(), &mailboxes)
	return mailboxes, err
}

func (b *Bus) readSubscriptions() (map[string][]string, error) {
	var subscriptions map[string][]string
	err := b.readJSON(b.subscriptionsPath(), &subscriptions)
	return subscriptions, err
}

func (b *Bus) readJSON(path string, target any) error {
	bytes, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(bytes, target)
}

func (b *Bus) writeJSON(path string, value any) error {
	bytes, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, bytes, 0o644)
}
