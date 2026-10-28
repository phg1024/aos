//go:build darwin || linux

package render

import (
	"fmt"
	"io"
	"os"
	"syscall"
)

// LineEditor reads lines from a terminal with in-place editing, slash
// command highlighting and completion, and input history.
type LineEditor struct {
	ui       Palette
	in       *os.File
	commands []Command
	history  []string
}

func NewLineEditor(ui Palette, in *os.File, commands []Command) *LineEditor {
	return &LineEditor{ui: ui, in: in, commands: commands}
}

// ReadLine shows prompt and returns the submitted line. It returns io.EOF on
// Ctrl-D at an empty line, and ErrLineEditorUnavailable when input is not a
// terminal or colour output is off.
func (e *LineEditor) ReadLine(prompt string) (string, error) {
	if !e.ui.Enabled {
		return "", ErrLineEditorUnavailable
	}
	fd := int(e.in.Fd())
	restore, err := enterTermMode(fd, func(mode *syscall.Termios) {
		mode.Lflag &^= syscall.ICANON | syscall.ECHO | syscall.IEXTEN
		mode.Cc[syscall.VMIN] = 1
		mode.Cc[syscall.VTIME] = 0
	})
	if err != nil {
		return "", ErrLineEditorUnavailable
	}
	defer restore()

	var buffer lineBuffer
	historyIndex := len(e.history)
	var draft string
	redraw := func(showHint bool) {
		width, _ := TerminalSize()
		row, column := renderInput(e.ui, prompt, buffer, e.commands, width, showHint)
		move := "\r"
		if column > 0 {
			move += fmt.Sprintf("\x1b[%dC", column)
		}
		fmt.Fprint(os.Stdout, "\r\x1b[2K"+row+move)
	}
	redraw(true)

	pending := []byte{}
	chunk := make([]byte, 256)
	for {
		n, err := syscall.Read(fd, chunk)
		if err == syscall.EINTR || err == syscall.EAGAIN {
			continue
		}
		if err != nil || n == 0 {
			return "", io.EOF
		}
		var keys []key
		keys, pending = decodeKeys(append(pending, chunk[:n]...))
		for _, k := range keys {
			switch k.kind {
			case keyEnter:
				line := buffer.String()
				redraw(false)
				fmt.Fprint(os.Stdout, "\r\n")
				if line != "" && (len(e.history) == 0 || e.history[len(e.history)-1] != line) {
					e.history = append(e.history, line)
				}
				return line, nil
			case keyEOF:
				if len(buffer.text) == 0 {
					fmt.Fprint(os.Stdout, "\r\n")
					return "", io.EOF
				}
				buffer.deleteForward()
			case keyRune:
				buffer.insert(k.r)
			case keyBackspace:
				buffer.backspace()
			case keyDelete:
				buffer.deleteForward()
			case keyTab:
				buffer.complete(e.commands)
			case keyLeft:
				buffer.left()
			case keyRight:
				buffer.right()
			case keyHome:
				buffer.cursor = 0
			case keyEnd:
				buffer.cursor = len(buffer.text)
			case keyKillToStart:
				buffer.killToStart()
			case keyKillWord:
				buffer.killWord()
			case keyUp:
				if historyIndex > 0 {
					if historyIndex == len(e.history) {
						draft = buffer.String()
					}
					historyIndex--
					buffer.set(e.history[historyIndex])
				}
			case keyDown:
				if historyIndex < len(e.history) {
					historyIndex++
					if historyIndex == len(e.history) {
						buffer.set(draft)
					} else {
						buffer.set(e.history[historyIndex])
					}
				}
			}
		}
		redraw(true)
	}
}
