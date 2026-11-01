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

`scripts/tui.sh [ollama|vllm|strata|strata-phg|strata-phg-vllm] [aos-tui args...]` starts the TUI against a backend preset. Each session keeps its state in `.aos-sessions/<timestamp>-<backend>` (`AOS_TUI_SESSIONS_DIR` moves it, `--state-dir` overrides it), printed at startup, so `aos inspect` can read it after the TUI exits. In the TUI, `/resume` lists the saved sessions in that directory, newest first, and `/resume N` (or a name prefix) switches to one: the TUI restarts on its state directory, shows the last exchanges, and the agent continues from its latest checkpoint. Starting the TUI on an existing session's `--state-dir` resumes it the same way. A session left mid-turn by a killed TUI is parked again; one whose agent exited retryably is recovered by the next message; one whose agent exited for good continues in a new agent seeded with its conversation. A session open in another TUI cannot be resumed. No tool reaches a saved session: the TUI protects the whole sessions directory from the file tools and hides it from `run`, or, when that directory holds the workspace or the temporary directory, each session in it.

`/model` switches the model mid-session. It lists the model profiles (↑/↓ and Enter), and `/model NAME` or `/model N` switches to one.
- **Profiles.** The profiles are the backend the TUI started with, named by `AOS_MODEL_PROFILE` (`tui.sh` sets it to the preset) or `env`, followed by those in the profiles file: `--models`, else `AOS_MODELS_FILE`, else `~/.config/aos/models.json`. `tui.sh` falls back to `scripts/models.json`, which holds its presets.
- **Keys.** A profile names the environment variable its key is in (`api_key_env`), never the key. `tui.sh` fills the Strata ones from `~/.hermes/config.yaml`. The TUI drops every key variable from its environment once read, so commands the agent runs do not see them.
- **What carries over.** The switch applies from the next model call, for the root agent and any delegates. History, working memory and budgets carry over, and tokens keep counting against the same budget. The prompt cache starts cold on the new server.
- **Compaction.** Compaction follows the new model's context window. History already over the new trigger gets a handoff and is compacted at the start of the next turn. History larger than the new window is refused.
- **Records.** A switch records a `model.switched` kernel event (`from`, `to`, `backend`, `base_url`, `model`, `api_key_env`, `context_window`, `compact_at_chars`, `compact_to_chars`), and the call ledger names each call's endpoint and model. A resumed session goes back to the model its last switch chose.

## Workflow Outcomes

A workflow succeeds only if its entry agent's recorded exit code is 0. `aos run --workflow` writes the summary (with `exit_status`) and the report either way, then exits with the entry agent's code when it is non-zero (1 if that code is outside 1–125), with the reason on stderr. Delegated agents' exits reach their parent as `exit_code`, `exit_reason` and `exit_retryable` fields on the `delegate_result`, not only through the child's last message.

A workflow can gate on a verifier with `"verification": {"policy": "required", "verifier": "<agent id>"}`. The verdict is the verifier's recorded exit code (0 passes), read from the kernel, never from a file or a message. Under `required` the workflow succeeds only if the entry agent exits 0 and the verifier's latest exit passed with no work-changing agent (one with `write_file`, `edit_file`, `run`, or write paths) running after it. A verifier's own judgement, a failure from its own exit action whatever `retryable` flag it sets, stands until a work-changing agent runs again, so re-running the verifier cannot shop for a pass. A failure the runtime recorded, such as a step limit or crash, can be cleared by a later run. Exit records carry `origin: "agent"` for exits an agent chose; the runtime sets it and an agent cannot. `best_effort` records the verdict without gating. The engine stores the verdict as a `verification` artifact produced by the workflow root, and in the summary's `verification` field. The checked-in rasterizer and reviewed-merge workflows require verification.

## Delegation Authority

A delegated agent never gets more than its parent may grant. Each agent's `capabilities` are what it may use, and its optional `delegable` set is authority it may only pass on, so a coordinator without `run` can still delegate to workers that need it. Validation rejects any `allowed_agents` edge where the child asks for more than its parent's capabilities and delegable authority together, naming the edge and the excess. At spawn the engine also narrows the child's tools, paths and mounts to what the parent's kernel record allows. The entry agent's sets, plus the run request, are the trust root. The `delegate` action needs `spawn_agent`, and `send`, `publish` and `subscribe` need `message_agent`; tool calls and actions share one check. Every denial is logged as a `capability.denied` kernel event.

## Kernel State

Kernel state lives under `<state-dir>/kernel` and is reachable only through `kernel.New`. The event log (`events.json`) is the source of truth: every state change appends its event, with the data needed to rebuild it, before the process table (`processes.json`) is written, and the table is repaired from newer events whenever it is read. Each update holds an exclusive lock (`flock` on `kernel.lock` on unix), so `aos run`, `aos-tui` and other processes can share a state directory, and event IDs stay gap-free and totally ordered across them.

## Artifacts

Artifacts under `<state-dir>/artifacts` are content-addressed: the index records each payload's sha256 and every read verifies it, so a payload changed on disk fails with `ErrArtifactTampered`. This makes payload edits evident; it does not stop an actor who can write the state directory itself, who could also rewrite or drop the recorded hash (entries without one are trusted as pre-hashing legacy). Keeping agents out of the state directory is the job of confining `run`. Payloads and the index are written atomically under a cross-process lock. An artifact's parents must exist and its producer must be a process the kernel knows, and runs link their artifacts: checkpoints chain to the previous one, records name the checkpoint they ran under, and the `workflow_summary` points at every `agent_run` and the verdict. Runtime types such as `checkpoint`, `verification` and `memory_segment` are reserved; agents publish under `published.<name>`. A process resumes only from its own checkpoints (or its `resumed_from` chain), and compacted history lives in runtime-owned `memory_segment` artifacts that agents find with `search_memory` and read back with `read_memory`. Every agent may call both and every prompt lists them; after a compaction the task header indexes the archive: which messages each segment holds, when it was archived, the tools called and the paths given to the file tools, with no prose summary. `search_memory query` returns up to 10 hits, newest first, in at most 8 KB; `read_memory from to` returns whole messages by number, up to 32 KB, with `next_from`; `read_memory artifact_id offset` still pages through a segment 64 KB at a time. None of them can read another process's memory or undo the compaction. Every message in a session carries a sequence number that survives compaction, Resume and Recover, and each segment records the range it holds. Compaction starts at the trigger and archives the oldest whole turns (an action stays with its results) until history is at half of it, so the next compaction is that far away; each one is a `memory.compacted` kernel event, marked `oversized` when the latest turn alone is over the target and `adopted` when a run replayed after a crash reuses the segment it had already written. A `run` result's stdout and stderr share the 64 KB output cap, and no recorded tool result exceeds 68 KB. A `read_memory` or `search_memory` result is archived as a one-hop reference to what it read, never as a copy, so segments do not nest; a copy in a segment written before this is read back as that reference. The runtime alone decides what history the model sees; the backend sends it verbatim. `aos probe-memory --workflow W --probes P <state-dir>` resumes agents from chosen checkpoints, on a copy of the state directory and with no tools but `search_memory` and `read_memory`, asks each probe's question several times and scores the answers (see `memory-system-design.md` and `bench/memory/`).

## Run Record

A run keeps its own record on disk as it goes, so a killed run can still be reconstructed. Under `<state-dir>/run`:

- `trace.jsonl` holds the engine trace (start, delegate, tool calls, exit, `capability_denied`, and so on). Records that match a kernel event, such as start, exit, runtime errors and denials, name it in `kernel_event`.
- `calls.jsonl` holds two lines per model call, so a call cut off by a kill still shows.
  - The **start** line (`"phase":"start"`) is written and synced before the request goes out. It holds the process and agent, the step, the seconds the run had used when the step began (`elapsed_s`), the run's `run.started` event (`run`), the request params, and the sha256 of the exact request body.
  - The **outcome** line (`"phase":"outcome"`, naming its start in `start`) is written when the call returns. It holds usage (prompt, cached and completion tokens), latency, error, and the raw reply (`response`).
  - A start no outcome names was in flight when the process died. `aos inspect` counts such calls as `in_flight`.
  - Prompts are stored by delta: a start records only the messages added since its process's previous start (`base` is that start's seq). So the system prompt is written once, and `inference.RebuildRequestBody` reproduces any request byte for byte.
  - Repair calls are flagged `repair`.
  - Ledgers from before the split have one line per finished call, without the reply.

Both are append-only JSONL. Each record gets a `seq` assigned under a file lock (`*.lock`), and the line is fsynced before the call returns. Several writers, including other processes, keep `seq` gap-free, and a kill can leave at most a torn last line, which readers skip. These files are diagnostic; the kernel event log stays the source of truth.

`aos inspect <state-dir>` rebuilds a run from these files and the kernel and artifact stores, without a model. It shows the process tree with exit records, which processes never exited (and whether a run still holds the state directory, or they are orphans), each process's latest checkpoint, the leases held, the verification verdict, token usage per process and in total, and the trace tail. Use `--json` for machine-readable output and `--tail N` to set the trace length (default 20); flags may come before or after the path. The Harbor adapter reads its token counts from `aos inspect --json`, and on a timeout from `calls.jsonl` directly.

If `aos run --workflow` stops on a Go error after the root process started (cancellation, a failed store write), it still writes the report and the summary JSON, with the error in `error`, and it records an exit for every process it started.

A workflow run records what it runs under before anything runs:
- a `run_spec` artifact holding the workflow spec, as the engine ran it, and the request;
- a `run.started` kernel event on the root process holding:
  - the run's budget (`budget_tokens`, `budget_seconds`, `budget_strict_time`);
  - the compaction and retry settings;
  - the build (`build_version`, set with `-ldflags -X main.version=…`; `build_revision` when stamped; and the binary's `build_sha256`);
  - the backend's endpoint, model and sampling, but never its key.

On SIGTERM or SIGINT, `aos run` stops the run as a strict budget does. The call or command in flight is cancelled, and each process exits `{1, "terminated by signal (SIGTERM)", retryable}` with a final checkpoint. The call gets its outcome line, and the summary is written. A second signal kills. The Harbor adapter sends SIGTERM when Harbor's agent timeout cancels it, and waits up to 15 s, so the ending is recorded before the logs are collected.

`run` records what a command did to the files of the `workspace` mount, but not in the output the agent sees, so prompts do not change. It compares each path's size and modification time before and after, and records the result in the `tool_result` artifact (`changes`: `created`, `modified`, `deleted`, capped at 200 with a count of the rest). The trace also shows it, as `files=+a,~b,-c`. **Writes outside the workspace mount are not covered.** A write that keeps a file's size and modification time is not seen. A workspace over 50,000 entries is recorded as `unknown`.

`aos replay <state-dir>` re-executes a recorded single-agent workflow run against its record.
- **What runs for real:** the runtime, kernel, artifact store and prompt assembly.
- **What comes from the record:**
  - the model's replies, from the outcome lines;
  - the world tools' results, from the run's `tool_result` and `tool_error` artifacts;
  - the clock: each step's recorded `elapsed_s`, and the kernel's and store's recorded timestamps.
- **The check:** every request must hash to the recorded `request_sha256`, and the kernel events, process table and artifacts must come out the same. A checkpoint's usage seconds are left out, because they come from the wall clock at save time.
- **Exit codes:** 0 when identical, 1 at the first divergence, which is reported by kind, call, step and message diff. Exit 2 means the run cannot be replayed: a multi-agent run, a ledger from before replies were recorded, or no `run.started`.
- **A killed run** replays up to the call in flight and is compared as far as its record goes.
- **Options:** `--json` prints the report as JSON, `--keep DIR` keeps the replay's own state, and `--unpinned-clock` shows what pinning the clock prevents.
- A replay by a different build says so.
- The TUI's sessions are not replayable yet.

## Run Lifecycle and Recovery

Every agent run ends in one of two ways. Either the process exits with a recorded status, or it parks in a checkpoint: `ready` after a yield or a cancel, `blocked` after `wait`. Exits the runtime decides (step limit, refusals, errors) carry no origin and name the process's latest checkpoint in `checkpoint`. A run can also fail on an error with nothing recorded: the artifact store, the bus or a kernel call fails. In that case one closer exits the process `{1, error}`. The exit is retryable unless the error is a contract violation, such as a tampered or foreign artifact, a reserved type or an invalid transition. If the kernel itself cannot be written, nothing can be recorded; `aos reap` closes the process once the run is gone. The workflow root is `running` for as long as its workflow runs.

A run of a process that has already exited is refused with `ErrProcessExited`, before any model call or artifact. `Recover` (and `Resume` of an exited process) is refused with `ErrNotRecoverable` unless the process has exited with a retryable status, whether an agent or the runtime recorded it. A live process can never be recovered, so the same work never runs twice. The checkpoint must still be the process's own. The recovered process takes over its predecessor's unexpired leases.

`aos run --workflow` and `aos-tui` each hold the state directory's lock (`<state-dir>.lock`) while they run. A run that dies, for example from SIGKILL or a power loss, leaves processes that look live. `aos reap <state-dir>` refuses while any run holds the lock. Otherwise it exits every process without an exit record with `{1, "orphaned: owning run is gone", retryable}`, naming its latest checkpoint, so each one can be recovered. On platforms without `flock`, reap refuses, since it cannot tell whether a run is alive.

## Confined `run` and Network

`run` executes inside an OS sandbox derived from the calling process's capabilities.
- On macOS that is `sandbox-exec` with a profile generated per call.
- On Linux it is Landlock (ABI 4 or later when network is denied), applied by a re-exec helper with raw syscalls.
- Elsewhere there is no sandbox.

A confined command:
- **reads** its read scopes that lie within its mounts, plus the system paths a toolchain needs. On macOS these are `/usr`, `/bin`, `/sbin`, `/System`, `/Library`, `/private/etc`, `/opt/homebrew`, `/Applications/Xcode.app` and `/dev`. On Linux they are `/usr`, `/bin`, `/sbin`, `/lib*`, `/etc`, `/opt`, `/proc`, `/sys`, `/dev` and `/run`. It can also read the install prefix of every `.../bin` on `PATH` (never the home directory itself) and anything listed in `AOS_SANDBOX_READ`.
- **writes** only its write scopes within its writable mounts, plus a scratch directory of its own. The scratch directory becomes `TMPDIR`, and `GOCACHE` and the xcrun cache are pointed there.
- **never reaches the state directory**: it can neither read nor write it, even when a mount covers it. The file tools refuse it too.
- **cannot write read-only mounts**, even inside a writable one.
- **has no network** unless the set has `allow_network`. On macOS only a short list of system services is reachable, which leaves out name resolution unless network is allowed.

When a command cannot be confined (no sandbox on the platform, or a policy Landlock cannot express), `run` is refused (`capability.denied`, capability `unconfined_run`). The only exception is a process whose capabilities include `"unconfined_run": true`. That flag is meant for agents isolated some other way, such as `workflows/terminal-bench.json` inside its container. It attenuates like any capability, and it shows in the process record, in `aos inspect` (`[UNCONFINED run]`) and in the HTML report. `run` reports its `confinement`. Its `timeout` is clamped to `max_run_seconds` (default 600), and the result records the bound.

`web_fetch` and `web_search` require `allow_network`. `web_fetch` refuses the model endpoint, and any address that is loopback, private, link-local, CGNAT or multicast, unless the set's `network_allow` names the host, the address or a CIDR range containing it. The check runs at every dial, including redirects, against the address actually dialled. Proxies are not used. The body is read up to 4 MiB and no further.

Not covered: CPU, memory, process count and disk usage limits (the timeout bounds wall time). On macOS, metadata (`stat`) is readable everywhere, GPU access (`iokit-open`) is allowed, and Metal prints benign `sandbox_extension_issue_file failed` warnings. On Linux, Landlock does not cover UDP, so DNS queries still go out. Linux Rosetta-translated (amd64-on-arm64) binaries fail under confinement. And a command can still use whatever the allowed system services do on its behalf.

The gated rasterizer workflows give the verifier read-only access to the workspace and a writable `<workspace>-verification` directory, which no other agent can write. With `run` confined, the writer cannot touch the verdict and the verifier cannot change the sources. `aos run --workflow` creates a missing writable mount directory when its parent exists.

## Leases

`acquire_lease` and `release_lease` use `<state-dir>/leases/leases.json`. The file is read and written under a file lock and replaced atomically, so managers in different processes never both hold a resource. A lease ends when it expires, when its owner releases it, or when its owner exits. The owner can renew a lease, which extends its TTL and keeps its token. Every new grant, takeover or transfer gets a fencing token, strictly increasing per resource, which write sites can compare (nothing enforces it yet). Grants, renewals, releases, takeovers (`reason: expired` or `owner_exited`) and transfers are logged as `lease.*` kernel events, and `aos inspect` lists the live leases. Files from before versioning, a bare map of leases, are still read.

## Agent Messaging

Agents message each other through the mailbox bus in `<state-dir>/bus`. That covers `send` to a PID, `publish` to a topic and `subscribe`, and they need the `message_agent` capability. `aos run --workflow` and `aos-tui` both wire the bus. Its files (`state.json`, `mailboxes.json`, `subscriptions.json`) are read and written under `bus.lock` and replaced atomically, so buses in several processes never lose or duplicate a message.

Delivery is at least once, and exactly once into a session:
- Every message has a stable ID (`msg-NNNNNN`).
- Receiving marks a message delivered (`deliveries`, `delivered_at` in `mailboxes.json`) but keeps it. The runtime acknowledges it only after a checkpoint records it, and the acknowledgement removes it.
- A run that starts from a checkpoint, after a crash, a cancel or a resume, gets every unacknowledged message again. The session records the IDs it holds (`mailbox_seen`) and drops the repeats.
- Mailboxes written before acknowledgements load as pending.

Mail to an unknown or exited process is refused. The sender sees a tool error, and the refusal is an `ipc.rejected` event. An exited process loses its subscriptions. `Recover` moves the old process's unacknowledged mail and subscriptions to the new one, along with its leases. Sends, deliveries, redeliveries, acknowledgements and transfers are `ipc.*` kernel events, and `aos inspect` lists mail not yet acknowledged.

A message from another agent reaches the model as `message from <agent> (<pid>) [<id>]: …`. The system prompt ranks it below the task and the user, so a peer cannot redefine another agent's work. Only the user's own lines speak as the user.

A delegated agent that uses `wait` is parked: blocked, with its checkpoint, not exited. Its delegator gets `delegate_result … status=waiting question=…`. Delegating to that agent again with the answer as the goal resumes the same process, which reads `Answer from <agent> (<pid>), who delegated your task: …`. When an agent finishes, any child it left parked is closed with `{1, "abandoned while waiting", retryable}` and its checkpoint, so nothing stays blocked. An entry agent that waits has nobody to answer it, so it is closed the same way and the workflow fails with its question.

## Budgets

A capability set may carry a budget: `"budget": {"tokens": N, "seconds": N, "strict_time": true}`. `aos run --workflow` sets the whole run's budget with `--token-budget N`, `--time-budget 15m` and `--strict-time-budget`. Zero means unlimited.

- **Tokens** count the prompt and completion tokens of every model call, repairs included. Cached prompt tokens are counted too, because the server still processes them. The limit is checked before each call and the reply's `max_tokens` is capped to what is left, so the budget can be overshot by at most one call's prompt. A spent budget ends the process with `{1, "token budget exceeded: <used> of <limit> tokens used (budget of <owner>)", retryable}` and a checkpoint.
- **Time** is wall-clock time spent running, not time parked between turns. It is **advisory by default**: each step the agent sees the time used and left, and once, with less than a tenth left, it is told to commit its best answer now. Only `strict_time` (`--strict-time-budget`) enforces it. When a strict budget runs out, the in-flight model call or command is cancelled (a command's whole process group is killed) and the process ends with `{1, "time budget exceeded: <N>s strict budget of <owner>", retryable}`. Tokens get the same one-time warning.
- **Steps:** every run has a step limit (`max_steps`, default 32), so the agent is not told it every step, which would add a message to every run. In the last tenth of its steps (at least the last step) each step carries `budget steps=<limit> used=<U> left=<L>`, counting this step as left, and the first of them a one-time warning to commit now. In the TUI the allowance, and the warning, start again with each message you type.
- **Hierarchy:** a delegate's budget is at most its parent's (strictness carries down), and its consumption counts against every ancestor and the run. The tightest remaining budget in the chain binds it, and an exit names whose budget ran out. A spec whose child asks for more than its parent may grant fails validation. A child that names no budget inherits its parent's.
- **Persistence:** steps, tokens and time are saved in each checkpoint, so Resume and Recover continue against the same budget and step limit. Running out and recovering does not reset them. An operator grants more explicitly with `RunOptions.Extend` (steps, tokens, seconds), recorded in the session. In the TUI each message you type starts a fresh step allowance; tokens and time keep counting.
- **Visibility:** `aos inspect` and the HTML report show each process's budget and consumption (`steps 7 · tokens 1200/5000 · time 95s/900s strict`), and the exit reason names the budget that ended it.

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
- `AOS_LLM_CONTEXT_WINDOW` model context size in tokens; when set, workflow runs compact agent history at `AOS_COMPACT_RATIO` (default `0.8`) of it instead of the small built-in limits; a window too small for that to leave room for one 64 KB tool output between compactions is refused at startup

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
