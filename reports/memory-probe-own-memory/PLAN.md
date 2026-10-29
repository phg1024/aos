# Own working memory evaluation plan (draft: design fixed, probes not yet written)

Proposed by aos code at the user's request on 2026-10-29; designed and run
by the evaluation session. The phase-3 probes injected a hand-written,
correct working memory, so they measure only whether the agent reads a
good memory. This evaluation measures the memory the agent wrote itself.

This file fixes the design and the rules for choosing checkpoints and
questions now, before the live run they come from has finished. The probe
file is written from the finished run and committed, with this plan
marked frozen, before any sample is run.

## Harness

`e528cde`: a probe without injected memory keeps the checkpoint's own
working memory and is labelled `own-memory`; `--no-working-memory` strips
it (header and `search_memory`) and is labelled `no-memory`. Each sample
records the working_memory version it resumed with. The runtime is
`74510b7` unchanged: `git diff 74510b7 e528cde` touches only
`internal/memprobe`, the probe CLI's flag text, `bench/` and `reports/`.
Binary `.cache/bin/aos-eval-e528cde`.

## Source run

`reports/pathtracer-bunny-phase3` (binary `74510b7`), probed only after it
has finished, so the state copied for each probe is not being written and
the probes do not share phg-gpu with it.

## Checkpoints

A candidate is a checkpoint of a writer or coordinator process taken
right after a compaction, whose working memory then has at least three
entries with evidence that lies wholly in the archive (message numbers at
or below the compaction's `to_seq`, or archived artifact IDs). From each
process, take the last such checkpoint; if there are more than four
processes, take the four with the most such entries. The list, with each
checkpoint's working memory version and archive range, goes into the
frozen probe file's notes.

## Questions, per checkpoint

Written from the archive and the run's logs, in words that do not copy
the working memory entry's key or wording, with ground truth cited by
message number:

1. recorded: a fact or decision the working memory holds, with its
   evidence in the archive. Two per checkpoint.
2. unrecorded: a fact or decision of the same weight that is in the
   archive but not in the working memory. One per checkpoint; the control
   that own memory does not hide what it left out.
3. recorded-wrong: an entry the archive contradicts (a superseded or
   mistaken claim), only where one exists. Found, not planted; the third
   arm aos code suggested (a deliberately wrong entry) needs an "add to
   own memory" mode the harness does not have and is not part of this
   plan.

Every probe has topic, expect and reject patterns as before; every answer
is also read by hand, and any disagreement with the frozen score is
reported beside it.

## Conditions, 9 samples per probe each, check rule on

- A `own-memory,check-rule`: the checkpoint as it is.
- B `no-memory,check-rule` (`--no-working-memory`): the same checkpoint
  without working memory. Its history and archive are unchanged, so
  update_memory calls the agent made before the checkpoint stay readable:
  B removes the working memory structure, not every trace of the notes.

## Comparisons

- Per probe, A against B: correct, superseded, off-topic, steps, bytes
  read; two-sided Fisher exact on correct.
- Per question kind, counts summed over probes, and a sign test over
  probes (A better / equal / worse).
- recorded-wrong: how often A repeats the wrong entry against how often B
  does.
- For each A answer that cites working memory: did the entry hold the
  detail the answer stated (as in the phase-3 report).

## Queue

After the phase-3 probe conditions now running and the live-run analysis.
Nothing on phg-gpu runs alongside it.
