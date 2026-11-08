// Command compare reads the trials of the Chapter 9 comparison (run.sh)
// and writes reports/inference-driver/compare.html and a JSON summary.
//
//	compare -root DIR [-html FILE] [-json FILE]   the report, from every trial under DIR
//	compare -spend DIR                            dollars the Anthropic calls under DIR cost
//	compare -worst-case TOKENS -max-tokens N      one trial's worst-case dollars
//
// A trial is a directory holding meta.json, run.log, output.html and
// state/ (run.sh's layout). Per call it reads the reply the way a step
// does (agentflow.ReadReply and ResolveRecordedReply), so its ladder
// categories are those of the D1-D7 probes. It writes no path, base URL,
// address or key, and refuses to write output that looks like one.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"aos/internal/agentflow"
	"aos/internal/inference"
	"aos/internal/runlog"
	"aos/internal/runtime"
)

// Prices in dollars per million tokens: the Anthropic API's for
// claude-sonnet-5-5, with cache writes at the 5-minute TTL.
const (
	priceInput      = 2.00
	priceOutput     = 10.00
	priceCacheRead  = 0.20
	priceCacheWrite = 2.50
)

func main() {
	root := flag.String("root", "", "directory holding the trials (run.sh's AOS_COMPARE_ROOT)")
	htmlPath := flag.String("html", "reports/inference-driver/compare.html", "HTML report to write")
	jsonPath := flag.String("json", "reports/inference-driver/compare.json", "JSON summary to write")
	spend := flag.String("spend", "", "print the dollars the Anthropic calls under this directory cost")
	worst := flag.Int("worst-case", 0, "print one trial's worst-case dollars for this token budget")
	maxTokens := flag.Int("max-tokens", 16000, "max_tokens per call, for -worst-case")
	flag.Parse()
	switch {
	case *spend != "":
		dollars, err := spendUnder(*spend)
		if err != nil {
			fail(err)
		}
		fmt.Printf("%.4f\n", dollars)
	case *worst > 0:
		fmt.Printf("%.4f\n", worstCase(*worst, *maxTokens))
	case *root != "":
		if err := report(*root, *htmlPath, *jsonPath); err != nil {
			fail(err)
		}
	default:
		flag.Usage()
		os.Exit(2)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "compare:", err)
	os.Exit(1)
}

// cost is what one call cost on the Anthropic API. The ledger's prompt
// tokens include the cache reads and writes.
func cost(call inference.CallRecord) float64 {
	uncached := max(0, call.PromptTokens-call.CachedTokens-call.CacheCreationTokens)
	return (float64(uncached)*priceInput + float64(call.CompletionTokens)*priceOutput +
		float64(call.CachedTokens)*priceCacheRead + float64(call.CacheCreationTokens)*priceCacheWrite) / 1e6
}

// worstCase bounds one trial's dollars under a token budget: every budget
// token at the output price, the dearest, plus one call past the budget,
// since the budget is checked before a call and a call's prompt is not
// subtracted from it (D7): a prompt as large as the whole budget at the
// cache-write price and a full max_tokens at the output price. This
// workflow's prompts are far smaller than the budget.
func worstCase(budget, maxTokens int) float64 {
	return (float64(budget)*priceOutput + float64(budget)*priceCacheWrite + float64(maxTokens)*priceOutput) / 1e6
}

// spendUnder sums the cost of every Anthropic call in the ledgers under
// dir.
func spendUnder(dir string) (float64, error) {
	total := 0.0
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil || entry.IsDir() || entry.Name() != runlog.CallsFile {
			return err
		}
		calls, err := runlog.Read[inference.CallRecord](path)
		if err != nil {
			return err
		}
		apis := map[int]string{}
		for _, call := range calls {
			if call.Phase == inference.PhaseStart {
				apis[call.Seq] = call.Params.API
			}
		}
		for _, call := range calls {
			if call.Phase == inference.PhaseOutcome && apis[call.Start] == inference.APIAnthropicMessages {
				total += cost(call)
			}
		}
		return nil
	})
	return total, err
}

type meta struct {
	Arm         string `json:"arm"`
	Set         string `json:"set"`
	Trial       int    `json:"trial"`
	DryRun      bool   `json:"dry_run"`
	Profile     string `json:"profile"`
	Backend     string `json:"backend"`
	Model       string `json:"model"`
	TokenBudget int    `json:"token_budget"`
	TimeBudget  string `json:"time_budget"`
	Started     string `json:"started"`
	Ended       string `json:"ended"`
	ExitCode    int    `json:"exit_code"`
	Rev         string `json:"rev"`
}

type runResult struct {
	ExitStatus struct {
		Code   int    `json:"code"`
		Reason string `json:"reason"`
	} `json:"exit_status"`
	Verification *struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
	} `json:"verification"`
}

// Call is one model call of a trial as the report shows it.
type Call struct {
	Agent      string   `json:"agent"`
	Repair     bool     `json:"repair"`
	Extraction string   `json:"extraction,omitempty"`
	Shape      string   `json:"shape,omitempty"`
	Source     string   `json:"source,omitempty"`
	Action     string   `json:"action,omitempty"`
	Misread    []string `json:"misread,omitempty"`
	Placement  string   `json:"placement,omitempty"`
	Reasoning  int      `json:"reasoning_chars"`
	// Unseen estimates the completion tokens the reply does not show: the
	// completion less the reply at four characters a token. Thinking that
	// the server returns apart is in Reasoning; the rest was dropped.
	Unseen     int     `json:"completion_tokens_not_in_reply"`
	Fallback   bool    `json:"fallback,omitempty"`
	Error      string  `json:"error,omitempty"`
	Prompt     int     `json:"prompt_tokens"`
	Completion int     `json:"completion_tokens"`
	Cached     int     `json:"cached_tokens"`
	CacheWrite int     `json:"cache_write_tokens"`
	LatencyMS  int64   `json:"latency_ms"`
	USD        float64 `json:"usd"`
}

// Trial is one run.
type Trial struct {
	Name     string   `json:"name"`
	Meta     meta     `json:"meta"`
	Verdict  string   `json:"verdict"`
	Exit     int      `json:"exit_code"`
	Calls    []Call   `json:"calls"`
	Steps    agentMap `json:"steps_per_agent"`
	Retries  int      `json:"backend_retries"`
	Errors   int      `json:"backend_errors"`
	USD      float64  `json:"usd"`
	HTML     htmlPath `json:"html"`
	Problems []string `json:"problems,omitempty"`
}

type agentMap map[string]int

// htmlPath is what happened to the merger's HTML on its way to the file.
type htmlPath struct {
	Merged      bool   `json:"merger_sent_html"`
	InGoal      bool   `json:"in_file_writer_goal"`
	Written     bool   `json:"file_writer_wrote"`
	SameAsSent  bool   `json:"written_equals_sent"`
	FileMatches bool   `json:"file_equals_written"`
	Difference  string `json:"difference,omitempty"`
}

func readTrial(root, dir string) (Trial, error) {
	rel, _ := filepath.Rel(root, dir)
	trial := Trial{Name: filepath.ToSlash(rel), Steps: agentMap{}}
	raw, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return trial, err
	}
	if err := json.Unmarshal(raw, &trial.Meta); err != nil {
		return trial, err
	}
	trial.Exit = trial.Meta.ExitCode
	trial.Verdict = "no result"
	if logBytes, err := os.ReadFile(filepath.Join(dir, "run.log")); err == nil {
		var result runResult
		if json.NewDecoder(bytes.NewReader(logBytes)).Decode(&result) == nil {
			trial.Verdict = "missing"
			if result.Verification != nil {
				trial.Verdict = result.Verification.Status
			}
		}
	}
	calls, err := runlog.Read[inference.CallRecord](filepath.Join(dir, "state", runlog.Dir, runlog.CallsFile))
	if err != nil {
		trial.Problems = append(trial.Problems, "no call ledger")
	}
	trial.Calls = readCalls(calls)
	for _, call := range trial.Calls {
		if !call.Repair {
			trial.Steps[call.Agent]++
		}
		if call.Error != "" {
			trial.Errors++
		}
		trial.USD += call.USD
	}
	trial.Retries = countEvents(filepath.Join(dir, "state", "kernel", "events.json"), "backend.retry")
	output, _ := os.ReadFile(filepath.Join(dir, "output.html"))
	trial.HTML = followHTML(trial.Calls, calls, output)
	return trial, nil
}

// readCalls reads every call of a ledger: each step's reply through the
// ladder, with its repair, as the step read them.
func readCalls(records []inference.CallRecord) []Call {
	outcomes := map[int]inference.CallRecord{}
	for _, record := range records {
		if record.Phase == inference.PhaseOutcome {
			outcomes[record.Start] = record
		}
	}
	var calls []Call
	for i, start := range records {
		if start.Phase != inference.PhaseStart {
			continue
		}
		outcome, ok := outcomes[start.Seq]
		if !ok {
			calls = append(calls, Call{Agent: start.AgentID, Repair: start.Repair, Error: "in flight when the run ended"})
			continue
		}
		call := Call{Agent: start.AgentID, Repair: start.Repair, Error: scrub(outcome.Error),
			Prompt: outcome.PromptTokens, Completion: outcome.CompletionTokens, Cached: outcome.CachedTokens,
			CacheWrite: outcome.CacheCreationTokens, LatencyMS: outcome.LatencyMS, Reasoning: len(outcome.Reasoning),
			Unseen:   max(0, outcome.CompletionTokens-len(outcome.Response)/4),
			Fallback: start.Params.API == inference.APIAnthropicMessages && outcome.Model != "" && outcome.Model != start.Params.Model}
		if start.Params.API == inference.APIAnthropicMessages {
			call.USD = cost(outcome)
		}
		if !start.Repair && outcome.Error == "" {
			repaired, hadRepair := repairOf(records, outcomes, i)
			readReply(&call, outcome.Response, repaired, hadRepair)
		}
		calls = append(calls, call)
	}
	return calls
}

// repairOf is the reply of the repair the step starting at records[i]
// asked for, if it did: the same process's next start, marked repair.
func repairOf(records []inference.CallRecord, outcomes map[int]inference.CallRecord, i int) (string, bool) {
	for _, next := range records[i+1:] {
		if next.Phase != inference.PhaseStart || next.PID != records[i].PID {
			continue
		}
		if !next.Repair {
			return "", false
		}
		outcome, ok := outcomes[next.Seq]
		return outcome.Response, ok && outcome.Error == ""
	}
	return "", false
}

func readReply(call *Call, raw, repaired string, hadRepair bool) {
	reading := agentflow.ReadReply(raw)
	call.Extraction, call.Shape = reading.Extraction, reading.Shape
	result, source, err := agentflow.ResolveRecordedReply(raw, repaired, hadRepair)
	call.Source = source
	if err != nil {
		call.Action = "none: " + err.Error()
		return
	}
	call.Action = describe(result)
	call.Misread = misreads(raw, result)
	if result.Action.Type == "delegate" && result.Action.Delegate != nil {
		inside, outside := result.Action.Delegate.Artifacts != "", result.Action.Artifacts != ""
		switch {
		case inside && outside:
			call.Placement = "both"
		case inside:
			call.Placement = "inside delegate"
		case outside:
			call.Placement = "action level"
		default:
			call.Placement = "none"
		}
	}
}

func describe(result runtime.GenerateResult) string {
	a := result.Action
	switch {
	case a.Type == "tool" && a.ToolCall != nil:
		return "tool " + a.ToolCall.Name
	case a.Type == "delegate" && a.Delegate != nil:
		return "delegate " + a.Delegate.AgentID
	case a.Type == "exit" && a.ExitStatus != nil:
		return fmt.Sprintf("exit %d", a.ExitStatus.Code)
	}
	return a.Type
}

// actionObjects are the reply's top-level JSON objects that read as a
// usable action on their own, in order.
func actionObjects(raw string) []string {
	var objects []string
	for i := 0; i < len(raw); i++ {
		if raw[i] != '{' {
			continue
		}
		decoder := json.NewDecoder(strings.NewReader(raw[i:]))
		var value map[string]any
		if decoder.Decode(&value) != nil {
			continue
		}
		end := i + int(decoder.InputOffset())
		if agentflow.ReadReply(raw[i:end]).Usable {
			objects = append(objects, raw[i:end])
		}
		i = end - 1
	}
	return objects
}

// misreads are the reasons a person reading raw would say the step took
// the wrong action: the ladder's known ways to pick the wrong object (D3).
func misreads(raw string, chosen runtime.GenerateResult) []string {
	var reasons []string
	objects := actionObjects(raw)
	if len(objects) > 1 {
		first := agentflow.ReadReply(objects[0]).Result
		if describe(first) != describe(chosen) || !jsonEqual(first, chosen) {
			reasons = append(reasons, "a later action object was taken over the first one in the reply")
		}
	}
	if end := strings.LastIndex(raw, "</think>"); end >= 0 && !strings.Contains(raw[end:], "{") && chosen.Action.Type != "message" {
		reasons = append(reasons, "the action came from inside a <think> block")
	}
	a := chosen.Action
	if (a.Type == "delegate" && (a.Delegate == nil || a.Delegate.AgentID == "")) || (a.Type == "tool" && (a.ToolCall == nil || a.ToolCall.Name == "")) {
		reasons = append(reasons, "the action is incomplete (read by the flat stage, which does not check it)")
	}
	return reasons
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

// followHTML traces the merger's HTML: what it sent in a message action,
// whether delivery_lead put it in file_writer's goal, what file_writer
// wrote, and whether the file holds those bytes.
func followHTML(calls []Call, records []inference.CallRecord, output []byte) htmlPath {
	var path htmlPath
	var sent, written, goal string
	outcomes := map[int]inference.CallRecord{}
	for _, record := range records {
		if record.Phase == inference.PhaseOutcome {
			outcomes[record.Start] = record
		}
	}
	for i, start := range records {
		outcome, ok := outcomes[start.Seq]
		if start.Phase != inference.PhaseStart || start.Repair || !ok || outcome.Error != "" {
			continue
		}
		repaired, hadRepair := repairOf(records, outcomes, i)
		result, _, err := agentflow.ResolveRecordedReply(outcome.Response, repaired, hadRepair)
		if err != nil {
			continue
		}
		a := result.Action
		switch {
		case start.AgentID == "merger" && a.Type == "message" && strings.Contains(strings.ToLower(a.Message), "<html"):
			sent = a.Message
		case start.AgentID == "file_writer" && a.Type == "tool" && a.ToolCall != nil && a.ToolCall.Name == "write_file":
			written = a.ToolCall.Args["content"]
		case a.Type == "delegate" && a.Delegate != nil && a.Delegate.AgentID == "file_writer":
			goal = a.Delegate.Goal
		}
	}
	path.Merged, path.Written = sent != "", written != ""
	path.InGoal = sent != "" && strings.Contains(goal, sent)
	path.SameAsSent = sent != "" && written == sent
	path.FileMatches = written != "" && string(output) == written
	switch {
	case sent != "" && written != "" && written != sent:
		path.Difference = difference(sent, written)
	case written != "" && string(output) != written:
		path.Difference = difference(written, string(output))
	}
	return path
}

// difference says where b first departs from a, and how, without quoting
// either.
func difference(a, b string) string {
	i := 0
	for i < len(a) && i < len(b) && a[i] == b[i] {
		i++
	}
	note := fmt.Sprintf("differs at byte %d (%d vs %d bytes)", i, len(a), len(b))
	switch {
	case strings.Contains(b, "&lt;") && !strings.Contains(a, "&lt;"):
		note += "; angle brackets were escaped"
	case strings.Contains(b, `\"`) && !strings.Contains(a, `\"`):
		note += "; quotes arrived backslash-escaped"
	case strings.Contains(a, "&amp;") && !strings.Contains(b, "&amp;"):
		note += "; entities were decoded"
	}
	return note
}

func countEvents(path, kind string) int {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	var events []struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &events) != nil {
		return 0
	}
	n := 0
	for _, event := range events {
		if event.Type == kind {
			n++
		}
	}
	return n
}

// Group is one arm on one input set.
type Group struct {
	Arm        string         `json:"arm"`
	Set        string         `json:"set"`
	DryRun     bool           `json:"dry_run"`
	Trials     int            `json:"trials"`
	Passes     int            `json:"passes"`
	PassRate   float64        `json:"pass_rate"`
	AllPassed  bool           `json:"all_trials_passed"`
	Verdicts   map[string]int `json:"verdicts"`
	Calls      int            `json:"calls"`
	Repairs    int            `json:"repairs"`
	Steps      agentMap       `json:"steps_per_agent"`
	Stages     map[string]int `json:"stages"`
	Sources    map[string]int `json:"sources"`
	Misreads   int            `json:"misreads"`
	Placements map[string]int `json:"delegate_placements"`
	Reasoning  int            `json:"reasoning_chars"`
	Unseen     int            `json:"completion_tokens_not_in_reply"`
	Fallbacks  int            `json:"fallbacks"`
	Errors     int            `json:"backend_errors"`
	Retries    int            `json:"backend_retries"`
	Prompt     int            `json:"prompt_tokens"`
	Completion int            `json:"completion_tokens"`
	Cached     int            `json:"cached_tokens"`
	CacheWrite int            `json:"cache_write_tokens"`
	LatencyP50 int64          `json:"latency_ms_p50"`
	LatencyP90 int64          `json:"latency_ms_p90"`
	USD        float64        `json:"usd"`
	HTMLIntact int            `json:"html_intact"`
	HTMLNotes  []string       `json:"html_notes,omitempty"`
}

// stage is the D3 row a call's reading falls under.
func stage(call Call) string {
	switch call.Source {
	case agentflow.SourceReply:
		return call.Extraction + " / " + call.Shape
	case "":
		return "backend error"
	}
	return call.Source
}

func group(trials []Trial) []Group {
	byKey := map[string]*Group{}
	var keys []string
	for _, trial := range trials {
		key := fmt.Sprintf("%t|%s|%s", trial.Meta.DryRun, trial.Meta.Arm, trial.Meta.Set)
		g, ok := byKey[key]
		if !ok {
			g = &Group{Arm: trial.Meta.Arm, Set: trial.Meta.Set, DryRun: trial.Meta.DryRun, Verdicts: map[string]int{}, Steps: agentMap{},
				Stages: map[string]int{}, Sources: map[string]int{}, Placements: map[string]int{}}
			byKey[key] = g
			keys = append(keys, key)
		}
		g.Trials++
		g.Verdicts[trial.Verdict]++
		if trial.Verdict == "pass" {
			g.Passes++
		}
		g.Retries += trial.Retries
		g.USD += trial.USD
		if trial.HTML.Merged && trial.HTML.SameAsSent && trial.HTML.FileMatches {
			g.HTMLIntact++
		} else if trial.HTML.Difference != "" {
			g.HTMLNotes = append(g.HTMLNotes, fmt.Sprintf("trial %d: %s", trial.Meta.Trial, trial.HTML.Difference))
		}
		for agent, n := range trial.Steps {
			g.Steps[agent] += n
		}
		var latencies []int64
		for _, call := range trial.Calls {
			g.Calls++
			g.Prompt += call.Prompt
			g.Completion += call.Completion
			g.Cached += call.Cached
			g.CacheWrite += call.CacheWrite
			g.Reasoning += call.Reasoning
			g.Unseen += call.Unseen
			latencies = append(latencies, call.LatencyMS)
			if call.Repair {
				g.Repairs++
				continue
			}
			if call.Error != "" {
				g.Errors++
				continue
			}
			g.Stages[stage(call)]++
			g.Sources[call.Source]++
			if len(call.Misread) > 0 {
				g.Misreads++
			}
			if call.Placement != "" {
				g.Placements[call.Placement]++
			}
			if call.Fallback {
				g.Fallbacks++
			}
		}
		g.LatencyP50, g.LatencyP90 = merge(g.LatencyP50, latencies, 0.5), merge(g.LatencyP90, latencies, 0.9)
	}
	var groups []Group
	sort.Strings(keys)
	for _, key := range keys {
		g := byKey[key]
		g.PassRate = float64(g.Passes) / float64(g.Trials)
		g.AllPassed = g.Passes == g.Trials
		groups = append(groups, *g)
	}
	return groups
}

// merge keeps the larger of a group's quantile so far and this trial's,
// a conservative per-group figure without keeping every latency.
func merge(current int64, latencies []int64, q float64) int64 {
	if len(latencies) == 0 {
		return current
	}
	slices.Sort(latencies)
	return max(current, latencies[int(math.Round(q*float64(len(latencies)-1)))])
}

// Summary is the JSON the report is rendered from.
type Summary struct {
	Groups []Group `json:"groups"`
	Trials []Trial `json:"trials"`
}

func report(root, htmlOut, jsonOut string) error {
	var trials []Trial
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && entry.Name() == "bin" {
			return filepath.SkipDir
		}
		if !entry.IsDir() && entry.Name() == "meta.json" {
			trial, err := readTrial(root, filepath.Dir(path))
			if err != nil {
				return fmt.Errorf("%s: %w", filepath.Dir(path), err)
			}
			trials = append(trials, trial)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(trials) == 0 {
		return errors.New("no trials under the root")
	}
	sort.Slice(trials, func(i, j int) bool { return trials[i].Name < trials[j].Name })
	summary := Summary{Groups: group(trials), Trials: trials}
	data, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		return err
	}
	page := render(summary)
	if err := checkSafe(string(data) + page); err != nil {
		return err
	}
	if err := os.WriteFile(jsonOut, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.WriteFile(htmlOut, []byte(page), 0o644)
}

var (
	urlPattern  = regexp.MustCompile(`(?i)https?://[^\s"\\]+`)
	addrPattern = regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}(:\d+)?\b|(?i)\b[a-z0-9-]+\.local(:\d+)?\b`)
)

// scrub takes the model server's address out of an error, which names the
// URL it posted to: the report says only that it was the model server.
func scrub(text string) string {
	return addrPattern.ReplaceAllString(urlPattern.ReplaceAllString(text, "<model server>"), "<model server>")
}

var unsafePatterns = []struct {
	name    string
	pattern *regexp.Regexp
}{
	{"an API key", regexp.MustCompile(`sk-[A-Za-z0-9_-]{16,}`)},
	{"a bearer token", regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._~+/-]{12,}`)},
	{"an x-api-key header", regexp.MustCompile(`(?i)x-api-key`)},
	{"an IPv4 address", regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`)},
	{"a .local hostname", regexp.MustCompile(`(?i)\b[a-z0-9-]+\.local\b`)},
	{"a home path", regexp.MustCompile(`/(Users|home)/[^/\s"<]+`)},
	{"a URL", regexp.MustCompile(`(?i)https?://`)},
}

// keyEnvs are the variables keys live in, whose values must never appear.
var keyEnvs = []string{"ANTHROPIC_API_KEY", "AOS_LLM_API_KEY", "AOS_VLLM_API_KEY", "AOS_KEY_STRATA_PHG", "AOS_KEY_STRATA_PHG_VLLM", "AOS_KEY_VLLM"}

// checkSafe refuses output that holds a key, an address, a hostname, a
// home path or a URL, naming only the kind.
func checkSafe(text string) error {
	for _, name := range keyEnvs {
		if value := os.Getenv(name); len(value) >= 8 && strings.Contains(text, value) {
			return fmt.Errorf("refusing to write the report: it holds the value of %s", name)
		}
	}
	for _, p := range unsafePatterns {
		if p.pattern.MatchString(text) {
			return fmt.Errorf("refusing to write the report: it holds %s", p.name)
		}
	}
	return nil
}

func render(summary Summary) string {
	var b strings.Builder
	esc := html.EscapeString
	b.WriteString(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Strata and Sonnet Compared</title>
  <style>
    body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; margin: 40px auto; max-width: 1180px; padding: 0 16px; line-height: 1.5; color: #1f2937; }
    code { background: #f3f4f6; padding: 2px 5px; border-radius: 4px; }
    .hero { background: linear-gradient(135deg, #fff7ed, #eff6ff); padding: 24px; border-radius: 16px; }
    .scroll { overflow-x: auto; }
    table { border-collapse: collapse; width: 100%; margin: 8px 0 16px; font-size: 14px; }
    th, td { border: 1px solid #e5e7eb; padding: 6px 10px; text-align: left; vertical-align: top; }
    th { background: #f9fafb; }
    .warn { color: #b45309; }
  </style>
</head>
<body>
  <div class="hero">
    <h1>Strata and Sonnet 5.5 on One Workflow</h1>
    <p>The same workflow (<code>live-reviewed-merge</code>), the same inputs and the same flags, through two model profiles. Each trial ran once on a fresh state directory with a 250,000-token budget and a strict 15-minute time budget, and passed only on review_partner's verdict. Every figure here is read from the trials' call ledgers and results; cached and cache-write tokens are what the servers reported. Dollars are the Anthropic calls' cost at $2 input, $10 output, $0.20 cache reads and $2.50 cache writes per million tokens.</p>
`)
	for _, g := range summary.Groups {
		if g.DryRun {
			b.WriteString(`    <p class="warn">This page includes dry runs: their replies come from the scripted model, and their tokens and dollars are invented.</p>` + "\n")
			break
		}
	}
	b.WriteString("  </div>\n\n  <h2>Outcomes</h2>\n  <div class=\"scroll\"><table>\n    <tr><th>Arm</th><th>Inputs</th><th>Trials</th><th>Verdicts</th><th>Pass rate</th><th>All trials passed (pass^k)</th><th>HTML intact</th><th>Dollars</th></tr>\n")
	for _, g := range summary.Groups {
		fmt.Fprintf(&b, "    <tr><td>%s%s</td><td>%s</td><td>%d</td><td>%s</td><td>%.0f%%</td><td>%s</td><td>%d of %d</td><td>%s</td></tr>\n",
			esc(g.Arm), dry(g.DryRun), esc(g.Set), g.Trials, esc(counts(g.Verdicts)), 100*g.PassRate, yesNo(g.AllPassed, g.Trials), g.HTMLIntact, g.Trials, dollars(g))
	}
	b.WriteString("  </table></div>\n\n  <h2>Calls and tokens</h2>\n  <div class=\"scroll\"><table>\n    <tr><th>Arm</th><th>Inputs</th><th>Calls (repairs)</th><th>Steps per agent</th><th>Prompt tokens</th><th>Completion</th><th>Cache read</th><th>Cache write</th><th>Latency p50 / p90</th><th>Errors, retries</th><th>Fallbacks</th></tr>\n")
	for _, g := range summary.Groups {
		fmt.Fprintf(&b, "    <tr><td>%s%s</td><td>%s</td><td>%d (%d)</td><td>%s</td><td>%d</td><td>%d</td><td>%s</td><td>%s</td><td>%.1f s / %.1f s</td><td>%d, %d</td><td>%d</td></tr>\n",
			esc(g.Arm), dry(g.DryRun), esc(g.Set), g.Calls, g.Repairs, esc(counts(g.Steps)), g.Prompt, g.Completion, share(g.Cached, g.Prompt), share(g.CacheWrite, g.Prompt),
			float64(g.LatencyP50)/1000, float64(g.LatencyP90)/1000, g.Errors, g.Retries, g.Fallbacks)
	}
	b.WriteString(`  </table></div>

  <h2>How the replies were read</h2>
  <p>Each step's reply read the way the step read it (<code>agentflow.ReadReply</code> and <code>ResolveRecordedReply</code>), so the rungs are the D3 probe's: how the object was found (as sent, the trailing object after prose, or the loose repair), the shape that read it (nested, generic, flat, legacy), or a later rung (a repair, fallbackMessage, notesAsMessage, unrepairable). A misread is an action a person reading the reply would not have taken from it: a later object taken over the first, an action from inside a <code>&lt;think&gt;</code> block, or an incomplete action from the flat stage.</p>
  <div class="scroll"><table>
    <tr><th>Arm</th><th>Inputs</th><th>Rungs</th><th>Misreads</th><th>Delegate artifacts</th><th>Reasoning recorded (chars)</th><th>Completion tokens not in the reply (est.)</th></tr>
`)
	for _, g := range summary.Groups {
		fmt.Fprintf(&b, "    <tr><td>%s%s</td><td>%s</td><td>%s</td><td>%d</td><td>%s</td><td>%d</td><td>%d</td></tr>\n",
			esc(g.Arm), dry(g.DryRun), esc(g.Set), esc(counts(g.Stages)), g.Misreads, esc(counts(g.Placements)), g.Reasoning, g.Unseen)
	}
	b.WriteString(`  </table></div>
  <p>Reasoning recorded is the thinking a server returned apart from the reply and the ledger kept: the Anthropic driver records it. The chat-completions driver, not streaming, reads only the reply, so a reasoning model's thinking there is dropped; the last column estimates what was dropped, at four characters a token.</p>

  <h2>The HTML's way to the file</h2>
  <p>merger returns the HTML in a message action, delivery_lead hands it to file_writer in a goal, and file_writer writes it. Intact means file_writer wrote exactly the bytes merger sent and the file holds exactly what was written.</p>
  <div class="scroll"><table>
    <tr><th>Trial</th><th>Verdict</th><th>merger sent HTML</th><th>In file_writer's goal</th><th>Written = sent</th><th>File = written</th><th>Difference</th></tr>
`)
	for _, t := range summary.Trials {
		fmt.Fprintf(&b, "    <tr><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td></tr>\n",
			esc(t.Name), esc(t.Verdict), mark(t.HTML.Merged), mark(t.HTML.InGoal), mark(t.HTML.SameAsSent), mark(t.HTML.FileMatches), esc(t.HTML.Difference))
	}
	b.WriteString("  </table></div>\n\n  <h2>Trials</h2>\n  <div class=\"scroll\"><table>\n    <tr><th>Trial</th><th>Profile</th><th>Model</th><th>Started (UTC)</th><th>Exit</th><th>Verdict</th><th>Calls</th><th>Dollars</th><th>Misread calls</th></tr>\n")
	for _, t := range summary.Trials {
		var misread []string
		for _, call := range t.Calls {
			for _, reason := range call.Misread {
				misread = append(misread, call.Agent+": "+reason)
			}
		}
		fmt.Fprintf(&b, "    <tr><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%d</td><td>%s</td><td>%d</td><td>$%.4f</td><td>%s</td></tr>\n",
			esc(t.Name), esc(t.Meta.Profile), esc(t.Meta.Model), esc(t.Meta.Started), t.Exit, esc(t.Verdict), len(t.Calls), t.USD, esc(strings.Join(misread, "; ")))
	}
	b.WriteString("  </table></div>\n</body>\n</html>\n")
	return b.String()
}

func dry(isDry bool) string {
	if isDry {
		return " (dry run)"
	}
	return ""
}

func yesNo(all bool, trials int) string {
	if trials < 3 {
		return fmt.Sprintf("n/a (%d trials)", trials)
	}
	if all {
		return "yes"
	}
	return "no"
}

func mark(ok bool) string {
	if ok {
		return "yes"
	}
	return "no"
}

func share(part, whole int) string {
	if whole == 0 {
		return "0"
	}
	return fmt.Sprintf("%d (%.1f%%)", part, 100*float64(part)/float64(whole))
}

func dollars(g Group) string {
	if g.Arm != "sonnet" {
		return "n/a"
	}
	return fmt.Sprintf("$%.4f", g.USD)
}

// counts writes a map as "key n, key n", keys sorted.
func counts[V int | float64](m map[string]V) string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		label := key
		if label == "" {
			label = "(none)"
		}
		parts = append(parts, fmt.Sprintf("%s %v", label, m[key]))
	}
	return strings.Join(parts, ", ")
}
