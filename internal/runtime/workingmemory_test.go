package runtime_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"aos/internal/artifact"
	"aos/internal/capability"
	"aos/internal/kernel"
	"aos/internal/runtime"
	"aos/internal/tools"
)

func setMemory(section, key, content, evidence string) runtime.GenerateResult {
	args := map[string]string{"section": section, "key": key, "content": content}
	if evidence != "" {
		args["evidence"] = evidence
	}
	return toolAction("update_memory", args)
}

// memoryRun runs actions one per step, then exits, and records every
// update_memory result and tool error.
func memoryRun(t *testing.T, agent *runtime.AgentRuntime, pid string, actions ...runtime.GenerateResult) (results []tools.Result, errs []error) {
	t.Helper()
	agent.Backend = &funcBackend{fn: func(call int, _ context.Context, _ runtime.GenerateRequest) (runtime.GenerateResult, error) {
		if call <= len(actions) {
			return actions[call-1], nil
		}
		return exitResult("done"), nil
	}}
	options := runtime.RunOptions{
		ContinueOnToolError: true,
		OnToolResult: func(result tools.Result) {
			if result.Tool == "update_memory" || result.Tool == "search_memory" {
				results = append(results, result)
			}
		},
		OnToolError: func(_ runtime.ToolCall, err error) { errs = append(errs, err) },
	}
	if _, err := agent.RunWithOptions(context.Background(), pid, []runtime.Message{{Role: "user", Content: "goal"}}, options); err != nil {
		t.Fatal(err)
	}
	return results, errs
}

func readMemory(t *testing.T, store *artifact.Store, id string) runtime.WorkingMemory {
	t.Helper()
	var memory runtime.WorkingMemory
	if err := store.ReadJSON(id, &memory); err != nil {
		t.Fatal(err)
	}
	return memory
}

// update_memory sets, replaces and deletes keyed entries, each update a new
// version whose parent is the previous one, and refuses an update over the
// cap, saying how large each section is, without writing anything.
func TestWorkingMemoryVersionsAndCaps(t *testing.T) {
	k, store, process := newMemoryFixture(t)
	agent := &runtime.AgentRuntime{Kernel: k, Artifacts: store, Tools: tools.NewRuntime(), MaxSteps: 20}
	results, errs := memoryRun(t, agent, process.PID,
		setMemory("decisions", "leaf2", "adopt leaf2, 27% faster", ""),
		setMemory("decisions", "leaf2", "rejected: the build dropped half the triangles", "1"),
		setMemory("measurements", "tg", "TG4 45.3 ms vs TG2 47.0", ""),
		toolAction("update_memory", map[string]string{"section": "measurements", "key": "tg", "delete": "true"}),
		setMemory("open", "big", strings.Repeat("y", runtime.MaxWorkingMemoryBytes), ""),
		setMemory("notes", "x", "y", ""),
	)
	if len(results) != 4 || len(errs) != 2 {
		t.Fatalf("expected 4 updates and 2 refusals, got %d and %v", len(results), errs)
	}
	if !strings.Contains(errs[0].Error(), "over its 8192-byte cap") || !strings.Contains(errs[0].Error(), "decisions ") || !strings.Contains(errs[0].Error(), "this entry: ") {
		t.Fatalf("an over-cap update should give the sizes: %v", errs[0])
	}
	if !strings.Contains(errs[1].Error(), "section must be one of") {
		t.Fatalf("an unknown section should be refused: %v", errs[1])
	}
	for i, result := range results {
		record, err := store.Metadata(result.Output["artifact_id"])
		if err != nil {
			t.Fatal(err)
		}
		if result.Output["version"] != string(rune('1'+i)) {
			t.Fatalf("update %d wrote version %s", i, result.Output["version"])
		}
		if i > 0 && (len(record.ParentIDs) != 1 || record.ParentIDs[0] != results[i-1].Output["artifact_id"]) {
			t.Fatalf("version %d should have version %d as parent: %+v", i+1, i, record.ParentIDs)
		}
	}
	last := readMemory(t, store, results[3].Output["artifact_id"])
	if entry, ok := last.Entry("decisions", "leaf2"); !ok || !strings.HasPrefix(entry.Content, "rejected") || entry.Evidence != "1" {
		t.Fatalf("leaf2 should hold its latest claim: %+v", entry)
	}
	if _, ok := last.Entry("measurements", "tg"); ok {
		t.Fatal("a deleted entry should be gone")
	}
	records, _ := store.List()
	versions := 0
	for _, record := range records {
		if record.Type == "working_memory" {
			versions++
		}
	}
	if versions != 4 {
		t.Fatalf("a refused update must write nothing: %d versions", versions)
	}
}

// Evidence must resolve: message numbers within the caller's history, or
// an artifact that exists and, if it is memory or a checkpoint, belongs to
// the caller.
func TestUpdateMemoryValidatesEvidence(t *testing.T) {
	k, store, process := newMemoryFixture(t)
	other, err := k.Spawn(kernel.ProcessSpec{Role: "agent:writer"})
	if err != nil {
		t.Fatal(err)
	}
	foreignSegment, err := store.CreateJSON("memory_segment", other.PID, map[string]any{"process_id": other.PID, "messages": []runtime.Message{}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	foreignNote, err := store.CreateJSON("published.note", other.PID, map[string]string{"body": "a measurement"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	agent := &runtime.AgentRuntime{Kernel: k, Artifacts: store, Tools: tools.NewRuntime(), MaxSteps: 20}
	results, errs := memoryRun(t, agent, process.PID,
		setMemory("decisions", "a", "ok", "1-2"),
		setMemory("decisions", "b", "bad", "0"),
		setMemory("decisions", "c", "bad", "5-3"),
		setMemory("decisions", "d", "bad", "1-9999"),
		setMemory("decisions", "e", "bad", foreignSegment.ID),
		setMemory("decisions", "f", "bad", "art-999999"),
		setMemory("decisions", "g", "ok", foreignNote.ID+", 3"),
	)
	if len(results) != 2 || len(errs) != 5 {
		t.Fatalf("expected 2 accepted and 5 refused, got %d and %v", len(results), errs)
	}
	for i, want := range []string{"not in your history", "not in your history", "not in your history", "another process's memory_segment", "neither a message range"} {
		if !strings.Contains(errs[i].Error(), want) {
			t.Fatalf("refusal %d should say %q: %v", i, want, errs[i])
		}
	}
	memory := readMemory(t, store, results[1].Output["artifact_id"])
	if entry, _ := memory.Entry("decisions", "g"); entry.Evidence != foreignNote.ID+", 3" {
		t.Fatalf("evidence should be kept as given: %+v", entry)
	}
}

// The snapshot marks decisions and rejections that cite no evidence, and
// only those.
func TestSnapshotMarksEntriesWithoutEvidence(t *testing.T) {
	memory := runtime.WorkingMemory{Sections: []runtime.MemorySection{
		{Name: "decisions", Entries: []runtime.MemoryEntry{{Key: "leaf2", Content: "rejected"}, {Key: "tg", Content: "TG4", Evidence: "275-277"}}},
		{Name: "measurements", Entries: []runtime.MemoryEntry{{Key: "base", Content: "65.8 ms"}}},
		{Name: "rejected", Entries: []runtime.MemoryEntry{{Key: "w4", Content: "8% slower"}}},
	}}
	rendered := memory.Render()
	for _, want := range []string{"- leaf2: rejected [no evidence]", "- tg: TG4 [evidence 275-277]", "- base: 65.8 ms\n", "- w4: 8% slower [no evidence]"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("snapshot lacks %q:\n%s", want, rendered)
		}
	}
}

// A replayed update_memory, the same update to the same parent, adopts the
// version an earlier attempt wrote instead of writing a second one.
func TestUpdateMemoryIsIdempotentAcrossRecover(t *testing.T) {
	k, store, process := newMemoryFixture(t)
	update := setMemory("decisions", "leaf2", "rejected: biased", "")
	agent := &runtime.AgentRuntime{Kernel: k, Artifacts: store, Tools: tools.NewRuntime(), MaxSteps: 20}
	var adopted []string
	options := runtime.RunOptions{OnToolResult: func(result tools.Result) {
		if result.Tool == "update_memory" {
			adopted = append(adopted, result.Output["adopted"]+" "+result.Output["artifact_id"])
		}
	}}
	// The first attempt writes the version, then fails before any later
	// checkpoint could matter; recovery replays from the checkpoint taken
	// before the update.
	agent.Backend = &funcBackend{fn: func(call int, _ context.Context, _ runtime.GenerateRequest) (runtime.GenerateResult, error) {
		switch call {
		case 1:
			return runtime.GenerateResult{Action: runtime.GenerateAction{Type: "message", Message: "starting"}}, nil
		case 2:
			return update, nil
		}
		return runtime.GenerateResult{}, errors.New("connection reset")
	}}
	options.StopOnAssistantMessage = true
	before, err := agent.RunWithOptions(context.Background(), process.PID, []runtime.Message{{Role: "user", Content: "goal"}}, options)
	if err != nil {
		t.Fatal(err)
	}
	options.StopOnAssistantMessage = false
	failed, err := agent.Resume(context.Background(), process.PID, before.CheckpointID, nil, options)
	if err == nil {
		t.Fatal("the first attempt should fail after its update")
	}
	agent.Backend = &funcBackend{fn: func(call int, _ context.Context, _ runtime.GenerateRequest) (runtime.GenerateResult, error) {
		if call == 1 {
			return update, nil
		}
		return exitResult("done"), nil
	}}
	if _, err := agent.Recover(context.Background(), process.PID, before.CheckpointID, nil, options); err != nil {
		t.Fatalf("recover (first attempt ended %+v): %v", failed.ExitStatus, err)
	}
	if len(adopted) != 2 || !strings.HasPrefix(adopted[0], "false ") || adopted[1] != "true "+strings.TrimPrefix(adopted[0], "false ") {
		t.Fatalf("the replay should adopt the first version: %v", adopted)
	}
	records, _ := store.List()
	versions := 0
	for _, record := range records {
		if record.Type == "working_memory" {
			versions++
		}
	}
	if versions != 1 {
		t.Fatalf("the replay wrote a second version: %d", versions)
	}
}

// Working memory is part of the session: a recovered process still has it.
func TestWorkingMemorySurvivesRecover(t *testing.T) {
	k, store, process := newMemoryFixture(t)
	agent := &runtime.AgentRuntime{Kernel: k, Artifacts: store, Tools: tools.NewRuntime(), MaxSteps: 20}
	agent.Backend = &funcBackend{fn: func(call int, _ context.Context, _ runtime.GenerateRequest) (runtime.GenerateResult, error) {
		if call == 1 {
			return setMemory("decisions", "leaf2", "rejected: biased build", ""), nil
		}
		return runtime.GenerateResult{}, errors.New("connection reset")
	}}
	failed, err := agent.Run(context.Background(), process.PID, []runtime.Message{{Role: "user", Content: "goal"}})
	if err == nil {
		t.Fatal("expected the run to fail")
	}
	var found string
	agent.Backend = &funcBackend{fn: func(call int, _ context.Context, request runtime.GenerateRequest) (runtime.GenerateResult, error) {
		if call == 1 {
			return toolAction("search_memory", map[string]string{"query": "leaf2"}), nil
		}
		found = lastToolMessage(request.History)
		return exitResult("done"), nil
	}}
	if _, err := agent.Recover(context.Background(), process.PID, failed.ExitStatus.Checkpoint, nil, runtime.RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(found, "working memory v1 (current) decisions/leaf2: rejected: biased build") {
		t.Fatalf("the recovered process should still have its working memory:\n%s", found)
	}
}

// handoffAgent writes 1000-character messages until a notice arrives, then
// takes onNotice as its handoff step; it records every request.
func handoffAgent(t *testing.T, onNotice runtime.GenerateResult, after func(call int, request runtime.GenerateRequest) runtime.GenerateResult) (*funcBackend, *[]runtime.GenerateRequest) {
	t.Helper()
	var requests []runtime.GenerateRequest
	noticed := 0
	backend := &funcBackend{fn: func(call int, _ context.Context, request runtime.GenerateRequest) (runtime.GenerateResult, error) {
		requests = append(requests, request)
		last := request.History[len(request.History)-1]
		switch {
		case noticed == 0 && last.Role == "notice":
			noticed = call
			return onNotice, nil
		case noticed > 0:
			return after(call, request), nil
		}
		return markedMessage(call, 1000), nil
	}}
	return backend, &requests
}

// Crossing the trigger appends one labelled notice and compacts at the next
// model call: the handoff step sees its whole history, and the snapshot
// after compaction holds what it wrote.
func TestCompactionGivesOneHandoffStep(t *testing.T) {
	k, store, process := newMemoryFixture(t)
	backend, requests := handoffAgent(t, setMemory("decisions", "plan", "keep TG4", ""),
		func(int, runtime.GenerateRequest) runtime.GenerateResult { return exitResult("done") })
	agent := &runtime.AgentRuntime{Kernel: k, Artifacts: store, Tools: tools.NewRuntime(), Backend: backend, MaxSteps: 40,
		Compaction: runtime.CompactionPolicy{MaxChars: 12000, Handoff: true}}
	if _, err := agent.Run(context.Background(), process.PID, []runtime.Message{{Role: "user", Content: "goal"}}); err != nil {
		t.Fatal(err)
	}
	reqs := *requests
	n := len(reqs)
	notice, after := reqs[n-2], reqs[n-1]
	text := notice.History[len(notice.History)-1].Content
	if !strings.HasPrefix(text, "runtime notice: History through message ") || len(notice.MemoryRefs) != 0 {
		t.Fatalf("the handoff step should see a notice and its whole history: refs %d, %q", len(notice.MemoryRefs), text)
	}
	if len(after.MemoryRefs) != 1 || after.WorkingMemory == nil {
		t.Fatalf("compaction should follow the handoff step: refs %d, snapshot %v", len(after.MemoryRefs), after.WorkingMemory)
	}
	if entry, ok := after.WorkingMemory.Entry("decisions", "plan"); !ok || entry.Content != "keep TG4" {
		t.Fatalf("the snapshot should hold the handoff's update: %+v", after.WorkingMemory)
	}
	events := compactionEvents(t, k)
	if len(events) != 1 || events[0].Data["handoff_updates"] != "1" || events[0].Data["handoff_through"] == "" {
		t.Fatalf("the compaction should record its handoff: %+v", events)
	}
	if !strings.Contains(text, "message "+events[0].Data["handoff_through"]+" ") {
		t.Fatalf("the notice should name the last message it archives (%s): %q", events[0].Data["handoff_through"], text)
	}
}

// After a handoff step that delegates, compaction happens at the next model
// call; after one that waits, when the process resumes, with the notice
// recorded as ignored; after an exit, never.
func TestHandoffStepThatDelegatesWaitsOrExits(t *testing.T) {
	policy := runtime.CompactionPolicy{MaxChars: 12000, Handoff: true}
	segments := func(store *artifact.Store) int {
		records, _ := store.List()
		n := 0
		for _, record := range records {
			if record.Type == "memory_segment" {
				n++
			}
		}
		return n
	}

	t.Run("exit", func(t *testing.T) {
		k, store, process := newMemoryFixture(t)
		backend, _ := handoffAgent(t, exitResult("done"), nil)
		agent := &runtime.AgentRuntime{Kernel: k, Artifacts: store, Tools: tools.NewRuntime(), Backend: backend, MaxSteps: 40, Compaction: policy}
		result, err := agent.Run(context.Background(), process.PID, []runtime.Message{{Role: "user", Content: "goal"}})
		if err != nil {
			t.Fatal(err)
		}
		if segments(store) != 0 {
			t.Fatal("an exit after the notice should not compact")
		}
		final := checkpointMessages(t, store, result.CheckpointID)
		if final.Messages[0].Content != "goal" {
			t.Fatal("the final checkpoint should keep the whole history")
		}
	})

	t.Run("wait", func(t *testing.T) {
		k, store, process := newMemoryFixture(t)
		backend, requests := handoffAgent(t, runtime.GenerateResult{Action: runtime.GenerateAction{Type: "wait", Message: "need input"}},
			func(int, runtime.GenerateRequest) runtime.GenerateResult { return exitResult("done") })
		agent := &runtime.AgentRuntime{Kernel: k, Artifacts: store, Tools: tools.NewRuntime(), Backend: backend, MaxSteps: 40, Compaction: policy}
		blocked, err := agent.Run(context.Background(), process.PID, []runtime.Message{{Role: "user", Content: "goal"}})
		if !errors.Is(err, runtime.ErrAgentBlocked) || segments(store) != 0 {
			t.Fatalf("waiting should not compact yet: %v, %d segments", err, segments(store))
		}
		if _, err := agent.Resume(context.Background(), process.PID, blocked.CheckpointID, []runtime.Message{{Role: "user", Content: "go on"}}, runtime.RunOptions{}); err != nil {
			t.Fatal(err)
		}
		reqs := *requests
		if len(reqs[len(reqs)-1].MemoryRefs) != 1 {
			t.Fatal("the resumed process should compact before its first call")
		}
		if events := compactionEvents(t, k); len(events) != 1 || events[0].Data["handoff_updates"] != "0" {
			t.Fatalf("an ignored notice should be recorded: %+v", events)
		}
	})

	// A notice is a promise: a process resumed under a larger window still
	// compacts what the notice said would move.
	t.Run("wait, resumed under a larger window", func(t *testing.T) {
		k, store, process := newMemoryFixture(t)
		backend, requests := handoffAgent(t, runtime.GenerateResult{Action: runtime.GenerateAction{Type: "wait", Message: "need input"}},
			func(int, runtime.GenerateRequest) runtime.GenerateResult { return exitResult("done") })
		agent := &runtime.AgentRuntime{Kernel: k, Artifacts: store, Tools: tools.NewRuntime(), Backend: backend, MaxSteps: 40, Compaction: policy}
		blocked, err := agent.Run(context.Background(), process.PID, []runtime.Message{{Role: "user", Content: "goal"}})
		if !errors.Is(err, runtime.ErrAgentBlocked) {
			t.Fatal(err)
		}
		agent.Compaction = runtime.CompactionPolicy{MaxChars: 20000, Handoff: true}
		if _, err := agent.Resume(context.Background(), process.PID, blocked.CheckpointID, []runtime.Message{{Role: "user", Content: "go on"}}, runtime.RunOptions{}); err != nil {
			t.Fatal(err)
		}
		reqs := *requests
		if len(reqs[len(reqs)-1].MemoryRefs) != 1 {
			t.Fatal("the pending compaction should happen though history is under the new trigger")
		}
	})

	t.Run("delegate", func(t *testing.T) {
		k, store, _ := newMemoryFixture(t)
		process, err := k.Spawn(kernel.ProcessSpec{Role: "agent:lead", CapabilitySet: capability.Set{Tools: []string{"spawn_agent"}}})
		if err != nil {
			t.Fatal(err)
		}
		backend, requests := handoffAgent(t, runtime.GenerateResult{Action: runtime.GenerateAction{Type: "delegate", Delegate: &runtime.DelegateCall{AgentID: "writer", Goal: "draft"}}},
			func(int, runtime.GenerateRequest) runtime.GenerateResult { return exitResult("done") })
		agent := &runtime.AgentRuntime{Kernel: k, Artifacts: store, Tools: tools.NewRuntime(), Backend: backend, MaxSteps: 40, Compaction: policy,
			DelegateHandler: func(context.Context, kernel.Process, runtime.DelegateCall, runtime.Session) (runtime.DelegateResult, error) {
				return runtime.DelegateResult{AgentID: "writer", Summary: "drafted"}, nil
			}}
		if _, err := agent.Run(context.Background(), process.PID, []runtime.Message{{Role: "user", Content: "goal"}}); err != nil {
			t.Fatal(err)
		}
		reqs := *requests
		if len(reqs[len(reqs)-1].MemoryRefs) != 1 || segments(store) != 1 {
			t.Fatal("the call after a delegating handoff step should come after compaction")
		}
	})
}

// The header's snapshot changes only at compaction: updates between
// compactions are in the history, and the prompt prefix stays the same.
func TestPromptPrefixStableBetweenCompactions(t *testing.T) {
	k, store, process := newMemoryFixture(t)
	var snapshots []string
	backend := &funcBackend{fn: func(call int, _ context.Context, request runtime.GenerateRequest) (runtime.GenerateResult, error) {
		snapshot := ""
		if request.WorkingMemory != nil {
			snapshot = request.WorkingMemory.ArtifactID
		}
		snapshots = append(snapshots, snapshot)
		switch {
		case len(request.MemoryRefs) >= 2:
			return exitResult("done"), nil
		case call%5 == 0:
			return setMemory("measurements", "step", "call "+string(rune('0'+call%10)), ""), nil
		}
		return markedMessage(call, 1000), nil
	}}
	agent := &runtime.AgentRuntime{Kernel: k, Artifacts: store, Tools: tools.NewRuntime(), Backend: backend, MaxSteps: 80,
		Compaction: runtime.CompactionPolicy{MaxChars: 12000}}
	if _, err := agent.Run(context.Background(), process.PID, []runtime.Message{{Role: "user", Content: "goal"}}); err != nil {
		t.Fatal(err)
	}
	changes := 0
	for i := 1; i < len(snapshots); i++ {
		if snapshots[i] != snapshots[i-1] {
			changes++
		}
	}
	events := compactionEvents(t, k)
	if changes == 0 || changes > len(events) {
		t.Fatalf("the snapshot should change only at compaction: %d changes, %d compactions, %v", changes, len(events), snapshots)
	}
	if snapshots[0] != "" {
		t.Fatal("before any compaction there is no snapshot; updates are in the history")
	}
}

// Search lists the current entry first and marks an earlier claim under the
// same key as superseded, with what replaced it.
func TestSearchMarksSupersededEntries(t *testing.T) {
	k, store, process := newMemoryFixture(t)
	agent := &runtime.AgentRuntime{Kernel: k, Artifacts: store, Tools: tools.NewRuntime(), MaxSteps: 20}
	results, errs := memoryRun(t, agent, process.PID,
		setMemory("decisions", "leaf2", "adopt leaf2: 27% faster", ""),
		setMemory("decisions", "leaf2", "rejected: leaf2 build dropped half the triangles", "1"),
		setMemory("open", "leaf2 retest", "retest leaf2 on wide nodes", ""),
		toolAction("update_memory", map[string]string{"section": "open", "key": "leaf2 retest", "delete": "true"}),
		toolAction("search_memory", map[string]string{"query": "leaf2"}),
	)
	if len(errs) != 0 || len(results) != 5 {
		t.Fatalf("results %d, errors %v", len(results), errs)
	}
	hits := results[4].Output["hits"]
	for _, want := range []string{
		"Working memory v4 (",
		"working memory v4 (current) decisions/leaf2: rejected: leaf2 build dropped half the triangles [evidence 1]",
		"working memory v1 decisions/leaf2: adopt leaf2: 27% faster (superseded in v2; now: rejected: leaf2 build dropped half the triangles)",
		"working memory v3 open/leaf2 retest: retest leaf2 on wide nodes (superseded in v4; deleted since)",
	} {
		if !strings.Contains(hits, want) {
			t.Fatalf("search lacks %q:\n%s", want, hits)
		}
	}
	if n := strings.Count(hits, "(superseded in"); n != 2 {
		t.Fatalf("only the two replaced claims are superseded, got %d:\n%s", n, hits)
	}
	if strings.Index(hits, "(current)") > strings.Index(hits, "superseded") {
		t.Fatalf("the current entry should come first:\n%s", hits)
	}
}
