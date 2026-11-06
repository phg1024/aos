package agentflow_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"aos/internal/agentflow"
	"aos/internal/capability"
	"aos/internal/sandbox"
	"aos/internal/tools"
)

const handlesWorkflow = "rasterizer-bunny-cuda-handles-ipc.json"

func loadHandlesWorkflow(t *testing.T, fills map[string]string) agentflow.WorkflowSpec {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "workflows", handlesWorkflow))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for placeholder, value := range fills {
		text = strings.ReplaceAll(text, placeholder, value)
	}
	var spec agentflow.WorkflowSpec
	if err := json.Unmarshal([]byte(text), &spec); err != nil {
		t.Fatal(err)
	}
	return spec
}

// live2 ran the older file, and its hash is how the report names that run.
func TestTheWorkflowLive2RanIsUnchanged(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "workflows", "rasterizer-bunny-cuda-ipc.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(raw)); got != "bf3abbc3db92a1bdddbbe61c293213339f1c13278c0214b753dbbe8d24c20676" {
		t.Fatalf("sha256 = %s", got)
	}
}

// The spec itself: only the benchmarker holds the private directory, the coordinator
// holds it only to grant it, and the budgets and the trial count are the ones decided.
func TestTheHandlesWorkflowKeepsTheRecordPrivate(t *testing.T) {
	const bench = "__RUN_DIR__/cuda-bench"
	const source = "__RUN_DIR__/cuda-workspace/cuda"
	spec := loadHandlesWorkflow(t, nil)
	if err := spec.Validate(); err != nil {
		t.Fatal(err)
	}
	steps := map[string]int{"coordinator": 45, "writer": 35, "benchmarker": 22, "verifier": 26}
	for _, agent := range spec.Agents {
		if agent.MaxSteps != steps[agent.ID] {
			t.Errorf("%s max_steps = %d", agent.ID, agent.MaxSteps)
		}
		holds := slices.Contains(agent.Capabilities.ReadPaths, bench) || slices.Contains(agent.Capabilities.WritePaths, bench)
		if holds != (agent.ID == "benchmarker") {
			t.Errorf("%s: holds the private directory: %v", agent.ID, holds)
		}
		if agent.Capabilities.UnconfinedRun || agent.Delegable.UnconfinedRun || agent.Capabilities.AllowNetwork || agent.Delegable.AllowNetwork {
			t.Errorf("%s is not confined: %+v", agent.ID, agent.Capabilities)
		}
		if strings.Contains(agent.SystemPrompt, "TMPDIR") {
			t.Errorf("%s is still told about TMPDIR", agent.ID)
		}
		if !strings.Contains(agent.SystemPrompt, "nvcc -O2 -std=c++17 -arch=sm_86 -I <dir>") {
			t.Errorf("%s is not told the build", agent.ID)
		}
	}
	coordinator, _ := spec.Agent("coordinator")
	if !slices.Contains(coordinator.Delegable.ReadPaths, bench) || !slices.Contains(coordinator.Delegable.WritePaths, bench) {
		t.Errorf("the coordinator cannot grant the directory: %+v", coordinator.Delegable)
	}
	if !slices.ContainsFunc(coordinator.Mounts, func(mount capability.Mount) bool { return mount.HostPath == bench && !mount.ReadOnly }) {
		t.Errorf("without a mount of the coordinator's, the benchmarker's is dropped: %+v", coordinator.Mounts)
	}
	for _, id := range []string{"benchmarker", "verifier"} {
		agent, _ := spec.Agent(id)
		if slices.Contains(agent.Capabilities.ReadPaths, "__RUN_DIR__/cuda-workspace") || !slices.Contains(agent.Capabilities.ReadPaths, source) {
			t.Errorf("%s reads %v, not only the source", id, agent.Capabilities.ReadPaths)
		}
		if !slices.ContainsFunc(agent.Mounts, func(mount capability.Mount) bool { return mount.HostPath == source && mount.ReadOnly }) {
			t.Errorf("%s mounts = %+v", id, agent.Mounts)
		}
		if strings.Contains(strings.Join(agent.Capabilities.WritePaths, " "), "cuda-workspace") {
			t.Errorf("%s writes %v: anything there the others can read", id, agent.Capabilities.WritePaths)
		}
	}
	benchmarker, _ := spec.Agent("benchmarker")
	// Named so because run records its changes to the mount of that name, which is how
	// analyze.py says what last changed a published file.
	if benchmarker.Mounts[0].HostPath != bench || benchmarker.Mounts[0].Name != tools.WorkspaceMount || !strings.Contains(benchmarker.SystemPrompt, "at least 50 trials") {
		t.Errorf("first mount %s, or the trial count changed", benchmarker.Mounts[0].HostPath)
	}
	verifier, _ := spec.Agent("verifier")
	if verifier.Mounts[0].HostPath != "__RUN_DIR__/cuda-verification" || !strings.Contains(verifier.SystemPrompt, "Time 5 trials yourself") {
		t.Errorf("first mount %s, or the verifier's trials are missing", verifier.Mounts[0].HostPath)
	}
}

// What the engine gives each agent, as it derives it, and what a confined command and
// the file tools can then reach. The record is in the private directory, the coordinator's
// ledger beside the source. No GPU is granted here, and none is needed.
func TestTheHandlesGrantsReachOnlyWhatTheySay(t *testing.T) {
	if err := sandbox.Available(); err != nil {
		t.Skip(err)
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	run := filepath.Join(root, "run")
	reference := filepath.Join(root, "aos", "reports", "rasterizer-bunny")
	toolchain := filepath.Join(root, "toolchain")
	record := filepath.Join(run, "cuda-bench", "raw_record.txt")
	ledger := filepath.Join(run, "cuda-workspace", "cuda_coordinator_log.txt")
	sourceFile := filepath.Join(run, "cuda-workspace", "cuda", "src", "main.cpp")
	report := filepath.Join(run, "cuda-verification", "report.txt")
	for _, path := range []string{record, ledger, sourceFile, report, filepath.Join(reference, "scene.json"), filepath.Join(toolchain, "bin", "nvcc")} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("marker-"+filepath.Base(path)+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	spec := loadHandlesWorkflow(t, map[string]string{"__RUN_DIR__": run, "__AOS_ROOT__": filepath.Join(root, "aos"), "__CUDA_HOME__": toolchain, "__GPU_UUID__": "none"})
	coordinator, _ := spec.Agent("coordinator")
	grantable := capability.Union(coordinator.Capabilities, coordinator.Delegable)
	type grant struct {
		set    capability.Set
		mounts []capability.Mount
	}
	derived := map[string]grant{"coordinator": {coordinator.Capabilities, coordinator.Mounts}}
	for _, id := range []string{"writer", "benchmarker", "verifier"} {
		agent, _ := spec.Agent(id)
		set := capability.DeriveChildSet(grantable, agent.Capabilities)
		set.AllowGPU = nil
		derived[id] = grant{set, capability.AttenuateMounts(coordinator.Mounts, agent.Mounts)}
	}
	if got := derived["benchmarker"].mounts; len(got) != 4 || got[0].HostPath != filepath.Dir(record) {
		t.Fatalf("the benchmarker's mounts after attenuation: %+v", got)
	}
	toolRuntime := tools.NewRuntime()
	toolRuntime.ScratchRoot = filepath.Join(root, "scratch")
	execute := func(id, tool string, args map[string]string) (tools.Result, error) {
		g := derived[id]
		return toolRuntime.ExecuteContext(context.Background(), tools.Request{Tool: tool, Args: args, CapabilitySet: g.set, Mounts: g.mounts, PID: "pid-" + id})
	}
	for _, test := range []struct {
		agent string
		path  string
		read  bool
	}{
		{"benchmarker", record, true}, {"benchmarker", sourceFile, true}, {"benchmarker", ledger, false}, {"benchmarker", report, false},
		{"verifier", record, false}, {"verifier", sourceFile, true}, {"verifier", ledger, false}, {"verifier", report, true},
		{"writer", record, false}, {"writer", sourceFile, true}, {"writer", ledger, true}, {"writer", report, false},
		{"coordinator", record, false}, {"coordinator", ledger, true}, {"coordinator", report, true},
	} {
		_, err := execute(test.agent, "read_file", map[string]string{"path": test.path})
		if (err == nil) != test.read {
			t.Errorf("%s read_file %s: %v", test.agent, test.path, err)
		}
		if test.agent == "coordinator" {
			continue
		}
		result, err := execute(test.agent, "run", map[string]string{"command": "cat " + test.path})
		if err != nil {
			t.Fatalf("%s run: %v", test.agent, err)
		}
		reached := strings.Contains(result.Output["stdout"], "marker-")
		if reached != test.read {
			t.Errorf("%s cat %s: reached %v: %v", test.agent, test.path, reached, result.Output)
		}
	}
	// The directory cannot be listed either, and nobody else can drop a file in it, and the
	// benchmarker can leave nothing where the others read.
	for _, test := range []struct {
		agent, command, marker string
	}{
		{"verifier", "ls " + filepath.Dir(record), "raw_record"},
		{"writer", "ls " + filepath.Dir(record), "raw_record"},
		{"verifier", "ls " + filepath.Join(run, "cuda-workspace"), "cuda_coordinator_log"},
		{"benchmarker", "ls " + filepath.Join(run, "cuda-workspace"), "cuda_coordinator_log"},
	} {
		result, err := execute(test.agent, "run", map[string]string{"command": test.command})
		if err != nil {
			t.Fatalf("%s run: %v", test.agent, err)
		}
		if strings.Contains(result.Output["stdout"], test.marker) {
			t.Errorf("%s %q listed it: %v", test.agent, test.command, result.Output)
		}
	}
	for _, target := range []string{filepath.Join(run, "cuda-workspace", "cuda", "planted"), filepath.Join(run, "cuda-workspace", "planted"), filepath.Join(run, "cuda-verification", "planted")} {
		if _, err := execute("benchmarker", "run", map[string]string{"command": "echo x > " + target}); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(target); err == nil {
			t.Errorf("the benchmarker wrote %s, where others read", target)
		}
		if _, err := execute("benchmarker", "write_file", map[string]string{"path": target, "content": "x"}); err == nil {
			t.Errorf("write_file reached %s", target)
		}
	}
	// What each needs still works: the benchmarker builds a copy, and the verifier too.
	if result, err := execute("benchmarker", "run", map[string]string{"command": "cp -R " + filepath.Dir(filepath.Dir(sourceFile)) + " " + filepath.Join(run, "cuda-bench", "build") + " && echo copied-here"}); err != nil || !strings.Contains(result.Output["stdout"], "copied-here") {
		t.Errorf("the benchmarker's copy: %v %v", result.Output, err)
	}
	if result, err := execute("verifier", "run", map[string]string{"command": "cp -R " + filepath.Dir(filepath.Dir(sourceFile)) + " " + filepath.Join(run, "cuda-verification", "build") + " && echo copied-here"}); err != nil || !strings.Contains(result.Output["stdout"], "copied-here") {
		t.Errorf("the verifier's copy: %v %v", result.Output, err)
	}
	// A confined run starts in the first mount, and the changes it records are there.
	result, err := execute("benchmarker", "run", map[string]string{"command": "pwd; echo new > fresh.txt"})
	if err != nil || !strings.Contains(result.Output["stdout"], filepath.Dir(record)) || result.Changes == nil || !slices.Contains(result.Changes.Created, "fresh.txt") {
		t.Errorf("the benchmarker's run in its first mount: %v %+v %v", result.Output, result.Changes, err)
	}
}

// live3 ran the handles workflow as it stands. The evidence workflow is its successor,
// and what it changes is its declaration, some prompt text and its step budgets: no
// process can reach anything it could not before.
func TestTheEvidenceWorkflowDeclaresAndGrantsNothingNew(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "workflows", handlesWorkflow))
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(raw)); got != "d0ab28df10b3246ef14ccfdd2accb7fa2f0157229dea078d208fd39e2c13c706" {
		t.Fatalf("the workflow live3 ran changed: %s", got)
	}
	load := func(name string) agentflow.WorkflowSpec {
		raw, err := os.ReadFile(filepath.Join("..", "..", "workflows", name))
		if err != nil {
			t.Fatal(err)
		}
		var spec agentflow.WorkflowSpec
		if err := json.Unmarshal(raw, &spec); err != nil {
			t.Fatal(err)
		}
		return spec
	}
	old, spec := load(handlesWorkflow), load("rasterizer-bunny-cuda-evidence-ipc.json")
	if err := spec.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(spec.Verification.Evidence) != 1 || spec.Verification.Evidence[0] != (agentflow.EvidenceSpec{Publisher: "benchmarker", Type: "raw_record", FromFile: true}) {
		t.Fatalf("evidence %+v", spec.Verification.Evidence)
	}
	if len(spec.Agents) != len(old.Agents) {
		t.Fatalf("%d agents", len(spec.Agents))
	}
	for i, agent := range spec.Agents {
		before := old.Agents[i]
		for name, pair := range map[string][2]any{
			"id":           {agent.ID, before.ID},
			"capabilities": {agent.Capabilities, before.Capabilities},
			"delegable":    {agent.Delegable, before.Delegable},
			"mounts":       {agent.Mounts, before.Mounts},
			"allowed":      {agent.AllowedAgents, before.AllowedAgents},
		} {
			if !reflect.DeepEqual(pair[0], pair[1]) {
				t.Errorf("%s: %s changed: %+v, was %+v", agent.ID, name, pair[0], pair[1])
			}
		}
	}
	coordinator, _ := spec.Agent("coordinator")
	for _, want := range []string{"attach that artifact whenever you delegate to the verifier", "needs it attached again", "may still pause with a wait"} {
		if !strings.Contains(coordinator.SystemPrompt, want) {
			t.Errorf("coordinator lacks %q", want)
		}
	}
	if strings.Contains(coordinator.SystemPrompt, "you can mail a paused process") {
		t.Error("the coordinator still leans on mailing a paused process")
	}
	verifier, _ := spec.Agent("verifier")
	if strings.Contains(verifier.SystemPrompt, "a record you could not read, makes the overall result FAIL") || !strings.Contains(verifier.SystemPrompt, "no reason to exit 1") {
		t.Error("the verifier is still told to fail what it could not read")
	}
	benchmarker, _ := spec.Agent("benchmarker")
	if !strings.Contains(benchmarker.SystemPrompt, "type raw_record") {
		t.Error("the benchmarker is not told the declared type")
	}
}
