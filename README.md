# AOS

AOS is a research prototype for an agent operating system built around deterministic runtime semantics, artifact-based collaboration, and replaceable inference backends.

The codebase is intentionally exploratory, but the core runtime is implemented and covered by Go tests. The current repo includes a file-backed kernel, IPC bus, artifact store, lease manager, capability model, tool runtime, workflow engine, configurable local LLM integration, and an interactive terminal UI.

## Current Status

- Research prototype, not a polished product release
- Core Go packages test cleanly with the repo-local cache path used by `make test`
- Active areas include workflow tooling, skills, local TUI ergonomics, and evaluation/demo workflows
- Generated live-run artifacts are treated as disposable outputs, not durable source assets

See [STATUS.md](./STATUS.md) for the current project snapshot and [RELEASE_CHECKLIST.md](./RELEASE_CHECKLIST.md) for repo hygiene before sharing milestones.

## Repository Layout

- `cmd/` entrypoints for the CLI, TUI, smoke tests, demos, and mini-project runners
- `internal/` runtime, kernel, workflow, inference, tool, and TUI implementation packages
- `skills/` local skill definitions used by workflow or interactive sessions
- `workflows/` JSON workflow specs for multi-agent experiments and demos
- `reports/` curated milestone reports and a small set of durable reference outputs

## Common Commands

- `make test`
- `make fmt`
- `go run ./cmd/aos`
- `go run ./cmd/aos-tui --workdir .`

## Workflows in the TUI

`go run ./cmd/aos-tui --workflow workflow.json --workdir DIR` runs the workflow's entry agent interactively. When it delegates, the delegated agents run through the same engine as `aos run --workflow`, and the TUI shows their progress:

- each delegated agent's activity is printed in the transcript, indented by depth between `┌ agent` and `└ agent exited` markers
- a pinned progress row shows the delegation stack (`coordinator 3/400 › writer 49/200`), how long the current agent has been running, what it is doing, and how many delegations have finished

Type `/` for commands: the input line highlights known commands, shows the best prefix match and its description as you type, and Tab completes. `/help` lists the commands, ↑/↓ recall earlier input, and a line starting with `//` is sent to the agent with a single `/`.

Press Esc while a task is running to cancel it. Cancelling stops the model call, kills any running `run` command, closes delegated agents, and checkpoints the conversation. Your next message continues from there, and the agent is told its previous task was interrupted.

`--agent ID` starts from a different agent in the workflow. Context compaction follows `AOS_LLM_CONTEXT_WINDOW` and `AOS_COMPACT_RATIO`, the same way it does for `aos run`.

`make test` uses repo-local `GOCACHE` and `GOTMPDIR` paths so tests run reliably in sandboxed environments too.

## Workflow Outcomes

A workflow succeeds only if its entry agent's recorded exit code is 0. `aos run --workflow` writes the summary (with `exit_status`) and the report either way, then exits with the entry agent's code when it is non-zero (1 if that code is outside 1–125), with the reason on stderr. Delegated agents' exits reach their parent as `exit_code`, `exit_reason` and `exit_retryable` fields on the `delegate_result`, not only through the child's last message.

A workflow can gate on a verifier with `"verification": {"policy": "required", "verifier": "<agent id>"}`. The verdict is the verifier's recorded exit code (0 passes), read from the kernel, never from a file or a message. Under `required` the workflow succeeds only if the entry agent exits 0 and the verifier's latest exit passed with no work-changing agent (one with `write_file`, `edit_file`, `run`, or write paths) running after it. A verifier's own judgement, a failure from its own exit action whatever `retryable` flag it sets, stands until a work-changing agent runs again, so re-running the verifier cannot shop for a pass. A failure the runtime recorded, such as a step limit or crash, can be cleared by a later run. Exit records carry `origin: "agent"` for exits an agent chose; the runtime sets it and an agent cannot. `best_effort` records the verdict without gating. The engine stores the verdict as a `verification` artifact produced by the workflow root, and in the summary's `verification` field. The checked-in rasterizer and reviewed-merge workflows require verification.

## Delegation Authority

A delegated agent never gets more than its parent may grant. Each agent's `capabilities` are what it may use, and its optional `delegable` set is authority it may only pass on, so a coordinator without `run` can still delegate to workers that need it. Validation rejects any `allowed_agents` edge where the child asks for more than its parent's capabilities and delegable authority together, naming the edge and the excess. At spawn the engine also narrows the child's tools, paths and mounts to what the parent's kernel record allows. The entry agent's sets, plus the run request, are the trust root. The `delegate` action needs `spawn_agent`, and `send`, `publish` and `subscribe` need `message_agent`; tool calls and actions share one check. Every denial is logged as a `capability.denied` kernel event.

## Kernel State

Kernel state lives under `<state-dir>/kernel` and is reachable only through `kernel.New`. The event log (`events.json`) is the source of truth: every state change appends its event, with the data needed to rebuild it, before the process table (`processes.json`) is written, and the table is repaired from newer events whenever it is read. Each update holds an exclusive lock (`flock` on `kernel.lock` on unix), so `aos run`, `aos-tui` and other processes can share a state directory, and event IDs stay gap-free and totally ordered across them.

## Artifacts

Artifacts under `<state-dir>/artifacts` are content-addressed: the index records each payload's sha256 and every read verifies it, so a payload changed on disk fails with `ErrArtifactTampered`. This makes payload edits evident; it does not stop an actor who can write the state directory itself, who could also rewrite or drop the recorded hash (entries without one are trusted as pre-hashing legacy). Keeping agents out of the state directory is the job of confining `run`. Payloads and the index are written atomically under a cross-process lock. An artifact's parents must exist and its producer must be a process the kernel knows, and runs link their artifacts: checkpoints chain to the previous one, records name the checkpoint they ran under, and the `workflow_summary` points at every `agent_run` and the verdict. Runtime types such as `checkpoint`, `verification` and `memory_segment` are reserved; agents publish under `published.<name>`. A process resumes only from its own checkpoints (or its `resumed_from` chain), and compacted history lives in runtime-owned `memory_segment` artifacts that agents read back with `read_memory`.

## Run Record

A run keeps its own record on disk as it goes, so a killed run can still be reconstructed. Under `<state-dir>/run`:

- `trace.jsonl` holds the engine trace (start, delegate, tool calls, exit, `capability_denied`, and so on). Records that match a kernel event, such as start, exit, runtime errors and denials, name it in `kernel_event`.
- `calls.jsonl` holds one record per model call: process and agent, usage (prompt, cached and completion tokens), latency, error, request params, and the sha256 of the exact request body. Prompts are stored by delta: a call records only the messages added since its process's previous successful call (`base` is how many it carries over). So the system prompt is written once, in the process's first call, and `inference.RebuildRequestBody` reproduces any request byte for byte. Repair calls are flagged `repair`.

Both are append-only JSONL. Each record gets a `seq` assigned under a file lock (`*.lock`), and the line is fsynced before the call returns. Several writers, including other processes, keep `seq` gap-free, and a kill can leave at most a torn last line, which readers skip. These files are diagnostic; the kernel event log stays the source of truth.

`aos inspect <state-dir>` rebuilds a run from these files and the kernel and artifact stores, without a model. It shows the process tree with exit records, which processes never exited, each process's latest checkpoint, the verification verdict, token usage per process and in total, and the trace tail. Use `--json` for machine-readable output and `--tail N` to set the trace length (default 20). The Harbor adapter reads its token counts from `aos inspect --json`, and on a timeout from `calls.jsonl` directly.

If `aos run --workflow` stops on a Go error after the root process started (cancellation, a failed store write), it still writes the report and the summary JSON, with the error in `error`, and it records an exit for every process it started.

## Verification Policy

Verification is a workflow choice, not a built-in step. `aos run` without `--workflow` takes `--verify none|required|best_effort` (default `none`):

- `none` writes the assembled output without a verifier call
- `required` writes the output only after the verifier approves it
- `best_effort` runs the verifier and records its verdict, but keeps the output either way

The contract harness in `internal/workflow` uses the same policies: `Engine.Verifier` is optional, and `Engine.Verification` defaults to `required` when a verifier is set and `none` otherwise.

## LLM Backend Config

AOS now uses backend-agnostic env vars for live model selection:

- `AOS_LLM_BACKEND` with values such as `vllm`, `ollama`, or `openai-compatible`
- `AOS_LLM_BASE_URL`
- `AOS_LLM_MODEL`
- `AOS_LLM_API_KEY`
- `AOS_LLM_TIMEOUT_SECONDS`
- `AOS_LLM_TEMPERATURE` (default `0`), `AOS_LLM_TOP_P`, `AOS_LLM_MAX_TOKENS` optional sampling controls; thinking models such as Qwen should not run greedy, e.g. temperature `0.6` with top_p `0.95`
- `AOS_LLM_THINKING_BUDGET` optional cap on reasoning tokens, sent as `reasoning_budget_tokens` for llama.cpp-style servers such as Strata
- `AOS_LLM_CONTEXT_WINDOW` model context size in tokens; when set, workflow runs compact agent history at `AOS_COMPACT_RATIO` (default `0.8`) of it instead of the small built-in limits

Legacy `AOS_VLLM_*` vars are still supported as fallback for existing scripts.

Example using Ollama:

```sh
AOS_LLM_BACKEND=ollama \
AOS_LLM_BASE_URL=http://localhost:11434 \
AOS_LLM_MODEL=qwen3.6:35b-a3b-mtp-q4_K_M \
go run ./cmd/aos run --workflow ./workflows/rasterizer-optimized-tail.json --output ./reports/rasterizer-team-demo/final-summary.txt
```

## Docs

- [STATUS.md](./STATUS.md)
- [Agent Microkernel Proposal](./agent-microkernel-proposal.md)
- [Guiding Principles](./guiding-principles.md)
- [Implementation Plan](./implementation-plan.md)
- [Testing Plan](./testing-plan.md)
- [Mini Projects](./multi-agent-mini-projects.md)

## Curated Reports

- [Phase 0 Report](./reports/phase-0.html)
- [Phase 1 Report](./reports/phase-1.html)
- [Phase 2 Report](./reports/phase-2.html)
- [Phase 3 Report](./reports/phase-3.html)
- [Phase 4 Report](./reports/phase-4.html)
- [Phase 5 Report](./reports/phase-5.html)
- [Phase 6 Report](./reports/phase-6.html)
- [Phase 7 Report](./reports/phase-7.html)
- [Phase 8 Report](./reports/phase-8.html)
- [Final Completion Report](./reports/final-completion.html)
