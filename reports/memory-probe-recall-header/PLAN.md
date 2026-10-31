# A1 re-test with fair-share header rendering (frozen)

Requested by aos book on 2026-10-30, after the A1 follow-up
(`../memory-probe-recall-fix/`, report `../memory-probe-recall-fix.html`)
found that the header showed agent indexes newest first within 6 KB, so
the oldest round-2 segment's index (art-000414, which holds the
`r2-verify-dir` truth) became a bare pointer in every draw. The user
approved fixing that and re-testing. aos code changed the header to a
fair-share allocation by whole lines in `6c55bdb`: the 6 KB budget is
shared round by round, oldest segment first within a round, each index
shows its first lines and ends with "(+K more lines of this index:
read_memory artifact_id=...)" when shortened.

This plan, the new probes and the scripts are committed before any model
call; nothing in them changes after. It is frozen on runtime `6c55bdb`
(local main, merged into this branch). The binaries
(`.cache/bin/aos-eval-<plan commit>`, `.cache/bin/aos-headercheck-<plan
commit>`) are built from the plan commit, which adds to `6c55bdb` only
memprobe's per-segment shown/hidden counts and this folder; `run.log`
stamps the binary's hash.

## Amendment at freeze: G1 failed, the draws are re-made

The draft reused the follow-up's 27 index draws, under gate G1: no change
to how an index is written between `50a7cfe` and the header commit, else
"stop ... and the plan is re-frozen with new handoffs". `6c55bdb` changes
`parseIndex`: a line holding several entries ("59: a 61: b 63: c") is now
split into them. Applying the new rule to the 338 stored lines, 2 would be
split (draw 1 art-001608: 1 line into 12; draw 3 art-000507: 1 line into
18), so the stored draws are not what the frozen runtime writes. G1
failed. As the rule says, the 27 handoffs are re-run on the frozen binary
(about 30 min and 5.3M tokens on phg-gpu), and gates G0, G2 and G3 are
checked on the new draws by `run.sh` before any probe call. Re-parsing the
two lines instead was considered and not done: it would change the gate's
remedy after the gate failed.

**Stopped, and the remedy reopened (2026-10-31).** aos book's check of
the same G1 caveat, sent before it saw this freeze, asks instead: if any
stored draw differs under the new parser, do not reuse or silently
re-parse it; report which draws and index recall under both parses, and
stop before any model call. `run.sh` had started the handoffs at 00:26:50
UTC; it was stopped at 00:27:25, before any handoff step finished (the
work copy holds no artifact from the run; at most the first model call
was in flight). The offline check (`reparse/`) replays all 34
index_memory calls of the 27 handoffs through the old and new parsers
(copied verbatim) with the cap each call's result reported: the old replay
reproduces all 27 stored indexes; under the new parser 25 are identical
and 2 differ (d1 art-001608: 1 line into 12; d3 art-000507: 1 line into
18), with the same rejections and refusals in all 34 calls. Index recall
(fact stated by a covering line in some draw): 16 of 23 under both
parses; per draw 13, 12, 8 (old) and 13, 12, 10 (new); with a shown line
under 6c55bdb's header, 10, 11, 7 and 10, 11, 9.

**Decision (aos book, 2026-10-31): option (b), the rule as written.** All
27 handoffs are re-run on the frozen binary, and the plan of `6d49bf7`
runs unchanged but for two additions: the header's agent-index budget is
pinned (below), and index recall on the new draws is graded as states /
points / no and as stated by a shown line. The old-against-new parse table
of `ce01aa3` is reported as a disclosed side result, not as a result of
the re-test. Freeze commit: the commit that adds this paragraph; the
binaries are named after it and `run.log` stamps their hash.

**Harness fix before any probe call (2026-10-31).** The run of `d9e653e`
finished the 27 handoffs and passed G0 (26 of 27 draws indexed), G2
(budget 6144) and G3 (21 of 23), then stopped before any probe was asked,
on two harness bugs: 7 new probes' `expect` patterns used `(?!\d)`, which
Go's regexp rejects, so the probe file did not load; and the stop-rule
check had a Python 3.10 syntax error. Fixed without touching the design:
each `(?!\d)` became `(?:\D|$)`, which matches the same texts (only
`expect` changed; probe file `5af3f5b442bb` became `e894f218de67`, and
the regex score is secondary to the hand grade); the check was rewritten.
No Go code changed, so the binaries of `d9e653e` run on; `run.sh probes`
resumes at the probe phase only if the earlier gates passed, without
re-running the handoffs.

**Header budget, pinned:** `RecallPolicy.HeaderIndexBytes = 6144` (the
runtime's default, now set explicitly): `run.sh` passes
`--header-index-bytes 6144` to every handoff and probe run and `-budget
6144` to `headercheck`; G2 also fails unless every rendered header's
budget is 6144; each A1 sample records `agent_index_budget`.

Consequences: the new probes were written before these indexes exist, so
no index can have influenced them; the index-recall grades of the old
draws (`coverage-old.*`, `coverage_grades-old.json`: fact stated in some
draw for 16 of 23) stay as a record and are not used.

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

- **The handoff procedure** of the follow-up (`make_indexes.py`,
  `gate.py`, `handoff_summary.py`, copied unchanged but for paths): 9
  segment checkpoints x 3 draws, `--handoff --recall a1`, one re-run of a
  failed draw, lines kept only if inside the original segment. Not the
  draws themselves (see the amendment).
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

For each new probe and draw, from the new indexes:

- **range-covered:** some line's message range includes the truth
  message (mechanical, `coverage.py`, before any probe call);
- **shown:** the covering line is among the lines the frozen header shows
  (`headercheck` renders the header of each probe checkpoint with each
  draw through the prompt backend, no model call; the header shows an
  index's first lines, so a fact late in a long segment's index can be
  hidden; before any probe call);
- **fact-covered:** some covering line states the fact (graded by hand:
  states it / points at it without the value / no), and **stated by a
  shown line** (a covering line the header shows states it). Graded from
  `coverage.md` after the run but before `blind.py` runs and before any
  answer is opened, and committed then.

Reported per probe, per segment and overall. Not used to choose probes.
Each A1 sample also records the lines its header showed and hid per
segment (`agent_index_shown`, `agent_index_hidden`).

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
(The old draws stated the fact in 13, 12 and 8 of 23 per draw, about
half; the new draws are not known until they are written.)
Cost: 27 handoffs (about 30 min, 5.3M tokens) and 23 x 18 + 11 x 18 =
612 answers (about 5 h, 28M tokens) on phg-gpu.

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

- The same test restricted to probes range-covered by a shown line in at
  least one draw, and to the rest (descriptive: the split is fixed by
  `coverage.json` before any probe call); also by the hand fact-covered
  grade.
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

- **G1, write path unchanged** (checked at freeze): failed on `6c55bdb`
  (`parseIndex` splits run-on lines); handled as the amendment above.

After the handoffs, before any probe call, `run.sh` stops unless:

- **G0, enough indexes** (`gate.py`, the follow-up's rule unchanged): at
  least 14 of the 27 draws kept a line, and art-000414 and art-001661
  each kept one in at least 2 of 3 draws.
- **G2, every index visible** (`headercheck`, `coverage.py`): each probe
  checkpoint's header, with each draw's indexes, shows at least one line
  of every segment that has an index. If not, stop and report to aos code.
- **G3, enough coverage to test** (`coverage.py`): at least 5 new probes
  are range-covered by a shown line in at least one draw. If not, stop
  after the index-recall table: A1 could not differ from C on enough
  facts to test.

During the run: if more than 10% of any probe run's answers end in an
error, stop and report what ran. No interim look at scores; the primary
test is run once, after grading.

Checked before freeze on the old draws, as a test of the scripts only:
`headercheck` shows every indexed segment (art-000414 15 of 20, 12 of 15
and 15 of 15 lines), and 20 of 23 new probes would be range-covered by a
shown line. 6 covering lines of 3 probes were hidden, all late in their
segment's index (h-414-1 at #56 and h-414-3 at #43 in 2 of 3 draws,
h-557-3 at #167 in 2); h-414-1 is covered only by hidden lines.

## Files

`select_candidates.py`, `candidates.json`, `context/`, `selection.md`,
`probes-new.json`, `rubric-new.json`; `make_indexes.py`, `gate.py`,
`handoff_summary.py`, `headercheck/`, `coverage.py`; `run.sh` and its
output (`run.log`, `handoffs.jsonl`, `indexes-d1..3.json`, `indexes.log`,
`gate.log`, `header.json`, `coverage.*`, `gates.json`,
`handoff_summary.json`, `samples-*.jsonl`); `blind.py`, grading files,
`primary.py`, `primary.json`; the old draws' records `coverage-old.*`,
`coverage_grades-old.json`; report
`reports/memory-probe-recall-header.html`.
