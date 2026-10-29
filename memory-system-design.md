# Memory System Design

Status: proposal, 2026-10-29, revised after review by the aos book session.
Phase 1 is implemented (see "Phase 1 as built"); the rest is not.

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
- **What it cost.** The 03:58 read of `art-001074` returned only copies, so
  the contradiction stayed open. At 04:07 the writer read an old note in the
  archive ("leaf2 measured 33.5 vs 45.8") as current, concluded that "the
  leaf2 adoption was lost", and adopted leaf2 again. It then spent about 20
  steps re-measuring leaf2 and found again that it was biased, which it had
  already established at 03:3x. That earlier finding was in `art-001056`
  the whole time. This is the failure the design is for: a superseded fact
  in the archive, taken as current, and the work redone.
- **And again.** In `art-001220` (msgs 46–66), the same writer decided that
  leaf2 "almost certainly" had a bug in its second-triangle slot. It built
  a new variant, leaf2b, fixed a shader error on the way, measured it 14%
  slower, and rejected it. `art-001056` (msgs 50–62) had already found and
  fixed that bug (RMSE 0.475902 against 0.475898) and measured the fixed
  leaf2 as a loss against wide1. The question was answered at 03:3x and
  asked again about an hour later. This is a different failure: no
  superseded fact was cited, an archived one was forgotten and the
  experiment repeated. The evaluation counts the two kinds separately. In
  `pid-000009` there is one of each.

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
run starts. A `memory.compacted` kernel event marked `oversized` remains for
the cases configuration cannot rule out: a call/result pair whose action is
itself large, such as a long `write_file`, or a `run` result, whose stdout
and stderr are capped at 64 KB each.

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
  A hit is a message holding every word of the query, or of one of its
  `|`-separated alternatives, or matching an `re:`-prefixed regexp; all
  case-insensitive. It
  searches the process's own archive and its own working-memory versions.
  - An archive hit gives the sequence number, role, time, and a window of
    about 300 characters around the match.
  - A hit in an old working-memory version is marked "superseded in vN",
    with the current entry under that key, if any. The agent can then see
    both what changed and why.
  - Every result begins with the precedence line: "working memory vN
    (time) is current; archive hits are history." Working memory is
    current because it is the record the agent maintains, not because it
    is true. Among archive hits the runtime knows only order, so it says
    only that: a higher number is later, not necessarily right.
  - Hits matching more alternatives come first, newest first among equals,
    and output is capped at 8 KB.
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
- The backend's own history bound (`compactHistory` in
  `internal/agentflow/backend.go`) is removed. The prompt's messages are
  exactly the session's live history. `TestPromptHistoryIsTheSession`
  asserts it.

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

## Alternatives considered

Measured on the run above at 06:47Z: one writer's archive is 16 segments and
2.35 MB, and all of the run's artifacts total 184 MB. A naive scan for
`leaf2` that parses every segment and runs a regexp over every message takes
13 ms and finds 80 hits.

The 80 hits are the point. The failure in this run was not finding a fact.
It was telling which of several matching facts is current. Keys, versions
and precedence solve that. A different search engine does not.

### Vector search

Not adopted.

- **The facts agents look for are exact.** The three `read_memory` calls
  looked for "33.5 ms", "TriData emission order" and "TG4": numbers, names
  and paths. Substring and regexp match them exactly. Embeddings rank
  "leaf1 45.8 ms" and "leaf2 33.5 ms" as near neighbours.
- **Similarity cannot express supersession.** A superseded decision and the
  one that replaced it are near-identical in meaning, so similarity may rank
  the old one first. Precedence has to be structural.
- **It breaks two non-negotiables.** Embedding every message is an inference
  call, so the runtime would depend on an inference provider. And a change
  of embedding model changes the results of the same query, so retrieval
  would no longer be deterministic, and replays would not reproduce it.
- **The scale does not call for it.** Each process searches only its own
  memory, a few MB per writer. Approximate nearest-neighbour indexes pay off
  at millions of items.

### SQLite

Not adopted now. It is the stronger alternative:

- FTS5 gives ranked full-text search (BM25);
- a transaction across rows would simplify idempotent `update_memory`;
- range queries by sequence number are natural.

Against it, today:

- **Dependency.** AOS is stdlib-only unless a dependency is clearly
  justified, and its early persistence is JSON files for that reason. In Go,
  SQLite means either cgo (`mattn/go-sqlite3`), which complicates
  cross-compiling and the Linux sandbox's re-exec helper, or a large
  pure-Go port (`modernc.org/sqlite`).
- **It conflicts with the artifact store's guarantees.** Artifacts are
  read-only files whose SHA-256 is checked on every read (book rule 3). Rows
  in a database can be updated in place. Either tamper evidence is rebuilt
  for rows, or the data is stored twice.
- **Inspectability.** The state directory is plain files that `cat`, `grep`,
  `aos inspect` and analysis scripts read directly. The sandbox protects it
  by path. A database file needs its own tooling and its own protection.
- **It does not remove the design work.** Keys, versions and precedence are
  needed in SQLite too.

### When to revisit

- **SQLite:** if `index.json`, which is rewritten whole under a file lock,
  becomes a measured bottleneck with many processes sharing a state
  directory, or if queries across processes or runs are needed. Then it
  should replace the artifact index as a whole, with its integrity
  guarantees carried over, not be added beside it for memory alone.
- **SQLite FTS5 for `search_memory` alone (decided 2026-10-29).** Phase 2
  changed matching twice: substring to all-words with `|` alternatives,
  then newest-first to ranking by alternatives matched. Both
  reimplemented parts of FTS. The search-engine part is only about 100
  lines of `retrieval.go`; numbering, ownership, one-hop references,
  legacy numbering, the index and range reads are needed with SQLite too.
  The failures that remain are not search quality: TriData stopped at an
  inconclusive A/B, and the leaf2 errors were about which claim is
  current, one with the fact in context. So the scan stays, under a rule:
  1. If `search_memory` needs a third change to matching, tokenization or
     ranking, stop hand-tuning and move to SQLite FTS5 (BM25) behind the
     same `search_memory` interface.
  2. SQLite is then a derived index only. Memory segments stay artifacts
     and remain the only record. The index is rebuilt from them and can be
     deleted and rebuilt at any time: context is derived, applied to the
     index. This removes the rule-3 tamper-evidence objection above.
  3. Use a pure-Go driver with FTS5 (`modernc.org/sqlite`): no cgo, so
     cross-compiling and the Linux sandbox's re-exec helper are
     unaffected. The index file sits in the state directory under the same
     path protection.
  4. The cost that remains is the dependency. Past that threshold,
     AGENTS.md's "clearly justified" is met.
- **Vector search:** if replay or live runs show agents failing to find
  facts they need by meaning, such as "have I tried a similar memory-layout
  change before", where exact search returned nothing. Then it is an
  optional second backend behind `search_memory`. Exact matching and
  precedence stay, and the runtime does not depend on it.

`search_memory`'s interface and precedence rule are what this design fixes.
Either backend can replace the scan later without changing what agents see.

## Contracts and tests

| Contract | Test |
| --- | --- |
| Configuration is refused when an output cap is not well under H − L | `TestCompactionRejectsCapsOverHysteresis` |
| Live history after compaction is ≤ L; the next compaction needs ≥ H − L new characters | `TestCompactionHasHysteresis` |
| A cut never separates a tool call from its result | `TestCompactionCutsCleanly` |
| An oversized call/result pair compacts and records `memory.compacted` with `oversized=true` | `TestOversizedTailIsReported` |
| Sequence numbers are unique and stable across compaction, Resume and Recover | `TestMessageSeqSurvivesRecovery` |
| Checkpoints and segments from before numbering resume and read as before | `TestUnnumberedCheckpointsStillResume` |
| Each original message is stored in exactly one segment; read and search results are archived as one-hop references | `TestArchiveStoresEachMessageOnce` |
| `read_memory` of an archived reference returns the reference, not its target | `TestReadMemoryDoesNotFollowReferences` |
| `search_memory` finds a fact from any archived segment, newest first under the precedence line | `TestSearchMemoryFindsOwnArchive` |
| `search_memory` reads only the caller's own archive | `TestSearchMemoryFindsOwnArchiveOnly` |
| A search sees a copied read in an old segment as a reference, so a fact is found once | `TestSearchSkipsCopiesInOldSegments` |
| Search results state precedence and mark superseded working-memory entries | `TestSearchMarksSupersededEntries` |
| `read_memory` by range returns whole messages under the cap, with `next_from` | `TestReadMemoryByRange` |
| Unnumbered segments read by range under the numbers their checkpoint implies, and old refs are indexed before their first prompt | `TestReadMemoryByRangeReadsUnnumberedSegments` |
| Search and range results are archived as one-hop references | `TestRetrievalResultsArchiveAsReferences` |
| A segment's index entry records its numbers, tools and file-tool paths, and no summary | `TestIndexRecordsToolsAndPaths` |
| The header indexes the archive and says what the index leaves out | `TestTaskHeaderIndexesTheArchive` |
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
   - peak live characters (must stay ≤ H unless a compaction was `oversized`);
   - `search_memory`, `read_memory` and `update_memory` calls;
   - superseded facts cited as current after a compaction (one in
     `pid-000009`: leaf2 re-adopted at 04:07);
   - archived results forgotten and experiments repeated, counted separately
     (one in `pid-000009`: leaf2b re-asked the question `art-001056` had
     answered);
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

### Phase 1 as built

- `Message.Seq` and `Session.NextSeq`. Messages are numbered before every
  compaction and in every checkpoint. A checkpoint from before numbering
  continues after its archived messages, and its segments still read with
  `artifact_id`/`offset`.
- `CompactionPolicy.TargetChars`, default `MaxChars / 2`. `KeepMessages` is
  now an upper bound, and a windowed policy sets none. Cuts fall only at the
  start of a turn: a tool result stays with its action, and an action record
  stays with the message that followed it.
- `CompactionPolicy.Validate(maxResult)` runs against
  `tools.MaxToolResultBytes` (68 KB), the bound on a whole recorded tool
  result. `run`'s stdout and stderr share the 64 KB output cap; each keeps
  half, and a quiet stream leaves its share to the other. The echoed command
  is capped at 2 KB, since the full command is in the action. The runtime
  truncates any result that would still be over the bound. `aos run` and
  `aos-tui` refuse a windowed policy whose gap `H − L` one such result can
  fill. The default policy, for an unknown window, is exempt; it was already
  bounded mostly by message count.
- What hysteresis guarantees, exactly. After a compaction, live history is
  at most the larger of `L` and the latest turn. So compaction on two
  consecutive steps needs a turn larger than `H − L`. Tool results cannot be
  that large. An action can be, for example a long `write_file`, because no
  configuration bounds an action's size: `max_tokens` counts thinking too,
  and validating against it would refuse every practical window. Such a
  turn may compact on the next step. That compaction still archives at
  least one whole turn and is reported as `oversized`.
  `TestHysteresisHoldsForTheLargestTurn` covers both cases.
- Compaction is idempotent across a crash. The segment is written before
  the checkpoint that drops its messages, so a crash in between replays the
  compaction. A segment records `from_seq` and `to_seq`. Suppose this
  process, or one it was recovered from, already cut a segment from the same
  checkpoint starting at the same message, and no later checkpoint followed
  it. Then the replay adopts that segment and cuts where it did
  (`memory.compacted` with `adopted=true`), but only if the replay's
  messages in that range are exactly the ones the segment holds. The check
  compares the SHA-256 recorded in the segment's metadata. Turns appended
  after the checkpoint, such as inbox messages or a Recover's appended
  line, can be in the archived range and can differ on replay while getting
  the same numbers. A mismatch cuts fresh and records `not_adopted`
  (`TestAdoptionRequiresIdenticalMessages`). A segment that a checkpoint
  followed belongs to a run that went on, so a second resume from the same
  checkpoint is a fork and cuts its own segment
  (`TestTwoResumesFromOneCheckpointKeepSeparateSegments` still holds). A
  fresh run's first compaction has no parent checkpoint and cannot be
  replayed.
  `TestCompactionIsIdempotentAcrossRecover` covers it.
- Each compaction is a `memory.compacted` kernel event. It records the
  segment, the sequence range, the live messages and characters, and
  `oversized`.
- A `read_memory` result is archived as `read_memory result (art-X, bytes
  a-b of n): not copied into this segment; read_memory artifact_id=art-X
  offset=a reads it again`. It is byte-based until phase 2 adds range reads.
  A legacy read, from a checkpoint before this change, is reduced to the
  segment it named.
- The backend's history compaction and its summary fields are removed.
  Windowed runs never had them filled, because the runtime compacted first,
  so their prompts are unchanged.
- `aos probe-memory` and `internal/memprobe` form the replay harness. Probes
  run on a copy of the kernel and artifact index. Payloads are read in place,
  so run the harness from the directory the run started in. The probe
  process has no tools but `read_memory`, and answers are scored against
  `expect`/`reject` patterns, several samples per probe. The probes for the
  run above are in `bench/memory/pathtracer-bunny-memfix-probes.json`.
- Baseline, 3 samples per probe at checkpoint `art-001084` (report:
  `reports/memory-phase-1.html`):
  - archive-only TriData: 3/3, with one read each;
  - live controls: 3/3, with no reads;
  - **why leaf2 was dropped: verdict 3/3, reason 0/3.** Every answer said
    leaf2 is not a real speedup, which is right, and none gave the reason.
    Both samples that read memory picked the old nested segment
    `art-001074` and got a copy. The bias finding is past the first page of
    `art-001056`. Every answer gave a confident, plausible substitute ("did
    not reproduce", "one noisy session", "that is not bias").

  This is the number phases 2 and 3 must move. Probes carry a `topic`
  pattern, and `--rescore` re-applies tightened patterns to recorded
  answers.

### Phase 2 as built

- `search_memory query limit?` (`internal/runtime/retrieval.go`). A
  message matches when it holds every word of the query, in any order, and
  `|` separates alternatives. `re:` takes a Go regexp. Both are
  case-insensitive. The design said substring. The first probe run showed
  why that fails: the model wrote `tridata order|leaf emission order`,
  meaning alternatives of loose words, and a literal match found nothing
  in an archive that says "TriData in tree/leaf emission order". Hits
  that match more alternatives come first, and the newest comes first
  among equals. The second run showed why ranking is needed. Broad
  alternatives ("tri order") matched 49 messages, and the 10 newest crowded
  out the older one that held the measurement and matched the whole query.
  Each hit keeps its message number, so ranking does not hide which is
  later.
  A hit is one message, with about 150 characters on each
  side of the first match and a count when a message matches more than
  once. A hit reads `#412 assistant (art-001056, archived 2026-10-29
  04:05): …`. Messages carry no time of their own, so the time is when
  their segment was archived. The result opens with a line that states
  only what the runtime knows: "Archive hits are history, ordered by how
  much of the query they match, then newest first. A higher # is later,
  not necessarily right: a later message can repeat or explain away a
  mistake. Check what a claim rests on; your live history is newer than
  any hit." The first wording said a later message supersedes an earlier
  one. Review found three counterexamples in the memfix run: the
  round-8 coordinator's wrong claim about leaf2's node layout, the
  round-3 "CONFIRMED HUGE WIN" written after the bias finding, and the
  writer log's invented explanation. All three are later than the facts
  they contradict. Working memory joins the line in phase 3. Output is capped at 8 KB and says how many
  matching messages it left out.
- `read_memory from to?` returns `#N role: content` lines. They are whole
  messages up to 32 KB, with `next_from` when the range goes on. The
  messages text then ends with `(stopped at 32 KB; #113-#115 not shown:
  read_memory from=113 to=115)`, since `next_from` itself prints after
  32 KB of text. A first
  message larger than the cap is cut, and the result names the
  `artifact_id` and byte `offset` where the rest continues. A range outside
  the archive is an error that says which numbers the archive holds.
  `artifact_id`/`offset` keeps working.
- Both read only the segments in the session's own refs, each checked with
  `ownedBy`, and both are always allowed. Each result is archived as a
  one-hop reference: `search_memory result (N matching messages): not
  copied into this segment; search_memory query="…" runs it again`, and
  `read_memory result (messages a-b): … read_memory from=a to=b reads it
  again`.
- Old segments. A segment from before numbering is numbered the way its
  checkpoint numbers live messages: counting from the start of the
  process's history. A `read_memory` result that an old segment copied
  whole is read back as the reference it would be archived as today. This
  holds for search, range reads and `artifact_id` reads alike, so the
  `art-001074` copy that misled the baseline is no longer a second source.
- The header index replaces summaries. A `MemoryRef` carries `from_seq`,
  `to_seq`, `archived`, tool counts and up to six paths passed to
  `read_file`, `write_file` and `edit_file`, all recorded at compaction.
  Refs written before the index get theirs on their next prompt, read once
  and then saved with the session. The prompt shows one archive line, how
  to search and read, the index's caveats (not a summary; files that `run`
  touched are not listed), and one line per segment with paths shortened
  under their common directory. Segments no longer store a summary, and
  `memory_refs` is gone from the header JSON.
- The probe harness also counts `search_memory` calls and the bytes they
  return.
- Probes at checkpoint `art-001084`, scored with the same final patterns
  (report: `reports/memory-phase-2.html`):
  - **why leaf2 was dropped: 0/9 → 8/9**, with one search and about 4 KB
    read, against 32–43 KB before;
  - archive-only TriData: 8/9 → 6/9, no detectable change at n=9
    (Fisher exact p = 0.58; the leaf2 change has p = 0.0004). The misses
    show a mechanism, though: agents read about ten messages around a hit
    and two asked for ranges ending at message 112, just before the
    decisive A/B at 113–115; none was cut short by the size cap. Search
    finds where a topic is, not where it was decided. A keyed
    working-memory entry written at the decision (phase 3) is the
    intended fix;
  - a trade-off: the threadgroup question, once answered from the prose
    summary in one step, now takes about four steps and 19 KB, and stays
    3/3. Removing summaries follows the imitation lesson and the
    no-fabrication rule; phase 3 should measure whether working memory
    recovers the cost;
  - live controls stay 3/3, and the w4 control makes no memory calls;
  - after review, the order-only precedence wording: leaf2-speedup 7/9
    and leaf2-status 9/9, no detectable change from 8/9 and 3/3.

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
