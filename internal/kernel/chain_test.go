package kernel_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"aos/internal/kernel"
)

func exited(t *testing.T, k *kernel.Kernel, status kernel.ExitStatus) kernel.Process {
	t.Helper()
	process, err := k.Spawn(kernel.ProcessSpec{Role: "agent:worker"})
	if err != nil {
		t.Fatal(err)
	}
	process, err = k.Exit(process.PID, status)
	if err != nil {
		t.Fatal(err)
	}
	return process
}

var retryable = kernel.ExitStatus{Code: 1, Reason: "crashed", Retryable: true}

func TestChainEnded(t *testing.T) {
	k, _ := kernel.New(t.TempDir())
	live, _ := k.Spawn(kernel.ProcessSpec{Role: "agent:worker"})
	superseded := exited(t, k, retryable)
	if _, err := k.Supersede(superseded.PID, kernel.ProcessSpec{Role: "agent:worker"}, nil); err != nil {
		t.Fatal(err)
	}
	superseded, _ = k.Get(superseded.PID)
	closed := exited(t, k, retryable)
	closed, _ = k.Close(closed.PID, "declined")
	for _, c := range []struct {
		name    string
		process kernel.Process
		ended   bool
	}{
		{"live", live, false},
		{"retryable", exited(t, k, retryable), false},
		{"superseded", superseded, false},
		{"closed", closed, true},
		{"code 0", exited(t, k, kernel.ExitStatus{Code: 0, Reason: "done"}), true},
		{"code 0, retryable", exited(t, k, kernel.ExitStatus{Code: 0, Reason: "done", Retryable: true}), true},
		{"final", exited(t, k, kernel.ExitStatus{Code: 1, Reason: "bad"}), true},
	} {
		if got := c.process.ChainEnded(); got != c.ended {
			t.Errorf("%s: ChainEnded = %t", c.name, got)
		}
	}
}

// Close and Supersede exclude each other, whichever comes first.
func TestCloseAndSupersedeExcludeEachOther(t *testing.T) {
	k, _ := kernel.New(t.TempDir())
	closed := exited(t, k, retryable)
	if process, err := k.Close(closed.PID, "declined"); err != nil || !process.Closed {
		t.Fatalf("close = %+v, %v", process, err)
	}
	if _, err := k.Supersede(closed.PID, kernel.ProcessSpec{Role: "agent:worker"}, nil); !errors.Is(err, kernel.ErrChainClosed) {
		t.Fatalf("supersede of a closed process = %v", err)
	}
	superseded := exited(t, k, retryable)
	if _, err := k.Supersede(superseded.PID, kernel.ProcessSpec{Role: "agent:worker"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Close(superseded.PID, "declined"); !errors.Is(err, kernel.ErrSuperseded) {
		t.Fatalf("close of a superseded process = %v", err)
	}
	live, _ := k.Spawn(kernel.ProcessSpec{Role: "agent:worker"})
	if _, err := k.Close(live.PID, "declined"); !errors.Is(err, kernel.ErrInvalidTransition) {
		t.Fatalf("close of a live process = %v", err)
	}
	if _, err := k.Close("pid-999999", "declined"); !errors.Is(err, kernel.ErrProcessNotFound) {
		t.Fatalf("close of an unknown process = %v", err)
	}
}

// Closing is recorded once, and a chain already ended records nothing.
func TestCloseRecordsOnce(t *testing.T) {
	k, _ := kernel.New(t.TempDir())
	count := func() int {
		events, _ := k.Events()
		n := 0
		for _, event := range events {
			if event.Type == "process.closed" {
				n++
			}
		}
		return n
	}
	process := exited(t, k, retryable)
	k.Close(process.PID, "declined")
	k.Close(process.PID, "again")
	if n := count(); n != 1 {
		t.Fatalf("%d process.closed events after closing twice", n)
	}
	final := exited(t, k, kernel.ExitStatus{Code: 0, Reason: "done"})
	if got, err := k.Close(final.PID, "declined"); err != nil || got.Closed || count() != 1 {
		t.Fatalf("close of a final exit = %+v, %v, %d events", got, err, count())
	}
}

// The event log is the record: a table that fell behind is repaired.
func TestClosedIsRebuiltFromTheLog(t *testing.T) {
	root := t.TempDir()
	k, _ := kernel.New(root)
	process := exited(t, k, retryable)
	table := filepath.Join(root, "processes.json")
	before, err := os.ReadFile(table)
	if err != nil {
		t.Fatal(err)
	}
	k.Close(process.PID, "declined")
	if err := os.WriteFile(table, before, 0o644); err != nil {
		t.Fatal(err)
	}
	reopened, err := kernel.New(root)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := reopened.Get(process.PID); !got.Closed {
		t.Fatalf("after repair: %+v", got)
	}
}

func TestChainEndFollowsSuccessors(t *testing.T) {
	k, _ := kernel.New(t.TempDir())
	first := exited(t, k, retryable)
	second, err := k.Supersede(first.PID, kernel.ProcessSpec{Role: "agent:worker"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	k.Exit(second.PID, retryable)
	third, err := k.Supersede(second.PID, kernel.ProcessSpec{Role: "agent:worker"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, pid := range []string{first.PID, second.PID, third.PID} {
		if end, err := k.ChainEnd(pid); err != nil || end.PID != third.PID {
			t.Fatalf("ChainEnd(%s) = %s, %v", pid, end.PID, err)
		}
	}
	if _, err := k.ChainEnd("pid-999999"); !errors.Is(err, kernel.ErrProcessNotFound) {
		t.Fatalf("an unknown start = %v", err)
	}
}

// A damaged table cannot hang the walk or send it somewhere: a loop and a
// link to nowhere both end in ErrBrokenChain.
func TestChainEndOnABrokenChain(t *testing.T) {
	root := t.TempDir()
	k, _ := kernel.New(root)
	a := exited(t, k, retryable)
	b := exited(t, k, retryable)
	c := exited(t, k, retryable)
	d := exited(t, k, retryable)
	table := filepath.Join(root, "processes.json")
	raw, err := os.ReadFile(table)
	if err != nil {
		t.Fatal(err)
	}
	var processes map[string]map[string]any
	if err := json.Unmarshal(raw, &processes); err != nil {
		t.Fatal(err)
	}
	processes[a.PID]["superseded_by"] = b.PID
	processes[b.PID]["superseded_by"] = a.PID
	processes[c.PID]["superseded_by"] = c.PID
	processes[d.PID]["superseded_by"] = "pid-999999"
	raw, _ = json.Marshal(processes)
	if err := os.WriteFile(table, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, pid := range []string{a.PID, b.PID, c.PID, d.PID} {
		done := make(chan error, 1)
		go func() {
			_, err := k.ChainEnd(pid)
			done <- err
		}()
		select {
		case err := <-done:
			if !errors.Is(err, kernel.ErrBrokenChain) {
				t.Fatalf("ChainEnd(%s) = %v", pid, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("ChainEnd(%s) hung", pid)
		}
	}
}
