# Grading notes (written before the key was opened)

Eight grader agents, one per group, each grading every answer of its
probes from both arms: a h-414-*; b h-507-1, h-557-*; c h-646-*;
d h-1608-*, n-1608-1; e h-1661-1, h-1661-2; f h-1661-3, n-1661-1;
g h-1766-*, n-1766-1; h h-3062-*, h-3133-1. 382 of 382 answers graded,
no gid twice, every grade 0/1, exact 1 only with main 1 and contra 0,
negative_claim 1 only with contra 1.

## Forbidden-file check

Every grader's tool calls were scanned for the paths BRIEF.md forbids
(key.json, samples, work, run logs, indexes, coverage, cells, probes-d*,
headers, reference, results, PLAN.md, and other files of the header
folder). One access, by grader f, in its second shell call:

    grep -n '1661-3\|n-1661-1' ../memory-probe-recall-header/*.json | head

It printed exactly these 10 lines (from the grader's transcript; the
order is the grader's shell's):

    ../memory-probe-recall-header/coverage-old.json:638:  "probe": "h-1661-3",
    ../memory-probe-recall-header/coverage-oldparse.json:787:  "probe": "h-1661-3",
    ../memory-probe-recall-header/coverage-newparse.json:827:  "probe": "h-1661-3",
    ../memory-probe-recall-header/coverage_grades.json:325: "h-1661-3": {
    ../memory-probe-recall-header/coverage_grades-old.json:268: "h-1661-3": {
    ../memory-probe-recall-header/coverage_grades-newparse.json:268: "h-1661-3": {
    ../memory-probe-recall-header/gates.json:29:   "h-1661-3",
    ../memory-probe-recall-header/gates-oldparse.json:25:   "h-1661-3",
    ../memory-probe-recall-header/primary.json:37:  "h-1661-3": {
    ../memory-probe-recall-header/coverage.json:836:  "probe": "h-1661-3",

The grep searched `*.json` only (no `coverage*.md`, so no "Truth:"
line), and `head` cut it to the matching lines themselves: each is a
probe id as a key or value. None is a truth text, an index line, a grade
note or a per-probe count (primary.json's counts for h-1661-3 are on
lines 38-40, which were not printed). Rerunning the same grep in this
folder gives 14 matches; its first 10 in this shell's order swap
primary.json:37 for gates-newparse.json:27, the same kind of line.
Nothing in them names this run's arms. Grader f's grades are still
reported separately in the results so that a reader can compare them.

The other matches of the scan are not accesses: grader b's and e's own
"coverage check" of their gids, and grader d's hand-back text listing
the files it did not open.

## Rubric readings the graders flagged (applied to every answer of the probe)

- h-1608-2 (d): all 24 answers give 45.220 ms (the round-4 figure in the
  round-5 goal and the benchmark JSON); the rubric's main is "about
  45.6 ms" (the writer-log tail, #5, 45.635). All main 0, contra 1. Only
  12 of the 24 pin 45.220 on the writer log; under a looser contra line
  the probe's contra would be 12.
- n-1608-1 (d): exact 1 when the answer names the single fastMath
  deprecation warning, without mathMode or the line number.
- n-1766-1 (g): exact read literally, requiring the 0xFFFFFFFFu value; 16
  answers are main 1, exact 0 for that alone (a looser reading: exact 23
  instead of 7).
- h-3062-2 (h): main 1 for "about 0.93% faster" without 10/10 (6
  answers); a strict reading makes them main 0.
- h-3133-1 (h): 31.214 (the #25 default run, before any diagnostic
  hook) accepted as main, exact 0; the note names #38 (31.236).
- h-1661-3 (f): exact needs 44.757; the base figure is optional.
- h-414-3 (a): exact needs the error string or exit 1; a generic write
  error is exact 0.
- Borderline contra calls on side details: h-646-1 (c, "round-1" binary
  label, 2 answers), h-1661-1 (e, g0065), h-1766-3 (g, count of
  failed attempts, 2 answers), h-557-3 (b, "second-worst" ranking of
  8x8, 4 answers), h-507-1 (b, wrong file for transform_mesh, 4 answers).
- Borderline negative claims: n-1661-1 (f, 5 answers that hedge but open
  with "No" or "no record"), h-3133-1 (h, g0182, "did not write down").

## Path slip in the brief

BRIEF.md lists `probes-negative.json` and `rubric-negative.json` next to
the header folder's files; they are in this folder. Graders d, f and g
looked in the header folder first, found nothing and used this folder's
copies (the only copies).
