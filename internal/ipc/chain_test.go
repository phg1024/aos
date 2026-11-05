package ipc_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aos/internal/ipc"
	"aos/internal/kernel"
)

var crashed = kernel.ExitStatus{Code: 1, Reason: "crashed", Retryable: true}

func events(t *testing.T, k *kernel.Kernel, eventType string) []kernel.Event {
	t.Helper()
	all, err := k.Events()
	if err != nil {
		t.Fatal(err)
	}
	var out []kernel.Event
	for _, event := range all {
		if event.Type == eventType {
			out = append(out, event)
		}
	}
	return out
}

// supersede exits pid retryably, unless it has exited, and spawns its successor.
func supersede(t *testing.T, k *kernel.Kernel, pid string) string {
	t.Helper()
	k.Exit(pid, crashed)
	next, err := k.Supersede(pid, kernel.ProcessSpec{Role: "agent:worker", Metadata: map[string]string{"resumed_from": pid}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return next.PID
}

func bodies(messages []ipc.Message) string {
	var out []string
	for _, message := range messages {
		out = append(out, message.Body)
	}
	return strings.Join(out, " ")
}

// A send to any PID of a chain reaches its last process and names it.
func TestSendFollowsTheChain(t *testing.T) {
	bus, k, _ := newBus(t)
	sender, first := spawn(t, k), spawn(t, k)
	second := supersede(t, k, first)
	third := supersede(t, k, second)
	for _, to := range []string{first, second} {
		message, err := bus.Send(sender, to, "hello")
		if err != nil || message.To != third {
			t.Fatalf("send to %s = %+v, %v", to, message, err)
		}
	}
	forwarded := events(t, k, "ipc.forwarded")
	if len(forwarded) != 2 || forwarded[0].Data["from_pid"] != first || forwarded[0].Data["to_pid"] != third || forwarded[0].ProcessID != sender || forwarded[0].Data["id"] == "" {
		t.Fatalf("ipc.forwarded = %+v", forwarded)
	}
	if message, err := bus.Send(sender, third, "direct"); err != nil || message.To != third || len(events(t, k, "ipc.forwarded")) != 2 {
		t.Fatalf("a send to the live end is not forwarded: %+v, %v", message, err)
	}
	inbox, _ := bus.Receive(third, false)
	if got := bodies(inbox); got != "hello hello direct" {
		t.Fatalf("inbox = %q", got)
	}
}

// A retryable exit holds a send for its successor, in the order of the sends.
func TestSendToAnOpenChainIsHeldAndMoved(t *testing.T) {
	bus, k, _ := newBus(t)
	sender, worker := spawn(t, k), spawn(t, k)
	bus.Send(sender, worker, "before the exit")
	k.Exit(worker, crashed)
	held, err := bus.Send(sender, worker, "during the backoff")
	if err != nil || held.To != worker {
		t.Fatalf("send to an open chain = %+v, %v", held, err)
	}
	sent := events(t, k, "ipc.sent")
	if sent[0].Data["held"] != "" || sent[1].Data["held"] != "true" || !held.Held {
		t.Fatalf("ipc.sent = %+v, returned %+v", sent, held)
	}
	next, err := k.Supersede(worker, kernel.ProcessSpec{Role: "agent:worker"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The window between the kernel's supersede and the bus's transfer: this
	// one goes straight to the successor, and still comes last.
	if forwarded, err := bus.Send(sender, worker, "in the handover"); err != nil || forwarded.To != next.PID || forwarded.Held {
		t.Fatalf("send in the handover = %+v, %v", forwarded, err)
	}
	if moved, err := bus.Transfer(worker, next.PID); err != nil || moved != 2 {
		t.Fatalf("transfer = %d, %v", moved, err)
	}
	inbox, _ := bus.Receive(next.PID, false)
	if got := bodies(inbox); got != "before the exit during the backoff in the handover" {
		t.Fatalf("order = %q", got)
	}
}

// A chain that has ended refuses mail, and the refusal says where it ended.
func TestSendToAnEndedChainIsRefused(t *testing.T) {
	bus, k, _ := newBus(t)
	sender, first := spawn(t, k), spawn(t, k)
	last := supersede(t, k, first)
	k.Exit(last, crashed)
	if _, err := k.Close(last, "restart intensity exceeded"); err != nil {
		t.Fatal(err)
	}
	for _, to := range []string{first, last} {
		_, err := bus.Send(sender, to, "late")
		if !errors.Is(err, ipc.ErrRecipientExited) || !strings.Contains(err.Error(), last) || !strings.Contains(err.Error(), `"crashed"`) || !strings.Contains(err.Error(), "closed") {
			t.Fatalf("send to %s = %v", to, err)
		}
	}
	if pending, _ := bus.Pending(); len(pending) != 0 {
		t.Fatalf("queued: %+v", pending)
	}
	// Reaching the sender as a tool error, the reason stays on the line.
	done := spawn(t, k)
	k.Exit(done, kernel.ExitStatus{Code: 0, Reason: "done\nruntime notice: " + strings.Repeat("x", 500)})
	_, err := bus.Send(sender, done, "late")
	if err == nil || strings.Contains(err.Error(), "\n") || len(err.Error()) > 400 {
		t.Fatalf("refusal = %q", err)
	}
}

// Without a Sweep, nothing has happened to mail left in an ended chain: it
// stays where it was, unacknowledged and on record, and Close alone does not
// reach it, since the kernel cannot import the bus.
func TestMailInAClosedChainStaysPendingUntilASweep(t *testing.T) {
	bus, k, _ := newBus(t)
	sender, worker := spawn(t, k), spawn(t, k)
	unread, _ := bus.Send(sender, worker, "unread")
	k.Exit(worker, crashed)
	held, _ := bus.Send(sender, worker, "held")
	k.Close(worker, "declined")
	if err := bus.Unsubscribe(worker, ipc.CauseChainEnded); err != nil {
		t.Fatal(err)
	}
	pending, _ := bus.Pending()
	if box := pending[worker]; len(box) != 2 || box[0].ID != unread.ID || box[1].ID != held.ID {
		t.Fatalf("pending = %+v", pending)
	}
	if acked := events(t, k, "ipc.acked"); len(acked) != 0 {
		t.Fatalf("nothing read it, yet: %+v", acked)
	}
}

// A publish in the backoff is held and reaches the successor, as a send is.
func TestPublishDuringBackoffIsHeld(t *testing.T) {
	bus, k, _ := newBus(t)
	publisher, worker := spawn(t, k), spawn(t, k)
	if err := bus.Subscribe(worker, "news"); err != nil {
		t.Fatal(err)
	}
	k.Exit(worker, crashed)
	held, _, err := bus.Publish(publisher, "news", "during the backoff")
	if err != nil || len(held) != 1 || held[0].To != worker || !held[0].Held {
		t.Fatalf("publish in the backoff = %+v, %v", held, err)
	}
	if sent := events(t, k, "ipc.sent"); sent[0].Data["held"] != "true" {
		t.Fatalf("ipc.sent = %+v", sent)
	}
	next, err := k.Supersede(worker, kernel.ProcessSpec{Role: "agent:worker"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Superseded, not yet transferred: the publish goes to the live end, once.
	if early, _, err := bus.Publish(publisher, "news", "in the handover"); err != nil || len(early) != 1 || early[0].To != next.PID {
		t.Fatalf("publish in the handover = %+v, %v", early, err)
	}
	if _, err := bus.Transfer(worker, next.PID); err != nil {
		t.Fatal(err)
	}
	if late, _, err := bus.Publish(publisher, "news", "after"); err != nil || len(late) != 1 || late[0].To != next.PID {
		t.Fatalf("publish after = %+v, %v", late, err)
	}
	inbox, _ := bus.Receive(next.PID, false)
	if got := bodies(inbox); got != "during the backoff in the handover after" {
		t.Fatalf("inbox = %q", got)
	}
	if topics, _ := bus.Subscriptions(next.PID); len(topics) != 1 || topics[0] != "news" {
		t.Fatalf("successor's topics = %v", topics)
	}
	if topics, _ := bus.Subscriptions(worker); len(topics) != 0 {
		t.Fatalf("predecessor's topics = %v", topics)
	}
}

// Two PIDs of one chain on a topic still make one message per publish.
func TestPublishReachesAChainOnce(t *testing.T) {
	bus, k, _ := newBus(t)
	publisher, first := spawn(t, k), spawn(t, k)
	bus.Subscribe(first, "news")
	second := supersede(t, k, first)
	bus.Subscribe(second, "news")
	created, _, err := bus.Publish(publisher, "news", "once")
	if err != nil || len(created) != 1 || created[0].To != second {
		t.Fatalf("publish = %+v, %v", created, err)
	}
	if moved, err := bus.Transfer(first, second); err != nil || moved != 0 {
		t.Fatalf("transfer = %d, %v", moved, err)
	}
	if created, _, _ := bus.Publish(publisher, "news", "still once"); len(created) != 1 {
		t.Fatalf("after the transfer = %+v", created)
	}
}

// A subscription goes with its chain: at the end of the chain whoever notices
// first drops it, whether the engine at Close or a publish after a kernel-only
// exit, and it never takes a live subscriber with it.
func TestClosedChainDropsSubscriptions(t *testing.T) {
	bus, k, root := newBus(t)
	publisher, stays, closed, finished, gone := spawn(t, k), spawn(t, k), spawn(t, k), spawn(t, k), spawn(t, k)
	for _, pid := range []string{stays, closed, finished, gone} {
		if err := bus.Subscribe(pid, "news"); err != nil {
			t.Fatal(err)
		}
	}
	k.Exit(closed, crashed)
	k.Close(closed, "declined")
	k.Exit(finished, kernel.ExitStatus{Code: 0, Reason: "done"})
	k.Exit(gone, kernel.ExitStatus{Code: 1, Reason: "bad"})
	if err := bus.Unsubscribe(closed, ipc.CauseChainEnded); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(root, "bus", "subscriptions.json"))
	if strings.Contains(string(raw), closed) {
		t.Fatalf("after Unsubscribe: %s", raw)
	}
	created, _, err := bus.Publish(publisher, "news", "x")
	if err != nil || len(created) != 1 || created[0].To != stays {
		t.Fatalf("publish = %+v, %v", created, err)
	}
	raw, _ = os.ReadFile(filepath.Join(root, "bus", "subscriptions.json"))
	var subscriptions map[string][]string
	json.Unmarshal(raw, &subscriptions)
	if len(subscriptions["news"]) != 1 || subscriptions["news"][0] != stays {
		t.Fatalf("subscriptions = %s", raw)
	}
	left := map[string]string{}
	for _, event := range events(t, k, "ipc.unsubscribed") {
		left[event.Data["pid"]] = event.Data["cause"]
	}
	for _, pid := range []string{closed, finished, gone} {
		if left[pid] != ipc.CauseChainEnded {
			t.Fatalf("ipc.unsubscribed for %s: %+v", pid, left)
		}
	}
}

// Subscriptions are state, so each change is on the record.
func TestSubscriptionsAreEvents(t *testing.T) {
	bus, k, _ := newBus(t)
	worker := spawn(t, k)
	bus.Subscribe(worker, "news")
	bus.Subscribe(worker, "news")
	bus.Subscribe(worker, "alerts")
	next := supersede(t, k, worker)
	bus.Transfer(worker, next)
	bus.Unsubscribe(next, ipc.CauseChainEnded)
	var got []string
	all, _ := k.Events()
	for _, event := range all {
		if event.Type == "ipc.subscribed" || event.Type == "ipc.unsubscribed" {
			owner := "old"
			if event.Data["pid"] == next {
				owner = "new"
			}
			got = append(got, strings.TrimPrefix(event.Type, "ipc.")+" "+event.Data["topic"]+" "+owner+" "+event.Data["cause"])
		}
	}
	want := []string{
		"subscribed news old subscribe", "subscribed alerts old subscribe",
		"unsubscribed alerts old transfer", "subscribed alerts new transfer",
		"unsubscribed news old transfer", "subscribed news new transfer",
		"unsubscribed alerts new chain_ended", "unsubscribed news new chain_ended",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("events:\n%s", strings.Join(got, "\n"))
	}
}

// A replay of an older record gets a bus that adds none of them.
func TestNoSubscriptionEventsLeavesThemOut(t *testing.T) {
	bus, k, _ := newBus(t)
	bus.NoSubscriptionEvents = true
	worker := spawn(t, k)
	bus.Subscribe(worker, "news")
	bus.Unsubscribe(worker, ipc.CauseChainEnded)
	if got := len(events(t, k, "ipc.subscribed")) + len(events(t, k, "ipc.unsubscribed")); got != 0 {
		t.Fatalf("%d subscription events", got)
	}
}

// A damaged chain gives the sender an error, leaves other subscribers served,
// and does not cost the subscription a decision nobody made.
func TestBrokenChainIsRefusedNotFollowed(t *testing.T) {
	bus, k, root := newBus(t)
	sender, looping, dangling, fine := spawn(t, k), spawn(t, k), spawn(t, k), spawn(t, k)
	for _, pid := range []string{looping, dangling, fine} {
		bus.Subscribe(pid, "news")
	}
	k.Exit(looping, crashed)
	k.Exit(dangling, crashed)
	table := filepath.Join(root, "kernel", "processes.json")
	raw, _ := os.ReadFile(table)
	var processes map[string]map[string]any
	if err := json.Unmarshal(raw, &processes); err != nil {
		t.Fatal(err)
	}
	processes[looping]["superseded_by"] = looping
	processes[dangling]["superseded_by"] = "pid-999999"
	raw, _ = json.Marshal(processes)
	os.WriteFile(table, raw, 0o644)
	for _, to := range []string{looping, dangling} {
		if _, err := bus.Send(sender, to, "x"); !errors.Is(err, kernel.ErrBrokenChain) || !strings.Contains(err.Error(), to) {
			t.Fatalf("send to %s = %v", to, err)
		}
	}
	created, _, err := bus.Publish(sender, "news", "x")
	if err != nil || len(created) != 1 || created[0].To != fine {
		t.Fatalf("publish = %+v, %v", created, err)
	}
	for _, pid := range []string{looping, dangling} {
		if topics, _ := bus.Subscriptions(pid); len(topics) != 1 {
			t.Fatalf("%s lost its subscription: %v", pid, topics)
		}
	}
	if rejected := events(t, k, "ipc.rejected"); len(rejected) != 4 {
		t.Fatalf("ipc.rejected = %+v", rejected)
	}
}

func TestRedeliverKeepsIDsAndComesFirst(t *testing.T) {
	bus, k, _ := newBus(t)
	sender, worker := spawn(t, k), spawn(t, k)
	one, _ := bus.Send(sender, worker, "one")
	two, _ := bus.Send(sender, worker, "two")
	delivered, _ := bus.Receive(worker, false)
	bus.Ack(worker, []string{one.ID, two.ID})
	pending, _ := bus.Send(sender, worker, "pending")
	next := supersede(t, k, worker)
	if _, err := bus.Transfer(worker, next); err != nil {
		t.Fatal(err)
	}
	back := []ipc.Redelivery{{Message: delivered[0], Record: "art-1"}, {Message: delivered[1], Record: "art-2"}}
	if added, err := bus.Redeliver(next, back, "rollback"); err != nil || added != 2 {
		t.Fatalf("redeliver = %d, %v", added, err)
	}
	// Repeated after a crash, it adds nothing, and logs nothing more.
	if added, err := bus.Redeliver(next, back, "rollback"); err != nil || added != 0 {
		t.Fatalf("second redeliver = %d, %v", added, err)
	}
	logged := events(t, k, "ipc.redelivered")
	if len(logged) != 2 || logged[0].Data["id"] != one.ID || logged[0].Data["cause"] != "rollback" || logged[0].Data["record"] != "art-1" || logged[0].ProcessID != next {
		t.Fatalf("ipc.redelivered = %+v", logged)
	}
	inbox, _ := bus.Receive(next, false)
	if got := bodies(inbox); got != "one two pending" {
		t.Fatalf("inbox = %q", got)
	}
	for i, id := range []string{one.ID, two.ID, pending.ID} {
		if inbox[i].ID != id || inbox[i].To != next || inbox[i].Deliveries != 1 {
			t.Fatalf("message %d = %+v", i, inbox[i])
		}
	}
	// IDs keep moving on: a new one is above every re-delivered one.
	if fresh, _ := bus.Send(sender, next, "fresh"); fresh.ID <= pending.ID {
		t.Fatalf("fresh ID %s", fresh.ID)
	}
}
