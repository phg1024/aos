//go:build darwin || linux

package render

import (
	"fmt"
	"os"
	"syscall"
)

// Pick shows items under title and lets the user choose one with the arrow
// keys (or j/k, or its number) and Enter. It returns the chosen index, or
// -1 when the user cancels with Esc or q. It returns
// ErrLineEditorUnavailable when input is not a terminal or colour output
// is off.
func Pick(ui Palette, in *os.File, title string, items []string) (int, error) {
	if !ui.Enabled {
		return -1, ErrLineEditorUnavailable
	}
	if len(items) == 0 {
		return -1, nil
	}
	fd := int(in.Fd())
	restore, err := enterTermMode(fd, func(mode *syscall.Termios) {
		mode.Lflag &^= syscall.ICANON | syscall.ECHO | syscall.IEXTEN
		mode.Cc[syscall.VMIN] = 1
		mode.Cc[syscall.VTIME] = 0
	})
	if err != nil {
		return -1, ErrLineEditorUnavailable
	}
	defer restore()
	fmt.Fprint(os.Stdout, "\x1b[?25l")
	defer fmt.Fprint(os.Stdout, "\x1b[?25h")

	p := &picker{items: items}
	drawnRows := 1
	redraw := func() {
		_, height := TerminalSize()
		rows := p.rows(ui, title, height)
		fmt.Fprint(os.Stdout, inputFrame(drawnRows, 0, rows, 0, 0))
		drawnRows = len(rows)
	}
	redraw()
	pending := []byte{}
	chunk := make([]byte, 256)
	for {
		n, err := syscall.Read(fd, chunk)
		if err == syscall.EINTR || err == syscall.EAGAIN {
			continue
		}
		if err != nil || n == 0 {
			fmt.Fprint(os.Stdout, clearFrame(drawnRows))
			return -1, nil
		}
		var keys []key
		keys, pending = decodeKeys(append(pending, chunk[:n]...))
		for _, k := range keys {
			if done, chosen := p.handle(k); done {
				fmt.Fprint(os.Stdout, clearFrame(drawnRows))
				return chosen, nil
			}
		}
		redraw()
	}
}
