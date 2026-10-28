package render

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

// ErrLineEditorUnavailable means input is not an interactive terminal; the
// caller should fall back to plain line reading.
var ErrLineEditorUnavailable = errors.New("line editor needs an interactive terminal")

// Command is a slash command the line editor highlights and completes.
type Command struct {
	// Name includes the leading slash, for example "/help".
	Name    string
	Summary string
}

// MatchCommands returns the commands, in registry order, whose names start
// with the command word typed so far. Once arguments follow the word only an
// exact name matches. Input that is not a slash command matches nothing.
func MatchCommands(commands []Command, input string) []Command {
	word, _, hasArgs := strings.Cut(input, " ")
	if !strings.HasPrefix(word, "/") {
		return nil
	}
	var matches []Command
	for _, command := range commands {
		if command.Name == word || (!hasArgs && strings.HasPrefix(command.Name, word)) {
			matches = append(matches, command)
		}
	}
	return matches
}

// LookupCommand returns the command named by the first word of input.
func LookupCommand(commands []Command, input string) (Command, bool) {
	word, _, _ := strings.Cut(strings.TrimSpace(input), " ")
	for _, command := range commands {
		if command.Name == word {
			return command, true
		}
	}
	return Command{}, false
}

// lineBuffer is the text being edited and the cursor position, in runes.
type lineBuffer struct {
	text   []rune
	cursor int
}

func (b *lineBuffer) String() string { return string(b.text) }

func (b *lineBuffer) set(value string) {
	b.text = []rune(value)
	b.cursor = len(b.text)
}

func (b *lineBuffer) insert(r rune) {
	b.text = append(b.text, 0)
	copy(b.text[b.cursor+1:], b.text[b.cursor:])
	b.text[b.cursor] = r
	b.cursor++
}

func (b *lineBuffer) backspace() {
	if b.cursor == 0 {
		return
	}
	b.text = append(b.text[:b.cursor-1], b.text[b.cursor:]...)
	b.cursor--
}

func (b *lineBuffer) deleteForward() {
	if b.cursor < len(b.text) {
		b.text = append(b.text[:b.cursor], b.text[b.cursor+1:]...)
	}
}

func (b *lineBuffer) left() {
	if b.cursor > 0 {
		b.cursor--
	}
}

func (b *lineBuffer) right() {
	if b.cursor < len(b.text) {
		b.cursor++
	}
}

// killToStart deletes everything before the cursor (Ctrl-U).
func (b *lineBuffer) killToStart() {
	b.text = append([]rune(nil), b.text[b.cursor:]...)
	b.cursor = 0
}

// killWord deletes the word before the cursor and the spaces after it (Ctrl-W).
func (b *lineBuffer) killWord() {
	start := b.cursor
	for start > 0 && b.text[start-1] == ' ' {
		start--
	}
	for start > 0 && b.text[start-1] != ' ' {
		start--
	}
	b.text = append(b.text[:start], b.text[b.cursor:]...)
	b.cursor = start
}

// complete extends a partly typed command word to the matches' longest
// common prefix, adding a space once a single command is left.
func (b *lineBuffer) complete(commands []Command) {
	if b.cursor != len(b.text) {
		return
	}
	matches := MatchCommands(commands, b.String())
	if len(matches) == 0 || strings.Contains(b.String(), " ") {
		return
	}
	if len(matches) == 1 {
		b.set(matches[0].Name + " ")
		return
	}
	prefix := matches[0].Name
	for _, match := range matches[1:] {
		for !strings.HasPrefix(match.Name, prefix) {
			prefix = prefix[:len(prefix)-1]
		}
	}
	if len(prefix) > len(b.String()) {
		b.set(prefix)
	}
}

type keyKind int

const (
	keyRune keyKind = iota
	keyEnter
	keyBackspace
	keyDelete
	keyTab
	keyLeft
	keyRight
	keyUp
	keyDown
	keyHome
	keyEnd
	keyKillToStart
	keyKillWord
	keyEOF
	keyIgnored
)

type key struct {
	kind keyKind
	r    rune
}

// decodeKeys turns raw terminal input into keys. It returns trailing bytes
// that may start an unfinished UTF-8 rune or escape sequence, to be retried
// with the next read.
func decodeKeys(input []byte) ([]key, []byte) {
	var keys []key
	for len(input) > 0 {
		c := input[0]
		switch {
		case c == '\r' || c == '\n':
			keys = append(keys, key{kind: keyEnter})
			input = input[1:]
		case c == 0x7f || c == 0x08:
			keys = append(keys, key{kind: keyBackspace})
			input = input[1:]
		case c == '\t':
			keys = append(keys, key{kind: keyTab})
			input = input[1:]
		case c == 0x01:
			keys = append(keys, key{kind: keyHome})
			input = input[1:]
		case c == 0x05:
			keys = append(keys, key{kind: keyEnd})
			input = input[1:]
		case c == 0x15:
			keys = append(keys, key{kind: keyKillToStart})
			input = input[1:]
		case c == 0x17:
			keys = append(keys, key{kind: keyKillWord})
			input = input[1:]
		case c == 0x04:
			keys = append(keys, key{kind: keyEOF})
			input = input[1:]
		case c == keyEsc:
			k, size, complete := decodeEscape(input)
			if !complete {
				return keys, input
			}
			keys = append(keys, k)
			input = input[size:]
		case c < 0x20:
			keys = append(keys, key{kind: keyIgnored})
			input = input[1:]
		default:
			if !utf8.FullRune(input) {
				return keys, input
			}
			r, size := utf8.DecodeRune(input)
			if r == utf8.RuneError {
				keys = append(keys, key{kind: keyIgnored})
			} else {
				keys = append(keys, key{kind: keyRune, r: r})
			}
			input = input[size:]
		}
	}
	return keys, nil
}

// decodeEscape reads one escape sequence starting at input[0] == Esc. A lone
// Esc at the end of a read is taken as the Esc key itself.
func decodeEscape(input []byte) (key, int, bool) {
	if len(input) == 1 {
		return key{kind: keyIgnored}, 1, true
	}
	switch input[1] {
	case 'O':
		if len(input) < 3 {
			return key{}, 0, false
		}
		return key{kind: escapeFinal(input[2], "")}, 3, true
	case '[':
		for i := 2; i < len(input); i++ {
			if input[i] >= 0x40 && input[i] <= 0x7e {
				return key{kind: escapeFinal(input[i], string(input[2:i]))}, i + 1, true
			}
		}
		return key{}, 0, false
	default:
		// Alt+key: drop the Esc and let the key through on its own.
		return key{kind: keyIgnored}, 1, true
	}
}

func escapeFinal(final byte, params string) keyKind {
	switch final {
	case 'A':
		return keyUp
	case 'B':
		return keyDown
	case 'C':
		return keyRight
	case 'D':
		return keyLeft
	case 'H':
		return keyHome
	case 'F':
		return keyEnd
	case '~':
		switch params {
		case "1", "7":
			return keyHome
		case "4", "8":
			return keyEnd
		case "3":
			return keyDelete
		}
	}
	return keyIgnored
}

// inputFrame is the one write that replaces a drawn input with new rows:
// go up to the first of the drawnRows old rows, clear them, write rows, and
// put the cursor at (cursorRow, cursorCol). Writing past the bottom of the
// scroll region scrolls it, so the pinned footer is not disturbed.
func inputFrame(drawnRows, oldCursorRow int, rows []string, cursorRow, cursorCol int) string {
	var frame strings.Builder
	if oldCursorRow > 0 {
		fmt.Fprintf(&frame, "\x1b[%dA", oldCursorRow)
	}
	frame.WriteString("\r")
	for i := 0; i < drawnRows; i++ {
		frame.WriteString("\x1b[2K")
		if i < drawnRows-1 {
			frame.WriteString("\x1b[B")
		}
	}
	if drawnRows > 1 {
		fmt.Fprintf(&frame, "\x1b[%dA", drawnRows-1)
	}
	frame.WriteString(strings.Join(rows, "\r\n"))
	if up := len(rows) - 1 - cursorRow; up > 0 {
		fmt.Fprintf(&frame, "\x1b[%dA", up)
	}
	frame.WriteString("\r")
	if cursorCol > 0 {
		fmt.Fprintf(&frame, "\x1b[%dC", cursorCol)
	}
	return frame.String()
}

// Styles of the characters in the input line.
const (
	stylePlain = iota
	styleCommandName
	styleCommandPartial
	styleCommandUnknown
)

func applyStyle(ui Palette, style int, text string) string {
	switch style {
	case styleCommandName:
		return ui.CommandName(text)
	case styleCommandPartial:
		return ui.CommandPartial(text)
	case styleCommandUnknown:
		return ui.CommandUnknown(text)
	}
	return text
}

// layoutInput lays the prompt and input out over as many rows of width
// columns as they need, so long input stays fully visible. The command word
// is highlighted, and with showHint and the cursor at the end a dim
// completion hint follows, cut to the room left on the last row. It returns
// the rows and the cursor's row and column. A wide rune never splits across
// rows. width <= 0 means no wrapping.
func layoutInput(ui Palette, prompt string, buffer lineBuffer, commands []Command, width int, showHint bool) (rows []string, cursorRow, cursorCol int) {
	text := buffer.text
	limit := width
	if limit <= 0 {
		limit = 1 << 30
	}
	input := buffer.String()
	matches := MatchCommands(commands, input)
	styles := make([]int, len(text))
	if word, _, _ := strings.Cut(input, " "); strings.HasPrefix(word, "/") {
		style := styleCommandUnknown
		switch {
		case len(matches) > 0 && matches[0].Name == word:
			style = styleCommandName
		case len(matches) > 0:
			style = styleCommandPartial
		}
		for i := range []rune(word) {
			styles[i] = style
		}
	}

	var line strings.Builder
	line.WriteString(prompt)
	col := ansi.StringWidth(prompt)
	var run []rune
	runStyle := stylePlain
	flush := func() {
		if len(run) > 0 {
			line.WriteString(applyStyle(ui, runStyle, string(run)))
			run = run[:0]
		}
	}
	newRow := func() {
		flush()
		rows = append(rows, line.String())
		line.Reset()
		col = 0
	}
	for i, r := range text {
		w := ansi.StringWidth(string(r))
		if col+w > limit && col > 0 {
			newRow()
		}
		if i == buffer.cursor {
			cursorRow, cursorCol = len(rows), col
		}
		if styles[i] != runStyle {
			flush()
			runStyle = styles[i]
		}
		run = append(run, r)
		col += w
	}
	if buffer.cursor == len(text) {
		if col >= limit {
			newRow()
		}
		cursorRow, cursorCol = len(rows), col
		if showHint {
			if hint := commandHint(input, matches); hint != "" {
				if room := limit - col; room > 3 {
					flush()
					line.WriteString(ui.Hint(ansi.Truncate(hint, room, "…")))
				}
			}
		}
	}
	flush()
	rows = append(rows, line.String())
	return rows, cursorRow, cursorCol
}

// commandHint is the rest of the best match, its summary, and any other
// candidates, as plain text for the caller to dim.
func commandHint(input string, matches []Command) string {
	if len(matches) == 0 || strings.Contains(input, " ") {
		return ""
	}
	best := matches[0]
	hint := strings.TrimPrefix(best.Name, input) + "  " + best.Summary
	if len(matches) > 1 {
		names := make([]string, 0, len(matches)-1)
		for _, match := range matches[1:] {
			names = append(names, match.Name)
		}
		hint += "  · also " + strings.Join(names, " ")
	}
	return hint
}
