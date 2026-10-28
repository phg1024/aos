//go:build !darwin && !linux

package render

import "os"

// LineEditor is unavailable on this platform; callers fall back to plain
// line reading.
type LineEditor struct{}

func NewLineEditor(ui Palette, in *os.File, commands []Command) *LineEditor {
	return &LineEditor{}
}

func (e *LineEditor) ReadLine(prompt string) (string, error) {
	return "", ErrLineEditorUnavailable
}
