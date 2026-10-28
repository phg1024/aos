//go:build unix

package runlog_test

import (
	"fmt"
	"os"
	"os/exec"
	"sync"
	"testing"

	"aos/internal/runlog"
)

const (
	helperPathEnv = "AOS_RUNLOG_HELPER_PATH"
	perWriter     = 40
)

func writeMany(path, writer string) error {
	log, err := runlog.Open(path)
	if err != nil {
		return err
	}
	for i := 0; i < perWriter; i++ {
		if _, err := log.Append(func(seq int) any { return record{Seq: seq, Note: fmt.Sprintf("%s-%d", writer, i)} }); err != nil {
			return err
		}
	}
	return nil
}

func TestRunlogHelperProcess(t *testing.T) {
	path := os.Getenv(helperPathEnv)
	if path == "" {
		t.Skip("subprocess helper")
	}
	if err := writeMany(path, os.Getenv("AOS_RUNLOG_HELPER_WRITER")); err != nil {
		t.Fatal(err)
	}
}

// Writers in separate processes and separate Logs in one process lose no
// lines, and seq stays strictly increasing in file order.
func TestConcurrentWritersKeepSeqOrdered(t *testing.T) {
	path := t.TempDir() + "/run/calls.jsonl"
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	writers := 0
	for i := 0; i < 3; i++ {
		writer := fmt.Sprintf("proc%d", i)
		cmd := exec.Command(os.Args[0], "-test.run=^TestRunlogHelperProcess$", "-test.count=1")
		cmd.Env = append(os.Environ(), helperPathEnv+"="+path, "AOS_RUNLOG_HELPER_WRITER="+writer)
		writers++
		wg.Add(1)
		go func() {
			defer wg.Done()
			if out, err := cmd.CombinedOutput(); err != nil {
				errs <- fmt.Errorf("%s: %v\n%s", writer, err, out)
			}
		}()
	}
	for i := 0; i < 2; i++ {
		writer := fmt.Sprintf("inproc%d", i)
		writers++
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := writeMany(path, writer); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	records, err := runlog.Read[record](path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(records) != writers*perWriter {
		t.Fatalf("expected %d lines, got %d", writers*perWriter, len(records))
	}
	seen := map[string]bool{}
	for idx, rec := range records {
		if rec.Seq != idx+1 {
			t.Fatalf("line %d has seq %d: seq must be gap-free and increasing", idx, rec.Seq)
		}
		if seen[rec.Note] {
			t.Fatalf("duplicate record %s", rec.Note)
		}
		seen[rec.Note] = true
	}
}
