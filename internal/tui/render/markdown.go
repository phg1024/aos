package render

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

// Markdown rendering for agent replies covers the subset models actually
// write: ATX headings, emphasis, inline code, strikethrough, fenced code,
// nested lists, block quotes, rules, links and GFM pipe tables. It is a
// lenient line-based parser, not CommonMark: anything it does not recognise
// stays literal text, so malformed input degrades to raw Markdown instead of
// losing words. Line breaks inside a paragraph are kept, as in chat UIs,
// because replies often lean on them for layout.

const (
	sgrReset = "\033[0m"

	mdHeading1 = "1;4;38;5;45"
	mdHeading2 = "1;38;5;45"
	mdHeading3 = "1"
	mdBold     = "1"
	mdItalic   = "3"
	mdBoldItal = "1;3"
	mdStrike   = "9"
	mdCode     = "38;5;180"
	mdLink     = "4;38;5;75"
	mdURL      = "2;38;5;110"
	mdBorder   = "38;5;244"
	mdMarker   = "38;5;45"
)

// minMarkdownWidth keeps deeply nested blocks readable on narrow terminals;
// below it lines overflow and the terminal wraps them.
const minMarkdownWidth = 12

// AgentReply prints an agent's final message: the coloured prefix followed
// by the body rendered as Markdown at the terminal width. Without colour it
// is the raw text, byte for byte.
func (p Palette) AgentReply(prefix, body string) string {
	if !p.Enabled {
		return prefix + body
	}
	width, _ := TerminalSize()
	return p.Agent(prefix) + renderMarkdown(body, width, ansi.StringWidth(prefix))
}

// Markdown renders src for a terminal of the given width; a disabled
// palette returns src unchanged.
func (p Palette) Markdown(src string, width int) string {
	if !p.Enabled {
		return src
	}
	return renderMarkdown(src, width, 0)
}

// renderMarkdown renders src; offset is how many columns the first line
// already holds. A leading paragraph wraps around it; any other leading
// block starts on the next line so its layout stays aligned.
func renderMarkdown(src string, width, offset int) string {
	// Width and wrapping work on graphemes; a stray byte would merge with
	// its neighbours, so it shows as U+FFFD instead.
	src = strings.ToValidUTF8(src, "\uFFFD")
	src = strings.ReplaceAll(src, "\r\n", "\n")
	src = strings.ReplaceAll(src, "\r", "\n")
	src = strings.Map(visibleControl, src)
	lines := strings.Split(src, "\n")
	for i, line := range lines {
		lines[i] = expandTabs(line)
	}
	blocks := parseBlocks(lines)
	if len(blocks) == 0 {
		return ""
	}
	width = max(width, minMarkdownWidth)
	var out []string
	for i, b := range blocks {
		blockWidth := width
		switch {
		case i > 0 && b.blankBefore:
			out = append(out, "")
		case i == 0 && offset > 0 && b.kind == blockParagraph:
			blockWidth = width - offset
		case i == 0 && offset > 0:
			out = append(out, "")
		}
		out = append(out, b.render(blockWidth, 0)...)
	}
	return strings.Join(out, "\n")
}

type blockKind int

const (
	blockParagraph blockKind = iota
	blockHeading
	blockCode
	blockRule
	blockQuote
	blockList
	blockTable
)

type block struct {
	kind        blockKind
	blankBefore bool
	lines       []string // paragraph text, code lines, quote body
	level       int      // heading level
	text        string   // heading text
	lang        string   // code fence info
	items       []listItem
	table       tableBlock
}

type listItem struct {
	marker      string
	ordered     bool
	blankBefore bool
	body        []string
}

type tableBlock struct {
	header []string
	aligns []byte // 'l', 'c' or 'r'
	rows   [][]string
}

func renderBlockList(blocks []block, width, depth int) []string {
	var out []string
	for i, b := range blocks {
		if i > 0 && b.blankBefore {
			out = append(out, "")
		}
		out = append(out, b.render(width, depth)...)
	}
	return out
}

func parseBlocks(lines []string) []block {
	var blocks []block
	blank := false
	for i := 0; i < len(lines); {
		line := lines[i]
		if isBlank(line) {
			blank = true
			i++
			continue
		}
		var b block
		switch {
		case isFenceOpen(line):
			b, i = parseFence(lines, i)
		case isHeading(line):
			b = parseHeading(line)
			i++
		case isRule(line):
			b = block{kind: blockRule}
			i++
		case isQuote(line):
			b, i = parseQuote(lines, i)
		case isListItem(line):
			b, i = parseList(lines, i)
		case isTableStart(lines, i):
			b, i = parseTable(lines, i)
		default:
			b, i = parseParagraph(lines, i)
		}
		b.blankBefore = blank
		blank = false
		blocks = append(blocks, b)
	}
	return blocks
}

// startsBlock reports whether lines[i] opens a block that interrupts a
// paragraph.
func startsBlock(lines []string, i int) bool {
	line := lines[i]
	return isFenceOpen(line) || isHeading(line) || isRule(line) || isQuote(line) || isListItem(line) || isTableStart(lines, i)
}

func parseParagraph(lines []string, i int) (block, int) {
	b := block{kind: blockParagraph}
	for ; i < len(lines); i++ {
		if isBlank(lines[i]) || (len(b.lines) > 0 && startsBlock(lines, i)) {
			break
		}
		b.lines = append(b.lines, strings.TrimSpace(lines[i]))
	}
	return b, i
}

// Fenced code.

type fence struct {
	char   byte
	size   int
	indent int
	info   string
}

func parseFenceLine(line string) (fence, bool) {
	indent := leadingSpaces(line)
	if indent > 3 {
		return fence{}, false
	}
	rest := line[indent:]
	if rest == "" || (rest[0] != '`' && rest[0] != '~') {
		return fence{}, false
	}
	size := runLength(rest, 0, rest[0])
	if size < 3 {
		return fence{}, false
	}
	info := strings.TrimSpace(rest[size:])
	if rest[0] == '`' && strings.Contains(info, "`") {
		return fence{}, false
	}
	return fence{char: rest[0], size: size, indent: indent, info: info}, true
}

func isFenceOpen(line string) bool {
	_, ok := parseFenceLine(line)
	return ok
}

func closesFence(line string, open fence) bool {
	f, ok := parseFenceLine(line)
	return ok && f.char == open.char && f.size >= open.size && f.info == ""
}

// parseFence collects a fenced block; an unclosed fence runs to the end so
// none of its text is lost.
func parseFence(lines []string, i int) (block, int) {
	open, _ := parseFenceLine(lines[i])
	b := block{kind: blockCode, lang: open.info}
	for i++; i < len(lines); i++ {
		if closesFence(lines[i], open) {
			return b, i + 1
		}
		b.lines = append(b.lines, dedent(lines[i], open.indent))
	}
	for len(b.lines) > 0 && isBlank(b.lines[len(b.lines)-1]) {
		b.lines = b.lines[:len(b.lines)-1]
	}
	return b, i
}

// Headings and rules.

func isHeading(line string) bool {
	_, _, ok := headingParts(line)
	return ok
}

func headingParts(line string) (int, string, bool) {
	indent := leadingSpaces(line)
	if indent > 3 {
		return 0, "", false
	}
	rest := line[indent:]
	level := runLength(rest, 0, '#')
	if level < 1 || level > 6 {
		return 0, "", false
	}
	rest = rest[level:]
	if rest != "" && rest[0] != ' ' {
		return 0, "", false
	}
	text := strings.TrimSpace(rest)
	// A closing run of #s is decoration when a space precedes it.
	if trimmed := strings.TrimRight(text, "#"); trimmed == "" || strings.HasSuffix(trimmed, " ") {
		text = strings.TrimSpace(trimmed)
	}
	return level, text, true
}

func parseHeading(line string) block {
	level, text, _ := headingParts(line)
	return block{kind: blockHeading, level: level, text: text}
}

func isRule(line string) bool {
	if leadingSpaces(line) > 3 {
		return false
	}
	var mark byte
	count := 0
	for i := 0; i < len(line); i++ {
		switch c := line[i]; c {
		case ' ':
		case '-', '*', '_':
			if mark != 0 && c != mark {
				return false
			}
			mark = c
			count++
		default:
			return false
		}
	}
	return count >= 3
}

// Block quotes.

func isQuote(line string) bool {
	indent := leadingSpaces(line)
	return indent <= 3 && indent < len(line) && line[indent] == '>'
}

func parseQuote(lines []string, i int) (block, int) {
	b := block{kind: blockQuote}
	for ; i < len(lines) && isQuote(lines[i]); i++ {
		rest := lines[i][leadingSpaces(lines[i])+1:]
		rest = strings.TrimPrefix(rest, " ")
		b.lines = append(b.lines, rest)
	}
	return b, i
}

// Lists.

type marker struct {
	indent  int
	text    string
	ordered bool
	content int // column where the item's text starts
}

func parseMarker(line string) (marker, bool) {
	indent := leadingSpaces(line)
	rest := line[indent:]
	size := 0
	ordered := false
	switch {
	case rest == "":
		return marker{}, false
	case rest[0] == '-' || rest[0] == '*' || rest[0] == '+':
		size = 1
	default:
		digits := 0
		for digits < len(rest) && digits < 10 && rest[digits] >= '0' && rest[digits] <= '9' {
			digits++
		}
		if digits == 0 || digits > 9 || digits >= len(rest) || (rest[digits] != '.' && rest[digits] != ')') {
			return marker{}, false
		}
		size = digits + 1
		ordered = true
	}
	if size < len(rest) && rest[size] != ' ' {
		return marker{}, false
	}
	gap := leadingSpaces(rest[size:])
	if gap == 0 || gap > 4 || size+gap == len(rest) {
		gap = 1
	}
	return marker{indent: indent, text: rest[:size], ordered: ordered, content: indent + size + gap}, true
}

func isListItem(line string) bool {
	if isRule(line) {
		return false
	}
	_, ok := parseMarker(line)
	return ok
}

// parseList collects items whose markers sit at the first item's indent
// (give or take one column). A line indented past that indent belongs to
// the current item, dedented by its content column, so nested lists and
// fences inside items render through the same block parser.
func parseList(lines []string, i int) (block, int) {
	first, _ := parseMarker(lines[i])
	base := first.indent
	sibling := func(line string) bool {
		m, ok := parseMarker(line)
		return ok && !isRule(line) && m.indent <= base+1
	}
	b := block{kind: blockList}
	blank := false
	for i < len(lines) && sibling(lines[i]) {
		m, _ := parseMarker(lines[i])
		item := listItem{marker: m.text, ordered: m.ordered, blankBefore: blank && len(b.items) > 0}
		blank = false
		item.body = append(item.body, lines[i][min(m.content, len(lines[i])):])
		var open *fence
		for i++; i < len(lines); i++ {
			line := lines[i]
			if open != nil {
				body := dedent(line, m.content)
				item.body = append(item.body, body)
				if closesFence(body, *open) {
					open = nil
				}
				continue
			}
			if isBlank(line) {
				next := i
				for next < len(lines) && isBlank(lines[next]) {
					next++
				}
				if next == len(lines) || (!sibling(lines[next]) && leadingSpaces(lines[next]) <= base+1) {
					b.items = append(b.items, item)
					return b, i
				}
				if sibling(lines[next]) {
					blank = true
					i = next
					break
				}
				for ; i < next; i++ {
					item.body = append(item.body, "")
				}
				i--
				continue
			}
			if sibling(line) {
				break
			}
			if leadingSpaces(line) > base+1 {
				body := dedent(line, m.content)
				item.body = append(item.body, body)
				if f, ok := parseFenceLine(body); ok {
					open = &f
				}
				continue
			}
			// A lazy continuation line extends the item's last paragraph.
			if !startsBlock(lines, i) && !isBlank(item.body[len(item.body)-1]) {
				item.body = append(item.body, strings.TrimSpace(line))
				continue
			}
			b.items = append(b.items, item)
			return b, i
		}
		b.items = append(b.items, item)
	}
	return b, i
}

// Tables.

func isTableStart(lines []string, i int) bool {
	return i+1 < len(lines) && strings.Contains(lines[i], "|") && isDelimiterRow(lines[i+1])
}

func isDelimiterRow(line string) bool {
	if !strings.Contains(line, "|") && !strings.Contains(line, "-") {
		return false
	}
	cells := splitRow(line)
	if len(cells) == 0 || (len(cells) == 1 && !strings.Contains(line, "|")) {
		return false
	}
	for _, cell := range cells {
		cell = strings.TrimSpace(cell)
		cell = strings.TrimPrefix(cell, ":")
		cell = strings.TrimSuffix(cell, ":")
		if cell == "" || strings.Trim(cell, "-") != "" {
			return false
		}
	}
	return true
}

func parseTable(lines []string, i int) (block, int) {
	b := block{kind: blockTable}
	b.table.header = splitRow(lines[i])
	for _, cell := range splitRow(lines[i+1]) {
		cell = strings.TrimSpace(cell)
		align := byte('l')
		switch {
		case strings.HasPrefix(cell, ":") && strings.HasSuffix(cell, ":"):
			align = 'c'
		case strings.HasSuffix(cell, ":"):
			align = 'r'
		}
		b.table.aligns = append(b.table.aligns, align)
	}
	for i += 2; i < len(lines); i++ {
		line := lines[i]
		if isBlank(line) || !strings.Contains(line, "|") || (startsBlock(lines, i) && !isTableStart(lines, i)) {
			break
		}
		b.table.rows = append(b.table.rows, splitRow(line))
	}
	return b, i
}

// splitRow splits a pipe table row into trimmed cells. Escaped pipes and
// pipes inside code spans stay in the cell.
func splitRow(line string) []string {
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(line, "|")
	if strings.HasSuffix(line, "|") && !strings.HasSuffix(line, `\|`) {
		line = line[:len(line)-1]
	}
	var cells []string
	start := 0
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '\\':
			i++
		case '`':
			size := runLength(line, i, '`')
			if end := findBacktickRun(line, i+size, size); end >= 0 {
				i = end + size - 1
			} else {
				i += size - 1
			}
		case '|':
			cells = append(cells, strings.TrimSpace(line[start:i]))
			start = i + 1
		}
	}
	return append(cells, strings.TrimSpace(line[start:]))
}

// Block rendering.

func (b block) render(width, depth int) []string {
	width = max(width, minMarkdownWidth)
	switch b.kind {
	case blockHeading:
		style := mdHeading3
		switch b.level {
		case 1:
			style = mdHeading1
		case 2:
			style = mdHeading2
		}
		return wrapStyled(renderInline(b.text, style), width)
	case blockCode:
		return renderCode(b.lang, b.lines, width)
	case blockRule:
		return []string{styled(mdBorder, strings.Repeat("─", width))}
	case blockQuote:
		bar := styled(mdBorder, "│") + " "
		inner := renderBlockList(parseBlocks(b.lines), width-2, depth)
		for i, line := range inner {
			inner[i] = strings.TrimRight(bar+line, " ")
		}
		return inner
	case blockList:
		return renderList(b.items, width, depth)
	case blockTable:
		return renderTable(b.table, width)
	default:
		return wrapStyled(renderInline(strings.Join(b.lines, "\n"), ""), width)
	}
}

func renderCode(lang string, lines []string, width int) []string {
	top := "╭─"
	if lang != "" {
		top += " " + lang
	}
	out := []string{styled(mdBorder, top)}
	bar := styled(mdBorder, "│")
	for _, line := range lines {
		if line == "" {
			out = append(out, bar)
			continue
		}
		for _, part := range strings.Split(ansi.Hardwrap(line, width-2, true), "\n") {
			out = append(out, bar+" "+part)
		}
	}
	return append(out, styled(mdBorder, "╰─"))
}

var bulletGlyphs = []string{"•", "◦", "▪"}

func renderList(items []listItem, width, depth int) []string {
	markers := make([]string, len(items))
	markerWidth := 0
	for i, item := range items {
		markers[i] = bulletGlyphs[depth%len(bulletGlyphs)]
		if item.ordered {
			markers[i] = item.marker
		}
		markerWidth = max(markerWidth, ansi.StringWidth(markers[i])+1)
	}
	var out []string
	for i, item := range items {
		if item.blankBefore {
			out = append(out, "")
		}
		pad := strings.Repeat(" ", markerWidth-ansi.StringWidth(markers[i]))
		lead := styled(mdMarker, markers[i]) + pad
		if items[i].ordered {
			// Right-align numbers so 9. and 10. share a text column.
			lead = pad[1:] + styled(mdMarker, markers[i]) + " "
		}
		body := renderBlockList(parseBlocks(item.body), width-markerWidth, depth+1)
		if len(body) == 0 {
			out = append(out, strings.TrimRight(lead, " "))
			continue
		}
		indent := strings.Repeat(" ", markerWidth)
		for j, line := range body {
			switch {
			case j == 0:
				out = append(out, lead+line)
			case line == "":
				out = append(out, "")
			default:
				out = append(out, indent+line)
			}
		}
	}
	return out
}

func renderTable(t tableBlock, width int) []string {
	cols := max(len(t.header), len(t.aligns))
	for _, row := range t.rows {
		cols = max(cols, len(row))
	}
	cell := func(row []string, c int, style string) string {
		if c < len(row) {
			return renderInline(row[c], style)
		}
		return ""
	}
	header := make([]string, cols)
	rows := make([][]string, len(t.rows))
	natural := make([]int, cols)
	for c := range cols {
		header[c] = cell(t.header, c, mdBold)
		natural[c] = max(ansi.StringWidth(header[c]), 1)
	}
	for r, row := range t.rows {
		rows[r] = make([]string, cols)
		for c := range cols {
			rows[r][c] = cell(row, c, "")
			natural[c] = max(natural[c], ansi.StringWidth(rows[r][c]))
		}
	}
	widths := fitColumns(natural, width-(3*cols+1))

	line := func(left, mid, right string) string {
		parts := make([]string, cols)
		for c, w := range widths {
			parts[c] = strings.Repeat("─", w+2)
		}
		return styled(mdBorder, left+strings.Join(parts, mid)+right)
	}
	bar := styled(mdBorder, "│")
	renderRow := func(cells []string) []string {
		wrapped := make([][]string, cols)
		height := 1
		for c := range cols {
			wrapped[c] = wrapStyled(cells[c], widths[c])
			height = max(height, len(wrapped[c]))
		}
		out := make([]string, height)
		for h := range height {
			var builder strings.Builder
			builder.WriteString(bar)
			for c := range cols {
				text := ""
				if h < len(wrapped[c]) {
					text = wrapped[c][h]
				}
				align := byte('l')
				if c < len(t.aligns) {
					align = t.aligns[c]
				}
				builder.WriteString(" " + alignCell(text, widths[c], align) + " " + bar)
			}
			out[h] = builder.String()
		}
		return out
	}

	out := []string{line("┌", "┬", "┐")}
	out = append(out, renderRow(header)...)
	out = append(out, line("├", "┼", "┤"))
	for _, row := range rows {
		out = append(out, renderRow(row)...)
	}
	return append(out, line("└", "┴", "┘"))
}

// fitColumns narrows the widest columns until the table fits in avail
// cells, wrapping their text; columns never shrink below a few cells, so a
// table with very many columns overflows instead of losing text.
func fitColumns(natural []int, avail int) []int {
	widths := append([]int(nil), natural...)
	total := 0
	for _, w := range widths {
		total += w
	}
	const floor = 4
	for total > avail {
		widest := 0
		for c, w := range widths {
			if w > widths[widest] {
				widest = c
			}
		}
		if widths[widest] <= floor {
			break
		}
		widths[widest]--
		total--
	}
	return widths
}

func alignCell(text string, width int, align byte) string {
	gap := max(width-ansi.StringWidth(text), 0)
	switch align {
	case 'r':
		return strings.Repeat(" ", gap) + text
	case 'c':
		return strings.Repeat(" ", gap/2) + text + strings.Repeat(" ", gap-gap/2)
	default:
		return text + strings.Repeat(" ", gap)
	}
}

// Inline rendering.

// inlineWriter emits styled text. Styles nest through a stack: closing one
// resets and re-applies the ones still open, since SGR has no "pop".
type inlineWriter struct {
	out   strings.Builder
	stack []string
	depth int
}

// maxInlineDepth bounds nested emphasis and links; deeper text is written
// as is, which keeps pathological nesting linear.
const maxInlineDepth = 16

func renderInline(text, base string) string {
	w := &inlineWriter{}
	if base != "" {
		w.push(base)
	}
	w.render(text)
	if base != "" {
		w.pop()
	}
	return w.out.String()
}

func (w *inlineWriter) push(code string) {
	w.stack = append(w.stack, code)
	w.out.WriteString("\033[" + code + "m")
}

func (w *inlineWriter) pop() {
	w.stack = w.stack[:len(w.stack)-1]
	w.out.WriteString(sgrReset)
	for _, code := range w.stack {
		w.out.WriteString("\033[" + code + "m")
	}
}

func (w *inlineWriter) literal(code, text string) {
	w.push(code)
	w.out.WriteString(text)
	w.pop()
}

func (w *inlineWriter) render(s string) {
	if w.depth > maxInlineDepth {
		w.out.WriteString(s)
		return
	}
	w.depth++
	defer func() { w.depth-- }()
	sc := newInlineScan(s)
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s) && isASCIIPunct(s[i+1]):
			w.out.WriteByte(s[i+1])
			i += 2
		case c == '`':
			size := runLength(s, i, '`')
			end := sc.codeEnd(i)
			if end < 0 {
				w.out.WriteString(s[i : i+size])
				i += size
				continue
			}
			code := strings.ReplaceAll(s[i+size:end], "\n", " ")
			if len(code) >= 2 && code[0] == ' ' && code[len(code)-1] == ' ' && strings.Trim(code, " ") != "" {
				code = code[1 : len(code)-1]
			}
			w.literal(mdCode, code)
			i = end + size
		case c == '*' || c == '_' || c == '~':
			size := runLength(s, i, c)
			if next, ok := w.emphasis(sc, i, size); ok {
				i = next
				continue
			}
			w.out.WriteString(s[i : i+size])
			i += size
		case c == '!' && i+1 < len(s) && s[i+1] == '[':
			if next, ok := w.link(sc, i+1, true); ok {
				i = next
				continue
			}
			w.out.WriteByte(c)
			i++
		case c == '[':
			if next, ok := w.link(sc, i, false); ok {
				i = next
				continue
			}
			w.out.WriteByte(c)
			i++
		case c == '<':
			if next, ok := w.autolink(s, i); ok {
				i = next
				continue
			}
			w.out.WriteByte(c)
			i++
		default:
			w.out.WriteByte(c)
			i++
		}
	}
}

// inlineScan indexes one inline text up front so each lookup is constant
// time and long unbalanced text renders in linear time.
type inlineScan struct {
	s string
	// ticks maps each unescaped backtick run to the start of the run that
	// closes it, or -1; read it through codeEnd.
	ticks map[int]int
	// pairs maps each '[' and '(' to its closing bracket, honouring nesting,
	// escapes and code spans.
	pairs map[int]int
	// missing marks emphasis runs with no closer left: closers are only
	// searched forward, so a run that found none cannot find one later.
	missing map[delimiter]bool
}

type delimiter struct {
	char byte
	size int
}

func newInlineScan(s string) *inlineScan {
	sc := &inlineScan{s: s, ticks: map[int]int{}, pairs: map[int]int{}, missing: map[delimiter]bool{}}
	var starts []int
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '`':
			starts = append(starts, i)
			i += runLength(s, i, '`') - 1
		}
	}
	next := map[int]int{}
	for k := len(starts) - 1; k >= 0; k-- {
		size := runLength(s, starts[k], '`')
		if end, ok := next[size]; ok {
			sc.ticks[starts[k]] = end
		} else {
			sc.ticks[starts[k]] = -1
		}
		next[size] = starts[k]
	}
	var squares, rounds []int
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '`':
			i = sc.skipCode(i) - 1
		case '[':
			squares = append(squares, i)
		case '(':
			rounds = append(rounds, i)
		case ']':
			if n := len(squares); n > 0 {
				sc.pairs[squares[n-1]] = i
				squares = squares[:n-1]
			}
		case ')':
			if n := len(rounds); n > 0 {
				sc.pairs[rounds[n-1]] = i
				rounds = rounds[:n-1]
			}
		}
	}
	return sc
}

// codeEnd returns where the run closing the backtick run at i starts, or
// -1 when nothing closes it.
func (sc *inlineScan) codeEnd(i int) int {
	if end, ok := sc.ticks[i]; ok && end > i {
		return end
	}
	return -1
}

// skipCode returns the index just past the code span or backtick run at i.
func (sc *inlineScan) skipCode(i int) int {
	size := runLength(sc.s, i, '`')
	if end := sc.codeEnd(i); end >= 0 {
		return end + size
	}
	return i + size
}

func (sc *inlineScan) closing(i int) int {
	if end, ok := sc.pairs[i]; ok {
		return end
	}
	return -1
}

// emphasis renders a delimiter run of size at i when a matching closer
// follows. Underscores do not open or close inside words, so snake_case
// identifiers stay intact.
func (w *inlineWriter) emphasis(sc *inlineScan, i, size int) (int, bool) {
	s := sc.s
	c := s[i]
	var style string
	switch {
	case c == '~' && size == 2:
		style = mdStrike
	case c == '~':
		return 0, false
	case size == 1:
		style = mdItalic
	case size == 2:
		style = mdBold
	case size == 3:
		style = mdBoldItal
	default:
		return 0, false
	}
	key := delimiter{c, size}
	if !canOpen(s, i, size) || sc.missing[key] {
		return 0, false
	}
	end := sc.findCloser(i+size, c, size)
	if end < 0 {
		sc.missing[key] = true
		return 0, false
	}
	w.push(style)
	w.render(s[i+size : end])
	w.pop()
	return end + size, true
}

func canOpen(s string, i, size int) bool {
	if i+size >= len(s) {
		return false
	}
	next, _ := utf8.DecodeRuneInString(s[i+size:])
	if unicode.IsSpace(next) {
		return false
	}
	if s[i] == '_' && i > 0 {
		prev, _ := utf8.DecodeLastRuneInString(s[:i])
		return !isWordRune(prev)
	}
	return true
}

func canClose(s string, i, size int) bool {
	prev, _ := utf8.DecodeLastRuneInString(s[:i])
	if unicode.IsSpace(prev) {
		return false
	}
	if s[i] == '_' && i+size < len(s) {
		next, _ := utf8.DecodeRuneInString(s[i+size:])
		return !isWordRune(next)
	}
	return true
}

// findCloser finds a run of exactly size c that can close, skipping escapes
// and code spans; the span between must not be empty.
func (sc *inlineScan) findCloser(from int, c byte, size int) int {
	s := sc.s
	for j := from; j < len(s); {
		switch s[j] {
		case '\\':
			j += 2
		case '`':
			j = sc.skipCode(j)
		case c:
			run := runLength(s, j, c)
			if run == size && j > from && canClose(s, j, size) {
				return j
			}
			j += run
		default:
			j++
		}
	}
	return -1
}

// link renders [text](url) starting at the bracket at i: the text styled,
// then the URL dimmed unless it repeats the text.
func (w *inlineWriter) link(sc *inlineScan, i int, image bool) (int, bool) {
	s := sc.s
	close := sc.closing(i)
	if close < 0 || close+1 >= len(s) || s[close+1] != '(' {
		return 0, false
	}
	end := sc.closing(close + 1)
	if end < 0 {
		return 0, false
	}
	text := s[i+1 : close]
	// The destination shows as written, so a "title" stays visible too.
	dest := strings.TrimSpace(s[close+2 : end])
	if strings.HasPrefix(dest, "<") && strings.HasSuffix(dest, ">") {
		dest = dest[1 : len(dest)-1]
	}
	if strings.ContainsAny(dest, "\n") {
		return 0, false
	}
	w.push(mdLink)
	if image {
		w.out.WriteString("image: ")
	}
	w.render(text)
	w.pop()
	if dest != "" && dest != text {
		w.out.WriteString(" ")
		w.literal(mdURL, "("+dest+")")
	}
	return end + 1, true
}

// autolink renders <scheme:target>. The scan stops at the next '<' or
// space, so runs of '<' stay linear.
func (w *inlineWriter) autolink(s string, i int) (int, bool) {
	end := i + 1
	for end < len(s) && s[end] != '>' && s[end] != '<' && s[end] != ' ' && s[end] != '\t' && s[end] != '\n' {
		end++
	}
	if end >= len(s) || s[end] != '>' {
		return 0, false
	}
	target := s[i+1 : end]
	scheme, rest, ok := strings.Cut(target, ":")
	if !ok || len(scheme) < 2 || rest == "" || !isScheme(scheme) {
		return 0, false
	}
	w.literal(mdLink, target)
	return end + 1, true
}

// findBacktickRun returns where a backtick run of exactly size starts at or
// after from, or -1.
func findBacktickRun(s string, from, size int) int {
	for j := from; j < len(s); {
		if s[j] != '`' {
			j++
			continue
		}
		run := runLength(s, j, '`')
		if run == size {
			return j
		}
		j += run
	}
	return -1
}

// Wrapping and style bookkeeping.

// wrapStyled wraps styled text to width and closes each line's styles, so
// anything prefixed to a line (indent, quote bar, table border) is never
// painted by a style left open on the line before.
func wrapStyled(s string, width int) []string {
	lines := strings.Split(wrapText(s, max(width, 1)), "\n")
	active := ""
	for i, line := range lines {
		start := active
		active = trackSGR(active, line)
		line = start + line
		if active != "" {
			line += sgrReset
		}
		lines[i] = strings.TrimRight(line, " ")
	}
	return lines
}

// Break classes for wrapText: what the last grapheme of the pending unit
// allows after it.
const (
	breakNever  = iota // inside a word
	breakAfter         // after a CJK character or closing punctuation
	breakNotYet        // after opening punctuation, which binds to what follows
)

// wrapText breaks styled text into lines of at most limit cells. ansi.Wrap
// breaks only at spaces, so a run of Chinese, which has none, moved whole
// to the next line or was cut anywhere. Here a line may also break between
// CJK characters, as in UAX #14, but never before closing punctuation such
// as ， 。 ） or after opening punctuation such as （ 「. Latin words and URLs
// stay whole unless one is wider than the line.
func wrapText(s string, limit int) string {
	var out strings.Builder
	for i, line := range strings.Split(s, "\n") {
		if i > 0 {
			out.WriteByte('\n')
		}
		wrapLine(&out, line, limit)
	}
	return out.String()
}

func wrapLine(out *strings.Builder, s string, limit int) {
	var (
		cur     int // cells on the current output line
		spaces  strings.Builder
		spaceW  int
		unit    strings.Builder // the pending unbreakable unit, escapes included
		unitW   int
		last    = breakNever
		state   byte
		pending = false // unit holds anything, even a zero-width escape
	)
	newline := func() {
		out.WriteByte('\n')
		cur = 0
	}
	flush := func() {
		if !pending {
			return
		}
		if cur > 0 && cur+spaceW+unitW > limit {
			newline()
		} else {
			out.WriteString(spaces.String())
			cur += spaceW
		}
		spaces.Reset()
		spaceW = 0
		text := unit.String()
		if cur+unitW <= limit {
			out.WriteString(text)
			cur += unitW
		} else {
			// Wider than a line: cut between graphemes.
			var st byte
			for rest := text; rest != ""; {
				seq, w, n, next := ansi.DecodeSequence(rest, st, nil)
				st, rest = next, rest[n:]
				if w > 0 && cur > 0 && cur+w > limit {
					newline()
				}
				out.WriteString(seq)
				cur += w
			}
		}
		unit.Reset()
		unitW = 0
		pending = false
	}
	add := func(seq string, w int) {
		unit.WriteString(seq)
		unitW += w
		pending = true
	}
	for rest := s; rest != ""; {
		seq, w, n, next := ansi.DecodeSequence(rest, state, nil)
		state, rest = next, rest[n:]
		r, _ := utf8.DecodeRuneInString(seq)
		switch {
		case w == 0:
			add(seq, 0) // escape sequences ride with the text after them
		case unicode.IsSpace(r) && r != 0xa0:
			flush()
			spaces.WriteString(seq)
			spaceW += w
			last = breakNever
		case strings.ContainsRune(closingPunct, r):
			add(seq, w)
			if w > 1 {
				last = breakAfter
			}
		case strings.ContainsRune(openingPunct, r):
			if last != breakNotYet {
				flush()
			}
			add(seq, w)
			last = breakNotYet
		case w > 1 && breaksAround(r):
			if last != breakNotYet {
				flush()
			}
			add(seq, w)
			last = breakAfter
		default:
			if last == breakAfter {
				flush()
			}
			add(seq, w)
			last = breakNever
		}
	}
	flush()
	out.WriteString(spaces.String())
}

// Punctuation that must not start a line, and punctuation that must not end
// one. ASCII closers stay glued to what precedes them without adding a break
// opportunity, so "example.com" never splits at the dot.
const (
	closingPunct = "，。、；：！？）」』】》〉〕｝”’…～·．,.;:!?)]}%"
	openingPunct = "（「『【《〈〔｛“‘"
)

// breaksAround reports whether a line may break on either side of a wide
// character: CJK ideographs, kana, hangul, their punctuation, fullwidth
// forms and emoji.
func breaksAround(r rune) bool {
	return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul) ||
		(r >= 0x3000 && r <= 0x303f) || (r >= 0xff00 && r <= 0xffef) || r >= 0x1f000
}

// trackSGR returns the SGR sequences still in effect after s, given those in
// effect before it.
func trackSGR(active, s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] != 0x1b || i+1 >= len(s) || s[i+1] != '[' {
			continue
		}
		j := i + 2
		for j < len(s) && s[j] >= 0x20 && s[j] <= 0x3f {
			j++
		}
		if j >= len(s) {
			break
		}
		if s[j] == 'm' {
			seq := s[i : j+1]
			if seq == sgrReset || seq == "\033[m" {
				active = ""
			} else {
				active += seq
			}
		}
		i = j
	}
	return active
}

func styled(code, text string) string {
	return "\033[" + code + "m" + text + sgrReset
}

// Small helpers.

func isBlank(line string) bool {
	return strings.TrimSpace(line) == ""
}

func leadingSpaces(line string) int {
	n := 0
	for n < len(line) && line[n] == ' ' {
		n++
	}
	return n
}

// dedent removes up to n leading spaces.
func dedent(line string, n int) string {
	return line[min(leadingSpaces(line), n):]
}

func runLength(s string, i int, c byte) int {
	n := 0
	for i+n < len(s) && s[i+n] == c {
		n++
	}
	return n
}

// visibleControl shows control characters as their control-picture glyphs
// (ESC as ␛), so text echoed from tools or the web cannot move the cursor,
// retitle the window or write the clipboard, and widths stay exact.
func visibleControl(r rune) rune {
	switch {
	case r == '\n' || r == '\t':
		return r
	case r < 0x20:
		return 0x2400 + r
	case r == 0x7f:
		return '␡'
	case r >= 0x80 && r <= 0x9f:
		return '\uFFFD'
	}
	return r
}

// expandTabs replaces tabs with spaces to the next multiple of four columns.
func expandTabs(line string) string {
	if !strings.Contains(line, "\t") {
		return line
	}
	var builder strings.Builder
	col := 0
	for _, r := range line {
		if r == '\t' {
			spaces := 4 - col%4
			builder.WriteString(strings.Repeat(" ", spaces))
			col += spaces
			continue
		}
		builder.WriteRune(r)
		col += ansi.StringWidth(string(r))
	}
	return builder.String()
}

func isASCIIPunct(c byte) bool {
	return strings.IndexByte("!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~", c) >= 0
}

func isWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

func isScheme(s string) bool {
	if len(s) > 32 {
		return false
	}
	for i, r := range s {
		ok := unicode.IsLetter(r) || (i > 0 && (unicode.IsDigit(r) || strings.ContainsRune("+.-", r)))
		if !ok || r > unicode.MaxASCII {
			return false
		}
	}
	return true
}
