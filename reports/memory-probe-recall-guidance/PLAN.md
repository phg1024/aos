# Guidance test: does "the index is partial" make A1 look things up? (DRAFT)

Requested by aos book on 2026-10-31 (the user, in the aos-book session:
"去试一下", go try it). In the header re-test (`../memory-probe-recall-header/`,
`c0c3471`) A1 was right in 93 of 96 answers whose header showed a line
stating the fact, but only 84 of 111 where no shown line stated it. Most
of those 27 misses claimed something was never done, measured or seen
(h-414-1, h-557-3, h-1661-2), with no memory call. aos code adds two
header sentences on top of `6c55bdb`, behind a RecallPolicy switch (zero
= on) and a probe-memory flag:

- **S1:** the index is partial; before answering that something was never
  done, measured or seen, search_memory for it.
- **S2** (only when some index lines are hidden): points to
  `read_memory artifact_id=...` for the whole index.

Nothing else changes: the index write path and the line selection are
those of `6c55bdb`.

This plan, the probes and the scripts are committed before any model call
and do not change after. Frozen on aos code's commit **(hash at freeze)**,
merged into this branch; the binaries (`.cache/bin/aos-eval-<plan
commit>`, `.cache/bin/aos-headercheck-<plan commit>`) are built from the
plan commit, and `run.log` stamps their hash.

## Arms

Both arms run the same binary, recall a1, header budget 6144 bytes
(`--header-index-bytes 6144`), the 27 index draws of the header re-test
(`indexes-d1..3.json`, copied unchanged from `d9e653e`; sha256 prefixes
550cd905, 90649d21, ce992dd4), the same checkpoints and the same probe
question template.

- **A1:** guidance off (the `6c55bdb` header).
- **A1+G:** guidance on (S1, and S2 where lines are hidden).

A fresh A1 control runs in this run; the re-test's answers are not reused.
The two arms run as two concurrent sequences over the same probe subsets
in the same order, so server load and time of day fall on both alike. `run.log` records each arm's wall-clock time.

The probe question template is unchanged in both arms ("Question from the
operator, answered from what you already know: ... Use your notes, your
history and, if needed, your compacted memory."). Its wording may push
every arm toward fewer lookups; the report says so.

## Cells

A cell is a probe asked with one draw's indexes. `cells.py` fixes them
from inputs that exist before this test.

- **Target set (primary):** cells where no shown line states the fact,
  by the re-test's index-recall grades (`coverage_grades.json`, graded
  before that run's answers were opened): 37 cells of the 23 re-test
  probes (22 "points" with the line shown, 3 "points" hidden, 5 "states"
  hidden, 7 "no"), plus the negative-claim probes' target cells (below).
  Samples per arm: 6 in the 15 cells where A1 missed at least once in the
  re-test, 2 in the 22 where it did not, 4 in each negative-claim cell.
- **Harm set:** cells where a shown line states the fact. 8 of the
  re-test's 32 such cells, drawn with `random.Random("guidance-harm")`
  (h-1608-1 d2, h-1661-1 d1, h-1661-1 d2, h-1766-2 d3, h-414-2 d2,
  h-414-3 d1, h-507-1 d2, h-557-1 d2), and every negative-claim cell of
  this kind; 3 samples per arm.
- **Totals** (`cells.py`): target 43 cells, 158 answers per arm; harm 11
  cells, 33 per arm; 382 answers in all, in 12 runs per arm.

## Negative-claim probes

New probes asking "did you ever ...?" about an action the agent did take
in an archived segment and that its working memory and live history do
not mention, so that relying on what it sees invites "no, never".

- **Candidates** (`select_negative.py`, mechanical): per archived
  segment, the results of `run` tool calls (at least 80 characters),
  excluding messages within 2 of any message an existing probe's note
  cites (11 frozen + 23 re-test probes); shuffled with
  `random.Random("a1-negative-" + segment)`.
- **Writer:** a fresh agent with no access to any index, header, grade
  or answer file (`WRITER.md`) takes each segment's candidates strictly in
  order and accepts the first that (a) is absent from the checkpoint's
  working memory and live history (grepped, logged), (b) has a definite
  outcome, (c) is not an existing probe's topic, (d) can be asked as "did
  you ever ...?" without giving the outcome away. At most 1 per segment.
  Output `probes-negative.json`, `rubric-negative.json`,
  `selection-negative.md`.
- **Result of the rule:** 3 probes from 47 candidates (38 considered),
  all at checkpoint art-001768: n-1608-1 (the first round-5 rebuild of the
  unchanged sources: BUILD_OK with one fastMath deprecation warning, #18),
  n-1661-1 (a grep of `_prof/src/pt_main.cpp` for params.pad / seed /
  getenv: nothing, #38), n-1766-1 (a grep for kInvalidNode: used at
  pt_bvh.cpp:399, defined nowhere, #140). The round-2 and round-8
  candidates were all confirmations, listings, file contents or already in
  working memory. All three are weak facts (a passing build, an empty grep,
  a missing definition).
- **Index recall** of these probes is graded by this session per draw
  (states / points / no, shown or hidden) after the probes are committed
  and before any model call (`coverage_grades-negative.json`); that grade
  decides each cell's set. n-1608-1: a shown line states it in all three
  draws (harm set). n-1661-1: no line (d1), points (d2, d3). n-1766-1:
  points in all three ("kInvalidNode added" implies, but does not say,
  that it was undefined). So 6 negative-claim target cells.
- **Disclosure:** this session has read the re-test's indexes and
  answers. It did not write, choose or edit the negative-claim probes
  beyond fixing format.

## Scoring

Every answer of both arms is graded by hand together, blind to arm:
answers shuffled with `random.Random(20261031)` and stripped of arm,
draw, sample, reads and header fields (`blind.py`). One grader per probe
grades all of that probe's answers from both arms, under one brief
(`grading/BRIEF.md`, the re-test's brief plus `negative_claim`):

- `main`, `exact`, `contra` (the own-memory rubric; `rubric-new.json`
  and `rubric-negative.json`);
- `negative_claim`: 1 if the answer says, as fact, that the asked-about
  thing was never done, measured, seen or recorded, when the archive shows
  it was.

## Primary test

Main score on the target set, A1+G against A1. Statistic: total A1+G
main minus total A1 main. Two-sided stratified permutation test with the
probe as the stratum (within each probe the arm labels of its answers are
permuted; both arms have the same samples in every cell), 100,000
permutations with `random.Random(20261031)`; alpha 0.05.

**Power** (simulated before freezing, `power.py`): with the re-test's
per-cell A1 rates as the baseline and S1 turning a fraction q of the
misses in cells whose misses were negative claims into right answers,
the 37 re-test target cells alone give power 0.27 / 0.63 / 0.90 at q =
0.3 / 0.5 / 0.7; with the 6 negative-claim target cells (baseline assumed
0.4, all affected) the whole target set gives **0.42 / 0.84 / 0.99**.

**Reading a non-significant result:** at this size a p above 0.05 is
*inconclusive*, not "no effect": a guidance that fixes 30% of the
negative-claim misses would be missed more often than not (power 0.28 to
0.42). The report
gives, next to the point estimate, the 95% confidence interval of the
A1+G minus A1 difference in main rate on the target set (stratified
bootstrap over cells, 10,000 resamples with `random.Random(20261031)`),
and states which effect sizes it rules out and which it cannot. The run
is not extended to chase significance.

## Secondary

- Memory calls per answer (searches, reads) and tokens per answer, per arm,
  in the target and harm sets.
- `negative_claim` errors per arm in the target set (pooled Fisher).
- The target set split: re-test probes vs negative-claim probes; cells at
  art-000648 (where S2 appears) vs the rest; by index grade (points shown,
  points or states hidden, no line).
- Per draw.

## Harm and cost check

- Harm set: main per arm (pooled Fisher, and any cell where A1+G main is
  2 or more below A1 of 3); contra per arm.
- Tokens and memory calls per answer in the harm set: needless lookups
  where the shown line already states the fact.

## Gates and stop rule

- **G1 (no model call):** with the guidance off, the frozen binary's
  system prompt and task header for every draw and probe checkpoint are
  byte-identical to `6c55bdb`'s (`reference-6c55bdb/`, rendered from this
  branch at `7d055e8`, whose `internal/agentflow` and `internal/runtime`
  equal `6c55bdb`'s), and the same index lines are shown and hidden.
- **G2 (no model call):** with the guidance on, the same lines are shown
  and hidden, the header differs from the guidance-off one only by
  inserted text, S1 is in every header and S2 exactly in those with
  hidden lines (`gate.py`).
- **G3 (live):** every A1 and A1+G sample records the shown/hidden counts
  G1 predicts and `agent_index_budget` 6144; checked after the run, before
  grading.
- **Stop rule:** a run with more than 10% of its answers ending in a
  backend error stops both arms.

## Files

`PLAN.md`, `cells.py` → `cells.json`, `probes-d*-n*.json`;
`select_negative.py` → `negative-candidates.json`; `WRITER.md`,
`probes-negative.json`, `rubric-negative.json`, `selection-negative.md`,
`coverage_grades-negative.json`; `power.py`; `headercheck/`,
`reference-6c55bdb/`, `gate.py`; `run.sh`; after the run: `run.log`,
`headers-off/`, `headers-on/`, `gate.log`, `samples-*.jsonl`, `grading/`.
