package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"aos/internal/tools"
)

const (
	// maxRangeReadBytes caps one read_memory by sequence range. It is half
	// a page on purpose: a range read is aimed, so it should not refill the
	// window the way paging through a whole segment does.
	maxRangeReadBytes = 32 * 1024
	// maxSearchBytes caps a search_memory result.
	maxSearchBytes = 8 * 1024
	// searchWindow is how much text a hit shows on each side of its match.
	searchWindow = 150
	// defaultSearchLimit is how many hits a search returns unless asked.
	defaultSearchLimit = 10
	// maxIndexPaths is how many paths one segment's index entry keeps.
	maxIndexPaths = 6
)

// searchPrecedence opens every search result. Phase 3 adds working memory
// to it; until then, message numbers are the only precedence there is.
const searchPrecedence = "Archive hits are history. Hits matching more of the query come first, newest first among equals; " +
	"a later message (higher #) supersedes an earlier one, and your live history is newer than any hit."

// ToolCount is how many times a segment's messages called one tool.
type ToolCount struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// archivedSegment is one segment as retrieval sees it: every message
// numbered, and read results reduced to references.
type archivedSegment struct {
	ID       string
	Archived time.Time
	Messages []Message
}

// loadArchive reads the segments refs name, in order. Segments from before
// numbering get the numbers numberMessages would have given them: refs
// count the process's history from its start. A ref from before segments
// were artifacts has no artifact to read and only advances the count.
func (r *AgentRuntime) loadArchive(pid string, refs []MemoryRef) ([]archivedSegment, error) {
	segments := make([]archivedSegment, 0, len(refs))
	next := 0
	for _, ref := range refs {
		if ref.ArtifactID == "" {
			next += ref.MessageCount
			continue
		}
		record, err := r.Artifacts.Metadata(ref.ArtifactID)
		if err != nil {
			return nil, err
		}
		if record.Type != "memory_segment" {
			return nil, fmt.Errorf("%s is not a memory segment", ref.ArtifactID)
		}
		if err := r.ownedBy(pid, record); err != nil {
			return nil, err
		}
		var segment memorySegment
		if err := r.Artifacts.ReadJSON(ref.ArtifactID, &segment); err != nil {
			return nil, err
		}
		messages := archivedMessages(segment.Messages)
		for i := range messages {
			if messages[i].Seq == 0 {
				messages[i].Seq = next + 1
			}
			next = messages[i].Seq
		}
		segments = append(segments, archivedSegment{ID: ref.ArtifactID, Archived: record.CreatedAt, Messages: messages})
	}
	return segments, nil
}

// indexMemoryRefs gives refs written before the index existed their index
// entries, reading each such segment once. Later refs are indexed when
// their segment is written.
func (r *AgentRuntime) indexMemoryRefs(pid string, session Session) (Session, error) {
	missing := false
	for _, ref := range session.MemoryRefs {
		if ref.ArtifactID != "" && ref.ToSeq == 0 {
			missing = true
		}
	}
	if !missing {
		return session, nil
	}
	segments, err := r.loadArchive(pid, session.MemoryRefs)
	if err != nil {
		return session, err
	}
	byID := map[string]archivedSegment{}
	for _, segment := range segments {
		byID[segment.ID] = segment
	}
	refs := append([]MemoryRef(nil), session.MemoryRefs...)
	for i, ref := range refs {
		segment, ok := byID[ref.ArtifactID]
		if !ok || ref.ToSeq != 0 || len(segment.Messages) == 0 {
			continue
		}
		refs[i] = indexedRef(ref, segment.Messages, segment.Archived)
	}
	session.MemoryRefs = refs
	return session, nil
}

// indexedRef fills ref's index entry from the messages its segment holds:
// facts the runtime recorded, never a summary.
func indexedRef(ref MemoryRef, messages []Message, archived time.Time) MemoryRef {
	ref.FromSeq = messages[0].Seq
	ref.ToSeq = messages[len(messages)-1].Seq
	ref.Archived = archived.UTC().Format(time.RFC3339)
	ref.Summary = ""
	counts := map[string]int{}
	var paths []string
	seen := map[string]bool{}
	for _, message := range messages {
		call, ok := recordedToolCall(message)
		if !ok {
			continue
		}
		counts[call.Name]++
		switch call.Name {
		case "read_file", "write_file", "edit_file":
			path := strings.TrimSpace(call.Args["path"])
			if path == "" || seen[path] {
				continue
			}
			seen[path] = true
			paths = append(paths, path)
		}
	}
	ref.Tools = nil
	for name, count := range counts {
		ref.Tools = append(ref.Tools, ToolCount{Name: name, Count: count})
	}
	sort.Slice(ref.Tools, func(i, j int) bool {
		if ref.Tools[i].Count != ref.Tools[j].Count {
			return ref.Tools[i].Count > ref.Tools[j].Count
		}
		return ref.Tools[i].Name < ref.Tools[j].Name
	})
	ref.Paths = paths
	ref.MorePaths = 0
	if len(paths) > maxIndexPaths {
		ref.Paths = paths[:maxIndexPaths]
		ref.MorePaths = len(paths) - maxIndexPaths
	}
	return ref
}

// recordedToolCall reads the tool call out of an assistant turn the runtime
// recorded (actionRecord), if it is one.
func recordedToolCall(message Message) (ToolCall, bool) {
	if message.Role != "assistant" || !strings.HasPrefix(message.Content, "{") {
		return ToolCall{}, false
	}
	var record struct {
		Action struct {
			ToolCall *ToolCall `json:"tool_call"`
		} `json:"action"`
	}
	if json.Unmarshal([]byte(message.Content), &record) != nil || record.Action.ToolCall == nil || record.Action.ToolCall.Name == "" {
		return ToolCall{}, false
	}
	return *record.Action.ToolCall, true
}

// searchMemory finds query in the archive, newest message first, one hit
// per message.
func (r *AgentRuntime) searchMemory(pid string, session Session, args map[string]string) (tools.Result, error) {
	query := strings.TrimSpace(args["query"])
	if query == "" {
		return tools.Result{}, errors.New("search_memory requires query")
	}
	limit := defaultSearchLimit
	if raw := strings.TrimSpace(args["limit"]); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 {
			return tools.Result{}, errors.New("search_memory limit must be a positive number")
		}
		limit = parsed
	}
	matcher, err := searchMatcher(query)
	if err != nil {
		return tools.Result{}, err
	}
	segments, err := r.loadArchive(pid, session.MemoryRefs)
	if err != nil {
		return tools.Result{}, err
	}

	type candidate struct {
		segment archivedSegment
		message Message
		found   [][]int
		score   int
	}
	var candidates []candidate
	for s := len(segments) - 1; s >= 0; s-- {
		segment := segments[s]
		for m := len(segment.Messages) - 1; m >= 0; m-- {
			message := segment.Messages[m]
			if found, score := matcher(message.Content); score > 0 {
				candidates = append(candidates, candidate{segment, message, found, score})
			}
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score > candidates[j].score
		}
		return candidates[i].message.Seq > candidates[j].message.Seq
	})
	var hits []string
	size, matched := 0, len(candidates)
	for _, c := range candidates {
		if len(hits) >= limit {
			break
		}
		hit := searchHit(c.segment, c.message, c.found)
		if size+len(hit) > maxSearchBytes {
			continue
		}
		size += len(hit) + 1
		hits = append(hits, hit)
	}
	text := searchPrecedence + "\n"
	if matched == 0 {
		text += fmt.Sprintf("No archived message matches %q.", query)
	} else {
		text += strings.Join(hits, "\n")
		if len(hits) < matched {
			text += fmt.Sprintf("\n(%d of %d matching messages shown; narrow the query or raise limit)", len(hits), matched)
		}
		text += "\nread_memory from=N to=N reads message N in full."
	}
	return tools.Result{
		Tool:    "search_memory",
		Summary: "searched memory",
		Output: map[string]string{
			"query":   query,
			"matched": strconv.Itoa(matched),
			"shown":   strconv.Itoa(len(hits)),
			"hits":    text,
		},
	}, nil
}

// searchMatcher compiles a query into a function that returns where a
// message matches and a score, zero for no match. "re:" takes a regexp and
// scores one. Otherwise words are matched as a model tends to write them: a
// message matches an alternative (the query is split on |) when it holds
// every word of it, in any order, and scores the number of alternatives it
// matches. Both are case-insensitive.
func searchMatcher(query string) (func(string) ([][]int, int), error) {
	if expr, ok := strings.CutPrefix(query, "re:"); ok {
		re, err := regexp.Compile("(?i)" + expr)
		if err != nil {
			return nil, fmt.Errorf("search_memory query: %w", err)
		}
		return func(content string) ([][]int, int) {
			found := re.FindAllStringIndex(content, -1)
			return found, min(len(found), 1)
		}, nil
	}
	var alternatives [][]*regexp.Regexp
	for _, alternative := range strings.Split(query, "|") {
		var terms []*regexp.Regexp
		for _, word := range strings.Fields(alternative) {
			terms = append(terms, regexp.MustCompile("(?i)"+regexp.QuoteMeta(word)))
		}
		if len(terms) > 0 {
			alternatives = append(alternatives, terms)
		}
	}
	if len(alternatives) == 0 {
		return nil, errors.New("search_memory requires query")
	}
	return func(content string) ([][]int, int) {
		var found [][]int
		score := 0
		for _, terms := range alternatives {
			var hits [][]int
			for _, term := range terms {
				at := term.FindAllStringIndex(content, -1)
				if at == nil {
					hits = nil
					break
				}
				hits = append(hits, at...)
			}
			if hits != nil {
				score++
			}
			found = append(found, hits...)
		}
		sort.Slice(found, func(i, j int) bool { return found[i][0] < found[j][0] })
		return found, score
	}, nil
}

// searchHit shows the first match in message with the text around it.
func searchHit(segment archivedSegment, message Message, found [][]int) string {
	content := message.Content
	start := max(0, found[0][0]-searchWindow)
	end := min(len(content), found[0][1]+searchWindow)
	for start > 0 && !utf8.RuneStart(content[start]) {
		start++
	}
	for end < len(content) && !utf8.RuneStart(content[end]) {
		end--
	}
	window := strings.Join(strings.Fields(content[start:end]), " ")
	if start > 0 {
		window = "…" + window
	}
	if end < len(content) {
		window += "…"
	}
	matches := ""
	if len(found) > 1 {
		matches = fmt.Sprintf(" [%d matches]", len(found))
	}
	return fmt.Sprintf("#%d %s (%s, archived %s)%s: %s", message.Seq, message.Role, segment.ID, segment.Archived.UTC().Format("2006-01-02 15:04"), matches, window)
}

// readMemoryRange returns archived messages from..to by sequence number,
// whole messages up to maxRangeReadBytes. A first message larger than that
// is cut, with the artifact_id and offset that page through the rest.
func (r *AgentRuntime) readMemoryRange(pid string, session Session, args map[string]string) (tools.Result, error) {
	from, err := strconv.Atoi(strings.TrimSpace(args["from"]))
	if err != nil || from < 1 {
		return tools.Result{}, errors.New("read_memory from must be a message number of 1 or more")
	}
	to := from
	if raw := strings.TrimSpace(args["to"]); raw != "" {
		to, err = strconv.Atoi(raw)
		if err != nil || to < from {
			return tools.Result{}, errors.New("read_memory to must be a message number no smaller than from")
		}
	}
	segments, err := r.loadArchive(pid, session.MemoryRefs)
	if err != nil {
		return tools.Result{}, err
	}

	var lines []string
	size, last, nextFrom := 0, 0, 0
	for _, segment := range segments {
		// offset is where each message's line starts in the segment's paged
		// text (segmentText), for a message too large to return whole.
		offset := 0
		for _, message := range segment.Messages {
			lineStart := offset
			offset += len(message.Role) + 2 + len(message.Content) + 1
			if message.Seq < from || message.Seq > to || nextFrom != 0 {
				continue
			}
			line := fmt.Sprintf("#%d %s: %s", message.Seq, message.Role, message.Content)
			if size+len(line) > maxRangeReadBytes {
				if len(lines) > 0 {
					nextFrom = message.Seq
					continue
				}
				cut := maxRangeReadBytes
				for cut > 0 && !utf8.RuneStart(line[cut]) {
					cut--
				}
				shown := cut - (len(line) - len(message.Content))
				line = line[:cut] + fmt.Sprintf("… (message #%d is %d bytes; read_memory artifact_id=%s offset=%d reads the rest)",
					message.Seq, len(message.Content), segment.ID, lineStart+len(message.Role)+2+shown)
			}
			size += len(line) + 1
			lines = append(lines, line)
			last = message.Seq
		}
	}
	if len(lines) == 0 {
		return tools.Result{}, fmt.Errorf("read_memory: messages %d-%d are not in your archive; %s", from, to, archiveSpan(session.MemoryRefs, segments))
	}
	output := map[string]string{
		"from":     strconv.Itoa(from),
		"to":       strconv.Itoa(last),
		"messages": strings.Join(lines, "\n"),
	}
	if nextFrom != 0 {
		output["next_from"] = strconv.Itoa(nextFrom)
	}
	return tools.Result{Tool: "read_memory", Summary: "read memory range", Output: output}, nil
}

// archiveSpan says which message numbers the archive holds, for a range
// read that missed.
func archiveSpan(refs []MemoryRef, segments []archivedSegment) string {
	first, last := 0, 0
	for _, segment := range segments {
		if len(segment.Messages) == 0 {
			continue
		}
		if first == 0 {
			first = segment.Messages[0].Seq
		}
		last = segment.Messages[len(segment.Messages)-1].Seq
	}
	if first == 0 {
		return "it is empty"
	}
	return fmt.Sprintf("it holds messages %d-%d; later ones are in your live history", first, last)
}

// segmentText is a segment as read_memory pages through it by artifact_id:
// one "role: content" line per message.
func segmentText(messages []Message) string {
	lines := make([]string, 0, len(messages))
	for _, message := range messages {
		lines = append(lines, message.Role+": "+message.Content)
	}
	return strings.Join(lines, "\n")
}

// rangeReadReference stands in for a range read's result in a segment.
func rangeReadReference(from, to string) string {
	return fmt.Sprintf("read_memory result (messages %s-%s): not copied into this segment; read_memory from=%s to=%s reads it again", from, to, from, to)
}

// searchReference stands in for a search result in a segment.
func searchReference(query, matched string) string {
	return fmt.Sprintf("search_memory result (%s matching messages): not copied into this segment; search_memory query=%q runs it again", matched, query)
}
