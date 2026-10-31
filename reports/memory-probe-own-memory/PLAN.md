# Own working memory evaluation plan (frozen)

Proposed by aos code at the user's request on 2026-10-29; designed and run
by the evaluation session. The phase-3 probes injected a hand-written,
correct working memory, so they measure only whether the agent reads a
good memory. This evaluation measures the memory the agent wrote itself.

The design and the rules below were committed (34fa829) before the live
run finished. The probe file was then written from the finished run and
is committed with this plan, now frozen, before any sample is run.

## Harness

`e528cde`: a probe without injected memory keeps the checkpoint's own
working memory and is labelled `own-memory`; `--no-working-memory` strips
it (header and `search_memory`) and is labelled `no-memory`. Each sample
records the working_memory version it resumed with. The runtime is
`74510b7` unchanged: `git diff 74510b7 e528cde` touches only
`internal/memprobe`, the probe CLI's flag text, `bench/` and `reports/`.
`956d59b` adds one harness fix on top: the probe ID is no longer put in
the resumed process's metadata, which the prompt header shows. The
runtime is still `74510b7`'s (`git diff --stat 74510b7 956d59b --
internal/runtime internal/agentflow internal/tools` is empty). Binary
`.cache/bin/aos-eval-956d59b`, built by this session from `956d59b`.

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

### Checkpoints chosen (rule applied to the finished run)

Writer and coordinator checkpoints right after a compaction, counting the
header snapshot's entries whose evidence lies wholly in the archive.
Only three processes reach three such entries; no other writer, and not
the coordinator (0 in its one post-compaction snapshot), qualifies.

| process | checkpoint | snapshot | archive | live | archived-evidence entries |
| --- | --- | --- | --- | --- | --- |
| pid-000006 writer, round 2 | art-000648 (after art-000646, 4th compaction) | art-000643 v8 | #1-#249 | #250-#331 | 6 of 8 |
| pid-000015 writer, round 5 | art-001768 (after art-001766, 3rd) | art-001763 v7 | #1-#167 | #168-#255 | 4 of 5 |
| pid-000025 writer, round 8 | art-003135 (after art-003133, 2nd) | art-003130 v5 | #1-#72 | #73-#135 | 3 of 4 |

Best of the rest: pid-000009 and pid-000022, 2 each.

### Probes (`bench/memory/pathtracer-bunny-phase3-own-memory-probes.json`)

| id | checkpoint | kind | truth (archive) |
| --- | --- | --- | --- |
| r2-first-sah | art-000648 | recorded | #93, #94, #103 |
| r2-root-exit | art-000648 | recorded | #144 |
| r2-shadow-limit | art-000648 | recorded-wrong | entry open/round2-open-bugs says the shadow t_limit bug is open; fixed at #112 |
| r2-verify-dir | art-000648 | unrecorded | #37, #38 |
| r5-empty-base | art-001768 | recorded | #75 |
| r5-entered-stack | art-001768 | recorded | #89 |
| r5-register-counts | art-001768 | unrecorded | #31-#35 |
| r5-tir-guard | art-001768 | recorded-wrong | entry says the Fresnel TIR guard never fires; code at #14, #61, #63 |
| r8-trace-share | art-003135 | recorded | #26 |
| r8-small-node | art-003135 | recorded | #51-#53 |
| r8-sibling-pairs | art-003135 | unrecorded | #29, #31 (and writer log line 163) |

`check_probes.py` (this folder) checks every row against the run: the
checkpoint follows the named compaction, the header snapshot is the named
version, each truth message is archived and holds the quoted text, the
answer's key token is not in live history, "recorded" keys are in the
snapshot and "unrecorded" keys are not, and the expect pattern matches the
archived truth. It reported 0 failures before freezing.

Two notes on what both arms can see. The run's files are on disk in their
final state, so later rounds' writer-log sections are readable by the
probed agent in both arms (r5-register-counts and r8-sibling-pairs can be
answered from the log; r2-verify-dir by listing the directory). And the
B arm keeps the archive and the archived update_memory calls, reachable
only by search_memory.

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

After the phase-3 probe conditions and the live-run analysis, both done.
The live run ended at 2026-10-30T10:31:19Z. Nothing else on phg-gpu runs
alongside it. 11 probes x 9 samples x 2 arms = 198 samples, run A then B,
by `run.sh` in this folder.
