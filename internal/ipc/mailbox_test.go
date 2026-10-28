package ipc_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aos/internal/ipc"
	"aos/internal/kernel"
)

func newBus(t *testing.T) (*ipc.Bus, *kernel.Kernel, string) {
	t.Helper()
	root := t.TempDir()
	k, err := kernel.New(filepath.Join(root, "kernel"))
	if err != nil {
		t.Fatal(err)
	}
	bus, err := ipc.New(filepath.Join(root, "bus"), k)
	if err != nil {
		t.Fatal(err)
	}
	return bus, k, root
}

func spawn(t *testing.T, k *kernel.Kernel) string {
	t.Helper()
	process, err := k.Spawn(kernel.ProcessSpec{Role: "agent:worker", Metadata: map[string]string{"agent_id": "worker"}})
	if err != nil {
		t.Fatal(err)
	}
	return process.PID
}

func TestSendReceiveAndPublishSubscribe(t *testing.T) {
	bus, k, _ := newBus(t)
	a, b, c := spawn(t, k), spawn(t, k), spawn(t, k)
	if _, err := bus.Send(a, b, "hello"); err != nil {
		t.Fatalf("send: %v", err)
	}
	if err := bus.Subscribe(c, "artifact.created"); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if _, err := bus.Publish(a, "artifact.created", "art-1"); err != nil {
		t.Fatalf("publish: %v", err)
	}
	inboxB, err := bus.Receive(b, false)
	if err != nil || len(inboxB) != 1 || inboxB[0].Body != "hello" || inboxB[0].Deliveries != 1 {
		t.Fatalf("direct message %#v, %v", inboxB, err)
	}
	inboxC, err := bus.Receive(c, false)
	if err != nil || len(inboxC) != 1 || inboxC[0].Topic != "artifact.created" {
		t.Fatalf("published message %#v, %v", inboxC, err)
	}
}

// A received message stays until acknowledged: it is not handed out again
// to the same run, but is redelivered to one that starts over, and gone
// once acknowledged.
func TestDeliveryIsAtLeastOnceUntilAcknowledged(t *testing.T) {
	bus, k, _ := newBus(t)
	a, b := spawn(t, k), spawn(t, k)
	sent, _ := bus.Send(a, b, "do the thing")
	if first, _ := bus.Receive(b, false); len(first) != 1 {
		t.Fatalf("first receive = %v", first)
	}
	if again, _ := bus.Receive(b, false); len(again) != 0 {
		t.Fatalf("a delivered message is not handed out twice to one run: %v", again)
	}
	redelivered, _ := bus.Receive(b, true)
	if len(redelivered) != 1 || redelivered[0].ID != sent.ID || redelivered[0].Deliveries != 2 {
		t.Fatalf("redelivery = %+v", redelivered)
	}
	if err := bus.Ack(b, []string{sent.ID, "msg-999999"}); err != nil {
		t.Fatal(err)
	}
	if after, _ := bus.Receive(b, true); len(after) != 0 {
		t.Fatalf("an acknowledged message came back: %v", after)
	}
	events, _ := k.Events()
	var kinds []string
	for _, event := range events {
		if strings.HasPrefix(event.Type, "ipc.") {
			kinds = append(kinds, event.Type)
		}
	}
	if strings.Join(kinds, ",") != "ipc.sent,ipc.delivered,ipc.redelivered,ipc.acked" {
		t.Fatalf("events = %v", kinds)
	}
}

func TestMailToUnknownOrExitedProcessesIsRefused(t *testing.T) {
	bus, k, _ := newBus(t)
	a, b := spawn(t, k), spawn(t, k)
	if _, err := bus.Send(a, "pid-999999", "hi"); !errors.Is(err, ipc.ErrUnknownRecipient) {
		t.Fatalf("unknown recipient: %v", err)
	}
	bus.Subscribe(b, "news")
	k.Exit(b, kernel.ExitStatus{Code: 0, Reason: "done"})
	if _, err := bus.Send(a, b, "hi"); !errors.Is(err, ipc.ErrRecipientExited) {
		t.Fatalf("exited recipient: %v", err)
	}
	if published, err := bus.Publish(a, "news", "x"); err != nil || len(published) != 0 {
		t.Fatalf("publish to an exited subscriber: %v %v", published, err)
	}
	pending, _ := bus.Pending()
	if len(pending) != 0 {
		t.Fatalf("refused mail was kept: %v", pending)
	}
	events, _ := k.Events()
	rejected := 0
	for _, event := range events {
		if event.Type == "ipc.rejected" {
			rejected++
		}
	}
	if rejected != 2 {
		t.Fatalf("rejections logged = %d", rejected)
	}
}

func TestTransferAndUnsubscribe(t *testing.T) {
	bus, k, _ := newBus(t)
	a, old, other := spawn(t, k), spawn(t, k), spawn(t, k)
	bus.Subscribe(old, "news")
	bus.Subscribe(other, "news")
	bus.Send(a, old, "one")
	bus.Send(a, old, "two")
	bus.Receive(old, false) // "one" and "two" in flight
	k.Exit(old, kernel.ExitStatus{Code: 1, Reason: "crashed", Retryable: true})
	recovered := spawn(t, k)
	if moved, err := bus.Transfer(old, recovered); err != nil || moved != 2 {
		t.Fatalf("transfer = %d, %v", moved, err)
	}
	inbox, _ := bus.Receive(recovered, true)
	if len(inbox) != 2 || inbox[0].To != recovered || inbox[0].Body != "one" {
		t.Fatalf("recovered inbox = %+v", inbox)
	}
	published, _ := bus.Publish(a, "news", "x")
	if len(published) != 2 {
		t.Fatalf("the recovered process should inherit the subscription: %+v", published)
	}
	bus.Unsubscribe(recovered)
	if published, _ := bus.Publish(a, "news", "y"); len(published) != 1 || published[0].To != other {
		t.Fatalf("after unsubscribe: %+v", published)
	}
}

// Mailboxes written before acknowledgements (no deliveries field) load as
// pending, and IDs continue past the highest one present.
func TestLegacyMailboxesLoad(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "bus"), 0o755)
	os.WriteFile(filepath.Join(root, "bus", "mailboxes.json"), []byte(`{"pid-2":[{"id":"msg-000007","from":"pid-1","to":"pid-2","body":"old","timestamp":"2026-01-01T00:00:00Z"}]}`), 0o644)
	os.WriteFile(filepath.Join(root, "bus", "state.json"), []byte(`{"next_message_id":3}`), 0o644)
	bus, _ := ipc.New(filepath.Join(root, "bus"), nil)
	inbox, err := bus.Receive("pid-2", false)
	if err != nil || len(inbox) != 1 || inbox[0].Body != "old" {
		t.Fatalf("legacy inbox = %+v, %v", inbox, err)
	}
	next, _ := bus.Send("pid-1", "pid-2", "new")
	if next.ID != "msg-000008" {
		t.Fatalf("next id = %s", next.ID)
	}
}
