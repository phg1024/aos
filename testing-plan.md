# AOS Testing Plan

## Goal

This plan covers how to test the revised AOS architecture:

- AOS core is a role-agnostic control plane
- agent roles live in workflow specs
- verification is optional workflow behavior, not automatic kernel behavior
- the core execution model is a multi-round LLM-tool-delegate loop

The testing strategy separates deterministic runtime correctness from model-backed behavior.

---

## Test Layers

### 1. Unit Tests

Unit tests protect deterministic local behavior and must run without live model access.

Core unit coverage should include:

- kernel process lifecycle and event logging
- artifact creation, lineage, and payload recovery
- mailbox send/receive semantics
- lease contention and wake-up behavior
- capability enforcement for tools and file paths
- tool runtime behavior for `read_file` and `write_file`
- workflow spec validation
- runtime malformed-action handling
- delegate action handling

Required rule:

**No live `vllm` dependency is allowed in unit tests.**

### 2. Integration Tests

Integration tests should combine multiple subsystems under fake or scripted backends.

Required coverage:

- one agent using multiple tool calls across several rounds
- one agent delegating to another agent
- a parent agent receiving a delegate result and continuing work
- output-file creation through agent-directed tool usage
- workflow summary artifact generation
- HTML report generation from a completed workflow run

These tests should prove that collaboration is native to the runtime and not hardcoded into one app path.

### 3. Scenario Tests

Scenario tests should exercise small but complete workflows with inspectable traces.

Required scenario classes:

- synthesis-and-write workflow
- extraction-and-write workflow
- workflow with a review agent
- workflow with no review agent

Scenario tests may use fake backends first and live `vllm` later.

### 4. Live Model Tests

Live tests use the local `vllm` endpoint and validate that the control plane works with a real LLM.

Live tests are not used to prove deterministic correctness.
They are used to prove:

- prompt contracts are usable
- action JSON is produced reliably enough
- tool feedback can drive multiple rounds
- delegation works in practice
- inspectable artifacts are created for real runs

Required live outputs for every scenario:

- state directory
- kernel event log
- artifact store
- final output file
- HTML completion report

---

## Acceptance Gates

### Gate A: Deterministic Correctness

Before any live run is treated as meaningful:

- `sh ./scripts/verify.sh` passes
- workflow-spec tests pass
- delegation integration tests pass
- tool runtime tests pass

### Gate B: Live Workflow Completion

A live workflow scenario passes when:

- the workflow finishes without manual patching
- at least two agents participate
- at least one tool call succeeds
- the final output file exists
- the report and state directory are inspectable afterward

### Gate C: Optional Review Behavior

At least one live scenario must include a review agent and prove:

- review is configured by the workflow
- AOS itself does not force the review
- the workflow still records the review artifact and trace

---

## Required Test Matrix

### Kernel and Primitives

- process spawn/exit persistence
- invalid lifecycle transition rejection
- event log ordering
- artifact payload round-trip
- mailbox round-trip
- lease block/wake sequence

### Runtime Loop

- tool action followed by tool result feedback
- delegate action followed by child result feedback
- message action followed by another action
- exit action with structured status
- malformed JSON or malformed action rejection

### Workflow Control Plane

- valid workflow spec loads successfully
- invalid workflow spec is rejected
- allowed-agent restrictions are enforced
- workflow summary artifact is written
- workflow run report renders

### Live Model Harness

- live docs merge
- live fact sheet
- live reviewed merge

---

## Reporting Requirements

Every completed phase or live mini-project should produce an HTML report that includes:

- what was implemented
- issues encountered
- how those issues were resolved
- design decisions made
- tests run
- links or paths to inspectable artifacts

The report is part of the acceptance evidence, not an optional extra.

---

## Execution Commands

Deterministic verification:

```bash
cd /Users/claw/Code/aos
sh ./scripts/verify.sh
```

Single workflow run:

```bash
cd /Users/claw/Code/aos
AOS_VLLM_BASE_URL=http://lenovo-px.local:8001 \
AOS_VLLM_MODEL=Qwen/Qwen3.5-27B \
AOS_VLLM_API_KEY=dummy \
go run ./cmd/aos run \
  --workflow /Users/claw/Code/aos/workflows/live-docs-merge.json \
  --input /abs/path/source-a.html \
  --input /abs/path/source-b.html \
  --output /abs/path/combined.html \
  --report /abs/path/run-report.html \
  --state-dir /abs/path/state \
  "Assemble the provided notes into one compact coherent HTML report."
```

Full live mini-project suite:

```bash
cd /Users/claw/Code/aos
AOS_VLLM_BASE_URL=http://lenovo-px.local:8001 \
AOS_VLLM_MODEL=Qwen/Qwen3.5-27B \
AOS_VLLM_API_KEY=dummy \
go run ./cmd/aos-mini-projects
```
