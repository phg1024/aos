package agentflow_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"aos/internal/agentflow"
	"aos/internal/artifact"
	"aos/internal/capability"
	"aos/internal/kernel"
	"aos/internal/runtime"
	"aos/internal/tools"
)

// refusedByTrust are the checked-in workflows whose verifier compares
// against a baseline the writer and benchmarker write. Their trusted paths
// say so, and spec contract v2 refuses them until the verifier keeps a
// baseline of its own, as pathtracer-bunny-metal's does.
var refusedByTrust = map[string]bool{"rasterizer-bunny.json": true, "rasterizer-team.json": true, "rasterizer-optimized-tail.json": true}

// The checked-in workflows decode strictly and meet the spec contract, but
// the three refusedByTrust, which v2 refuses for their baseline.
func TestCheckedInWorkflowsMeetTheContract(t *testing.T) {
	paths, _ := filepath.Glob("../../workflows/*.json")
	if len(paths) < 14 {
		t.Fatalf("found %d workflows", len(paths))
	}
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		spec, err := agentflow.DecodeWorkflowSpec(raw)
		if err == nil {
			err = spec.Validate()
		}
		switch name := filepath.Base(path); {
		case refusedByTrust[name]:
			if err == nil || !strings.Contains(err.Error(), "can be written by writer, benchmarker: what the verifier trusts must be out of every other agent's write scope") {
				t.Errorf("%s: %v", name, err)
			}
			if spec.ValidateContractV1() != nil {
				t.Errorf("%s: spec contract v1 refuses it too", name)
			}
		case err != nil:
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Strict decoding names the field's path and the nearest known name.
func TestDecodeWorkflowSpecRefusesUnknownFields(t *testing.T) {
	cases := []struct{ spec, want string }{
		{`{"name":"w","entry_agent":"a","verifcation":{},"agents":[{"id":"a","system_prompt":"A."}]}`, "verifcation: unknown field; did you mean verification?"},
		{`{"name":"w","entry_agent":"a","agents":[{"id":"a","system_prompt":"A."},{"id":"b","system_prompt":"B.","max_step":3}]}`, "agents[1].max_step: unknown field; did you mean max_steps?"},
		{`{"name":"w","entry_agent":"a","agents":[{"id":"a","system_prompt":"A.","capabilites":{}}]}`, "agents[0].capabilites: unknown field; did you mean capabilities?"},
		{`{"name":"w","entry_agent":"a","agents":[{"id":"a","system_prompt":"A.","allowed_agent":["a"]}]}`, "agents[0].allowed_agent: unknown field; did you mean allowed_agents?"},
		{`{"name":"w","entry_agent":"a","agents":[{"id":"a","system_prompt":"A.","capabilities":{"tool":["run"]}}]}`, "agents[0].capabilities.tool: unknown field; did you mean tools?"},
		{`{"name":"w","entry_agent":"a","agents":[{"id":"a","system_prompt":"A.","mounts":[{"name":"m","host_path":"/x","readonly":true}]}]}`, "agents[0].mounts[0].readonly: unknown field; did you mean read_only?"},
		{`{"name":"w","entry_agent":"a","agents":[{"id":"a","system_prompt":"A.","restart":{"totl":2}}]}`, "agents[0].restart.totl: unknown field; did you mean total?"},
		{`{"name":"w","entry_agent":"a","recall":{"no_agent_indx":true},"agents":[{"id":"a","system_prompt":"A."}]}`, "recall.no_agent_indx: unknown field; did you mean no_agent_index?"},
		{`{"name":"w","entry_agent":"a","verification":{"policy":"required","verifier":"a","evidence":[{"publisher":"a","kind":"x"}]},"agents":[{"id":"a","system_prompt":"A."}]}`, "verification.evidence[0].kind: unknown field; known fields here: publisher, type, from_file"},
		{`{"name":"w","entry_agent":"a","agents":[{"id":"a","system_prompt":"A.","model":"claude-sonnet-5-5"}]}`, "agents[0].model: per-agent models are not supported yet"},
		{`{"name":"w","entry_agent":"a","agents":[{"id":"a","system_prompt":"A.","max_steps":0}]}`, "agents[0].max_steps: 0 is not a step limit; leave max_steps out for the default of 24, or give 1 to 1000"},
	}
	for _, c := range cases {
		_, err := agentflow.DecodeWorkflowSpec([]byte(c.spec))
		if !errors.Is(err, agentflow.ErrWorkflowSpecInvalid) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s:\n got %v\nwant %q", c.spec, err, c.want)
		}
	}
	// encoding/json matches field names ignoring case; so does the check.
	if _, err := agentflow.DecodeWorkflowSpec([]byte(`{"name":"w","Entry_Agent":"a","agents":[{"id":"a","system_prompt":"A."}]}`)); err != nil {
		t.Fatal(err)
	}
}

// Each rule of the spec contract, with the error it gives.
func TestValidateRefusesWhatTheContractForbids(t *testing.T) {
	const grants = `"delegable":{"tools":["write_file","read_file","run","publish_artifact","spawn_agent"],"write_paths":["/"],"unconfined_run":true}`
	const lead = `{"id":"lead","system_prompt":"L.","allowed_agents":["writer","checker"],"capabilities":{"tools":["spawn_agent"]},` + grants + `}`
	const writer = `{"id":"writer","system_prompt":"W.","capabilities":{"tools":["write_file"],"write_paths":["/work"]}}`
	const checker = `{"id":"checker","system_prompt":"C.","capabilities":{"tools":["read_file"]}}`
	required := func(verifier string) string {
		return `"verification":{"policy":"required","verifier":"` + verifier + `"},`
	}
	cases := []struct{ name, spec, want string }{
		{"max_steps negative", `{"name":"w","entry_agent":"a","agents":[{"id":"a","system_prompt":"A.","max_steps":-5}]}`,
			"agents[0] (a): max_steps -5 is out of range; give 1 to 1000, or leave it out for the default of 24"},
		{"max_steps past the ceiling", `{"name":"w","entry_agent":"a","agents":[{"id":"a","system_prompt":"A.","max_steps":1001}]}`, "max_steps 1001 is out of range"},
		{"edges without spawn_agent", `{"name":"w","entry_agent":"a","agents":[{"id":"a","system_prompt":"A.","allowed_agents":["b"]},{"id":"b","system_prompt":"B."}]}`,
			`agents[0] (a): allowed_agents [b] needs the spawn_agent tool, without which every delegation is refused at run time`},
		{"spawn_agent without edges", `{"name":"w","entry_agent":"a","agents":[{"id":"a","system_prompt":"A.","capabilities":{"tools":["spawn_agent"]}}]}`,
			"agents[0] (a): holds spawn_agent with no allowed_agents, so it can delegate to nobody"},
		{"unreachable agent", `{"name":"w","entry_agent":"lead","agents":[` + lead + `,` + writer + `,` + checker + `,{"id":"loner","system_prompt":"L."}]}`,
			`agent "loner" is not reachable from entry agent "lead" through allowed_agents; reachable: lead, lead -> writer, lead -> checker`},
		{"self-delegation", `{"name":"w","entry_agent":"a","agents":[{"id":"a","system_prompt":"A.","allowed_agents":["a"],"capabilities":{"tools":["spawn_agent"]}}]}`,
			"delegation cycle a -> a: a cycle needs max_delegation_depth (1 to 100) to bound how deep delegations nest, and the run then needs a token budget or a strict time budget to stop it"},
		{"a cycle", `{"name":"w","entry_agent":"a","agents":[{"id":"a","system_prompt":"A.","allowed_agents":["b"],"capabilities":{"tools":["spawn_agent"]}},{"id":"b","system_prompt":"B.","allowed_agents":["a"],"capabilities":{"tools":["spawn_agent"]}}]}`,
			"delegation cycle a -> b -> a"},
		{"depth out of range", `{"name":"w","entry_agent":"a","max_delegation_depth":101,"agents":[{"id":"a","system_prompt":"A."}]}`,
			"max_delegation_depth 101 is out of range; give 1 to 100, or leave it out for no limit"},
		{"evidence from an unreachable publisher", `{"name":"w","entry_agent":"lead","verification":{"policy":"required","verifier":"checker","evidence":[{"publisher":"reporter","type":"report"}]},"agents":[` + lead + `,` + writer + `,` + checker + `,{"id":"reporter","system_prompt":"R.","capabilities":{"tools":["publish_artifact"]}}]}`,
			`verification.evidence[0]: publisher "reporter" is not reachable from entry agent "lead" through allowed_agents, so the evidence can never be published`},
		{"the verifier is the only writer", `{"name":"w","entry_agent":"lead",` + required("writer") + `"agents":[{"id":"lead","system_prompt":"L.","allowed_agents":["writer"],"capabilities":{"tools":["spawn_agent"]},` + grants + `},` + writer + `]}`,
			`verifier "writer" can change the work it judges: it is the only agent that can change the work, so it would judge its own. A verifier may hold`},
		{"the verifier writes where the request points", `{"name":"w","entry_agent":"lead",` + required("checker") + `"agents":[` + lead + `,` + writer + `,{"id":"checker","system_prompt":"C.","capabilities":{"tools":["write_file"]}}]}`,
			"it holds write_file with no write_paths, so it writes where the run request points, which is where the work goes"},
		{"the verifier's write paths overlap the work", `{"name":"w","entry_agent":"lead",` + required("checker") + `"agents":[` + lead + `,` + writer + `,{"id":"checker","system_prompt":"C.","capabilities":{"tools":["run"],"write_paths":["/work/out"]}}]}`,
			"its write path /work/out overlaps /work, which writer can write"},
		{"the verifier runs unconfined", `{"name":"w","entry_agent":"lead",` + required("checker") + `"agents":[` + lead + `,` + writer + `,{"id":"checker","system_prompt":"C.","capabilities":{"tools":["run"],"write_paths":["/check"],"unconfined_run":true}}]}`,
			"it holds run with unconfined_run, which can write anywhere"},
		{"the verifier delegates to a writer", `{"name":"w","entry_agent":"lead",` + required("checker") + `"agents":[{"id":"lead","system_prompt":"L.","allowed_agents":["checker"],"capabilities":{"tools":["spawn_agent","write_file"],"write_paths":["/work"]},` + grants + `},{"id":"checker","system_prompt":"C.","allowed_agents":["writer"],"capabilities":{"tools":["spawn_agent"]},"delegable":{"tools":["write_file"],"write_paths":["/work"]}},` + writer + `]}`,
			"it can delegate to writer, which can change the work"},
	}
	for _, c := range cases {
		spec, err := agentflow.DecodeWorkflowSpec([]byte(c.spec))
		if err == nil {
			err = spec.Validate()
		}
		if !errors.Is(err, agentflow.ErrWorkflowSpecInvalid) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s:\n got %v\nwant %q", c.name, err, c.want)
		}
	}
	// A verifier with write paths of its own, apart from the work, may run
	// and write there, as the rasterizer workflows' verifiers do.
	// Under a required gate it names what it trusts, and no agent is
	// unconfined.
	confined := strings.Replace(lead, `,"unconfined_run":true`, "", 1)
	ok, err := agentflow.DecodeWorkflowSpec([]byte(`{"name":"w","entry_agent":"lead","verification":{"policy":"required","verifier":"checker","trusted":["/reference"]},"agents":[` + confined + `,` + writer + `,{"id":"checker","system_prompt":"C.","capabilities":{"tools":["run","write_file"],"write_paths":["/work-verification"]}}]}`))
	if err != nil || ok.Validate() != nil {
		t.Fatalf("a verifier writing apart from the work: %v %v", err, ok.Validate())
	}
}

// A cycle with a depth limit validates, but runs only with a budget that
// can stop it, and the engine refuses a delegation past the depth.
func TestACycleNeedsADepthAndABudget(t *testing.T) {
	spec, err := agentflow.DecodeWorkflowSpec([]byte(`{"name":"w","entry_agent":"a","max_delegation_depth":3,"agents":[
		{"id":"a","system_prompt":"A.","allowed_agents":["b"],"capabilities":{"tools":["spawn_agent"]}},
		{"id":"b","system_prompt":"B.","allowed_agents":["a"],"capabilities":{"tools":["spawn_agent"]}}]}`))
	if err != nil || spec.Validate() != nil {
		t.Fatalf("%v %v", err, spec.Validate())
	}
	for _, budget := range []capability.Budget{{}, {Seconds: 60}} {
		if spec.CanStopCycle(budget) {
			t.Fatalf("%+v should not stop a cycle", budget)
		}
	}
	for _, budget := range []capability.Budget{{Tokens: 1000}, {Seconds: 60, StrictTime: true}} {
		if !spec.CanStopCycle(budget) {
			t.Fatalf("%+v should stop a cycle", budget)
		}
	}
	f := newFixture(t)
	engine := agentflow.Engine{Kernel: f.kernel, Artifacts: f.store, Tools: f.tools, Factory: &cycleFactory{}}
	if _, err := engine.Run(context.Background(), spec, agentflow.RunRequest{Goal: "go"}); !errors.Is(err, agentflow.ErrUnboundedDelegation) ||
		!strings.Contains(err.Error(), "a -> b -> a, with max_delegation_depth 3, can still start a process per step at every level; give the run a token budget (aos run --token-budget N) or a strict time budget (--time-budget D --strict-time-budget)") {
		t.Fatalf("no budget: %v", err)
	}
	processes, _ := f.kernel.Processes()
	if len(processes) != 0 {
		t.Fatalf("nothing should be spawned without a budget: %d processes", len(processes))
	}
	factory := &cycleFactory{}
	engine = agentflow.Engine{Kernel: f.kernel, Artifacts: f.store, Tools: f.tools, Factory: factory, Budget: capability.Budget{Tokens: 1_000_000}}
	summary, err := engine.Run(context.Background(), spec, agentflow.RunRequest{Goal: "go"})
	if err != nil || summary.ExitStatus.Code != 0 {
		t.Fatalf("with a budget: %+v %v", summary.ExitStatus, err)
	}
	// The entry agent and three levels below it; the fourth delegation is
	// refused, and its delegator is told.
	if factory.spawned != 4 || !strings.Contains(factory.refused, "delegation refused: b -> a would start a process 4 delegations deep, past the workflow's max_delegation_depth of 3") {
		t.Fatalf("spawned %d, refused delegation %q", factory.spawned, factory.refused)
	}
}

type contractFixture struct {
	kernel *kernel.Kernel
	store  *artifact.Store
	tools  *tools.Runtime
}

func newFixture(t *testing.T) contractFixture {
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
	return contractFixture{kernel: k, store: store, tools: tools.NewRuntime()}
}

// cycleFactory's agents delegate to the other agent of a <-> b on their
// first step and exit on their next, keeping the last delegate result
// their history shows.
type cycleFactory struct {
	mu      sync.Mutex
	spawned int
	refused string
}

func (f *cycleFactory) BackendFor(agent agentflow.AgentSpec, _ agentflow.WorkflowSpec) (runtime.Backend, error) {
	f.mu.Lock()
	f.spawned++
	f.mu.Unlock()
	other := map[string]string{"a": "b", "b": "a"}[agent.ID]
	steps := 0
	return backendFunc(func(_ context.Context, request runtime.GenerateRequest) (runtime.GenerateResult, error) {
		steps++
		if steps == 1 {
			return runtime.GenerateResult{Action: runtime.GenerateAction{Type: "delegate", Delegate: &runtime.DelegateCall{AgentID: other, Goal: "go on"}}}, nil
		}
		for i := len(request.History) - 1; i >= 0; i-- {
			if strings.Contains(request.History[i].Content, "delegation refused") {
				f.mu.Lock()
				f.refused = request.History[i].Content
				f.mu.Unlock()
				break
			}
		}
		return runtime.GenerateResult{Action: runtime.GenerateAction{Type: "exit", ExitStatus: &kernel.ExitStatus{Code: 0, Reason: "done"}}}, nil
	}), nil
}

type backendFunc func(context.Context, runtime.GenerateRequest) (runtime.GenerateResult, error)

func (f backendFunc) Generate(ctx context.Context, request runtime.GenerateRequest) (runtime.GenerateResult, error) {
	return f(ctx, request)
}
