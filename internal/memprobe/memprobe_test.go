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
		if strings.Contains(user, "leaf2") && strings.Contains(user, "Is leaf2 adopted") {
			return "Yes, leaf2 is adopted."
		}
		return "TG4, from my compacted memory."
	}}
	probes := []memprobe.Probe{
		{ID: "tg", Kind: "archive", Checkpoint: checkpoint, Question: "Which threadgroup size did you keep?", Expect: `(?i)\bTG ?4\b`},
		{ID: "leaf2", Kind: "superseded", Checkpoint: checkpoint, Question: "Is leaf2 adopted?", Expect: `(?i)reject|biased|not adopted`, Reject: `(?i)\bis adopted\b`},
	}
	samples, err := memprobe.Run(context.Background(), memprobe.Config{
		StateDir: stateDir, WorkDir: t.TempDir(), Workflow: probeWorkflow(), Completer: completer, Samples: 2,
	}, probes)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 4 {
		t.Fatalf("expected 2 samples of 2 probes, got %+v", samples)
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
	if len(summaries) != 2 || summaries[0].Correct != 2 || summaries[1].Superseded != 2 || summaries[0].MeanReads != 1 {
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
