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

A delegated agent never gets more than its parent may grant. Each agent's `capabilities` are what it may use, and its optional `delegable` set is authority it may only pass on, so a coordinator without `run` can still delegate to workers that need it. Validation rejects any `allowed_agents` edge where the child asks for more than its parent's capabilities and delegable authority together, naming the edge and the excess. At spawn the engine also narrows the child's tools, paths and mounts to what the parent's kernel record allows. The entry agent's sets, plus the run request, are the trust root. The `delegate` action needs `spawn_agent`, and `send`, `publish` and `subscribe` need `message_agent`; tool calls and actions share one check. Every denial is logged as a `capability.denied` kernel event. A file tool called without a path (`write_file`, `read_file`, `edit_file`) gets an argument error, `<tool> requires path`, like any other missing argument: nothing was refused, so no event is logged. `list_dir` and `find_files` take an empty path as the first mount's root.

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
  - the run's budget (`budget_tokens`, `budget_seconds`, `budget_strict_time`, and `budget_time`: none, advisory or strict; a limit of 0 means none);
  - the compaction and retry settings and each agent's `max_steps_<id>`, as they take effect, with defaults filled in, and `compaction_source` and `retry_source` saying whether each was configured or is the default;
  - the build (`build_version`, set with `-ldflags -X main.version=…`; `build_revision` when stamped; and the binary's `build_sha256`);
  - the backend's endpoint, model and sampling, never its key. A sampling field the run leaves unset reads `server default`, because it is not sent.

On SIGTERM or SIGINT, `aos run` stops the run as a strict budget does. The call or command in flight is cancelled, and each process exits `{1, "terminated by signal (SIGTERM)", retryable}` with a final checkpoint. The call gets its outcome line, and the summary is written. A second signal kills. The Harbor adapter sends SIGTERM when Harbor's agent timeout cancels it, and waits up to 15 s, so the ending is recorded before the logs are collected.

`run` records what a command did to the files of the `workspace` mount, but not in the output the agent sees, so prompts do not change. It compares each path's size and modification time before and after, and records the result in the `tool_result` artifact (`changes`: `created`, `modified`, `deleted`, capped at 200 with a count of the rest). The trace also shows it, as `files=+a,~b,-c`. **Writes outside the workspace mount are not covered.** A write that keeps a file's size and modification time is not seen. A workspace over 50,000 entries is recorded as `unknown`.

`aos replay <state-dir>` re-executes a recorded single-agent workflow run against its record.
- **What runs for real:** the runtime, kernel, artifact store and prompt assembly.
- **What comes from the record:**
  - the model's replies, from the outcome lines;
  - the world tools' results, from the run's `tool_result`, `tool_error` and `tool_cancelled` artifacts;
  - the clock: each step's recorded `elapsed_s`, and the kernel's and store's recorded timestamps.
- **The check:** every request must hash to the recorded `request_sha256`, and the kernel events, process table and artifacts must come out the same. A checkpoint's usage seconds are left out, because they come from the wall clock at save time.
- **The record is verified first:** every payload with a recorded sha256 must match the index, as the store requires on each read. A changed payload makes the run not replayable, naming the artifact (`ErrArtifactTampered`), before any call is replayed; otherwise it would surface as a divergence at the first call it feeds. Index entries without a hash predate hashing and are trusted.
- **Exit codes:** 0 when identical, 1 at the first divergence, which is reported by kind, call, step and message diff. Exit 2 means the run cannot be replayed: a multi-agent run, a ledger from before replies were recorded, no `run.started`, or a payload that no longer matches its hash.
- **A killed run** replays up to the call in flight and is compared as far as its record goes.
- **Older records:** a `run.started` from before settings were recorded as they take effect (builds before `d9b3514`, such as `v0.1.1`) has no `*_source`, `budget_time` or `max_steps_*`, and records a default setting as zero. The replay writes the current format, so that one event is compared by meaning: the same budget, the same compaction and retry settings as they take effect, and every other field exactly. The report says so.
- **Records without fencing:** a `run.started` without `lease_fencing` predates lease fencing; the replay neither fences file writes nor shows `acquire_lease` tokens, and the report says so (`legacy_leases`).
- **Leases:** a lease's expiry comes from the kernel clock, the same reading that stamps its `lease.*` event, so a replay, which reproduces event times, reproduces expiries, takeovers and `acquire_lease` results. A record whose `run.started` lacks `lease_clock` and that granted a lease took its expiries from the wall clock; it is refused as not replayable (exit 2), naming the first grant.
- **Records without intents:** a record that ran world tools with no `tool.started` event was made before intents existed. It is replayed without them (`NoToolIntents`), so its kernel events and artifacts come out as the original's, and the report says so (`legacy_tool_intents`). A record with intents is replayed with them and compared byte for byte.
- **Options:** `--json` prints the report as JSON, `--keep DIR` keeps the replay's own state, and `--unpinned-clock` shows what pinning the clock prevents.
- A replay by a different build says so.
- The TUI's sessions are not replayable yet.

## Run Lifecycle and Recovery

An agent in a workflow spec may carry a restart policy, for example `"restart": {"max": 3, "within": "10m", "backoff": "1s..30s", "total": 10}`; those are also the defaults for any field left out (total defaults to 10, or `max` if larger), so a policy always bounds its restarts. `max` and `total` are at most 100, `total` is at least `max`, and `within` must be at least 1s and outlast the backoffs of `max` restarts. When such an agent exits with a retryable, non-zero status (and the run is not being stopped), the engine waits the backoff, which doubles on each restart in the window, and recovers it through `Recover` from the checkpoint its exit names, as a successor that continues the same budget, leases and mail. Calls the failed attempt left unfinished are reported to it, once, and so are those of an earlier attempt whose handover failed. The restart is a `supervisor.restart` event on the successor. Restarts are counted from the recorded `supervisor.restart` events of the agent's process chain, so the count holds across a wait and a resume and across a new engine on the same state directory. More than `max` restarts within `within`, or more than `total` in all, ends the agent with a final, non-retryable `restart intensity exceeded` exit for its delegator to handle (`supervisor.escalated`); the engine never restarts the delegator. A retryable exit whose budget is already spent (steps, tokens or a strict time budget) is passed on unchanged (`supervisor.not_restarted`). Without a policy nothing is restarted. `aos replay` refuses a run with restarts, since it then has more than one agent process.

Every agent run ends in one of two ways. Either the process exits with a recorded status, or it parks in a checkpoint: `ready` after a yield or a cancel, `blocked` after `wait`. Exits the runtime decides (step limit, refusals, errors) carry no origin and name the process's latest checkpoint in `checkpoint`. A run can also fail on an error with nothing recorded: the artifact store, the bus or a kernel call fails. In that case one closer exits the process `{1, error}`. The exit is retryable unless the error is a contract violation, such as a tampered or foreign artifact, a reserved type or an invalid transition. If the kernel itself cannot be written, nothing can be recorded; `aos reap` closes the process once the run is gone. The workflow root is `running` for as long as its workflow runs.

A run of a process that has already exited is refused with `ErrProcessExited`, before any model call or artifact. `Recover` (and `Resume` of an exited process) is refused with `ErrNotRecoverable` unless the process has exited with a retryable status, whether an agent or the runtime recorded it. A live process can never be recovered, and an exited one is recovered at most once: the kernel spawns the successor and sets the old process's `superseded_by` in one transaction (`Kernel.Supersede`, a `process.superseded` event), so of any number of `Recover` calls, sequential or concurrent, in one OS process or several, one gets the successor and the rest get `ErrNotRecoverable` wrapping `kernel.ErrSuperseded`, naming the successor to recover instead. `Recover` starts from the checkpoint the exit names when given none. Any other checkpoint is a rollback: refused unless `RunOptions.Rollback` asks for it, and recorded as `rollback_from` on the `process.superseded` event. The checkpoint must still be the process's own. The recovered process takes over its predecessor's unexpired leases.

World tools (`run`, `write_file`, `edit_file`) record a `tool.started` intent in the kernel log before they act: the tool, a clipped command or path, and the arguments' sha256. If the intent cannot be recorded, the tool does not run and the run fails retryably. The call's outcome artifact (`tool_result`, `tool_error`, or `tool_cancelled`) names the intent in its `intent` metadata. A call cut off while the run is alive, by a cancel, a strict budget or SIGTERM, records `tool_cancelled` with the files the command had changed, and the agent's note says it started, may have taken effect, and what changed; a command stopped by its timeout records its changes on its `tool_error`. An intent no outcome names is a call whose process died during it. `Resume` and `Recover` record a `tool_interrupted` outcome for each such call of the process they continue and the processes it was recovered from, produced by the process that runs next, and tell the agent before its first step that the call may have taken effect, fully or in part. Each call is recorded once and reaches the history the agent continues once, even when an earlier recovery failed before the agent was told. `aos inspect` lists every call that started and did not finish: still open, cancelled, or interrupted and who was told. AOS cannot know what a cut-off command did beyond the workspace files it compared, and nothing stops a model from repeating the call; the record and the note make the repeat a decision rather than an accident.

`aos run --workflow` and `aos-tui` each hold the state directory's lock (`<state-dir>.lock`) while they run. A run that dies, for example from SIGKILL or a power loss, leaves processes that look live. `aos reap <state-dir>` refuses while any run holds the lock. Otherwise it exits every process without an exit record with `{1, "orphaned: owning run is gone", retryable}`, naming its latest checkpoint, so each one can be recovered. On platforms without `flock`, reap refuses, since it cannot tell whether a run is alive.

## Confined `run` and Network

`run` executes inside an OS sandbox derived from the calling process's capabilities.
- On macOS that is `sandbox-exec` with a profile generated per call.
- On Linux it is Landlock (ABI 4 or later when network is denied) with a seccomp filter on making sockets, applied by a re-exec helper with raw syscalls.
- Elsewhere there is no sandbox.

A confined command:
- **reads** its read scopes that lie within its mounts, plus the system paths a toolchain needs. On macOS these are `/usr`, `/bin`, `/sbin`, `/System`, `/Library`, `/private/etc`, `/opt/homebrew`, `/Applications/Xcode.app` and `/dev`. On Linux they are `/usr`, `/bin`, `/sbin`, `/lib*`, `/etc`, `/opt`, `/proc`, `/sys`, `/dev` and `/run`. It can also read the install prefix of every `.../bin` on `PATH` (never the home directory itself) and anything listed in `AOS_SANDBOX_READ`.
- **writes** only its write scopes within its writable mounts, plus a scratch directory of its own. The scratch directory becomes `TMPDIR`, and `GOCACHE` and the xcrun cache are pointed there.
- **never reaches the state directory**: it can neither read nor write it, even when a mount covers it. The file tools refuse it too.
- **cannot write read-only mounts**, even inside a writable one.
- **has no network** unless the set has `allow_network`. On macOS only a short list of system services is reachable, which leaves out name resolution unless network is allowed.
- **has no NVIDIA GPU on Linux** unless the set has `allow_gpu`, a list of card UUIDs attenuated like the network. A card's own `/dev/nvidiaN` opens, with the shared `/dev/nvidiactl` and `/dev/nvidia-uvm`, and a UUID not on the machine refuses the run. It also makes `/proc` writable, since CUDA will not start otherwise and Landlock cannot grant less: such a command can change `oom_score_adj` and `coredump_filter` of this user's processes. The grant stops at the device: the sandbox has no say in what a process does on the GPU, or between two users of a card, the shared nodes are not per card, and no budget meters GPU use. On macOS every confined command can reach Metal with or without it, and any entry refuses the run, since it names an NVIDIA card that is not there.
- **makes few sockets on Linux.** Landlock does not stop a connection to a Unix socket by path, which would otherwise reach the user's service manager and run anything outside the sandbox, so a seccomp filter decides at `socket()`. IPv4 and IPv6 need `allow_network`, which grants nothing else: no Unix socket, netlink, vsock or packet socket. `allow_gpu` adds seqpacket Unix sockets only, which CUDA cannot start without and which cannot connect to a stream or datagram listener. Connected Unix socketpairs are always allowed, since they reach nothing outside the process tree. `io_uring` and other architectures' syscalls are refused with `EPERM`. A filter that cannot be installed refuses the run. A socket `aos` itself inherited does not reach the command.
- **does not get model API keys**: `AOS_LLM_API_KEY`, `AOS_VLLM_API_KEY` and `AOS_KEY_*` are removed, and no policy can name them back in. Other variables pass through, including any other credential in `aos`'s environment. `aos-tui` also removes each profile's `api_key_env` from its own environment. `unconfined_run` keeps the full environment.
- **stays inside for signals and abstract sockets**: from Landlock ABI 6 it can signal only what is in its own sandbox, and reach only abstract Unix sockets there. Below ABI 6 signals stay open and it is told so on stderr, and `allow_gpu` is refused.

When a command cannot be confined (no sandbox on the platform, or a policy Landlock cannot express), `run` is refused (`capability.denied`, capability `unconfined_run`). The only exception is a process whose capabilities include `"unconfined_run": true`. That flag is meant for agents isolated some other way, such as `workflows/terminal-bench.json` inside its container. It attenuates like any capability, and it shows in the process record, in `aos inspect` (`[UNCONFINED run]`) and in the HTML report. `run` reports its `confinement`. Its `timeout` is clamped to `max_run_seconds` (default 600), and the result records the bound.

`web_fetch` and `web_search` require `allow_network`. `web_fetch` refuses the model endpoint, and any address that is loopback, private, link-local, CGNAT or multicast, unless the set's `network_allow` names the host, the address or a CIDR range containing it. The check runs at every dial, including redirects, against the address actually dialled. Proxies are not used. The body is read up to 4 MiB and no further.

Not covered: CPU, memory, process count and disk usage limits (the timeout bounds wall time). On macOS, metadata (`stat`) is readable everywhere, GPU access (`iokit-open`) is allowed, and Metal prints benign `sandbox_extension_issue_file failed` warnings. On Linux, a command with `allow_network` can send UDP as well as TCP, and one with `allow_gpu` can reach a seqpacket listener at any path this user can write to, such as an MPS control socket. Unix sockets being refused also stops a few ordinary tools: Python's `multiprocessing.Manager` fails, and Pool and Queue already did. On macOS, `allow_network` opens Unix sockets too, `launchctl print` answers a confined command, and an inherited descriptor reaches it. Linux Rosetta-translated (amd64-on-arm64) binaries fail under confinement. And a command can still use whatever the allowed system services do on its behalf.

The gated rasterizer workflows give the verifier read-only access to the workspace and a writable `<workspace>-verification` directory, which no other agent can write. With `run` confined, the writer cannot touch the verdict and the verifier cannot change the sources. `aos run --workflow` creates a missing writable mount directory when its parent exists.

## Leases

`acquire_lease` and `release_lease` use `<state-dir>/leases/leases.json`. The file is read and written under a file lock and replaced atomically, so managers in different processes never both hold a resource. A lease ends when it expires, when its owner releases it, or when its owner exits. The owner can renew a lease, which extends its TTL and keeps its token. Every new grant, takeover or transfer gets a fencing token, strictly increasing per resource, and `acquire_lease` shows it (`token`). Grants, renewals, releases, takeovers (`reason: expired` or `owner_exited`) and transfers are logged as `lease.*` kernel events, and `aos inspect` lists the live leases. Files from before versioning, a bare map of leases, are still read.

`write_file` and `edit_file` are fenced by leases. A lease's resource names a path: an absolute one as is, a relative one under the workspace mount (else the first mount), and it covers that path and everything below it within the process's mounts; a name such as `build-lock` covers only `<workspace>/build-lock`. Before the write, under the lease file's lock, every covering lease is checked: a live lease held by another process refuses the write (`held_by_other`), and so does a lease the writer (or a process it was recovered from) once held whose resource has since been granted again (`stale_token`: acquire it again to write). A writer whose own lease lapsed with no one granted it since still holds the current token. The refusal is `lease.ErrLeaseFenced`, a `lease.fenced` kernel event, and a tool error naming the lease, its holder and the tokens. The lock is held from the check through the `tool.started` intent and the write, so no grant, takeover or transfer by any process can come between them, and a refused write records no intent. `run` is not fenced: what a command writes is known only after it runs, so a command can still write a path another process leases. Writes outside AOS's tools are not fenced either. A run's `run.started` records `lease_fencing: file_writes`; a record without it predates fencing and is replayed unfenced, with `acquire_lease` results without the token (`legacy_leases`). A grant's expiry is the time of its `lease.*` event plus the ttl, read from the kernel clock, so runs with leases replay identically (`run.started` records `lease_clock: kernel`). Checks that record no event read the same clock without advancing it, which in a live run is the wall clock.

## Agent Messaging

Agents message each other through the mailbox bus in `<state-dir>/bus`. That covers `send` to a PID, `publish` to a topic and `subscribe`, and they need the `message_agent` capability. `aos run --workflow` and `aos-tui` both wire the bus. Its files (`state.json`, `mailboxes.json`, `subscriptions.json`) are read and written under `bus.lock` and replaced atomically, so buses in several processes never lose or duplicate a message.

Delivery is at least once, and exactly once into a session:
- Every message has a stable ID (`msg-NNNNNN`).
- Receiving marks a message delivered (`deliveries`, `delivered_at` in `mailboxes.json`) but keeps it. The runtime acknowledges it only after a checkpoint records it, and the acknowledgement removes it.
- A run that starts from a checkpoint, after a crash, a cancel or a resume, gets every unacknowledged message again. The session records the IDs it holds (`mailbox_seen`) and drops the repeats.
- Mailboxes written before acknowledgements load as pending.

A PID names its process chain, the processes linked by `Recover`, and mail and subscriptions belong to the chain:

- Mail to a superseded PID is forwarded to the chain's last process (`ipc.forwarded`), and the sender's result names it.
- Mail or a publish to a process that exited retryably is held, and the sender's result says `held`: the process is not running and may never read it. `Recover` moves it, with the old process's unacknowledged mail, subscriptions and leases, to the new one.
- A chain ends when its last process exits with code 0 or non-retryably, or when its retryable exit is closed (`Kernel.Close`, `process.closed`). The engine closes the exits its restart policy escalates or declines, and `Recover` refuses them. The end of a chain drops its subscriptions.
- Mail to an unknown process or an ended chain is refused. The sender sees a tool error naming the final exit, and the refusal is an `ipc.rejected` event.
- Mail left unread when a chain ends becomes an `ipc.dead_letter` event, its mailbox empties, and the message stays on disk: `aos inspect --dead-letter ID <state-dir>` shows it. A sender whose chain is live gets one `undelivered:` line per ended chain at its next step. A publisher and the operator get the event only. A chain nobody closes has not ended, so its mail just waits: that is the default for an agent with no restart policy.
- Mail stays unacknowledged until a model call has returned with its body. If compaction archives it first, the agent gets an `archived unread:` line, and a `read_memory` result holding the body counts.
- A new message is held to 16 KiB, and a mailbox to 256 unread messages or 1 MiB. A refusal is a tool error that says what the sender can do, a publish skips a full subscriber and reports it, and an operator's line reaches a full mailbox. One step takes in at most 32 KiB of mail, and less under the default compaction policy, where mail over 2,000 bytes comes one message a step. So a full mailbox takes several steps, and mail can be waiting when a model decides. The step says how much with a `runtime notice:`, and a model that exits anyway leaves it unread, as dead letters. A failed call takes on no more mail.
- Larger data goes in a published artifact (`publish_artifact`), and `message_agent` attaches it with `artifacts:`. `read_artifact(id, offset, limit)` reads it in pages of at most 64 KiB, if the caller's chain published it, it came attached to mail the chain received, or a descendant published it. `list_artifacts` lists only that. An ID in a body grants nothing.
- A `Recover` from an earlier checkpoint puts back the mail acknowledged after it, under the same IDs (`ipc.redelivered`, cause `rollback`). A subscription that history no longer shows is told in a `subscribed to:` notice.

Sends, deliveries, redeliveries, acknowledgements, transfers and subscriptions are `ipc.*` kernel events, and `aos inspect` lists mail not yet acknowledged.

A message from another agent reaches the model as a header, `message from <agent> (<pid>) [<id>], <n> lines:`, and then its body with every line marked `| `, so no line of it can pass for a turn, a tool result or a note. Its raw body stays in the `mailbox_message` artifact. The system prompt ranks it below the task and the user, so a peer cannot redefine another agent's work. Only the user's own lines speak as the user: `Bus.Send` refuses the sender `user`, and only `aos-tui` calls `Bus.SendAsOperator`.

Text the runtime quotes, such as a file a tool read or a child's final message, can look like the runtime's own lines. On its way to the model, a later line of such a turn that begins with a reserved opener (`internal/framing`) gets a backslash. Records keep what the tool returned. `edit_file` also accepts `old_text` copied with that backslash. This stops imitation, not persuasion: capabilities are what bound a persuaded agent.

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
