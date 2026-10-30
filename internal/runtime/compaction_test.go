package runtime_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"aos/internal/artifact"
	"aos/internal/ipc"
	"aos/internal/kernel"
	"aos/internal/runtime"
	"aos/internal/tools"
)

// segment is the part of a memory_segment payload these tests read.
type segment struct {
	FromSeq  int               `json:"from_seq"`
	ToSeq    int               `json:"to_seq"`
	Messages []runtime.Message `json:"messages"`
}

func readSegment(t *testing.T, store *artifact.Store, id string) segment {
	t.Helper()
	var s segment
	if err := store.ReadJSON(id, &s); err != nil {
		t.Fatalf("read segment %s: %v", id, err)
	}
	return s
}

func chars(messages []runtime.Message) int {
	total := 0
	for _, message := range messages {
		total += len(message.Role) + len(message.Content)
	}
	return total
}

func compactionEvents(t *testing.T, k *kernel.Kernel) []kernel.Event {
	t.Helper()
	events, err := k.Events()
	if err != nil {
		t.Fatal(err)
	}
	var out []kernel.Event
	for _, event := range events {
		if event.Type == "memory.compacted" {
			out = append(out, event)
		}
	}
	return out
}

// markedMessage is a message action of about size characters that names
// its call, so tests can tell where its text ended up.
func markedMessage(call, size int) runtime.GenerateResult {
	text := fmt.Sprintf("marker-%03d ", call)
	return runtime.GenerateResult{Action: runtime.GenerateAction{Type: "message", Message: text + strings.Repeat("m", size-len(text))}}
}

func newMemoryFixture(t *testing.T) (*kernel.Kernel, *artifact.Store, kernel.Process) {
	t.Helper()
	root := t.TempDir()
	k, err := kernel.New(filepath.Join(root, "kernel"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := artifact.New(filepath.Join(root, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	process, err := k.Spawn(kernel.ProcessSpec{Role: "agent:writer"})
	if err != nil {
		t.Fatal(err)
	}
	return k, store, process
}

// A policy whose hysteresis one tool output can defeat is refused before
// anything runs; a 64k window passes, a 16k window does not.
func TestCompactionRejectsCapsOverHysteresis(t *testing.T) {
	if err := runtime.ContextCompactionPolicy(65536, 0.8).Validate(tools.MaxToolOutputBytes); err != nil {
		t.Fatalf("a 64k window should leave room for a 64 KB output: %v", err)
	}
	if err := runtime.ContextCompactionPolicy(16384, 0.8).Validate(tools.MaxToolOutputBytes); err == nil {
		t.Fatal("a 16k window compacts at about 39k chars; a 64 KB output must be refused")
	}
	if err := (runtime.CompactionPolicy{MaxChars: 1000, TargetChars: 1000}).Validate(10); err == nil {
		t.Fatal("a target at the trigger has no hysteresis")
	}
	if err := (runtime.CompactionPolicy{}).Validate(tools.MaxToolOutputBytes); err != nil {
		t.Fatalf("the default policy is for an unknown window and is not validated: %v", err)
	}
}

// After compacting, live history is at most the target, so the next
// compaction is at least the gap between trigger and target away.
func TestCompactionHasHysteresis(t *testing.T) {
	k, store, process := newMemoryFixture(t)
	policy := runtime.CompactionPolicy{MaxChars: 20000}
	var histories [][]runtime.Message
	var refs []int
	backend := &funcBackend{fn: func(call int, _ context.Context, request runtime.GenerateRequest) (runtime.GenerateResult, error) {
		histories = append(histories, request.History)
		refs = append(refs, len(request.MemoryRefs))
		if call == 40 {
			return exitResult("done"), nil
		}
		return markedMessage(call, 1000), nil
	}}
	agent := &runtime.AgentRuntime{Kernel: k, Artifacts: store, Tools: tools.NewRuntime(), Backend: backend, MaxSteps: 40, Compaction: policy}
	if _, err := agent.Run(context.Background(), process.PID, nil); err != nil {
		t.Fatal(err)
	}
	compactedAt := []int{}
	for i, history := range histories {
		if size := chars(history); size > policy.MaxChars {
			t.Fatalf("call %d saw %d chars, over the %d trigger", i+1, size, policy.MaxChars)
		}
		if i > 0 && refs[i] > refs[i-1] {
			compactedAt = append(compactedAt, i)
			if size := chars(history); size > policy.MaxChars/2 {
				t.Fatalf("call %d: compaction left %d chars live, over the target %d", i+1, size, policy.MaxChars/2)
			}
		}
	}
	if len(compactedAt) < 2 {
		t.Fatalf("expected several compactions, got %v", compactedAt)
	}
	// Each step adds about 2,300 chars, so 10,000 chars of headroom is at
	// least four steps.
	for i := 1; i < len(compactedAt); i++ {
		if gap := compactedAt[i] - compactedAt[i-1]; gap < 4 {
			t.Fatalf("compactions %d steps apart (%v): no hysteresis", gap, compactedAt)
		}
	}
	for _, event := range compactionEvents(t, k) {
		if event.Data["oversized"] != "false" {
			t.Fatalf("no turn here is over the target: %+v", event.Data)
		}
	}
}

// pairedHistory is turns of an action, its large result and a notice.
func pairedHistory(turns int) []runtime.Message {
	messages := []runtime.Message{{Role: "user", Content: "goal"}}
	for i := 0; i < turns; i++ {
		messages = append(messages,
			runtime.Message{Role: "assistant", Content: fmt.Sprintf(`{"action":{"type":"tool","tool_call":{"name":"read_file","args":{"path":"f%d"}}}}`, i)},
			runtime.Message{Role: "tool", Content: fmt.Sprintf("read_file:map[content:%s]", strings.Repeat("r", 1500))},
			runtime.Message{Role: "tool", Content: "tool_notice name=read_file useful=false"},
		)
	}
	return messages
}

// A cut never separates a tool call from its result: the segment ends at
// the end of a turn and the live history starts at the beginning of one.
func TestCompactionCutsCleanly(t *testing.T) {
	k, store, process := newMemoryFixture(t)
	var first runtime.GenerateRequest
	backend := &funcBackend{fn: func(call int, _ context.Context, request runtime.GenerateRequest) (runtime.GenerateResult, error) {
		if call == 1 {
			first = request
		}
		return exitResult("done"), nil
	}}
	// A turn is 1,659 chars. The target admits three turns plus the
	// previous turn's notice (5,020 chars), so only the turn rule keeps
	// the cut off that notice.
	agent := &runtime.AgentRuntime{Kernel: k, Artifacts: store, Tools: tools.NewRuntime(), Backend: backend,
		Compaction: runtime.CompactionPolicy{MaxChars: 12000, TargetChars: 5030}}
	if _, err := agent.Run(context.Background(), process.PID, pairedHistory(10)); err != nil {
		t.Fatal(err)
	}
	if len(first.MemoryRefs) != 1 {
		t.Fatalf("expected one compaction, refs %+v", first.MemoryRefs)
	}
	if first.History[0].Role != "assistant" {
		t.Fatalf("live history starts mid-turn with a %s message", first.History[0].Role)
	}
	archived := readSegment(t, store, first.MemoryRefs[0].ArtifactID).Messages
	if last := archived[len(archived)-1]; !strings.HasPrefix(last.Content, "tool_notice") {
		t.Fatalf("segment ends mid-turn with %q", last.Content)
	}
	if size := chars(first.History); size > 5030 {
		t.Fatalf("live history %d chars, over the target", size)
	}
}

// When the latest turn alone is over the target, compaction keeps it whole
// and says so.
func TestOversizedTailIsReported(t *testing.T) {
	k, store, process := newMemoryFixture(t)
	history := []runtime.Message{
		{Role: "user", Content: "goal"},
		{Role: "assistant", Content: "first action"},
		{Role: "tool", Content: "small result"},
		{Role: "assistant", Content: "second action"},
		{Role: "tool", Content: strings.Repeat("big", 2000)},
	}
	var first runtime.GenerateRequest
	backend := &funcBackend{fn: func(call int, _ context.Context, request runtime.GenerateRequest) (runtime.GenerateResult, error) {
		if call == 1 {
			first = request
		}
		return exitResult("done"), nil
	}}
	agent := &runtime.AgentRuntime{Kernel: k, Artifacts: store, Tools: tools.NewRuntime(), Backend: backend,
		Compaction: runtime.CompactionPolicy{MaxChars: 4000}}
	if _, err := agent.Run(context.Background(), process.PID, history); err != nil {
		t.Fatal(err)
	}
	if len(first.History) != 2 || first.History[0].Content != "second action" {
		t.Fatalf("expected the latest turn kept whole, got %+v", first.History)
	}
	events := compactionEvents(t, k)
	if len(events) != 1 || events[0].Data["oversized"] != "true" || events[0].Data["archived"] != "3" {
		t.Fatalf("expected one oversized compaction of 3 messages, got %+v", events)
	}
}

// checkpointMessages reads the messages of a checkpoint.
func checkpointMessages(t *testing.T, store *artifact.Store, id string) runtime.Session {
	t.Helper()
	var session runtime.Session
	if err := store.ReadJSON(id, &session); err != nil {
		t.Fatal(err)
	}
	return session
}

// Sequence numbers run from 1 without gaps through every segment and the
// live history, and a recovered process continues them.
func TestMessageSeqSurvivesRecovery(t *testing.T) {
	k, store, process := newMemoryFixture(t)
	policy := runtime.CompactionPolicy{MaxChars: 12000}
	first := &funcBackend{fn: func(call int, _ context.Context, _ runtime.GenerateRequest) (runtime.GenerateResult, error) {
		if call == 15 {
			return runtime.GenerateResult{}, errors.New("connection reset")
		}
		return markedMessage(call, 1000), nil
	}}
	agent := &runtime.AgentRuntime{Kernel: k, Artifacts: store, Tools: tools.NewRuntime(), Backend: first, MaxSteps: 40, Compaction: policy, BackendRetry: runtime.BackendRetryPolicy{Attempts: -1}}
	failed, err := agent.Run(context.Background(), process.PID, []runtime.Message{{Role: "user", Content: "goal"}})
	if err == nil || !failed.ExitStatus.Retryable {
		t.Fatalf("expected a retryable failure, got %+v %v", failed.ExitStatus, err)
	}
	agent.Backend = &funcBackend{fn: func(call int, _ context.Context, _ runtime.GenerateRequest) (runtime.GenerateResult, error) {
		if call == 10 {
			return exitResult("done"), nil
		}
		return markedMessage(100+call, 1000), nil
	}}
	recovered, err := agent.Recover(context.Background(), process.PID, failed.ExitStatus.Checkpoint, nil, runtime.RunOptions{Extend: runtime.Extension{Steps: 20}})
	if err != nil {
		t.Fatal(err)
	}
	final := checkpointMessages(t, store, recovered.CheckpointID)

	var seqs []int
	for _, ref := range final.MemoryRefs {
		s := readSegment(t, store, ref.ArtifactID)
		if s.FromSeq != s.Messages[0].Seq || s.ToSeq != s.Messages[len(s.Messages)-1].Seq {
			t.Fatalf("segment %s range %d-%d does not match its messages", ref.ArtifactID, s.FromSeq, s.ToSeq)
		}
		for _, message := range s.Messages {
			seqs = append(seqs, message.Seq)
		}
	}
	if len(final.MemoryRefs) < 3 {
		t.Fatalf("expected compactions before and after recovery, got %d", len(final.MemoryRefs))
	}
	for _, message := range final.Messages {
		seqs = append(seqs, message.Seq)
	}
	for i, seq := range seqs {
		if seq != i+1 {
			t.Fatalf("message %d has seq %d; sequence numbers must run 1..n without gaps: %v", i, seq, seqs)
		}
	}
	if final.NextSeq != len(seqs) {
		t.Fatalf("next_seq %d, want %d", final.NextSeq, len(seqs))
	}
}

// legacyCheckpoint writes what a run before numbering left behind: a
// segment without sequence numbers and a checkpoint whose live history
// has none, ending with a read_memory result that copied the segment.
func legacyCheckpoint(t *testing.T, store *artifact.Store, pid string) (checkpointID, segmentID string) {
	t.Helper()
	var older []runtime.Message
	for i := 0; i < 5; i++ {
		older = append(older, runtime.Message{Role: "user", Content: fmt.Sprintf("legacy-%d", i)})
	}
	seg, err := store.CreateJSON("memory_segment", pid, map[string]any{
		"process_id": pid, "sequence": 1, "message_count": 5, "messages": older,
	}, nil, map[string]string{"sequence": "1"})
	if err != nil {
		t.Fatal(err)
	}
	session := runtime.Session{
		Messages: []runtime.Message{
			{Role: "assistant", Content: `{"action":{"type":"tool","tool_call":{"name":"read_memory","args":{"artifact_id":"` + seg.ID + `"}}}}`},
			{Role: "tool", Content: "read_memory:map[artifact_id:" + seg.ID + " message_count:5 messages:user: legacy-0 " + strings.Repeat("c", 3000) + " offset:0 total_bytes:3020]"},
		},
		MemoryRefs:     []runtime.MemoryRef{{ArtifactID: seg.ID, MessageCount: 5}},
		MemorySequence: 1,
	}
	checkpoint, err := store.CreateJSON("checkpoint", pid, session, []string{seg.ID}, map[string]string{"kind": "intermediate"})
	if err != nil {
		t.Fatal(err)
	}
	return checkpoint.ID, seg.ID
}

// A checkpoint from before numbering resumes: its live messages are
// numbered after its archived ones, its old segments still read, and its
// old read_memory copy is archived as a reference, not copied again.
func TestUnnumberedCheckpointsStillResume(t *testing.T) {
	k, store, process := newMemoryFixture(t)
	checkpointID, segmentID := legacyCheckpoint(t, store, process.PID)
	var requests []runtime.GenerateRequest
	backend := &funcBackend{fn: func(call int, _ context.Context, request runtime.GenerateRequest) (runtime.GenerateResult, error) {
		requests = append(requests, request)
		switch call {
		case 1:
			return toolAction("read_memory", map[string]string{"artifact_id": segmentID}), nil
		case 8:
			return exitResult("done"), nil
		}
		return markedMessage(call, 1000), nil
	}}
	agent := &runtime.AgentRuntime{Kernel: k, Artifacts: store, Tools: tools.NewRuntime(), Backend: backend, MaxSteps: 20,
		Compaction: runtime.CompactionPolicy{MaxChars: 9000}}
	if _, err := agent.Resume(context.Background(), process.PID, checkpointID, nil, runtime.RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := requests[0].History[0].Seq; got != 6 {
		t.Fatalf("the first live message after 5 archived ones should be seq 6, got %d", got)
	}
	if read := requests[1].History[len(requests[1].History)-1].Content; !strings.Contains(read, "legacy-0") {
		t.Fatalf("an old segment should still read back, got %.200q", read)
	}
	last := requests[len(requests)-1].MemoryRefs
	if len(last) < 2 {
		t.Fatalf("expected a new compaction, refs %+v", last)
	}
	archived := readSegment(t, store, last[1].ArtifactID)
	if archived.FromSeq != 6 {
		t.Fatalf("the first new segment should start at seq 6, got %d", archived.FromSeq)
	}
	for _, message := range archived.Messages {
		if strings.Contains(message.Content, "legacy-0") {
			t.Fatalf("an old read_memory copy was archived again: %.200q", message.Content)
		}
	}
	if !strings.Contains(archived.Messages[1].Content, "read_memory result ("+segmentID) {
		t.Fatalf("the old read should be archived as a reference, got %.200q", archived.Messages[1].Content)
	}
}

// archiveScenario runs an agent that reads its first segment back, keeps
// working until that read is itself archived, then reads the segment that
// archived it. It returns the store, the final refs and the two reads.
func archiveScenario(t *testing.T) (*artifact.Store, []runtime.MemoryRef, []string) {
	t.Helper()
	k, store, process := newMemoryFixture(t)
	var refs []runtime.MemoryRef
	var reads []string
	phase := 0
	readArchived := func(history []runtime.Message) bool {
		for _, message := range history {
			if strings.HasPrefix(message.Content, "read_memory:") {
				return false
			}
		}
		return true
	}
	backend := &funcBackend{fn: func(call int, _ context.Context, request runtime.GenerateRequest) (runtime.GenerateResult, error) {
		refs = request.MemoryRefs
		if n := len(request.History); n > 0 && strings.HasPrefix(request.History[n-1].Content, "read_memory:") {
			reads = append(reads, request.History[n-1].Content)
		}
		switch {
		case phase == 0 && len(refs) == 1:
			phase = 1
			return toolAction("read_memory", map[string]string{"artifact_id": refs[0].ArtifactID}), nil
		case phase == 1 && readArchived(request.History):
			phase = 2
			return toolAction("read_memory", map[string]string{"artifact_id": refs[len(refs)-1].ArtifactID}), nil
		case phase == 2:
			return exitResult("done"), nil
		}
		return markedMessage(call, 3000), nil
	}}
	agent := &runtime.AgentRuntime{Kernel: k, Artifacts: store, Tools: tools.NewRuntime(), Backend: backend, MaxSteps: 80,
		Compaction: runtime.CompactionPolicy{MaxChars: 60000}}
	if _, err := agent.Run(context.Background(), process.PID, nil); err != nil {
		t.Fatal(err)
	}
	if phase != 2 || len(reads) != 2 {
		t.Fatalf("scenario did not finish: phase %d, %d reads", phase, len(reads))
	}
	return store, refs, reads
}

// Each original message is stored in exactly one segment: a read_memory
// result is archived as a reference to what it read.
func TestArchiveStoresEachMessageOnce(t *testing.T) {
	store, refs, _ := archiveScenario(t)
	seen := map[int]string{}
	firstRead := false
	for _, ref := range refs {
		for _, message := range readSegment(t, store, ref.ArtifactID).Messages {
			if previous, ok := seen[message.Seq]; ok {
				t.Fatalf("seq %d stored in %s and %s", message.Seq, previous, ref.ArtifactID)
			}
			seen[message.Seq] = ref.ArtifactID
			if strings.Contains(message.Content, "marker-001 ") && ref.ArtifactID != refs[0].ArtifactID {
				t.Fatalf("%s holds a copy of the first segment", ref.ArtifactID)
			}
			if strings.HasPrefix(message.Content, "read_memory result ("+refs[0].ArtifactID+", bytes 0-") {
				firstRead = true
			}
		}
	}
	if !firstRead {
		t.Fatal("the archived read should be a reference naming the segment and bytes it read")
	}
}

// read_memory of a segment holding a reference returns the reference, not
// the text it names, so a read of a read stays small.
func TestReadMemoryDoesNotFollowReferences(t *testing.T) {
	_, refs, reads := archiveScenario(t)
	if !strings.Contains(reads[0], "marker-001 ") {
		t.Fatalf("the first read should return the first segment, got %.200q", reads[0])
	}
	if strings.Contains(reads[1], "marker-001 ") {
		t.Fatalf("reading %s followed a reference into the first segment", refs[len(refs)-1].ArtifactID)
	}
	if !strings.Contains(reads[1], "read_memory result ("+refs[0].ArtifactID) {
		t.Fatalf("the second read should show the reference, got %.300q", reads[1])
	}
}

// worstRun is a run action whose result is as large as a tool result can
// be: 70 KB on each stream, and a command padded to pad bytes.
func worstRun(pad int) runtime.GenerateResult {
	command := "head -c 70000 /dev/zero | tr '\\0' o; head -c 70000 /dev/zero | tr '\\0' e >&2 # " + strings.Repeat("c", pad)
	return toolAction("run", map[string]string{"command": command})
}

// With a 64k window, the largest tool result fits the hysteresis gap, so a
// run of worst-case results never compacts on consecutive steps. A turn
// whose own action pushes it past the gap may, and every such compaction is
// reported as oversized.
func TestHysteresisHoldsForTheLargestTurn(t *testing.T) {
	policy := runtime.ContextCompactionPolicy(65536, 0.8)
	if err := policy.Validate(tools.MaxToolResultBytes); err != nil {
		t.Fatalf("a 64k window should hold the largest tool result: %v", err)
	}
	for _, tc := range []struct {
		name          string
		pad           int
		consecutiveOK bool
	}{
		{"largest result", 5000, false},
		{"large action", 40000, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k, store, process := newCancelFixture(t)
			var refs []int
			backend := &funcBackend{fn: func(call int, _ context.Context, request runtime.GenerateRequest) (runtime.GenerateResult, error) {
				refs = append(refs, len(request.MemoryRefs))
				for _, message := range request.History {
					if message.Role == "tool" && len(message.Content) > tools.MaxToolResultBytes {
						t.Errorf("a %d-byte tool result is over the bound", len(message.Content))
					}
				}
				if call == 9 {
					return exitResult("done"), nil
				}
				return worstRun(tc.pad), nil
			}}
			// Recall aids off: a re-noticed handoff compacts mid-step, which
			// TestHandoffOverrunIsBounded covers.
			agent := &runtime.AgentRuntime{Kernel: k, Artifacts: store, Tools: tools.NewRuntime(), Backend: backend, MaxSteps: 12, Compaction: policy, Recall: runtime.RecallOff()}
			if _, err := agent.Run(context.Background(), process.PID, nil); err != nil {
				t.Fatal(err)
			}
			events := compactionEvents(t, k)
			if len(events) < 2 {
				t.Fatalf("expected repeated compaction, got %d", len(events))
			}
			for i := 2; i < len(refs); i++ {
				consecutive := refs[i] > refs[i-1] && refs[i-1] > refs[i-2]
				if consecutive && !tc.consecutiveOK {
					t.Fatalf("compacted on consecutive steps %d and %d: %v", i-1, i, refs)
				}
			}
			if tc.consecutiveOK {
				for _, event := range events[1:] {
					if event.Data["oversized"] != "true" {
						t.Fatalf("a turn past the gap should be reported oversized: %+v", event.Data)
					}
				}
			}
		})
	}
}

// A run that compacts and then fails before checkpointing leaves a segment
// its last checkpoint does not know. Recovering replays the compaction:
// it adopts that segment and cuts where it did, so no message is stored
// twice.
func TestCompactionIsIdempotentAcrossRecover(t *testing.T) {
	k, store, process := newMemoryFixture(t)
	policy := runtime.CompactionPolicy{MaxChars: 12000}
	var firstRefs []runtime.MemoryRef
	first := &funcBackend{fn: func(call int, _ context.Context, request runtime.GenerateRequest) (runtime.GenerateResult, error) {
		if len(request.MemoryRefs) > 0 {
			firstRefs = request.MemoryRefs
			return runtime.GenerateResult{}, errors.New("connection reset")
		}
		return markedMessage(call, 1000), nil
	}}
	agent := &runtime.AgentRuntime{Kernel: k, Artifacts: store, Tools: tools.NewRuntime(), Backend: first, MaxSteps: 40, Compaction: policy, BackendRetry: runtime.BackendRetryPolicy{Attempts: -1}}
	failed, err := agent.Run(context.Background(), process.PID, []runtime.Message{{Role: "user", Content: "goal"}})
	if err == nil || len(firstRefs) != 1 {
		t.Fatalf("expected a failure right after the first compaction, refs %+v, err %v", firstRefs, err)
	}
	var recoveredRefs []runtime.MemoryRef
	agent.Backend = &funcBackend{fn: func(call int, _ context.Context, request runtime.GenerateRequest) (runtime.GenerateResult, error) {
		if call == 1 {
			recoveredRefs = request.MemoryRefs
		}
		return exitResult("done"), nil
	}}
	if _, err := agent.Recover(context.Background(), process.PID, failed.ExitStatus.Checkpoint, nil, runtime.RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(recoveredRefs) != 1 || recoveredRefs[0].ArtifactID != firstRefs[0].ArtifactID {
		t.Fatalf("recovery should adopt %s, got %+v", firstRefs[0].ArtifactID, recoveredRefs)
	}
	records, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	segments := 0
	for _, record := range records {
		if record.Type == "memory_segment" {
			segments++
		}
	}
	if segments != 1 {
		t.Fatalf("the replayed compaction wrote another segment: %d segments", segments)
	}
	events := compactionEvents(t, k)
	if len(events) != 2 || events[0].Data["adopted"] != "false" || events[1].Data["adopted"] != "true" {
		t.Fatalf("expected the replay recorded as adopted: %+v", events)
	}
}

// Turns appended after a checkpoint, such as inbox messages, can be in the
// archived range and can differ on replay while getting the same numbers.
// A replay adopts a segment only when its messages are exactly the ones the
// segment holds; otherwise it cuts its own, and no message is lost.
func TestAdoptionRequiresIdenticalMessages(t *testing.T) {
	k, store, process := newMemoryFixture(t)
	bus, err := ipc.New(t.TempDir(), k)
	if err != nil {
		t.Fatal(err)
	}
	body := func(name string) string { return name + " " + strings.Repeat("b", 3000) }
	policy := runtime.CompactionPolicy{MaxChars: 5000}
	first := &funcBackend{fn: func(call int, _ context.Context, request runtime.GenerateRequest) (runtime.GenerateResult, error) {
		if call == 1 {
			for _, name := range []string{"inbox-one", "inbox-two"} {
				if _, err := bus.Send("user", process.PID, body(name)); err != nil {
					t.Fatal(err)
				}
			}
			return runtime.GenerateResult{Action: runtime.GenerateAction{Type: "message", Message: "noted"}}, nil
		}
		return runtime.GenerateResult{}, errors.New("connection reset")
	}}
	agent := &runtime.AgentRuntime{Kernel: k, Artifacts: store, Bus: bus, Tools: tools.NewRuntime(), Backend: first, MaxSteps: 10, Compaction: policy, BackendRetry: runtime.BackendRetryPolicy{Attempts: -1}}
	failed, err := agent.Run(context.Background(), process.PID, []runtime.Message{{Role: "user", Content: "goal"}})
	if err == nil {
		t.Fatal("expected the crash")
	}
	crashed := compactionEvents(t, k)
	if len(crashed) != 1 {
		t.Fatalf("expected one compaction before the crash, got %+v", crashed)
	}
	archivedFirst := readSegment(t, store, crashed[0].Data["segment"]).Messages
	if last := archivedFirst[len(archivedFirst)-1].Content; !strings.HasPrefix(last, "inbox-one") {
		t.Fatalf("the crashed compaction should archive the first inbox message, ended with %.30q", last)
	}

	// The replay gets a different batch: the operator's line, then the
	// redelivered inbox. It takes the numbers inbox-one had.
	var live []runtime.Message
	var refs []runtime.MemoryRef
	agent.Backend = &funcBackend{fn: func(call int, _ context.Context, request runtime.GenerateRequest) (runtime.GenerateResult, error) {
		if call == 1 {
			live, refs = request.History, request.MemoryRefs
		}
		return exitResult("done"), nil
	}}
	if _, err := agent.Recover(context.Background(), process.PID, failed.ExitStatus.Checkpoint, []runtime.Message{{Role: "user", Content: body("operator-line")}}, runtime.RunOptions{}); err != nil {
		t.Fatal(err)
	}
	events := compactionEvents(t, k)
	if len(events) != 2 || events[1].Data["adopted"] != "false" || !strings.Contains(events[1].Data["not_adopted"], crashed[0].Data["segment"]) {
		t.Fatalf("a segment holding other messages must not be adopted: %+v", events)
	}
	var seen []runtime.Message
	for _, ref := range refs {
		seen = append(seen, readSegment(t, store, ref.ArtifactID).Messages...)
	}
	seen = append(seen, live...)
	for _, name := range []string{"goal", "operator-line", "inbox-one", "inbox-two"} {
		found := false
		for _, message := range seen {
			if strings.HasPrefix(message.Content, name) {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s is in neither the archive nor the live history", name)
		}
	}
}
