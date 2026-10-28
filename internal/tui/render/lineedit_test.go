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
	row, column := renderInput(Palette{}, "you> ", b, testCommands, width, hint)
	return row, column
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
	b.set("/help")
	if row, _ := renderInput(colored, "you> ", b, testCommands, 120, true); !strings.Contains(row, colored.CommandName("/help")) {
		t.Fatalf("exact command should be highlighted: %q", row)
	}
	b.set("/nope x")
	if row, _ := renderInput(colored, "you> ", b, testCommands, 120, true); !strings.Contains(row, colored.CommandUnknown("/nope")) {
		t.Fatalf("unknown command should be marked: %q", row)
	}
	b.set("/s")
	if row, _ := renderInput(colored, "you> ", b, testCommands, 120, true); !strings.Contains(row, colored.CommandPartial("/s")) {
		t.Fatalf("partial command should be marked: %q", row)
	}
}

func TestRenderInputCursorWithWideTextAndScroll(t *testing.T) {
	var b lineBuffer
	b.set("你好 world")
	b.cursor = 2
	if _, column := plainInput(b, 120, true); column != 5+4 {
		t.Fatalf("cursor after two wide runes should be at column 9, got %d", column)
	}

	b.set(strings.Repeat("abcdefghij", 10))
	row, column := plainInput(b, 40, true)
	if ansi.StringWidth(row) > 39 || column > 39 || !strings.HasSuffix(row, "ghij") {
		t.Fatalf("long input should scroll to keep the cursor visible: width %d column %d row %q", ansi.StringWidth(row), column, row)
	}
}
