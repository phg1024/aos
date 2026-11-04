// Package framing keeps text from others apart from what the runtime itself
// says to a model. The prompt has one role for both, so the difference has
// to be in the text.
//
// A peer's message body is marked line by line (MarkBody), so no line of it
// can begin where a runtime line begins. Everything else the runtime writes
// opens with an Opener from one table. Text it quotes after that opening,
// such as a file a tool read, is left as it is, except that a later line
// beginning with an opener is escaped with a backslash (Escape). That
// escape stops imitation only: a model can still be persuaded by what it
// reads, and only capabilities bound what follows.
package framing

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Opener is how a runtime-written line begins. Only this package can make
// one, so a note cannot open with anything the escape does not know about.
type Opener struct{ text string }

func (o Opener) String() string { return o.text }

// The openers the runtime writes. Undelivered, ArchivedUnread and
// SubscribedTo are reserved ahead of the notices that will use them.
var (
	PeerHeader        = Opener{"message from "}
	BudgetSteps       = Opener{"budget steps="}
	BudgetTime        = Opener{"budget time="}
	BudgetTokens      = Opener{"budget tokens="}
	BudgetWarning     = Opener{"budget warning:"}
	RuntimeNotice     = Opener{"runtime notice:"}
	ToolError         = Opener{"tool_error "}
	ToolNotice        = Opener{"tool_notice "}
	ToolCancelled     = Opener{"tool_cancelled "}
	ToolInterrupted   = Opener{"tool_interrupted "}
	MessageRecorded   = Opener{"message_recorded "}
	DelegateResult    = Opener{"delegate_result "}
	DelegateCancelled = Opener{"delegate_cancelled "}
	MailSent          = Opener{"send:to="}
	MailPublished     = Opener{"publish:topic="}
	MailSubscribed    = Opener{"subscribe:topic="}
	ResultTruncated   = Opener{"[tool result truncated:"}
	Undelivered       = Opener{"undelivered:"}
	ArchivedUnread    = Opener{"archived unread:"}
	SubscribedTo      = Opener{"subscribed to:"}
)

var openers = []Opener{
	PeerHeader, BudgetSteps, BudgetTime, BudgetTokens, BudgetWarning, RuntimeNotice, ToolError, ToolNotice, ToolCancelled,
	ToolInterrupted, MessageRecorded, DelegateResult, DelegateCancelled, MailSent,
	MailPublished, MailSubscribed, ResultTruncated, Undelivered, ArchivedUnread, SubscribedTo,
}

// ToolResult opens a tool's result turn: its name, then the output map.
// A new tool needs no entry in the table, since reserved matches the form.
func ToolResult(tool string) Opener { return Opener{tool + ":"} }

// ReservedPrefixes lists the literal openers. Besides these, a turn number
// ("[#7]") and a tool result ("run:map[") are reserved by their form.
func ReservedPrefixes() []string {
	out := make([]string, len(openers))
	for i, opener := range openers {
		out[i] = opener.text
	}
	return out
}

// Compose is text as a turn opening with o. Text that already opens with it
// is returned byte for byte; other text gets the opener, so a turn cannot
// open any other way.
func Compose(o Opener, text string) string {
	if strings.HasPrefix(text, o.text) {
		return text
	}
	return o.text + text
}

// maxOpening is how much of a line reserved looks at, in visible bytes: more
// than any opener, and than any tool name in a result's opening.
const maxOpening = 128

// reserved reports whether line reads like a runtime line.
func reserved(line string) bool {
	// With and without what looks like a terminal sequence: "ESC[t" is one,
	// and also a hidden ESC before "[tool result truncated:".
	return opensReserved(visibleOpening(line, true)) || opensReserved(visibleOpening(line, false))
}

func opensReserved(line string) bool {
	if line == "" {
		return false
	}
	if isTurnNumber(line) || isToolResult(line) {
		return true
	}
	for _, opener := range openers {
		if strings.HasPrefix(line, opener.text) {
			return true
		}
	}
	return false
}

// visibleOpening is the start of line as a reader sees it: no invisible
// characters and no terminal escape sequences anywhere, no indentation or
// backslashes in front, and any space character as a plain space. A zero-width
// space before "runtime notice:" still reads as "runtime notice:".
func visibleOpening(line string, dropSequences bool) string {
	var out strings.Builder
	for i := 0; i < len(line) && out.Len() < maxOpening; {
		if n := terminalSequence(line[i:]); dropSequences && n > 0 {
			i += n
			continue
		}
		r, size := utf8.DecodeRuneInString(line[i:])
		chunk := line[i : i+size]
		i += size
		switch {
		case unicode.IsSpace(r):
			if out.Len() > 0 {
				out.WriteByte(' ')
			}
		case Invisible(r):
		case r == '\\' && out.Len() == 0:
		default:
			out.WriteString(chunk)
		}
	}
	return out.String()
}

// terminalSequence is the length of the ANSI control sequence (ESC [ ... )
// at the start of s, or 0.
func terminalSequence(s string) int {
	if !strings.HasPrefix(s, "\x1b[") {
		return 0
	}
	i := 2
	for i < len(s) && s[i] >= 0x30 && s[i] <= 0x3f { // parameters
		i++
	}
	for i < len(s) && s[i] >= 0x20 && s[i] <= 0x2f { // intermediates
		i++
	}
	if i < len(s) && s[i] >= 0x40 && s[i] <= 0x7e { // the final byte
		return i + 1
	}
	return 0
}

// Invisible reports a character with no mark of its own: a control or format
// character, a variation selector, or another default-ignorable one, such as
// a Hangul filler. Space characters are not included; they read as spaces.
func Invisible(r rune) bool {
	return unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r) ||
		unicode.Is(unicode.Variation_Selector, r) || unicode.Is(unicode.Other_Default_Ignorable_Code_Point, r)
}

// PlainLine reports whether text is one line a reader sees all of: valid
// UTF-8, no invisible characters, and no space character but the plain one.
func PlainLine(text string) bool {
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		if r == utf8.RuneError && size == 1 {
			return false
		}
		if Invisible(r) || (unicode.IsSpace(r) && r != ' ') {
			return false
		}
		i += size
	}
	return true
}

// Inline writes text so that it stays on one line and shows everything in
// it, for a name that goes in a line the runtime writes.
func Inline(text string) string {
	if PlainLine(text) {
		return text
	}
	var out strings.Builder
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			fmt.Fprintf(&out, `\x%02x`, text[i])
		case Invisible(r) || (unicode.IsSpace(r) && r != ' '):
			fmt.Fprintf(&out, `\u%04X`, r)
		default:
			out.WriteString(text[i : i+size])
		}
		i += size
	}
	return out.String()
}

// isTurnNumber matches "[#7]".
func isTurnNumber(line string) bool {
	rest, ok := strings.CutPrefix(line, "[#")
	if !ok {
		return false
	}
	digits := 0
	for digits < len(rest) && rest[digits] >= '0' && rest[digits] <= '9' {
		digits++
	}
	return digits > 0 && strings.HasPrefix(rest[digits:], "]")
}

// isToolResult matches "run:map[": a tool name, then a printed output map.
func isToolResult(line string) bool {
	name, rest, ok := strings.Cut(line, ":")
	if !ok || name == "" || !strings.HasPrefix(rest, "map[") {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !(c == '_' || c == '-' || c == '.' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
			return false
		}
	}
	return true
}

// breakLen is the length of the line boundary at the start of s, or 0.
// The set is what Python's str.splitlines breaks on, since a model may read
// any of these as a new line: \r\n, \r, \n, \v, \f, U+001C to U+001E, U+0085,
// U+2028 and U+2029.
func breakLen(s string) int {
	switch s[0] {
	case '\r':
		if len(s) > 1 && s[1] == '\n' {
			return 2
		}
		return 1
	case '\n', '\v', '\f', 0x1c, 0x1d, 0x1e:
		return 1
	case 0xc2:
		if strings.HasPrefix(s, "\u0085") {
			return 2
		}
	case 0xe2:
		if strings.HasPrefix(s, "\u2028") || strings.HasPrefix(s, "\u2029") {
			return 3
		}
	}
	return 0
}

// mapLines rewrites each line of text but the first when skipFirst is set,
// keeping every line boundary as it was.
func mapLines(text string, skipFirst bool, rewrite func(line string) string) string {
	var out strings.Builder
	first := true
	start := 0
	flush := func(end int) {
		line := text[start:end]
		if !first || !skipFirst {
			line = rewrite(line)
		}
		first = false
		out.WriteString(line)
	}
	for i := 0; i < len(text); {
		if n := breakLen(text[i:]); n > 0 {
			flush(i)
			out.WriteString(text[i : i+n])
			i += n
			start = i
			continue
		}
		i++
	}
	flush(len(text))
	return out.String()
}

// Escape quotes the lines of a runtime-written turn that begin like a
// runtime line, except the first, whose opening the runtime wrote: each gets
// a leading backslash. All other bytes are unchanged.
func Escape(turn string) string {
	if !strings.ContainsAny(turn, "\n\r\v\f\x1c\x1d\x1e\u0085\u2028\u2029") {
		return turn
	}
	return mapLines(turn, true, func(line string) string {
		if reserved(line) {
			return `\` + line
		}
		return line
	})
}

// Unescape removes the backslash Escape adds, from every line, for text a
// model copied out of a tool result.
func Unescape(text string) string {
	return mapLines(text, false, func(line string) string {
		if strings.HasPrefix(line, `\`) && reserved(line) {
			return line[1:]
		}
		return line
	})
}

// MarkBody renders a peer's message body as marked lines and counts them.
// Every line boundary becomes \n first, so the count is exact and no other
// character is left to read as a new line, and other control characters are
// written out visibly. An empty body has no lines.
func MarkBody(body string) (marked string, lines int) {
	if body == "" {
		return "", 0
	}
	var out strings.Builder
	out.WriteString("| ")
	lines = 1
	for i := 0; i < len(body); {
		if n := breakLen(body[i:]); n > 0 {
			out.WriteString("\n| ")
			lines++
			i += n
			continue
		}
		r, size := utf8.DecodeRuneInString(body[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			fmt.Fprintf(&out, `\x%02x`, body[i])
		case escaped(r):
			fmt.Fprintf(&out, `\u%04X`, r)
		default:
			out.WriteString(body[i : i+size])
		}
		i += size
	}
	return out.String(), lines
}

// escaped reports the characters a body may not carry as they are: control
// characters other than tab, and the bidirectional controls, which can make
// a quoted line display in another order.
func escaped(r rune) bool {
	switch {
	case r == '\t':
		return false
	case r < 0x20, r >= 0x7f && r <= 0x9f:
		return true
	case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
		return true
	}
	return false
}
