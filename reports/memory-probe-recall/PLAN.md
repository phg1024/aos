# Agent-written archive index (A1) evaluation plan (frozen)

Requested by aos book on 2026-10-30, after aos code's recall aids landed
(`d65b94f`). Designed and run by the evaluation session. This plan, the
scripts in this folder and the harness are committed before any model
call is made, and nothing in them changes after.

## Question

In the own-memory evaluation (`reports/memory-probe-own-memory`), facts
that were archived but not in the agent's working memory were not
recovered: `r2-verify-dir` and `r5-register-counts` scored 0/9 in both
arms (hand-read main score, 0/18 per arm). The agent did not search for
them. A1 asks the agent, at each handoff, to write an index of what it
archives, which the header then shows next to the segment. Does that
index make unrecorded facts recoverable, and at what cost?

## Harness

Runtime `d65b94f` (aos code). memprobe on this branch:

- `aad14f4`: `--recall` names the aids that are on; every aid not named
  is off (`runtime.RecallOff()`), on both the runtime and the prompt
  backend. Retry is off unless `--backend-retry` is given (it is not, in
  any run here). `--handoff` runs `RunOptions.HandoffOnly` on
  checkpoints, in a copy.
- `3fa2cd0`: test that a failed model call takes a step, as before.
- the commit that freezes this plan: `--agent-indexes` puts given
  indexes on a probe checkpoint's segment references; samples record
  tokens and each memory read with its arguments; handoffs get a fresh
  step allowance.

The binary is built by this session from the freezing commit, as
`.cache/bin/aos-eval-<commit>`, and its hash goes into `run.log`.

## Model and settings

As in the own-memory evaluation: `qwen3.8-flash-next-nvfp4-nvfp4` on
phg-gpu (vLLM), context 65536, compaction ratio 0.8, temperature 0.6,
top_p 0.95, max tokens 32768, thinking budget 16384, 12 steps per
answer. Only this evaluation runs on phg-gpu while it runs.

## Step 1: indexes, written by the agent at each archived segment's handoff

The three probe checkpoints of the own-memory evaluation hold nine
archived segments. For each segment, the handoff is re-run from the last
checkpoint before the run's original handoff notice for it, with a1 on:
the agent gets the handoff notice (now asking for an index of the
messages to be archived) at the same point in its history as in the run.

| probe checkpoint | segment | range | re-run from | dry-run range |
| --- | --- | --- | --- | --- |
| art-000648 (r2, pid-000006) | art-000414 | 1-57 | art-000410 | 1-59 |
| | art-000507 | 58-105 | art-000503 | 58-105 |
| | art-000557 | 106-194 | art-000553 | 106-194 |
| | art-000646 | 195-249 | art-000642 | 195-247 |
| art-001768 (r5, pid-000015) | art-001608 | 1-29 | art-001605 | 1-29 |
| | art-001661 | 30-120 | art-001657 | 30-120 |
| | art-001766 | 121-167 | art-001762 | 121-167 |
| art-003135 (r8, pid-000025) | art-003062 | 1-17 | art-003059 | 1-17 |
| | art-003133 | 18-72 | art-003129 | 18-72 |

"Dry-run range" is what the handoff archived with a scripted model
(locally, no phg-gpu): 7 of 9 match the run's segment exactly.

Three draws per segment (`--samples 3`), so 27 handoffs. A failed
handoff (a backend error or no segment archived) is re-run once and both
attempts are reported; if the re-run also fails, that draw has no index
for that segment.

Only the index is kept. The working-memory updates the agent makes in a
handoff are discarded: the probes run on the probe checkpoint's own
working memory, so the arms differ only in the index.

`make_indexes.py` turns the handoffs into one index file per draw,
segment ID to lines. A line is kept for a segment only if it lies wholly
in that segment's range; any other line is dropped and counted. The
indexes are committed with the results.

Known differences from a live run with a1 on: the handoff process has
only the memory tools; the earlier segments it sees have no index; the
index is on the probe checkpoint's segment references, so the header
shows it but `read_memory` of a segment does not repeat it.

## Step 2: probes

The 11 frozen own-memory probes, unchanged
(`bench/memory/pathtracer-bunny-phase3-own-memory-probes.json`, sha256
prefix `e5e11d8560f5`), on their own checkpoints, own working memory,
check rule on. Two arms, 9 samples per probe each:

- **C** (control): no flags, so `RecallOff()`: the prompts from before
  the aids.
- **A1**: `--recall a1 --agent-indexes indexes-dN.json`, 3 samples per
  draw N = 1, 2, 3.

Run C first, then A1 draws 1 to 3, by `run.sh`. 198 samples.

## Scoring

Every answer is graded by hand with the own-memory rubric (`RUBRIC` in
`../memory-probe-own-memory/handread.py`: main, exact, contradicted),
reported next to the frozen regex score. Grading is not blind: an A1
answer can show that it used the index.

## Comparisons

Primary: main score on `r2-verify-dir` + `r5-register-counts`, A1
against C, 18 answers each, two-sided Fisher exact.

Secondary:

- each unrecorded probe, main and exact (`r8-sibling-pairs` is 9/9 main
  in both earlier arms, so its exact score is the one that can move);
- recorded and recorded-wrong probes, main / exact / contradicted, as a
  regression check;
- per probe and draw: does the index of the segment that holds the
  truth have a line that points at the truth messages and says what is
  there? This separates "the agent did not index it" from "it indexed it
  but the probed agent did not use it".

## Harm check

- Steps, tokens and bytes read per answer, A1 against C, per kind; and
  the header bytes the indexes add (`agent_index_bytes`).
- Answers that trust an index line without reading what it cites: an A1
  answer that states something an index line says, which its working
  memory and live history do not hold, with no `read_memory` covering
  that line's messages (from the sample's `reads`). Each is counted, and
  marked right or wrong against the archive.
- Any A1 answer that is wrong because of an index line.

## Index quality

For each line drawn, read the messages it cites and grade it: **holds**
(the cited messages hold what the line says), **partly** (the right
place, but a number or detail is wrong or missing) or **wrong** (the cited
messages do not hold it). Lines are:

- 40 drawn uniformly from all kept lines of all draws, with
  `random.Random(20261030)` over the lines sorted by (draw, segment,
  from, to, text);
- plus every line, in any draw, that cites a truth message of an
  unrecorded probe.

Also reported: lines per index, bytes per index, lines dropped by the
range rule, and how much of each segment the lines cover.

## Files

`run.sh`, `make_indexes.py`, `handoffs.jsonl`, `indexes-d1..3.json`,
`samples-C.jsonl`, `samples-A1-d1..3.jsonl`, `run.log`, then the
grading and summary scripts and the HTML report
`reports/memory-probe-recall.html`.
