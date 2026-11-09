package agentflow

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"aos/internal/capability"
)

// The preflight names each host path that cannot work here, and passes a
// writable mount whose parent exists, which the run creates.
func TestPreflightNamesMissingHostPaths(t *testing.T) {
	root := t.TempDir()
	spec := WorkflowSpec{Name: "w", Agents: []AgentSpec{{ID: "a",
		Mounts: []capability.Mount{
			{Name: "ref", HostPath: filepath.Join(root, "missing-ref"), ReadOnly: true},
			{Name: "out", HostPath: filepath.Join(root, "new-out")},
			{Name: "deep", HostPath: filepath.Join(root, "no-parent", "out")},
		},
		Capabilities: capability.Set{ReadPaths: []string{root, filepath.Join(root, "missing-read")}}}}}
	err := PreflightHostPaths(spec, []string{filepath.Join(root, "skills")})
	if !errors.Is(err, ErrPreflight) {
		t.Fatalf("err %v", err)
	}
	for _, want := range []string{
		"workflow w names host paths that do not exist on this machine; nothing was run:",
		"skill_roots[0]: " + filepath.Join(root, "skills") + " does not exist",
		`agents[0] (a).mounts[0] "ref", read-only: ` + filepath.Join(root, "missing-ref") + " does not exist",
		`agents[0] (a).mounts[2] "deep", writable: neither ` + filepath.Join(root, "no-parent", "out") + " nor its parent exists",
		"agents[0] (a).capabilities.read_paths[1]: " + filepath.Join(root, "missing-read") + " does not exist",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("%v\nlacks %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "new-out") || strings.Contains(err.Error(), "read_paths[0]") {
		t.Fatalf("a path that works was named: %v", err)
	}
	spec.Agents[0].Mounts[0].HostPath = "__RUN_DIR__/ref"
	if err := PreflightHostPaths(spec, nil); !strings.Contains(err.Error(), "placeholder such as __RUN_DIR__") {
		t.Fatalf("placeholder hint: %v", err)
	}
}
