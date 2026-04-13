//go:build !darwin && !linux && !freebsd && !netbsd && !openbsd && !dragonfly && !solaris

package render

import (
	"os"
	"strconv"
)

func TerminalSize() (width, height int) {
	return fallbackTerminalSize()
}

func fallbackTerminalSize() (width, height int) {
	width = parsePositiveEnv("COLUMNS", 120)
	height = parsePositiveEnv("LINES", 40)
	return width, height
}

func parsePositiveEnv(name string, fallback int) int {
	if value, err := strconv.Atoi(os.Getenv(name)); err == nil && value > 0 {
		return value
	}
	return fallback
}
