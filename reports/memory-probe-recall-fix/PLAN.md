# Agent-written archive index (A1) follow-up plan (frozen)

Requested by aos book on 2026-10-30, after the first A1 evaluation
(`../memory-probe-recall/`, report `../memory-probe-recall.html`)
stopped after its handoff phase: 5 of 27 handoffs left an index, and
neither primary truth segment got one. aos code changed the handoff in
`50a7cfe` (report `../memory-recall-a1-fix.html`). Designed and run by
the evaluation session. This plan, the scripts in this folder and the
harness are committed before any model call is made, and nothing in them
changes after.

## Question

Unchanged from the first plan. With the handoff fixed, does an
agent-written index make archived but unrecorded facts recoverable
(`r2-verify-dir`, `r5-register-counts`: 0/18 per arm in the own-memory
evaluation), and at what cost? First, does the fixed handoff leave an
index?

## What changed since the first A1 evaluation

Only the runtime, `50a7cfe` on `68538ea`, merged into this branch. With
a1 on:

- The handoff archives exactly the range its notice named
  (`memory.compacted` `to_seq` equals `memory.handoff` `through_seq`).
  In the first evaluation 23 of 27 handoffs archived past it.
- The notice asks for the index first, names the range and the cap, then
  asks for working memory. The reminder names `index_memory`.
- The cap is clamp(64 B x archived messages, 1536, 4096).
- `index_memory` keeps the good lines and reports each rejected one with
  its reason; it is refused only when no line is good.
- A refused or partly rejected write earns one retry step, past the step
  limit and the 16 KB bound.
- `memory.compacted` records `handoff_end` (step_limit, overrun, wrote,
  no_write, other_action) and `handoff_retried`.

With every aid off (`RecallOff()`, arm C) nothing changes (aos code).
`68538ea` also keeps the index out of the single-prompt fallback when a1
is off; the probe backend uses the header path, so this does not touch
the run.

memprobe is unchanged from the first evaluation (`d66bd30`). Its check on
given indexes is against `runtime.MaxSegmentIndexBytes`, now 4096, the
ceiling of the cap, and its tests pass on this runtime.

The binary is built by this session from the commit that freezes this
plan, as `.cache/bin/aos-eval-<commit>`, and its hash goes into
`run.log`.

## Unchanged from the first plan

As written in `../memory-probe-recall/PLAN.md`, and not repeated here:

- model and settings (`qwen3.8-flash-next-nvfp4-nvfp4` on phg-gpu, the
  same sampling and limits, 12 steps per answer; nothing else on phg-gpu
  while it runs);
- step 1: the 9 segments, re-run from the same checkpoints, 3 draws
  each, 27 handoffs, one re-run of a failed handoff; only the index is
  kept; `make_indexes.py` (copied unchanged but for a comment) keeps a
  line only if it lies wholly in the original segment's range;
- step 2: the 11 frozen own-memory probes (sha256 prefix
  `e5e11d8560f5`), arm C (no flags, `RecallOff()`) and arm A1
  (`--recall a1 --agent-indexes indexes-dN.json`, 3 samples per draw),
  9 samples per probe per arm, 198 samples, C first;
- scoring by hand with the own-memory rubric, next to the frozen regex
  score, not blind;
- primary comparison: main score on `r2-verify-dir` +
  `r5-register-counts`, A1 against C, 18 answers each, two-sided Fisher
  exact; the secondary comparisons;
- the harm check (steps, tokens, bytes read per answer; header bytes the
  indexes add; answers that trust an index line without reading what it
  cites; answers wrong because of an index line);
- index quality: 40 lines drawn with `random.Random(20261030)` over the
  kept lines sorted by (draw, segment, from, to, text), plus every line
  citing a truth message of an unrecorded probe, graded holds / partly /
  wrong against the cited messages.

## Stop rule

The first evaluation's rule, unchanged, now part of the plan from the
start (`gate.py`): the probe phase runs only if at least 14 of the 27
draws kept an index line, and each primary truth segment (art-000414,
art-001661) kept one in at least 2 of its 3 draws. `run.sh` applies it
after `make_indexes.py` and stops before the probe phase on FAIL. The
index rate and the handoff analysis below are reported either way.

## Handoff analysis

`handoff_summary.py`, per handoff and in total:

- outcome: indexed, every `index_memory` call refused, never called;
- how the handoff ended (`handoff_end`) and whether it got the retry
  step (`handoff_retried`), which settles whether the 16 KB bound or no
  write ended handoffs (aos code asked for this tally);
- rejected index lines by reason (cap, range, format);
- the first tool called (index first, as the notice now asks?);
- whether the archive matched the notice's range;
- index bytes against the cap, model calls and tokens per handoff.

Compared with the first evaluation's 27 handoffs, handoff by handoff.

## Files

`run.sh`, `make_indexes.py`, `gate.py`, `handoff_summary.py`,
`collect.py` (unchanged), `handoffs.jsonl`, `indexes-d1..3.json`,
`indexes.log`, `gate.log`, `run.log`, `handoff_summary.json`; if the
probe phase runs, `samples-C.jsonl`, `samples-A1-d1..3.jsonl` and the
grading and summary scripts. Report: `reports/memory-probe-recall-fix.html`.
