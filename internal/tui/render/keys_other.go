//go:build !darwin && !linux

package render

import "os"

// WatchEsc is unsupported on this platform; runs can only be stopped with Ctrl-C.
func WatchEsc(in *os.File, onEsc func()) (stop func()) {
	return func() {}
}
