package runlog_test

import (
	"os"
	"path/filepath"
	"testing"

	"aos/internal/runlog"
)

type record struct {
	Seq  int    `json:"seq"`
	Note string `json:"note"`
}

func appendNote(t *testing.T, log *runlog.Log, note string) int {
	t.Helper()
	seq, err := log.Append(func(seq int) any { return record{Seq: seq, Note: note} })
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	return seq
}

func TestAppendSurvivesATornLastLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run", "trace.jsonl")
	log, err := runlog.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if seq := appendNote(t, log, "a"); seq != 1 {
		t.Fatalf("first seq = %d", seq)
	}
	appendNote(t, log, "b")
	// A writer killed mid-line leaves a partial record with no newline.
	file, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	file.WriteString(`{"seq":3,"note":"cut of`)
	file.Close()

	records, err := runlog.Read[record](path)
	if err != nil || len(records) != 2 {
		t.Fatalf("Read should skip the torn line: %+v %v", records, err)
	}
	if seq := appendNote(t, log, "c"); seq != 3 {
		t.Fatalf("seq after a torn line = %d, want 3", seq)
	}
	records, _ = runlog.Read[record](path)
	if len(records) != 3 || records[2] != (record{Seq: 3, Note: "c"}) {
		t.Fatalf("records after recovery: %+v", records)
	}
}

func TestReadMissingFileIsEmpty(t *testing.T) {
	records, err := runlog.Read[record](filepath.Join(t.TempDir(), "absent.jsonl"))
	if err != nil || len(records) != 0 {
		t.Fatalf("got %+v %v", records, err)
	}
}
