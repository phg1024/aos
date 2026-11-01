# Record-reconstruction probe: frozen plan

Frozen on 2026-11-01, before any answer is scored, by the AOS eval session for
aos book (Chapter 6, Observability and Replay). The scripts that answer and
score may be fixed after this commit only for parsing bugs. Any such fix,
and any change to this plan, goes in a dated "Amendments" section at the end,
and the report lists it.

## Question

Can an operator reconstruct a run's state and causality from AOS's own
records alone, without the model and without outside evidence?

## Material

- **Trials.** The 72 counted trials of the Terminal-Bench budget rerun
  (reports/terminal-bench/budget-rerun, v0.1.1 = 70779be). No new runs and no
  GPU. The three smoke trials are not used.
- **AOS records, the only input to the records-only answers.** Each trial's
  `agent/aos-state/` folder, as Harbor left it in
  `reports/terminal-bench/jobs/tb2-budget-NNN-*/<trial>/`:
  - `kernel/events.json`, `kernel/processes.json`, `kernel/kernel_state.json`;
  - `run/trace.jsonl` (the trace) and `run/calls.jsonl` (the call ledger);
  - `artifacts/index.json` and its payloads: tool results, tool errors,
    checkpoints, the agent-run record and the workflow summary;
  - `aos inspect --json --tail -1`, built at v0.1.1 from `cmd/aos`
    (`.cache/bin/aos-v0.1.1`), run on a copy of the state folder.

  The checkpoint holds the message history the model saw. It is an AOS
  artifact, so it counts as a record. Reading it is not the same as asking
  the model, which this probe never does.
- **Not records.** `agent/aos.log` is AOS's stdout as Harbor captured it,
  outside the state folder, so it is not used. Neither is anything Harbor
  wrote.
- **Ground truth, from outside AOS.**
  - Harbor's `result.json`: exception, agent execution span, reward, adapter
    metadata.
  - The verifier's `ctrf.json` and `test-stdout.txt`.
  - `relay.log`: every request that reached phg-gpu, by start time.
  - `schedule.json`: each trial's arm.
  - `handcheck.json`: the hand-check of the 14 failed trials, done for the
    budget-rerun report.
  - Harbor's container artifact folder (`artifacts/logs/artifacts`) is empty
    in every trial, so it gives nothing.

## Who answers

- **`reconstruct.py`** answers every question from the AOS records only. It
  never opens a file outside `aos-state/` and never reads `results.jsonl`,
  `analysis.json` or `collect.py` output, because those mix in Harbor data.
  For the answer-write question it uses the frozen write rule of the
  budget-rerun plan, copied into the script, since that rule reads only
  trace and ledger records.
- **`truth.py`** answers the same questions from the ground-truth sources
  only.
- **`score.py`** compares the two and writes `scores.jsonl` and
  `summary.json`.
- **Questions marked "hand"** need judgement. They are answered by reading the
  records, and the report says so for each one.

## Scoring

Each question, for each trial it applies to, gets one score:

- **correct**: answerable from the records, and the answer matches the
  ground truth;
- **wrong**: answerable from the records, but the answer differs from the
  ground truth;
- **not answerable**: the records do not hold the answer;
- **no truth**: the records give an answer, but no source outside AOS can
  check it. These are counted apart and never added to "correct".

## Questions

| # | Question | Applies to | Records-only answer | Ground truth | Correct when |
|---|---|---|---|---|---|
| Q1 | Did the run end on its own, and how? One of: agent exit, strict budget exit, error exit, did not end on its own. | 72 | The kernel `process.exited` event for the agent process (code, origin, reason), or its absence; `inspect` "not exited". | Harbor: `AgentTimeoutError` means it did not end on its own. Otherwise the process exit code `aos_exit` that the adapter's shell captured: 0 is an agent exit. 1 is a strict budget exit when the arm is strict and the agent time reached the 879 s budget (±2 s), and otherwise an error exit. The adapter's other metadata (`requests`, usage) is read from the ledger, so it is not used as truth. | The classes match. |
| Q2 | If the run did not end on its own, what ended it? | runs where the truth says "did not end on its own" | Anything in the records naming an outside stop. | Harbor's exception. | The records name the outside stop. |
| Q3 | How many model requests did the run send to the backend? | 72 | The number of ledger records (`run/calls.jsonl`) = `inspect` calls. | `relay.log` POSTs whose start time falls between the trial's agent start and the next trial's first backend check. | The counts are equal, or the difference is exactly the ledger records whose error says the connection never opened (`dial tcp`). |
| Q4 | What did the run spend in tokens? | 72 | Ledger sums: prompt, cached and completion tokens. | None outside AOS: the backend does not report per-request tokens. | Scored "no truth". The report adds whether every request in Q3's truth has a ledger record. |
| Q5 | How long did the agent run? | 72 | From the first kernel event to the agent's `process.exited` event. With no exit event, to the last trace record. | Harbor's `agent_execution` span. | Within ±5 s (setup takes about 1 s). |
| Q6 | What was the last thing the agent did before the end? | 72 | The last trace record: an exit, or the last tool call or result. | `relay.log`: whether any request started after the start of the last ledger call. Its start time is the record time minus `latency_ms`, matched within 2 s. | No request started later than the last recorded one. |
| Q7 | Which tool calls were refused, and was the agent told? | trials with a `tool_error` | Trace `tool_error` records, the matching `tool_error` artifact, and whether its error text reaches a later ledger call's messages. Also, whether a kernel event records it. | None outside AOS. | Scored "no truth". The three records are checked against each other, and the report counts refusals and where each one is recorded. |
| Q8 | Was the run under a time budget, of which kind, and did the low-time warning reach the agent, and when? | 72 | Budget notes in the ledger messages: `budget time=…`, the strict suffix, and the warning text. Also the checkpoint's `budget_warned`. | Kind: `schedule.json`'s arm. Warning: none outside AOS. | Kind matches the arm (off means no notes). The warning is scored "no truth", with the ledger and the checkpoint checked against each other. |
| Q9 | When was the answer file first written? | the 13 failed trials with an answer path | The frozen write rule over trace and ledger: seconds from the first kernel event to the result of the first successful write of every answer path. | `handcheck.json`'s `first_answer_s` (hand-read). | Within 2 s. |
| Q9b | Was the answer file written at all, as far as the records show? | the 39 passed trials with an answer path | The same rule, as yes or no. | The verifier passed, so the file existed. | The rule finds a write. |
| Q10 | What was in the answer at the end? (hand) | the 12 failed trials whose verifier ran and that have an answer path | **raman-fitting:** the last tool output in the records that shows `/app/results.json` after its last write, read for the G and 2D `x0`. **polyglot-c-py:** the last listing of `/app/polyglot` after the last write. | Verifier: the "Got" values (raman) and the folder listing in the assertion (polyglot). | **raman:** both x0 match to 0.1. **polyglot:** the listing matches. |
| Q11 | Did the agent claim a finished, checked answer? (descriptive) | the 14 failed trials | The exit reason text. | The reward. | Not scored. The report counts claims of success in failed trials. Correctness is the verifier's job, and the records are not expected to know it. |

## Known gaps, to confirm and count

These were raised before scoring. Each is confirmed or refuted by the scores,
and counted over all 72 trials. The report gives one example trial for each.

1. **File changes by `run`.** A `run` result records the command, exit
   code, stdout and stderr, but not which files changed. Example: seq 27,
   whose answer was written by a `python3 - <<'EOF'` script.
   - **Count:** the failed trials whose answer write is not found by the
     rule (Q9), and the passed trials where the rule finds no write (Q9b).
2. **Refusals.** For seq 25's `update_memory` refusal: is it recorded as a
   kernel event, or only as a trace `tool_error` and an artifact? Did the
   agent see it, and what did it report afterwards?
   - **Count:** refusals over all trials, and where each one is recorded.
3. **Killed in-flight calls.** What does a Harbor-killed trial's record show
   about its last call?
   - The four trials whose requests were logged late are seq 2, 9, 40
     and 66. The fifth killed trial is seq 58.
   - **Count:** requests in Q3's truth with no ledger record, per killed
     trial, and the time between the last record and the kill.

The scores may show further gaps. They are reported under the same rules:
named by the record that is missing, counted, with one example each.

## Report

- `reports/record-reconstruction-probe.html`, per AGENTS.md.
- A per-question table of correct, wrong, not answerable and no truth.
- Each gap named, counted, and shown with an example trial.
- No fixes are proposed beyond naming the missing record. aos code is
  defining replay separately.
- The scripts, `scores.jsonl` and `summary.json` are committed with the
  report, on this branch. Nothing is pushed.

## Limits

- **One workflow.** All 72 trials are one single-agent workflow on one
  model. Multi-agent runs, leases and the bus are not exercised: the bus and
  lease folders are empty.
- **Ground truth for the answer-write time is itself read from AOS
  records by a person.** No outside timeline of the container's files
  exists. Q9 therefore tests a script against a careful reader, not
  against the container.
- **Killed trials.** The state folder holds nothing written after Harbor
  ended the agent phase. Whether Harbor copied it at that moment, or the
  process stopped writing, is not known from either source. The probe scores
  the folder as Harbor left it.

## Amendments

### A1, 2026-11-01, after scoring: two counts and one window, no rule changed

- **Q9b's count.** The plan says 39 passed trials have an answer path. The
  correct count is 41: 54 trials of the six answer-path tasks, less the 13
  failed ones. Q9b is scored on all 41. The rule is unchanged.
- **Q3's truth window.** `truth.py` attributes a request to a trial when it
  starts between that trial's agent start and the next trial's agent start.
  The only requests between a trial's end and the next trial's agent start
  are the driver's `GET /v1/status` checks, which are not counted. So this
  is the same set of POSTs as the plan's window ("to the next trial's first
  backend check").
- **Q9 for a missed write.** When the write rule finds no write but the
  hand-check does (seq 27), the records-only answer is "not written". The
  plan's four classes do not name this case. It is scored "wrong": the
  script gave an answer, and the answer was wrong.
