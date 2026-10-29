# Phase 3 memory evaluation plan (frozen)

Written by the evaluation session before any sample of this probe set was
run. The probe file and this plan are committed together; the patterns do
not change during the comparison. If a pattern later proves wrong, the
report shows the frozen and the revised scores side by side.

## What is evaluated

- Runtime: commit `74510b7` (phase 3), built into its own binary
  `.cache/bin/aos-eval-74510b7`.
- Probes: `bench/memory/pathtracer-bunny-memfix-phase3-eval-probes.json`.
- Run: `reports/pathtracer-bunny-memfix` (binary `eceb917`), run from the
  directory it started in, with workflow
  `.cache/workflows/pathtracer-bunny-memfix.json`.
- Model and parameters as the run and phases 1-2: phg-gpu,
  `qwen3.8-flash-next-nvfp4-nvfp4`, temperature 0.6, top-p 0.95,
  65,536-token window, compact ratio 0.8, max tokens 32,768, thinking
  budget 16,384, at most 12 steps per answer.

## Probes

| id | from | checkpoint | where the answer is |
| --- | --- | --- | --- |
| tridata-order | phase 1 | art-001084 | archive; working memory |
| tg-sweep | phase 1 | art-001084 | archive; working memory |
| leaf2-status | phase 1 | art-001084 | live; working memory |
| leaf2-speedup | phase 1 | art-001084 | archive (superseded); working memory |
| w4-first-ab | phase 1 | art-001084 | live (control) |
| leaf2-readopted | 74510b7 | art-001094 | as written by aos code |
| decide-leaf2-wide | 74510b7 | art-001084 | as written by aos code; decision probe for the check rule |
| wm-only-next | new | art-001084 | working memory only (synthetic entry) |
| narrow-tree-wm | new | art-001942 | archive holds the later wrong claim (#407); working memory holds the right one |
| narrow-tree-own | new | art-001222 | archive holds both (#232-248 right, #408 wrong); live holds the wrong one too; working memory holds the right one |

Phase 1/2 probes keep their questions and patterns exactly. They now carry
the working memory that `74510b7`'s phase-3 probes give, so every
art-001084 probe sees the same five entries (wm-only-next adds one).
`TestEvalProbesPointWhereTheySay` checks the message numbers above
against the run, and that the wm-only answer is nowhere in the history.

The working memory in every probe is written by a probe author (aos code
for the shared five entries, this session for the rest), not by the
agent. The probes measure whether an agent uses a working memory it has;
only the live run measures whether it writes a good one.

## Conditions, 9 samples per probe each

1. `memory,check-rule`: the default phase-3 prompt with the probe's
   working memory.
2. `memory,no-check-rule` (`--no-check-rule`): the ablation of "check
   working memory before deciding".
3. `no-memory,check-rule` (`--no-working-memory`): the same binary without
   the working memory. For wm-only-next this is the control that its
   answer is not elsewhere (expected about 0/9).

## Comparisons

- Each probe that existed in phase 2, condition 1 against phase 2:
  - leaf2-speedup, leaf2-status: `reports/memory-probe-phase2/precedence/`
    (order-only wording, the phase-2 binary as committed, 9 each);
  - tridata-order: `reports/memory-probe-phase2/samples-rescored.jsonl`
    plus `archive-x6/samples-phase2.jsonl` (9);
  - tg-sweep, w4-first-ab: `reports/memory-probe-phase2/samples-rescored.jsonl`
    (3 each; too few for a test, reported as counts).
- Condition 1 against 2 (check rule) and 1 against 3 (working memory),
  every probe.
- Two-sided Fisher exact test on correct / not correct. Off-topic answers,
  errors and refusals count as not correct and are listed.
- tg-sweep cost: mean, min and max steps and bytes read, against phase 2's
  4.3 steps and about 19 KB.

## Known weaknesses of the frozen patterns

- narrow-tree-*: `expect` accepts a negation near "faster/speed/help", so
  an answer that affirms the narrow-tree claim and denies a wide-tree
  speedup can score correct; `reject` only catches an affirmative "yes" or
  "that's right". Every narrow-tree answer is also read by hand, and any
  disagreement with the frozen score is reported beside it.
- The samples aos code ran in `reports/memory-probe-phase3/` (6 per probe,
  its own probe file) are the implementer's. They are reported separately
  and are not part of this evaluation.
