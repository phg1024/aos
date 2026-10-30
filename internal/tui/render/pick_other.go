//go:build !darwin && !linux

package render

import "os"

// Pick is unavailable on this platform; callers fall back to a numbered
// list.
func Pick(ui Palette, in *os.File, title string, items []string) (int, error) {
	return -1, ErrLineEditorUnavailable
}
