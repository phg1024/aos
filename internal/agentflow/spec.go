package agentflow

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"aos/internal/artifact"
	"aos/internal/capability"
	"aos/internal/runtime"
	"aos/internal/supervisor"
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
	// Recall switches the memory-recall aids for every agent of the
	// workflow, e.g. {"no_agent_index": true}. Absent turns them all on.
	Recall runtime.RecallPolicy `json:"recall,omitzero"`
	// MaxDelegationDepth bounds how deep delegations nest: the entry agent
	// is at depth 0, and an agent it delegates to at 1. The engine refuses a
	// delegation that would start a process deeper than this, and the
	// delegator is told so in its delegate_result. Zero means no limit,
	// which the spec contract allows only without a delegation cycle.
	MaxDelegationDepth int `json:"max_delegation_depth,omitempty"`
}

// VerificationSpec names the agent whose recorded kernel exit is the
// workflow's verdict (0 passes, anything else fails) and how it gates
// success. Policy uses the workflow package's vocabulary.
type VerificationSpec struct {
	Policy   string `json:"policy"`
	Verifier string `json:"verifier,omitempty"`
	// Evidence is what the verifier must have read for its exit 0 to be a pass.
	Evidence []EvidenceSpec `json:"evidence,omitempty"`
	// Task is the verifier's task, which the engine gives it at each
	// delegation; the delegator's goal follows, marked as the delegator's
	// words. Absent, the task is DefaultVerifierTask.
	Task string `json:"task,omitempty"`
	// Trusted are the paths the verifier measures with and against: its
	// scripts, baselines and reference binaries. No other agent may be able
	// to write one (spec contract v2). A verifier holding run under a
	// required gate must name them.
	Trusted []string `json:"trusted,omitempty"`
	// Checks are commands the gate runs itself, after the verifier passes,
	// in the verifier's confinement; each must exit as declared for the
	// verdict to stay a pass.
	Checks []CheckSpec `json:"checks,omitempty"`
}

// CheckSpec is one command the gate runs. Uses names the scripts and
// binaries it runs, each inside a trusted path; the verdict records a
// SHA-256 of each, the command's exit code and a SHA-256 of its output.
type CheckSpec struct {
	Name    string   `json:"name"`
	Command string   `json:"command"`
	Uses    []string `json:"uses"`
	// ExitCode is the exit the check must give; absent, 0.
	ExitCode int `json:"exit_code,omitempty"`
	// Timeout bounds the command, as run's timeout arg; absent, 60s.
	Timeout string `json:"timeout,omitempty"`
}

// EvidenceSpec declares one record a verifier must read. Its artifact ID is not
// known when the workflow is written, so it is the latest artifact of this type
// from this agent at the time it is checked.
type EvidenceSpec struct {
	Publisher string `json:"publisher"`
	Type      string `json:"type"`
	// FromFile requires the record to be a file's bytes (publish_artifact's path)
	// and not text a model typed.
	FromFile bool `json:"from_file,omitempty"`
	// TrustedInstrument requires the file to have been written by a run of
	// the publisher whose every named file lies in a trusted path or outside
	// every agent's mounts (a system tool): not a script a worker can write.
	// It implies FromFile.
	TrustedInstrument bool `json:"trusted_instrument,omitempty"`
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
	// Restart, when set, restarts the agent after a retryable exit, from
	// its exit's checkpoint (see Engine.supervise). Absent, an exit goes
	// to the delegator as it is.
	Restart *supervisor.RestartPolicy `json:"restart,omitempty"`
}

// Validate checks the spec against the spec contract: the rules below, then
// validateContract's. A spec that passes can be run; budgets, which come
// from the run, are checked when it runs (Engine.Run).
func (s WorkflowSpec) Validate() error {
	if err := s.ValidateWithoutContract(); err != nil {
		return err
	}
	return s.validateContract(SpecContractVersion)
}

// ValidateContractV1 checks the spec as spec contract v1 did, before the
// verification rules of v2, for replaying a record made under v1.
func (s WorkflowSpec) ValidateContractV1() error {
	if err := s.ValidateWithoutContract(); err != nil {
		return err
	}
	return s.validateContract(1)
}

// ValidateWithoutContract checks the spec as AOS did before the spec
// contract (run.started spec_contract), for replaying a record made then.
func (s WorkflowSpec) ValidateWithoutContract() error {
	if s.Name == "" || s.EntryAgent == "" || len(s.Agents) == 0 {
		return ErrWorkflowSpecInvalid
	}
	if s.Recall.NoPromptFraming {
		return fmt.Errorf("%w: the prompt framing cannot be turned off for a workflow", ErrWorkflowSpecInvalid)
	}
	seen := map[string]bool{}
	hasEntry := false
	for _, agent := range s.Agents {
		if agent.ID == "" || agent.SystemPrompt == "" || seen[agent.ID] {
			return ErrWorkflowSpecInvalid
		}
		seen[agent.ID] = true
		if agent.Restart != nil {
			if _, err := agent.Restart.Effective(); err != nil {
				return fmt.Errorf("%w: agent %s: %v", ErrWorkflowSpecInvalid, agent.ID, err)
			}
		}
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
	if s.Verification != nil && len(s.Verification.Evidence) > 0 {
		if policy == workflow.VerificationNone {
			return fmt.Errorf("%w: verification evidence needs a verification policy other than none", ErrWorkflowSpecInvalid)
		}
		for _, evidence := range s.Verification.Evidence {
			if err := s.validateEvidence(evidence, agents); err != nil {
				return err
			}
		}
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

func (s WorkflowSpec) validateEvidence(evidence EvidenceSpec, agents map[string]bool) error {
	publisher, ok := s.Agent(evidence.Publisher)
	switch {
	case evidence.Publisher == "":
		return fmt.Errorf("%w: verification evidence needs a publisher", ErrWorkflowSpecInvalid)
	case !agents[evidence.Publisher] || !ok:
		return fmt.Errorf("%w: evidence publisher %q is not an agent", ErrWorkflowSpecInvalid, evidence.Publisher)
	case evidence.Publisher == s.Verification.Verifier:
		return fmt.Errorf("%w: verifier %q cannot publish its own evidence", ErrWorkflowSpecInvalid, evidence.Publisher)
	case strings.TrimSpace(evidence.Type) == "" || strings.HasPrefix(evidence.Type, artifact.PublishedPrefix):
		return fmt.Errorf("%w: verification evidence needs a type, without %q", ErrWorkflowSpecInvalid, artifact.PublishedPrefix)
	case !slices.Contains(publisher.Capabilities.Tools, "publish_artifact"):
		return fmt.Errorf("%w: evidence publisher %q does not hold publish_artifact", ErrWorkflowSpecInvalid, evidence.Publisher)
	case evidence.FromFile && !slices.Contains(publisher.Capabilities.Tools, "read_file"):
		return fmt.Errorf("%w: evidence publisher %q publishes from a file, which needs read_file", ErrWorkflowSpecInvalid, evidence.Publisher)
	}
	return nil
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
