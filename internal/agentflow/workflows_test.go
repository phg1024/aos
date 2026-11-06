package agentflow_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"aos/internal/agentflow"
	"aos/internal/capability"
	"aos/internal/workflow"
)

// Every checked-in workflow must validate, and the ones with a verifier
// role must gate on it.
func TestCheckedInWorkflowsValidate(t *testing.T) {
	gated := map[string]string{
		"rasterizer-team.json":                   "verifier",
		"rasterizer-optimized-tail.json":         "verifier",
		"rasterizer-bunny.json":                  "verifier",
		"rasterizer-bunny-metal.json":            "verifier",
		"rasterizer-bunny-metal-ipc.json":        "verifier",
		"rasterizer-bunny-cuda-ipc.json":         "verifier",
		"rasterizer-bunny-cuda-handles-ipc.json": "verifier",
		"pathtracer-bunny-metal.json":            "verifier",
		"live-reviewed-merge.json":               "review_partner",
	}
	paths, err := filepath.Glob(filepath.Join("..", "..", "workflows", "*.json"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no workflows found: %v", err)
	}
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		var spec agentflow.WorkflowSpec
		if err := json.Unmarshal(raw, &spec); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		if err := spec.Validate(); err != nil {
			t.Fatalf("%s does not validate: %v", path, err)
		}
		policy, _ := spec.VerificationPolicy()
		if verifier, ok := gated[filepath.Base(path)]; ok {
			if policy != workflow.VerificationRequired || spec.Verification.Verifier != verifier {
				t.Fatalf("%s should require verification by %s, has %+v", path, verifier, spec.Verification)
			}
		}
	}
}

// In the gated rasterizer workflows the verifier works in its own area: no
// other agent can write where it writes, and it can write nothing where
// the others work. With run confined, this holds for run as well.
func TestRasterizerVerifiersAreIsolated(t *testing.T) {
	for _, name := range []string{"rasterizer-team.json", "rasterizer-optimized-tail.json", "rasterizer-bunny.json", "rasterizer-bunny-metal.json", "rasterizer-bunny-metal-ipc.json", "rasterizer-bunny-cuda-ipc.json", "pathtracer-bunny-metal.json"} {
		raw, err := os.ReadFile(filepath.Join("..", "..", "workflows", name))
		if err != nil {
			t.Fatal(err)
		}
		var spec agentflow.WorkflowSpec
		if err := json.Unmarshal(raw, &spec); err != nil {
			t.Fatal(err)
		}
		verifier, _ := spec.Agent(spec.Verification.Verifier)
		_, verifierWrites, _ := capability.RunScopes(verifier.Capabilities, verifier.Mounts)
		if len(verifierWrites) != 1 || !strings.HasSuffix(verifierWrites[0], "-verification") {
			t.Fatalf("%s: verifier writes %v, want only its verification directory", name, verifierWrites)
		}
		for _, agent := range spec.Agents {
			if agent.ID == verifier.ID {
				continue
			}
			_, writes, _ := capability.RunScopes(agent.Capabilities, agent.Mounts)
			for _, write := range writes {
				for _, own := range verifierWrites {
					if capability.Within(write, own) || capability.Within(own, write) {
						t.Fatalf("%s: %s can write %s, overlapping the verifier's %s", name, agent.ID, write, own)
					}
				}
			}
		}
	}
	raw, _ := os.ReadFile(filepath.Join("..", "..", "workflows", "terminal-bench.json"))
	var bench agentflow.WorkflowSpec
	json.Unmarshal(raw, &bench)
	if solver, _ := bench.Agent(bench.EntryAgent); !solver.Capabilities.UnconfinedRun || solver.Capabilities.MaxRunSeconds != 900 {
		t.Fatalf("terminal-bench runs in a disposable container and opts in to unconfined run: %+v", solver.Capabilities)
	}
	// The solver prompt describes the budget notes the runtime gives, not
	// the clock notes it no longer does.
	solver, _ := bench.Agent(bench.EntryAgent)
	for _, stale := range []string{"clock note", "20%"} {
		if strings.Contains(solver.SystemPrompt, stale) {
			t.Fatalf("the solver prompt still says %q", stale)
		}
	}
	for _, note := range []string{"budget time=", "(strict: the run is stopped when it runs out)", "budget steps=", "budget warning"} {
		if !strings.Contains(solver.SystemPrompt, note) {
			t.Fatalf("the solver prompt does not describe %q", note)
		}
	}
}

// The mail variant of the Metal workflow differs from it in its mail tools, step limits and
// prompts: no agent gains a file path, a network, a skill or another tool, and no address or
// home directory is in the spec.
func TestTheIPCMetalWorkflowWidensNothing(t *testing.T) {
	load := func(name string) (agentflow.WorkflowSpec, string) {
		raw, err := os.ReadFile(filepath.Join("..", "..", "workflows", name))
		if err != nil {
			t.Fatal(err)
		}
		var spec agentflow.WorkflowSpec
		if err := json.Unmarshal(raw, &spec); err != nil {
			t.Fatal(err)
		}
		return spec, string(raw)
	}
	base, _ := load("rasterizer-bunny-metal.json")
	variant, raw := load("rasterizer-bunny-metal-ipc.json")
	for _, absent := range []string{"/Users/", "http://", "127.0.0.1"} {
		if strings.Contains(raw, absent) {
			t.Fatalf("the spec holds %q", absent)
		}
	}
	mapped := func(path string) string {
		for _, pair := range [][2]string{
			{"/Users/claw/Code/aos/reports/rasterizer-bunny-metal-verification", "__RUN_DIR__/metal-verification"},
			{"/Users/claw/Code/aos/reports/rasterizer-bunny-metal", "__RUN_DIR__/metal-workspace"},
			{"/Users/claw/Code/aos", "__AOS_ROOT__"},
		} {
			if rest, ok := strings.CutPrefix(path, pair[0]); ok {
				return pair[1] + rest
			}
		}
		return path
	}
	extra := map[string][]string{
		"coordinator": {"message_agent", "publish_artifact", "list_artifacts"},
		"writer":      {"message_agent"},
		"benchmarker": {"message_agent", "publish_artifact"},
		"verifier":    {"message_agent", "list_artifacts"},
	}
	if len(variant.Agents) != len(base.Agents) {
		t.Fatalf("%d agents, want %d", len(variant.Agents), len(base.Agents))
	}
	for _, old := range base.Agents {
		agent, ok := variant.Agent(old.ID)
		if !ok {
			t.Fatalf("no %s", old.ID)
		}
		want := append(append([]string(nil), old.Capabilities.Tools...), extra[old.ID]...)
		if !slices.Equal(agent.Capabilities.Tools, want) {
			t.Errorf("%s tools = %v, want %v", old.ID, agent.Capabilities.Tools, want)
		}
		if agent.Capabilities.AllowNetwork != old.Capabilities.AllowNetwork || agent.Capabilities.UnconfinedRun || old.Capabilities.UnconfinedRun {
			t.Errorf("%s network or confinement changed", old.ID)
		}
		for name, paths := range map[string][2][]string{
			"read":   {old.Capabilities.ReadPaths, agent.Capabilities.ReadPaths},
			"write":  {old.Capabilities.WritePaths, agent.Capabilities.WritePaths},
			"dwrite": {old.Delegable.WritePaths, agent.Delegable.WritePaths},
		} {
			var want []string
			for _, path := range paths[0] {
				want = append(want, mapped(path))
			}
			if !slices.Equal(want, paths[1]) {
				t.Errorf("%s %s paths = %v, want %v", old.ID, name, paths[1], want)
			}
		}
		if !slices.Equal(agent.Delegable.Tools, old.Delegable.Tools) || !slices.Equal(agent.Skills, old.Skills) || !slices.Equal(agent.AllowedAgents, old.AllowedAgents) {
			t.Errorf("%s delegable tools, skills or allowed agents changed", old.ID)
		}
		if agent.Restart != nil || agent.MaxSteps == 0 || agent.MaxSteps > old.MaxSteps {
			t.Errorf("%s: restart %+v, max steps %d, base %d", old.ID, agent.Restart, agent.MaxSteps, old.MaxSteps)
		}
	}
}

// The CUDA variant differs from the Metal one in its job: the same agents, tools and limits, a card
// only for the roles that run GPU code, the toolchain read-only, and nothing about a machine.
func TestTheIPCCudaWorkflowGrantsOnlyWhatItNeeds(t *testing.T) {
	load := func(name string) (agentflow.WorkflowSpec, string) {
		raw, err := os.ReadFile(filepath.Join("..", "..", "workflows", name))
		if err != nil {
			t.Fatal(err)
		}
		var spec agentflow.WorkflowSpec
		if err := json.Unmarshal(raw, &spec); err != nil {
			t.Fatal(err)
		}
		return spec, string(raw)
	}
	metal, _ := load("rasterizer-bunny-metal-ipc.json")
	cuda, raw := load("rasterizer-bunny-cuda-ipc.json")
	for _, absent := range []string{"/Users/", "/home/", "http://", "10.0.", ".local", "Metal", "metal", "GPU-", "web_", "on the web", "allow_network", "unconfined_run"} {
		if strings.Contains(raw, absent) {
			t.Fatalf("the spec holds %q", absent)
		}
	}
	gpu := []string{"__GPU_UUID__"}
	wants := map[string][]string{"writer": gpu, "benchmarker": gpu, "verifier": gpu}
	worst := 0
	for _, old := range metal.Agents {
		agent, ok := cuda.Agent(old.ID)
		if !ok {
			t.Fatalf("no %s", old.ID)
		}
		if !slices.Equal(agent.Capabilities.AllowGPU, wants[old.ID]) {
			t.Errorf("%s allow_gpu = %v", old.ID, agent.Capabilities.AllowGPU)
		}
		offline := slices.DeleteFunc(slices.Clone(old.Capabilities.Tools), func(tool string) bool { return strings.HasPrefix(tool, "web_") })
		if !slices.Equal(agent.Capabilities.Tools, offline) || agent.Restart != nil {
			t.Errorf("%s changed from the Metal variant: %+v", old.ID, agent.Capabilities)
		}
		// A command with network here can reach what has no authentication on the LAN, and
		// leave the sandbox that way. Nothing can pass it on either.
		if agent.Capabilities.AllowNetwork || agent.Delegable.AllowNetwork || agent.Capabilities.UnconfinedRun || agent.Delegable.UnconfinedRun || len(agent.Capabilities.NetworkAllow) != 0 {
			t.Errorf("%s can reach the network: %+v, delegable %+v", old.ID, agent.Capabilities, agent.Delegable)
		}
		worst += agent.MaxSteps
		if old.ID != "coordinator" {
			worst += agent.MaxSteps
		}
		if len(agent.Capabilities.WritePaths) != len(old.Capabilities.WritePaths) {
			t.Errorf("%s write paths = %v", old.ID, agent.Capabilities.WritePaths)
		}
		toolchain := slices.IndexFunc(agent.Mounts, func(mount capability.Mount) bool { return mount.HostPath == "__CUDA_HOME__" })
		if toolchain < 0 || !agent.Mounts[toolchain].ReadOnly {
			t.Errorf("%s mounts = %+v", old.ID, agent.Mounts)
		}
		for _, write := range append(agent.Capabilities.WritePaths, agent.Delegable.WritePaths...) {
			if strings.HasPrefix(write, "__CUDA_HOME__") || strings.HasPrefix(write, "__AOS_ROOT__") {
				t.Errorf("%s can write %s", old.ID, write)
			}
		}
	}
	// Two rounds of three phases and the coordinator, against about 200 calls.
	if worst > 200 {
		t.Errorf("the step limits sum to %d", worst)
	}
	coordinator, _ := cuda.Agent("coordinator")
	if !slices.Equal(coordinator.Delegable.AllowGPU, gpu) {
		t.Fatalf("the coordinator cannot grant the card: %+v", coordinator.Delegable)
	}
	// The build job.sh checks first, and a launch failure is not left to look like a blank frame.
	for _, agent := range cuda.Agents {
		if !strings.Contains(agent.SystemPrompt, "nvcc -O2 -std=c++17 -arch=sm_86 -I <dir>") || !strings.Contains(agent.SystemPrompt, "check cudaGetLastError") {
			t.Errorf("%s is not told the build or the launch check", agent.ID)
		}
	}
	if strings.Contains(raw, "nvcc -O2 -std=c++17 -I") {
		t.Error("a build line leaves out the card's architecture")
	}
}
