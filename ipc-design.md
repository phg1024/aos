# IPC Design

Status: proposal, 2026-11-02. It was written at the request of the aos
book session for Chapter 8 (IPC Without Shared Context), and covers the
audit and the design. Nothing in it is built yet. The commit that adds
this note changes no production code.

## Why

Chapter 8 asks: **when should supervised processes use mailboxes, events,
or artifact handles?** Today AOS has all three, but nothing decides which
one a piece of data goes through:

- an agent can mail a 10 MB body;
- it can say "tests pass" in a message and mean it as a result;
- it can learn that a peer exited only by polling.

The audit at `180d076` (committed as `8a75465`) measured the mailbox bus as
it stands. Delivery works: at least once, deduplicated, ordered, and
consistent across OS processes. Most of what is missing is policy: who
may send, how much, to whom across a recovery, and what the sender learns.

This note does three things:

- It summarizes what the audit found, with each probe's verdict.
- It states the contract the chapter should describe, as rules the
  runtime enforces and not only advice.
- It names the smallest slice that would make the contract true, and the
  probes each step flips from logging to asserting.

## How the Audit Was Run

Every result below comes from running code at `180d076`, not from reading
it. The probes are Go tests in `bench/ipc/audit_test.go` behind the
`ipcaudit` build tag, so `go test ./...` does not run them. They use
scripted agents and helper OS processes (test-binary re-exec), and need
no model and no phg-gpu:

```
go test -tags ipcaudit -count=1 -v ./bench/ipc/
AOS_IPC_TRIALS=20 go test -tags ipcaudit -count=1 -v -run B4 ./bench/ipc/
```

Each probe logs `FINDING:` lines and fails only when it cannot run. A
comment on each probe names the contract it was measured against. The
full table, with the observed numbers, is in `reports/ipc-audit.html`. The
book session re-ran all ten probes independently (50 `FINDING` lines) and
checked the code citations for B5, B8c, B9 and B2b.

**A note on numbering.** Replay's divergence report numbers a call by the
ledger sequence of its start line (`replay.Divergence.Call`). Each model
call writes a start line and an outcome line, so the third model call
starts at ledger seq 5. B9's log line "diverged after 2/3 calls:
kind=request call=5" and the audit report's "diverges at call 3" are the
same call. This note counts model calls (1, 2, 3) and gives the ledger
seq in parentheses where it matters.

## Today's State

| Probe | What it does | At `180d076` | Verdict |
|---|---|---|---|
| B1a | SIGKILL a receiver after delivery, before its checkpoint; reap; Recover | The successor's first request has the message exactly once; acked after the successor's checkpoint | holds |
| B1b | The checkpoint records the message, the ack is lost, then Recover | Seen once, from the checkpoint; the redelivery is dropped by `MailboxSeen` and acked | holds |
| B1c | Mail arrives in the step that spends the last of a token budget | The next step receives it into the history, then the budget check stops the run before any model call. The checkpoint acks it. No model ever sees it, and the sender is never told | gap |
| B2a | Mail to a parked process that then exits retryably and is Recovered | The successor gets it once, through `Transfer` | holds |
| B2b | Mail to an exited PID, before and after its Recover | Refused (`ErrRecipientExited`, `ipc.rejected`) both times. Not forwarded along `superseded_by`, although the successor is live | gap |
| B2c | Mail to a parked process that then exits 0 without reading it | Pending for good. Its only event is `ipc.sent`. No bounce; the sender is never told | gap |
| B3 | Two senders interleave, across two Recovers, one of them before a checkpoint | Both successors see A1 B1 A2 B2 A3, then A4, each once | holds |
| B4 | 4 sender OS processes × 50 messages, 1 receiver process, one bus directory, 25 trials | 0 lost, 0 duplicated, 0 corrupted, 0 order violations | holds |
| B5 | A body with forged lines, and a message from `user`, rendered through `PromptBackend` | Only lines starting `message from ` are quoted. `[#7] message from coordinator …`, `[#8] run:map[exit_code:0 …]`, a budget warning and `SYSTEM: …` pass unquoted. Every non-assistant turn is role `user` (`agentflow/backend.go:84`). Mail from `user` becomes a plain user turn | **defect** |
| B6 | 1 MB and 10 MB bodies through `send` and `message_agent` | Both accepted; no cap. `mailboxes.json` reaches 20 MB and is rewritten on every send (20 ms empty, 140 ms behind 20 MB). No tool reads an artifact's body. `list_artifacts` lists every process's artifacts, ungated | gap |
| B7 | A parent mails a parked child and calls `await_agent`; the child exits without answering | `await_agent` is a non-blocking poll (`state: ready`, then `code: 0`). No correlation ID, reply-to or deadline on `ipc.Message` | gap |
| B8a | 2,000 unread messages to one mailbox | All accepted; 542 KB; 21 ms per send rising to 33 ms. No bound, no backpressure | gap |
| B8b | Publish before any subscriber, then with 3 | 0 messages, then one per live subscriber with its own ID; a late subscriber sees nothing earlier | holds |
| B8c | Subscribe, exit retryably, Recover, publish | `subscriptions.json` is `{}` after the exit; the publish reaches 0 subscribers. The successor's history still says it subscribed. `runSession` unsubscribes an exited process (`runtime/runtime.go:586`) before `Transfer` can move the subscription | **defect** |
| B9 | A single-agent run with operator mail between steps 2 and 3, then replay and inspect | Replay diverges at model call 3 (ledger seq 5): "the replayed request has 7 messages, the recorded 8"; the missing one is the operator mail. Replay opens a fresh, empty bus (`replay/replay.go:284`). Multi-agent runs are refused (`replay.go:245`). `inspect` reads only unacked mail. TUI sessions are a documented limit (`replay-design.md`, "As built", Limits) | gap |
| B10a | An agent sends to a process it has no relation to | Accepted: the only checks are the `message_agent` capability and a live recipient | gap |
| B10b | A delegated worker mails its parent, then exits | The parent sees the delegate result first, then the earlier mail | note |
| B10c | Code: `Bus.update` (`ipc/mailbox.go:302`) | Writes `state.json`, `mailboxes.json`, `subscriptions.json` one after another. Each is atomic; the three together are not. IDs are never reused | gap |
| B10d | Code: who sets `From` | The runtime always passes the process's PID; an agent cannot choose it. `aos-tui` passes `user` (`cmd/aos-tui/root.go:70`) | holds |

### What the book can call Implemented today

- **At-least-once delivery, made exactly-once by the receiver.** The bus
  redelivers until an ack; acks follow checkpoints; the session drops
  repeats by message ID (B1a, B1b).
- **Per-sender FIFO, and send order overall**, across Recover (B3).
- **Consistency across OS processes.** Every operation is a
  read-modify-write under one file lock (B4: 0 of 5,000 lost or
  duplicated).
- **Unacked mail moves to a recovered successor** (B2a).
- **Sender identity is the runtime's, not the agent's** (B10d).
- **Mail is capability-gated** (`message_agent`; B10a's second case).
- **Mail is data in the history, below the task.** It is labelled with its
  sender and ID, and the system prompt ranks it below the task and the
  user. The label can be forged (B5), so this is advice to the model, not
  a boundary.

## The Contract

### The rule: which channel carries what

The answer to the chapter's question, stated as rules the runtime
enforces:

| Channel | Carries | Enforced by |
|---|---|---|
| **Mail** | Control and short data between processes that are running or will run: a question, a reply, a notice, a pointer | An inline cap (decision 7). A message is addressed to a process chain (3), consumed only when a model has seen it (1), and always has an outcome for the sender (2) |
| **Artifact handle** | Anything over the cap, and anything that is a claim someone should check: a diff, a test log, a sub-agent's result | The cap refuses large bodies and names the handle path. A handle is read under the receiver's capabilities (7). The verdict is derived from records, never from a message (see below) |
| **Event log** | State changes: spawn, exit, supersede, lease, subscription, delivery, dead letter | Only the runtime writes kernel events. No tool lets an agent append one, so a message can never stand in for one |

The rule is enforced in three different ways, and they are not equally
strong:

- **Size** is fully enforced: a body over the cap is refused.
- **State** is fully enforced: an agent cannot write an event, so "I hold
  the lease" in a message changes nothing; the lease table does.
- **Claims** are enforced at the verdict, not at the send. The runtime
  cannot tell a claim from a question. But the workflow's verdict is
  derived from the verifier's recorded exit (`agentflow/verification.go`),
  never from mail or a delegate summary. So a claim sent only by mail
  cannot pass a run. What the slice adds is a way for the claimant to send
  the evidence itself, a handle, so the verifier reads the same bytes
  rather than a description of them.

### 1. Consumed vs delivered (B1c)

**Decision: a message is consumed once a model call's request has
included it. An ack still follows the checkpoint, as today. A message
that its recipient's chain ends without showing to a model becomes a dead
letter (decision 2).**

- Two separate facts:
  - **acked:** the message is durable in the receiver's checkpoint, so
    the bus may drop it;
  - **consumed:** a model has seen it.

  Today the first implies the second, and B1c shows it does not.
- The session gains `MailboxShown`: the IDs of messages a sent model
  request contained. It is set after the call's ledger start line is
  written, so a crash during the call counts as shown. The call may have
  read it, and the successor continues that history and shows it again
  anyway.
- When the recipient's chain ends (defined under decision 2), every ID in
  `MailboxSeen` but not in `MailboxShown` is dead-lettered with reason
  `not_shown`.
- **Why not "redeliver until shown":** an exit after a spent budget is
  final in practice. A successor would start with the budget already
  spent (the restart policy refuses `budget_spent`), so it would never
  make the call either. Waiting for a later call would just be a slower
  form of B2c's silence.
- **Why not move the ack after the model call:** the history is what the
  successor continues. Acking after the checkpoint keeps the exactly-once
  argument of B1a/B1b unchanged. The new field only adds a fact to it.

### 2. Sender outcomes (B2c, B1c, B7)

**Decision: dead letters, and a bounce to the sender's chain. When a
recipient's chain ends with mail it never consumed, each message becomes
an `ipc.dead_letter` kernel event on the recipient. The sender gets a
runtime notice, not a peer message, if its own chain is still live.**

- **When a chain ends.** A process chain ends when its last process:
  - exits with code 0, or exits non-retryably; or
  - exited retryably and is then **closed**: the restart policy escalated
    or declined it (`supervisor.escalated`, or `supervisor.not_restarted`
    with cause `budget_spent`, `no_checkpoint` or `not_recoverable`), or
    an operator closed it.

  Closing is a new kernel operation, `Kernel.Close(pid, reason)`, which
  sets `Process.Closed` and records `process.closed`. Recover refuses a
  closed process. Until a retryable exit is closed or superseded, its
  chain is open, and its mail waits for a successor.
- **What a dead letter holds:** the message ID, the sender, the reason
  (`unread`, meaning never delivered, or `not_shown`), and the
  recipient's final exit (code, reason).
- **The bounce:** a notice to the sender's chain, rendered as a runtime
  line, not as `message from …`:
  `undelivered: msg-000012 to worker (pid-000004) was not read; it exited
  code 0 "done"`.
  - Bounces are never bounced.
  - A sender whose chain has ended gets only the event.
- **Compared with Erlang:**
  - A plain send to a pid "never fails, even if the pid identifies a
    non-existing process" (Erlang reference manual, Expressions, Send:
    https://www.erlang.org/doc/system/expressions.html).
  - The outcome comes from a monitor: if the monitored process
    terminates, a `{'DOWN', Ref, process, Pid, Reason}` message is sent;
    if it does not exist, `'DOWN'` comes at once with reason `noproc`
    (Processes, Monitors:
    https://www.erlang.org/doc/system/ref_man_processes.html).
  - AOS differs in two ways:
    - A send to an ended chain already fails synchronously
      (`ErrRecipientExited`). This is kept, because the sender is a model
      that can act on an error at once.
    - The asynchronous outcome is per message, not per process: an agent
      cares whether *its question* was read, and the kernel log already
      records the exit.
- **Akka** sends undeliverable messages to dead letters, which it treats
  as a debugging aid rather than a delivery guarantee (Message Delivery
  Reliability, Dead Letters:
  https://doc.akka.io/libraries/akka-core/current/general/message-delivery-reliability.html).
  AOS's dead letters are records, but the guarantee is still only that
  the sender is told, not that the message is retried.
- **A known limit:** a retryable exit that nobody closes or recovers
  leaves its chain open, so its mail is neither delivered nor bounced.
  The engine closes the exits it declines to restart. An exit left after
  the engine's run ends stays open on purpose, because an operator may
  still Recover it (`/recover` in the TUI). Its senders are usually in
  the same run, which has also ended.

### 3. Addressing across recovery (B2b)

**Decision: a PID in a `to` field names the process chain it belongs to.
Delivery goes to the chain's live end, following `superseded_by`. A send
is refused only when the chain has ended.**

- Temporal is the close analog:
  - a Workflow ID names the chain;
  - each run has its own Run ID;
  - at most one execution with a given Workflow ID is open at a time
    (https://docs.temporal.io/workflow-execution/workflowid-runid).
  - In the Go SDK, `SignalWorkflow` with an empty run ID "will pick the
    running execution of that workflow ID"
    (https://pkg.go.dev/go.temporal.io/sdk/client).
- AOS's chain is the `resumed_from` / `superseded_by` lineage. Agents
  learn PIDs from delegate results and sender labels, and the PIDs they
  know go stale on every restart. So an agent-facing address follows the
  chain by default. Unlike Temporal, AOS needs no separate ID: any PID on
  the chain resolves to the same chain.
- **What a send to a superseded PID does:** it follows `superseded_by` to
  the live end, records `ipc.forwarded` (`from_pid`, `to_pid`), and the
  sender's result names the PID it reached.
- **A send to a retryably exited, unclosed end:** it is accepted and held
  in that process's mailbox. Recover's `Transfer` moves it, as it moves
  mail sent before the exit (B2a). If the chain is then closed, it is a
  dead letter. This is what makes decision 4 work.
- **Exact-run sends** stay internal (`Bus.Send` to a PID, used by the
  runtime for bounces and replies). No agent tool offers them; no use case
  needs one.
- **Recorded as:** the forwarding is resolved under the bus lock with the
  kernel's state, so a Recover running concurrently either happens before
  it (the send is forwarded) or after it (the send is held and
  transferred). Either way the message is not lost.

### 4. Subscriptions as durable process state (B8c, a defect)

**Decision: a subscription belongs to the process chain, like its mail.
It is dropped when the chain ends, not when a process exits, and
`Transfer` moves it, as the code already intends. Subscribe and
unsubscribe become kernel events. A late subscriber gets nothing
earlier.**

- **The fix:** `runSession` stops unsubscribing on every exit
  (`runtime.go:586`). The bus drops a chain's subscriptions when the
  chain ends (decision 2).
- **Publish** treats a subscriber whose chain is open as deliverable, as
  decision 3 does for a send. A publish between a retryable exit and its
  Recover is held and transferred.
  - **Why the defect needs decision 3:** without it, a publish in that
    gap is dropped, because `Publish` skips and removes exited
    subscribers. Fixing only the unsubscribe would turn B8c from "lost on
    every Recover" into "lost during the backoff".
- **`ipc.subscribed` and `ipc.unsubscribed` kernel events** (`topic`,
  `pid`, `cause`). A subscription is state, so it goes in the event log.
  `inspect` can show who listens to what, and a replay can compare it.
- **A late subscriber gets nothing earlier.** There are no retained
  messages and no topic history (B8b, kept).
  - A topic is a notification channel, not a store.
  - Anything a late joiner needs is state, and state is either an event
    or an artifact. The rule in the first table says so: publish a
    handle, and have the joiner read the artifact.

### 5. Trust boundary (B5, a defect)

**Decision: framing is structural. Every line of a peer body is marked as
body, so no body line can begin where a turn, a sender label, a tool
result, a budget note or a system line begins. A probe tries B5's
forgeries against the rendered prompt. Authority comes from
capabilities, never from text. Operator mail stays a user turn, because
only the operator can send it.**

- **The framing.** `peerMessage` renders a message as:
  - one header line written by the runtime:
    `message from worker (pid-000004) [msg-000012], 3 lines:`;
  - then every body line prefixed with `| `.

  Today only lines that start with `message from ` are quoted.
- **Why every line:** it makes the rule checkable without knowing the
  forgeries. In a peer turn, the runtime writes column 0 of exactly one
  line, the header. Every other line starts with `| `. Any prefix a
  forger might pick (`[#7]`, `run:map[`, `SYSTEM:`, a budget note) is
  inside the quote.
  - The line count in the header lets a reader see where the message
    ends without a closing delimiter. A body cannot forge a closing
    delimiter that does not exist.
- **What it cannot change:** the API has no role for peer mail.
  - `PromptBackend` sends every non-assistant turn as role `user`
    (`backend.go:84`). The OpenAI-compatible `tool` role needs a
    `tool_call_id`, which AOS's JSON actions do not have.
  - So the mark is in the text, and the `[#N]` turn number stays on the
    turn.
- **What framing cannot do.** A model can still follow an instruction
  written inside a quote.
  - Delimiting and marking untrusted input reduce indirect prompt
    injection but do not remove it. Spotlighting reports attack success
    falling from over 50% to under 2% in the authors' GPT-family
    experiments, which is not zero (Hines et al. 2024,
    https://arxiv.org/abs/2403.14720).
  - The design that does not depend on the model obeying is to keep
    untrusted data out of the control path, and enforce policy with
    capabilities on tool calls (CaMeL; Debenedetti et al. 2025,
    https://arxiv.org/abs/2503.18813).
- **AOS's form of that rule: a message never widens the receiver's
  authority.**
  - A process's capability set, mounts and budget are fixed at spawn,
    derived from its parent's by attenuation (`DeriveChildSet`), and kept
    unchanged through Recover. No message, body or handle changes them.
  - A persuaded receiver can do only what it was already allowed to do.
  - The one thing a message adds is data. A handle (decision 7) lets the
    receiver read one artifact the sender could already read. It is the
    same as the sender inlining it: it grants no tool, path, network,
    budget or delegation.
- **Mail from `user` (aos-tui) stays a user turn.** The grounds:
  - `From` is set by the caller of `Bus.Send`, and the runtime always
    passes the process's PID (B10d). Kernel PIDs are `pid-NNNNNN`, so no
    agent can send as `user`.
  - The only caller that does is `aos-tui`, which holds the state
    directory. Anything that holds the state directory can already
    rewrite every record, so it is the operator.

  The slice makes this structural rather than incidental: `Bus.Send`
  refuses the reserved sender `user`. A separate `Bus.SendAsOperator` is
  the only way to send as `user`, and the runtime package never calls it.
  A test checks both.

### 6. Request and response (B7)

**Decision: `ipc.Message` gains `correlation_id`, `reply_to` and
`deadline`. A blocking `await_reply` with a timeout returns exactly one of
four outcomes: the reply, a timeout, or the replier's chain ending,
either unread or with no reply. An await that would deadlock the engine
is refused.**

- **Fields:**
  - `deadline` is a kernel-clock time.
  - `reply_to` is a chain address, defaulting to the sender.
  - A reply carries `correlation_id` set to the request's ID.
  - `message_agent` gains `reply_to: <msg-id>` for a reply and
    `expect_reply: true` (with an optional `timeout`) for a request.
- **`await_reply(id, timeout)`** blocks until one of:
  - `reply` with the body;
  - `timeout`;
  - `recipient_ended` with the exit, when the request was never consumed
    (decision 2's dead letter);
  - `no_reply` with the exit, when the request was consumed and the
    chain ended without a reply.

  The runtime produces the last two when the replier's chain ends. It
  checks every open request addressed to the chain, and records
  `ipc.request_failed`.
- **The default timeout is 5 minutes, and never more than the asker's
  remaining time budget.** The wait counts against that budget.
  - Erlang's `gen_server:call/2` is `call(ServerRef, Request, 5000)`, a
    5 s default; when no reply comes in time, the caller exits with
    reason `timeout`. A dead or nonexistent server gives `noproc`, or
    the server's own exit reason if it died during the call
    (https://www.erlang.org/doc/apps/stdlib/gen_server.html).
  - 5 s suits an in-memory server. A replier here is a model, and one
    step takes seconds to minutes.
- **Temporal's Update** is the same shape: the sender "can wait for a
  response on completion or an error on failure"
  (https://docs.temporal.io/encyclopedia/workflow-message-passing).
  - The Update ID is a UUID by default and deduplicated by the server per
    run.
  - Clients waiting on Updates that do not finish before the run
    completes get Not Found errors
    (https://docs.temporal.io/handling-messages).
  - `no_reply` is AOS's version of that, with the exit attached.
- **The deadlock hazard.** agentflow runs a delegated child inside its
  delegator's `delegate` step, one at a time. A parked child runs only
  when its delegator delegates to it again.
  - So a parent that awaits a reply from its own parked child would wait
    for something that cannot happen until the wait ends.
  - `await_reply` refuses a target that only the asker's engine can run,
    meaning a parked descendant of the asker:
    `would deadlock: pid-… runs only when you delegate to it`.
  - Inside a workflow, `delegate` stays the request and response to a
    child.
  - `await_reply` is for peers that run on their own: another `aos run`
    or a TUI session on the same state directory, or a sibling the engine
    runs concurrently once it can.
- **`await_agent`** stays a non-blocking state read. Its description says
  so.

### 7. Size and handles (B6)

**Decision: inline bodies are capped at 16 KiB, with a refusal that
tells the sender what to do. `message_agent` can attach artifact
handles, and a new `read_artifact` reads one under the receiver's
capabilities. `list_artifacts` is gated to the same set.**

- **The cap:** `ipc.MaxBodyBytes = 16 KiB`. A larger body is
  `ErrMessageTooLarge`, logged as `ipc.rejected` with reason `too_large`:
  `message body is 1048576 bytes, over 16384; publish it with
  publish_artifact and send the artifact_id`.
  - It applies at the bus, so `send`, `publish` and `message_agent` all
    hit it.
  - **Why 16 KiB:**
    - A message is a turn the receiver's model reads in full. Tool output
      is capped at 64 KiB (`tools.MaxToolOutputBytes`), and a message
      should be well under one tool result.
    - With decision 8's mailbox bound, the cap bounds the bus file
      (decision 10).
    - The sender's own action turn holds the body, so its checkpoint grows
      by the same amount (B6: 21 MB for 10 MB).
- **Handles:**
  - `message_agent` takes `artifacts: art-…,art-…`. The bus records them
    on `Message.Artifacts`. The sender must be able to read each one
    itself, so a handle passes on only what the sender holds, the same
    attenuation as delegation.
  - The receiver's `mailbox_message` record names them, and that durable
    record is the receiver chain's grant.
  - **`read_artifact(id, offset, limit)`** returns a published
    artifact's body in pages of at most 64 KiB, like `read_memory`'s
    ranges.
- **Who may read whose artifacts.** A process may read an artifact if
  one of these holds:
  1. its own chain produced it;
  2. it was attached to mail the chain received;
  3. a descendant of the chain published it.

  The parent pays for its children and answers for their work, so it may
  read what they publish.
  - Siblings and unrelated processes need a handle.
  - Runtime records (checkpoints, verdicts, ledger entries) are never
    readable through `read_artifact`. Only `published:*` types are.
- **`list_artifacts`** lists only what the caller may read. Today it
  lists every process's artifacts (`runtime.go:2451`).
- **A sub-agent's summary is a claim.** A delegate result's `summary`,
  or a body saying "tests pass", is text the child wrote about its work.
  Chapter 11's verification applies to it: the verdict comes from the
  verifier's recorded exit, and the verifier should check the evidence,
  not the summary.
  - The handle is how the claimant hands over evidence: the test log, the
    diff.
  - What the runtime enforces is that the summary never becomes the
    verdict. Whether a verifier reads the handle is its own prompt's
    job.

### 8. Backpressure (B8a)

**Decision: a mailbox holds at most 256 unacked messages or 1 MiB of
bodies, whichever comes first. A send to a full mailbox is refused with
`ErrMailboxFull`, which reaches the sender as a tool error. It is not
blocked or dropped. A publish skips full subscribers and reports
them.**

- **Why refuse:**
  - **Blocking** a model step on another agent's mailbox ties up the
    sender's time budget. In agentflow's one-at-a-time engine, the
    receiver may need the sender to finish before it can run at all.
  - **Dropping** breaks at-least-once, and the sender would never know.
  - **A refusal** is synchronous, recorded (`ipc.rejected`, reason
    `mailbox_full`), and a model can act on it: wait, send less, or send
    a handle.
- **Publish:** delivers to the subscribers with room. Its result lists
  the ones it skipped, and each skip is an `ipc.rejected`.
- **Prior art:**
  - Erlang's manuals state no limit on a process's message queue
    (https://www.erlang.org/doc/system/ref_man_processes.html).
  - In Akka the bound is opt-in. The default mailbox,
    `SingleConsumerOnlyUnboundedMailbox`, is unbounded
    (https://doc.akka.io/libraries/akka-core/current/typed/mailboxes.html).
    Its bounded mailboxes pass messages that arrive when full to dead
    letters, or block the sender only with a non-zero
    `mailbox-push-timeout-time`.
  - AOS bounds by default, for the same reason the restart policy has a
    total: an unbounded default is a default nobody sets.
- **Why these numbers:**
  - 256 messages is far above B3/B4's working sets, and well below B8a's
    2,000. A model that has 256 unread messages is not reading its mail.
  - 1 MiB is 64 bodies at the cap.

### 9. Addressing policy (B10a)

**Decision: a capability scoped to targets, defaulting to the workflow's
delegation edges, and attenuated like every other capability. A process
may always reply to a request it received.**

- **The scope:** `capability.Set` gains `MessageTargets`, a list of
  workflow agent IDs plus the keyword `parent`.
  - `message_agent` refuses a recipient whose chain is not one of them,
    with `capability.denied`, as it does today without the capability.
  - The spec field is `message_targets` on an agent.
  - **Absent, it defaults to the delegation edges:** `parent` and the
    agents in its `allowed_agents`.
- **Attenuation:** `DeriveChildSet` intersects. A child's targets are at
  most its parent's targets plus the parent itself. A child cannot reach
  a process its parent cannot. This is the same rule Chapter 4 applies to
  tools and paths.
- **Replies:** receiving a request (`expect_reply`) grants one reply to
  its `reply_to`, even outside `MessageTargets`. This one-shot right is
  recorded on the `mailbox_message`. So a server agent can answer whoever
  asked it without being able to start a conversation with them.
- **Why a capability and not workflow edges alone:** edges exist only in
  a workflow. A capability also covers a single `aos run` and TUI
  sessions, where the default with no workflow is "replies only".
  Delegation already makes the workflow graph the source of grants, so
  the edges are the default, not a separate mechanism.
- **Topics** are scoped the same way later (`topic:<name>` entries).
  This slice leaves them under the plain capability.

### 10. Bus durability (B10c, B6's rewrite)

**Decision: one file. `state.json`, `mailboxes.json` and
`subscriptions.json` become one `bus.json`, written by one atomic
replace. An append-only journal is not needed at the sizes the caps
allow.**

- **Why one file is the smaller change:** `Bus.update` already loads and
  writes all three under one lock in one transaction. Merging them makes
  the write match the transaction: one temp file, one fsync, one rename.
  - `load` reads the old three-file layout once and writes `bus.json`.
  - A journal would need compaction, replay on open and a format, all for
    a property one rename already gives.
- **Today's split is benign but fragile.** A crash between the writes can
  leave subscriptions behind the mailboxes. IDs are safe only because
  `load` re-derives the counter from the mailboxes.
- **The per-send cost:**
  - Each send still rewrites the whole bus, which is O(bus size). B6
    measured 20 ms on an empty bus and 140 ms behind 20 MB. B8a measured
    33 ms at 542 KB.
  - With decisions 7 and 8, a live mailbox holds at most 1 MiB, so the
    bus is at most 1 MiB per live chain with mail. A workflow of a few
    agents stays in the tens of milliseconds per send, against model
    steps of seconds.
  - The cost would matter with hundreds of busy agents on one state
    directory. That is when a journal, or one file per mailbox, earns its
    complexity. Not now.

### 11. Replay (B9)

**Decision: replay feeds the recorded mail back. Each `mailbox_message`
artifact the replayed process received is delivered in the step where
the record received it, so a single-agent run with operator mail
replays. Multi-agent replay stays out of scope.**

- **How:** the record already has what this needs.
  - Each received message is a `mailbox_message` artifact.
  - The checkpoints' `MailboxSeen` shows the step at which each first
    appeared.
  - Replay's bus is a player: at step N it hands out the messages the
    record first holds at step N, in recorded order, and acks them as
    recorded.
  - With decision 1 in place, `MailboxShown` must match as well.
- **Mail the replayed agent sends** is compared against the recorded
  `ipc.sent` events and not delivered, as tool effects are compared.
- **Multi-agent runs stay refused** (`replay.go:245`). Their mail ordering
  depends on the scheduling of several processes, and replaying that is
  a different project.
- **TUI sessions:** this removes one of the two reasons they are not
  replayable, since their input arrives as mail from `user`. The other is
  that they are multi-turn (`replay-design.md`, "As built", Limits).

### Not a guarantee: mail versus delegate results (B10b)

Mail and delegate results are separate channels, with no order between
them. A child that mails its parent and then exits is seen in the order
B10b shows: the result first, because the parent reads mail only at its
next step. If order matters, the child puts it in its result. The chapter
should state this rather than leave it implied.

## Prior Art, Briefly

The book is verifying these sources itself. Each default or number above
names its URL.

- **Erlang/OTP:**
  - asynchronous send that never fails;
  - monitors and `'DOWN'`, with `noproc`;
  - `gen_server:call` with a reference and a 5 s default timeout;
  - no stated mailbox bound.

  AOS takes the call shape and the per-call timeout. It fails a send to
  an ended chain synchronously, rather than silently. It bounds mailboxes
  by default.
- **Temporal:**
  - a Workflow ID versus a Run ID;
  - a signal by Workflow ID alone goes to the running execution;
  - Updates with an ID and a result or failure;
  - Signals are fire-and-forget.

  AOS's process chain plays the Workflow ID. `await_reply` plays an
  Update. Plain mail plays a Signal.
- **Akka:**
  - dead letters;
  - bounded mailboxes as an opt-in, with overflow to dead letters.

  AOS refuses rather than diverting, because the sender can act on a
  refusal.
- **Prompt injection:**
  - Spotlighting (delimiting and marking untrusted input) reduces but
    does not remove injection;
  - CaMeL keeps untrusted data off the control path and enforces policy
    with capabilities.

  AOS frames mail structurally, but its boundary is the capability set,
  which no message can change.
- **What an agent kernel adds:**
  - **A message is consumed when a model reads it**, not when a process
    stores it. A process can hold mail its model never sees, and that has
    to be an outcome.
  - **Every channel is charged and bounded:** bodies, mailboxes, waits
    against the time budget.
  - **Claims are separated from evidence.** A message can say anything;
    a verdict is derived from records.

## Gaps

| Gap | Contract clause | Where |
|---|---|---|
| B5 Forgeable framing (defect) | 5: every body line marked; reserved operator sender | `runtime.peerMessage`, `ipc.Bus.Send` |
| B8c Subscriptions lost on Recover (defect) | 4: chain-owned subscriptions, events | `runtime.runSession`, `ipc.Bus` |
| B2b No forwarding after Recover | 3: chain addressing, `ipc.forwarded` | `ipc.Bus.deliverable`, `kernel` |
| B2c Stranded mail is silent | 2: chain end, dead letters, bounces; `Kernel.Close` | `kernel`, `ipc`, `agentflow.supervise` |
| B1c Acked but never shown | 1: `MailboxShown`; `not_shown` dead letters | `runtime` session |
| B7 No request and response | 6: correlation, deadline, `await_reply`, deadlock refusal | `ipc.Message`, `runtime` tools |
| B6 No size cap; no handle read; `list_artifacts` ungated | 7: 16 KiB cap, handles, `read_artifact`, gated listing | `ipc`, `runtime` tools |
| B8a No backpressure | 8: 256 messages / 1 MiB, `ErrMailboxFull` | `ipc.Bus` |
| B10a Any-to-any | 9: `MessageTargets`, attenuated; reply right | `capability`, `agentflow` spec, `runtime` |
| B10c Three files, not one write | 10: one `bus.json` | `ipc.Bus.update` |
| B9 Replay drops mail from outside the run | 11: a replay bus that plays the record | `replay` |

## Hazards

- **Forwarding races Recover.** A send that resolves the chain's live
  end and then enqueues in a separate step could pick a process that is
  superseded in between. Resolution and enqueue happen under the bus
  lock, reading the kernel's state there, the order the bus already uses
  (the kernel's lock may be taken inside the bus's, never the reverse).
  `Recover`'s `Transfer` takes the bus lock too, so the two are
  serialized.
- **Open chains hold mail indefinitely.** Decision 3 accepts mail for a
  retryably exited process. If nothing ever closes it, the mail neither
  arrives nor bounces (the known limit under decision 2). The engine must
  close every exit it declines to restart. A probe should check that no
  `supervisor.not_restarted` leaves its chain open.
- **Bounce storms.** A sender with many outstanding messages to a chain
  that ends gets one notice per message. Notices to one sender from one
  ended chain should be one notice listing the IDs. Bounces are never
  bounced.
- **Framing changes every prompt with mail.**
  - The rendered peer turn changes, so a recorded run with mail no longer
    produces the same request. `run.started` records `peer_framing: v2`,
    and a record without it is replayed with the old rendering
    (`legacy_peer_framing`), as `NoToolIntents` and `NoLeaseFencing` did.
  - Prompt caching is unaffected beyond the new turns, because the change
    is inside appended history.
- **The cap breaks large sends that work today.** A workflow that mails
  big bodies starts getting tool errors. The refusal names the remedy,
  and the change goes in the commit's "changed for existing runs".
- **Deadlock detection is only for the known case.** `await_reply`
  refuses a parked descendant of the asker. A cycle across separate `aos`
  processes (A awaits B while B awaits A) is not detected. It ends by
  timeout, which is why the timeout is never unbounded.
- **Handles are grants that outlive the message.** A received handle stays
  readable for the receiver's chain, after the sender's chain ends. That
  is the point of a handle. It also means a handle cannot be revoked:
  sending one is like sending the bytes.
- **Replay of mail.**
  - The player must deliver at the recorded step, not by wall time.
  - A record whose `MailboxSeen` cannot place a message, such as one
    written before checkpoints held it, is refused as not replayable
    rather than guessed.

## Recommendation: the Smallest Slice

In order. Each step is one commit with its tests, and each flips named
probes from logging to asserting.

1. **Structural framing (B5, a defect).**
   - Header line plus `| `-marked body in `peerMessage`; header carries
     the line count.
   - `Bus.Send` refuses the sender `user`; `Bus.SendAsOperator` for
     `aos-tui`.
   - `peer_framing` on `run.started`, legacy rendering for older records.
   - *First because:* it is a defect, it is local to two functions, and
     it touches only how mail reads. Nothing else in the slice depends on
     it, and nothing in it depends on the rest.
2. **Chain-owned delivery (B8c, a defect, and B2b).**
   - `Kernel.Close` and `Process.Closed`. agentflow closes the exits it
     escalates or declines.
   - `deliverable` accepts an open chain and follows `superseded_by`
     (`ipc.forwarded`).
   - Subscriptions are dropped at chain end instead of at exit;
     `ipc.subscribed` / `ipc.unsubscribed` events.
   - *Why B2b rides with B8c:* without holding mail for an open chain,
     fixing B8c's unsubscribe still loses every publish made during a
     restart's backoff (decision 4). The defect fix and chain addressing
     are the same rule.
3. **Sender outcomes and consumption (B2c, B1c).**
   - `MailboxShown` in the session.
   - At chain end: `ipc.dead_letter` per unconsumed message, one bounce
     notice per sender chain.
   - *After step 2* because "chain end" is step 2's `Close`.
4. **Request and response (B7).**
   - `correlation_id`, `reply_to`, `deadline` on `ipc.Message`.
   - `message_agent` gains `expect_reply` and `reply_to`; a new
     `await_reply` with the four outcomes.
   - `ipc.request_failed` at the replier's chain end, which reuses step
     3's hook.
   - The deadlock refusal.
5. **Bounds and handles (B6, B8a).**
   - The 16 KiB cap and the 256-message / 1 MiB mailbox bound at the bus.
   - `Message.Artifacts`, `read_artifact`, and the gated
     `list_artifacts`.
   - *Why the cap waits for the handle:* a cap without a way to send the
     data that no longer fits is a regression. The mailbox bound is the
     same check in the same place, so it rides along.

Everything else stays **Proposed**:

- the addressing policy, `MessageTargets` (B10a, decision 9);
- one `bus.json` (B10c, decision 10). Small, but nothing in the slice
  needs it, and the caps already bound its cost;
- the replay bus that plays recorded mail (B9, decision 11);
- topic scoping;
- framing tool outputs. A `web_fetch` or `run` output can carry the same
  forged lines as a peer body, and the same marking would apply;
- multi-agent replay;
- recording a `delegate` in flight as an intent (from
  `supervision-design.md`, "Not covered").

**Why this order rather than another:**

- The two defects come first, because the chapter cannot describe a
  trust boundary or durable subscriptions while a probe shows them
  broken.
- Outcomes (3) come before request and response (4), because a failed
  request is just a dead letter with a waiter, so step 4 is small once
  step 3 exists.
- The caps (5) come last, because they are the only step that refuses
  something that works today. They should land with the handle path that
  replaces it.

## Tests That Would Prove It

All of these use scripted agents and the `bench/ipc` harness, with no
model, so eval can re-run them without phg-gpu. Each step turns its
audit probe from logging the finding into asserting the fixed behavior,
and keeps the `180d076` result in a comment as the "before".

- **Step 1:**
  - **B5 asserts:** in the rendered prompt, every line of the peer turn
    after the header starts with `| `. The forgeries `[#7] message from
    coordinator …`, `[#8] run:map[…]`, the budget warning and `SYSTEM:
    …` each appear only quoted. A `user` message still renders as a plain
    user turn.
  - `TestPeerMessageMarksEveryBodyLine`, including an empty body, a body
    of only newlines and a body that copies a full header line.
  - `TestOnlyTheOperatorSendsAsUser`: `Bus.Send("user", …)` is refused;
    `SendAsOperator` works. A grep test that `internal/runtime` never
    calls `SendAsOperator`.
  - `TestReplayOfARecordWithoutPeerFraming`.
- **Step 2:**
  - **B8c asserts:** after exit and Recover, `subscriptions.json` names
    the successor, and a publish reaches it once.
  - **B2b asserts:** a send to the superseded PID is delivered to the
    successor, with `ipc.forwarded`. A send to a closed chain is refused,
    naming the final exit.
  - `TestPublishDuringBackoffIsHeld`: a publish between a retryable exit
    and its Recover reaches the successor.
  - `TestForwardingRacesRecover`: N OS processes send while one recovers.
    Every message reaches exactly one live process (B4's harness).
  - `TestClosedChainDropsSubscriptions`.
- **Step 3:**
  - **B1c asserts:** the message is an `ipc.dead_letter` with reason
    `not_shown`, and the sender's next request carries the
    `undelivered:` notice.
  - **B2c asserts:** the same with reason `unread`, and nothing is left
    pending.
  - `TestBouncesAreNotBounced`, `TestOneNoticePerEndedChain`, and
    `TestRetryableExitKeepsMailUntilClosed`.
- **Step 4:**
  - **B7 asserts:** a reply before the deadline returns `reply`. No reply
    gives `timeout` at the deadline, on the kernel clock. The child
    exiting without answering gives `no_reply` with its exit, and an
    unread request gives `recipient_ended`.
  - `TestAwaitOnParkedChildIsRefused`: the deadlock case.
  - `TestAwaitIsChargedToTheTimeBudget`.
- **Step 5:**
  - **B6 asserts:** the 1 MB and 10 MB sends are refused with
    `ErrMessageTooLarge`, naming the remedy. The same data sent as a
    handle is readable by the receiver through `read_artifact`, in 64 KiB
    pages. An unrelated process's `read_artifact` and `list_artifacts`
    do not see it.
  - **B8a asserts:** the 257th unread message is refused with
    `ErrMailboxFull`, and a publish reports the skipped subscriber.
  - `TestHandleNeedsTheSenderToReadIt`: a sender cannot pass a handle it
    cannot read.
  - `TestRuntimeRecordsAreNotReadable`: a checkpoint ID is refused.
- **Later steps:**
  - B10a asserts the refusal outside `MessageTargets`, and the reply
    right;
  - B10c asserts one file after a send;
  - B9 asserts that the operator-mail run replays identically.
- **Kept as they are:** B1a, B1b, B2a, B3, B4, B8b and B10d already
  hold. They become regression checks, and B4 must still show 0 lost
  and 0 duplicated after steps 2 and 5 change the bus.
