package render

import (
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

var md = Palette{Enabled: true}

// plain renders src and strips the escape codes, leaving the layout.
func plain(src string, width int) string {
	return ansi.Strip(md.Markdown(src, width))
}

func plainLines(src string, width int) []string {
	return strings.Split(plain(src, width), "\n")
}

// assertClean checks every rendered line fits the width and leaves no style
// open, so a prefix on the next line is never painted.
func assertClean(t *testing.T, out string, width int) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if got := ansi.StringWidth(line); got > width {
			t.Errorf("line is %d cells wide, limit %d: %q", got, width, ansi.Strip(line))
		}
		if open := trackSGR("", line); open != "" {
			t.Errorf("line leaves style %q open: %q", open, line)
		}
	}
}

func TestMarkdownDisabledPaletteIsPassthrough(t *testing.T) {
	inputs := []string{
		"",
		"plain text",
		"# Title\n\n**bold** and `code`\r\n- item\n\n| a | b |\n|---|---|\n| 1 | 2 |\n",
		"```go\nunclosed\n",
		"trailing newline\n\n\n",
		"\ttabs\tand  spaces  ",
	}
	off := Palette{}
	for _, input := range inputs {
		if got := off.Markdown(input, 40); got != input {
			t.Errorf("Markdown changed %q to %q", input, got)
		}
		if got := off.AgentReply("agent> ", input); got != "agent> "+input {
			t.Errorf("AgentReply changed %q to %q", input, got)
		}
	}
}

func TestMarkdownHeadings(t *testing.T) {
	out := md.Markdown("# Top\n## Second ##\n### Third\n#hashtag\n# C#", 40)
	lines := strings.Split(ansi.Strip(out), "\n")
	want := []string{"Top", "Second", "Third", "#hashtag", "C#"}
	if strings.Join(lines, "|") != strings.Join(want, "|") {
		t.Fatalf("headings = %q, want %q", lines, want)
	}
	if !strings.Contains(out, "\033["+mdHeading1+"mTop") || !strings.Contains(out, "\033["+mdHeading2+"mSecond") {
		t.Fatalf("headings are not styled: %q", out)
	}
	assertClean(t, out, 40)
}

func TestMarkdownInlineStyles(t *testing.T) {
	out := md.Markdown("a **bold** b *it* c _it2_ d `x*y*` e ~~gone~~ f ***both***", 80)
	if got := ansi.Strip(out); got != "a bold b it c it2 d x*y* e gone f both" {
		t.Fatalf("stripped = %q", got)
	}
	for _, want := range []string{
		"\033[" + mdBold + "mbold",
		"\033[" + mdItalic + "mit",
		"\033[" + mdItalic + "mit2",
		"\033[" + mdCode + "mx*y*",
		"\033[" + mdStrike + "mgone",
		"\033[" + mdBoldItal + "mboth",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %q", want, out)
		}
	}
}

func TestMarkdownNestedEmphasisRestoresOuterStyle(t *testing.T) {
	out := md.Markdown("**bold *both* still**", 80)
	if ansi.Strip(out) != "bold both still" {
		t.Fatalf("stripped = %q", ansi.Strip(out))
	}
	// After the italic closes, bold is re-applied before " still".
	if !strings.Contains(out, sgrReset+"\033["+mdBold+"m still") {
		t.Fatalf("outer style not restored: %q", out)
	}
}

func TestMarkdownLeavesNonEmphasisAlone(t *testing.T) {
	cases := map[string]string{
		"snake_case_name and __init__ x":   "snake_case_name and init x",
		"2 * 3 * 4":                        "2 * 3 * 4",
		"~5 minutes":                       "~5 minutes",
		"**unclosed bold":                  "**unclosed bold",
		"`unclosed code":                   "`unclosed code",
		`\*escaped\* and \_too\_`:          "*escaped* and _too_",
		"[not a link] and [x]":             "[not a link] and [x]",
		"a < b > c":                        "a < b > c",
		"<https://example.com/a?b=1>":      "https://example.com/a?b=1",
		"```inline``` stays":               "inline stays",
		"use `` a`b `` here":               "use a`b here",
		"a *space before closer *":         "a *space before closer *",
		"[unclosed](https://example.com":   "[unclosed](https://example.com",
		"emoji 🎉 and **粗体** mixed":         "emoji 🎉 and 粗体 mixed",
		"**bold**followed and *it*alic ok": "boldfollowed and italic ok",
	}
	for input, want := range cases {
		if got := plain(input, 80); got != want {
			t.Errorf("%q rendered %q, want %q", input, got, want)
		}
	}
}

func TestMarkdownLinks(t *testing.T) {
	out := md.Markdown(`see [the docs](https://example.com/docs "Docs") and [https://x.io](https://x.io) and ![logo](img.png)`, 120)
	want := `see the docs (https://example.com/docs "Docs") and https://x.io and image: logo (img.png)`
	if got := ansi.Strip(out); got != want {
		t.Fatalf("links = %q\nwant    %q", got, want)
	}
	if !strings.Contains(out, "\033["+mdLink+"mthe docs") || !strings.Contains(out, "\033["+mdURL+"m(https://example.com/docs \"Docs\")") {
		t.Fatalf("link styles missing: %q", out)
	}
	if got := plain("[**bold** link](u)", 80); got != "bold link (u)" {
		t.Fatalf("styled link text = %q", got)
	}
}

func TestMarkdownParagraphKeepsLineBreaksAndWraps(t *testing.T) {
	lines := plainLines("Name: aos\nState: running\n\n"+strings.Repeat("word ", 20), 30)
	if lines[0] != "Name: aos" || lines[1] != "State: running" || lines[2] != "" {
		t.Fatalf("line breaks lost: %q", lines)
	}
	for _, line := range lines[3:] {
		if ansi.StringWidth(line) > 30 || strings.HasSuffix(line, " ") {
			t.Fatalf("bad wrapped line %q", line)
		}
	}
	if got := strings.Join(strings.Fields(strings.Join(lines[3:], " ")), " "); got != strings.TrimSpace(strings.Repeat("word ", 20)) {
		t.Fatalf("wrapping lost words: %q", got)
	}
}

func TestMarkdownWrapCarriesStyleAcrossLines(t *testing.T) {
	out := md.Markdown("**"+strings.TrimSpace(strings.Repeat("bold ", 10))+"**", 20)
	lines := strings.Split(out, "\n")
	if len(lines) < 2 {
		t.Fatalf("expected wrapping, got %q", out)
	}
	for _, line := range lines {
		if !strings.HasPrefix(line, "\033["+mdBold+"m") {
			t.Errorf("wrapped line lost bold: %q", line)
		}
	}
	assertClean(t, out, 20)
}

func TestMarkdownFencedCode(t *testing.T) {
	src := "before\n```go\nfunc main() {\n\tfmt.Println(\"**not bold**\")\n}\n```\nafter"
	lines := plainLines(src, 40)
	want := []string{
		"before",
		"╭─ go",
		"│ func main() {",
		"│     fmt.Println(\"**not bold**\")",
		"│ }",
		"╰─",
		"after",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("code block:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
	assertClean(t, md.Markdown(src, 40), 40)
}

func TestMarkdownCodeBlockHardWrapsLongLines(t *testing.T) {
	long := strings.Repeat("x", 50)
	lines := plainLines("~~~\n"+long+"\n~~~", 20)
	joined := ""
	for _, line := range lines[1 : len(lines)-1] {
		if !strings.HasPrefix(line, "│ ") || ansi.StringWidth(line) > 20 {
			t.Fatalf("bad code line %q", line)
		}
		joined += strings.TrimPrefix(line, "│ ")
	}
	if joined != long {
		t.Fatalf("code text changed: %q", joined)
	}
}

func TestMarkdownUnclosedFenceKeepsText(t *testing.T) {
	lines := plainLines("```python\nprint(1)\n\n# not a heading", 40)
	want := []string{"╭─ python", "│ print(1)", "│", "│ # not a heading", "╰─"}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("unclosed fence = %q", lines)
	}
}

func TestMarkdownBulletListsNest(t *testing.T) {
	src := "- one\n- two\n  - nested **bold**\n    - deeper\n- three\n  continued lazily"
	lines := plainLines(src, 40)
	want := []string{
		"• one",
		"• two",
		"  ◦ nested bold",
		"    ▪ deeper",
		"• three",
		"  continued lazily",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("list:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
}

func TestMarkdownOrderedListAlignsAndHangs(t *testing.T) {
	var src strings.Builder
	for i := 1; i <= 10; i++ {
		src.WriteString(strconv.Itoa(i) + ". item\n")
	}
	src.WriteString("11. " + strings.Repeat("long ", 8))
	lines := plainLines(src.String(), 20)
	if lines[0] != " 1. item" || lines[9] != "10. item" {
		t.Fatalf("numbers not right-aligned: %q", lines[:10])
	}
	for _, line := range lines[11:] {
		if !strings.HasPrefix(line, "    ") {
			t.Fatalf("continuation not hanging under the text: %q", lines[10:])
		}
	}
}

func TestMarkdownListWithCodeAndLooseItems(t *testing.T) {
	src := "1. Run:\n\n   ```bash\n   go test ./...\n\n   go vet\n   ```\n\n2. Done"
	lines := plainLines(src, 40)
	want := []string{
		"1. Run:",
		"",
		"   ╭─ bash",
		"   │ go test ./...",
		"   │",
		"   │ go vet",
		"   ╰─",
		"",
		"2. Done",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("list with code:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
}

func TestMarkdownListEndsAtUnindentedParagraph(t *testing.T) {
	lines := plainLines("- a\n- b\n\nAfter the list.", 40)
	want := []string{"• a", "• b", "", "After the list."}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got %q", lines)
	}
}

func TestMarkdownBlockQuote(t *testing.T) {
	src := "> quoted **text**\n> > nested\n>\n> - item"
	lines := plainLines(src, 40)
	want := []string{"│ quoted text", "│ │ nested", "│", "│ • item"}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("quote = %q", lines)
	}
	assertClean(t, md.Markdown(src, 40), 40)
}

func TestMarkdownRules(t *testing.T) {
	for _, rule := range []string{"---", "***", "_ _ _", " - - -"} {
		lines := plainLines("above\n\n"+rule+"\n\nbelow", 30)
		if len(lines) != 5 || lines[2] != strings.Repeat("─", 30) {
			t.Errorf("rule %q rendered %q", rule, lines)
		}
	}
}

func TestMarkdownTableAligns(t *testing.T) {
	src := "| Name | Count | Note |\n|:-----|------:|:----:|\n| a | 1 | x |\n| longer | 1234 | mid |"
	lines := plainLines(src, 80)
	want := []string{
		"┌────────┬───────┬──────┐",
		"│ Name   │ Count │ Note │",
		"├────────┼───────┼──────┤",
		"│ a      │     1 │  x   │",
		"│ longer │  1234 │ mid  │",
		"└────────┴───────┴──────┘",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("table:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
}

func TestMarkdownTableWideCharacters(t *testing.T) {
	src := "| 名前 | 说明 |\n|---|---|\n| 中文 | emoji 🎉 |\n| a | 한국어 텍스트 |"
	out := md.Markdown(src, 80)
	lines := strings.Split(ansi.Strip(out), "\n")
	width := ansi.StringWidth(lines[0])
	for _, line := range lines {
		if ansi.StringWidth(line) != width {
			t.Fatalf("rows differ in display width:\n%s", strings.Join(lines, "\n"))
		}
	}
	if !strings.Contains(lines[3], "│ 中文 │ emoji 🎉") {
		t.Fatalf("cell text missing: %q", lines[3])
	}
	assertClean(t, out, 80)
}

func TestMarkdownRaggedTable(t *testing.T) {
	src := "| a | b |\n|---|---|\n| only one |\n| 1 | 2 | 3 extra |\nno pipe ends it"
	lines := plainLines(src, 80)
	text := strings.Join(lines, "\n")
	for _, want := range []string{"only one", "3 extra", "no pipe ends it"} {
		if !strings.Contains(text, want) {
			t.Fatalf("ragged table lost %q:\n%s", want, text)
		}
	}
	width := ansi.StringWidth(lines[0])
	for _, line := range lines[:len(lines)-1] {
		if ansi.StringWidth(line) != width {
			t.Fatalf("ragged rows not padded:\n%s", text)
		}
	}
	if lines[len(lines)-1] != "no pipe ends it" {
		t.Fatalf("trailing paragraph = %q", lines[len(lines)-1])
	}
}

func TestMarkdownTableEscapedPipesAndCode(t *testing.T) {
	lines := plainLines("| expr | meaning |\n|---|---|\n| `a|b` | a or b \\| c |", 80)
	if !strings.Contains(lines[3], "│ a|b  │ a or b | c │") {
		t.Fatalf("pipes inside cells split the row: %q", lines[3])
	}
}

func TestMarkdownTableShrinksToWidth(t *testing.T) {
	src := "| key | description |\n|---|---|\n| k | " + strings.Repeat("a fairly long description ", 4) + "|"
	out := md.Markdown(src, 40)
	assertClean(t, out, 40)
	text := strings.Join(strings.Fields(strings.ReplaceAll(ansi.Strip(out), "│", " ")), " ")
	if !strings.Contains(text, strings.TrimSpace(strings.Repeat("a fairly long description ", 4))) {
		t.Fatalf("wrapped cell lost text: %q", text)
	}
}

func TestMarkdownWideCharacterWrapping(t *testing.T) {
	src := strings.Repeat("中文字符", 20)
	out := md.Markdown(src, 21)
	assertClean(t, out, 21)
	if got := strings.ReplaceAll(ansi.Strip(out), "\n", ""); got != src {
		t.Fatalf("CJK text changed while wrapping: %q", got)
	}
}

func TestMarkdownBreaksBetweenCJKCharacters(t *testing.T) {
	// A reply from a live session: before, the whole Chinese run after
	// "markdown" moved to its own line and left the line above half empty.
	src := "如果你看到的是带 | 的原始文本，说明这个界面不渲染 markdown，我可以改用纯文本对齐版（用空格/破折号排版），到哪都能看清。要纯文本版吗？"
	const width = 30
	out := md.Markdown(src, width)
	assertClean(t, out, width)
	lines := strings.Split(ansi.Strip(out), "\n")
	for i, line := range lines {
		first, _ := utf8.DecodeRuneInString(line)
		if strings.ContainsRune(closingPunct, first) {
			t.Errorf("line %d starts with closing punctuation: %q", i, line)
		}
		last, _ := utf8.DecodeLastRuneInString(line)
		if strings.ContainsRune(openingPunct, last) {
			t.Errorf("line %d ends with opening punctuation: %q", i, line)
		}
		// A line followed by CJK text is filled; a glued punctuation mark
		// or a wide character may leave up to three cells free. Before a
		// Latin word it may break early to keep the word whole.
		if i == len(lines)-1 {
			continue
		}
		if next, _ := utf8.DecodeRuneInString(lines[i+1]); breaksAround(next) && ansi.StringWidth(line) < width-3 {
			t.Errorf("line %d broke early (%d cells): %q", i, ansi.StringWidth(line), line)
		}
	}
	if joined := strings.Join(lines, ""); strings.ReplaceAll(joined, " ", "") != strings.ReplaceAll(src, " ", "") {
		t.Fatalf("text changed:\n%s", joined)
	}
}

func TestMarkdownKeepsLatinWordsWhole(t *testing.T) {
	lines := plainLines("中文 Burlingame 和 Redwood City，网址 https://example.com/a.b 不拆。", 16)
	for _, word := range []string{"Burlingame", "Redwood", "https://example.com/a.b"} {
		found := false
		for _, line := range lines {
			found = found || strings.Contains(line, word)
		}
		if !found && ansi.StringWidth(word) <= 16 {
			t.Errorf("%q was split: %q", word, lines)
		}
	}
	// Wider than a line, a word is cut between graphemes and kept whole.
	long := strings.Repeat("x", 40)
	if got := strings.Join(plainLines(long, 16), ""); got != long {
		t.Fatalf("long word = %q", got)
	}
}

func TestMarkdownShowsControlCharacters(t *testing.T) {
	out := md.Markdown("evil \x1b]52;c;aGk=\x07 \x1b[2J done\x00", 80)
	if got := ansi.Strip(out); got != "evil ␛]52;c;aGk=␇ ␛[2J done␀" {
		t.Fatalf("controls = %q", got)
	}
	if strings.ContainsAny(out, "\x07\x00") || strings.Contains(out, "\x1b]") || strings.Contains(out, "\x1b[2J") {
		t.Fatalf("raw control reached the terminal: %q", out)
	}
	if got := plain("bad \xff byte", 80); got != "bad \uFFFD byte" {
		t.Fatalf("invalid UTF-8 = %q", got)
	}
}

func TestAgentReplyPrefix(t *testing.T) {
	// A leading paragraph shares the prefix's line.
	if got := ansi.Strip(md.AgentReply("agent> ", "Done.")); got != "agent> Done." {
		t.Fatalf("short reply = %q", got)
	}
	// Any other leading block starts below it to keep its alignment.
	got := ansi.Strip(md.AgentReply("agent> ", "- a\n- b"))
	if got != "agent> \n• a\n• b" {
		t.Fatalf("list reply = %q", got)
	}
	// The first paragraph wraps narrower to leave room for the prefix.
	out := renderMarkdown(strings.Repeat("word ", 12), 30, 7)
	first := strings.Split(ansi.Strip(out), "\n")[0]
	if ansi.StringWidth(first) > 23 {
		t.Fatalf("first line %q ignores the prefix width", first)
	}
}

// TestMarkdownMalformedKeepsWords feeds broken Markdown and checks every
// word of the input survives into the rendered text.
func TestMarkdownMalformedKeepsWords(t *testing.T) {
	inputs := []string{
		"```\nunclosed fence with **stars**",
		"~~~~ \n```\nmixed fences\n~~~",
		"**bold _italic** close_ `code",
		"\\`` escaped tick then `x` and \\` alone",
		"[link](unclosed and [nested [brackets]](x)",
		"| a | b\n|---\n| c",
		"|---|---|\n| no header |",
		"- item\n    - over-indented\n - under\n\t- tabbed\n-\n-  ",
		"> quote\nlazy line\n>> deep\n>",
		"# \n######## seven\n#\tTab",
		"1) one\n2. two\n10000000000. too long",
		"\x00 control and \u200b zero width",
		"***\n___\n---\n* * *\n- -",
		strings.Repeat("*a ", 300) + strings.Repeat("_b ", 300) + strings.Repeat("[", 300),
		strings.Repeat("> ", 200) + "deep",
		strings.Repeat("  ", 100) + "- deep item",
		"日本語の**太字**と`コード`と[リンク](https://例え.jp)",
	}
	for _, input := range inputs {
		out := plain(input, 24)
		// Quote bars sit between the pieces of a word broken by wrapping.
		squashed := strings.Join(strings.Fields(strings.ReplaceAll(out, "│", "")), "")
		for _, word := range words(input) {
			if !strings.Contains(squashed, word) {
				t.Errorf("input %q lost %q; got:\n%s", input, word, out)
			}
		}
	}
}

// TestMarkdownLargeUnbalancedInputIsFast guards against quadratic scans:
// each input is a few hundred kilobytes of openers without closers.
func TestMarkdownLargeUnbalancedInputIsFast(t *testing.T) {
	var nested strings.Builder
	for i := range 500 {
		nested.WriteString(strings.Repeat("  ", i) + "- item\n")
	}
	inputs := []string{
		strings.Repeat("*a [b <c `d ~~e _f ", 20000),
		strings.Repeat("[", 200000),
		strings.Repeat("<", 200000) + ">",
		strings.Repeat("` `` ``` ", 30000),
		strings.Repeat("**a *b [c](", 20000),
		strings.Repeat("> ", 1000) + "deep",
		nested.String(),
	}
	start := time.Now()
	for _, input := range inputs {
		md.Markdown(input, 80)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("rendering took %s", elapsed)
	}
}

func FuzzMarkdown(f *testing.F) {
	for _, seed := range []string{
		"# h\n**b** _i_ `c` ~~s~~\n- a\n  1. b\n> q\n---\n| a | b |\n|:-|-:|\n| 1 | 2 |\n```go\nx\n```",
		"```", "|", "-", ">", "*", "[", "![", "<", "\\", "中文|表格\n-|-\n格|子",
	} {
		f.Add(seed, 30)
	}
	f.Fuzz(func(t *testing.T, src string, width int) {
		width = 12 + abs(width)%100
		have := letters(ansi.Strip(md.Markdown(src, width)))
		for r, n := range letters(withoutDestinations(strings.ToValidUTF8(src, ""))) {
			if have[r] < n {
				t.Fatalf("lost %q (%d of %d) from %q", r, n-have[r], n, src)
			}
		}
	})
}

// withoutDestinations drops every "(...)" that follows "]": a link whose
// text repeats its URL shows it once, and TestMarkdownLinks covers URLs.
func withoutDestinations(s string) string {
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '(' || i == 0 || s[i-1] != ']' {
			out.WriteByte(s[i])
			continue
		}
		depth := 0
		for ; i < len(s); i++ {
			if s[i] == '(' {
				depth++
			} else if s[i] == ')' {
				if depth--; depth == 0 {
					break
				}
			}
		}
	}
	return out.String()
}

// letters counts the letters and digits in s: whatever the layout, the
// rendered text must contain at least as many of each as the source.
func letters(s string) map[rune]int {
	counts := map[rune]int{}
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			counts[r]++
		}
	}
	return counts
}

// words returns the letter and digit runs of s; rendering may drop markup
// and move whitespace but must keep these.
func words(s string) []string {
	var out []string
	for _, field := range strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		// Wide runs can be split across wrapped lines or cell rows; check
		// a prefix that fits a narrow column.
		runes := []rune(field)
		out = append(out, string(runes[:min(len(runes), 4)]))
	}
	return out
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
