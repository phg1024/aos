# Brief: write the negative-claim probes

You write a few probe questions for a memory evaluation of a coding agent.
Each asks the agent whether it ever did something that it really did, in
an earlier part of its run that now sits in an archive, and that its
working memory and live history do not mention. An agent that relies on
what it can see tends to answer "no, never"; the truth is "yes, and it
showed ...".

## Files you may read (in this folder only)

- `negative-candidates.json`: per archived segment, candidate messages in
  a fixed seeded order: `seq` (message number), `issued_by` (the agent's
  turn that ran the command) and `content` (the command's result).
- `archive/<segment>.txt`: the archived messages, numbered #N, for context.
- `context/<checkpoint>.txt`: the probe checkpoint's working memory and
  live history, which the agent sees when asked.
- `../../bench/memory/pathtracer-bunny-phase3-own-memory-probes.json` and
  `../memory-probe-recall-header/probes-new.json`: existing probes (format
  to copy; topics to avoid).

Do NOT open anything else: no `indexes-*`, `handoffs*`, `header*`,
`coverage*`, `grading/`, `samples-*`, `harm*`, `analysis*`, `primary*`,
`PLAN.md`, and no other file in `../memory-probe-recall-header/` or any
other report folder. Do not run project scripts.

## Rule

For each segment, take its candidates strictly in order. For each, decide
on the one action the command stands for (a build, a benchmark, a check, a
listing, a grep) and its outcome, and accept it only if all hold:

- (a) **The action is absent from the checkpoint's context:** nothing in
  `context/<checkpoint>.txt` (working memory and live history) says this
  action was ever done or what it showed. Check with case-insensitive
  fixed-string searches for the command's distinctive parts (tool, flag,
  file, value, error text) and read the hits. Log the strings you searched
  and the hit counts.
- (b) **It has a definite outcome** the archive states (a number, an
  error, a pass/fail, a found/not found).
- (c) **It is not an existing probe's topic** and differs from what you
  accepted before.
- (d) **It can be asked as "did you ever ...?"** naming the action
  specifically enough that only this event fits, without giving the
  outcome away. Form: "During round N, did you ever <action>? If so, what
  did it show?" (use the round the checkpoint's goal names).

Accept at most 1 per segment, stop a segment at its first acceptance, and
log every candidate considered with its reason in `selection-negative.md`.
A command whose output is only file contents, a listing with no finding,
or a "patched" confirmation has no outcome: reject it.

## Output

- `probes-negative.json`: a JSON array like the existing probes, one entry
  per accepted fact: `id` = `n-<segment digits without leading zeros>-1`
  (e.g. `n-557-1`), `kind` = `"negative-claim"`, `checkpoint`, `question`,
  `topic` (a regex the answer's topic matches), `expect` (a regex a right
  answer matches), `reject` (a regex for "no, never" style answers, e.g.
  `(?i)^\W*no\b`), and `note`: `"Truth: #N <quote of the outcome>. Not in
  the checkpoint's working memory (<id>) or live history (checked:
  <strings>)."`. Regexes are Go RE2: no lookahead or lookbehind (use
  `(?:\D|$)` instead of `(?!\d)`).
- `rubric-negative.json`: `{id: {"main": "...", "exact": "..."}}` in the
  style of `../memory-probe-recall-header/rubric-new.json` (you may read
  that one file too): main = "Yes" plus the core outcome; exact = main
  plus the key detail.
- `selection-negative.md`: the log.

Reply with the accepted probes (id, question, truth) and the totals per
segment.
