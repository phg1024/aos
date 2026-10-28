package agentflow

import (
	"errors"
	"fmt"
	"strings"

	"aos/internal/capability"
	"aos/internal/workflow"
)

var ErrWorkflowSpecInvalid = errors.New("invalid workflow spec")

type WorkflowSpec struct {
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	EntryAgent  string      `json:"entry_agent"`
	SkillRoots  []string    `json:"skill_roots,omitempty"`
	Agents      []AgentSpec `json:"agents"`
	// Verification gates the workflow on a verifier's recorded exit. Absent
	// means policy none: success follows the entry agent alone.
	Verification *VerificationSpec `json:"verification,omitempty"`
}

// VerificationSpec names the agent whose recorded kernel exit is the
// workflow's verdict (0 passes, anything else fails) and how it gates
// success. Policy uses the workflow package's vocabulary.
type VerificationSpec struct {
	Policy   string `json:"policy"`
	Verifier string `json:"verifier,omitempty"`
}

type AgentSpec struct {
	ID            string             `json:"id"`
	SystemPrompt  string             `json:"system_prompt"`
	AllowedAgents []string           `json:"allowed_agents,omitempty"`
	Skills        []string           `json:"skills,omitempty"`
	MaxSteps      int                `json:"max_steps,omitempty"`
	Mounts        []capability.Mount `json:"mounts,omitempty"`
	Capabilities  capability.Set     `json:"capabilities"`
	// Delegable is authority the agent may grant to agents it delegates to
	// but may not exercise itself, e.g. a coordinator without run whose
	// workers need it.
	Delegable capability.Set    `json:"delegable,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

func (s WorkflowSpec) Validate() error {
	if s.Name == "" || s.EntryAgent == "" || len(s.Agents) == 0 {
		return ErrWorkflowSpecInvalid
	}
	seen := map[string]bool{}
	hasEntry := false
	for _, agent := range s.Agents {
		if agent.ID == "" || agent.SystemPrompt == "" || seen[agent.ID] {
			return ErrWorkflowSpecInvalid
		}
		seen[agent.ID] = true
		if agent.ID == s.EntryAgent {
			hasEntry = true
		}
	}
	if !hasEntry {
		return ErrWorkflowSpecInvalid
	}
	for _, agent := range s.Agents {
		for _, delegate := range agent.AllowedAgents {
			if !seen[delegate] {
				return ErrWorkflowSpecInvalid
			}
		}
	}
	if err := s.validateDelegation(); err != nil {
		return err
	}
	return s.validateVerification(seen)
}

// validateDelegation checks every allowed_agents edge: a child may request
// no more than its parent's capabilities and delegable authority, and its
// mounts must sit inside the parent's without widening read-only ones.
// Path scopes left empty are derived from the run request at spawn, so they
// are checked at run time instead.
func (s WorkflowSpec) validateDelegation() error {
	for _, parent := range s.Agents {
		grantable := capability.Union(parent.Capabilities, parent.Delegable)
		for _, childID := range parent.AllowedAgents {
			child, _ := s.Agent(childID)
			excess := capability.Excess(grantable, withDerivedPathsAllowed(grantable, child.Capabilities))
			for _, item := range capability.Excess(grantable, withDerivedPathsAllowed(grantable, child.Delegable)) {
				excess = append(excess, "delegable "+item)
			}
			if len(parent.Mounts) > 0 && len(child.Mounts) > 0 {
				excess = append(excess, capability.MountExcess(parent.Mounts, child.Mounts)...)
			}
			if len(excess) > 0 {
				return fmt.Errorf("%w: %s -> %s exceeds what %s may grant: %s", ErrWorkflowSpecInvalid, parent.ID, childID, parent.ID, strings.Join(excess, ", "))
			}
		}
	}
	return nil
}

// withDerivedPathsAllowed drops requested path scopes that cannot be
// checked statically because the parent's own scopes come from the run
// request.
func withDerivedPathsAllowed(grantable, requested capability.Set) capability.Set {
	if len(grantable.ReadPaths) == 0 {
		requested.ReadPaths = nil
	}
	if len(grantable.WritePaths) == 0 {
		requested.WritePaths = nil
	}
	return requested
}

// VerificationPolicy is the declared policy, none when no block is given.
func (s WorkflowSpec) VerificationPolicy() (workflow.VerificationPolicy, error) {
	if s.Verification == nil {
		return workflow.VerificationNone, nil
	}
	policy, err := workflow.ParseVerificationPolicy(s.Verification.Policy)
	if err != nil {
		return "", err
	}
	if policy == "" {
		return workflow.VerificationNone, nil
	}
	return policy, nil
}

func (s WorkflowSpec) validateVerification(agents map[string]bool) error {
	policy, err := s.VerificationPolicy()
	if err != nil {
		return fmt.Errorf("%w: %v", ErrWorkflowSpecInvalid, err)
	}
	if policy == workflow.VerificationNone {
		return nil
	}
	verifier := s.Verification.Verifier
	switch {
	case verifier == "":
		return fmt.Errorf("%w: verification policy %s needs a verifier", ErrWorkflowSpecInvalid, policy)
	case !agents[verifier]:
		return fmt.Errorf("%w: verifier %q is not an agent", ErrWorkflowSpecInvalid, verifier)
	case verifier == s.EntryAgent:
		return fmt.Errorf("%w: verifier %q cannot be the entry agent", ErrWorkflowSpecInvalid, verifier)
	case !s.reachable(s.EntryAgent, verifier):
		return fmt.Errorf("%w: verifier %q is not reachable from entry agent %q through allowed_agents", ErrWorkflowSpecInvalid, verifier, s.EntryAgent)
	}
	return nil
}

// reachable reports whether from can delegate, directly or through other
// agents, to target.
func (s WorkflowSpec) reachable(from, target string) bool {
	seen := map[string]bool{from: true}
	queue := []string{from}
	for len(queue) > 0 {
		agent, _ := s.Agent(queue[0])
		queue = queue[1:]
		for _, next := range agent.AllowedAgents {
			if next == target {
				return true
			}
			if !seen[next] {
				seen[next] = true
				queue = append(queue, next)
			}
		}
	}
	return false
}

// workChangingTools can alter the work a verifier checks.
var workChangingTools = map[string]bool{"write_file": true, "edit_file": true, "run": true}

// IsWorkChanging reports whether the agent could change the work: it holds
// a writing or command tool, or declares write paths. A verdict is stale
// once such a delegated agent runs after the verifier.
func (a AgentSpec) IsWorkChanging() bool {
	if len(a.Capabilities.WritePaths) > 0 {
		return true
	}
	for _, tool := range a.Capabilities.Tools {
		if workChangingTools[tool] {
			return true
		}
	}
	return false
}

func (s WorkflowSpec) Agent(id string) (AgentSpec, bool) {
	for _, agent := range s.Agents {
		if agent.ID == id {
			return agent, true
		}
	}
	return AgentSpec{}, false
}
