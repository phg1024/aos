# Memory System Design

Status: proposal, 2026-10-29, revised after review by the aos book session.
Nothing here is implemented yet except where a section says so.

## Why

AOS compacts an agent's history into runtime-owned `memory_segment` artifacts
and lets the agent read them back with `read_memory`. Commit `eceb917` made
that tool visible in the prompt and paged its output. The live path-tracer run
on that binary (`reports/pathtracer-bunny-memfix`, 65,536-token window,
compaction at about 157,000 characters) is the first run where agents used it.

Snapshot as of 03:57Z on 2026-10-29, during round 4 of 10. The run had 16
segments by 06:47Z; the evidence below does not depend on the later ones.

- 13 compactions, all in writers: 2, 3, 7 and 1 per writer process.
- 3 `read_memory` calls, all from the round-3 writer (`pid-000009`), all for
  the right reason. Before retrying an idea, it looked up what that idea had
  measured. Later it checked a contradiction between its notes and an older
  segment: the segment said "leaf2 adopted, 33.5 ms", the current notes said
  leaf2 was not adopted.
- The mechanism worked against it:
  - **Thrash.** Compaction keeps the 12 most recent messages whatever their
    size. With 64 KB tool results among them, the kept tail alone can exceed
    the trigger. `art-001069` is 11 messages and only 15,904 characters,
    archived at a 157,000-character trigger, so the kept tail is what carried
    the bulk. It came four minutes after the previous compaction and four
    minutes before the next. The writer's live history peaked at 186,200
    characters, above the trigger.
  - **Nesting.** A `read_memory` result is an ordinary tool message, so the
    next compaction copies it into a new segment. `art-001074` holds 4
    messages and 126,762 characters. Two of them are `read_memory` results:
    65,640 characters copied from `art-000969` and 59,963 from `art-001056`.
    The other two are the agent's own short turns. The writer read it
    because the ref said "4 messages", got old content again, and never
    resolved the contradiction.
  - **Blind paging.** The writer could only page by byte offset, so it
    guessed offsets (`46000`, `45000`).
  - **No authoritative record.** Notes are part of assistant messages and are
    compacted with them. The contradiction existed because two notes, one
    archived and one live, both claimed to be current.
  - **Growing header.** One `MemoryRef` per compaction, each with a
    mechanical summary of 180-character fragments.

Patching each of these on its own would leave the design where it is: one
compaction pass, one paging tool, and no place for state that must survive.
This document proposes the whole memory system instead.

## What is already committed to

From the book (Chapter 3, *Context Is RAM, Artifacts Are Storage*):

- Rule 1: context is derived. It is assembled from storage before each call
  and is authoritative for nothing.
- Rule 3: artifacts are write-once, attributable, and linked.
- Rule 4: compaction moves context into storage. Old history goes into an
  artifact the agent can read back, not into the void.
- The imitation lesson (Chapter 3 opening): a model reproduces the shape of
  its own past turns. Never put placeholders, summaries, or truncations where
  the model will read them as examples of its own actions.

From `guiding-principles.md` §2–3: durable memory lives outside the model;
handoffs are structured artifacts ("what was attempted, what changed, what
remains, what failed, what should happen next"); compaction and reset are
policies, not assumptions.

Chapter 3 also records a limit this design removes: two places decide what
the model sees, the runtime's compaction and the backend's own prompt bound.

## Goals

- The live context stays under a hard budget, and compaction does not
  re-trigger immediately.
- Nothing is lost: every message is recoverable verbatim, exactly once.
- An agent can find an old fact by what it is about, not by byte offset.
- There is one authoritative, agent-maintained record of current decisions
  and measurements. It survives compaction and recovery, and it takes
  precedence over the archive.
- The prompt prefix stays stable between compactions, so prefix caching keeps
  working.
- Everything is deterministic and stdlib-only. No embeddings, no extra model
  calls in the runtime.

Non-goals: memory shared between agents (that is IPC, Chapter 8), memory
across workflow runs, and semantic or vector search.

## Model

Four layers, from what the model sees to what only storage holds:

| Layer | Holds | Written by | In the prompt |
| --- | --- | --- | --- |
| Working set | The prompt for one call | Runtime, assembled each call | Is the prompt |
| Working memory | Current decisions, measurements, rejected ideas, open questions, next steps, as keyed entries; size-capped | Agent, through `update_memory`; stored by the runtime | Snapshot in the header, refreshed at compaction; updates in live history between |
| Live history | Recent messages, under a character budget | Runtime | Yes, verbatim |
| Archive | Every compacted message, verbatim, numbered | Runtime | No; found with `search_memory`, read with `read_memory` |

The checkpoint holds the live history, the working memory's artifact ID, and
the archive index. The archive and every working-memory version are
artifacts.

**Precedence.** Working memory is current; the archive is history. This is
stated in the header line, in the system prompt's description of the memory
tools, and in every `search_memory` result (see Retrieval).

## Mechanisms

### Message numbering

Every message in a session gets a sequence number when it is appended
(`Message.Seq`; `Session.NextSeq`). Numbers never change and are never
reused, including across Resume and Recover. Segments hold contiguous ranges.
The archive is then addressable as "messages 412–470" regardless of how it
was cut, and every tool below speaks in sequence numbers.

Checkpoints written before this change have messages without numbers. On
restore they are numbered in order, and their existing segments stay
readable with `artifact_id`/`offset`. Resuming the checkpoints of the run
above must keep working, because the replay evaluation depends on them.

### Compaction: budget, hysteresis, clean cuts

- Trigger at a high watermark `H` (as now: window × ratio × 3 characters).
- When it fires, archive the oldest messages until the live history is at or
  below a low watermark `L`, default `H / 2`. The live tail is chosen by
  characters, not by count.
- Never cut between a tool call and its result, and always keep the latest
  call/result pair live.

**Precondition, checked when the runtime is configured.** Hysteresis only
prevents thrash if no single message can fill most of the gap. Configuration
is rejected with an error unless every per-call output cap is well under
`H − L`. That covers the tool output cap (64 KB today) and the `read_memory`
and `search_memory` caps. At a 65,536-token window, `L` ≈ 78,600 characters
and the current caps pass. At a 16,384-token window, `H` ≈ 39,300 characters,
so a single 64 KB result exceeds `H` itself and every step would compact. That
configuration must be refused, or the caps scaled to the window, before the
run starts. `compaction.oversized` remains as a runtime trace event for the
one case configuration cannot rule out: a call/result pair whose action is
itself large, such as a long `write_file`.

With the precondition met and `L = H / 2`, the next compaction is at least
`H − L` characters of new history away.

### Pre-compaction handoff

When `H` is crossed, the runtime does not compact at once. It appends one
message, labelled the way peer messages are, so that it reads neither as a
tool's output nor as the user:

> runtime notice: History through message N will move to the archive before
> your next call. Your working memory is what you will still see. Update it
> now with anything from messages up to N that you will need: results,
> decisions, and what you rejected and why.

The agent gets one step. The runtime compacts before the next model call
whatever that step was:

- a tool call, typically `update_memory`: compact before the next call;
- `delegate` or `wait`: compact before the next model call after the process
  resumes;
- `exit`: no compaction. The run is over, and the final checkpoint keeps the
  full live history.

A model that ignores the notice costs one step and nothing else. This is the
"structured handoff" of the guiding principles, done by the only party that
knows what matters (decision D1).

### Working memory

- Entries are keyed: `update_memory section=... key=... content=...` sets one
  entry, and `update_memory section=... key=... delete=true` removes it.
  Sections are `decisions`, `measurements`, `rejected`, `open` and `next`.
  Replacing per key gives one current claim per subject, and the leaf2
  decision is one key. The agent never has to rewrite a whole section, so
  there is no pressure to drop entries silently to stay under the cap.
- Each update is a new `working_memory` artifact. Its parent is the previous
  version, so the chain of versions is book rule 3 applied: write-once,
  attributable, and linked. The session holds the latest version's ID.
- A hard cap, default 8 KB total. An update over the cap fails with an error
  giving the current size of each section and the size of the rejected
  entry, so the agent can decide what to cut. It is never truncated, so no
  truncated text is ever shown to the model as its own.
- **Exactly once across recovery.** If `update_memory` writes version N+1 and
  the process crashes before the checkpoint records it, the replayed step
  would write a second N+1 with the same parent. Unlike the external side
  effects of Chapter 3, this effect is runtime-internal, so it can be made
  exactly-once. Each version records an idempotency key: the parent ID, the
  section and key, and the SHA-256 of the content. Before writing, the
  runtime looks for an existing child of the parent with the same key and
  adopts it instead.
- Prompt placement keeps the append-only layout. Between compactions, the
  `update_memory` calls are already in the live history verbatim. At each
  compaction, the header, which only changes at compaction today, is
  rebuilt with the current snapshot. The prefix changes exactly as often as
  it does now.
- Resume and Recover carry it over, like the rest of the session.
- `notes` stays. It is step-to-step scratch; working memory is what survives
  (decision D2).

### Archive without nesting

A segment stores the original messages of its range. When a `read_memory` or
`search_memory` result is archived, its content is not copied. The segment
stores a one-hop reference instead:

```text
read_memory result (messages 412–470) — read_memory from=412 to=470
```

`read_memory` never resolves references recursively. A read that returns an
archived reference returns only the reference, so a read of a read cannot
bring back 130 KB.

This puts a reference where full text was, which the imitation lesson warns
about. It is acceptable here for three reasons:

- the model never authors tool results, so it has no turn of its own whose
  shape it could copy;
- a reference appears only inside read or search output, never as a turn
  the model authored;
- it appears only in the archive, never in live history.

`TestArchiveStoresEachMessageOnce` and the replay evaluation check this.

### Retrieval

- `search_memory query=... limit?` returns up to `limit` (default 10) hits.
  A hit is a case-insensitive substring or `re:`-prefixed regexp match. It
  searches the process's own archive and its own working-memory versions.
  - An archive hit gives the sequence number, role, time, and a window of
    about 300 characters around the match.
  - A hit in an old working-memory version is marked "superseded in vN",
    with the current entry under that key, if any. The agent can then see
    both what changed and why.
  - Every result begins with the precedence line: "working memory vN
    (time) is current; archive hits are history."
  - Hits are newest first, and output is capped at 8 KB.
- `read_memory from=... to=...` returns messages by sequence number, capped at
  32 KB. If a range is too large, it returns whole messages up to the cap and
  `next_from`. `artifact_id`/`offset` keep working for old checkpoints.
- Both are always allowed, like `read_memory` today, and only ever read the
  calling process's own memory (`ownedBy`).

### Context assembly has one owner

- The header shows the working-memory snapshot, the precedence line, and one
  index line instead of a ref per segment, for example: "Archive: messages
  1–905 in 13 segments (search_memory to find, read_memory to read)". It has
  no prose summary.
- The index lists factual metadata per segment and says exactly where each
  field comes from:
  - the sequence range and time span;
  - the tool names called;
  - "paths passed to `read_file`, `write_file` and `edit_file`".
  
  Files that `run` commands touch do not appear, and the index says so, so it
  never reads as a complete list of files.
- When a runtime compaction policy is set, the backend's own history bound
  (`compactHistory` in `internal/agentflow/backend.go`) is disabled. The
  prompt's messages are then exactly the session's live history.
  `TestPromptHistoryIsTheSession` asserts it.

### Tool results in live history (experiment)

Large tool outputs, such as file reads and build logs up to 64 KB, are what
fill the window. One option is to store any output over a threshold as an
artifact and keep a reference plus its head in live history. Chapter 3's
failures came from placeholders in the model's own actions, not in tool
results. Unlike the archive references above, these would be in live
history, so they need evidence. This is phase 4, behind a flag, and it is
adopted only if the replay evaluation shows no imitation and no loss.

## Decisions

| # | Question | Options | Status |
| --- | --- | --- | --- |
| D1 | Who writes what survives compaction | (a) runtime mechanically; (b) a separate model call; (c) the agent, through a pre-compaction handoff | **Decided 2026-10-29: (c).** It is the cheapest, it keeps the runtime free of inference, and the agent is the only party that knows what mattered. The runtime contributes only exact facts (the index). |
| D2 | Where working memory lives | (a) a runtime-owned artifact; (b) a file in the agent's workspace, as writer ledgers are today | **Decided 2026-10-29: (a).** It is auditable, versioned, carried over on recovery, and cannot be overwritten by `run` or `write_file`. Workspace ledgers stay useful for handing off between agents. |
| D3 | References in place of large tool results in live history | adopt; reject; experiment first | Open. Recommendation: experiment first (phase 4). The nesting fix uses references only in the archive, which does not depend on this decision. |

## Contracts and tests

| Contract | Test |
| --- | --- |
| Configuration is refused when an output cap is not well under H − L | `TestCompactionRejectsCapsOverHysteresis` |
| Live history after compaction is ≤ L; the next compaction needs ≥ H − L new characters | `TestCompactionHasHysteresis` |
| A cut never separates a tool call from its result | `TestCompactionCutsCleanly` |
| An oversized call/result pair compacts and records `compaction.oversized` | `TestOversizedTailIsReported` |
| Sequence numbers are unique and stable across compaction, Resume and Recover | `TestMessageSeqSurvivesRecovery` |
| Checkpoints and segments from before numbering resume and read as before | `TestUnnumberedCheckpointsStillResume` |
| Each original message is stored in exactly one segment; read and search results are archived as one-hop references | `TestArchiveStoresEachMessageOnce` |
| `read_memory` of an archived reference returns the reference, not its target | `TestReadMemoryDoesNotFollowReferences` |
| `search_memory` finds a fact from any archived segment and only the caller's own | `TestSearchMemoryFindsOwnArchiveOnly` |
| Search results state precedence and mark superseded working-memory entries | `TestSearchMarksSupersededEntries` |
| `read_memory` by range returns whole messages under the cap, with `next_from` | `TestReadMemoryByRange` |
| A labelled notice precedes compaction; a tool step compacts next call | `TestCompactionGivesOneHandoffStep` |
| After a handoff step that delegates or waits, compaction happens at the next model call; after an exit, none | `TestHandoffStepThatDelegatesWaitsOrExits` |
| `update_memory` sets and deletes keyed entries, versions the artifact, and rejects over-cap updates with per-section sizes | `TestWorkingMemoryVersionsAndCaps` |
| A replayed `update_memory` adopts the version an earlier attempt wrote | `TestUpdateMemoryIsIdempotentAcrossRecover` |
| Working memory survives Recover | `TestWorkingMemorySurvivesRecover` |
| The header changes only at compaction; between compactions, prompts are append-only | `TestPromptPrefixStableBetweenCompactions` |
| With a runtime policy, the prompt's history equals the session's live history | `TestPromptHistoryIsTheSession` |

## Evaluation

1. **Unit tests**, as above, for every phase.
2. **Offline replay.** From checkpoints of `reports/pathtracer-bunny-memfix`,
   resume a writer with one appended question, then score the answer against
   the segment text. Measure correctness, steps, and bytes read.
   - Archive questions, whose answer exists only in the archive:
     - What did TriData emission order measure?
     - Which threadgroup sizes were swept, and what won?
   - Control: a question whose answer is in live history. The agent should
     answer without searching.
   - Control: a question whose archived answer was later superseded: "Is
     leaf2 (2 triangles per leaf) adopted?" The correct answer is the newer
     one. This tests precedence directly.

   Use the same model and parameters as the run (temperature 0.6, top-p
   0.95), several samples per question, and report per-question variance.
   Compare the current memory system with each phase.
3. **Live rerun** of `pathtracer-bunny-metal` on the new binary. Measure:
   - compactions per process and the smallest segment size (no thrash);
   - peak live characters (must stay ≤ H unless `compaction.oversized`);
   - `search_memory`, `read_memory` and `update_memory` calls;
   - superseded facts cited as current after a compaction, the failure this
     run showed;
   - repeated experiments, found as benchmark commands that recur after a
     compaction with no code change in between;
   - the final speedup and verifier verdicts, to confirm nothing regresses.

## Phases

Each phase ends with tests, a commit and an HTML report in `reports/`.

1. **Mechanics.** No prompt changes yet: sequence numbers with compatibility
   for unnumbered checkpoints, budget-based compaction with hysteresis and
   the configuration precondition, clean cuts, no nesting, and one owner of
   context.
2. **Retrieval.** `search_memory`, `read_memory` by range, and the header
   index in place of per-segment summaries.
3. **Working memory.** Keyed `update_memory`, the versioned `working_memory`
   artifact with idempotent writes, precedence in prompt and search, and the
   pre-compaction handoff step.
4. **Experiment.** References for large tool results in live history behind
   a flag, judged by the replay evaluation.

The replay harness is built in phase 1, so every later phase is measured
against the current system.

## Book impact

Chapter 3 describes its pinned commit, so the book does not wait for these
phases. When the current run ends, the book session plans to revise Chapter
3 against a commit that includes `eceb917`:

- `read_memory` used for real becomes Demonstrated;
- nesting and thrash become the chapter's second failure story: the archive
  is verbatim, yet a copy of a copy still misleads the reader;
- this four-layer design becomes Proposed, with a pointer here.

After phase 3, the Compaction section is rewritten around the four layers. The
"two places decide what the model sees" limit goes away in phase 1. Idempotent
`update_memory` gives the chapter a contrast with its unfixed external ledger
case: when the runtime owns the effect, exactly-once is achievable.

## Open questions

- Should a new writer in the next round start from the previous writer's
  working memory, passed as an artifact handle by the coordinator, instead of
  only from the workspace ledger? It would be cross-process memory, so it
  belongs with Chapter 8's handles, but it is the most likely way to stop
  the round-4 writer re-reading what round 3 learned.
- Is 8 KB of working memory right for a 65,536-token window? Should it, and
  the output caps, scale with the window?
