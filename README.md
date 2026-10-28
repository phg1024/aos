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

A workflow can gate on a verifier with `"verification": {"policy": "required", "verifier": "<agent id>"}`. The verdict is the verifier's recorded exit code (0 passes), read from the kernel, never from a file or a message. Under `required` the workflow succeeds only if the entry agent exits 0 and the verifier's latest exit passed with no work-changing agent (one with `write_file`, `edit_file`, `run`, or write paths) running after it. `best_effort` records the verdict without gating. The engine stores the verdict as a `verification` artifact produced by the workflow root, and in the summary's `verification` field. The checked-in rasterizer and reviewed-merge workflows require verification.

## Delegation Authority

A delegated agent never gets more than its parent may grant. Each agent's `capabilities` are what it may use, and its optional `delegable` set is authority it may only pass on, so a coordinator without `run` can still delegate to workers that need it. Validation rejects any `allowed_agents` edge where the child asks for more than its parent's capabilities and delegable authority together, naming the edge and the excess. At spawn the engine also narrows the child's tools, paths and mounts to what the parent's kernel record allows. The entry agent's sets, plus the run request, are the trust root. The `delegate` action needs `spawn_agent`, and `send`, `publish` and `subscribe` need `message_agent`; tool calls and actions share one check. Every denial is logged as a `capability.denied` kernel event.

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
