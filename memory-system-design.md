# Memory System Design

Status: proposal, 2026-10-29. Nothing here is implemented yet except where a
section says so.

## Why

AOS compacts an agent's history into runtime-owned `memory_segment` artifacts
and lets the agent read them back with `read_memory`. Commit `eceb917` made
that tool visible in the prompt and paged its output. The live path-tracer run
on that binary (`reports/pathtracer-bunny-memfix`, 65,536-token window,
compaction at about 157,000 characters) is the first run where agents used it.
As of round 4 of 10:

- 13 compactions, all in writers: 2, 3, 7 and 1 per writer process.
- 3 `read_memory` calls, all from the round-3 writer (`pid-000009`), all for
  the right reason. Before retrying an idea, it looked up what that idea had
  measured. Later it checked a contradiction between its notes and an older
  segment: the segment said "leaf2 adopted, 33.5 ms", the current notes said
  leaf2 was not adopted.
- The mechanism worked against it:
  - **Thrash.** Compaction keeps the 12 most recent messages whatever their
    size. With 64 KB tool results among them, the kept tail alone can exceed
    the trigger, so compaction fires again a step or two later. Segments of
    11 and 4 messages followed each other within four minutes. The writer's
    live history peaked at 186,200 characters, above the 157,000 trigger.
  - **Nesting.** A `read_memory` result is an ordinary tool message, so the
    next compaction copies it into a new segment. `art-001074` holds 4
    messages and 132 KB, almost all of it copies of `art-000969`. The writer
    read it because the ref said "4 messages", got old content again, and
    never resolved the contradiction.
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

1. Context is derived. It is assembled from storage before each call and is
   authoritative for nothing.
2. Compaction moves context into storage. Old history goes into an artifact
   the agent can read back, not into the void.
3. A model reproduces the shape of its own past turns. Never put
   placeholders, summaries, or truncations where the model will read them as
   examples of its own actions.

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
  and measurements. It survives compaction and recovery.
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
| Working memory | Current decisions, measurements, rejected ideas, open questions, next steps; size-capped | Agent, through `update_memory`; stored by the runtime | Snapshot in the header, refreshed at compaction; updates in live history between |
| Live history | Recent messages, under a character budget | Runtime | Yes, verbatim |
| Archive | Every compacted message, verbatim, numbered | Runtime | No; found with `search_memory`, read with `read_memory` |

The checkpoint holds the live history, the working memory's artifact ID, and
the archive index. The archive and every working-memory version are
artifacts.

## Mechanisms

### Message numbering

Every message in a session gets a sequence number when it is appended
(`Message.Seq`; `Session.NextSeq`). Numbers never change and are never
reused, including across Resume and Recover. Segments hold contiguous ranges.
The archive is then addressable as "messages 412–470" regardless of how it
was cut, and every tool below speaks in sequence numbers.

### Compaction: budget, hysteresis, clean cuts

- Trigger at a high watermark `H` (as now: window × ratio × 3 characters).
- When it fires, archive the oldest messages until the live history is at or
  below a low watermark `L`, default `H / 2`. The live tail is chosen by
  characters, not by count.
- Never cut between a tool call and its result, and always keep the latest
  call/result pair live.
- If that pair alone exceeds `L`, compact to it anyway and record a
  `compaction.oversized` trace event. That is a sign the tool output cap is
  too large for the window. It is reported, not hidden.

With `L = H / 2`, the next compaction is at least `H / 2` characters of new
history away. Thrash becomes impossible by construction rather than by
tuning.

### Pre-compaction handoff

When `H` is crossed, the runtime does not compact at once. It appends one
tool-role message:

> History through message N will move to the archive after this step. Your
> working memory is what you will still see. Update it now with anything from
> messages up to N that you will need: results, decisions, and what you
> rejected and why.

The agent gets that one step, typically an `update_memory` call. The runtime
compacts before the next call whatever the agent did, so a model that ignores
the note costs one step and nothing else. This is the "structured handoff" of
the guiding principles, done by the only party that knows what matters.

The message is a tool result, not a fabricated assistant turn, so it gives
the model nothing to imitate as its own action.

### Working memory

- The agent edits it with
  `update_memory section="decisions|measurements|rejected|open|next" content=...`.
  Each call replaces one named section. Replacing, not appending, is what
  stops two "current" claims from coexisting.
- Each update is a new `working_memory` artifact. Its parent is the previous
  version, so its history is auditable, and the session holds the latest
  version's ID.
- A hard cap, default 8 KB total. An update over the cap fails with an error
  saying how large the memory is. It is never truncated, so no truncated text
  is ever shown to the model as its own.
- Prompt placement keeps the append-only layout. Between compactions, the
  `update_memory` calls are already in the live history verbatim. At each
  compaction, the header, which only changes at compaction today, is
  rebuilt with the current snapshot. The prefix changes exactly as often as
  it does now.
- Resume and Recover carry it over, like the rest of the session.
- `notes` stays. It is step-to-step scratch; working memory is what survives.

### Archive without nesting

A segment stores the original messages of its range. When a `read_memory` or
`search_memory` result is archived, its content is not copied. The segment
stores a reference instead: "read_memory result: messages 412–470 of the
archive". The original text is stored once, where it first appeared.

This puts a reference where the full text was, which rule 3 warns about. The
risk is lower here, for two reasons. The model never authors tool results,
so it has no reason to copy their shape into its own actions. And this text
appears only in the archive, never in live history.
`TestArchiveStoresEachMessageOnce` and the replay evaluation below check both
points.

### Retrieval

- `search_memory query=... limit?` returns up to `limit` (default 10) hits
  across the process's own archive. A hit is a case-insensitive substring or
  `re:`-prefixed regexp match. Each hit gives the sequence number, role, and a
  window of about 300 characters around the match, newest first. Output is
  capped at 8 KB.
- `read_memory from=... to=...` returns messages by sequence number, capped at
  32 KB. If a range is too large, it returns whole messages up to the cap and
  `next_from`. `artifact_id`/`offset` keep working for old checkpoints.
- Both are always allowed, like `read_memory` today, and only ever read the
  calling process's own archive (`ownedBy`).

### Context assembly has one owner

- The header shows the working-memory snapshot and one index line instead of
  a ref per segment, for example: "Archive: messages 1–905 in 13 segments
  (search_memory to find, read_memory to read)". The index also lists, per
  segment, factual metadata the runtime knows exactly: the sequence range,
  the time span, the tools used, and the files written or read. It has no
  prose summary. Files and tools are what agents search for, and the runtime
  can state them without summarising.
- When a runtime compaction policy is set, the backend's own history bound
  (`compactHistory` in `internal/agentflow/backend.go`) is disabled. The
  prompt's messages are then exactly the session's live history.
  `TestPromptHistoryIsTheSession` asserts it.

### Tool results in live history (experiment)

Large tool outputs, such as file reads and build logs up to 64 KB, are what
fill the window. One option is to store any output over a threshold as an
artifact and keep a reference plus its head in live history. Chapter 3's
failures came from placeholders in the model's own actions, not in tool
results, but this has not been tested. It is phase 4, behind a flag, and it
is adopted only if the replay evaluation shows no imitation and no loss.

## Decisions

| # | Question | Options | Status |
| --- | --- | --- | --- |
| D1 | Who writes what survives compaction | (a) runtime mechanically; (b) a separate model call; (c) the agent, through a pre-compaction handoff | **Decided 2026-10-29: (c).** It is the cheapest, it keeps the runtime free of inference, and the agent is the only party that knows what mattered. The runtime contributes only exact facts (the index). |
| D2 | Where working memory lives | (a) a runtime-owned artifact; (b) a file in the agent's workspace, as writer ledgers are today | **Decided 2026-10-29: (a).** It is auditable, versioned, carried over on recovery, and cannot be overwritten by `run` or `write_file`. Workspace ledgers stay useful for handing off between agents. |
| D3 | References in place of large tool results | adopt; reject; experiment first | Open. Recommendation: experiment first (phase 4). The nesting fix uses references only in the archive, which does not depend on this decision. |

## Contracts and tests

| Contract | Test |
| --- | --- |
| Live history after compaction is ≤ L; the next compaction needs ≥ H − L new characters | `TestCompactionHasHysteresis` |
| A cut never separates a tool call from its result | `TestCompactionCutsCleanly` |
| An oversized tail compacts and records `compaction.oversized` | `TestOversizedTailIsReported` |
| Sequence numbers are unique and stable across compaction, Resume and Recover | `TestMessageSeqSurvivesRecovery` |
| Each original message is stored in exactly one segment; read and search results are archived as references | `TestArchiveStoresEachMessageOnce` |
| `search_memory` finds a fact from any archived segment and only the caller's own | `TestSearchMemoryFindsOwnArchiveOnly` |
| `read_memory` by range returns whole messages under the cap, with `next_from` | `TestReadMemoryByRange` |
| A handoff note precedes every compaction, and compaction happens on the next step regardless | `TestCompactionGivesOneHandoffStep` |
| `update_memory` replaces a section, versions the artifact, rejects over-cap updates and survives Recover | `TestWorkingMemoryVersionsAndCaps` |
| The header changes only at compaction; between compactions, prompts are append-only | `TestPromptPrefixStableBetweenCompactions` |
| With a runtime policy, the prompt's history equals the session's live history | `TestPromptHistoryIsTheSession` |

## Evaluation

1. **Unit tests**, as above, for every phase.
2. **Offline replay.** From checkpoints of `reports/pathtracer-bunny-memfix`,
   resume a writer with one appended question whose answer exists only in
   its archive, then score the answer against the segment text. Measure
   correctness, steps, and bytes read. Seed questions from this run:
   - Why was leaf2 (2 triangles per leaf) dropped after measuring 33.5 ms?
   - What did TriData emission order measure?
   - Which threadgroup sizes were swept, and what won?

   Compare the current memory system with each phase.
3. **Live rerun** of `pathtracer-bunny-metal` on the new binary. Measure:
   - compactions per process and the smallest segment size (no thrash);
   - peak live characters (must stay ≤ H unless `compaction.oversized`);
   - `search_memory`, `read_memory` and `update_memory` calls;
   - repeated experiments, found as benchmark commands that recur after a
     compaction with no code change in between;
   - the final speedup and verifier verdicts, to confirm nothing regresses.

## Phases

Each phase ends with tests, a commit and an HTML report in `reports/`.

1. **Mechanics.** No prompt changes yet: sequence numbers, budget-based
   compaction with hysteresis, clean cuts, no nesting, one owner of context.
2. **Retrieval.** `search_memory`, `read_memory` by range, and the header
   index in place of per-segment summaries.
3. **Working memory.** `update_memory`, the `working_memory` artifact, and
   the pre-compaction handoff step.
4. **Experiment.** References for large tool results behind a flag, judged
   by the replay evaluation.

The replay harness is built in phase 1, so every later phase is measured
against the current system.

## Book impact

Chapter 3's Compaction section describes the current mechanism and marks it
Implemented. After phase 3 it needs rewriting around the four layers. The
"two places decide what the model sees" limit goes away in phase 1. The
nesting and thrash observed in this run are a natural second failure story
for the chapter, after the placeholder story that opens it. The archive is
verbatim, yet a copy of a copy still misleads the reader.

## Open questions

- Should a new writer in the next round start from the previous writer's
  working memory, passed as an artifact handle by the coordinator, instead of
  only from the workspace ledger? It would be cross-process memory, so it
  belongs with Chapter 8's handles, but it is the most likely way to stop
  the round-4 writer re-reading what round 3 learned.
- Is 8 KB of working memory right for a 65,536-token window? Should the cap
  scale with the window?
- Should `search_memory` also search the agent's working-memory versions, so
  that an overwritten decision can be found?
