package ipc_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"aos/internal/ipc"
	"aos/internal/kernel"
)

type boundsFixture struct {
	dir    string
	kernel *kernel.Kernel
	bus    *ipc.Bus
	from   string
	to     string
}

func newBoundsFixture(t *testing.T) boundsFixture {
	t.Helper()
	dir := t.TempDir()
	k, err := kernel.New(filepath.Join(dir, "kernel"))
	if err != nil {
		t.Fatal(err)
	}
	bus, err := ipc.New(filepath.Join(dir, "bus"), k)
	if err != nil {
		t.Fatal(err)
	}
	f := boundsFixture{dir: dir, kernel: k, bus: bus}
	f.from, f.to = f.spawn(t), f.spawn(t)
	return f
}

func (f boundsFixture) spawn(t *testing.T) string {
	t.Helper()
	process, err := f.kernel.Spawn(kernel.ProcessSpec{Role: "agent:x"})
	if err != nil {
		t.Fatal(err)
	}
	return process.PID
}

// fill leaves n messages of size bytes in pid's mailbox: the last few by sending, so
// the bound lets them in, and the rest by writing the bus's file, since a send costs
// about 20 ms.
func (f boundsFixture) fill(t *testing.T, pid string, n, size int) {
	t.Helper()
	seeded := max(n-4, 0)
	pending, err := f.bus.Pending()
	if err != nil {
		t.Fatal(err)
	}
	next := 1
	for _, box := range pending {
		next += len(box)
	}
	for i := 0; i < seeded; i++ {
		pending[pid] = append(pending[pid], ipc.Message{ID: fmt.Sprintf("msg-%06d", next+i), From: f.from, To: pid, Body: strings.Repeat("x", size), Timestamp: time.Now().UTC()})
	}
	raw, err := json.Marshal(pending)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.dir, "bus", "mailboxes.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	for i := seeded; i < n; i++ {
		if _, err := f.bus.Send(f.from, pid, strings.Repeat("x", size)); err != nil {
			t.Fatalf("send %d of %d: %v", i+1, n, err)
		}
	}
}

func (f boundsFixture) rejections(t *testing.T, reason string) []kernel.Event {
	t.Helper()
	events, err := f.kernel.Events()
	if err != nil {
		t.Fatal(err)
	}
	var out []kernel.Event
	for _, event := range events {
		if event.Type == "ipc.rejected" && event.Data["reason"] == reason {
			out = append(out, event)
		}
	}
	return out
}

func (f boundsFixture) pending(t *testing.T, pid string) []ipc.Message {
	t.Helper()
	pending, err := f.bus.Pending()
	if err != nil {
		t.Fatal(err)
	}
	return pending[pid]
}

func TestTheCapRefusesAndLogs(t *testing.T) {
	f := newBoundsFixture(t)
	if err := f.bus.Subscribe(f.to, "news"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.bus.Send(f.from, f.to, strings.Repeat("x", ipc.MaxBodyBytes)); err != nil {
		t.Fatalf("at the cap: %v", err)
	}
	over := strings.Repeat("x", ipc.MaxBodyBytes+1)
	if _, err := f.bus.Send(f.from, f.to, over); !errors.Is(err, ipc.ErrMessageTooLarge) {
		t.Fatalf("send = %v", err)
	}
	if _, _, err := f.bus.Publish(f.from, "news", over); !errors.Is(err, ipc.ErrMessageTooLarge) {
		t.Fatalf("publish = %v", err)
	}
	if _, err := f.bus.SendAsOperator(f.to, over); !errors.Is(err, ipc.ErrMessageTooLarge) {
		t.Fatalf("operator = %v", err)
	}
	// A handle list cannot carry what a body may not.
	if _, err := f.bus.SendWithArtifacts(f.from, f.to, strings.Repeat("x", ipc.MaxBodyBytes-3), []string{"art-1"}); !errors.Is(err, ipc.ErrMessageTooLarge) {
		t.Fatalf("handles count = %v", err)
	}
	if got := f.rejections(t, "too_large"); len(got) != 4 || got[0].Data["bytes"] != "16385" || got[0].Data["to"] != f.to || got[1].Data["topic"] != "news" {
		t.Fatalf("ipc.rejected = %+v", got)
	}
	if got := f.pending(t, f.to); len(got) != 1 {
		t.Fatalf("a refused message stayed: %d pending", len(got))
	}
}

func TestTheBoundCountsMessagesAndBytes(t *testing.T) {
	t.Run("messages", func(t *testing.T) {
		f := newBoundsFixture(t)
		f.fill(t, f.to, ipc.MaxMailboxMessages, 1)
		_, err := f.bus.Send(f.from, f.to, "the 257th")
		if !errors.Is(err, ipc.ErrMailboxFull) {
			t.Fatalf("send = %v", err)
		}
		if strings.Contains(err.Error(), ipc.HeldMeaning) {
			t.Fatalf("a live receiver is not held: %v", err)
		}
		if got := f.rejections(t, "mailbox_full"); len(got) != 1 || got[0].Data["to"] != f.to {
			t.Fatalf("ipc.rejected = %+v", got)
		}
		if got := f.pending(t, f.to); len(got) != ipc.MaxMailboxMessages {
			t.Fatalf("%d pending", len(got))
		}
	})
	t.Run("bytes", func(t *testing.T) {
		f := newBoundsFixture(t)
		f.fill(t, f.to, 64, ipc.MaxBodyBytes)
		if _, err := f.bus.Send(f.from, f.to, "x"); !errors.Is(err, ipc.ErrMailboxFull) {
			t.Fatalf("one byte over 1 MiB = %v", err)
		}
		// Another mailbox is another bound.
		other := f.spawn(t)
		if _, err := f.bus.Send(f.from, other, "x"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("a message that does not fit does not stop a smaller one", func(t *testing.T) {
		f := newBoundsFixture(t)
		f.fill(t, f.to, 63, ipc.MaxBodyBytes)
		f.fill(t, f.to, 1, ipc.MaxBodyBytes-10)
		if _, err := f.bus.Send(f.from, f.to, strings.Repeat("x", 11)); !errors.Is(err, ipc.ErrMailboxFull) {
			t.Fatalf("over by one = %v", err)
		}
		if _, err := f.bus.Send(f.from, f.to, strings.Repeat("x", 10)); err != nil {
			t.Fatalf("exactly fits = %v", err)
		}
	})
}

// A held mailbox fills up too: a chain nobody closes gives no outcome by itself,
// and now its senders get a refusal once it is full.
func TestAHeldMailboxFillsAndRefuses(t *testing.T) {
	f := newBoundsFixture(t)
	if _, err := f.kernel.Exit(f.to, kernel.ExitStatus{Code: 1, Reason: "crashed", Retryable: true}); err != nil {
		t.Fatal(err)
	}
	f.fill(t, f.to, ipc.MaxMailboxMessages, 1)
	_, err := f.bus.Send(f.from, f.to, "one more")
	if !errors.Is(err, ipc.ErrMailboxFull) || !strings.Contains(err.Error(), ipc.HeldMeaning) {
		t.Fatalf("send = %v", err)
	}
}

func TestPublishSkipsAFullSubscriber(t *testing.T) {
	f := newBoundsFixture(t)
	room := f.spawn(t)
	for _, pid := range []string{f.to, room} {
		if err := f.bus.Subscribe(pid, "news"); err != nil {
			t.Fatal(err)
		}
	}
	f.fill(t, f.to, ipc.MaxMailboxMessages, 1)
	created, skipped, err := f.bus.Publish(f.from, "news", "hello")
	if err != nil {
		t.Fatal(err)
	}
	if len(created) != 1 || created[0].To != room || !slices.Equal(skipped, []string{f.to}) {
		t.Fatalf("created %+v, skipped %v", created, skipped)
	}
	if got := f.rejections(t, "mailbox_full"); len(got) != 1 || got[0].Data["to"] != f.to || got[0].Data["topic"] != "news" {
		t.Fatalf("ipc.rejected = %+v", got)
	}
	// Full is not gone: it stays subscribed.
	if topics, _ := f.bus.Subscriptions(f.to); !slices.Equal(topics, []string{"news"}) {
		t.Fatalf("subscriptions = %v", topics)
	}
}

func TestTheOperatorIsNotHeldToTheBound(t *testing.T) {
	f := newBoundsFixture(t)
	f.fill(t, f.to, ipc.MaxMailboxMessages, 1)
	if _, err := f.bus.SendAsOperator(f.to, "stop what you are doing"); err != nil {
		t.Fatalf("operator = %v", err)
	}
	got := f.pending(t, f.to)
	if len(got) != ipc.MaxMailboxMessages+1 || got[len(got)-1].Body != "stop what you are doing" {
		t.Fatalf("%d pending", len(got))
	}
	if _, err := f.bus.Send(f.from, f.to, "peer"); !errors.Is(err, ipc.ErrMailboxFull) {
		t.Fatalf("peer = %v", err)
	}
}

// Mail already accepted is never refused at a handover, and a mailbox over its
// bound takes no new mail until it has drained below it.
func TestHandoverMayOvershootTheBound(t *testing.T) {
	check := func(t *testing.T, f boundsFixture, pid string, want int) {
		t.Helper()
		got := f.pending(t, pid)
		if len(got) != want {
			t.Fatalf("kept %d, want %d", len(got), want)
		}
		if _, err := f.bus.Send(f.from, pid, "new"); !errors.Is(err, ipc.ErrMailboxFull) {
			t.Fatalf("over the bound, send = %v", err)
		}
		// Down to the bound is still full; one below takes one.
		var ids []string
		for _, message := range got[:want-ipc.MaxMailboxMessages] {
			ids = append(ids, message.ID)
		}
		if err := f.bus.Ack(pid, ids); err != nil {
			t.Fatal(err)
		}
		if _, err := f.bus.Send(f.from, pid, "new"); !errors.Is(err, ipc.ErrMailboxFull) {
			t.Fatalf("at the bound, send = %v", err)
		}
		if err := f.bus.Ack(pid, []string{f.pending(t, pid)[0].ID}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.bus.Send(f.from, pid, "new"); err != nil {
			t.Fatalf("below the bound, send = %v", err)
		}
	}
	t.Run("transfer", func(t *testing.T) {
		f := newBoundsFixture(t)
		f.fill(t, f.to, ipc.MaxMailboxMessages, 1)
		f.kernel.Exit(f.to, kernel.ExitStatus{Code: 1, Reason: "crashed", Retryable: true})
		next, err := f.kernel.Supersede(f.to, kernel.ProcessSpec{Role: "agent:x"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		// Sent to the successor before the transfer lands.
		f.fill(t, next.PID, 10, 1)
		if moved, err := f.bus.Transfer(f.to, next.PID); err != nil || moved != ipc.MaxMailboxMessages {
			t.Fatalf("transfer = %d, %v", moved, err)
		}
		check(t, f, next.PID, ipc.MaxMailboxMessages+10)
	})
	t.Run("rollback", func(t *testing.T) {
		f := newBoundsFixture(t)
		f.fill(t, f.to, ipc.MaxMailboxMessages, 1)
		var back []ipc.Redelivery
		for i := 0; i < 5; i++ {
			back = append(back, ipc.Redelivery{Message: ipc.Message{ID: "msg-90000" + string(rune('0'+i)), From: f.from, Body: "undone"}, Record: "art-x"})
		}
		if added, err := f.bus.Redeliver(f.to, back, "rollback"); err != nil || added != 5 {
			t.Fatalf("redeliver = %d, %v", added, err)
		}
		check(t, f, f.to, ipc.MaxMailboxMessages+5)
	})
}

// A body accepted before the cap goes on being re-delivered whole.
func TestRedeliveryIgnoresTheCap(t *testing.T) {
	f := newBoundsFixture(t)
	big := strings.Repeat("x", 2*ipc.MaxBodyBytes)
	if _, err := f.bus.Redeliver(f.to, []ipc.Redelivery{{Message: ipc.Message{ID: "msg-000001", From: f.from, Body: big}, Record: "art-x"}}, "rollback"); err != nil {
		t.Fatal(err)
	}
	if got := f.pending(t, f.to); len(got) != 1 || got[0].Body != big {
		t.Fatalf("pending = %d", len(got))
	}
}

func TestReceiveBatchTakesWhatFitsInOrder(t *testing.T) {
	f := newBoundsFixture(t)
	f.fill(t, f.to, 5, 100)
	first, left, err := f.bus.ReceiveBatch(f.to, false, nil, ipc.Batch{Bytes: 250})
	if err != nil || len(first) != 2 || left != (ipc.Waiting{Messages: 3, Bytes: 300}) {
		t.Fatalf("first = %d, %+v, %v", len(first), left, err)
	}
	held := []string{first[0].ID, first[1].ID}
	rest, left, err := f.bus.ReceiveBatch(f.to, false, held, ipc.Batch{Bytes: 250})
	if err != nil || len(rest) != 2 || rest[0].ID <= first[1].ID || left.Messages != 1 {
		t.Fatalf("second = %+v, %+v, %v", rest, left, err)
	}
	held = append(held, rest[0].ID, rest[1].ID)
	// What the receiver holds costs nothing, so it cannot starve what it does not.
	again, left, err := f.bus.ReceiveBatch(f.to, true, held, ipc.Batch{Bytes: 150})
	if err != nil || len(again) != 5 || left.Messages != 0 {
		t.Fatalf("redelivered = %d, %+v, %v", len(again), left, err)
	}
	// Without redeliver, what it holds does not come again.
	if none, left, err := f.bus.ReceiveBatch(f.to, false, append(held, again[4].ID), ipc.Batch{}); err != nil || len(none) != 0 || left.Messages != 0 {
		t.Fatalf("all held = %+v, %+v, %v", none, left, err)
	}
	// A message over the limit still comes, alone, so the mailbox always moves.
	g := newBoundsFixture(t)
	g.fill(t, g.to, 3, 1000)
	one, left, err := g.bus.ReceiveBatch(g.to, false, nil, ipc.Batch{Bytes: 10})
	if err != nil || len(one) != 1 || left != (ipc.Waiting{Messages: 2, Bytes: 2000}) {
		t.Fatalf("over the limit = %d, %+v, %v", len(one), left, err)
	}
	if all, _, err := g.bus.ReceiveBatch(g.to, false, []string{one[0].ID}, ipc.Batch{}); err != nil || len(all) != 2 {
		t.Fatalf("no limit = %d, %v", len(all), err)
	}
	h := newBoundsFixture(t)
	h.fill(t, h.to, 5, 1)
	if some, left, err := h.bus.ReceiveBatch(h.to, false, nil, ipc.Batch{Messages: 3}); err != nil || len(some) != 3 || left.Messages != 2 {
		t.Fatalf("by count = %d, %+v, %v", len(some), left, err)
	}
}

// Handed out before is not held now: a session that did not last took it, and this
// one has never seen it. It comes at any step, in its turn.
func TestReceiveBatchTakesDeliveredMailItDoesNotHold(t *testing.T) {
	f := newBoundsFixture(t)
	f.fill(t, f.to, 5, 1)
	if _, err := f.bus.Receive(f.to, false); err != nil {
		t.Fatal(err)
	}
	first, left, err := f.bus.ReceiveBatch(f.to, false, nil, ipc.Batch{Messages: 2})
	if err != nil || len(first) != 2 || left.Messages != 3 || first[0].Deliveries != 2 {
		t.Fatalf("first = %+v, %+v, %v", first, left, err)
	}
	rest, left, err := f.bus.ReceiveBatch(f.to, false, []string{first[0].ID, first[1].ID}, ipc.Batch{})
	if err != nil || len(rest) != 3 || left.Messages != 0 {
		t.Fatalf("rest = %+v, %+v, %v", rest, left, err)
	}
	// Receive itself, which replay uses, is as it was.
	g := newBoundsFixture(t)
	g.fill(t, g.to, 2, 1)
	if _, err := g.bus.Receive(g.to, false); err != nil {
		t.Fatal(err)
	}
	if none, err := g.bus.Receive(g.to, false); err != nil || len(none) != 0 {
		t.Fatalf("Receive without redeliver = %+v, %v", none, err)
	}
	if all, err := g.bus.Receive(g.to, true); err != nil || len(all) != 2 {
		t.Fatalf("Receive with redeliver = %+v, %v", all, err)
	}
}

// Hold takes on nothing new, reports it, and still re-delivers what is held.
func TestReceiveBatchHolds(t *testing.T) {
	f := newBoundsFixture(t)
	f.fill(t, f.to, 3, 10)
	first, _, err := f.bus.ReceiveBatch(f.to, false, nil, ipc.Batch{Messages: 1})
	if err != nil || len(first) != 1 {
		t.Fatal(first, err)
	}
	got, left, err := f.bus.ReceiveBatch(f.to, true, []string{first[0].ID}, ipc.Batch{Hold: true})
	if err != nil || len(got) != 1 || got[0].ID != first[0].ID || left != (ipc.Waiting{Messages: 2, Bytes: 20}) {
		t.Fatalf("held = %+v, %+v, %v", got, left, err)
	}
}

// A grant is only as good as the copies of the message: every path that moves one
// keeps its handles.
func TestHandlesTravelWithTheMail(t *testing.T) {
	handles := []string{"art-000007", "art-000008"}
	carries := func(t *testing.T, message ipc.Message) {
		t.Helper()
		if !slices.Equal(message.Artifacts, handles) {
			t.Fatalf("message %s carries %v", message.ID, message.Artifacts)
		}
	}
	f := newBoundsFixture(t)
	sent, err := f.bus.SendWithArtifacts(f.from, f.to, "see these", handles)
	if err != nil {
		t.Fatal(err)
	}
	carries(t, sent)
	f.kernel.Exit(f.to, kernel.ExitStatus{Code: 1, Reason: "crashed", Retryable: true})
	next, err := f.kernel.Supersede(f.to, kernel.ProcessSpec{Role: "agent:x"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Forwarded along the chain, then moved along it.
	forwarded, err := f.bus.SendWithArtifacts(f.from, f.to, "forwarded", handles)
	if err != nil || forwarded.To != next.PID {
		t.Fatalf("forwarded = %+v, %v", forwarded, err)
	}
	carries(t, forwarded)
	if _, err := f.bus.Transfer(f.to, next.PID); err != nil {
		t.Fatal(err)
	}
	received, err := f.bus.Receive(next.PID, false)
	if err != nil || len(received) != 2 {
		t.Fatalf("received = %d, %v", len(received), err)
	}
	carries(t, received[0])
	carries(t, received[1])
	// Acknowledged, then put back from its record by a rollback.
	if err := f.bus.Ack(next.PID, []string{received[0].ID, received[1].ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.bus.Redeliver(next.PID, []ipc.Redelivery{{Message: received[0], Record: "art-x"}}, "rollback"); err != nil {
		t.Fatal(err)
	}
	carries(t, f.pending(t, next.PID)[0])
	// Swept: the kept body has them too.
	f.kernel.Exit(next.PID, kernel.ExitStatus{Code: 0, Reason: "done"})
	if err := f.bus.Sweep(); err != nil {
		t.Fatal(err)
	}
	kept, err := f.bus.DeadLetterBody(received[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	carries(t, kept)
}

// A record made before the bounds replays as it ran.
func TestNoBoundsTakesAnything(t *testing.T) {
	f := newBoundsFixture(t)
	f.bus.NoBounds = true
	f.fill(t, f.to, ipc.MaxMailboxMessages+1, 1)
	if err := f.bus.Subscribe(f.to, "news"); err != nil {
		t.Fatal(err)
	}
	if created, skipped, err := f.bus.Publish(f.from, "news", strings.Repeat("x", ipc.MaxBodyBytes+1)); err != nil || len(created) != 1 || skipped != nil {
		t.Fatalf("publish = %d, %v, %v", len(created), skipped, err)
	}
	if got := f.rejections(t, "mailbox_full"); len(got) != 0 {
		t.Fatalf("ipc.rejected = %+v", got)
	}
}
