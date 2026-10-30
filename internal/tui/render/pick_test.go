package render

import (
	"strings"
	"testing"
)

// The arrow keys (or j/k, or a number) move the selection within the list,
// Enter picks it, and Esc or q cancels.
func TestPickerKeys(t *testing.T) {
	keys := func(input string) []key {
		decoded, rest := decodeKeys([]byte(input))
		if len(rest) != 0 {
			t.Fatalf("undecoded input %q", rest)
		}
		return decoded
	}
	for _, tc := range []struct {
		input  string
		chosen int
	}{
		{"\r", 0},
		{"\x1b[B\x1b[B\r", 2},
		{"\x1b[B\x1b[B\x1b[B\x1b[B\r", 3},
		{"\x1b[A\r", 0},
		{"jjk\r", 1},
		{"3\r", 2},
		{"9\r", 0},
		{"\x1b[F\x1b[H\r", 0},
		{"\x1b[F\r", 3},
		{"j\x1b", -1},
		{"q", -1},
	} {
		p := &picker{items: []string{"a", "b", "c", "d"}}
		done, chosen := false, 0
		for _, k := range keys(tc.input) {
			if done, chosen = p.handle(k); done {
				break
			}
		}
		if !done || chosen != tc.chosen {
			t.Fatalf("%q: done %t, chosen %d, want %d", tc.input, done, chosen, tc.chosen)
		}
	}
}

// A lone Esc is the Esc key, which the line editor ignores as before.
func TestLoneEscDecodesAsEscape(t *testing.T) {
	decoded, _ := decodeKeys([]byte{keyEsc})
	if len(decoded) != 1 || decoded[0].kind != keyEscape {
		t.Fatalf("decoded = %+v", decoded)
	}
}

// The picker marks the selected row and keeps it in view in a short
// terminal, saying how many rows are out of view.
func TestPickerRowsScroll(t *testing.T) {
	items := []string{"s1", "s2", "s3", "s4", "s5", "s6", "s7", "s8"}
	p := &picker{items: items}
	rows := p.rows(Palette{}, "Resume", 7)
	if len(rows) != 5 || !strings.Contains(rows[0], "Resume") || rows[1] != "❯ s1" || rows[2] != "  s2" || rows[4] != "   ↓ 5 more" {
		t.Fatalf("rows = %q", rows)
	}
	p.selected = 6
	rows = p.rows(Palette{}, "Resume", 7)
	if rows[1] != "   ↑ 4 more" || rows[len(rows)-2] != "❯ s7" || rows[len(rows)-1] != "   ↓ 1 more" {
		t.Fatalf("scrolled rows = %q", rows)
	}
	p.selected = 0
	rows = p.rows(Palette{}, "Resume", 7)
	if rows[1] != "❯ s1" {
		t.Fatalf("scrolling back up should show the selection: %q", rows)
	}
}
