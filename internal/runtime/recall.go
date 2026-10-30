package runtime

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"aos/internal/artifact"
	"aos/internal/tools"
)

// RecallPolicy switches the aids that help an agent recall what it
// archived. The zero value turns every one on; each No field turns one off
// and restores the behaviour from before it, so a run with every field set
// matches a run from before the aids existed. The runtime emits each aid's
// notices; PromptBackend reads NoPointerPrompt and NoAgentIndex for the
// wording it shows.
type RecallPolicy struct {
	// NoOverwriteWarning (w1) stops update_memory from showing what a
	// replaced entry dropped and which keys its section already holds.
	NoOverwriteWarning bool `json:"no_overwrite_warning,omitempty"`
	// NoPointerPrompt (w2) restores the working-memory wording from before
	// entries were described as pointers to their evidence.
	NoPointerPrompt bool `json:"no_pointer_prompt,omitempty"`
	// NoHandoffRenotice (w4) compacts after one handoff step even when the
	// agent ignored the notice.
	NoHandoffRenotice bool `json:"no_handoff_renotice,omitempty"`
	// NoAgentIndex (a1) stops asking the agent to index what a handoff
	// archives, and refuses index_memory.
	NoAgentIndex bool `json:"no_agent_index,omitempty"`
}

// RecallOff is the policy with every aid off: the behaviour from before.
func RecallOff() RecallPolicy {
	return RecallPolicy{NoOverwriteWarning: true, NoPointerPrompt: true, NoHandoffRenotice: true, NoAgentIndex: true}
}

// Recall-notice items, as memory.notice events name them.
const (
	NoticeOverwrite = "w1"
	NoticeRenotice  = "w4"
	NoticeIndex     = "a1"
)

// noteRecall records a recall notice in the kernel event log, so fired
// notices can be counted per item without matching their text.
func (r *AgentRuntime) noteRecall(pid, item string, data map[string]string) error {
	event := map[string]string{"item": item}
	for key, value := range data {
		event[key] = value
	}
	_, err := r.Kernel.RecordEvent(pid, "memory.notice", event)
	return err
}

// BackendRetryPolicy retries a model call that failed within the same
// step, waiting Base, then twice as long each time up to Max, for at most
// Attempts calls in all. Retries are not steps: an outage must not use up
// the step limit. When they run out, the process exits retryable with a
// checkpoint, to be recovered once the endpoint is back. The zero value
// means the defaults (12 attempts, 1s doubling to 2m: about 12 minutes of
// waiting); Attempts below zero turns retrying off, so every failure takes
// a step or ends the run as before.
type BackendRetryPolicy struct {
	Attempts int
	Base     time.Duration
	Max      time.Duration
}

func (p BackendRetryPolicy) withDefaults() BackendRetryPolicy {
	if p.Attempts == 0 {
		p.Attempts = 12
	}
	if p.Base <= 0 {
		p.Base = time.Second
	}
	if p.Max <= 0 {
		p.Max = 2 * time.Minute
	}
	return p
}

// delay is the wait before retry number attempt (1 for the first retry).
func (p BackendRetryPolicy) delay(attempt int) time.Duration {
	d := p.Base
	for i := 1; i < attempt && d < p.Max; i++ {
		d *= 2
	}
	return min(d, p.Max)
}

// sleepContext waits d, or until ctx is done.
func sleepContext(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

// IndexLine is one line of an agent's index of an archived segment: the
// messages it points at and what the agent said is there. The runtime
// checked only that the numbers lie in the segment.
type IndexLine struct {
	From int    `json:"from"`
	To   int    `json:"to"`
	Text string `json:"text"`
}

// String renders the line as the agent wrote it, canonically numbered.
func (l IndexLine) String() string {
	if l.To != l.From {
		return fmt.Sprintf("%d-%d: %s", l.From, l.To, l.Text)
	}
	return fmt.Sprintf("%d: %s", l.From, l.Text)
}

// RenderIndex is an index as the header shows it and its cap counts.
func RenderIndex(lines []IndexLine) string {
	var builder strings.Builder
	for _, line := range lines {
		builder.WriteString(line.String() + "\n")
	}
	return builder.String()
}

// MaxSegmentIndexBytes caps one segment's rendered index. An index over it
// is refused, never truncated, as working memory is.
const MaxSegmentIndexBytes = 1536

// SegmentIndex reads the agent-written index of a memory_segment
// artifact: nil when the agent wrote none.
func SegmentIndex(store *artifact.Store, segmentID string) ([]IndexLine, error) {
	record, err := store.Metadata(segmentID)
	if err != nil {
		return nil, err
	}
	if record.Type != "memory_segment" {
		return nil, fmt.Errorf("%s is a %s, not a memory segment", segmentID, record.Type)
	}
	var segment memorySegment
	if err := store.ReadJSON(segmentID, &segment); err != nil {
		return nil, err
	}
	return segment.Index, nil
}

// evidenceForms is what an error about message numbers says is accepted.
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

// parseIndex reads an index_memory argument: one "N: text" or "N-M: text"
// line per entry, numbers in any evidence form, each inside from-to.
func parseIndex(text string, from, to int) ([]IndexLine, error) {
	var lines []IndexLine
	for _, raw := range strings.Split(text, "\n") {
		raw = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(raw), "-*"))
		if raw == "" {
			continue
		}
		where, what, found := strings.Cut(raw, ":")
		what = strings.Join(strings.Fields(what), " ")
		first, last, _, ok := messageRange(where)
		if !found || !ok || what == "" {
			return nil, fmt.Errorf("index_memory line %q should be \"N: what is there\" or \"N-M: what is there\", numbered as %s", raw, evidenceForms)
		}
		if first < from || last < first || last > to {
			return nil, fmt.Errorf("index_memory line %q cites messages outside the ones being archived, %d-%d", raw, from, to)
		}
		lines = append(lines, IndexLine{From: first, To: last, Text: what})
	}
	if len(lines) == 0 {
		return nil, errors.New("index_memory needs index: one \"N: what is there\" line per entry")
	}
	return lines, nil
}

// indexMemory checks the agent's index of the messages a pending handoff
// archives and returns it canonically; the step keeps it with the handoff
// until the segment is written, then it is stored in the segment, which is
// never rewritten. A second call before that replaces the first.
func (r *AgentRuntime) indexMemory(session Session, args map[string]string) (tools.Result, error) {
	if r.Recall.NoAgentIndex {
		return tools.Result{}, errors.New("index_memory is not enabled in this run")
	}
	if session.HandoffSeq == 0 || len(session.Messages) == 0 {
		return tools.Result{}, errors.New("index_memory indexes the messages a handoff notice says will move to the archive; there is no handoff pending")
	}
	from, to := session.Messages[0].Seq, session.HandoffSeq
	lines, err := parseIndex(args["index"], from, to)
	if err != nil {
		return tools.Result{}, err
	}
	if size := len(RenderIndex(lines)); size > MaxSegmentIndexBytes {
		return tools.Result{}, fmt.Errorf("index_memory: the index is %d bytes, over its %d-byte cap; keep the lines you will need to find things again, then retry", size, MaxSegmentIndexBytes)
	}
	return tools.Result{
		Tool:    "index_memory",
		Summary: "indexed archived messages",
		Output: map[string]string{
			"messages": fmt.Sprintf("%d-%d", from, to),
			"lines":    strconv.Itoa(len(lines)),
			"bytes":    fmt.Sprintf("%d of %d", len(RenderIndex(lines)), MaxSegmentIndexBytes),
			"replaced": strconv.FormatBool(len(session.HandoffIndex) > 0),
			"index":    RenderIndex(lines),
		},
	}, nil
}

// maxDroppedTokens bounds the tokens an overwrite warning lists.
const maxDroppedTokens = 20

// identifierTokens are the tokens of text that name or measure something:
// any token with a digit, or with _, ., / or : inside it, or with an upper
// case letter after its first. Tokens are runs of letters, digits and
// _.-/:, with sentence punctuation trimmed from their ends. The
// tokenizer is deterministic and knows nothing of meaning.
func identifierTokens(text string) []string {
	var out []string
	seen := map[string]bool{}
	fields := strings.FieldsFunc(text, func(c rune) bool {
		return !(unicode.IsLetter(c) || unicode.IsDigit(c) || strings.ContainsRune("_.-/:", c))
	})
	for _, token := range fields {
		token = strings.Trim(token, ".-:/")
		if token == "" || seen[token] || !identifierLike(token) {
			continue
		}
		seen[token] = true
		out = append(out, token)
	}
	return out
}

func identifierLike(token string) bool {
	for i, c := range token {
		switch {
		case unicode.IsDigit(c), strings.ContainsRune("_./:", c):
			return true
		case i > 0 && unicode.IsUpper(c):
			return true
		}
	}
	return false
}

// overwriteWarning says what replacing old with the update drops: the old
// content, the old evidence the new lacks, and old identifier tokens the
// new content lacks. It is "" when nothing was replaced.
func overwriteWarning(old MemoryEntry, update MemoryUpdate) (warning string, dropped []string) {
	newEvidence := map[string]bool{}
	for _, part := range strings.Split(update.Evidence, ",") {
		newEvidence[strings.TrimSpace(part)] = true
	}
	var lostEvidence []string
	for _, part := range strings.Split(old.Evidence, ",") {
		if part = strings.TrimSpace(part); part != "" && !newEvidence[part] {
			lostEvidence = append(lostEvidence, part)
		}
	}
	kept := map[string]bool{}
	for _, token := range identifierTokens(update.Content + " " + update.Evidence) {
		kept[token] = true
	}
	for _, token := range identifierTokens(old.Content) {
		if !kept[token] {
			dropped = append(dropped, token)
		}
	}
	more := 0
	if len(dropped) > maxDroppedTokens {
		more = len(dropped) - maxDroppedTokens
		dropped = dropped[:maxDroppedTokens]
	}
	var builder strings.Builder
	fmt.Fprintf(&builder, "runtime notice: this replaced %s/%s, whose content was: %q", update.Section, update.Key, old.Content)
	if len(lostEvidence) > 0 {
		fmt.Fprintf(&builder, "; the new entry no longer cites evidence %s", strings.Join(lostEvidence, ", "))
	}
	if len(dropped) > 0 {
		fmt.Fprintf(&builder, "; it no longer holds %s", strings.Join(dropped, ", "))
		if more > 0 {
			fmt.Fprintf(&builder, " +%d more", more)
		}
	}
	builder.WriteString(". If any of that still holds, set the entry again with it; earlier versions stay in the working_memory chain.")
	return builder.String(), dropped
}

// sectionKeys lists the keys section holds in m, except skip.
func sectionKeys(m WorkingMemory, section, skip string) []string {
	var keys []string
	for _, s := range m.Sections {
		if s.Name != section {
			continue
		}
		for _, entry := range s.Entries {
			if entry.Key != skip && !slices.Contains(keys, entry.Key) {
				keys = append(keys, entry.Key)
			}
		}
	}
	return keys
}
