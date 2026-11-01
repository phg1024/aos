# Replay check on ca78ad9: plan

Fixed on 2026-11-01, while the three trials were running and before any of
them was replayed or scored. Requested by aos code (replay recordings) and
aos book (killed-trial checks and the reconstruction re-check). This is a
**check, not a measurement**: three trials, one of each kind, no
statistics.

## Build and backend

- **AOS:** ca78ad9, local main, not pushed (parent c53cd79), exported with
  `git archive` into `.cache/replay-ca78ad9/`. Version string
  `v0.1.1-4-gca78ad9`, set with `-ldflags -X main.version=…` as
  `bench/harbor/run-tb.sh` does at ca78ad9. SHA-256:
  - `aos-linux-amd64`
    79501e3cfab43c2e0f45ddbf3f6d327eaa60188382f36cc1e7c9b133430e769e
  - `aos-linux-arm64`
    b0834a1ea105245a66ea51d94669b568fd7063076d2fdc06685b46e1cca21b0c
  - the host build used for `aos replay`:
    b44f71e695fe12ab6bdc99f61770cadf195eb8b79407fe5097e9787287a9acff
- **Adapter:** `bench/harbor/aos_agent.py` at ca78ad9.
- **Backend:** the Strata on phg-gpu, through the budget rerun's `relay.py`
  on 127.0.0.1:18080. phg-gpu came back at a new address:
  - **Address:** 10.0.0.71, where `phg-gpu.local` now resolves, instead of
    10.0.0.197.
  - **Model:** `qwen3.8-flash-next-nvfp4-nvfp4`, engine 0.1.54.
  - **Server start:** 1793522223 (2026-11-01 08:37:03Z).
  - **Before the run:** the server's last request was 85 min earlier, so no
    other client was active.
  - **Relay log:** `.cache/replay-ca78ad9/relay.log`.
- **Harness:** Harbor 0.25.0, `terminal-bench@2.0`, default timeouts, one
  trial at a time.

## Trials

| Job | Task | Arm | Why |
|---|---|---|---|
| `tb2-replay-fix-git-strict` | fix-git | strict | Short, and ends on its own. |
| `tb2-replay-vulnerable-secret-strict` | vulnerable-secret | strict | Writes `/app/results.txt` with `run`, which exercises the new run file changes. |
| `tb2-replay-cobol-modernization-off` | cobol-modernization | off | Hit Harbor's 900 s kill in 3 of 3 rerun trials, which exercises the SIGTERM path. |

Nothing is rerun. A trial that does not time out is reported as such.

## 1. Replay

- For each trial, run `aos replay --json --keep <dir> <copy of aos-state>`
  with the ca78ad9 host build. Record the exit code (0 identical,
  1 diverged, 2 refused) and the verdict.
- For exit 1, record the first divergence: kind, call, step and message
  diff. For exit 2, record the reason.
- The replays run on copies, so the job folders are never changed.

## 2. The killed trial (cobol-modernization, off)

| Check | How |
|---|---|
| AOS exits with `terminated by signal (SIGTERM)` and a final checkpoint | The kernel `process.exited` events and the checkpoint artifact they name. |
| The cut call has an outcome line | Every `phase: start` line in `run/calls.jsonl` has an outcome line that names it. |
| No request reaches the relay after the agent phase ends | The start times of the relay's `POST /v1/chat/completions` against Harbor's `agent_execution.finished_at`. |
| Whether it replays (exit 0) or is refused (exit 2), and why | Section 1. |

If Harbor does not kill it (the agent finishes first), the report says so
and these checks are marked not exercised.

## 3. Reconstruction re-check (Q1 to Q9b of the frozen probe)

- **No frozen script is edited.** A new script, `recheck.py`, answers
  Q1–Q9b on the three new trials in two ways:
  - **(a) "as before".** The frozen probe's records-only method, changed
    only so it reads the two-phase ledger: one call per `phase: start`
    line, with usage taken from its outcome line.
  - **(b) "with the new records".** The same questions, answered from what
    ca78ad9 adds:
    - Q1/Q2 from the signal exit reason;
    - Q3/Q4 from the start and outcome lines, with in-flight calls being
      starts that no outcome names;
    - Q5/Q6 from the exit event and the last record;
    - Q8 from `run.started`'s budget fields;
    - Q9/Q9b from the `changes` recorded on `run` tool results, plus
      `write_file`/`edit_file`.
- **Ground truth** comes from the same sources as the frozen probe, read by
  the same rules:
  - Harbor's `result.json`;
  - the relay log of this run;
  - the arm;
  - the verifier;
  - for Q9's timing, a hand-check of each trial's first answer write,
    marked as such.
- **Before and after for the three gaps.** The "before" column is the
  72-trial probe (44c7c4a); the "after" column is these trials.
  - **Killed run:** Q2, Q3, Q5, Q6.
  - **Answer write:** found by the frozen rule, and found by `changes`.
  - **Budget:** recovered from note text, and recovered from
    `run.started`.
- Q7 (refusals) is reported as counts, as in the probe. Q10 and Q11 are
  not part of the re-check.

## Output

- `reports/replay-check/`: this plan, `recheck.py`, the replay JSON
  outputs, and the records and scores of the re-check.
- `reports/replay-check.html`, per AGENTS.md.
- Committed on this branch. Nothing is pushed or merged.
- Replay results also go to aos code, and the full results to aos book.
