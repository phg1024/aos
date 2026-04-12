# AOS Implementation Plan

## Goal

This is the revised phased plan for implementing AOS as a role-agnostic control plane.

The key rule is:

**AOS provides orchestration primitives. Workflow specs define what each agent does.**

This plan follows harness-engineering discipline:

- deterministic substrate first
- small verifiable slices
- inspectable artifacts after every meaningful run
- live `vllm` used only after deterministic checks pass

---

## Phase 1: Architecture Correction

### Scope

- revise the design docs
- remove the assumption that planner/worker/verifier are built into AOS
- define the new agent and workflow spec model

### Deliverables

- revised `testing-plan.md`
- revised `multi-agent-mini-projects.md`
- revised `implementation-plan.md`
- architecture notes in `AOS_REFACTOR_PLAN.md`

### Acceptance Criteria

- docs consistently describe AOS as role-agnostic
- docs state that verification is optional workflow behavior
- docs define live mini projects and reporting requirements

---

## Phase 2: Workflow Spec and Generic Control Plane

### Scope

- introduce declarative workflow specs
- add validation for agent IDs, entry agent, and allowed delegations
- make the control plane execute agents by ID rather than by hardcoded role

### Deliverables

- `internal/agentflow/spec.go`
- `internal/agentflow/engine.go`
- workflow-spec validation tests

### Acceptance Criteria

- a workflow spec can be loaded from JSON
- invalid workflow specs are rejected deterministically
- entry agent is resolved from the workflow spec
- AOS no longer needs a built-in verifier to run a workflow

---

## Phase 3: Multi-Round Agent Runtime With Delegation

### Scope

- extend the runtime loop so agents can:
  - emit messages
  - use tools
  - delegate to another agent
  - continue after delegate results return
  - exit with structured status

### Deliverables

- delegate action support in `internal/runtime`
- parent-child agent execution through the control plane
- session checkpoints that preserve tool and delegate artifacts

### Acceptance Criteria

- one agent can delegate to another by agent ID
- child results are fed back into the parent session
- tool results are fed back into the same session
- deterministic tests cover delegate and tool round-trips

---

## Phase 4: Operator CLI and Inspectable Reports

### Scope

- add CLI support for workflow-driven runs
- persist workflow state directories
- render HTML reports for workflow runs

### Deliverables

- `aos run --workflow ...`
- `--state-dir` support
- `--report` support
- workflow run report renderer

### Acceptance Criteria

- user can run a workflow from the CLI with input and output files
- workflow state can be preserved in a chosen directory
- HTML report includes processes, artifacts, events, decisions, issues, and tests

---

## Phase 5: Live Mini-Project Harness

### Scope

- define the first real workflow specs
- seed inspectable input files
- run the scenarios against local `vllm`
- preserve outputs and state directories for inspection

### Deliverables

- workflow spec files under `workflows/`
- `cmd/aos-mini-projects`
- per-project output files
- per-project HTML completion reports

### Acceptance Criteria

- at least three live mini projects complete
- each live project uses at least two agents
- each live project produces an output file plus inspectable state
- at least one project includes a review agent without AOS enforcing review globally

---

## Phase 6: Hardening and Next Kernel Work

### Scope

- recovery and resume
- stronger scheduler integration
- richer mailbox usage
- evaluator tuning for optional review workflows

### Deliverables

- restart-safe workflow reconciliation
- mailbox-backed collaboration scenarios
- richer live scenario coverage

### Acceptance Criteria

- interrupted workflows can be resumed from durable state
- scheduler traces remain inspectable
- collaboration can use both delegation and mailbox-based coordination

---

## Required Verification For Every Phase

Before a phase is marked complete:

- `sh ./scripts/verify.sh` passes
- the relevant new tests exist
- a report is generated for the phase
- the report lists completed work, issues, decisions, and tests

For live phases:

- the run must use the local `vllm`
- the output and state directory must remain on disk
- the report must link to inspectable artifacts by local path

---

## Recommended Execution Order

1. Phase 1
2. Phase 2
3. Phase 3
4. Phase 4
5. Phase 5
6. Phase 6

Do not skip from architecture correction straight to live workflows.
The control plane and runtime loop need deterministic proof first.
