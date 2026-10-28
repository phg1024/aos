package render

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

var testCommands = []Command{
	{Name: "/help", Summary: "list commands"},
	{Name: "/state", Summary: "show state"},
	{Name: "/stop", Summary: "stop it"},
	{Name: "/quit", Summary: "exit"},
}

func TestDecodeKeys(t *testing.T) {
	keys, rest := decodeKeys([]byte("a\x1b[D\x1b[C\x1b[A\x1bOB\x1b[3~\x1b[H\x1b[4~\t\x7f\x15\x17\x04\r"))
	want := []keyKind{keyRune, keyLeft, keyRight, keyUp, keyDown, keyDelete, keyHome, keyEnd, keyTab, keyBackspace, keyKillToStart, keyKillWord, keyEOF, keyEnter}
	if len(rest) != 0 || len(keys) != len(want) {
		t.Fatalf("decoded %d keys (rest %q), want %d", len(keys), rest, len(want))
	}
	for i, k := range keys {
		if k.kind != want[i] {
			t.Fatalf("key %d = %v, want %v", i, k.kind, want[i])
		}
	}
}

func TestDecodeKeysKeepsSplitInput(t *testing.T) {
	han := []byte("你")
	keys, rest := decodeKeys(append([]byte("x"), han[:2]...))
	if len(keys) != 1 || string(rest) != string(han[:2]) {
		t.Fatalf("split rune should wait for more bytes, got keys=%v rest=%q", keys, rest)
	}
	keys, rest = decodeKeys(append(rest, han[2:]...))
	if len(keys) != 1 || keys[0].r != '你' || len(rest) != 0 {
		t.Fatalf("completed rune not decoded: %v %q", keys, rest)
	}
	keys, rest = decodeKeys([]byte("\x1b["))
	if len(keys) != 0 || string(rest) != "\x1b[" {
		t.Fatalf("unfinished escape should wait, got %v %q", keys, rest)
	}
}

func TestLineBufferEditing(t *testing.T) {
	var b lineBuffer
	for _, r := range "hello world" {
		b.insert(r)
	}
	b.killWord()
	if b.String() != "hello " {
		t.Fatalf("killWord: %q", b.String())
	}
	b.cursor = 0
	b.insert('>')
	b.left()
	b.deleteForward()
	if b.String() != "hello " || b.cursor != 0 {
		t.Fatalf("insert/delete at start: %q cursor %d", b.String(), b.cursor)
	}
	b.cursor = 3
	b.killToStart()
	if b.String() != "lo " || b.cursor != 0 {
		t.Fatalf("killToStart: %q", b.String())
	}
}

func TestCompleteCommands(t *testing.T) {
	var b lineBuffer
	b.set("/h")
	b.complete(testCommands)
	if b.String() != "/help " {
		t.Fatalf("single match should complete with a space, got %q", b.String())
	}
	b.set("/s")
	b.complete(testCommands)
	if b.String() != "/st" {
		t.Fatalf("several matches should complete their common prefix, got %q", b.String())
	}
	b.set("hello /h")
	b.complete(testCommands)
	if b.String() != "hello /h" {
		t.Fatalf("plain text must not complete, got %q", b.String())
	}
}

func TestMatchAndLookupCommands(t *testing.T) {
	if got := MatchCommands(testCommands, "/st"); len(got) != 2 || got[0].Name != "/state" {
		t.Fatalf("prefix match: %v", got)
	}
	if got := MatchCommands(testCommands, "/state now"); len(got) != 1 {
		t.Fatalf("with arguments only the exact command matches: %v", got)
	}
	if got := MatchCommands(testCommands, "/sta now"); len(got) != 0 {
		t.Fatalf("a partial word with arguments matches nothing: %v", got)
	}
	if got := MatchCommands(testCommands, "hello"); got != nil {
		t.Fatalf("plain text: %v", got)
	}
	if command, ok := LookupCommand(testCommands, " /quit "); !ok || command.Name != "/quit" {
		t.Fatalf("lookup: %v %v", command, ok)
	}
}

func plainInput(b lineBuffer, width int, hint bool) (string, int) {
	rows, _, column := layoutInput(Palette{}, "you> ", b, testCommands, width, hint)
	return strings.Join(rows, "\n"), column
}

func TestRenderInputHintsAndHighlights(t *testing.T) {
	var b lineBuffer
	b.set("/st")
	row, column := plainInput(b, 120, true)
	if row != "you> /state  show state  · also /stop" || column != 8 {
		t.Fatalf("partial command row %q column %d", row, column)
	}
	if row, _ := plainInput(b, 120, false); row != "you> /st" {
		t.Fatalf("submitted rows drop the hint, got %q", row)
	}

	colored := Palette{Enabled: true}
	render := func(text string) string {
		var buffer lineBuffer
		buffer.set(text)
		rows, _, _ := layoutInput(colored, "you> ", buffer, testCommands, 120, true)
		return strings.Join(rows, "\n")
	}
	if row := render("/help"); !strings.Contains(row, colored.CommandName("/help")) {
		t.Fatalf("exact command should be highlighted: %q", row)
	}
	if row := render("/nope x"); !strings.Contains(row, colored.CommandUnknown("/nope")) {
		t.Fatalf("unknown command should be marked: %q", row)
	}
	if row := render("/s"); !strings.Contains(row, colored.CommandPartial("/s")) {
		t.Fatalf("partial command should be marked: %q", row)
	}
}

// Long input wraps onto further rows instead of scrolling out of view.
func TestLayoutInputWrapsLongText(t *testing.T) {
	var b lineBuffer
	b.set(strings.Repeat("abcdefghij", 10))
	rows, row, col := layoutInput(Palette{}, "you> ", b, testCommands, 40, true)
	if strings.Join(rows, "") != "you> "+strings.Repeat("abcdefghij", 10) {
		t.Fatalf("wrapping must keep every character: %q", rows)
	}
	for i, line := range rows[:len(rows)-1] {
		if ansi.StringWidth(line) != 40 {
			t.Fatalf("row %d is %d columns wide, want 40: %q", i, ansi.StringWidth(line), line)
		}
	}
	// 5 prompt columns + 100 runes = 105 columns: rows of 40, 40, 25.
	if len(rows) != 3 || row != 2 || col != 25 {
		t.Fatalf("got %d rows, cursor at row %d col %d", len(rows), row, col)
	}
	b.cursor = 40
	if _, row, col := layoutInput(Palette{}, "you> ", b, testCommands, 40, true); row != 1 || col != 5 {
		t.Fatalf("cursor in the middle at rune 40 should be row 1 col 5, got %d %d", row, col)
	}
}

func TestLayoutInputKeepsWideRunesWhole(t *testing.T) {
	var b lineBuffer
	b.set("你好世界")
	rows, row, col := layoutInput(Palette{}, "you> ", b, testCommands, 10, false)
	// "you> " is 5 columns; 你好 fill to 9, 世 (2 columns) would cross 10 and moves down.
	if len(rows) != 2 || rows[0] != "you> 你好" || rows[1] != "世界" || row != 1 || col != 4 {
		t.Fatalf("got rows %q cursor %d %d", rows, row, col)
	}
	b.cursor = 2
	if _, row, col := layoutInput(Palette{}, "you> ", b, testCommands, 10, false); row != 1 || col != 0 {
		t.Fatalf("cursor before 世 should be at the start of row 1, got %d %d", row, col)
	}
}

func TestLayoutInputCursorAtRowEndStartsNewRow(t *testing.T) {
	var b lineBuffer
	b.set("12345")
	rows, row, col := layoutInput(Palette{}, "you> ", b, testCommands, 10, false)
	if len(rows) != 2 || rows[1] != "" || row != 1 || col != 0 {
		t.Fatalf("a full row should leave the cursor on a fresh row, got %q %d %d", rows, row, col)
	}
}

func TestLayoutInputCutsHintToRoom(t *testing.T) {
	var b lineBuffer
	b.set("/st")
	rows, _, _ := layoutInput(Palette{}, "you> ", b, testCommands, 20, true)
	if len(rows) != 1 || ansi.StringWidth(rows[0]) > 20 || !strings.HasPrefix(rows[0], "you> /state") || !strings.HasSuffix(rows[0], "…") {
		t.Fatalf("the hint should be cut to the row, got %q", rows)
	}
}

func TestInputFrameClearsOldRowsAndPlacesCursor(t *testing.T) {
	frame := inputFrame(3, 1, []string{"you> abc", "def"}, 1, 2)
	want := "\x1b[1A\r\x1b[2K\x1b[B\x1b[2K\x1b[B\x1b[2K\x1b[2A" + "you> abc\r\ndef" + "\r\x1b[2C"
	if frame != want {
		t.Fatalf("frame = %q\nwant    %q", frame, want)
	}
	if frame := inputFrame(1, 0, []string{"you> "}, 0, 5); frame != "\r\x1b[2Kyou> \r\x1b[5C" {
		t.Fatalf("single-row frame = %q", frame)
	}
}
