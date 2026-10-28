package agentflow_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"aos/internal/agentflow"
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
