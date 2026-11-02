# Supervision Design

Status: proposal, 2026-11-01, written at the request of the aos book
session for Chapter 7 (Supervision, Leases, and Recovery). It covers the
audit and the design.

**Update, 2026-11-01:** steps 1 and 2 of the slice are built (exclusive
Recover, the exit's checkpoint by default, `tool.started` intents, file
changes on cancel). N1, N2 and N6 are marked Implemented below, and the
audit probes for them now assert the fixed behavior. See
[Implemented: Steps 1 and 2](#implemented-steps-1-and-2).

**Update, 2026-11-02:** step 3 is built for file writes: `acquire_lease`
shows the token, and `write_file` and `edit_file` are fenced. N3 and gap 4
are marked Implemented for file writes, and probe A4 asserts it. `run` is
still not fenced (probe A4b). See
[Implemented: Step 3](#implemented-step-3).

**Update, 2026-11-02 (later):** step 4 is built: a per-agent `restart`
policy in the workflow spec, applied by agentflow through the exclusive
Recover. `internal/supervisor` is reduced to the policy type and is on
`aos run`'s path. N5 and gap 1 are marked Implemented, and probe A9
asserts it. See [Implemented: Step 4](#implemented-step-4). All four
steps of the slice are now built.

## Why

The book's design review (2026-10-28) found that most of Chapter 7 would
have to be marked Proposed. The review listed six gaps. The same day,
`f3eef1d` ("Close every run, guard recovery, make leases cross-process")
closed several of them. Nobody re-checked the review afterwards.

This note does three things:

- It re-runs each claim at `e401e4e` and reports what holds.
- It states the contract the chapter should describe: what a parent owes a
  child, and what the runtime guarantees when a child fails, is killed, or
  its holder dies.
- It names the smallest slice that would make that contract true.

## How the Audit Was Run

Every result below comes from running code at `e401e4e`, not from reading
it. The probes are Go tests behind a build tag, so `go test ./...` does not
run them. They use `FakeBackend`-style scripted replies and need no model:

```
go test -tags supervisionaudit -count=1 -v ./bench/supervision/
```

Each probe logs `FINDING:` lines and fails only when it cannot run. The
in-tree tests cited below were run with `-count=1` at the same commit.
Since steps 1 and 2 were built, the N1, N2 and N6 probes also assert the
fixed behavior and fail if it regresses. The tables below keep the
`e401e4e` results as the "before" picture.

## Today's State

### The six gaps from the review

| # | Review claim (2026-10-28) | At `e401e4e` | How it was checked | Result |
|---|---|---|---|---|
| 1 | The supervisor is not on the path that runs real agents | **Still held** at `e401e4e`. **Implemented** by step 4: `internal/supervisor` is the restart policy agentflow applies, and `go list -deps ./cmd/aos` names it. | `go list -deps ./cmd/aos ./cmd/aos-tui \| grep -c aos/internal/supervisor` | `0`. The only importer of `internal/supervisor` is its own test. |
| 2 | Two lease managers on one state dir both won the lease in 193 of 200 trials | **Fixed** (`f3eef1d`) | Probe A2 repeats the review's experiment: two OS processes, one `Acquire` each at a shared start, 200 trials. In-tree, `TestLeaseExclusiveAcrossProcesses` runs 3 processes × 200 attempts. | A2: both won in **0 of 200**. In-tree: 105 grants, no overlap (an unlocked counter lost no increment), tokens exactly 1..105. |
| 3 | Nothing releases a lease when its holder exits, so a recovered process is locked out until the TTL | **Fixed for recorded exits.** A dead OS process still holds its leases until reaped (see N4). | Probe A3 | After the holder exited, another process acquired the lease: `lease.taken_over`, reason `owner_exited`, token 2. Recovering a process that held a lease gives the lease to the new process: `lease.transferred`, token 1 → 2. The recovered agent's own `acquire_lease` then renews it. |
| 4 | There are no fencing tokens | **Half fixed** at `e401e4e`: tokens were issued, but nothing checked them (see N3). **Implemented for file writes** by step 3; `run` is not fenced. | Probe A4, in-tree token test | Each grant gets a token that strictly increases per resource and is never reissued. But a holder whose lease expired and was taken over (token 2) still wrote the file afterwards. `acquire_lease` does not show the agent its token: its output keys are `held, resource, owner_pid, expires_at`. |
| 5 | `Recover` has no state check | **Fixed.** A guarded Recover was built in `f3eef1d`. It was not exclusive (N1, since fixed by step 1). | Probe A5 | A running process → `ErrNotRecoverable` ("only an exited process can be recovered"). A non-retryable exit → `ErrNotRecoverable`. Another process's checkpoint → `ErrCheckpointNotOwned`. |
| 6 | `runSession` runs a process that has already exited | **Fixed** (`f3eef1d`) | Probe A6 | `RunWithOptions` on an exited PID → `ErrProcessExited`, with 0 model calls, no new artifacts and no file written. |

### New findings

- **N1. Recover is not exclusive.** *(Implemented by step 1: 1 successor
  of 20 concurrent Recovers.)* The guard checks the process being
  recovered, but nothing marks it as recovered. Probe A5 recovered the same
  exited process twice in a row, which gave two successors (`pid-000005`,
  `pid-000006`). Twenty concurrent Recovers of one process all succeeded,
  leaving 20 processes that name it `resumed_from`. That is twenty runners
  of the same work. The README's line "a live one never can, so two
  processes never run the same work" covers the old process only; it does
  not cover its successors.
- **N2. Recover accepts any checkpoint the process owns.** *(Implemented
  by step 1: refused unless a rollback is asked for, and then recorded.)*
  It does not have
  to use the one its exit names. A5 recovered from the first of two
  checkpoints while the exit named the second. A recovery can therefore
  quietly roll a process back and repeat steps that already had effects.
- **N3. Leases are advisory names, and tokens are never checked where the
  effect happens.** *(Implemented for `write_file` and `edit_file` by
  step 3: the A4 write is fenced. `run` is still unfenced, A4b.)*
  - A lease's resource is a free string.
  - `write_file`, `edit_file` and `run` never consult leases.
  - The agent never sees its token, so it has nothing to present.
  - A4: the writer's lease lapsed, another process took it over, and the
    stale holder's `write_file` still succeeded. The holder is not told
    that its lease lapsed.
  - The default TTL is 10 minutes. Nothing renews it automatically.
- **N4. A holder killed with SIGKILL keeps its leases until someone runs
  `aos reap`.** Probe A8 killed the OS process mid-command. Afterwards:
  - The kernel still showed the process as `running`.
  - Its lease stayed held for its full one-hour TTL. Another process's
    `acquire_lease` got `ErrLeaseHeld`.
  - Recover was refused, because the process looked live.
  - After `statedir.Reap`, the process had a retryable "orphaned" exit
    naming its last checkpoint. Recover then worked and moved the lease
    (`lease.transferred`).

  Reaping is manual. Nothing calls it on its own, and `aos run` does not
  reap a directory before using it.
- **N5. No restart path in `aos run`.** *(Implemented by step 4: an agent
  with a `restart` policy is recovered from its exit's checkpoint; probe
  A9.)*
  - A delegated child that exits retryably is reported to its parent as
    `DelegateResult.ExitStatus`. The parent can only delegate again, and
    that starts a fresh process with no checkpoint.
  - Recover is reachable from the Go API and from `aos-tui`'s `/resume`. No
    CLI command reaches it.
  - The unused `supervisor.RunChildren` would not help as written. Its
    restarts spawn fresh processes, not from a checkpoint. It has no
    backoff and no restart intensity. It never charges a restart to any
    budget, and its scheduler-driven loop is not how agentflow runs agents.
- **N6. A side effect after the last checkpoint leaves no record of having
  started.** *(Implemented by step 2: the intent is on record, and the
  agent and `inspect` are told.)*
  - Each step is checkpointed after its tool returns, so what is at risk is
    the step in flight.
  - A7 (the run is cancelled mid-`run`): the process parks `ready`. Its
    checkpoint ends `tool_cancelled name=run reason=interrupted_by_user`.
    No `tool_result` or file changes are recorded, yet the command had
    already appended to a file.
  - Resuming the process with a model that repeats the command appended a
    second time.
  - A8 (SIGKILL): the checkpoint holds the step before the command. Only
    the file itself shows that the command ran.
  - With a live backend, the two-phase ledger would also hold the reply
    that asked for the command (see the hard case under the contract).
- **N7. The two stops end differently.** A plain cancel parks the process
  `ready`, so the next step is Resume, which runs the same PID. A SIGTERM
  stop (`ErrTerminated`, A7b) exits it retryably with a checkpoint
  (`terminated by signal (SIGTERM)`), so the next step is Recover. Both are
  by design, but the chapter should say which one leads where.
- **N8. Reap exits every process that has not exited.** That includes
  processes that were cleanly parked (`ready`, `blocked`) with
  checkpoints, and they are recorded as "orphaned". Recovery still works.
  Only the reason string is misleading.

### What the book can call Implemented today

- **Every run ends on the record.** It ends in a recorded exit or a
  checkpointed park, and the checkpoint is saved after every step. SIGTERM
  gives a retryable exit with a final checkpoint.
- **An exited process cannot run.** Running it is refused before any model
  call or artifact (A6).
- **Guarded Recover.** Only an exited, retryable process can be recovered,
  only from its own checkpoint chain (A5). The new process gets:
  - the old one's unexpired leases, each with a new token (A3);
  - its unacknowledged mail and subscriptions;
  - the same capability set and mounts.

  It also continues against the same budget and step limit, because usage
  is saved in each checkpoint.
- **Cross-process exclusive leases** (A2: 0 of 200; in-tree: no overlap
  across 3 processes). A lease is freed when its owner's exit is recorded
  (A3).
- **Fencing tokens are issued and logged.** They strictly increase per
  resource and are never reissued. Every grant, renewal, release, takeover
  and transfer is a `lease.*` kernel event, written before the lease file
  changes. *Checking* the token is not implemented.
- **Dead runs can be cleaned up.** The state-dir lock and `aos reap`
  convert the processes of a dead run into retryable exits that can be
  recovered (A8).

## The Contract

### What a parent owes a child

- A parent spawns a child with **attenuated authority**: the child's
  capabilities, delegable set and mounts are derived from the parent's
  grantable set. A parent cannot hand on more than it holds (today's
  `DeriveChildSet` / `AttenuateMounts`).
- A parent receives the child's **exit as data**: code, reason, whether it
  is retryable, origin, and the checkpoint. It is never left to infer the
  outcome from prose.
- The parent's account **pays for the child**. A child's tokens and seconds
  are charged up its account chain (Chapter 5). Restarts are charged the
  same way: a restart never resets usage.
- A parent whose child is **parked** in a wait answers it or abandons it.
  When the parent exits, the children still parked are closed as
  abandoned (today's `drive`).

### Who may recover a process, from which states and checkpoints

- **States.** Only an exited process with a retryable exit may be
  recovered. A live process, including one that only looks live because
  its run died, is never recovered. It must be reaped first. A parked
  process is resumed, not recovered.
- **At most once.** A process is recovered **at most once**. Recover records
  the successor on the predecessor (`superseded_by`) atomically with the
  spawn, in one kernel transaction (`Kernel.Supersede`, a
  `process.superseded` event). A second Recover is refused with
  `ErrNotRecoverable` and names the successor. *(Gap N1: implemented.)*
- **Which checkpoint.** By default, recovery uses the checkpoint the exit
  names. Any other checkpoint of the same chain is allowed only when the
  caller explicitly asks for it (`RunOptions.Rollback`), and the choice is
  recorded on the `process.superseded` event: `checkpoint` and
  `rollback_from`. *(Gap N2: implemented. The proposal put this in the
  successor's metadata; the event keeps it next to the supersession.)*
- **Who.**
  - The parent agent's engine, under the restart policy below.
  - An operator: through the TUI today, and through a proposed
    `aos recover` command.
  - Never the process itself, and never a sibling.
  - Recovery keeps the predecessor's capability set and mounts unchanged.
    It cannot widen them.

### Restart policy and its budget

*(Implemented by step 4; the differences from this text are listed under
[Implemented: Step 4](#implemented-step-4).)*

- The policy is per agent in the workflow spec:
  `restart: {max: N, within: "10m", backoff: "1s..2m"}`. Without it, a
  child is not restarted (today's behaviour).
- A **retryable** exit is restarted with Recover from the checkpoint its
  exit names. This is the equivalent of Erlang's "transient" child.
- A **non-retryable** exit is final and goes to the parent.
- **Restart intensity:**
  - More than `max` restarts within `within` ends the child with a final,
    non-retryable exit (`restart intensity exceeded`), and the parent is
    told.
  - Escalation is to the parent, as in Erlang/OTP. The engine does not
    restart the parent on its own: the parent is an agent and decides what
    to do with the failure.
- **Restarts are charged to the same account.** The successor continues
  the checkpoint's usage. Backoff waits count against a time budget like
  any other time. A restart that would start with the budget already spent
  is not made; the child's exit stands.
- Each restart is a `supervisor.restart` kernel event on the successor,
  naming the predecessor, the attempt number, the checkpoint and the
  backoff.

### Leases across OS processes, release on exit, fencing

- A lease is exclusive across every manager that shares the state
  directory. *(Implemented.)*
- A lease binds until it expires, its owner releases it, or its owner's
  exit is recorded. A recovered successor inherits its predecessor's
  unexpired leases with new tokens. *(Implemented.)*
- An owner that dies without a record holds its leases until `aos reap`
  records its exit or the TTL lapses. *(Implemented, and documented as a
  limit. Automatic reaping and heartbeat renewal are proposed.)*
- **Fencing is checked at the point of effect.** *(Gap N3: implemented
  for `write_file` and `edit_file`; `run` proposed.)*
  - A lease on a path under a mount covers writes to that path and below
    it. A relative resource is under the workspace mount.
  - `write_file` and `edit_file` check two things, under the lease lock,
    before the write:
    - (a) no live lease on the target, or on a directory above it, is held
      by another process;
    - (b) if the caller, or a process it was recovered from, has ever held
      a lease on the target, its token is still the current one.
  - A failed check is `ErrLeaseFenced`, a `lease.fenced` kernel event, and
    a tool error. The agent sees its token in the `acquire_lease` result.
    There is no automatic renewal, so there is no failed renewal to report.
    A renewal the agent asks for that loses to another holder already
    returns `held: true` and the holder.

  This follows Chubby's sequencers and the fencing-token practice: the
  resource, not the lock service, rejects the stale holder. AOS can do this
  because the tool runtime is the only writer an agent has.

### What a recovered process may repeat

Every step is checkpointed after its tool returns, so what a recovered
process may repeat is the step that was in flight when it died: one model
call and at most one tool call. The cases are:

| In flight at death | What the record gives you today | What a recovered process does |
|---|---|---|
| Model call | A start line with no outcome in the two-phase ledger (`inference.InFlight`) | It repeats the call. This is safe: the call has no effect outside the ledger. |
| A tool with no effect outside AOS (read, list, find, search) | The reply that asked for it, in the ledger's outcome line | It repeats the call. This is safe. |
| `write_file` / `edit_file` | The reply naming the path and content. No sign of whether the write happened. | It repeats the write. A plain write ends in the same state when repeated. A repeated `edit_file` usually fails (`old_text not found`), and the agent sees that. It applies a second time only when `new_text` still contains `old_text`. |
| `run` | The reply naming the command. Nothing says whether it started or finished. If it was cancelled, the checkpoint says `tool_cancelled`. File changes are recorded only when the command completes (A7, A8). | It may run the command again. **This is the hard case.** |
| `web_fetch` / `web_search` | The reply | It repeats the call. GETs are assumed to have no effect on the remote side. |

The contract for the hard case is **at most one unknown, and it is
named**:

- Before a world tool runs, the runtime records an intent: a
  `tool.started` kernel event with the call's tool, the command or path
  (clipped to 200 bytes), and a digest of its arguments.
- After the tool returns, the outcome refers to that intent (`intent`
  metadata on the `tool_result`, `tool_error` or `tool_cancelled`).
- On Resume or Recover, an intent with no outcome gets a
  `tool_interrupted` outcome, produced by the process that runs next. The
  agent is told before its next step:
  "`tool_interrupted name=run target=… note=this call started before the
  process stopped and has no recorded outcome; it may have taken effect,
  fully or in part; check before repeating it`". `aos inspect` lists it,
  first as open and then as interrupted, naming who was told.
- `run` records file changes on cancel and timeout as well, so a cancelled
  command's partial writes are on the record (A7). A cancelled world tool's
  note says `started=true may_have_taken_effect=true changes="…"`.

This is a write-ahead intent record. It does not make `run`
exactly-once, which no runtime can promise for arbitrary shell commands.
It makes a possible repeat visible, to the agent and to the verifier.
A model can still repeat the call after the note: A7's scripted model
does, and the file gets a second line. *(Gap N6: implemented.)*

### Prior art, briefly

The book should verify these citations itself.

- **Erlang/OTP supervisors:**
  - restart strategies (`one_for_one` and others);
  - child restart types (`permanent`, `transient`, `temporary`);
  - restart intensity: at most MaxR restarts in MaxT seconds, otherwise
    the supervisor itself exits and the failure goes up the tree.

  AOS borrows transient children, intensity and escalation. It differs in
  that a restart resumes from a checkpoint instead of starting from the
  init state. It also differs in that the parent is a model-driven agent
  that is told, not a supervisor that restarts it.
- **Temporal (durable execution):**
  - workflow state is rebuilt by replaying an event history;
  - activities are retried under a retry policy;
  - activities are at-least-once, so they should be idempotent;
  - heartbeats detect long activities that have stalled.

  AOS's step checkpoints and two-phase ledger play the role of the event
  history. A tool call is AOS's activity, with the same at-least-once
  caveat. The proposed `tool.started` intent matches Temporal's
  scheduled/started/completed activity events.
- **Leases and fencing tokens:**
  - Gray and Cheriton's leases (1989);
  - Chubby's sequencers (Burrows 2006);
  - Kleppmann's "How to do distributed locking" (2016): a lock without a
    token checked by the resource is not safe against a paused or delayed
    holder.

  A4 demonstrates exactly that hazard.
- **What an agent kernel adds:**
  - **Capabilities survive recovery unchanged and cannot widen.** A
    restart is not a chance to re-grant.
  - **Budgets are charged to model calls across restarts.** A crash loop
    spends a finite account, and it does not reset the meter.
  - **Verification is external.** A recovered worker's result is not
    trusted because it recovered. It goes to the same verifier, which also
    sees the named unknowns from the intent records.

## Gaps

| Gap | Contract clause | Where |
|---|---|---|
| N1 Recover not exclusive | At most once (`superseded_by`) | `kernel` (one transaction), `runtime.Recover` |
| N2 Any owned checkpoint | The exit's checkpoint by default, with rollback recorded | `runtime.Recover`, `RunOptions` |
| N3 No fencing at the point of effect | Path leases checked by the file tools and `run`; token visible to the agent | `tools`, `lease`, `runtime` |
| N4 Dead holder keeps its leases | Documented limit; reaping on start proposed | `statedir`, `cmd/aos` |
| N5 No restart path | Restart policy in agentflow, using the guarded Recover | `agentflow`, workflow spec |
| N6 No start record for side effects | `tool.started` intent, changes recorded on cancel | `runtime`, `tools` |
| N8 Reap mislabels parked processes | Reason `parked when the run died` for `ready`/`blocked` | `statedir` |
| Gap 1 Supervisor unused | Retire `internal/supervisor`, or rebuild it on Recover as the engine's restart policy | `internal/supervisor` |

## Hazards

- **A Recover race across OS processes.** The claim and the spawn must be
  one kernel transaction under the kernel's file lock. A check followed by
  a separate spawn reproduces N1.
- **A restart loop on a deterministic failure.** A retryable exit that
  fails the same way every time burns budget until intensity stops it.
  Intensity limits must be on by default whenever a restart policy is set.
- **Fencing false positives.**
  - A lease on a directory fences every write below it.
  - `run` cannot be fenced per file before it runs. It can only be refused
    when the workspace has a live lease held by another process, or its
    writes checked after the fact. The coarse rule is to refuse `run` while
    another process holds a lease anywhere in the mount, which may be too
    strict for parallel workers. That is a choice to make before building.
    *(Still open after step 3; probe A4b shows the stale holder writing
    through `run`.)*
- **Replay compatibility.**
  - New kernel events (`process.superseded`, `tool.started`,
    `lease.fenced`, `supervisor.restart`) change the kernel log. Old
    records stay replayable, because their events are compared as
    recorded.
  - Replay never recovers, so `process.superseded` never appears in a
    replayed run. `tool.started` would be new on every world tool call. A
    record with no `tool.started` is therefore replayed with
    `NoToolIntents`, the old behavior, and the report says
    `legacy_tool_intents`. *(Implemented: eval's three `ca78ad9` trials
    replay identically.)*
  - Fencing changes what replay runs live: `acquire_lease` is a runtime
    tool, so its new `token` output and the fence check run during
    replay. `run.started` records `lease_fencing: file_writes`, and a
    record without it is replayed with `NoLeaseFencing` (`legacy_leases`).
  - A run that acquired a lease never replayed identically: the
    `lease.acquired` event and the `acquire_lease` result carried a
    wall-clock `expires_at`. *(Fixed 2026-11-02: leases run on the kernel
    clock; see "Lease clock" in replay-design.md. Older records with
    leases are refused as not replayable.)*
- **The TUI's `/recover`** goes through the same exclusive Recover, so two
  terminals on one session directory cannot both recover, lock or not.

## Recommendation: the Smallest Slice

In order. Each step is small enough for one commit with its tests.

1. **Exclusive, explicit Recover (N1, N2).** *(Implemented.)*
   - Add a kernel operation that, in one transaction, checks that the
     process is exited and not superseded, spawns the successor, and
     records `superseded_by` on the predecessor with a
     `process.superseded` event. (The runtime checks retryable first; an
     exit never changes, so that check cannot race.)
   - Recover uses the exit's checkpoint by default. An earlier checkpoint
     needs `RunOptions.Rollback`, which is recorded.
   - *Unlocks for the chapter:* the invariant "one runner per unit of
     work", stated as Implemented.
2. **Intent records for world tools (N6).** *(Implemented.)*
   - Write `tool.started` before each world tool and refer to it from the
     outcome.
   - On Recover or Resume, put the unmatched intent first in the agent's
     history.
   - Make `inspect` list unmatched intents.
   - Record `run`'s file changes on cancel and timeout.
   - *Unlocks:* the "what a recovered process may repeat" section, with
     the hard case named and on the record.
3. **Fencing at the point of effect (N3).** *(Implemented for file
   writes.)*
   - Return the token from `acquire_lease`.
   - Path leases are checked by `write_file` and `edit_file` (the holder
     check and the stale-token check), with `lease.fenced` events.
   - `run` waits for the hazard decision above.
   - *Unlocks:* "leases with fencing tokens" stated as Implemented for file
     writes, with A4 as the demonstration that fails today and passes
     afterwards.
4. **A restart policy in agentflow (N5, Gap 1).** *(Implemented.)*
   - Add `restart` in the workflow spec. The engine recovers a child that
     exits retryably, from its exit's checkpoint, through step 1's Recover.
   - Restart intensity and escalation.
   - `supervisor.restart` events, charged to the same account.
   - Delete `internal/supervisor`, or reduce it to the policy type.
   - *Unlocks:* "the supervisor is on the main path", and the comparison
     with Erlang/OTP stated against running code.

Everything else stays **Proposed**:

- an `aos recover` CLI command;
- automatic reaping when `aos run` starts on a directory;
- heartbeat renewal of leases, and liveness of the OS process that holds
  one;
- fencing `run` per file;
- N8's reason string;
- recovery inside multi-agent replay;
- any verifier changes for recovered results.

## Tests That Would Prove It

All of these run with `FakeBackend` or a scripted backend, with no model,
so eval can probe recovery without phg-gpu.

- **Step 1:**
  - `TestRecoverIsExclusive`: a second Recover of the same PID, one after
    the other, is `ErrNotRecoverable` naming the successor. This is A5
    inverted.
  - `TestConcurrentRecoverAcrossProcesses`: N OS processes (test-binary
    re-exec, as `TestLeaseExclusiveAcrossProcesses` does) recover one
    exited PID. Exactly one succeeds, and exactly one process names it
    `resumed_from`.
  - `TestRecoverUsesTheExitCheckpoint`: recovering from an earlier
    checkpoint without `Rollback` is refused. With `Rollback`, it is
    allowed and recorded.
- **Step 2:**
  - `TestKilledMidRunNamesTheUnknown`: re-exec a helper that runs an agent
    to the middle of a `run` command and SIGKILL it (A8's harness). Then
    reap and Recover. The recovered agent's first message names the
    command as possibly run. `aos inspect` lists the unmatched intent.
  - `TestCancelledRunRecordsChanges`: A7's harness. The cancelled
    command's appended file appears in the recorded changes.
  - `TestIntentReplay`: a recorded run with intents replays identically.
    An older record without intents still replays.
- **Step 3:**
  - `TestStaleHolderIsFenced`: A4 inverted. After a takeover, the stale
    holder's `write_file` is `ErrLeaseFenced`, a `lease.fenced` event is
    recorded, and the file is unchanged.
  - `TestLeaseTokenIsShown`: the `acquire_lease` output has `token`.
  - `TestForeignLeaseBlocksWrite`: a path leased by another live process
    refuses `write_file` and `edit_file`.
- **Step 4:**
  - `TestTransientChildRestartsFromCheckpoint`: a child scripted to exit
    retryably once is recovered from its exit's checkpoint. The parent
    sees one successful result, and `supervisor.restart` names the
    attempt.
  - `TestRestartIntensityEscalates`: a child that always fails is
    restarted `max` times, then ends non-retryably. The parent is told.
  - `TestRestartsChargeTheSameAccount`: the successor's token and step
    usage continue from the checkpoint, so a restart cannot exceed the
    run's budget.
  - `TestNonRetryableExitIsFinal`: no restart happens.
- **Kept:** the audit probes in `bench/supervision` stay as the "before"
  picture. Each step flips one of their findings.

## Implemented: Steps 1 and 2

Built on 2026-11-01, after `d80ed15`.

**Step 1, exclusive Recover:**

- `kernel.Kernel.Supersede(pid, spec, data)`: one transaction under the
  kernel's file lock. It refuses a process that has not exited
  (`ErrInvalidTransition`) or already has a successor (`ErrSuperseded`).
  It spawns the successor, then appends `process.superseded` (`successor`,
  `checkpoint`, and `rollback_from` for a rollback) and sets
  `Process.SupersededBy`. A table that missed the event is repaired from
  the log, like the other `process.*` events.
- `runtime.Recover`: an empty checkpoint means the one the exit names. Any
  other needs `RunOptions.Rollback`. A superseded process is refused with
  `ErrNotRecoverable` wrapping `kernel.ErrSuperseded`, naming the
  successor.
- `aos inspect` shows `superseded by pid-…` on the process line.

**Step 2, intents:**

- `runtime.WorldTool` is `run`, `write_file` and `edit_file`. Each records
  `tool.started` after the capability check and before the tool acts. If
  the event cannot be written, the tool does not run and the run fails
  retryably.
- Outcomes name the intent. The paths are:
  - success: `tool_result`;
  - a tool error: `tool_error`, now also when the error ends the run;
  - a cut-off command: `tool_cancelled`, with the changes;
  - a timeout: `tool_error` with the changes.
- `Resume` and `Recover` call `noteInterrupted`. For each open intent of
  the process being continued, it records a `tool_interrupted` outcome
  and adds the note before any appended message.
- `aos inspect` lists every call that started and did not finish
  (`cut_off_calls`).
- Replay: `NoToolIntents` for records without intents. The player answers
  a `tool_cancelled` from the record, with its changes.

**Probes, before (`e401e4e`) and after:**

| Probe | Before | After |
|---|---|---|
| A5 sequential double Recover | 2 successors | the second is refused: "pid-000001 was superseded by pid-000005" |
| A5 20 concurrent Recovers | 20 succeeded, 20 successors | 1 succeeded, 1 successor |
| A5 Recover from the older of 2 checkpoints | accepted | refused as a rollback; with `Rollback`, accepted and recorded (`rollback_from: art-000003`) |
| A5 foreign checkpoint | `ErrCheckpointNotOwned` | the same (now asked as a rollback, so ownership is what refuses it) |
| A7 cancel mid-`run` | no record of the change; note `tool_cancelled name=run reason=interrupted_by_user` | 1 intent; `tool_cancelled(run)` with `created effects`; the note adds `started=true may_have_taken_effect=true changes="created effects"`; the resumed agent's first request carries it |
| A7 after resume | the scripted model repeats, so the file reads `once\nonce` | the same: the note does not stop a model that repeats |
| A8 SIGKILL mid-`run`, after reap | only the file shows the command ran | `inspect` lists the `run` as started with no outcome |
| A8 Recover after reap | the agent is not told | `Recover(pid, "")` uses the exit's checkpoint; the first request has `tool_interrupted name=run target="echo once >> …"`; `inspect` shows it interrupted and names the successor that was told |

**Unit tests:**

- `kernel`:
  - `TestSupersedeRecordsOneSuccessor`;
  - `TestSupersedeIsExclusiveAcrossProcesses` (6 OS processes and 6
    goroutines: 1 successor, 11 refusals);
  - `TestSupersededIsReplayed`.
- `runtime`:
  - `TestRecoverIsExclusive` (sequential, and 20 concurrent);
  - `TestRecoverUsesTheExitsCheckpoint`;
  - `TestWorldToolRecordsIntentFirst`;
  - `TestCancelledCommandRecordsItsChanges`;
  - `TestTimedOutCommandRecordsItsChanges`;
  - `TestRecoverReportsInterruptedCalls`;
  - `TestNoToolIntents`.
- `tools`: `TestCutOffRunReturnsItsChanges`.
- `inspect`: `TestLoadShowsCutOffCallsAndSuccessors`.
- `replay`:
  - `TestReplayOfARecordWithoutIntents`;
  - `TestReplayOfACommandCutOff` (a strict budget, with and without
    intents).

**Changed for existing callers:**

- `Recover` with a checkpoint that is not the exit's now fails unless
  `Rollback` is set. One existing test rolled back on purpose and now says
  so.
- Every world tool call adds one kernel event.
- A cancelled world tool's note to the agent is longer.
- A tool error that ends the run now leaves a `tool_error` artifact.

## Implemented: Step 3

Built on 2026-11-02, after `84287c1`.

- **`lease.Manager.Guard(writers, covers, write)`.**
  - Under the lease file's lock, it checks every resource that covers
    the target, in name order.
  - It returns a `*lease.FenceError` (`ErrLeaseFenced`, reason
    `held_by_other` or `stale_token`) without calling `write`, or runs
    `write` with the lock still held.
  - `leases.json` gains `holders`: per resource, the last token each
    owner was granted. Older files have none. For them, the current owner
    counts as holding the current token.
- **The runtime.** For `write_file` and `edit_file` with a path, the
  runtime runs the intent and the tool inside `Guard`.
  - **Order: fence, then intent, then write.** A refused write records
    no `tool.started`, so it leaves no open intent and needs no outcome.
    Recording the intent first would put an intent on record for a write
    that never started.
  - The writers are the process and its `resumed_from` chain, so a
    recovered process writes under the lease it took over.
  - The resource-to-path rule is `leaseCovers`.
  - A refusal logs `lease.fenced` with `tool`, `path`, `resource`,
    `reason`, `holder`, `token` and `held_token`.
  - The error names no time, so it reads the same in a replay.
- **`acquire_lease`** shows `token`.
- **The window.** None is left between the check and a fenced write.
  - Every grant, renewal, takeover and transfer takes the same lock.
    It is a file lock, so this holds across OS processes as well
    (`TestGuardHoldsTheLockThroughTheWrite`: a takeover started during
    the write completes only after it).
  - What is left open is writes that do not go through `Guard`: `run`
    (A4b), and anything outside AOS's tools.
  - An owner's exit is read from the kernel and can only free a lease,
    never grant one, so it opens no gap either.

**Probe A4, before and after:**

| | Before (`e401e4e`) | After |
|---|---|---|
| The stale holder's `write_file` after a takeover | wrote `"stale holder"` | fenced; the file is empty; one `lease.fenced` (`held_by_other`, holder the thief, token 2) |
| The `acquire_lease` output | keys `held, resource, owner_pid, expires_at` | also `token: 1` |
| A4b: the same holder through `run` | not probed | writes the file; no `lease.fenced` (`run` is unfenced) |

**Tests:**

- `lease`:
  - `TestGuardFencesWrites`;
  - `TestGuardFollowsTransfers`;
  - `TestGuardReadsOlderLeaseFiles`;
  - `TestGuardHoldsTheLockThroughTheWrite`.
- `runtime`:
  - `TestStaleHolderIsFenced`: A4 inverted, then stale after the release,
    then a re-acquire and the write;
  - `TestLeaseTokenIsShown`;
  - `TestForeignLeaseBlocksWrite`: a directory lease, an absolute
    resource, a non-path name, a sibling `srcfile` not covered by `src`,
    `edit_file`, and the holder's own edit;
  - `TestRecoveredProcessWritesUnderItsLease`;
  - `TestNoLeaseFencing`.
- `replay`: `TestReplayOfARecordWithoutFencing`, with and without
  intents.

**Changed for existing runs:**

- A write under another live process's lease, or with a stale token, is
  now a tool error.
- `acquire_lease` output has `token`.
- `run.started` has `lease_fencing`.
- Every `write_file` and `edit_file` takes the lease lock and reads
  `leases.json`.

## Implemented: Step 4

Built on 2026-11-02, after `41d61dd`.

- **The policy.** `AgentSpec.Restart` is a `*supervisor.RestartPolicy`
  with fields `max`, `within` and `backoff`. Absent, nothing changes.
  - The defaults are max 3, within 10m, backoff `1s..30s`. An empty
    policy (`"restart": {}`) is bounded, and no setting removes the
    bound.
  - Backoff doubles from its base on each restart in the window, up to
    its maximum. A single duration is a fixed wait.
  - An invalid policy fails spec validation.
- **`internal/supervisor`: reduced to the policy, not retired.** The
  package was `RunChildren`, a scheduler-driven loop that respawned
  fresh processes, plus `Tree`. Nothing imported either, and neither fit
  agentflow. Both are deleted. What remains is the policy type with its
  defaults, backoff and intensity window. agentflow imports it, so the
  package is on `aos run`'s path, and its name now matches what it does.
  `internal/scheduler` lost its only importer and is left alone.
- **The engine** (`agentflow.Engine.supervise`) applies the policy after
  every `runAgent` and `resumeAgent`, so to the entry agent and to
  delegated children alike.
  - It acts on a retryable, non-zero exit that is not a wait, as long as
    the run's ctx is live: a SIGTERM stop is never restarted.
  - It checks, in order:
    1. the intensity;
    2. that the exit names a checkpoint;
    3. that the budget is not spent (`runtime.SpentBudget`).
  - Then it waits the backoff and calls
    `Recover(pid, "", …)`, which uses the exit's checkpoint and makes one
    successor.
  - `drive` follows the successor through `RunOptions.OnRecovered`, so
    traces and the `agent_run` name the process that actually ran.
- **Events.**
  - `supervisor.restart` is recorded on the successor before its first
    step, with `predecessor`, `attempt`, `checkpoint`, `backoff` and
    `reason`.
  - `supervisor.escalated` is recorded when the intensity is exceeded.
  - `supervisor.not_restarted` is recorded when a retryable exit is not
    restarted. Its `cause` is one of `budget_spent`, `no_checkpoint`,
    `not_recoverable` or `error`.
- **What the delegator sees.**
  - A successful restart: the successor's result.
  - The intensity exceeded: a final exit, `restart intensity exceeded: N
    restarts within W (max M); last exit: …`, with `exit_retryable=false`.
    The last process's kernel record keeps its retryable exit, because an
    exit is never rewritten. An operator can still Recover it on purpose;
    the engine will not.
  - The budget spent: the child's own exit unchanged, for example `step
    limit exceeded`, retryable. The parent decides what to do, as it does
    today.
- **Budget.** The successor continues the checkpoint's usage and the
  same account chain, so the step limit, tokens and strict time carry
  over. A restart that would start with any of them spent is not made.
  Backoff waits run on the delegator's time, as any other wait does.
- **Backoff and replay.** The backoff goes through `Engine.Sleep` when it
  is set, as backend retries do, and otherwise through a timer that
  respects ctx. The intensity window reads the kernel clock and the
  `supervisor.restart` timestamps. Replay does not need to reproduce any
  of this: a restarted run has two agent processes, so it is refused with
  a reason naming the restart (`TestReplayRefusesARestartedRun`). Records
  without restarts replay as before.
- **Concurrency.** The engine runs a delegated child inside the
  delegator's `delegate` step, one at a time. A backoff therefore holds up
  only the delegator that is already waiting for that child. There are no
  other children running alongside it to block.
- **Differences from the contract text above.**
  - Escalation is a non-retryable exit reported to the delegator, not a
    rewritten kernel exit.
  - A budget-spent child's exit is passed on as it is.

**Probe A9 (N5), before and after:**

| | Before (`e401e4e`) | After |
|---|---|---|
| `go list -deps ./cmd/aos` names `internal/supervisor` | no (0) | yes |
| A child exits retryably, no policy | the parent gets it retryable; there is no restart path | the same: 1 worker process, 0 restarts |
| The same child, `restart: {max: 2, within: 1m, backoff: 0s}` | no such field | 2 worker processes, 1 `supervisor.restart`; the coordinator gets `exit 0 "done after restart"` |

**Tests:**

- `supervisor`:
  - `TestEffectiveAppliesBoundedDefaults`;
  - `TestBackoffDoublesUpToTheMaximum`;
  - `TestIntensityCountsTheWindow`.
- `agentflow`:
  - `TestTransientChildRestartsFromCheckpoint`;
  - `TestRestartIntensityEscalates`;
  - `TestDefaultPolicyIsBounded`;
  - `TestRestartsChargeTheSameAccount`;
  - `TestNonRetryableExitIsFinal`;
  - `TestNoPolicyNoRestart`;
  - `TestOpenIntentReachesRestartedChild` (steps 2 and 4);
  - `TestLeasesTransferOnRestart` (steps 3 and 4);
  - `TestBackoffWaitsAndStopsWithTheRun`;
  - `TestInvalidRestartPolicyIsRefused`.
- `replay`: `TestReplayRefusesARestartedRun`.

**A live check worth one trial, sketched and not run.** On eval's queue,
run one Terminal-Bench task with the restart policy on and a step budget.
Inside the container, `kill -9` the `aos` process after its third tool
call, then run `aos reap` and `aos recover` (or let the engine restart the
child, once step 4 exists). Check four things:

- the successor continues the budget;
- the unmatched intent is in its first message;
- the verifier's verdict is unchanged;
- `aos replay` handles the record, or says why not.

This needs step 2 and the CLI command, so it waits for both.
