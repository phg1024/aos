# AOS Multi-Agent Mini Projects

## Goal

This suite is designed to test real collaboration in the revised AOS architecture.

The projects are intentionally small enough to run often, but rich enough to prove:

- agents can collaborate without hardcoded roles in AOS
- the runtime can handle multiple rounds of tool use
- agents can delegate work to other agents
- optional review behavior can be expressed in the workflow spec
- local `vllm` can drive real end-to-end runs

---

## Project 1: Live Docs Merge

### Purpose

Show a simple collaborative publishing flow with no built-in verifier:

- coordinator agent
- synthesizer agent
- writer agent

### Workflow

1. coordinator delegates reading and synthesis
2. synthesizer reads multiple input files
3. synthesizer returns merged HTML content
4. coordinator delegates file writing
5. writer writes the final output file
6. coordinator exits

### What It Tests

- delegation
- multi-round tool usage
- passing deliverables between agents
- final file creation
- absence of mandatory verifier behavior

### Inspectable Artifacts

- merged HTML output
- workflow summary artifact
- delegate result artifacts
- kernel event log
- HTML completion report

---

## Project 2: Live Fact Sheet

### Purpose

Show a structured extraction workflow where one agent extracts facts and another materializes the deliverable.

### Workflow

1. router agent delegates extraction
2. extractor reads plain-text notes
3. extractor returns a markdown fact sheet
4. router delegates writing
5. writer writes the markdown output
6. router exits

### What It Tests

- structured extraction from source files
- content handoff from one agent to another
- non-HTML output support
- inspectable markdown output

### Inspectable Artifacts

- markdown fact sheet
- workflow state directory
- tool result artifacts
- completion report

---

## Project 3: Live Reviewed Merge

### Purpose

Show that review is a workflow choice rather than a built-in AOS rule.

### Workflow

1. lead agent delegates merge work
2. merger agent reads source files and produces final HTML
3. writer agent writes the output file
4. review partner reads the produced output and returns a review note
5. lead agent decides how to finish using that review note

### What It Tests

- optional reviewer participation
- delegation to more than one downstream agent
- review as normal workflow behavior
- collaboration without kernel-enforced verification

### Inspectable Artifacts

- final HTML output
- review note in session history and delegate artifacts
- workflow summary artifact
- HTML completion report

---

## Future Expansion Projects

After the first three live scenarios are stable, the next candidates are:

- parallel code-edit workflow with lease coordination
- resume-after-crash workflow
- mixed local-and-external runtime workflow
- long-running artifact review chain

These are useful later, but the first suite should stay small and reliable.

---

## Acceptance Criteria

Each mini project is considered complete only when:

- the workflow finishes with a clean final exit
- at least two agents participate
- at least one agent uses a tool
- the final output file exists
- the state directory remains on disk for inspection
- the HTML completion report is generated

---

## Run Command

```bash
cd /Users/claw/Code/aos
AOS_VLLM_BASE_URL=http://lenovo-px.local:8001 \
AOS_VLLM_MODEL=Qwen/Qwen3.5-27B \
AOS_VLLM_API_KEY=dummy \
go run ./cmd/aos-mini-projects
```

By default the reports are written under:

`/Users/claw/Code/aos/reports/live-mini-projects`
