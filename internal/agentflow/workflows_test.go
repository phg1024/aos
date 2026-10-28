package agentflow_test

import (
	"encoding/json"
	"os"
	"path/filepath"
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
		"rasterizer-team.json":           "verifier",
		"rasterizer-optimized-tail.json": "verifier",
		"rasterizer-bunny.json":          "verifier",
		"rasterizer-bunny-metal.json":    "verifier",
		"live-reviewed-merge.json":       "review_partner",
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
	for _, name := range []string{"rasterizer-team.json", "rasterizer-optimized-tail.json", "rasterizer-bunny.json", "rasterizer-bunny-metal.json"} {
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
}
