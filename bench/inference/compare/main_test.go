package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aos/internal/inference"
	"aos/internal/runlog"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestCostAndWorstCase(t *testing.T) {
	call := inference.CallRecord{PromptTokens: 1_000_000, CachedTokens: 500_000, CacheCreationTokens: 100_000, CompletionTokens: 100_000}
	// 400k uncached at $2, 100k out at $10, 500k reads at $0.20, 100k writes at $2.50.
	if got := cost(call); !near(got, 0.8+1.0+0.1+0.25) {
		t.Fatalf("cost %v", got)
	}
	if got := worstCase(250_000, 16_000); !near(got, 2.5+0.625+0.16) {
		t.Fatalf("worst case %v", got)
	}
}

// step is one scripted call of a test ledger.
type step struct {
	agent, pid string
	repair     bool
	api, model string
	answered   string // the model that answered, if not model
	reply      string
	err        string
	usage      [4]int // prompt, completion, cached, cache writes
}

func writeTrial(t *testing.T, root, name string, m meta, verdict string, steps []step, output string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Join(dir, "state", "kernel"), 0o755); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(m)
	os.WriteFile(filepath.Join(dir, "meta.json"), raw, 0o644)
	os.WriteFile(filepath.Join(dir, "run.log"), []byte(`{"exit_status":{"code":0},"verification":{"status":"`+verdict+`"}}`+"\nworkflow done\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "output.html"), []byte(output), 0o644)
	os.WriteFile(filepath.Join(dir, "state", "kernel", "events.json"), []byte(`[{"type":"backend.retry"},{"type":"run.started"}]`), 0o644)
	log, err := runlog.Open(runlog.Path(filepath.Join(dir, "state"), runlog.CallsFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		start := inference.CallRecord{Phase: inference.PhaseStart, PID: s.pid, AgentID: s.agent, Repair: s.repair, Model: s.model,
			Params: inference.RequestParams{API: s.api, Model: s.model}}
		seq, _ := log.Append(func(seq int) any { start.Seq = seq; return start })
		answered := s.model
		if s.answered != "" {
			answered = s.answered
		}
		outcome := inference.CallRecord{Phase: inference.PhaseOutcome, Start: seq, PID: s.pid, AgentID: s.agent, Repair: s.repair, Model: answered,
			Response: s.reply, Error: s.err, PromptTokens: s.usage[0], CompletionTokens: s.usage[1], CachedTokens: s.usage[2], CacheCreationTokens: s.usage[3], LatencyMS: 1500}
		if s.api == inference.APIAnthropicMessages {
			outcome.Reasoning = "thought"
		}
		log.Append(func(seq int) any { outcome.Seq = seq; return outcome })
	}
}

const page = `<!doctype html><html><body><p>a &amp; "b"</p></body></html>`

func TestReportReadsEachCallAsTheStepDid(t *testing.T) {
	root := t.TempDir()
	quoted, _ := json.Marshal(page)
	sonnet := func(agent, pid, reply string) step {
		return step{agent: agent, pid: pid, api: inference.APIAnthropicMessages, model: "claude-sonnet-5-5", reply: reply, usage: [4]int{1000, 100, 800, 0}}
	}
	steps := []step{
		sonnet("delivery_lead", "pid-2", `{"action":{"type":"delegate","delegate":{"agent_id":"merger","goal":"merge"}}}`),
		sonnet("merger", "pid-3", `Here it is: {"action":{"type":"message","message":`+string(quoted)+`}}`),
		sonnet("merger", "pid-3", `I will {exit now}`),
		{agent: "merger", pid: "pid-3", repair: true, api: inference.APIAnthropicMessages, model: "claude-sonnet-5-5", reply: `{"action":{"type":"exit","exit_status":{"code":0}}}`},
		sonnet("delivery_lead", "pid-2", `{"action":{"type":"delegate","artifacts":"art-1","delegate":{"agent_id":"file_writer","goal":"write `+strings.ReplaceAll(page, `"`, `\"`)+`"}}}`),
		sonnet("file_writer", "pid-4", `{"action":{"type":"tool","tool_call":{"name":"write_file","args":{"path":"out.html","content":`+string(quoted)+`}}}}`),
		sonnet("file_writer", "pid-4", `{"action":{"type":"exit","exit_status":{"code":0}}} or {"action":{"type":"message","message":"example"}}`),
		{agent: "review_partner", pid: "pid-5", api: inference.APIAnthropicMessages, model: "claude-sonnet-5-5", answered: "claude-sonnet-5", reply: `<think>{"action":{"type":"exit","exit_status":{"code":0}}}</think>Looks good.`},
		{agent: "review_partner", pid: "pid-5", api: inference.APIAnthropicMessages, model: "claude-sonnet-5-5", err: `Post "http://gpu-box.local:8080/v1/messages": dial tcp 10.1.2.3:8080: connect: refused`},
	}
	writeTrial(t, root, "sonnet/good/trial-1", meta{Arm: "sonnet", Set: "good", Trial: 1, Profile: "claude", Model: "claude-sonnet-5-5"}, "pass", steps, page)
	writeTrial(t, root, "strata/good/trial-1", meta{Arm: "strata", Set: "good", Trial: 1, Profile: "strata-phg", Model: "qwen"}, "fail",
		[]step{{agent: "merger", pid: "pid-3", model: "qwen", reply: `{"action":{"type":"message","message":"<html>x</html>"}}`, usage: [4]int{100, 400, 0, 0}},
			{agent: "file_writer", pid: "pid-4", model: "qwen", reply: `{"action":{"type":"tool","tool_call":{"name":"write_file","args":{"content":"&lt;html>x</html>"}}}}`}}, "&lt;html>x</html>")
	htmlOut, jsonOut := filepath.Join(root, "compare.html"), filepath.Join(root, "compare.json")
	if err := report(root, htmlOut, jsonOut); err != nil {
		t.Fatal(err)
	}
	var summary Summary
	raw, _ := os.ReadFile(jsonOut)
	json.Unmarshal(raw, &summary)
	if len(summary.Trials) != 2 || len(summary.Groups) != 2 {
		t.Fatalf("trials %d groups %d", len(summary.Trials), len(summary.Groups))
	}
	s := summary.Trials[0]
	byAgent := func(agent string, i int) Call {
		n := 0
		for _, c := range s.Calls {
			if c.Agent == agent && !c.Repair {
				if n == i {
					return c
				}
				n++
			}
		}
		t.Fatalf("no call %d of %s", i, agent)
		return Call{}
	}
	if c := byAgent("merger", 0); c.Extraction != "trailing object" || c.Shape != "nested" || c.Source != "reply" {
		t.Fatalf("merger's message: %+v", c)
	}
	if c := byAgent("merger", 1); c.Source != "repair" || c.Action != "exit 0" {
		t.Fatalf("merger's repaired exit: %+v", c)
	}
	if c := byAgent("delivery_lead", 0); c.Placement != "none" {
		t.Fatalf("first delegation: %+v", c)
	}
	if c := byAgent("delivery_lead", 1); c.Placement != "action level" {
		t.Fatalf("second delegation: %+v", c)
	}
	if c := byAgent("file_writer", 1); len(c.Misread) != 1 || !strings.Contains(c.Misread[0], "later action object") {
		t.Fatalf("an example after the action: %+v", c)
	}
	if c := byAgent("review_partner", 0); len(c.Misread) != 1 || !strings.Contains(c.Misread[0], "<think>") || !c.Fallback {
		t.Fatalf("a <think> action from a fallback model: %+v", c)
	}
	if last := s.Calls[len(s.Calls)-1]; last.Error != `Post "<model server>": dial tcp <model server>: connect: refused` {
		t.Fatalf("the error is not scrubbed: %q", last.Error)
	}
	if !s.HTML.Merged || !s.HTML.InGoal || !s.HTML.SameAsSent || !s.HTML.FileMatches || s.Retries != 1 || s.Errors != 1 || s.Steps["merger"] != 2 {
		t.Fatalf("sonnet trial: %+v", s)
	}
	if !near(s.USD, 6*cost(inference.CallRecord{PromptTokens: 1000, CachedTokens: 800, CompletionTokens: 100})) {
		t.Fatalf("dollars %v", s.USD)
	}
	strata := summary.Trials[1]
	if strata.USD != 0 || strata.HTML.SameAsSent || !strings.Contains(strata.HTML.Difference, "angle brackets were escaped") || strata.Calls[0].Unseen != 400-len(`{"action":{"type":"message","message":"<html>x</html>"}}`)/4 {
		t.Fatalf("strata trial: %+v", strata)
	}
	g := summary.Groups[0]
	if g.Arm != "sonnet" || g.Misreads != 2 || g.Fallbacks != 1 || g.Repairs != 1 || g.Errors != 1 || g.Stages["repair"] != 1 || g.Reasoning != len("thought")*9 {
		t.Fatalf("sonnet group: %+v", g)
	}
	page, _ := os.ReadFile(htmlOut)
	for _, want := range []string{"Strata and Sonnet 5.5 on One Workflow", "trailing object / nested", "angle brackets were escaped"} {
		if !strings.Contains(string(page), want) {
			t.Fatalf("the page lacks %q", want)
		}
	}
	if total, err := spendUnder(filepath.Join(root, "sonnet")); err != nil || !near(total, s.USD) {
		t.Fatalf("spend %v %v", total, err)
	}
	if total, _ := spendUnder(filepath.Join(root, "strata")); total != 0 {
		t.Fatalf("strata spend %v", total)
	}
	if total, err := spendUnder(filepath.Join(root, "absent")); err != nil || total != 0 {
		t.Fatalf("no arm yet: %v %v", total, err)
	}
}

func TestReportRefusesKeysAndAddresses(t *testing.T) {
	b := make([]byte, 16)
	rand.Read(b)
	key := "test-" + hex.EncodeToString(b)
	t.Setenv("ANTHROPIC_API_KEY", key)
	for _, text := range []string{"the key " + key, "sk-ant-abcdefghijklmnopqrstu", "Authorization: Bearer abcdefghijklmnop", "x-api-key", "at 10.0.0.1", "on box.local", "/Users/someone/x", "http://server"} {
		if err := checkSafe(text); err == nil || strings.Contains(err.Error(), key) {
			t.Fatalf("%q: %v", text, err)
		}
	}
	if err := checkSafe("a review note about 8 pm, 1-5 pm, and 12,000 books"); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	writeTrial(t, root, "strata/good/trial-1", meta{Arm: "strata", Set: "good", Trial: 1, Profile: "strata-phg", Model: key}, "pass", nil, "")
	htmlOut := filepath.Join(root, "compare.html")
	if err := report(root, htmlOut, filepath.Join(root, "compare.json")); err == nil || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Fatalf("err %v", err)
	}
	if _, err := os.Stat(htmlOut); err == nil {
		t.Fatal("a refused report was written")
	}
}
