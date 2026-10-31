// Command reparse replays every index_memory call of the follow-up's 27
// handoffs (calls.json, from extract.py) through the index parser and cap
// of runtime 50a7cfe (old) and of 6c55bdb (new), each copied verbatim from
// internal/runtime/recall.go, with indexMemory's cap and the step's
// re-parse of the canonical index (runtime.go). It reports, per handoff,
// whether the stored index and every call's refusals are the same under
// both, and writes the new parse's indexes as stored (reparsed.json).
//
// Usage, from the repository root:
//
//	go run ./reports/memory-probe-recall-header/reparse reports/memory-probe-recall-header/reparse
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

type IndexLine struct {
	From int    `json:"from"`
	To   int    `json:"to"`
	Text string `json:"text"`
}

func (l IndexLine) String() string {
	if l.To != l.From {
		return fmt.Sprintf("%d-%d: %s", l.From, l.To, l.Text)
	}
	return fmt.Sprintf("%d: %s", l.From, l.Text)
}

func RenderIndex(lines []IndexLine) string {
	var builder strings.Builder
	for _, line := range lines {
		builder.WriteString(line.String() + "\n")
	}
	return builder.String()
}

const evidenceForms = `12, 12-15, #12, #12-#15, msg 12 or messages 12-15`

var (
	evidenceRange  = regexp.MustCompile(`^(\d+)(?:-(\d+))?$`)
	evidencePrefix = regexp.MustCompile(`(?i)^(?:messages?|msgs?)\s*`)
)

// messageRange reads a message number or range in any accepted form and
// returns it canonically ("12" or "12-15"); ok is false for anything else,
// such as an artifact ID.
func messageRange(part string) (from, to int, canonical string, ok bool) {
	text := strings.TrimSpace(evidencePrefix.ReplaceAllString(strings.TrimSpace(part), ""))
	text = strings.ReplaceAll(text, "#", "")
	text = strings.Join(strings.Fields(text), "")
	m := evidenceRange.FindStringSubmatch(text)
	if m == nil {
		return 0, 0, "", false
	}
	from, _ = strconv.Atoi(m[1])
	to = from
	canonical = m[1]
	if m[2] != "" {
		to, _ = strconv.Atoi(m[2])
		canonical = fmt.Sprintf("%d-%d", from, to)
	}
	return from, to, canonical, true
}

// runOnEntry finds where a later entry may start inside a line: a number
// or range after whitespace, followed by ": ".
var runOnEntry = regexp.MustCompile(`\s(#?\d+(?:\s*-\s*#?\d+)?):\s`)

// splitRunOn splits a line holding several entries ("59: a 61: b") at each
// later "N:" or "N-M:" that cites messages within from-to, numbered after
// the entry before it, so a number in an entry's text ("ratio 3: 1") is
// left alone unless it is in range and above the entry's own; the agent
// sees the split index in the result.
func splitRunOn(line string, from, to int) []string {
	line = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "-*"))
	where, _, found := strings.Cut(line, ":")
	previous, _, _, ok := messageRange(where)
	if !found || !ok {
		return []string{line}
	}
	var parts []string
	start := 0
	for _, m := range runOnEntry.FindAllStringSubmatchIndex(line, -1) {
		first, last, _, ok := messageRange(line[m[2]:m[3]])
		if !ok || first <= previous || first < from || last > to || last < first {
			continue
		}
		parts = append(parts, line[start:m[2]])
		start, previous = m[2], first
	}
	return append(parts, line[start:])
}

// parseIndex reads an index_memory argument: one "N: text" or "N-M: text"
// line per entry, numbers in any evidence form, each inside from-to. A line
// that is not is rejected on its own, with the reason, and the rest kept;
// a line holding several entries is split into them.
func parseIndex(text string, from, to int) (lines []IndexLine, rejected []string) {
	var raws []string
	for _, line := range strings.Split(text, "\n") {
		raws = append(raws, splitRunOn(line, from, to)...)
	}
	for _, raw := range raws {
		raw = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(raw), "-*"))
		if raw == "" {
			continue
		}
		where, what, found := strings.Cut(raw, ":")
		what = strings.Join(strings.Fields(what), " ")
		first, last, _, ok := messageRange(where)
		switch {
		case !found || !ok || what == "":
			rejected = append(rejected, fmt.Sprintf("%s: not \"N: what is there\" or \"N-M: what is there\" (numbers as %s)", quoteLine(raw), evidenceForms))
		case first < from || last < first || last > to:
			rejected = append(rejected, fmt.Sprintf("%s: cites messages outside %d-%d, the ones being archived", quoteLine(raw), from, to))
		default:
			lines = append(lines, IndexLine{From: first, To: last, Text: what})
		}
	}
	return lines, rejected
}

// parseIndex reads an index_memory argument: one "N: text" or "N-M: text"
// line per entry, numbers in any evidence form, each inside from-to. A line
// that is not is rejected on its own, with the reason, and the rest kept.
func parseIndexOld(text string, from, to int) (lines []IndexLine, rejected []string) {
	for _, raw := range strings.Split(text, "\n") {
		raw = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(raw), "-*"))
		if raw == "" {
			continue
		}
		where, what, found := strings.Cut(raw, ":")
		what = strings.Join(strings.Fields(what), " ")
		first, last, _, ok := messageRange(where)
		switch {
		case !found || !ok || what == "":
			rejected = append(rejected, fmt.Sprintf("%s: not \"N: what is there\" or \"N-M: what is there\" (numbers as %s)", quoteLine(raw), evidenceForms))
		case first < from || last < first || last > to:
			rejected = append(rejected, fmt.Sprintf("%s: cites messages outside %d-%d, the ones being archived", quoteLine(raw), from, to))
		default:
			lines = append(lines, IndexLine{From: first, To: last, Text: what})
		}
	}
	return lines, rejected
}

// maxQuotedLine bounds how much of a rejected line its reason quotes.
const maxQuotedLine = 120

// quoteLine quotes a rejected line, cut to maxQuotedLine bytes.
func quoteLine(line string) string {
	if len(line) <= maxQuotedLine {
		return strconv.Quote(line)
	}
	cut := maxQuotedLine
	for cut > 0 && !utf8.RuneStart(line[cut]) {
		cut--
	}
	return strconv.Quote(line[:cut]) + "..."
}

// The rest is this program: indexMemory's cap and refusal (as in 6c55bdb), and the step's
// re-parse of the canonical index, unchanged since 50a7cfe.

type parser func(text string, from, to int) ([]IndexLine, []string)

type outcome struct {
	Kept     []IndexLine `json:"kept"`
	Rejected []string    `json:"rejected"`
	Refused  bool        `json:"refused"`
}

// indexMemory takes the cap the call's tool result reported: the runtime
// counts the live messages up to the handoff, which the segment does not
// all keep.
func indexMemory(parse parser, raw string, from, to, limit int) outcome {
	parsed, rejected := parse(raw, from, to)
	var lines []IndexLine
	size := 0
	for i, line := range parsed {
		if size+len(line.String())+1 > limit {
			for _, over := range parsed[i:] {
				rejected = append(rejected, fmt.Sprintf("%s: over the index's %d-byte cap for these messages", quoteLine(over.String()), limit))
			}
			break
		}
		size += len(line.String()) + 1
		lines = append(lines, line)
	}
	return outcome{Kept: lines, Rejected: rejected, Refused: len(lines) == 0}
}

type handoff struct {
	Checkpoint string      `json:"checkpoint"`
	Draw       int         `json:"draw"`
	Error      string      `json:"error"`
	From       int         `json:"from"`
	To         int         `json:"to"`
	Cap        int         `json:"cap"`
	Stored     []IndexLine `json:"stored"`
	Calls      []struct {
		Seq   int    `json:"seq"`
		Index string `json:"index"`
	} `json:"calls"`
}

type result struct {
	Checkpoint   string      `json:"checkpoint"`
	Draw         int         `json:"draw"`
	OldMatches   bool        `json:"old_replay_matches_stored"`
	SameStored   bool        `json:"same_stored"`
	SameRefusals bool        `json:"same_rejections"`
	StepError    string      `json:"step_error,omitempty"`
	OldCalls     []outcome   `json:"old_calls"`
	NewCalls     []outcome   `json:"new_calls"`
	NewStored    []IndexLine `json:"new_stored"`
}

// stored is the index the last call that kept a line leaves, after the
// step re-parses its canonical form; stepError is set if that re-parse
// rejects a line (the runtime then fails the step).
func stored(parse parser, calls []outcome, to int) (lines []IndexLine, stepError string) {
	for _, call := range calls {
		if call.Refused {
			continue
		}
		var rejected []string
		lines, rejected = parse(RenderIndex(call.Kept), 1, to)
		if len(rejected) > 0 {
			stepError = rejected[0]
		}
	}
	return lines, stepError
}

func main() {
	dir := os.Args[1]
	bytes, err := os.ReadFile(filepath.Join(dir, "calls.json"))
	if err != nil {
		panic(err)
	}
	var handoffs []handoff
	if err := json.Unmarshal(bytes, &handoffs); err != nil {
		panic(err)
	}
	var results []result
	differ := 0
	for _, h := range handoffs {
		r := result{Checkpoint: h.Checkpoint, Draw: h.Draw}
		for _, call := range h.Calls {
			r.OldCalls = append(r.OldCalls, indexMemory(parseIndexOld, call.Index, h.From, h.To, h.Cap))
			r.NewCalls = append(r.NewCalls, indexMemory(parseIndex, call.Index, h.From, h.To, h.Cap))
		}
		oldStored, _ := stored(parseIndexOld, r.OldCalls, h.To)
		r.NewStored, r.StepError = stored(parseIndex, r.NewCalls, h.To)
		r.OldMatches = len(oldStored) == len(h.Stored) && (len(oldStored) == 0 || reflect.DeepEqual(oldStored, h.Stored))
		r.SameStored = len(oldStored) == len(r.NewStored) && (len(oldStored) == 0 || reflect.DeepEqual(oldStored, r.NewStored))
		r.SameRefusals = true
		for i := range r.OldCalls {
			if r.OldCalls[i].Refused != r.NewCalls[i].Refused || !reflect.DeepEqual(r.OldCalls[i].Rejected, r.NewCalls[i].Rejected) {
				r.SameRefusals = false
			}
		}
		if !r.SameStored || !r.SameRefusals || r.StepError != "" {
			differ++
		}
		fmt.Printf("%s d%d calls %d: old replay matches stored %v; old %d lines, new %d; same stored %v, same rejections %v %s\n",
			h.Checkpoint, h.Draw, len(h.Calls), r.OldMatches, len(oldStored), len(r.NewStored), r.SameStored, r.SameRefusals, r.StepError)
		results = append(results, r)
	}
	fmt.Printf("%d of %d handoffs differ\n", differ, len(handoffs))
	out, _ := json.MarshalIndent(results, "", " ")
	if err := os.WriteFile(filepath.Join(dir, "reparsed.json"), out, 0o644); err != nil {
		panic(err)
	}
}
