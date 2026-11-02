# Supervision Design

Status: proposal, 2026-11-01, written at the request of the aos book
session for Chapter 7 (Supervision, Leases, and Recovery). It covers the
audit and the design only. Nothing here is built yet, except the audit
probes in `bench/supervision/audit_test.go`.

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

## Today's State

### The six gaps from the review

| # | Review claim (2026-10-28) | At `e401e4e` | How it was checked | Result |
|---|---|---|---|---|
| 1 | The supervisor is not on the path that runs real agents | **Still holds** | `go list -deps ./cmd/aos ./cmd/aos-tui \| grep -c aos/internal/supervisor` | `0`. The only importer of `internal/supervisor` is its own test. |
| 2 | Two lease managers on one state dir both won the lease in 193 of 200 trials | **Fixed** (`f3eef1d`) | Probe A2 repeats the review's experiment: two OS processes, one `Acquire` each at a shared start, 200 trials. In-tree, `TestLeaseExclusiveAcrossProcesses` runs 3 processes × 200 attempts. | A2: both won in **0 of 200**. In-tree: 105 grants, no overlap (an unlocked counter lost no increment), tokens exactly 1..105. |
| 3 | Nothing releases a lease when its holder exits, so a recovered process is locked out until the TTL | **Fixed for recorded exits.** A dead OS process still holds its leases until reaped (see N4). | Probe A3 | After the holder exited, another process acquired the lease: `lease.taken_over`, reason `owner_exited`, token 2. Recovering a process that held a lease gives the lease to the new process: `lease.transferred`, token 1 → 2. The recovered agent's own `acquire_lease` then renews it. |
| 4 | There are no fencing tokens | **Half fixed.** Tokens are issued, but nothing checks them (see N3). | Probe A4, in-tree token test | Each grant gets a token that strictly increases per resource and is never reissued. But a holder whose lease expired and was taken over (token 2) still wrote the file afterwards. `acquire_lease` does not show the agent its token: its output keys are `held, resource, owner_pid, expires_at`. |
| 5 | `Recover` has no state check | **Fixed.** A guarded Recover was built in `f3eef1d`. It is not exclusive (see N1). | Probe A5 | A running process → `ErrNotRecoverable` ("only an exited process can be recovered"). A non-retryable exit → `ErrNotRecoverable`. Another process's checkpoint → `ErrCheckpointNotOwned`. |
| 6 | `runSession` runs a process that has already exited | **Fixed** (`f3eef1d`) | Probe A6 | `RunWithOptions` on an exited PID → `ErrProcessExited`, with 0 model calls, no new artifacts and no file written. |

### New findings

- **N1. Recover is not exclusive.** The guard checks the process being
  recovered, but nothing marks it as recovered. Probe A5 recovered the same
  exited process twice in a row, which gave two successors (`pid-000005`,
  `pid-000006`). Twenty concurrent Recovers of one process all succeeded,
  leaving 20 processes that name it `resumed_from`. That is twenty runners
  of the same work. The README's line "a live one never can, so two
  processes never run the same work" covers the old process only; it does
  not cover its successors.
- **N2. Recover accepts any checkpoint the process owns.** It does not have
  to use the one its exit names. A5 recovered from the first of two
  checkpoints while the exit named the second. A recovery can therefore
  quietly roll a process back and repeat steps that already had effects.
- **N3. Leases are advisory names, and tokens are never checked where the
  effect happens.**
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
- **N5. No restart path in `aos run`.**
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
  started.**
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
  spawn, in one kernel transaction. A second Recover is refused with
  `ErrNotRecoverable` and names the successor. *(Gap N1: proposed.)*
- **Which checkpoint.** By default, recovery uses the checkpoint the exit
  names. An earlier checkpoint of the same chain is allowed only when the
  operator explicitly asks for it, and the choice is recorded in the
  successor's metadata (`recovered_from_checkpoint`, plus
  `rollback: true`). *(Gap N2: proposed.)*
- **Who.**
  - The parent agent's engine, under the restart policy below.
  - An operator: through the TUI today, and through a proposed
    `aos recover` command.
  - Never the process itself, and never a sibling.
  - Recovery keeps the predecessor's capability set and mounts unchanged.
    It cannot widen them.

### Restart policy and its budget

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
- **Fencing is checked at the point of effect.** *(Gap N3: proposed.)*
  - A lease on a path under a mount covers writes to that path and below
    it.
  - `write_file`, `edit_file` and `run` (for the workspace mount) check, in
    the tool runtime, two things:
    - (a) no live lease on the target is held by another process;
    - (b) if the caller has ever held a lease on the target, its token is
      still the current one.
  - A failed check is `ErrLeaseFenced`, a `lease.fenced` kernel event, and
    a tool error. The agent sees its token in the `acquire_lease` result
    and is told when a renewal fails.

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
  `tool.started` kernel event with the call's tool, a digest of its
  arguments, and the step.
- After the tool returns, the outcome refers to that intent.
- On Recover, an intent with no outcome is shown to the agent as the first
  message: "`run` <command> may have run before the crash; check its
  effects before repeating it". `aos inspect` shows it too.
- `run` records file changes on cancel and timeout as well, so a cancelled
  command's partial writes are on the record (A7).

This is a write-ahead intent record. It does not make `run`
exactly-once, which no runtime can promise for arbitrary shell commands.
It makes a possible repeat visible, to the agent and to the verifier.
*(Gap N6: proposed.)*

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
- **Replay compatibility.**
  - New kernel events (`superseded_by` transitions, `tool.started`,
    `lease.fenced`, `supervisor.restart`) change the kernel log. Old
    records stay replayable, because their events are compared as
    recorded.
  - A new build replaying an old record will not emit the new events where
    the old build didn't, but `tool.started` would be new on every tool
    call. Replay needs a format check like the `run.started` one
    (`e401e4e`).
- **The TUI's `/resume`** recovers the root in place. It must go through the
  same exclusive Recover, or two terminals on one session directory could
  both recover. The state-dir lock prevents that today.

## Recommendation: the Smallest Slice

In order. Each step is small enough for one commit with its tests.

1. **Exclusive, explicit Recover (N1, N2).**
   - Add a kernel operation that, in one transaction, checks that the
     process is exited, retryable and not superseded, spawns the
     successor, and records `superseded_by` on the predecessor with a
     `process.recovered` event.
   - Recover uses the exit's checkpoint by default. An earlier checkpoint
     needs `RunOptions.Rollback`, which is recorded.
   - *Unlocks for the chapter:* the invariant "one runner per unit of
     work", stated as Implemented.
2. **Intent records for world tools (N6).**
   - Write `tool.started` before each world tool and refer to it from the
     outcome.
   - On Recover or Resume, put the unmatched intent first in the agent's
     history.
   - Make `inspect` list unmatched intents.
   - Record `run`'s file changes on cancel and timeout.
   - *Unlocks:* the "what a recovered process may repeat" section, with
     the hard case named and on the record.
3. **Fencing at the point of effect (N3).**
   - Return the token from `acquire_lease`.
   - Path leases are checked by `write_file` and `edit_file` (the holder
     check and the stale-token check), with `lease.fenced` events.
   - `run` waits for the hazard decision above.
   - *Unlocks:* "leases with fencing tokens" stated as Implemented for file
     writes, with A4 as the demonstration that fails today and passes
     afterwards.
4. **A restart policy in agentflow (N5, Gap 1).**
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
