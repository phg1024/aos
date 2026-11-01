# Replay Design

Status: proposal, 2026-10-31, written at the request of the aos book session
for Chapter 6 (Observability and Replay). Nothing here is implemented
except what "Today" says is. The user decides what to build after reading.

## Why

"Replay" means three different things in conversations about AOS, and the
code supports only one of them:

- **In the code**, replay means `tx.replay` in `internal/kernel/store.go`. It
  applies the `process.*` events that the process table has not caught up
  with, which repairs the table after a crash between the two writes. It
  rebuilds kernel state from the event log. It runs nothing.
- **In the book**, the natural claim is stronger. AOS keeps state in durable
  artifacts and logs, not in model context. So a run should be
  re-executable from its record, and it should reach the same state.
- **In eval**, the Terminal-Bench rerun (`c9002d3`) needed to explain trials
  after the fact. It found gaps where the record could not answer a question
  without a hand-check.

This note defines the levels, says what each costs on today's records, lists
the gaps and hazards, and recommends what to build.

## Today's Record

A run's state directory holds:

| Record | What it holds | Authority |
|---|---|---|
| `kernel/events.json` | `process.spawned/transition/exited`, `ipc.*`, `lease.*`, `capability.denied`, `backend.retry`, `memory.header/compacted/handoff/notice` | Source of truth; the process table is derived from it |
| `kernel/processes.json` | The process table, with `last_event_id` | Derived |
| `artifacts/` | Checkpoints (the whole `Session` after each step), `tool_result`, `tool_error`, `backend_error`, `memory_segment`, `working_memory`, mailbox and delegate records, each with parents | Durable; checkpoints chain |
| `run/calls.jsonl` | One `CallRecord` per model call: pid, params, the messages added since that pid's previous call (`base`), `request_sha256` of the exact body, usage, latency, error | Diagnostic (runlog's own words) |
| `run/trace.jsonl` | The trace: tool calls, tool errors, messages, exits, with kernel event IDs where one exists | Diagnostic |

`aos inspect` reads all of these without calling a model.

Three facts about the ledger matter below. They were checked against trial
seq 25 of the rerun (`raman-fitting__vNvP9FK`: 27 calls, 26 checkpoints).

1. **The ledger records requests, not replies.** `CallRecord` has no response
   field.
   - The reply can still be recovered. The next call of the same pid opens
     with it, as the assistant entry the runtime appended. That entry is the
     canonical action record (`{"action":…}`) or the message text, not the
     raw model output.
   - A malformed reply appears raw in the repair call that follows it
     (`Repair: true`, "Malformed output: …").
   - A process's last reply is in no later request. It is in the final
     checkpoint, if one was saved.
2. **A call is recorded after it returns.** `recordCall` runs after `send`.
   - A call cancelled through its context, as a strict budget does, is
     recorded with its error.
   - A call in flight when the process is killed is never recorded.
3. **Requests are hashed.** `RebuildRequestBody` reproduces a body exactly,
   so any re-execution can be checked byte for byte against
   `request_sha256`.

## Levels

### (a) Reconstruction: inspect and explain, no execution

This level answers what happened, from the record alone.

- **Today:** the kernel's event-log replay of the process table, `aos
  inspect`, and the HTML run report. Eval's reconstruction probe works at
  this level.
- **Cost:** read-only, and cheap.
- **Missing:** effects that no record holds (gap 1), calls in flight at a
  kill (gap 3), and links between records. A call record does not say which
  step it was. A `tool_error` artifact does not say which call it answers.
  Eval had to compute "step #19 at 628 s" for seq 27.

### (b) Deterministic re-execution with recorded replies and recorded tool results

The runtime, the kernel, the artifact store and prompt assembly run for real.
Everything nondeterministic comes from the record:

- **Model replies:** a replay transport behind `VLLMBackend` returns the
  recorded reply for each call. `VLLMConfig.HTTPClient` is already
  injectable, so the whole stack runs except the network: prompt assembly,
  request building, parsing and repair.
- **World tools:** `run`, `read_file`, `write_file`, `edit_file`,
  `list_dir`, `find_files` and `web_fetch` return their recorded
  `tool_result` or `tool_error` payloads, in order.
- **Runtime tools run for real:** memory tools, delegation and leases.
  They are functions of the store, which is what replay tests.
- **The clock and retry sleeps** are pinned (see Hazards).

The verdict has three parts:

1. Every request the replay sends hashes to the recorded `request_sha256`.
2. The kernel reaches the same process table and the same per-process
   sequence of event types and data, timestamps aside.
3. The checkpoint chain holds the same sessions.

The first part is the strong one. A request is a function of the session,
the header and the system prompt, so equal hashes at every call mean the
runtime saw and decided the same things.

- **Cost:** moderate. It needs three seams that do not exist yet:
  - an injectable clock for the budget account, the artifact store and the
    kernel (the account already has an unexported `now`);
  - an injectable sleeper for retry backoff;
  - a stub behind `tools.Runtime`, which is a concrete type today.

  It also needs a comparator and a command. A replay runs in seconds,
  without a model, a container or a GPU.
- **Missing records:** the raw reply and each process's last reply (fact 1),
  a start record for each call (fact 2, gap 3), and the clock reading at
  each step. For multi-agent runs, it also needs the order in which
  processes stepped.

### (c) Re-execution with live tools and recorded replies

The model is recorded, and the world is live. Tool results are compared with
the record, and the first divergence is the finding. Use this to ask whether
a changed environment, tool or sandbox would have shown the agent something
different.

- **Cost:** high.
  - It needs the original environment: the image digest, the environment
    variables and the initial files.
  - Outputs that differ on every run need normalising rules: durations,
    PIDs, temporary names and ports, the noise seen in the least-privilege
    probe results.
  - After the first real divergence, the recorded replies no longer answer
    what the agent saw. Replay stops there.
- **Missing records:** the environment's identity, and what each command
  changed (gap 1). Without the second, a divergence in the world is found
  only when a later read happens to show it.

### Which level fits the contract

- **The kernel's contract is already a replay contract, at level (a).** The
  event log is the truth, the table is derived, and replay rebuilds it.
  This is implemented and tested.
- **Level (b) is the one that tests AOS's own claims.** AGENTS.md asks for
  a deterministic kernel independent of inference, with state in durable
  artifacts and logs. If inference and the world are the only
  nondeterministic inputs, recording them should make the rest a pure
  function, and (b) checks exactly that. This is deterministic replay in
  the record-and-replay sense: record the inputs from outside, re-run the
  rest.
- **Level (c) is an evaluation tool, not a contract.** It is worth having
  only once (b) holds. Otherwise every divergence is ambiguous: it could
  come from the world, or from AOS itself.

## Known Record Gaps

| Gap | Needed by | Smallest record change |
|---|---|---|
| 1. `run` does not record which files it changed (seq 27: the answer was written by a `python3 - <<'EOF'` heredoc, found only by hand) | (a) and (c). Not (b): it replays the tool result and never touches the world | Snapshot the workspace mount before and after each `run`, as path, size and mtime |
| 2. A tool refusal shows only as a trace `tool_error` (seq 25: `update_memory` refused an evidence field) | (b) needs the result the agent saw, and it is already durable. (a) needs it findable | Add the call's arguments to the `tool_error` payload. List tool errors in `aos inspect`. Put the artifact ID in the trace record |
| 3. A model call killed in flight is logged late or not at all (four ±1 request-count pairs) | (a) for usage and for how a run ended. (b) needs it to know whether the record ends mid-call. It does not need it to replay what came before | Split each call record into a start and an outcome |

### Gap 1: files changed by `run`

**Smallest change:** snapshot the workspace mount before and after each
`run`, recording path, size and mtime. Record what was created, changed or
deleted as a capped list (for example 200 paths plus a count of the rest).
If the walk exceeds a bound, record "unknown" instead.

- Store it in the `tool_result` artifact, but not in the output the agent
  sees, so prompts and replays do not change.
- Writes outside the workspace mount are not covered. Terminal-Bench grants
  `/`, so `apt install` would not show.

### Gap 2: tool refusals

The refusal is durable, though not as a kernel event.

- `update_memory`'s refusal in seq 25 is artifact `art-000049`, of type
  `tool_error`, with metadata `tool=update_memory`, the error text as its
  payload, and the step's checkpoint as its parent.
- No kernel event records it, and none should. A kernel event records an
  authority decision. That is why `capability.denied` is one and a
  malformed argument is not.
- What is missing is the call's arguments. Today they are only in the
  assistant entry of the next request or checkpoint. Also, nothing lists
  tool errors.

### Gap 3: calls killed in flight

**Smallest change:** split each call record into two lines.

- A start line, written and synced before the request is sent: pid,
  messages or base, params and `request_sha256`.
- An outcome line after the call returns: usage, latency, error and the
  raw reply.

A start line with no outcome means the call was in flight when the process
died. The cost is one more synced append per call, which is small next to
model latency.

The ±1 pairs eval saw came from its relay, which logs a request when the
reply ends. AOS's own ledger has the same blind spot, and worse: the killed
call is never logged at all. The killed process also stays `running` in the
kernel, since nobody records its exit. A reconstruction can infer "killed"
from a running process plus an unpaired start line.

### Further gaps, found while writing this

- **Raw replies.** Put the reply in the outcome line (above). This also
  gives each process's last reply, which today is only in a final
  checkpoint.
- **Step and clock per call.** The runtime knows the step number and the
  account's elapsed seconds when it calls. Passing both through `CallInfo`
  into the call record links calls to steps, which eval had to work out by
  hand, and gives replay its clock (see Hazards).
- **Run identity.** Record the AOS build (version and commit) and the hash
  of the workflow spec in the record. A replay on a different build should
  diverge, and the report should say that this is why.

## Determinism Hazards

| Hazard | Where it enters | How a replay detects or pins it |
|---|---|---|
| Wall clock | Budget notes (`budget time=… used=13s`), the strict-stop timer, the advisory warning's step, `archived <time>` in the archive header, artifact `created_at`, kernel event times | Use an injectable clock everywhere. In replay it returns the recorded reading for each step (the per-call clock above; today it can be parsed from the time note). Timestamps outside prompts are left out of the comparison. A strict stop is replayed as recorded, not recomputed: the replay ends where the record ends and checks the exit reason |
| Compaction cut points | Character-based (`historyChars`), so deterministic given the messages | A time note's digit count changes history length, so an unpinned clock can move a cut by a step. Pinning the clock fixes that. `memory.compacted` data and segment message ranges are compared |
| Token usage | Comes from the server. It drives the `max_tokens` cap, token notes, the warning and the token-budget exit | The replay transport reports the recorded usage. The cap is in the request params, so the hash checks it |
| Retries | A failed call, then `backend.retry`, then a sleep | Failed calls are in the ledger with their error, and replay returns the same errors in order. Sleeps are zeroed through the sleeper. `backend.retry` events are compared. An attempt killed in flight is the gap-3 case |
| Inbox ordering and concurrency | Processes that run at the same time interleave artifact and event IDs, and bus delivery order varies | Compare per process, not by global ID. For a faithful multi-agent replay, record the schedule: the global order of steps across pids. Ledger seq and kernel event IDs give most of it. The first slice is limited to single-agent runs; Terminal-Bench's solver is one |
| Tool output noise | Durations, PIDs, temporary names and ports in `run` and `web_fetch` results | Not an issue at (b): results are recorded. At (c), normalising rules apply, and replay stops at the first divergence that survives them |
| Go map order | Tool outputs rendered with `%v`, and JSON | Both sort map keys, so they are deterministic. A round-trip test guards this |
| Build and spec | A changed prompt, tool or header | Intended to diverge. Run identity names the cause |

## Recommendation

### What the book can describe as Implemented today

- Kernel state rebuilt from the event log (`tx.replay`). Crash recovery
  depends on it, and it is tested.
- Reconstruction from the durable record (`aos inspect`, the run report),
  with the gaps above stated as limits.

### Smallest slice worth building

This slice is single-agent deterministic re-execution, level (b). It has
two parts.

1. **Ledger changes:**
   - start and outcome lines;
   - the raw reply in the outcome;
   - step number and elapsed seconds per call;
   - run identity.

   These are small, they help reconstruction on their own, and eval's
   probe can count the gaps closing.
2. **`aos replay <state-dir>`** for runs with one agent process:
   - Seams: the clock, the sleeper, the tools stub and the replay
     transport.
   - Verdict: request hashes at every call, the process table, per-process
     kernel event types and data, and the checkpoint chain.
   - Output: the first divergence, as call seq, step and a diff of the
     differing message.

Once this is built, the book can claim it as implemented: a recorded
single-agent run re-executes against its record and reaches the same state,
and any change to AOS that alters what an agent would have seen shows up at
the first call it affects.

### Stays Proposed

- Multi-agent replay with a recorded schedule.
- Level (c).
- Gap 1's file-change record. It is small and independent, so it can be
  built whenever eval needs it, but nothing in (b) depends on it.
- Gap 2's argument and listing improvements. They are convenience; (b)
  already has what it needs.

### Tests that would prove it

- **Round trip:** run a scripted single-agent workflow (`FakeBackend`, real
  file tools in a temp dir) that writes, fails a tool call, retries a model
  call, compacts with a handoff, and exits. Replay its state directory.
  Every request hash matches, and the table, events and checkpoints are
  equal.
- **Divergence is caught where it happens:**
  - change one recorded tool result, and replay reports the next call;
  - change the system prompt, and it reports call 1;
  - delete a ledger line, and it reports a missing reply at that call.
- **Time-budget run:** with a pinned clock, the time notes, the warning's
  step and a strict stop replay as recorded. With an unpinned clock, the
  test shows the divergence the pin prevents.
- **Token budget:** the `max_tokens` caps and the token exit replay from
  recorded usage.
- **Killed run:** a run killed during a call leaves an unpaired start line.
  Replay reproduces everything before it, then reports that the record ends
  in flight.
- **Real data:** one Terminal-Bench trial, re-recorded with the new ledger,
  replays clean. This is the golden test on real model output.
- **Ledger format:** start and outcome pairs survive a torn last line, as
  `runlog` promises. `aos inspect` totals usage from outcome lines and
  counts unpaired starts.

## Open Questions

- Should replay also run on today's ledgers? It could recover replies from
  the next request and clock readings from the time notes, at the cost of
  code that the new ledger makes unnecessary.
- How much ledger growth from raw replies is acceptable for long runs, and
  should the reply be optional?
- Whether multi-agent replay should serialize steps by the recorded order,
  or compare processes independently and accept weaker cross-process
  checks.
