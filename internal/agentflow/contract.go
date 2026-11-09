package agentflow

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"aos/internal/capability"
	"aos/internal/tools"
	"aos/internal/workflow"
)

// The spec contract's limits.
const (
	// SpecContractVersion is the spec contract Validate checks: 2 adds the
	// verification rules (verificationContract) to 1.
	SpecContractVersion = 2
	// MaxStepsCeiling is the most steps an agent may be given. The largest a
	// checked-in workflow sets is 800 (pathtracer-bunny-metal's
	// coordinator); a limit past this is a typo or no limit at all.
	MaxStepsCeiling = 1000
	// MaxDelegationDepthCeiling is the deepest max_delegation_depth a spec
	// may set.
	MaxDelegationDepthCeiling = 100
)

// ErrUnboundedDelegation is a run of a spec with a delegation cycle that
// has no budget that can stop it.
var ErrUnboundedDelegation = errors.New("a delegation cycle needs a budget that can stop it")

// writingTools are the tools that can change files: the work a verifier
// checks.
var writingTools = []string{"write_file", "edit_file", "run"}

// validateContract is the spec contract beyond the older rules: limits in
// range, delegation edges that can be used, every agent reachable, cycles
// bounded, evidence that can be published, and a verifier that cannot
// change the work it judges.
func (s WorkflowSpec) validateContract(version int) error {
	var problems []string
	for i, agent := range s.Agents {
		where := fmt.Sprintf("agents[%d] (%s)", i, agent.ID)
		if agent.MaxSteps < 0 || agent.MaxSteps > MaxStepsCeiling {
			problems = append(problems, fmt.Sprintf("%s: max_steps %d is out of range; give 1 to %d, or leave it out for the default of %d", where, agent.MaxSteps, MaxStepsCeiling, maxSteps(AgentSpec{})))
		}
		spawns := slices.Contains(agent.Capabilities.Tools, "spawn_agent")
		switch {
		case len(agent.AllowedAgents) > 0 && !spawns:
			problems = append(problems, fmt.Sprintf("%s: allowed_agents %v needs the spawn_agent tool, without which every delegation is refused at run time", where, agent.AllowedAgents))
		case spawns && len(agent.AllowedAgents) == 0:
			problems = append(problems, fmt.Sprintf("%s: holds spawn_agent with no allowed_agents, so it can delegate to nobody", where))
		}
	}
	if s.MaxDelegationDepth < 0 || s.MaxDelegationDepth > MaxDelegationDepthCeiling {
		problems = append(problems, fmt.Sprintf("max_delegation_depth %d is out of range; give 1 to %d, or leave it out for no limit", s.MaxDelegationDepth, MaxDelegationDepthCeiling))
	}
	paths := s.delegationPaths(s.EntryAgent)
	for _, agent := range s.Agents {
		if _, ok := paths[agent.ID]; !ok {
			problems = append(problems, fmt.Sprintf("agent %q is not reachable from entry agent %q through allowed_agents; reachable: %s", agent.ID, s.EntryAgent, s.describePaths(paths)))
		}
	}
	if cycle := s.DelegationCycle(); cycle != "" && s.MaxDelegationDepth == 0 {
		problems = append(problems, fmt.Sprintf("delegation cycle %s: a cycle needs max_delegation_depth (1 to %d) to bound how deep delegations nest, and the run then needs a token budget or a strict time budget to stop it", cycle, MaxDelegationDepthCeiling))
	}
	if s.Verification != nil {
		for i, evidence := range s.Verification.Evidence {
			if _, ok := paths[evidence.Publisher]; !ok {
				problems = append(problems, fmt.Sprintf("verification.evidence[%d]: publisher %q is not reachable from entry agent %q through allowed_agents, so the evidence can never be published", i, evidence.Publisher, s.EntryAgent))
			}
		}
	}
	if policy, _ := s.VerificationPolicy(); policy != workflow.VerificationNone {
		if reasons := s.verifierChangesWork(s.Verification.Verifier); len(reasons) > 0 {
			problems = append(problems, fmt.Sprintf("verifier %q can change the work it judges: %s. %s", s.Verification.Verifier, strings.Join(reasons, "; "), VerifierRule))
		}
	}
	if version >= 2 {
		problems = append(problems, s.verificationContract()...)
	}
	if len(problems) > 0 {
		return fmt.Errorf("%w: %s", ErrWorkflowSpecInvalid, strings.Join(problems, "; "))
	}
	return nil
}

// VerifierRule is what "cannot change the work it judges" means, as the
// spec contract checks it and its errors state it.
const VerifierRule = "A verifier may hold write_file, edit_file or run only with write_paths of its own, none of which overlaps (contains or is inside) another work-changing agent's write_paths, and without unconfined_run; it must not be the only agent that can change the work; and it may delegate only to agents that cannot change the work"

// verifierChangesWork is why the verifier could change the work it judges,
// none if it cannot. An agent changes the work when it holds a writing tool
// or declares write paths (IsWorkChanging); its write scope is its
// write_paths, or, when it has none, the paths the run request derives,
// which are where the work goes.
func (s WorkflowSpec) verifierChangesWork(id string) []string {
	verifier, ok := s.Agent(id)
	if !ok {
		return nil
	}
	var reasons []string
	writes := holdsAny(verifier.Capabilities.Tools, writingTools)
	if writes {
		if slices.Contains(verifier.Capabilities.Tools, "run") && verifier.Capabilities.UnconfinedRun {
			reasons = append(reasons, "it holds run with unconfined_run, which can write anywhere")
		}
		if len(verifier.Capabilities.WritePaths) == 0 {
			reasons = append(reasons, "it holds "+strings.Join(intersect(verifier.Capabilities.Tools, writingTools), ", ")+" with no write_paths, so it writes where the run request points, which is where the work goes")
		}
		others := 0
		for _, agent := range s.Agents {
			if agent.ID == id || !agent.IsWorkChanging() {
				continue
			}
			others++
			for _, theirs := range agent.Capabilities.WritePaths {
				for _, own := range verifier.Capabilities.WritePaths {
					if pathsOverlap(own, theirs) {
						reasons = append(reasons, fmt.Sprintf("its write path %s overlaps %s, which %s can write", own, theirs, agent.ID))
					}
				}
			}
		}
		if others == 0 {
			reasons = append(reasons, "it is the only agent that can change the work, so it would judge its own")
		}
	}
	for target := range s.delegationPaths(id) {
		if target == id {
			continue
		}
		if agent, ok := s.Agent(target); ok && agent.IsWorkChanging() {
			reasons = append(reasons, fmt.Sprintf("it can delegate to %s, which can change the work", target))
		}
	}
	slices.Sort(reasons)
	return slices.Compact(reasons)
}

func holdsAny(tools, wanted []string) bool { return len(intersect(tools, wanted)) > 0 }

func intersect(tools, wanted []string) []string {
	var held []string
	for _, tool := range wanted {
		if slices.Contains(tools, tool) {
			held = append(held, tool)
		}
	}
	return held
}

// pathsOverlap reports whether one path is the other or lies inside it.
func pathsOverlap(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	return a == b || strings.HasPrefix(b, a+string(filepath.Separator)) || strings.HasPrefix(a, b+string(filepath.Separator))
}

// delegationPaths is every agent from can reach through allowed_agents,
// itself included, with the shortest chain of delegations to it.
func (s WorkflowSpec) delegationPaths(from string) map[string][]string {
	paths := map[string][]string{from: {from}}
	queue := []string{from}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		agent, _ := s.Agent(current)
		for _, next := range agent.AllowedAgents {
			if _, seen := paths[next]; !seen {
				paths[next] = append(slices.Clone(paths[current]), next)
				queue = append(queue, next)
			}
		}
	}
	return paths
}

// describePaths lists the reachable agents in spec order, each with its
// chain from the entry agent.
func (s WorkflowSpec) describePaths(paths map[string][]string) string {
	var parts []string
	for _, agent := range s.Agents {
		if path, ok := paths[agent.ID]; ok {
			parts = append(parts, strings.Join(path, " -> "))
		}
	}
	return strings.Join(parts, ", ")
}

// DelegationCycle is a cycle of allowed_agents edges among the agents the
// entry agent reaches, as "a -> b -> a" (self-delegation is "a -> a"), or
// "" when there is none.
func (s WorkflowSpec) DelegationCycle() string {
	const (
		unvisited = iota
		active
		done
	)
	state := map[string]int{}
	var stack []string
	var visit func(id string) string
	visit = func(id string) string {
		state[id] = active
		stack = append(stack, id)
		agent, _ := s.Agent(id)
		for _, next := range agent.AllowedAgents {
			switch state[next] {
			case active:
				start := slices.Index(stack, next)
				return strings.Join(append(slices.Clone(stack[start:]), next), " -> ")
			case unvisited:
				if cycle := visit(next); cycle != "" {
					return cycle
				}
			}
		}
		stack = stack[:len(stack)-1]
		state[id] = done
		return ""
	}
	return visit(s.EntryAgent)
}

// CanStopCycle reports whether budget, the run's, or the entry agent's own
// budget can stop a delegation cycle: a token budget, or a strict time
// budget. An advisory time budget only warns the agent, and steps are per
// process, so neither bounds how many processes a cycle starts.
func (s WorkflowSpec) CanStopCycle(budget capability.Budget) bool {
	entry, _ := s.Agent(s.EntryAgent)
	for _, b := range []capability.Budget{budget, entry.Capabilities.Budget} {
		if b.Tokens > 0 || (b.Seconds > 0 && b.StrictTime) {
			return true
		}
	}
	return false
}

// CheckCycleBudget refuses a delegation cycle that neither budget nor the
// entry agent's own budget can stop (CanStopCycle). howToBudget tells the
// person how the command running the spec gives it one.
func (s WorkflowSpec) CheckCycleBudget(budget capability.Budget, howToBudget string) error {
	cycle := s.DelegationCycle()
	if cycle == "" || s.CanStopCycle(budget) {
		return nil
	}
	return fmt.Errorf("%w: %s, with max_delegation_depth %d, can still start a process per step at every level; %s", ErrUnboundedDelegation, cycle, s.MaxDelegationDepth, howToBudget)
}

// verificationContract is spec contract v2's rules for a verification: what
// the verifier trusts can be written by no other agent, gate checks run only
// trusted programs, and a required gate has neither an unconfined agent nor
// a work scope of the whole filesystem, which no hash can bind.
func (s WorkflowSpec) verificationContract() []string {
	v := s.Verification
	if v == nil {
		return nil
	}
	policy, _ := s.VerificationPolicy()
	var problems []string
	if policy == workflow.VerificationNone {
		if v.Task != "" || len(v.Trusted) > 0 || len(v.Checks) > 0 {
			problems = append(problems, "verification task, trusted and checks need a verification policy other than none")
		}
		return problems
	}
	if policy == workflow.VerificationRequired {
		for i, agent := range s.Agents {
			if agent.Capabilities.UnconfinedRun || agent.Delegable.UnconfinedRun {
				problems = append(problems, fmt.Sprintf("agents[%d] (%s): unconfined_run under a required gate: an unconfined command can write the state directory the gate reads, so a workflow with verification policy required may give it to no agent", i, agent.ID))
			}
			if agent.ID == v.Verifier || !agent.IsWorkChanging() {
				continue
			}
			for _, scope := range staticWriteScopes(agent) {
				if filepath.Clean(scope) == "/" {
					problems = append(problems, fmt.Sprintf("agents[%d] (%s): writes / under a required gate: the verdict is bound to a hash of the work's write scopes, and / cannot be hashed; narrow its write_paths and mounts to the work", i, agent.ID))
					break
				}
			}
		}
		if verifier, ok := s.Agent(v.Verifier); ok && slices.Contains(verifier.Capabilities.Tools, "run") && len(v.Trusted) == 0 {
			problems = append(problems, fmt.Sprintf("verifier %q holds run under a required gate and names no verification.trusted paths: name the scripts, baselines and reference binaries it runs and compares against, which no other agent may write", v.Verifier))
		}
	}
	for i, trusted := range v.Trusted {
		if strings.TrimSpace(trusted) == "" {
			problems = append(problems, fmt.Sprintf("verification.trusted[%d] is empty", i))
			continue
		}
		if writers := s.trustedWriters(trusted, nil); len(writers) > 0 {
			problems = append(problems, fmt.Sprintf("verification.trusted[%d] %s can be written by %s: what the verifier trusts must be out of every other agent's write scope", i, trusted, strings.Join(writers, ", ")))
		}
	}
	names := map[string]bool{}
	for i, check := range v.Checks {
		where := fmt.Sprintf("verification.checks[%d]", i)
		switch {
		case strings.TrimSpace(check.Name) == "":
			problems = append(problems, where+": needs a name")
		case names[check.Name]:
			problems = append(problems, fmt.Sprintf("%s: name %q is used twice", where, check.Name))
		}
		names[check.Name] = true
		if strings.TrimSpace(check.Command) == "" {
			problems = append(problems, where+": needs a command")
		}
		if len(check.Uses) == 0 {
			problems = append(problems, where+": needs uses, the scripts and binaries it runs, inside verification.trusted")
		}
		for j, used := range check.Uses {
			if !s.isTrusted(used) {
				problems = append(problems, fmt.Sprintf("%s.uses[%d] %s is not inside a verification.trusted path", where, j, used))
			}
		}
		if check.ExitCode < 0 {
			problems = append(problems, fmt.Sprintf("%s: exit_code %d is negative", where, check.ExitCode))
		}
		if _, err := tools.DurationArg("check timeout", check.Timeout, time.Minute); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", where, err))
		}
	}
	for i, evidence := range v.Evidence {
		if evidence.TrustedInstrument && len(v.Trusted) == 0 {
			problems = append(problems, fmt.Sprintf("verification.evidence[%d]: trusted_instrument needs verification.trusted paths to check the instrument against", i))
		}
	}
	return problems
}

// isTrusted reports whether path lies in a verification.trusted path.
func (s WorkflowSpec) isTrusted(path string) bool {
	if s.Verification == nil {
		return false
	}
	for _, trusted := range s.Verification.Trusted {
		if pathsWithin(capability.ResolvePath(path), capability.ResolvePath(trusted)) {
			return true
		}
	}
	return false
}

func pathsWithin(path, root string) bool {
	path, root = filepath.Clean(path), filepath.Clean(root)
	return path == root || root == "/" || strings.HasPrefix(path, root+string(filepath.Separator))
}

// trustedWriters are the agents other than the verifier that can write
// path: their write scope contains it or lies inside it. scopes, when set,
// gives each agent's effective write scopes, as a run derives them; without
// it, an agent with no write_paths and no mounts writes where the run
// request points, which is checked when the run starts.
func (s WorkflowSpec) trustedWriters(path string, scopes func(AgentSpec) []string) []string {
	var writers []string
	for _, agent := range s.Agents {
		if s.Verification != nil && agent.ID == s.Verification.Verifier || !agent.IsWorkChanging() {
			continue
		}
		agentScopes := staticWriteScopes(agent)
		if scopes != nil {
			agentScopes = scopes(agent)
		}
		for _, scope := range agentScopes {
			if pathsOverlap(capability.ResolvePath(scope), capability.ResolvePath(path)) {
				writers = append(writers, agent.ID)
				break
			}
		}
	}
	return writers
}

// staticWriteScopes is where agent can write as far as the spec alone says:
// its write_paths within its writable mounts, its writable mounts when it
// names no write_paths, its write_paths when it has no mounts, and nothing
// it can know when it has neither.
func staticWriteScopes(agent AgentSpec) []string {
	if !agent.IsWorkChanging() {
		return nil
	}
	writable := func() []string {
		var roots []string
		for _, mount := range agent.Mounts {
			if !mount.ReadOnly {
				roots = append(roots, mount.HostPath)
			}
		}
		return roots
	}
	switch {
	case len(agent.Mounts) == 0:
		return agent.Capabilities.WritePaths
	case len(agent.Capabilities.WritePaths) == 0:
		return writable()
	}
	_, write, _ := capability.RunScopes(agent.Capabilities, agent.Mounts)
	return write
}

// checkTrustedAt repeats the trusted-path rule with each agent's write
// scopes as req derives them, which covers agents whose scopes the spec
// leaves to the run request.
func (s WorkflowSpec) checkTrustedAt(req RunRequest) error {
	if s.Verification == nil {
		return nil
	}
	var problems []string
	for i, trusted := range s.Verification.Trusted {
		if writers := s.trustedWriters(trusted, func(agent AgentSpec) []string { return effectiveWriteScopes(agent, req) }); len(writers) > 0 {
			problems = append(problems, fmt.Sprintf("verification.trusted[%d] %s can be written by %s, with the scopes this run request derives: what the verifier trusts must be out of every other agent's write scope", i, trusted, strings.Join(writers, ", ")))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("%w: %s", ErrWorkflowSpecInvalid, strings.Join(problems, "; "))
	}
	return nil
}

// effectiveWriteScopes is where agent's process can write in a run of req:
// its write scopes within its writable mounts, both as the engine grants
// them.
func effectiveWriteScopes(agent AgentSpec, req RunRequest) []string {
	if !agent.IsWorkChanging() {
		return nil
	}
	_, write, _ := capability.RunScopes(agentCapabilities(agent, req), agentMounts(agent, req))
	return write
}
