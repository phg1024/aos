# Blind grading brief (A1 header re-test)

You grade answers that a coding agent gave to questions about its own
earlier work. Each answer is graded against what really happened, which is
recorded in an archive of the agent's messages. You do not know, and must
not try to find out, which condition produced an answer.

## Files you may read (all in this folder or below it)

- `grading/blind-<probe>.json`: the answers for one probe, as
  `[{gid, probe, question, answer}]`, already shuffled.
- `probes-new.json` (probes `h-*`) and
  `../../bench/memory/pathtracer-bunny-phase3-own-memory-probes.json`
  (probes `r*-*`): each probe's question, its `checkpoint` and its `note`,
  which states the truth and the message numbers (#N) where it is recorded.
- `rubric-new.json` (probes `h-*`) and `grading/rubric-frozen.json`
  (probes `r*-*`): what `main` and `exact` require.
- `grading/archive/<segment>.txt`: the archived messages, numbered #N.
  Segments: art-000414 #1-57, art-000507 #58-105, art-000557 #106-194,
  art-000646 #195-249 (checkpoint art-000648); art-001608 #1-29,
  art-001661 #30-120, art-001766 #121-167 (checkpoint art-001768);
  art-003062 #1-17, art-003133 #18-72 (checkpoint art-003135).
- `context/<checkpoint>.txt`: the agent's working memory and live history
  at that checkpoint (later messages than the archive).

## Files you must NOT open

`grading/key.json`, any `samples-*.jsonl`, any `work-*` folder, `run.log`,
`indexes-*.json`, `coverage*`, `header.json`, `handoffs.jsonl`,
`primary.json`, `PLAN.md`, and anything outside this report folder except
the bench probe file above. Do not run scripts. If an answer names its
own condition (for example, mentions an index line in its header), grade
the facts it states and nothing else.

## Grades, per answer (0 or 1)

- `main`: the answer gives the core fact the question asks for, as the
  rubric's `main` states it. Hedged or "I don't know" answers are 0. An
  answer that gives the core fact and also wrong details is still main 1
  (contra catches the wrong details).
- `exact`: main, plus the rubric's `exact` detail, and no stated fact that
  the archive contradicts (so exact 1 implies contra 0).
- `contra`: the answer states, as fact, something the archive (or the
  context file) contradicts. A refusal, "not recorded", or "I don't know"
  is not contra. Saying an event did not happen when the archive shows it
  did, or giving a wrong number, error, cause or outcome, is contra.
  Small asides unrelated to the question count only if clearly wrong and
  stated as fact about this run.
- `repeats_wrong_entry` (only for `r2-shadow-limit` and `r5-tir-guard`,
  whose working-memory entry is wrong): 1 if the answer repeats that wrong
  entry (r2-shadow-limit: says the t_limit shadow bug is still open /
  unfixed; r5-tir-guard: says the TIR guard never fires with ior 1.5).
- `note`: one short sentence for every answer that is not main 1 / exact 1
  / contra 0: what is missing or wrong, and what the archive says (#N).

Check every number, error string and outcome the answer gives against the
archive before you grade it; do not trust the probe note alone for details
it does not mention. Grade every answer in your files; do not skip any.

## Output

Write `grading/grades-<your group>.json` as
`{gid: {"main": 0|1, "exact": 0|1, "contra": 0|1, "note": "..."}}`
(plus `"repeats_wrong_entry"` for the two probes above), covering every gid
in your blind files. Then reply with the counts per probe (answers, main,
exact, contra) and any answer you found hard to grade.
