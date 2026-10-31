# Blind grading brief (guidance test)

You grade answers that a coding agent gave to questions about its own
earlier work. Each answer is graded against what really happened, which is
recorded in an archive of the agent's messages. You do not know, and must
not try to find out, which condition produced an answer.

## Files you may read (all in this folder or below it)

- `grading/blind-<probe>.json`: the answers for one probe, as
  `[{gid, probe, question, answer}]`, already shuffled.
- `../memory-probe-recall-header/probes-new.json` (probes `h-*`) and
  `probes-negative.json` (probes `n-*`): each probe's question, its
  `checkpoint` and its `note`, which states the truth and the message
  numbers (#N) where it is recorded.
- `../memory-probe-recall-header/rubric-new.json` (probes `h-*`) and
  `rubric-negative.json` (probes `n-*`): what `main` and `exact` require.
- `archive/<segment>.txt`: the archived messages, numbered #N.
  Segments: art-000414 #1-57, art-000507 #58-105, art-000557 #106-194,
  art-000646 #195-249 (checkpoint art-000648); art-001608 #1-29,
  art-001661 #30-120, art-001766 #121-167 (checkpoint art-001768);
  art-003062 #1-17, art-003133 #18-72 (checkpoint art-003135).
- `context/<checkpoint>.txt`: the agent's working memory and live history
  at that checkpoint (later messages than the archive).

## Files you must NOT open

`grading/key.json`, any `samples-*.jsonl`, any `work-*` folder, `run*.log`,
`indexes-*.json`, `coverage*`, `cells.json`, `probes-d*`, `headers-*`,
`reference-*`, `results.json`, `PLAN.md`, and anything in other report
folders except the two files of `../memory-probe-recall-header/` named
above. Do not run scripts. If an answer names its
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
- `negative_claim`: 1 if the answer states, as fact, that the thing the
  question asks about was never done, never measured, never seen or never
  recorded (or that no such run, result or error exists), when the archive
  shows it was. "I have no record of it" stated as a fact about the past
  counts; "I can't recall / I'm not sure" does not. A negative claim is
  also contra.
- `note`: one short sentence for every answer that is not main 1 / exact 1
  / contra 0: what is missing or wrong, and what the archive says (#N).

Check every number, error string and outcome the answer gives against the
archive before you grade it; do not trust the probe note alone for details
it does not mention. Grade every answer in your files; do not skip any.

## Output

Write `grading/grades-<your group>.json` as
`{gid: {"main": 0|1, "exact": 0|1, "contra": 0|1, "negative_claim": 0|1, "note": "..."}}`
`"negative_claim"` included, covering every gid in your blind files. Grade
all of a probe's answers with the same reading of the rubric; for side
details, mark contra only for a wrong statement about this run that the
archive or context file plainly contradicts, and keep the same line for
every answer of the probe. Then reply with the counts per probe (answers,
main, exact, contra, negative_claim) and any answer you found hard to
grade.
