package runtime_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"aos/internal/artifact"
	"aos/internal/capability"
	"aos/internal/kernel"
	"aos/internal/runtime"
	"aos/internal/tools"
)

// lastToolMessage is the newest tool message in history.
func lastToolMessage(history []runtime.Message) string {
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == "tool" {
			return history[i].Content
		}
	}
	return ""
}

// seqOf finds the archived message containing text and returns its number.
func seqOf(t *testing.T, store *artifact.Store, refs []runtime.MemoryRef, text string) int {
	t.Helper()
	for _, ref := range refs {
		for _, message := range readSegment(t, store, ref.ArtifactID).Messages {
			if strings.Contains(message.Content, text) {
				return message.Seq
			}
		}
	}
	t.Fatalf("%q is in no segment", text)
	return 0
}

// searchScenario states a fact early, works until it is two segments deep,
// then runs the calls in queries, one per step, and records each result.
func searchScenario(t *testing.T, calls ...func(refs []runtime.MemoryRef, results []string) runtime.GenerateResult) (*kernel.Kernel, *artifact.Store, []runtime.MemoryRef, []string) {
	t.Helper()
	k, store, process := newMemoryFixture(t)
	var refs []runtime.MemoryRef
	var results []string
	next := -1
	backend := &funcBackend{fn: func(call int, _ context.Context, request runtime.GenerateRequest) (runtime.GenerateResult, error) {
		refs = request.MemoryRefs
		if next > 0 {
			results = append(results, lastToolMessage(request.History))
		}
		switch {
		case call == 2:
			return runtime.GenerateResult{Action: runtime.GenerateAction{Type: "message", Message: "fact-alpha: TG4 stays the default, TG2 measured 47.0 ms " + strings.Repeat("f", 2000)}}, nil
		case next < 0 && len(refs) >= 2:
			next = 0
		}
		if next >= 0 {
			if next == len(calls) {
				return exitResult("done"), nil
			}
			next++
			return calls[next-1](refs, results), nil
		}
		return markedMessage(call, 3000), nil
	}}
	agent := &runtime.AgentRuntime{Kernel: k, Artifacts: store, Tools: tools.NewRuntime(), Backend: backend, MaxSteps: 120,
		Compaction: runtime.CompactionPolicy{MaxChars: 30000}}
	if _, err := agent.RunWithOptions(context.Background(), process.PID, nil, runtime.RunOptions{ContinueOnToolError: true}); err != nil {
		t.Fatal(err)
	}
	if len(results) != len(calls) {
		t.Fatalf("expected %d results, got %d", len(calls), len(results))
	}
	return k, store, refs, results
}

// hitsNewest is the number of the first hit in a newest-first result.
func hitsNewest(t *testing.T, result string) int {
	t.Helper()
	first := regexp.MustCompile(`#(\d+) assistant`).FindStringSubmatch(result)
	if first == nil {
		t.Fatalf("no hits in:\n%.300s", result)
	}
	var n int
	fmt.Sscan(first[1], &n)
	return n
}

func search(args map[string]string) func([]runtime.MemoryRef, []string) runtime.GenerateResult {
	return func([]runtime.MemoryRef, []string) runtime.GenerateResult { return toolAction("search_memory", args) }
}

// search_memory finds a fact from any segment, not only the newest, names
// the message that holds it, and lists hits newest first under the
// precedence line.
func TestSearchMemoryFindsOwnArchive(t *testing.T) {
	_, store, refs, results := searchScenario(t,
		search(map[string]string{"query": "FACT-ALPHA"}),
		search(map[string]string{"query": `re:marker-0\d\d `, "limit": "3"}),
		search(map[string]string{"query": "never-said"}),
		search(map[string]string{"query": "never-said|DEFAULT tg4"}),
		search(map[string]string{"query": "tg4 never-said"}),
		search(map[string]string{"query": "marker|fact-alpha|tg4 default", "limit": "2"}),
	)
	seq := seqOf(t, store, refs, "fact-alpha")
	if seqOf(t, store, refs[:1], "fact-alpha") != seq {
		t.Fatal("the fact should be in the oldest segment")
	}
	found := results[0]
	if !strings.Contains(found, fmt.Sprintf("#%d assistant (%s", seq, refs[0].ArtifactID)) || !strings.Contains(found, "TG2 measured 47.0 ms") {
		t.Fatalf("search should name message %d in %s and show the fact:\n%.600s", seq, refs[0].ArtifactID, found)
	}
	if !strings.Contains(found, "Archive hits are history.") || !strings.Contains(found, "supersedes an earlier one") {
		t.Fatalf("a search result must open with precedence:\n%.300s", found)
	}

	hits := regexp.MustCompile(`#(\d+) assistant`).FindAllStringSubmatch(results[1], -1)
	if len(hits) != 3 {
		t.Fatalf("limit 3 should show 3 hits:\n%.1500s", results[1])
	}
	for i := 1; i < len(hits); i++ {
		var newer, older int
		fmt.Sscan(hits[i-1][1], &newer)
		fmt.Sscan(hits[i][1], &older)
		if older >= newer {
			t.Fatalf("hits should be newest first: %v", hits)
		}
	}
	if !strings.Contains(results[1], "3 of ") || !strings.Contains(results[1], "matching messages shown") {
		t.Fatalf("a limited search should say how many matched:\n%.300s", results[1])
	}
	if !strings.Contains(results[2], "No archived message matches") {
		t.Fatalf("a miss should say so:\n%.300s", results[2])
	}
	// Words match in any order, and | separates alternatives; one word of
	// an alternative is not enough.
	if !strings.Contains(results[3], fmt.Sprintf("#%d assistant", seq)) || !strings.Contains(results[3], "matched:1 ") {
		t.Fatalf("an alternative whose words all appear, in any order, should match once:\n%.600s", results[3])
	}
	if !strings.Contains(results[4], "No archived message matches") {
		t.Fatalf("a message must hold every word of an alternative:\n%.300s", results[4])
	}
	// A message matching more alternatives ranks first, however old; the
	// rest stay newest first.
	ranked := regexp.MustCompile(`#(\d+) assistant`).FindAllStringSubmatch(results[5], -1)
	if len(ranked) != 2 || ranked[0][1] != fmt.Sprint(seq) {
		t.Fatalf("the oldest message, matching two alternatives, should rank first:\n%.800s", results[5])
	}
	var newest int
	fmt.Sscan(ranked[1][1], &newest)
	if newest != hitsNewest(t, results[1]) {
		t.Fatalf("among single matches the newest should come first, got #%d", newest)
	}
}

// search_memory reads only the caller's own archive: a session whose refs
// name another process's segment gets an error, not that segment's text.
func TestSearchMemoryFindsOwnArchiveOnly(t *testing.T) {
	k, store, owner := newMemoryFixture(t)
	other, err := k.Spawn(kernel.ProcessSpec{Role: "agent:writer"})
	if err != nil {
		t.Fatal(err)
	}
	seg, err := store.CreateJSON("memory_segment", owner.PID, map[string]any{
		"process_id": owner.PID, "sequence": 1, "message_count": 1, "from_seq": 1, "to_seq": 1,
		"messages": []runtime.Message{{Role: "assistant", Content: "owner-secret 42", Seq: 1}},
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := store.CreateJSON("checkpoint", other.PID, runtime.Session{
		Messages:   []runtime.Message{{Role: "user", Content: "go", Seq: 2}},
		MemoryRefs: []runtime.MemoryRef{{ArtifactID: seg.ID, MessageCount: 1, FromSeq: 1, ToSeq: 1}},
		NextSeq:    2,
	}, nil, map[string]string{"kind": "intermediate"})
	if err != nil {
		t.Fatal(err)
	}
	var seen []string
	var toolErr error
	backend := &funcBackend{fn: func(call int, _ context.Context, request runtime.GenerateRequest) (runtime.GenerateResult, error) {
		seen = append(seen, lastToolMessage(request.History))
		if call == 1 {
			return toolAction("search_memory", map[string]string{"query": "owner-secret"}), nil
		}
		return exitResult("done"), nil
	}}
	agent := &runtime.AgentRuntime{Kernel: k, Artifacts: store, Tools: tools.NewRuntime(), Backend: backend, MaxSteps: 5}
	options := runtime.RunOptions{ContinueOnToolError: true, OnToolError: func(_ runtime.ToolCall, err error) { toolErr = err }}
	if _, err := agent.Resume(context.Background(), other.PID, checkpoint.ID, nil, options); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(toolErr, runtime.ErrCheckpointNotOwned) {
		t.Fatalf("searching another process's segment should be refused, got %v", toolErr)
	}
	for _, text := range seen {
		if strings.Contains(text, "owner-secret 42") {
			t.Fatalf("another process's archive leaked: %.200q", text)
		}
	}
}

// read_memory by range returns whole numbered messages up to its cap, says
// where to continue, cuts a single message larger than the cap with the
// offset that pages through the rest, and says what the archive holds when
// a range misses it.
func TestReadMemoryByRange(t *testing.T) {
	k, store, process := newMemoryFixture(t)
	var refs []runtime.MemoryRef
	var reads []string
	var first, huge int
	step := -1
	backend := &funcBackend{fn: func(call int, _ context.Context, request runtime.GenerateRequest) (runtime.GenerateResult, error) {
		refs = request.MemoryRefs
		if step > 0 {
			reads = append(reads, lastToolMessage(request.History))
		}
		if call == 3 {
			return markedMessage(call, 40000), nil
		}
		if step < 0 && len(refs) >= 2 {
			step = 0
			first, huge = refs[0].FromSeq, 0
		}
		if step < 0 {
			return markedMessage(call, 3000), nil
		}
		step++
		switch step {
		case 1:
			return toolAction("read_memory", map[string]string{"from": fmt.Sprint(first), "to": fmt.Sprint(first + 30)}), nil
		case 2:
			next := regexp.MustCompile(`next_from:(\d+)`).FindStringSubmatch(reads[0])
			if next == nil {
				t.Fatalf("a range over the cap should give next_from:\n%.300s", reads[0])
			}
			return toolAction("read_memory", map[string]string{"from": next[1], "to": fmt.Sprint(first + 30)}), nil
		case 3:
			huge = seqOf(t, store, refs, "marker-003 ")
			return toolAction("read_memory", map[string]string{"from": fmt.Sprint(huge)}), nil
		case 4:
			hint := regexp.MustCompile(`read_memory artifact_id=(\S+) offset=(\d+) reads the rest`).FindStringSubmatch(reads[2])
			if hint == nil {
				t.Fatalf("a cut message should say how to read the rest:\n…%s", reads[2][max(0, len(reads[2])-400):])
			}
			return toolAction("read_memory", map[string]string{"artifact_id": hint[1], "offset": hint[2]}), nil
		case 5:
			return toolAction("read_memory", map[string]string{"from": "100000"}), nil
		}
		return exitResult("done"), nil
	}}
	agent := &runtime.AgentRuntime{Kernel: k, Artifacts: store, Tools: tools.NewRuntime(), Backend: backend, MaxSteps: 120,
		Compaction: runtime.CompactionPolicy{MaxChars: 90000}}
	if _, err := agent.RunWithOptions(context.Background(), process.PID, nil, runtime.RunOptions{ContinueOnToolError: true}); err != nil {
		t.Fatal(err)
	}
	if len(reads) != 5 {
		t.Fatalf("expected 5 reads, got %d", len(reads))
	}

	numbered := regexp.MustCompile(`(?m)(?:messages:|\n)#(\d+) `)
	page := numbered.FindAllStringSubmatch(reads[0], -1)
	if len(reads[0]) > 33*1024 || len(page) == 0 || page[0][1] != fmt.Sprint(first) {
		t.Fatalf("the first page should start at #%d and stay under the cap (%d bytes):\n%.300s", first, len(reads[0]), reads[0])
	}
	for _, message := range readSegment(t, store, refs[0].ArtifactID).Messages {
		if message.Seq == first+1 && !strings.Contains(reads[0], message.Content) {
			t.Fatal("range reads must return whole messages")
		}
	}
	rest := numbered.FindAllStringSubmatch(reads[1], -1)
	if len(rest) == 0 || rest[0][1] == page[len(page)-1][1] {
		t.Fatalf("next_from should continue after the first page, not repeat #%s", page[len(page)-1][1])
	}

	if !strings.Contains(reads[2], fmt.Sprintf("#%d assistant: ", huge)) || !strings.Contains(reads[2], "is 40") {
		t.Fatalf("a message over the cap should be cut and say its size:\n%.200s", reads[2])
	}
	shown := reads[2][strings.Index(reads[2], "marker-003 "):]
	shown = shown[:strings.Index(shown, "… (message")]
	continued := reads[3][strings.Index(reads[3], "messages:")+len("messages:"):]
	remainder := continued[:strings.IndexAny(continued, "\n ")]
	if strings.Trim(remainder, "m") != "" || len(shown)+len(remainder) != 40000 {
		t.Fatalf("the offset should continue the cut message where the range read stopped: %d shown + %d read on\n%.120s", len(shown), len(remainder), continued)
	}
	if !strings.Contains(reads[4], "not in your archive") || !strings.Contains(reads[4], fmt.Sprintf("holds messages %d-", first)) {
		t.Fatalf("a range outside the archive should say what it holds:\n%.300s", reads[4])
	}
}

// Segments from before numbering read by range under the numbers their
// checkpoint gives them: they count the process's history from its start.
func TestReadMemoryByRangeReadsUnnumberedSegments(t *testing.T) {
	k, store, process := newMemoryFixture(t)
	checkpointID, _ := legacyCheckpoint(t, store, process.PID)
	var read string
	var requests []runtime.GenerateRequest
	backend := &funcBackend{fn: func(call int, _ context.Context, request runtime.GenerateRequest) (runtime.GenerateResult, error) {
		requests = append(requests, request)
		if call == 1 {
			return toolAction("read_memory", map[string]string{"from": "2", "to": "3"}), nil
		}
		read = lastToolMessage(request.History)
		return exitResult("done"), nil
	}}
	agent := &runtime.AgentRuntime{Kernel: k, Artifacts: store, Tools: tools.NewRuntime(), Backend: backend, MaxSteps: 5}
	if _, err := agent.Resume(context.Background(), process.PID, checkpointID, nil, runtime.RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(read, "#2 user: legacy-1") || !strings.Contains(read, "#3 user: legacy-2") || strings.Contains(read, "legacy-3") {
		t.Fatalf("messages 2-3 of an unnumbered segment should be legacy-1 and legacy-2:\n%s", read)
	}
	if ref := requests[0].MemoryRefs[0]; ref.FromSeq != 1 || ref.ToSeq != 5 || ref.Archived == "" {
		t.Fatalf("an unindexed ref should be indexed before its first prompt: %+v", ref)
	}
}

// Search and range results are archived as one-hop references, like
// read_memory by artifact_id.
func TestRetrievalResultsArchiveAsReferences(t *testing.T) {
	k, store, process := newMemoryFixture(t)
	var refs []runtime.MemoryRef
	step := -1
	backend := &funcBackend{fn: func(call int, _ context.Context, request runtime.GenerateRequest) (runtime.GenerateResult, error) {
		refs = request.MemoryRefs
		switch {
		case step < 0 && len(refs) == 1:
			step = 0
			return toolAction("search_memory", map[string]string{"query": "marker-001"}), nil
		case step == 0:
			step = 1
			return toolAction("read_memory", map[string]string{"from": fmt.Sprint(refs[0].FromSeq), "to": fmt.Sprint(refs[0].FromSeq + 1)}), nil
		case step >= 1 && len(refs) >= 3:
			return exitResult("done"), nil
		}
		return markedMessage(call, 3000), nil
	}}
	agent := &runtime.AgentRuntime{Kernel: k, Artifacts: store, Tools: tools.NewRuntime(), Backend: backend, MaxSteps: 120,
		Compaction: runtime.CompactionPolicy{MaxChars: 30000}}
	if _, err := agent.Run(context.Background(), process.PID, nil); err != nil {
		t.Fatal(err)
	}
	var searched, ranged bool
	for _, ref := range refs[1:] {
		for _, message := range readSegment(t, store, ref.ArtifactID).Messages {
			if strings.Contains(message.Content, "marker-001 ") {
				t.Fatalf("%s holds a copy of what was retrieved: %.200q", ref.ArtifactID, message.Content)
			}
			searched = searched || strings.HasPrefix(message.Content, `search_memory result (1 matching messages): not copied into this segment; search_memory query="marker-001"`)
			ranged = ranged || strings.HasPrefix(message.Content, fmt.Sprintf("read_memory result (messages %d-%d)", refs[0].FromSeq, refs[0].FromSeq+1))
		}
	}
	if !searched || !ranged {
		t.Fatalf("both results should be archived as references (search %v, range %v)", searched, ranged)
	}
}

// A segment's index entry records its message numbers, the tools its
// messages called and the paths given to the file tools; it carries no
// summary.
func TestIndexRecordsToolsAndPaths(t *testing.T) {
	root := t.TempDir()
	k, err := kernel.New(filepath.Join(root, "kernel"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := artifact.New(filepath.Join(root, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	work, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	process, err := k.Spawn(kernel.ProcessSpec{Role: "agent:writer", Mounts: []capability.Mount{{Name: "workspace", HostPath: work}}, CapabilitySet: capability.Set{
		Tools: []string{"write_file", "read_file"}, ReadPaths: []string{work}, WritePaths: []string{work}}})
	if err != nil {
		t.Fatal(err)
	}
	notes := filepath.Join(work, "notes.txt")
	input := filepath.Join(work, "input.txt")
	if err := os.WriteFile(input, []byte("scene"), 0o644); err != nil {
		t.Fatal(err)
	}
	var refs []runtime.MemoryRef
	backend := &funcBackend{fn: func(call int, _ context.Context, request runtime.GenerateRequest) (runtime.GenerateResult, error) {
		refs = request.MemoryRefs
		switch {
		case len(refs) > 0:
			return exitResult("done"), nil
		case call == 1:
			return toolAction("write_file", map[string]string{"path": notes, "content": "leaf2 rejected"}), nil
		case call == 2 || call == 3:
			return toolAction("read_file", map[string]string{"path": input}), nil
		}
		return markedMessage(call, 3000), nil
	}}
	agent := &runtime.AgentRuntime{Kernel: k, Artifacts: store, Tools: tools.NewRuntime(), Backend: backend, MaxSteps: 60,
		Compaction: runtime.CompactionPolicy{MaxChars: 30000}}
	if _, err := agent.Run(context.Background(), process.PID, nil); err != nil {
		t.Fatal(err)
	}
	ref := refs[0]
	archived := readSegment(t, store, ref.ArtifactID)
	if ref.FromSeq != archived.FromSeq || ref.ToSeq != archived.ToSeq || ref.Archived == "" {
		t.Fatalf("the index should give the segment's numbers and time: %+v vs %d-%d", ref, archived.FromSeq, archived.ToSeq)
	}
	if ref.Summary != "" {
		t.Fatalf("a new ref should carry no summary: %q", ref.Summary)
	}
	want := []runtime.ToolCount{{Name: "read_file", Count: 2}, {Name: "write_file", Count: 1}}
	if fmt.Sprint(ref.Tools) != fmt.Sprint(want) {
		t.Fatalf("tools = %+v, want %+v", ref.Tools, want)
	}
	if fmt.Sprint(ref.Paths) != fmt.Sprint([]string{notes, input}) {
		t.Fatalf("paths = %v, want [%s %s]", ref.Paths, notes, input)
	}
}

// Segments from before references copied read_memory results whole. A
// search sees such a copy as the reference it would be archived as today,
// so a fact is found where it was said, not again in every copy.
func TestSearchSkipsCopiesInOldSegments(t *testing.T) {
	k, store, process := newMemoryFixture(t)
	first, err := store.CreateJSON("memory_segment", process.PID, map[string]any{
		"process_id": process.PID, "sequence": 1, "message_count": 2,
		"messages": []runtime.Message{{Role: "user", Content: "go"}, {Role: "assistant", Content: "leaf2 is 14% slower on the wide tree"}},
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.CreateJSON("memory_segment", process.PID, map[string]any{
		"process_id": process.PID, "sequence": 2, "message_count": 2,
		"messages": []runtime.Message{
			{Role: "assistant", Content: `{"action":{"type":"tool","tool_call":{"name":"read_memory","args":{"artifact_id":"` + first.ID + `"}}}}`},
			{Role: "tool", Content: "read_memory:map[artifact_id:" + first.ID + " message_count:2 messages:user: go\nassistant: leaf2 is 14% slower on the wide tree offset:0 total_bytes:50]"},
		},
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := store.CreateJSON("checkpoint", process.PID, runtime.Session{
		Messages:   []runtime.Message{{Role: "user", Content: "what about leaf2?"}},
		MemoryRefs: []runtime.MemoryRef{{ArtifactID: first.ID, MessageCount: 2}, {ArtifactID: second.ID, MessageCount: 2}},
	}, nil, map[string]string{"kind": "intermediate"})
	if err != nil {
		t.Fatal(err)
	}
	var result string
	backend := &funcBackend{fn: func(call int, _ context.Context, request runtime.GenerateRequest) (runtime.GenerateResult, error) {
		if call == 1 {
			return toolAction("search_memory", map[string]string{"query": "14% slower"}), nil
		}
		result = lastToolMessage(request.History)
		return exitResult("done"), nil
	}}
	agent := &runtime.AgentRuntime{Kernel: k, Artifacts: store, Tools: tools.NewRuntime(), Backend: backend, MaxSteps: 5}
	if _, err := agent.Resume(context.Background(), process.PID, checkpoint.ID, nil, runtime.RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "#2 assistant ("+first.ID) || !strings.Contains(result, "matched:1 ") {
		t.Fatalf("the fact should be found once, as message 2 of %s:\n%s", first.ID, result)
	}
}
