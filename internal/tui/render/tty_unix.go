//go:build darwin || linux || freebsd || netbsd || openbsd || dragonfly || solaris

package render

import (
	"os"
	"strconv"
	"syscall"
	"unsafe"
)

type winsize struct {
	Row    uint16
	Col    uint16
	Xpixel uint16
	Ypixel uint16
}

func TerminalSize() (width, height int) {
	ws := winsize{}
	_, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		os.Stdout.Fd(),
		uintptr(syscall.TIOCGWINSZ),
		uintptr(unsafe.Pointer(&ws)),
	)
	if errno == 0 && ws.Col > 0 && ws.Row > 0 {
		return int(ws.Col), int(ws.Row)
	}
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
