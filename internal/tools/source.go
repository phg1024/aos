package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
	"unicode/utf8"
)

// MaxSourceBytes caps a file published by path. Past it the store and every later
// read carry more than the agent can page through, so the refusal says so.
const MaxSourceBytes = 1 << 20

// ErrBadSource is a file a publish cannot take: not a regular file, not UTF-8,
// or over MaxSourceBytes.
var ErrBadSource = errors.New("publish_artifact cannot take this file")

// ReadSource returns the bytes of the file req's path names, for a publish that
// takes them from there instead of from what the model typed. It reaches only what
// read_file would, under the same check. A body is text, so a file that is not
// UTF-8 is refused: storing it would change its bytes.
func (r *Runtime) ReadSource(ctx context.Context, req Request) ([]byte, error) {
	if r.RecordedSource != nil {
		return r.RecordedSource(ctx, req)
	}
	path, err := requiredPath(req)
	if err != nil {
		return nil, err
	}
	if err := r.readDenied(path, req); err != nil {
		return nil, err
	}
	// Non-blocking, so that a FIFO is refused below and not waited on.
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s is not a regular file", ErrBadSource, path)
	}
	content, err := io.ReadAll(io.LimitReader(file, MaxSourceBytes+1))
	if err != nil {
		return nil, err
	}
	if len(content) > MaxSourceBytes {
		return nil, fmt.Errorf("%w: %s is over %d bytes", ErrBadSource, path, MaxSourceBytes)
	}
	if !utf8.Valid(content) {
		return nil, fmt.Errorf("%w: %s is not UTF-8 text", ErrBadSource, path)
	}
	return content, nil
}
