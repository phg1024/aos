package ipc_test

import (
	"path/filepath"
	"testing"

	"aos/internal/ipc"
)

func TestSendReceiveAndPublishSubscribe(t *testing.T) {
	bus, err := ipc.New(filepath.Join(t.TempDir(), "ipc"))
	if err != nil {
		t.Fatalf("new bus: %v", err)
	}

	if _, err := bus.Send("pid-a", "pid-b", "hello"); err != nil {
		t.Fatalf("send: %v", err)
	}
	if err := bus.Subscribe("pid-c", "artifact.created"); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if _, err := bus.Publish("pid-a", "artifact.created", "art-1"); err != nil {
		t.Fatalf("publish: %v", err)
	}

	inboxB, err := bus.ReceiveAll("pid-b")
	if err != nil {
		t.Fatalf("receive b: %v", err)
	}
	if len(inboxB) != 1 || inboxB[0].Body != "hello" {
		t.Fatalf("unexpected direct message %#v", inboxB)
	}

	inboxC, err := bus.ReceiveAll("pid-c")
	if err != nil {
		t.Fatalf("receive c: %v", err)
	}
	if len(inboxC) != 1 || inboxC[0].Topic != "artifact.created" {
		t.Fatalf("unexpected published message %#v", inboxC)
	}
}
