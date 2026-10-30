package memprobe_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aos/internal/agentflow"
	"aos/internal/artifact"
	"aos/internal/capability"
	"aos/internal/kernel"
	"aos/internal/memprobe"
	"aos/internal/runtime"
)

// memoryCompleter reads the one segment it is pointed at, then answers
// with whatever its answer function makes of the prompt.
type memoryCompleter struct {
	segment string
	answer  func(userPrompt string) string
	systems []string
}

func (c *memoryCompleter) CompleteJSON(_ context.Context, system, user string, target any) error {
	c.systems = append(c.systems, system)
	action := map[string]any{"type": "tool", "tool_call": map[string]any{"name": "read_memory", "args": map[string]string{"artifact_id": c.segment}}}
	if strings.Contains(user, "read_memory:map[") {
		action = map[string]any{"type": "message", "message": c.answer(user)}
	}
	encoded, _ := json.Marshal(map[string]any{"action": action})
	return json.Unmarshal(encoded, target)
}

// probedRun is a state directory left by a writer whose round-3 sweep
// result was compacted away.
func probedRun(t *testing.T) (stateDir, checkpoint, segment string) {
	t.Helper()
	stateDir = t.TempDir()
	k, err := kernel.New(filepath.Join(stateDir, "kernel"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := artifact.New(filepath.Join(stateDir, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	writer, err := k.Spawn(kernel.ProcessSpec{Role: "agent:writer", Metadata: map[string]string{"agent_id": "writer", "goal": "make it fast"},
		CapabilitySet: capability.Set{Tools: []string{"run", "write_file"}}})
	if err != nil {
		t.Fatal(err)
	}
	seg, err := store.CreateJSON("memory_segment", writer.PID, map[string]any{
		"process_id": writer.PID, "sequence": 1, "message_count": 1,
		"messages": []runtime.Message{{Role: "assistant", Content: "TG4 stays default (TG4 45.3 vs TG2 47.0); leaf2 rejected as biased", Seq: 1}},
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	cp, err := store.CreateJSON("checkpoint", writer.PID, runtime.Session{
		Messages:   []runtime.Message{{Role: "user", Content: "keep optimizing", Seq: 2}},
		MemoryRefs: []runtime.MemoryRef{{ArtifactID: seg.ID, MessageCount: 1}},
		NextSeq:    2,
	}, []string{seg.ID}, map[string]string{"kind": "intermediate"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k.Exit(writer.PID, kernel.ExitStatus{Code: 0, Reason: "done", Checkpoint: cp.ID}); err != nil {
		t.Fatal(err)
	}
	return stateDir, cp.ID, seg.ID
}

func probeWorkflow() agentflow.WorkflowSpec {
	return agentflow.WorkflowSpec{Name: "probe-test", EntryAgent: "writer", Agents: []agentflow.AgentSpec{{
		ID: "writer", SystemPrompt: "Optimize the renderer.", Capabilities: capability.Set{Tools: []string{"run", "write_file"}},
	}}}
}

// Probes run on a copy: the agent reads its compacted memory to answer,
// answers are scored, and the original state directory is unchanged.
func TestProbesScoreAnswersFromMemoryOnACopy(t *testing.T) {
	stateDir, checkpoint, segment := probedRun(t)
	before := map[string][]byte{}
	for _, rel := range []string{"artifacts/index.json", "kernel/processes.json", "kernel/events.json"} {
		before[rel], _ = os.ReadFile(filepath.Join(stateDir, rel))
	}
	completer := &memoryCompleter{segment: segment, answer: func(user string) string {
		switch {
		case strings.Contains(user, "Is leaf2 adopted"):
			return "Yes, leaf2 is adopted."
		case strings.Contains(user, "Was TriData reordered"):
			return "My tools were rejected, so I will retry the benchmark next turn."
		}
		return "TG4, from my compacted memory."
	}}
	probes := []memprobe.Probe{
		{ID: "tg", Kind: "archive", Checkpoint: checkpoint, Question: "Which threadgroup size did you keep?", Expect: `(?i)\bTG ?4\b`},
		{ID: "leaf2", Kind: "superseded", Checkpoint: checkpoint, Question: "Is leaf2 adopted?", Expect: `(?i)reject|biased|not adopted`, Reject: `(?i)\bis adopted\b`},
		{ID: "tridata", Kind: "archive", Checkpoint: checkpoint, Question: "Was TriData reordered?", Topic: `(?i)tri ?data`, Expect: `(?i)reject|no win`},
	}
	samples, err := memprobe.Run(context.Background(), memprobe.Config{
		StateDir: stateDir, WorkDir: t.TempDir(), Workflow: probeWorkflow(), Completer: completer, Samples: 2,
	}, probes)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 6 {
		t.Fatalf("expected 2 samples of 3 probes, got %+v", samples)
	}
	for _, sample := range samples {
		if sample.Error != "" || sample.Steps != 2 || sample.MemoryReads != 1 || sample.BytesRead == 0 {
			t.Fatalf("each sample should read memory once and answer: %+v", sample)
		}
		switch sample.Probe {
		case "tg":
			if !sample.Correct || sample.Superseded {
				t.Fatalf("tg answer should score correct: %+v", sample)
			}
		case "leaf2":
			if sample.Correct || !sample.Superseded {
				t.Fatalf("leaf2 answer states the superseded fact: %+v", sample)
			}
		case "tridata":
			if sample.Correct || !sample.OffTopic {
				t.Fatalf("a reply about something else must not score on a stray %q: %+v", "rejected", sample)
			}
		}
	}
	for _, system := range completer.systems {
		if strings.Contains(system, "- run") || strings.Contains(system, "- write_file") {
			t.Fatalf("a probe must not offer the agent's working tools:\n%s", system)
		}
	}
	for rel, want := range before {
		got, _ := os.ReadFile(filepath.Join(stateDir, rel))
		if !bytes.Equal(got, want) {
			t.Fatalf("probing changed the original %s", rel)
		}
	}

	summaries := memprobe.Summarize(samples)
	if len(summaries) != 3 || summaries[0].Correct != 2 || summaries[1].Superseded != 2 || summaries[2].OffTopic != 2 || summaries[0].MeanReads != 1 {
		t.Fatalf("summaries = %+v", summaries)
	}
	var table bytes.Buffer
	memprobe.WriteTable(&table, summaries)
	if !strings.Contains(table.String(), "tg") || !strings.Contains(table.String(), "2/2") {
		t.Fatalf("table:\n%s", table.String())
	}
}

// A probe must name a checkpoint, and its patterns must compile.
func TestLoadProbesChecksProbes(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) string {
		path := filepath.Join(dir, "probes.json")
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	if _, err := memprobe.LoadProbes(write(`[{"id":"a","question":"q","expect":"x"}]`)); err == nil {
		t.Fatal("a probe without a checkpoint should be refused")
	}
	if _, err := memprobe.LoadProbes(write(`[{"id":"a","checkpoint":"art-1","question":"q","expect":"("}]`)); err == nil {
		t.Fatal("a bad pattern should be refused")
	}
	probes, err := memprobe.LoadProbes(write(`[{"id":"a","kind":"live","checkpoint":"art-1","question":"q","expect":"x"}]`))
	if err != nil || len(probes) != 1 {
		t.Fatalf("probes = %+v, %v", probes, err)
	}
}

// Rescoring applies tightened patterns to recorded answers: a regex that
// matched a stray word no longer counts.
func TestRescoreAppliesTightenedPatterns(t *testing.T) {
	samples := []memprobe.Sample{
		{Probe: "leaf2", Answer: "leaf2 did not reproduce; the build has bary dropped", Correct: true},
		{Probe: "leaf2", Answer: "leaf2 was biased: it dropped half the triangles", Correct: true},
		{Probe: "other", Answer: "anything", Correct: true},
	}
	path := filepath.Join(t.TempDir(), "samples.jsonl")
	var lines bytes.Buffer
	encoder := json.NewEncoder(&lines)
	for _, sample := range samples {
		_ = encoder.Encode(sample)
	}
	if err := os.WriteFile(path, lines.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	read, err := memprobe.ReadSamples(path)
	if err != nil || len(read) != 3 {
		t.Fatalf("read %d samples: %v", len(read), err)
	}
	rescored := memprobe.Rescore(read, []memprobe.Probe{{ID: "leaf2", Topic: "leaf2", Expect: `(?i)biased|dropped half`}})
	if rescored[0].Correct || !rescored[1].Correct || !rescored[2].Correct {
		t.Fatalf("rescored = %+v", rescored)
	}
}

// promptCompleter answers at once and keeps every prompt it was shown.
type promptCompleter struct{ prompts []string }

func (c *promptCompleter) CompleteJSON(_ context.Context, system, user string, target any) error {
	c.prompts = append(c.prompts, system+"\n"+user)
	encoded, _ := json.Marshal(map[string]any{"action": map[string]any{"type": "message", "message": "leaf2 was rejected: biased"}})
	return json.Unmarshal(encoded, target)
}

// A probe's working memory is shown to the agent unless the condition
// leaves it out, the check rule follows its switch, and each sample says
// which condition it ran under. The original state is not touched.
func TestProbeWorkingMemoryConditions(t *testing.T) {
	stateDir, checkpoint, _ := probedRun(t)
	before, _ := os.ReadFile(filepath.Join(stateDir, "artifacts/index.json"))
	probe := memprobe.Probe{ID: "leaf2", Kind: "decision", Checkpoint: checkpoint, Question: "Run leaf2 again?", Expect: "rejected",
		WorkingMemory: []memprobe.ProbeMemoryEntry{{Section: "rejected", Key: "leaf2", Content: "biased: dropped half the triangles", Evidence: "1"}}}
	const entry = "- leaf2: biased: dropped half the triangles [evidence 1]"
	const rule = "Before you propose, adopt or reject an approach"
	for _, c := range []struct {
		config    memprobe.Config
		condition string
		memory    bool
		rule      bool
	}{
		{memprobe.Config{}, "memory,check-rule", true, true},
		{memprobe.Config{NoCheckRule: true}, "memory,no-check-rule", true, false},
		{memprobe.Config{NoWorkingMemory: true}, "no-memory,check-rule", false, true},
	} {
		completer := &promptCompleter{}
		c.config.StateDir, c.config.WorkDir, c.config.Workflow, c.config.Completer = stateDir, t.TempDir(), probeWorkflow(), completer
		samples, err := memprobe.Run(context.Background(), c.config, []memprobe.Probe{probe})
		if err != nil || len(samples) != 1 {
			t.Fatalf("%s: %v %+v", c.condition, err, samples)
		}
		prompt := completer.prompts[0]
		if samples[0].Condition != c.condition || strings.Contains(prompt, entry) != c.memory || strings.Contains(prompt, rule) != c.rule {
			t.Fatalf("%s: condition %q, memory shown %v, rule shown %v", c.condition, samples[0].Condition, strings.Contains(prompt, entry), strings.Contains(prompt, rule))
		}
	}
	after, _ := os.ReadFile(filepath.Join(stateDir, "artifacts/index.json"))
	if !bytes.Equal(before, after) {
		t.Fatal("giving working memory changed the original state")
	}
}

// searchCompleter searches working memory once, then answers, keeping
// every prompt it was shown.
type searchCompleter struct{ prompts []string }

func (c *searchCompleter) CompleteJSON(_ context.Context, system, user string, target any) error {
	c.prompts = append(c.prompts, user)
	action := map[string]any{"type": "tool", "tool_call": map[string]any{"name": "search_memory", "args": map[string]string{"query": "leaf2"}}}
	if len(c.prompts) > 1 {
		action = map[string]any{"type": "message", "message": "leaf2 was rejected: biased"}
	}
	encoded, _ := json.Marshal(map[string]any{"action": action})
	return json.Unmarshal(encoded, target)
}

// A checkpoint that holds the agent's own working memory keeps it when
// the probe gives none ("own-memory"); --no-working-memory strips it from
// the header and from search_memory, and also when the probe gives one.
func TestProbeOwnWorkingMemoryConditions(t *testing.T) {
	stateDir, checkpoint, _ := probedRun(t)
	store, err := artifact.New(filepath.Join(stateDir, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.Metadata(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	own, err := store.CreateJSON("working_memory", record.ProducerPID, runtime.WorkingMemory{ProcessID: record.ProducerPID, Version: 2,
		Sections: []runtime.MemorySection{{Name: "rejected", Entries: []runtime.MemoryEntry{{Key: "leaf2", Content: "own note: leaf2 biased", Evidence: "1"}}}},
		Update:   runtime.MemoryUpdate{Section: "rejected"}}, nil, map[string]string{"version": "2"})
	if err != nil {
		t.Fatal(err)
	}
	var session runtime.Session
	if err := store.ReadJSON(checkpoint, &session); err != nil {
		t.Fatal(err)
	}
	session.WorkingMemory, session.MemorySnapshot = own.ID, own.ID
	withMemory, err := store.CreateJSON("checkpoint", record.ProducerPID, session, []string{checkpoint, own.ID}, map[string]string{"kind": "intermediate"})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(stateDir, "artifacts/index.json"))

	const ownEntry, injected = "own note: leaf2 biased", "injected note: leaf2 dropped half the triangles"
	plain := memprobe.Probe{ID: "own", Kind: "own-memory", Checkpoint: withMemory.ID, Question: "Run leaf2 again?", Expect: "rejected"}
	given := plain
	given.ID, given.WorkingMemory = "given", []memprobe.ProbeMemoryEntry{{Section: "rejected", Key: "leaf2", Content: injected, Evidence: "1"}}
	for _, c := range []struct {
		probe     memprobe.Probe
		config    memprobe.Config
		condition string
		shown     string // the entry the header and search show, if any
	}{
		{plain, memprobe.Config{}, "own-memory,check-rule", ownEntry},
		{plain, memprobe.Config{NoCheckRule: true}, "own-memory,no-check-rule", ownEntry},
		{plain, memprobe.Config{NoWorkingMemory: true}, "no-memory,check-rule", ""},
		{given, memprobe.Config{}, "memory,check-rule", injected},
		{given, memprobe.Config{NoWorkingMemory: true}, "no-memory,check-rule", ""},
	} {
		completer := &searchCompleter{}
		c.config.StateDir, c.config.WorkDir, c.config.Workflow, c.config.Completer = stateDir, t.TempDir(), probeWorkflow(), completer
		samples, err := memprobe.Run(context.Background(), c.config, []memprobe.Probe{c.probe})
		if err != nil || len(samples) != 1 || len(completer.prompts) != 2 {
			t.Fatalf("%s %s: %v %+v, %d prompts", c.probe.ID, c.condition, err, samples, len(completer.prompts))
		}
		sample := samples[0]
		if sample.Condition != c.condition || (sample.WorkingMemory == "") != (c.shown == "") {
			t.Fatalf("%s: condition %q, working memory %q, want %q shown %q", c.probe.ID, sample.Condition, sample.WorkingMemory, c.condition, c.shown)
		}
		for i, prompt := range completer.prompts {
			for _, entry := range []string{ownEntry, injected} {
				if got := strings.Contains(prompt, entry); got != (entry == c.shown) {
					t.Fatalf("%s %s prompt %d: %q shown %v", c.probe.ID, c.condition, i+1, entry, got)
				}
				// After the search, the entry is in the header and in its hits.
				if i == 1 && entry == c.shown && strings.Count(prompt, entry) < 2 {
					t.Fatalf("%s %s: search_memory did not find %q:\n%s", c.probe.ID, c.condition, entry, prompt)
				}
			}
		}
		if c.probe.ID == "own" && c.shown != "" && sample.WorkingMemory != own.ID {
			t.Fatalf("own memory should be the checkpoint's version %s, got %s", own.ID, sample.WorkingMemory)
		}
	}
	after, _ := os.ReadFile(filepath.Join(stateDir, "artifacts/index.json"))
	if !bytes.Equal(before, after) {
		t.Fatal("stripping working memory changed the original state")
	}
}

// The probed agent is not shown the probe's ID, which can hint at the
// answer.
func TestProbeIDIsNotShown(t *testing.T) {
	stateDir, checkpoint, _ := probedRun(t)
	completer := &promptCompleter{}
	probe := memprobe.Probe{ID: "xyzzy-readopted", Kind: "superseded", Checkpoint: checkpoint, Question: "Is leaf2 adopted?", Expect: "rejected"}
	samples, err := memprobe.Run(context.Background(), memprobe.Config{StateDir: stateDir, WorkDir: t.TempDir(), Workflow: probeWorkflow(), Completer: completer}, []memprobe.Probe{probe})
	if err != nil || len(samples) != 1 || len(completer.prompts) == 0 {
		t.Fatalf("%v %+v", err, samples)
	}
	for _, prompt := range completer.prompts {
		if strings.Contains(prompt, "xyzzy") {
			t.Fatalf("the prompt names the probe:\n%s", prompt)
		}
	}
}
