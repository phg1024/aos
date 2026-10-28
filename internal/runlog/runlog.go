// Package runlog keeps append-only JSONL records of a run (its trace and
// its model calls) that survive the run being killed. Every record carries
// a seq that increases across all writers of the file, including other
// processes, because seq is assigned and the line written under one
// filelock. Append fsyncs before returning, so a record whose Append
// returned is on disk; a kill during Append can leave at most a torn last
// line, which Read skips and the next Append terminates.
//
// These files are diagnostic. The kernel event log stays the source of
// truth; records that correspond to a kernel event name its ID.
package runlog

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"

	"aos/internal/filelock"
)

// File names under a state directory.
const (
	Dir       = "run"
	TraceFile = "trace.jsonl"
	CallsFile = "calls.jsonl"
)

// Path returns the path of a run log file under stateDir.
func Path(stateDir, name string) string {
	return filepath.Join(stateDir, Dir, name)
}

// Log is one append-only JSONL file.
type Log struct {
	path string
	mu   sync.Mutex
}

// Open prepares the log at path, creating its directory.
func Open(path string) (*Log, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	return &Log{path: path}, nil
}

// Path is the file the log appends to.
func (l *Log) Path() string { return l.path }

// Append assigns the next seq, builds the record with it, and writes it as
// one line, synced to disk before returning.
func (l *Log) Append(build func(seq int) any) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	held, err := filelock.Acquire(l.path + ".lock")
	if err != nil {
		return 0, err
	}
	defer held.Release()

	file, err := os.OpenFile(l.path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	last, torn, err := lastSeq(file)
	if err != nil {
		return 0, err
	}
	seq := last + 1
	line, err := json.Marshal(build(seq))
	if err != nil {
		return 0, err
	}
	if torn {
		// Finish a line a killed writer left without its newline.
		line = append([]byte("\n"), line...)
	}
	if _, err := file.Write(append(line, '\n')); err != nil {
		return 0, err
	}
	return seq, file.Sync()
}

// Read returns every complete record in the file at path, decoded into T,
// in file order. A missing file is empty; a torn last line is skipped.
func Read[T any](path string) ([]T, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var out []T
	reader := bufio.NewReader(file)
	for {
		line, err := reader.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 && err == nil {
			var record T
			if json.Unmarshal(line, &record) == nil {
				out = append(out, record)
			}
		}
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

// lastSeq finds the seq of the last complete record, scanning back from the
// end, and whether the file ends in a torn line.
func lastSeq(file *os.File) (int, bool, error) {
	info, err := file.Stat()
	if err != nil {
		return 0, false, err
	}
	size := info.Size()
	if size == 0 {
		return 0, false, nil
	}
	const chunk = 64 << 10
	var tail []byte
	torn := false
	for end := size; end > 0; {
		start := end - chunk
		if start < 0 {
			start = 0
		}
		buffer := make([]byte, end-start)
		if _, err := file.ReadAt(buffer, start); err != nil && err != io.EOF {
			return 0, false, err
		}
		tail = append(buffer, tail...)
		if end == size {
			torn = tail[len(tail)-1] != '\n'
		}
		// Try each complete line from the end until one decodes.
		lines := bytes.Split(bytes.TrimRight(tail, "\n"), []byte("\n"))
		for i := len(lines) - 1; i >= 0; i-- {
			if i == 0 && start > 0 {
				break // may be cut off; read further back
			}
			if i == len(lines)-1 && torn {
				continue
			}
			var record struct {
				Seq int `json:"seq"`
			}
			if json.Unmarshal(lines[i], &record) == nil && record.Seq > 0 {
				return record.Seq, torn, nil
			}
		}
		end = start
	}
	return 0, torn, nil
}
