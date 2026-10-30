package memprobe_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"aos/internal/agentflow"
	"aos/internal/memprobe"
	"aos/internal/runtime"
	"aos/internal/skills"
)

// rangeCompleter reads one message range, then answers with the prompt it
// was shown, so a test can check what the probed agent would see.
type rangeCompleter struct {
	from, to string
	prompts  []string
}

func (c *rangeCompleter) CompleteJSON(_ context.Context, _, user string, target any) error {
	c.prompts = append(c.prompts, user)
	action := map[string]any{"type": "tool", "tool_call": map[string]any{"name": "read_memory", "args": map[string]string{"from": c.from, "to": c.to}}}
	if len(c.prompts) > 1 {
		action = map[string]any{"type": "message", "message": "done"}
	}
	encoded, _ := json.Marshal(map[string]any{"action": action})
	return json.Unmarshal(encoded, target)
}

// TestEvalProbesPointWhereTheySay checks the frozen phase-3 evaluation
// probes against the memfix run itself: the message numbers their notes
// cite hold what the notes say, the working-memory-only answer appears
// nowhere in the agent's history, and the working memory reaches the
// prompt, under the run's own policy (65,536-token window, ratio 0.8).
// It needs the run's state, so it runs only when AOS_MEMFIX_ROOT
// names the directory the run started in (payload paths are relative to
// it) and AOS_MEMFIX_WORKFLOW names the workflow the run used.
func TestEvalProbesPointWhereTheySay(t *testing.T) {
	root, workflowPath := os.Getenv("AOS_MEMFIX_ROOT"), os.Getenv("AOS_MEMFIX_WORKFLOW")
	if root == "" || workflowPath == "" {
		t.Skip("set AOS_MEMFIX_ROOT and AOS_MEMFIX_WORKFLOW to check the probes against the memfix run")
	}
	probesPath, err := filepath.Abs("../../bench/memory/pathtracer-bunny-memfix-phase3-eval-probes.json")
	if err != nil {
		t.Fatal(err)
	}
	probes, err := memprobe.LoadProbes(probesPath)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]memprobe.Probe{}
	for _, probe := range probes {
		byID[probe.ID] = probe
	}
	specBytes, err := os.ReadFile(workflowPath)
	if err != nil {
		t.Fatal(err)
	}
	var spec agentflow.WorkflowSpec
	if err := json.Unmarshal(specBytes, &spec); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)

	cases := []struct {
		probe, from, to string
		// read must match the range read back; live must or must not
		// match the history the agent is resumed with.
		read    string
		live    string
		liveHas bool
	}{
		{probe: "narrow-tree-wm", from: "407", to: "407", read: `#407 tool:[\s\S]*Explains why leaf2 helped\s+the old 32-byte narrow tree`, live: `narrow tree`, liveHas: false},
		{probe: "narrow-tree-own", from: "408", to: "408", read: `#408 assistant:[\s\S]*14% SLOWER`, live: `narrow tree`, liveHas: true},
		{probe: "narrow-tree-own", from: "232", to: "248", read: `#232 assistant:[\s\S]*0\.7946[\s\S]*#236 assistant:[\s\S]*DROPPED`, live: `narrow tree`, liveHas: true},
		{probe: "wm-only-next", from: "1", to: "288", read: `#1 `, live: `(?i)quantiz|qnode|16-bit|half bounds`, liveHas: false},
	}
	for _, c := range cases {
		t.Run(c.probe+"/"+c.from, func(t *testing.T) {
			probe := byID[c.probe]
			completer := &rangeCompleter{from: c.from, to: c.to}
			samples, err := memprobe.Run(context.Background(), memprobe.Config{
				StateDir: filepath.Join(root, "reports/pathtracer-bunny-memfix/.aos-state"), WorkDir: t.TempDir(),
				Workflow: spec, Skills: skills.NewRegistry([]string{filepath.Join(root, "skills")}), Completer: completer, Samples: 1, MaxSteps: 3,
				Compaction: runtime.ContextCompactionPolicy(65536, 0.8),
			}, []memprobe.Probe{probe})
			if err != nil {
				t.Fatal(err)
			}
			if len(completer.prompts) < 2 {
				t.Fatalf("expected a read and an answer, got %d prompts; sample %+v", len(completer.prompts), samples)
			}
			first, second := promptContext(t, completer.prompts[0]), promptContext(t, completer.prompts[1])
			for _, entry := range probe.WorkingMemory {
				if !strings.Contains(first.Memory, entry.Content) {
					t.Errorf("working memory entry %q is not in the prompt", entry.Key)
				}
			}
			var live strings.Builder
			for _, message := range first.History {
				if !strings.Contains(message.Content, "Question from the operator") {
					live.WriteString(message.Content + "\n")
				}
			}
			if got := regexp.MustCompile(c.live).MatchString(live.String()); got != c.liveHas {
				t.Errorf("live history matches %q: %v, want %v", c.live, got, c.liveHas)
			}
			read := second.History[len(second.History)-1].Content
			if !regexp.MustCompile(c.read).MatchString(read) {
				t.Errorf("read_memory from=%s to=%s does not match %q:\n%.2000s", c.from, c.to, c.read, read)
			}
			if c.probe == "wm-only-next" {
				all := readWholeArchive(t, root, spec, probe)
				if regexp.MustCompile(c.live).MatchString(all) {
					t.Errorf("the working-memory-only answer is in the archive")
				}
			}
		})
	}
}

// shownContext is the part of a user prompt these checks read.
type shownContext struct {
	History []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"history"`
	Memory string `json:"working_memory"`
}

func promptContext(t *testing.T, prompt string) shownContext {
	t.Helper()
	start, end := strings.Index(prompt, "{"), strings.LastIndex(prompt, "}")
	var shown shownContext
	if start < 0 || end < start {
		t.Fatalf("no context in prompt %.200s", prompt)
	}
	if err := json.Unmarshal([]byte(prompt[start:end+1]), &shown); err != nil {
		t.Fatal(err)
	}
	return shown
}

// readWholeArchive pages through the probe agent's whole archive.
func readWholeArchive(t *testing.T, root string, spec agentflow.WorkflowSpec, probe memprobe.Probe) string {
	t.Helper()
	var text strings.Builder
	for from := 1; from <= 288; {
		completer := &rangeCompleter{from: fmt.Sprint(from), to: "288"}
		if _, err := memprobe.Run(context.Background(), memprobe.Config{
			StateDir: filepath.Join(root, "reports/pathtracer-bunny-memfix/.aos-state"), WorkDir: t.TempDir(),
			Workflow: spec, Skills: skills.NewRegistry([]string{filepath.Join(root, "skills")}), Completer: completer, Samples: 1, MaxSteps: 3, NoWorkingMemory: true,
			Compaction: runtime.ContextCompactionPolicy(65536, 0.8),
		}, []memprobe.Probe{probe}); err != nil {
			t.Fatal(err)
		}
		shown := promptContext(t, completer.prompts[1])
		result := shown.History[len(shown.History)-1].Content
		text.WriteString(result)
		next := regexp.MustCompile(`next_from[:=](\d+)`).FindStringSubmatch(result)
		if next == nil {
			break
		}
		fmt.Sscan(next[1], &from)
	}
	return text.String()
}

// TestOwnMemoryProbesLoad checks that the frozen own-memory probes parse
// and their patterns compile as Go regexps. Their pointers into the
// phase-3 live run are checked by
// reports/memory-probe-own-memory/check_probes.py.
func TestOwnMemoryProbesLoad(t *testing.T) {
	probes, err := memprobe.LoadProbes("../../bench/memory/pathtracer-bunny-phase3-own-memory-probes.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(probes) != 11 {
		t.Fatalf("got %d probes, want 11", len(probes))
	}
	for _, probe := range probes {
		if len(probe.WorkingMemory) > 0 {
			t.Errorf("%s injects working memory; own-memory probes must keep the checkpoint's own", probe.ID)
		}
	}
}
