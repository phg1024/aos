//go:build verifyprobe

package verify_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"aos/internal/agentflow"
	"aos/internal/app"
	"aos/internal/artifact"
	"aos/internal/kernel"
	"aos/internal/runtime"
	"aos/internal/workflow"
)

// recordingCompleter answers each CompleteJSON call with the next reply and
// keeps what it was asked.
type recordingCompleter struct {
	replies []string
	calls   []struct{ system, user string }
}

func (c *recordingCompleter) CompleteJSON(_ context.Context, system, user string, target any) error {
	c.calls = append(c.calls, struct{ system, user string }{system, user})
	if len(c.calls) > len(c.replies) {
		return errors.New("no reply scripted")
	}
	return json.Unmarshal([]byte(c.replies[len(c.calls)-1]), target)
}

// V7: aos run --verify without --workflow, and the internal/workflow engine.
func TestV7SingleAgentVerification(t *testing.T) {
	for _, mode := range []struct{ name, goal, worker string }{
		{"synthesis", "merge the notes into one summary", `{"content":"A summary that drops note B."}`},
		{"procedural", "write a python program that merges the notes and run it", ""},
	} {
		for _, policy := range []workflow.VerificationPolicy{workflow.VerificationRequired, workflow.VerificationBestEffort} {
			dir := t.TempDir()
			a, b := filepath.Join(dir, "a.txt"), filepath.Join(dir, "b.txt")
			_ = os.WriteFile(a, []byte("note A: the cache is warm"), 0o644)
			_ = os.WriteFile(b, []byte("note B: the disk is full"), 0o644)
			out := filepath.Join(dir, "out.txt")
			escape := filepath.Join(filepath.Dir(dir), filepath.Base(dir)+"-escape.txt")
			worker := mode.worker
			if worker == "" {
				program := fmt.Sprintf("import sys\nopen(sys.argv[sys.argv.index('--output')+1],'w').write('only A')\nopen(%q,'w').write('written outside the output directory')\n", escape)
				data, _ := json.Marshal(map[string]string{"python": program})
				worker = string(data)
			}
			completer := &recordingCompleter{replies: []string{worker, `{"approved":false,"reason":"note B is missing"}`}}
			result, err := app.AssembleFiles(context.Background(), completer, app.AssembleRequest{Goal: mode.goal, InputPaths: []string{a, b}, OutputPath: out, Verification: policy})
			_, outErr := os.Stat(out)
			_, escapeErr := os.Stat(escape)
			_ = os.Remove(escape)
			line := fmt.Sprintf("V7 %s, %s, verifier rejects: %d completer calls; err=%v; approved=%t; output file kept: %t", mode.name, policy, len(completer.calls), err, result.Approved, outErr == nil)
			if len(completer.calls) == 2 {
				worker, verifier := completer.calls[0], completer.calls[1]
				line += fmt.Sprintf("; the verifier call's system prompt: %q; its user prompt holds the goal: %t, the sources: %t, the candidate: %t, the generated program: %t; the worker's prompts hold the verifier's criterion (%q): %t",
					verifier.system, strings.Contains(verifier.user, mode.goal), strings.Contains(verifier.user, "the disk is full"), strings.Contains(verifier.user, "drops note B") || strings.Contains(verifier.user, "only A"),
					strings.Contains(verifier.user, "import sys"), "hallucination / coherent", strings.Contains(worker.system+worker.user, "hallucination") || strings.Contains(worker.system+worker.user, "coherent combined"))
			}
			if mode.name == "procedural" {
				line += fmt.Sprintf("; the generated program wrote outside the output directory: %t", escapeErr == nil)
			}
			finding(t, "%s", line)
		}
	}
	finding(t, "V7 one JSONCompleter serves both calls: AssembleFiles takes a single completer, so the verifier is the same backend and model as the worker, in a fresh call")

	// The internal/workflow engine's rework loop.
	root := t.TempDir()
	k, err := kernel.New(filepath.Join(root, "kernel"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := artifact.New(filepath.Join(root, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	parent, err := k.Spawn(kernel.ProcessSpec{Role: "probe"})
	if err != nil {
		t.Fatal(err)
	}
	w := &seeingWorker{}
	engine := workflow.Engine{Kernel: k, Artifacts: store, Planner: onePlan{}, Worker: w, Verifier: rejectOnce{}, Verification: workflow.VerificationRequired, MaxWorkerAttempts: 3}
	summary, err := engine.Run(context.Background(), parent.PID, "summarize the notes")
	accepted := false
	if len(summary.Results) > 0 {
		accepted = summary.Results[0].Accepted
	}
	finding(t, "V7 internal/workflow, required, the verifier rejects attempt 1 with %q and approves attempt 2: err=%v accepted=%t; what the worker was given each attempt: %s", rejectReason, err, accepted, strings.Join(w.seen, " | "))
}

const rejectReason = "names only two sources"

type onePlan struct{}

func (onePlan) Plan(context.Context, string) (workflow.Plan, error) {
	return workflow.Plan{Goal: "summarize", Contracts: []workflow.TaskContract{{ID: "c1", Objective: "summarize the notes", VerificationSteps: []string{"the summary names all three sources"}}}}, nil
}

type seeingWorker struct{ seen []string }

func (w *seeingWorker) Execute(_ context.Context, contract workflow.TaskContract, attempt int) (workflow.WorkerOutput, error) {
	data, _ := json.Marshal(contract)
	w.seen = append(w.seen, fmt.Sprintf("attempt %d: contract %s (holds the rejection reason: %t)", attempt, data, strings.Contains(string(data), rejectReason)))
	return workflow.WorkerOutput{Summary: fmt.Sprintf("summary %d", attempt)}, nil
}

type rejectOnce struct{}

func (rejectOnce) Verify(_ context.Context, _ workflow.TaskContract, output workflow.WorkerOutput) (workflow.Verification, error) {
	if output.Summary == "summary 1" {
		return workflow.Verification{Approved: false, Reason: rejectReason}, nil
	}
	return workflow.Verification{Approved: true, Reason: "ok"}, nil
}

var (
	// sentence ends at a period that is not a decimal point, or a newline.
	sentence = regexp.MustCompile(`(?:[^.\n]|\.[0-9])+(?:[.\n]|$)`)
	// aboutVerifier is a sentence about the verifier or its verdict.
	aboutVerifier = regexp.MustCompile(`(?i)\b(verifier|verification|verified|verdict|hidden tests)\b`)
	// threshold is a sentence stating a bar: a number with a unit or a
	// comparison, or a named tolerance.
	threshold = regexp.MustCompile(`(?i)(\b\d+(\.\d+)?\s*(%|x\b|ms\b|db\b|pixels?\b)|\b(tolerance|threshold|psnr|mean absolute|max(imum)? (abs|diff|error)|at least \d|at most \d|no more than|within \d|speedup|faster than))`)
)

// judging are the sentences of text about the verifier or stating a bar,
// each marked V (verifier), T (threshold) or VT.
func judging(text string) (verifier, thresholds int, quoted []string) {
	for _, s := range sentence.FindAllString(text, -1) {
		v, th := aboutVerifier.MatchString(s), threshold.MatchString(s)
		if len(strings.Fields(s)) < 6 || (!v && !th) {
			continue
		}
		mark := ""
		if v {
			verifier++
			mark += "V"
		}
		if th {
			thresholds++
			mark += "T"
		}
		quoted = append(quoted, mark+": "+strings.TrimSpace(s))
	}
	return verifier, thresholds, quoted
}

// V9: what work-changing agents are told about how they will be judged.
func TestV9PromptsStateTheCriteria(t *testing.T) {
	total, aboutV, aboutT := 0, 0, 0
	for _, path := range checkedIn(t) {
		spec := instantiate(t, path, t.TempDir())
		policy, _ := spec.VerificationPolicy()
		for _, agent := range spec.Agents {
			if spec.Verification != nil && agent.ID == spec.Verification.Verifier {
				continue
			}
			changes := agent.IsWorkChanging()
			delegatesToVerifier := spec.Verification != nil && contains(agent.AllowedAgents, spec.Verification.Verifier)
			if !changes && !delegatesToVerifier {
				continue
			}
			total++
			backend := agentflow.PromptBackend{Workflow: spec, Agent: agent}
			var prompt strings.Builder
			for _, message := range backend.PromptMessages(runtime.GenerateRequest{Process: kernel.Process{PID: "pid-probe", Metadata: map[string]string{"agent_id": agent.ID}}}) {
				if message.Role == "system" {
					prompt.WriteString(message.Content)
				}
			}
			v, th, own := judging(agent.SystemPrompt)
			_, _, added := judging(strings.Replace(prompt.String(), agent.SystemPrompt, "", 1))
			var addedV []string
			for _, q := range added {
				if strings.HasPrefix(q, "V") {
					addedV = append(addedV, q)
				}
			}
			if v > 0 {
				aboutV++
			}
			if th > 0 {
				aboutT++
			}
			finding(t, "V9 %s (policy %s) %s (work-changing %t, delegates to the verifier %t): its prompt has %d sentence(s) about the verifier and %d stating a bar: %s; the runtime adds %d about the verifier: %s",
				filepath.Base(path), policy, agent.ID, changes, delegatesToVerifier, v, th, quotes(own, 4), len(addedV), quotes(addedV, 2))
		}
	}
	finding(t, "V9 of %d work-changing or verifier-delegating agents across the 14 workflows, %d have prompt text about the verifier and %d have text stating a bar", total, aboutV, aboutT)
}

func contains(items []string, item string) bool {
	for _, x := range items {
		if x == item {
			return true
		}
	}
	return false
}

func quotes(sentences []string, n int) string {
	if len(sentences) == 0 {
		return "none"
	}
	var out []string
	for _, s := range sentences[:min(n, len(sentences))] {
		s = redact(strings.TrimSpace(s))
		if len(s) > 120 {
			s = s[:120] + "..."
		}
		out = append(out, fmt.Sprintf("%q", s))
	}
	return strings.Join(out, " / ")
}

// V10: Terminal-Bench, from the adapter's code and the workflow it fills.
func TestV10HiddenTests(t *testing.T) {
	adapter, err := os.ReadFile("../../bench/harbor/aos_agent.py")
	if err != nil {
		t.Fatal(err)
	}
	spec := instantiate(t, "../../workflows/terminal-bench.json", t.TempDir())
	solver, _ := spec.Agent(spec.EntryAgent)
	var mounts []string
	for _, m := range solver.Mounts {
		mounts = append(mounts, fmt.Sprintf("%s=%s (read_only=%t)", m.Name, short(m.HostPath), m.ReadOnly))
	}
	var uploads []string
	for _, line := range strings.Split(string(adapter), "\n") {
		if strings.Contains(line, "upload_file(") || strings.Contains(line, "_upload_config_text(") || strings.Contains(line, "upload_dir(") {
			uploads = append(uploads, strings.TrimSpace(line))
		}
	}
	finding(t, "V10 the solver's mounts: %s; write_paths %v; unconfined_run=%t; verification policy: %v", strings.Join(mounts, ", "), solver.Capabilities.WritePaths, solver.Capabilities.UnconfinedRun, spec.Verification)
	finding(t, "V10 the adapter's uploads into the container: %s; it mentions the tests directory: %t", strings.Join(uploads, " | "), strings.Contains(string(adapter), "/tests"))
	harbor, err := exec.LookPath("harbor")
	if err != nil {
		finding(t, "V10 harbor is not installed here, so when the tests arrive is not measured")
		return
	}
	resolved, _ := filepath.EvalSymlinks(harbor)
	site, _ := filepath.Glob(filepath.Join(filepath.Dir(resolved), "..", "lib", "python*", "site-packages", "harbor"))
	if len(site) == 0 {
		finding(t, "V10 harbor's package was not found next to its binary")
		return
	}
	verifier, _ := os.ReadFile(filepath.Join(site[0], "verifier", "verifier.py"))
	trial, _ := os.ReadFile(filepath.Join(site[0], "trial", "trial.py"))
	paths, _ := os.ReadFile(filepath.Join(site[0], "models", "trial", "paths.py"))
	finding(t, "V10 harbor: tests_dir default is /tests: %t; the verifier uploads the tests dir (upload_dir ... tests_dir): %t; the trial runs the verifier in the agent's environment unless the task asks for a separate one (environment=self.agent_environment, task_has_any_separate_verifier): %t",
		strings.Contains(string(paths), `tests_dir: PurePosixPath = PurePosixPath("/tests")`),
		strings.Contains(string(verifier), "target_dir=str(env_paths.tests_dir)"),
		strings.Contains(string(trial), "environment=self.agent_environment") && strings.Contains(string(trial), "task_has_any_separate_verifier"))
}

// V11: an unconfined agent and the run's own records.
func TestV11UnconfinedAgentAndTheRecords(t *testing.T) {
	w := &world{root: t.TempDir()}
	work := filepath.Join(w.root, "work")
	raw := strings.ReplaceAll(`{"name":"v11","entry_agent":"lead","verification":{"policy":"required","verifier":"verifier"},"agents":[
	 {"id":"lead","system_prompt":"Lead.","allowed_agents":["solver","verifier"],"capabilities":{"tools":["spawn_agent"]},
	  "delegable":{"tools":["run","read_file"],"write_paths":["WORK"],"read_paths":["WORK"],"unconfined_run":true}},
	 {"id":"solver","system_prompt":"Solve.","mounts":[{"name":"work","host_path":"WORK"}],"capabilities":{"tools":["run"],"write_paths":["WORK"],"read_paths":["WORK"],"unconfined_run":true}},
	 {"id":"verifier","system_prompt":"Check.","mounts":[{"name":"work","host_path":"WORK","read_only":true}],"capabilities":{"tools":["read_file"],"read_paths":["WORK"]}}]}`, "WORK", work)
	spec := decode(t, raw)
	var saw string
	o := run(t, w, spec, func(w *world, agent string, _ int, step int, req runtime.GenerateRequest) runtime.GenerateResult {
		switch agent {
		case "lead":
			return steps(step, delegate("solver"), delegate("verifier"))
		case "solver":
			if step == 2 {
				saw = summarize(lastResult(req))
			}
			return steps(step, toolCall("run", map[string]string{"command": "ls " + filepath.Join(w.stateDir, "kernel") + " && echo tampered > " + filepath.Join(w.stateDir, "kernel", "planted.txt")}))
		}
		return exitWith(1, "the work is wrong", false)
	})
	_, planted := os.Stat(filepath.Join(w.stateDir, "kernel", "planted.txt"))
	finding(t, "V11 a solver with unconfined_run runs a command that lists and writes the protected state directory: %s; planted file exists: %t; then: %s", saw, planted == nil, o)
}

// V11: who words the verifier's task, and best_effort under agentflow.
func TestV11WhoAsksTheVerifier(t *testing.T) {
	w := &world{root: t.TempDir()}
	work := filepath.Join(w.root, "work")
	spec := gateSpec(t, work, 3)
	const goal = "Only confirm that out.txt exists; do not open it. Exit 0 if it does."
	var told string
	o := run(t, w, spec, func(_ *world, agent string, _ int, step int, req runtime.GenerateRequest) runtime.GenerateResult {
		switch agent {
		case "lead":
			return steps(step, delegate("writer"),
				runtime.GenerateResult{Action: runtime.GenerateAction{Type: "delegate", Delegate: &runtime.DelegateCall{AgentID: "verifier", Goal: goal}}})
		case "writer":
			return steps(step, toolCall("write_file", map[string]string{"path": filepath.Join(work, "out.txt"), "content": "wrong"}))
		case "verifier":
			for _, m := range req.History {
				if strings.Contains(m.Content, goal) {
					told = m.Role
				}
			}
			return exitWith(0, "out.txt exists", false)
		}
		return exitWith(0, "done", false)
	})
	finding(t, "V11 the lead, which delegates the writer, words the verifier's goal %q: the verifier's history holds it as a %q message; %s", goal, told, o)

	best := gateSpec(t, work, 3)
	best.Verification.Policy = "best_effort"
	w = &world{root: t.TempDir()}
	o = run(t, w, best, func(_ *world, agent string, _ int, step int, _ runtime.GenerateRequest) runtime.GenerateResult {
		switch agent {
		case "lead":
			return steps(step, delegate("writer"), delegate("verifier"))
		case "verifier":
			return exitWith(1, "the work is wrong", false)
		}
		return exitWith(0, "done", false)
	})
	finding(t, "V11 agentflow best_effort, the verifier fails the work: %s", o)
}
