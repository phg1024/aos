package main

import (
	"errors"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"aos/internal/agentflow"
	"aos/internal/capability"
	"aos/internal/kernel"
)

// cyclicSpec is a and b delegating to each other, with a's budget as given.
func cyclicSpec(budget capability.Budget) agentflow.WorkflowSpec {
	tools := []string{"spawn_agent"}
	return agentflow.WorkflowSpec{Name: "w", EntryAgent: "a", MaxDelegationDepth: 3, Agents: []agentflow.AgentSpec{
		{ID: "a", SystemPrompt: "A.", AllowedAgents: []string{"b"}, Capabilities: capability.Set{Tools: tools, Budget: budget}},
		{ID: "b", SystemPrompt: "B.", AllowedAgents: []string{"a"}, Capabilities: capability.Set{Tools: tools}},
	}}
}

// The TUI checks a workflow as aos run does: the contract, the host paths,
// and, with no run budget, a budget of the driven agent's own for a cycle.
func TestCheckWorkflowAsAosRunDoes(t *testing.T) {
	root := t.TempDir()
	workflowPath := filepath.Join(root, "w.json")
	unreachable := agentflow.WorkflowSpec{Name: "w", EntryAgent: "a", Agents: []agentflow.AgentSpec{
		{ID: "a", SystemPrompt: "A."}, {ID: "b", SystemPrompt: "B."},
	}}
	if err := checkWorkflow(unreachable, workflowPath, "a"); !errors.Is(err, agentflow.ErrWorkflowSpecInvalid) ||
		!strings.Contains(err.Error(), workflowPath+": ") || !strings.Contains(err.Error(), `agent "b" is not reachable from entry agent "a"`) {
		t.Fatalf("contract: %v", err)
	}
	missing := agentflow.WorkflowSpec{Name: "w", EntryAgent: "a", Agents: []agentflow.AgentSpec{
		{ID: "a", SystemPrompt: "A.", Capabilities: capability.Set{ReadPaths: []string{filepath.Join(root, "gone")}}},
	}}
	if err := checkWorkflow(missing, workflowPath, "a"); !errors.Is(err, agentflow.ErrPreflight) ||
		!strings.Contains(err.Error(), "agents[0] (a).capabilities.read_paths[0]: "+filepath.Join(root, "gone")+" does not exist") {
		t.Fatalf("preflight: %v", err)
	}
	err := checkWorkflow(cyclicSpec(capability.Budget{}), workflowPath, "a")
	if !errors.Is(err, agentflow.ErrUnboundedDelegation) ||
		!strings.Contains(err.Error(), "a -> b -> a, with max_delegation_depth 3, can still start a process per step at every level; aos-tui has no run budget, so give agent a a budget of its own in the workflow") {
		t.Fatalf("cycle: %v", err)
	}
	if err := checkWorkflow(cyclicSpec(capability.Budget{Tokens: 1000}), workflowPath, "a"); err != nil {
		t.Fatalf("a token budget stops the cycle: %v", err)
	}
	if err := checkWorkflow(cyclicSpec(capability.Budget{Seconds: 60}), workflowPath, "a"); !errors.Is(err, agentflow.ErrUnboundedDelegation) {
		t.Fatalf("an advisory time budget does not: %v", err)
	}
	// The TUI drives --agent b as its root, so b's budget is the one that
	// counts, not the entry agent's.
	if err := checkWorkflow(cyclicSpec(capability.Budget{Tokens: 1000}), workflowPath, "b"); !errors.Is(err, agentflow.ErrUnboundedDelegation) ||
		!strings.Contains(err.Error(), "b -> a -> b") || !strings.Contains(err.Error(), "give agent b a budget") {
		t.Fatalf("driven agent: %v", err)
	}
	if err := checkWorkflow(cyclicSpec(capability.Budget{}), workflowPath, "c"); err == nil || !strings.Contains(err.Error(), "agent c not found") {
		t.Fatalf("unknown agent: %v", err)
	}
}

// A resumed root keeps the budget it was started with, so a workflow that
// now gives the agent a budget does not cover a session whose root had none.
func TestCheckResumedRootUsesItsOwnBudget(t *testing.T) {
	spec := cyclicSpec(capability.Budget{Tokens: 1000})
	old := kernel.Process{PID: "p-1"}
	err := checkResumedRoot(spec, "w.json", "a", old)
	if !errors.Is(err, agentflow.ErrUnboundedDelegation) || !strings.Contains(err.Error(), "this session's a (p-1) was started without a budget that can stop it") {
		t.Fatalf("err %v", err)
	}
	old.CapabilitySet.Budget = capability.Budget{Seconds: 600, StrictTime: true}
	if err := checkResumedRoot(spec, "w.json", "a", old); err != nil {
		t.Fatalf("a strict time budget stops the cycle: %v", err)
	}
}

// aos-tui refuses a misspelled or unchecked workflow before it makes its
// state directory. The test binary runs main as aos-tui in a child.
func TestStartupRefusesBeforeTheStateDirectory(t *testing.T) {
	if args := os.Getenv("AOS_TUI_TEST_ARGS"); args != "" {
		os.Args = append([]string{"aos-tui"}, strings.Split(args, "\n")...)
		flag.CommandLine = flag.NewFlagSet("aos-tui", flag.ExitOnError)
		main()
		return
	}
	dir := t.TempDir()
	for name, c := range map[string]struct{ spec, want string }{
		"misspelled": {`{"name":"w","entry_agent":"a","agents":[{"id":"a","system_prompt":"A.","alowed_agents":["a"]}]}`,
			"agents[0].alowed_agents: unknown field; did you mean allowed_agents?"},
		"cycle": {`{"name":"w","entry_agent":"a","max_delegation_depth":2,"agents":[{"id":"a","system_prompt":"A.","allowed_agents":["a"],"capabilities":{"tools":["spawn_agent"]}}]}`,
			"a -> a, with max_delegation_depth 2, can still start a process per step at every level; aos-tui has no run budget"},
	} {
		workflowPath := filepath.Join(dir, name+".json")
		if err := os.WriteFile(workflowPath, []byte(c.spec), 0o644); err != nil {
			t.Fatal(err)
		}
		stateDir := filepath.Join(dir, name+"-state")
		cmd := exec.Command(os.Args[0], "-test.run=^TestStartupRefusesBeforeTheStateDirectory$")
		cmd.Env = append(os.Environ(), "AOS_TUI_TEST_ARGS="+strings.Join([]string{"--workflow", workflowPath, "--state-dir", stateDir, "--workdir", dir}, "\n"))
		out, err := cmd.CombinedOutput()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 || !strings.Contains(string(out), c.want) {
			t.Fatalf("%s: err %v, output:\n%s", name, err, out)
		}
		if _, statErr := os.Stat(stateDir); statErr == nil {
			t.Fatalf("%s: the refused start made its state directory", name)
		}
	}
}
