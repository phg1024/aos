# A1 re-test with fair-share header rendering (DRAFT, not frozen)

Requested by aos book on 2026-10-30, after the A1 follow-up
(`../memory-probe-recall-fix/`, report `../memory-probe-recall-fix.html`)
found that the header showed agent indexes newest first within 6 KB, so
the oldest round-2 segment's index (art-000414, which holds the
`r2-verify-dir` truth) became a bare pointer in every draw. The user
approved fixing that and re-testing. aos code is changing the header to a
fair-share allocation by whole lines, recording per segment how many lines
were shown and hidden.

This plan, the new probes and the scripts are committed before any model
call; nothing in them changes after. It is frozen on aos code's header
commit: COMMIT (filled in at freeze).

## Questions

1. Primary: with every index visible, does A1 recover archived facts the
   agent did not record better than C, on a set of new unrecorded-fact
   probes chosen without looking at any index?
2. What share of those facts do the agent's indexes cover at all (index
   recall), separately from whether the probed agent uses them?
3. Harm: does A1 make answers on recorded and recorded-wrong facts worse
   (the `r5-tir-guard` pattern), and how often does an answer trust an
   index line without reading what it cites, and with what result?

## What is reused

- **The 27 index draws** of the follow-up (`b87b59a`,
  `../memory-probe-recall-fix/indexes-d1..3.json`), written by handoffs on
  runtime `50a7cfe`. Only the header rendering changes, so the handoffs
  are not re-run (about 5.3M tokens saved). Gate G1 checks this in the
  code before any model call.
- **The 11 frozen probes** (`e5e11d8560f5`): the 3 unrecorded ones as a
  continuity check, the 8 recorded and recorded-wrong ones as the
  regression check.
- Model and settings as in the follow-up (`qwen3.8-flash-next-nvfp4-nvfp4`
  on phg-gpu, context 65536, compaction 0.8, temperature 0.6, top_p 0.95,
  thinking budget 16384, 12 steps per answer); other recall aids off in
  both arms; nothing else on phg-gpu while it runs.

## New probes

Chosen by a rule written before any index was opened for this purpose
(`select_candidates.py`, then the writer's rule below), so that no fact is
picked because an index mentions it.

- **Candidates** (`select_candidates.py`, mechanical): per archived
  segment, the tool results of reporting tools (run, read_file, list_dir,
  search) of at least 80 characters that hold a decimal number or an
  outcome word, excluding messages within 2 of any message a frozen
  probe's note cites; shuffled with `random.Random("a1-facts-" + segment)`.
- **Writer:** a fresh agent with no access to any index file (it was told
  not to open the two earlier A1 folders, any `indexes-*`, `handoffs*`,
  `index_lines*`, `answers*` or `grades*` file, or a segment's `index`
  field) takes each segment's candidates strictly in order. For each it
  picks the one most decision-relevant number or outcome the message
  states and accepts it only if (a) it is absent from the probe
  checkpoint's working memory and live history (`context/*.txt`, grepped
  and read), (b) it is not a frozen probe's topic, (c) it differs from
  facts already accepted for the segment, and (d) a question can be asked
  that does not give it away. Up to 3 per segment; every candidate
  considered is logged with its reason (`selection.md`).
- **Second pass (rule amended after the first pass, still without any
  index):** the first pass gave 11 probes, because candidates ran out
  before 3 were accepted in every segment (about 40% are source or config
  reads, and the working memories hold most benchmark numbers). The first
  pass was committed (`86f8f05`), then the writer revisited, for each
  segment short of 3, the candidates in the same order and could accept a
  secondary measured fact per message (one value of a sweep, a node count,
  one check among several; never a source or config constant), under the
  same checks (a)-(d). This gave 12 more: **23 probes** (art-000414 3,
  art-000507 2, art-000557 3, art-000646 3, art-001608 3, art-001661 3,
  art-001766 3, art-003062 2, art-003133 1; 11 on art-000648, 9 on
  art-001768, 3 on art-003135). All 23 are kept as the rule produced them,
  including minor values (setup times, one run's average) an operator is
  unlikely to ask about. First-pass against second-pass probes is a
  pre-declared breakdown, not a filter.
- **Disclosure:** this session (not the writer) had already seen 45 index
  lines and a few greps of the follow-up's indexes while grading it. It
  did not write, choose or edit the new probes beyond fixing format.

Output: `probes-new.json`, `rubric-new.json`, `selection.md`.

## Index recall (before any model call)

For each new probe and draw, from the reused indexes:

- **range-covered:** some line's message range includes the truth
  message (mechanical);
- **fact-covered:** some such line states the fact (graded by hand:
  states it / points at it without the value / no);
- **shown:** with the new header, the covering line is among the lines
  the header shows (from the header commit's renderer).

Reported per probe, per segment and overall. Not used to choose probes.

## Arms and samples

- **C:** `RecallOff()`, no indexes.
- **A1:** `--recall a1 --agent-indexes indexes-dN.json`, N = 1, 2, 3.
- New probes: 9 answers per probe per arm (A1: 3 per draw).
- Frozen 11 probes: 9 per arm (A1: 3 per draw), as before.
- Order: C (new, then frozen), then A1 draws 1 to 3 (new, then frozen).

**Why 9 answers per new probe.** A simulation of the primary test (below)
with 23 probes, a C rate per probe drawn from Beta(0.5, 4.5) (mean 0.1,
most near 0, as seen for unrecorded facts), and A1 correct with
probability max(C rate, q) on facts its index covers (300 trials, 300
permutations each):

| answers per arm | covered share | q | power |
| --- | --- | --- | --- |
| 6 | 0.15 | 0.6 | 0.40 |
| 6 | 0.25 | 0.5 | 0.57 |
| 9 | 0.15 | 0.6 | 0.56 |
| 9 | 0.25 | 0.5 | 0.70 |
| 9 | 0.25 | 0.7 | 0.90 |
| 9 | 0.50 | 0.5 | 0.99 |

The follow-up saw q about 0.67 on the one covered unrecorded probe (4 of
6). Covered share is unknown and may be low, since many of the new facts
are minor values; 9 answers keep about 0.7 power at a quarter covered.
Cost: 23 x 18 + 11 x 18 = 612 answers, about 5 h and 28M tokens on
phg-gpu.

## Scoring

Every answer is graded by hand with main / exact / contra (the own-memory
rubric; `rubric-new.json` for new probes). Grading is blind to arm:
answers are shuffled with a fixed seed and stripped of arm, draw and
index-related fields before grading; the frozen regex score is reported
next to it.

## Primary test

Main score on the new probes, A1 against C. Unit: the probe (answers about
one fact are not independent). Statistic: total A1 main minus total C main
over all new probes. Two-sided stratified permutation test: within each
probe, the 18 answers' arm labels are permuted (9 and 9), 100,000 times
with `random.Random(20261031)`; p = share of permuted |statistic| at least
the observed. Alpha 0.05. Reported with the per-probe differences and the
pooled rates.

## Secondary

- The same test restricted to probes whose fact is covered by a shown line
  in at least one draw, and to the rest (descriptive: the split is fixed
  before model calls, from the index-recall table).
- Continuity: the 3 frozen unrecorded probes, main / exact / contra per arm,
  next to the follow-up's numbers.
- First-pass (11) against second-pass (12) probes.
- Per-draw A1 scores.

## Harm check

- **Regression on the 8 recorded and recorded-wrong probes:** main / exact /
  contra and repeats-wrong-entry per arm; pooled main and pooled contra,
  A1 against C, two-sided Fisher (72 answers each). Any single probe where
  A1 main is at least 3 below C main (of 9) is flagged and read for
  whether an index line contributed.
- **Answers that use an index line without reading what it cites:** an A1
  answer that states a fact found in a shown index line of its draw but not
  in its working memory or live history, with no memory read covering the
  line's messages. Counted, split by right and wrong (and contra), for new
  and frozen probes.
- **Answers wrong because of an index line:** each such answer, with the
  line.
- Steps, tokens and bytes read per answer by kind and arm; header bytes the
  indexes add.

## Gates and stop rule (all fixed now)

Before any model call:

- **G1, write path unchanged:** between `50a7cfe` and COMMIT, no change to
  how an index is written or stored (`indexMemory`, `parseIndex`,
  `SegmentIndexCap`, the handoff notice and steps, `SegmentIndex`, the
  memory_segment format). If any changed, stop: the reused draws would not
  be what the new runtime writes, and the plan is re-frozen with new
  handoffs.
- **G2, every index visible:** rendering each probe checkpoint's header
  with each draw's indexes (no model call) shows at least one line for
  every segment that has an index, and no segment only as a pointer. If
  not, stop and report to aos code.
- **G3, enough coverage to test:** at least 5 new probes are range-covered
  by a shown line in at least one draw. If not, stop after the index-recall
  table: A1 could not differ from C on enough facts to test.

During the run: if more than 10% of any arm's answers end in a backend
error, stop and report what ran. No interim look at scores; the primary
test is run once, after grading.

## Files

`select_candidates.py`, `candidates.json`, `context/`, `selection.md`,
`probes-new.json`, `rubric-new.json`, `coverage.py` and its output,
`run.sh`, `run.log`, `samples-*.jsonl`, grading files, `summarize.py`,
`summary.json`; report `reports/memory-probe-recall-header.html`.
