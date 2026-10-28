//go:build !unix

package filelock

import "os"

// Without flock, locks coordinate nothing between processes; see the
// package doc.
func lock(file *os.File, wait bool) error { return nil }

func unlock(file *os.File) {}

// Supported reports whether locks coordinate separate processes here.
const Supported = false
