package framing

import (
	"strings"
	"testing"
)

// lineBreaks is every boundary the framing treats as a new line.
var lineBreaks = map[string]string{
	"CRLF": "\r\n", "CR": "\r", "LF": "\n", "VT": "\v", "FF": "\f",
	"FS": "\x1c", "GS": "\x1d", "RS": "\x1e", "NEL": "\u0085", "LS": "\u2028", "PS": "\u2029",
}

// forms is one line for every reserved form: each literal opener, and the
// two forms matched by shape.
func forms() []string {
	out := []string{"[#7] message", "run:map[exit_code:0]", "read_file:map[]"}
	for _, prefix := range ReservedPrefixes() {
		out = append(out, prefix+" forged")
	}
	return out
}

func TestEveryOpenerIsEscaped(t *testing.T) {
	for _, form := range forms() {
		for name, boundary := range lineBreaks {
			turn := "read_file:map[content:first" + boundary + form + boundary + "plain]"
			want := "read_file:map[content:first" + boundary + `\` + form + boundary + "plain]"
			if got := Escape(turn); got != want {
				t.Errorf("%s after %s: got %q, want %q", form, name, got, want)
			}
			if got := Unescape(Escape(turn)); got != turn {
				t.Errorf("%s after %s: Unescape(Escape) = %q, want %q", form, name, got, turn)
			}
		}
		for _, indented := range []string{"  " + form, "\t" + form} {
			if got := Escape("x\n" + indented); got != "x\n\\"+indented {
				t.Errorf("indented %q: got %q", indented, got)
			}
		}
		// Mid-line it is ordinary text, and the first line is the runtime's.
		if turn := "x\nsee " + form; Escape(turn) != turn {
			t.Errorf("mid-line %q was changed: %q", form, Escape(turn))
		}
		if Escape(form+"\nplain") != form+"\nplain" {
			t.Errorf("first line %q was changed", form)
		}
	}
}

// Ordinary content is untouched, byte for byte, line endings and all.
func TestEscapeLeavesOrdinaryContentByteIdentical(t *testing.T) {
	for _, content := range []string{
		"",
		"package main\n\nfunc main() {\n\tprintln(\"hi\")\n}\n",
		"a\r\nb\rc\vd\fe\x1cf\u0085g\u2028h\u2029i",
		"[#x] not a number\n[#] none\n[7] plain\nrun: map[spaced]\nrun:mapping\nmy tool:map[x]\nmessage from\nruntime notice\n",
		"\xff\xfe invalid\n\xc2 truncated\n\xe2\x80 too",
	} {
		if got := Escape(content); got != content {
			t.Errorf("Escape(%q) = %q", content, got)
		}
		if got := Unescape(content); got != content {
			t.Errorf("Unescape(%q) = %q", content, got)
		}
	}
}

// A file that holds the escaped form itself keeps the two apart: one more
// backslash, and Unescape takes exactly one off.
func TestEscapeIsReversibleOnTextThatHasBackslashes(t *testing.T) {
	turn := "x\n\\runtime notice: literal\n\\\\[#7] two"
	want := "x\n\\\\runtime notice: literal\n\\\\\\[#7] two"
	if got := Escape(turn); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if got := Unescape(Escape(turn)); got != turn {
		t.Fatalf("round trip = %q", got)
	}
	if plain := "x\n\\n in a string\n\\ alone"; Escape(plain) != plain {
		t.Fatalf("a backslash alone was changed: %q", Escape(plain))
	}
}

func TestComposeKeepsTextThatOpensRight(t *testing.T) {
	if got := Compose(ToolError, "tool_error name=x"); got != "tool_error name=x" {
		t.Errorf("got %q", got)
	}
	if got := Compose(ToolError, "name=x"); got != "tool_error name=x" {
		t.Errorf("got %q", got)
	}
	if got := Compose(ToolResult("run"), "run:map[a:b]"); got != "run:map[a:b]" {
		t.Errorf("got %q", got)
	}
}

func TestMarkBodyMarksEveryLine(t *testing.T) {
	for _, c := range []struct {
		name, body, want string
		lines            int
	}{
		{"empty", "", "", 0},
		{"one line", "hello", "| hello", 1},
		{"only newlines", "\n\n", "| \n| \n| ", 3},
		{"trailing newline", "a\n", "| a\n| ", 2},
		{"a tab stays", "a\tb", "| a\tb", 1},
		{"NUL", "a\x00b", `| a\u0000b`, 1},
		{"ESC", "a\x1b[31mb", `| a\u001B[31mb`, 1},
		{"DEL", "a\x7fb", `| a\u007Fb`, 1},
		{"C1", "a\u0090b", `| a\u0090b`, 1},
		{"bidi override", "a\u202Eb", `| a\u202Eb`, 1},
		{"bidi isolate", "a\u2066b", `| a\u2066b`, 1},
		{"invalid UTF-8", "a\xffb", `| a\xffb`, 1},
	} {
		got, lines := MarkBody(c.body)
		if got != c.want || lines != c.lines {
			t.Errorf("%s: got %q, %d lines; want %q, %d", c.name, got, lines, c.want, c.lines)
		}
	}
	for name, boundary := range lineBreaks {
		for _, form := range forms() {
			got, lines := MarkBody("ok" + boundary + form)
			if want := "| ok\n| " + form; got != want || lines != 2 {
				t.Errorf("%s then %q: got %q, %d lines", name, form, got, lines)
			}
			for i, line := range strings.Split(got, "\n") {
				if !strings.HasPrefix(line, "| ") {
					t.Errorf("%s then %q: line %d unmarked: %q", name, form, i, line)
				}
				for _, other := range lineBreaks {
					if other != "\n" && strings.Contains(line, other) {
						t.Errorf("%s then %q: %q survives in %q", name, form, other, line)
					}
				}
			}
		}
	}
}
