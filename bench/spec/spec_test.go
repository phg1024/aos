//go:build workflowspec

// Package spec_test holds the measure-only probes of workflow specs behind
// Chapter 10 (Workflow Specs as Executable Contracts). The probe code is
// new; the behaviour it measures is v0.1.7's (6745ba4): run it in a
// checkout of that tag to reproduce reports/workflow-spec. Each probe
// S1..S10 checks one claim an earlier design review made, against
// Validate and against a run on a scripted backend that returns actions
// directly, so no model, prompt or parser is involved. Findings are
// logged as "FINDING:" lines; a probe fails only when it cannot run.
//
//	go test -tags workflowspec -count=1 -v ./bench/spec/
//
// Host paths are printed with a home directory's user name replaced, and
// no address or hostname is printed.
package spec_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"

	"aos/internal/agentflow"
	"aos/internal/artifact"
	"aos/internal/capability"
	"aos/internal/ipc"
	"aos/internal/kernel"
	"aos/internal/runtime"
	"aos/internal/tools"
	"aos/internal/workflow"
)

var (
	homePath = regexp.MustCompile(`/(Users|home)/[^/\s"]+`)
	address  = regexp.MustCompile(`\b(\d{1,3}\.){3}\d{1,3}(:\d+)?\b`)
)

// redact keeps user names and addresses out of a finding.
func redact(text string) string {
	text = homePath.ReplaceAllString(text, "/$1/<user>")
	return address.ReplaceAllString(text, "<address>")
}

func finding(t *testing.T, format string, args ...any) {
	t.Helper()
	t.Log("FINDING: " + redact(fmt.Sprintf(format, args...)))
}

// script decides an agent process's action at its step (1-based); n is the
// number of agent processes spawned before this one.
type script func(ctx context.Context, agent string, n, step int, req runtime.GenerateRequest) runtime.GenerateResult

// scriptFactory gives every agent process a backend of its own over one
// script, and counts the processes.
type scriptFactory struct {
	fn script

	mu     sync.Mutex
	spawns []string
	steps  map[string]int
}

func (f *scriptFactory) BackendFor(agent agentflow.AgentSpec, _ agentflow.WorkflowSpec) (runtime.Backend, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := len(f.spawns)
	f.spawns = append(f.spawns, agent.ID)
	return &scriptBackend{factory: f, agent: agent.ID, n: n}, nil
}

func (f *scriptFactory) spawned() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.spawns...)
}

type scriptBackend struct {
	factory *scriptFactory
	agent   string
	n, step int
}

func (b *scriptBackend) Generate(ctx context.Context, req runtime.GenerateRequest) (runtime.GenerateResult, error) {
	b.step++
	b.factory.mu.Lock()
	if b.factory.steps == nil {
		b.factory.steps = map[string]int{}
	}
	b.factory.steps[b.agent]++
	b.factory.mu.Unlock()
	return b.factory.fn(ctx, b.agent, b.n, b.step, req), nil
}

func delegate(to string) runtime.GenerateResult {
	return runtime.GenerateResult{Action: runtime.GenerateAction{Type: "delegate", Delegate: &runtime.DelegateCall{AgentID: to, Goal: "go on"}}}
}

func exitWith(code int, reason string) runtime.GenerateResult {
	return runtime.GenerateResult{Action: runtime.GenerateAction{Type: "exit", ExitStatus: &kernel.ExitStatus{Code: code, Reason: reason}}}
}

func toolCall(name string, args map[string]string) runtime.GenerateResult {
	return runtime.GenerateResult{Action: runtime.GenerateAction{Type: "tool", ToolCall: &runtime.ToolCall{Name: name, Args: args}}}
}

// meter charges a call's tokens to the step's meter, as a driver does.
func meter(ctx context.Context, tokens int) {
	if info, ok := runtime.CallInfoFrom(ctx); ok && info.Meter != nil {
		info.Meter.Add(tokens)
	}
}

type outcome struct {
	summary agentflow.RunSummary
	err     error
	spawned []string
	steps   map[string]int
	kernel  *kernel.Kernel
}

// run runs spec on a scripted backend in a fresh state directory, as aos
// run --workflow does, without creating writable mounts.
func run(t *testing.T, spec agentflow.WorkflowSpec, budget capability.Budget, fn script) outcome {
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
	bus, err := ipc.New(filepath.Join(root, "bus"), k)
	if err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(root, "input.txt")
	_ = os.WriteFile(input, []byte("input"), 0o644)
	factory := &scriptFactory{fn: fn}
	engine := agentflow.Engine{Kernel: k, Artifacts: store, Bus: bus, Tools: tools.NewRuntime(), Factory: factory, Budget: budget,
		BackendRetry: runtime.BackendRetryPolicy{Attempts: -1}}
	summary, err := engine.Run(context.Background(), spec, agentflow.RunRequest{Goal: "probe", InputPaths: []string{input}, OutputPath: filepath.Join(root, "out.html")})
	factory.mu.Lock()
	steps := map[string]int{}
	for id, n := range factory.steps {
		steps[id] = n
	}
	factory.mu.Unlock()
	return outcome{summary: summary, err: err, spawned: factory.spawned(), steps: steps, kernel: k}
}

func (o outcome) String() string {
	verdict := ""
	if v := o.summary.Verification; v != nil {
		verdict = fmt.Sprintf(" verdict=%s (%s)", v.Status, v.Reason)
	}
	errText := ""
	if o.err != nil {
		errText = fmt.Sprintf(" err=%q", o.err.Error())
	}
	return fmt.Sprintf("workflow exit code=%d reason=%q%s%s; %d agent process(es)", o.summary.ExitStatus.Code, o.summary.ExitStatus.Reason, verdict, errText, len(o.spawned))
}

func decode(t *testing.T, raw string) agentflow.WorkflowSpec {
	t.Helper()
	var spec agentflow.WorkflowSpec
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return spec
}

func errText(err error) string {
	if err == nil {
		return "accepted"
	}
	return "refused: " + err.Error()
}

// exitsAfterDelegating delegates to each agent in order once, then exits 0.
func exitsAfterDelegating(order map[string][]string) script {
	return func(_ context.Context, agent string, _ int, step int, _ runtime.GenerateRequest) runtime.GenerateResult {
		if next := order[agent]; step <= len(next) {
			return delegate(next[step-1])
		}
		return exitWith(0, "done")
	}
}

// S1: an agent that lists itself in allowed_agents.
func TestS1SelfDelegation(t *testing.T) {
	spec := decode(t, `{"name":"s1","entry_agent":"a","agents":[{"id":"a","system_prompt":"Work.","allowed_agents":["a"],"capabilities":{"tools":["spawn_agent"]}}]}`)
	finding(t, "S1 Validate of an agent in its own allowed_agents: %s", errText(spec.Validate()))
	const cap = 30
	o := run(t, spec, capability.Budget{}, func(_ context.Context, _ string, n, step int, _ runtime.GenerateRequest) runtime.GenerateResult {
		if step == 1 && n < cap {
			return delegate("a")
		}
		return exitWith(0, "done")
	})
	finding(t, "S1 run: a delegates to a on its first step until the probe stops it at %d processes: %s", cap, o)
}

// S2: a delegation cycle A -> B -> A, at run time: with no budget, with a
// step limit of 1, and with a token budget.
func TestS2DelegationCycle(t *testing.T) {
	raw := `{"name":"s2","entry_agent":"a","agents":[{"id":"a","system_prompt":"Work.","allowed_agents":["b"],"capabilities":{"tools":["spawn_agent"]}%s},{"id":"b","system_prompt":"Work.","allowed_agents":["a"],"capabilities":{"tools":["spawn_agent"]}%s}]}`
	spec := decode(t, fmt.Sprintf(raw, "", ""))
	finding(t, "S2 Validate of a -> b -> a: %s", errText(spec.Validate()))
	const cap = 60
	cycle := func(tokens int) script {
		return func(ctx context.Context, agent string, n, step int, _ runtime.GenerateRequest) runtime.GenerateResult {
			meter(ctx, tokens)
			other := map[string]string{"a": "b", "b": "a"}[agent]
			if step == 1 && n < cap {
				return delegate(other)
			}
			return exitWith(0, "done")
		}
	}
	o := run(t, spec, capability.Budget{}, cycle(0))
	finding(t, "S2 no budget, max_steps default: every process delegates on its first step until the probe's own stop at %d: %s", cap, o)
	o = run(t, decode(t, fmt.Sprintf(raw, `,"max_steps":1`, `,"max_steps":1`)), capability.Budget{}, cycle(0))
	finding(t, "S2 max_steps 1 for both: %s; deepest exit: %s", o, deepestExit(o))
	o = run(t, spec, capability.Budget{Tokens: 20000}, cycle(1000))
	finding(t, "S2 a run token budget of 20000, 1000 tokens a call: %s; deepest exit: %s", o, deepestExit(o))
	finding(t, "S2 a process's step limit counts its own steps; a delegation is one step of the delegator and the child starts with its own limit, so steps do not bound the depth. Only a budget shared through the run's account chain does, or the time budget when strict.")
}

// deepestExit is the exit of the last agent process spawned.
func deepestExit(o outcome) string {
	processes, _ := o.kernel.Processes()
	var last *kernel.Process
	for i := range processes {
		if processes[i].Metadata["agent_id"] != "" && (last == nil || processes[i].PID > last.PID) {
			last = &processes[i]
		}
	}
	if last == nil || last.ExitStatus == nil {
		return "none"
	}
	return fmt.Sprintf("%s code=%d reason=%q", last.Metadata["agent_id"], last.ExitStatus.Code, last.ExitStatus.Reason)
}

// S3: an agent no allowed_agents edge reaches from the entry agent.
func TestS3UnreachableAgent(t *testing.T) {
	spec := decode(t, `{"name":"s3","entry_agent":"a","agents":[{"id":"a","system_prompt":"Work.","allowed_agents":["b"],"capabilities":{"tools":["spawn_agent"]}},{"id":"b","system_prompt":"Work."},{"id":"c","system_prompt":"Work."}]}`)
	finding(t, "S3 Validate with c unreachable from the entry agent: %s", errText(spec.Validate()))
	o := run(t, spec, capability.Budget{}, exitsAfterDelegating(map[string][]string{"a": {"b"}}))
	finding(t, "S3 run: %s; processes spawned: %v", o, o.spawned)
	// allowed_agents is only half of delegating: the runtime also needs the
	// spawn_agent tool.
	noTool := decode(t, `{"name":"s3b","entry_agent":"a","agents":[{"id":"a","system_prompt":"Work.","allowed_agents":["b"]},{"id":"b","system_prompt":"Work."}]}`)
	o = run(t, noTool, capability.Budget{}, exitsAfterDelegating(map[string][]string{"a": {"b"}}))
	finding(t, "S3 allowed_agents without the spawn_agent tool: Validate %s; run: %s; processes spawned: %v; a's delegation was refused: %t", errText(noTool.Validate()), o, o.spawned, refused(o.kernel, "delegate"))
}

// S4: max_steps negative and zero.
func TestS4MaxSteps(t *testing.T) {
	for _, steps := range []int{-5, 0, 1_000_000_000} {
		spec := decode(t, fmt.Sprintf(`{"name":"s4","entry_agent":"a","agents":[{"id":"a","system_prompt":"Work.","max_steps":%d}]}`, steps))
		validate := errText(spec.Validate())
		if steps == 1_000_000_000 {
			finding(t, "S4 Validate of max_steps %d: %s (not run)", steps, validate)
			continue
		}
		// A tool the agent does not hold, with new args each step: a tool error
		// that takes a step and is no repeat.
		o := run(t, spec, capability.Budget{}, func(_ context.Context, _ string, _ int, step int, _ runtime.GenerateRequest) runtime.GenerateResult {
			return toolCall("read_file", map[string]string{"path": fmt.Sprintf("/nonexistent/%d", step)})
		})
		finding(t, "S4 max_steps %d: Validate %s; run: %d steps taken, %s", steps, validate, o.steps["a"], o)
	}
}

// S5: an unreachable agent holding run and write access to "/".
func TestS5UnreachablePowerfulAgent(t *testing.T) {
	spec := decode(t, `{"name":"s5","entry_agent":"a","agents":[
		{"id":"a","system_prompt":"Work.","capabilities":{"tools":["read_file"]}},
		{"id":"root","system_prompt":"Anything.","mounts":[{"name":"all","host_path":"/","read_only":false}],"capabilities":{"tools":["run","write_file"],"write_paths":["/"],"read_paths":["/"],"unconfined_run":true}}]}`)
	finding(t, "S5 Validate with an unreachable agent holding run, write_file, write_paths [/], a writable mount of / and unconfined_run: %s", errText(spec.Validate()))
	reachable := decode(t, `{"name":"s5b","entry_agent":"a","agents":[
		{"id":"a","system_prompt":"Work.","allowed_agents":["root"],"capabilities":{"tools":["read_file"]}},
		{"id":"root","system_prompt":"Anything.","capabilities":{"tools":["run","write_file"],"write_paths":["/"],"unconfined_run":true}}]}`)
	finding(t, "S5 the same agent made reachable from an entry agent without that authority: %s", errText(reachable.Validate()))
	entry := decode(t, `{"name":"s5c","entry_agent":"root","agents":[
		{"id":"root","system_prompt":"Anything.","capabilities":{"tools":["run","write_file"],"write_paths":["/"],"unconfined_run":true}}]}`)
	finding(t, "S5 the same authority on the entry agent: %s (the spec and run request are the trust root)", errText(entry.Validate()))
	o := run(t, spec, capability.Budget{}, exitsAfterDelegating(nil))
	finding(t, "S5 run: %s; processes spawned: %v", o, o.spawned)
}

// S6: unknown fields, as aos run decodes a spec (json.Unmarshal), and what
// each typo does.
func TestS6UnknownFields(t *testing.T) {
	const correct = `{"name":"s6","entry_agent":"lead",%[1]s"agents":[
		{"id":"lead","system_prompt":"Lead.","allowed_agents":["writer","checker"],"capabilities":{"tools":["spawn_agent","read_file","write_file"]}},
		{"id":"writer","system_prompt":"Write.","%[2]s":%[3]s,"%[4]s":{"tools":["write_file"]}},
		{"id":"checker","system_prompt":"Check.","capabilities":{"tools":["read_file"]}}]}`
	verification := `"verification":{"policy":"required","verifier":"checker"},`
	cases := []struct {
		name                      string
		top, maxStepsKey, capsKey string
	}{
		{name: "as written", top: verification, maxStepsKey: "max_steps", capsKey: "capabilities"},
		{name: `"verifcation"`, top: strings.Replace(verification, "verification", "verifcation", 1), maxStepsKey: "max_steps", capsKey: "capabilities"},
		{name: `"max_step"`, top: verification, maxStepsKey: "max_step", capsKey: "capabilities"},
		{name: `"capabilites"`, top: verification, maxStepsKey: "max_steps", capsKey: "capabilites"},
	}
	order := map[string][]string{"lead": {"writer", "checker"}}
	writes := func(ctx context.Context, agent string, n, step int, req runtime.GenerateRequest) runtime.GenerateResult {
		switch {
		case agent == "writer" && step == 1:
			return toolCall("write_file", map[string]string{"path": "out.html", "content": "x"})
		case agent == "writer" && step <= 3:
			// A writer that would go on past a limit of 2 steps.
			return toolCall("write_file", map[string]string{"path": fmt.Sprintf("more-%d.html", step), "content": "x"})
		case agent == "writer":
			return exitWith(0, "written")
		case agent == "checker":
			return exitWith(1, "the work is wrong")
		}
		return exitsAfterDelegating(order)(ctx, agent, n, step, req)
	}
	for _, c := range cases {
		raw := fmt.Sprintf(correct, c.top, c.maxStepsKey, "2", c.capsKey)
		spec := decode(t, raw)
		writer, _ := spec.Agent("writer")
		o := run(t, spec, capability.Budget{}, writes)
		finding(t, "S6 %-15s Validate %s; decoded: verification=%t writer max_steps=%d writer tools=%v; run (checker fails the work): writer took %d steps; %s",
			c.name+":", errText(spec.Validate()), spec.Verification != nil, writer.MaxSteps, writer.Capabilities.Tools, o.steps["writer"], o)
	}
	allowed := decode(t, `{"name":"s6b","entry_agent":"lead","agents":[{"id":"lead","system_prompt":"Lead.","allowed_agent":["writer"],"capabilities":{"tools":["spawn_agent"]}},{"id":"writer","system_prompt":"Write."}]}`)
	o := run(t, allowed, capability.Budget{}, exitsAfterDelegating(map[string][]string{"lead": {"writer"}}))
	finding(t, `S6 "allowed_agent": Validate %s; decoded allowed_agents=%v; run: %s`, errText(allowed.Validate()), allowed.Agents[0].AllowedAgents, o)
	withVerifier := decode(t, `{"name":"s6c","entry_agent":"lead","verification":{"policy":"required","verifier":"writer"},"agents":[{"id":"lead","system_prompt":"Lead.","allowed_agent":["writer"]},{"id":"writer","system_prompt":"Write."}]}`)
	finding(t, `S6 "allowed_agent" with a verifier behind it: Validate %s`, errText(withVerifier.Validate()))
	model := decode(t, `{"name":"s6d","entry_agent":"a","agents":[{"id":"a","system_prompt":"Work.","model":"claude-sonnet-5-5","profile":"claude","backend":"anthropic"}]}`)
	again, _ := json.Marshal(model.Agents[0])
	finding(t, `S6 per-agent "model", "profile", "backend" (D5): Validate %s; the agent after a round trip: %s`, errText(model.Validate()), again)
	strict := json.NewDecoder(strings.NewReader(`{"name":"s6","entry_agent":"a","verifcation":{},"agents":[{"id":"a","system_prompt":"Work.","max_step":3}]}`))
	strict.DisallowUnknownFields()
	var refused agentflow.WorkflowSpec
	finding(t, "S6 a decoder with DisallowUnknownFields stops at the first: %v", strict.Decode(&refused))
}

// S7: a model per agent.
func TestS7PerAgentModel(t *testing.T) {
	completer := &noCompleter{}
	factory := agentflow.StaticBackendFactory{Completer: completer}
	spec := decode(t, `{"name":"s7","entry_agent":"a","agents":[{"id":"a","system_prompt":"A."},{"id":"b","system_prompt":"B."}]}`)
	first, _ := factory.BackendFor(spec.Agents[0], spec)
	second, _ := factory.BackendFor(spec.Agents[1], spec)
	a, _ := first.(agentflow.PromptBackend)
	b, _ := second.(agentflow.PromptBackend)
	finding(t, "S7 StaticBackendFactory.BackendFor gives agents a and b the same Completer: %t; AgentSpec has a model, profile or backend field: %t", a.Completer == b.Completer && a.Completer == agentflow.JSONCompleter(completer), hasField(agentflow.AgentSpec{}, "model", "profile", "backend"))
}

type noCompleter struct{}

func (*noCompleter) CompleteJSON(context.Context, string, string, any) error {
	return errors.New("not used")
}

// hasField reports whether v's JSON names any of names.
func hasField(v any, names ...string) bool {
	raw, _ := json.Marshal(v)
	var fields map[string]any
	_ = json.Unmarshal(raw, &fields)
	for _, name := range names {
		if _, ok := fields[name]; ok {
			return true
		}
	}
	return false
}

// S8: the review's three gaps in internal/workflow's planner, worker and
// verifier engine.
func TestS8WorkflowPackage(t *testing.T) {
	newEngine := func(t *testing.T, planner workflow.Planner, worker workflow.Worker, verifier workflow.Verifier) *workflow.Engine {
		root := t.TempDir()
		k, _ := kernel.New(filepath.Join(root, "kernel"))
		store, _ := artifact.New(filepath.Join(root, "artifacts"))
		return &workflow.Engine{Kernel: k, Artifacts: store, Planner: planner, Worker: worker, Verifier: verifier, Verification: workflow.VerificationRequired, MaxWorkerAttempts: 2}
	}
	parent := func(e *workflow.Engine) string {
		p, _ := e.Kernel.Spawn(kernel.ProcessSpec{Role: "probe"})
		return p.PID
	}
	empty := newEngine(t, planner{}, &both{approve: true}, &both{approve: true})
	summary, err := empty.Run(context.Background(), parent(empty), "goal")
	finding(t, "S8 an empty plan under required: %d results, err=%v", len(summary.Results), err)
	self := &both{approve: true}
	same := newEngine(t, planner{contracts: 1}, self, self)
	summary, err = same.Run(context.Background(), parent(same), "goal")
	finding(t, "S8 one object as worker and verifier: err=%v; accepted=%t approved=%t; it verified its own output: %t", err, summary.Results[0].Accepted, summary.Results[0].Approved, self.verified > 0 && self.executed > 0)
	rejecting := newEngine(t, planner{contracts: 1}, &both{}, &both{approve: false})
	summary, err = rejecting.Run(context.Background(), parent(rejecting), "goal")
	finding(t, "S8 a verifier that rejects every attempt under required: err=%v; accepted=%t approved=%t attempts=%d", err, summary.Results[0].Accepted, summary.Results[0].Approved, summary.Results[0].Attempts)
}

type planner struct{ contracts int }

func (p planner) Plan(context.Context, string) (workflow.Plan, error) {
	plan := workflow.Plan{Goal: "goal"}
	for i := 0; i < p.contracts; i++ {
		plan.Contracts = append(plan.Contracts, workflow.TaskContract{ID: fmt.Sprintf("c%d", i+1), Objective: "do it"})
	}
	return plan, nil
}

// both is a worker and a verifier.
type both struct {
	approve            bool
	executed, verified int
}

func (b *both) Execute(context.Context, workflow.TaskContract, int) (workflow.WorkerOutput, error) {
	b.executed++
	return workflow.WorkerOutput{Summary: "done"}, nil
}

func (b *both) Verify(context.Context, workflow.TaskContract, workflow.WorkerOutput) (workflow.Verification, error) {
	b.verified++
	return workflow.Verification{Approved: b.approve, Reason: "checked"}, nil
}

// S9: the verification block.
func TestS9Verification(t *testing.T) {
	agents := `"agents":[
		{"id":"lead","system_prompt":"Lead.","allowed_agents":["writer","checker"],"capabilities":{"tools":["spawn_agent","write_file","read_file","publish_artifact","read_artifact"]}},
		{"id":"writer","system_prompt":"Write.","capabilities":{"tools":["write_file"]}},
		{"id":"checker","system_prompt":"Check.","max_steps":3,"capabilities":{"tools":["read_file","read_artifact"]}},
		{"id":"reporter","system_prompt":"Report.","capabilities":{"tools":["read_file","publish_artifact"]}},
		{"id":"loner","system_prompt":"Alone."}]`
	cases := []struct{ name, verification string }{
		{"verifier not an agent", `{"policy":"required","verifier":"ghost"}`},
		{"verifier unreachable", `{"policy":"required","verifier":"loner"}`},
		{"verifier is the worker it judges", `{"policy":"required","verifier":"writer"}`},
		{"evidence from a publisher that never runs", `{"policy":"required","verifier":"checker","evidence":[{"publisher":"reporter","type":"report"}]}`},
		{"verifier never delegated", `{"policy":"required","verifier":"checker"}`},
	}
	for _, c := range cases {
		spec := decode(t, `{"name":"s9","entry_agent":"lead","verification":`+c.verification+`,`+agents+`}`)
		validate := errText(spec.Validate())
		if spec.Validate() != nil {
			finding(t, "S9 %-42s Validate %s", c.name+":", validate)
			continue
		}
		order := map[string][]string{"lead": {"writer", "checker"}}
		if c.name == "verifier is the worker it judges" {
			order["lead"] = []string{"writer"}
		}
		if c.name == "verifier never delegated" {
			order["lead"] = []string{"writer"}
		}
		o := run(t, spec, capability.Budget{}, exitsAfterDelegating(order))
		finding(t, "S9 %-42s Validate %s; run: %s; steps %v", c.name+":", validate, o, sortedSteps(o.steps))
	}
}

// refused reports whether a capability refusal of action was recorded.
func refused(k *kernel.Kernel, action string) bool {
	events, _ := k.Events()
	for _, event := range events {
		if strings.Contains(event.Type, "denied") && strings.Contains(fmt.Sprint(event.Data), action) {
			return true
		}
	}
	return false
}

func sortedSteps(steps map[string]int) string {
	var parts []string
	for id, n := range steps {
		parts = append(parts, fmt.Sprintf("%s=%d", id, n))
	}
	sort.Strings(parts)
	return strings.Join(parts, " ")
}

// S10: the checked-in workflows.
func TestS10CheckedInWorkflows(t *testing.T) {
	paths, _ := filepath.Glob("../../workflows/*.json")
	sort.Strings(paths)
	for _, path := range paths {
		raw, _ := os.ReadFile(path)
		var spec agentflow.WorkflowSpec
		decodeErr := json.Unmarshal(raw, &spec)
		strict := json.NewDecoder(strings.NewReader(string(raw)))
		strict.DisallowUnknownFields()
		var strictSpec agentflow.WorkflowSpec
		strictErr := strict.Decode(&strictSpec)
		name := filepath.Base(path)
		if decodeErr != nil {
			finding(t, "S10 %-40s does not decode: %v", name, decodeErr)
			continue
		}
		missing := missingPaths(spec)
		reachable := reachableFrom(spec)
		var unreachable []string
		for _, agent := range spec.Agents {
			if !reachable[agent.ID] {
				unreachable = append(unreachable, agent.ID)
			}
		}
		order := map[string][]string{}
		for _, agent := range spec.Agents {
			order[agent.ID] = agent.AllowedAgents
		}
		const cap = 40
		fn := func(ctx context.Context, agent string, n, step int, req runtime.GenerateRequest) runtime.GenerateResult {
			if n >= cap {
				return exitWith(0, "done")
			}
			return exitsAfterDelegating(order)(ctx, agent, n, step, req)
		}
		o := run(t, spec, capability.Budget{}, fn)
		finding(t, "S10 %-40s Validate %s; strict decode: %s; %d agents, unreachable %v; host paths that do not exist here: %v; scripted run (each agent delegates to its allowed agents in order, then exits 0): %s",
			name, errText(spec.Validate()), errText(strictErr), len(spec.Agents), unreachable, missing, o)
	}
}

// missingPaths are the spec's host paths that do not exist on this machine.
func missingPaths(spec agentflow.WorkflowSpec) []string {
	seen := map[string]bool{}
	var missing []string
	check := func(path string) {
		if path == "" || seen[path] || !filepath.IsAbs(path) {
			return
		}
		seen[path] = true
		if _, err := os.Stat(path); err != nil {
			missing = append(missing, path)
		}
	}
	for _, root := range spec.SkillRoots {
		check(root)
	}
	for _, agent := range spec.Agents {
		for _, mount := range agent.Mounts {
			check(mount.HostPath)
		}
		for _, path := range append(append([]string(nil), agent.Capabilities.ReadPaths...), agent.Capabilities.WritePaths...) {
			check(path)
		}
	}
	return missing
}

func reachableFrom(spec agentflow.WorkflowSpec) map[string]bool {
	seen := map[string]bool{spec.EntryAgent: true}
	queue := []string{spec.EntryAgent}
	for len(queue) > 0 {
		agent, _ := spec.Agent(queue[0])
		queue = queue[1:]
		for _, next := range agent.AllowedAgents {
			if !seen[next] {
				seen[next] = true
				queue = append(queue, next)
			}
		}
	}
	return seen
}
