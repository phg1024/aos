# Terminal-Bench budget rerun: frozen plan

Frozen on 2026-10-31, before any trial, by the AOS eval session for aos book
(Chapter 5, budgets). The plan is committed before the run. Any change after
this commit goes in a dated "Amendments" section at the end, and the report
lists it. One amendment is already planned: once aos code's fix commit lands,
an amendment (A1) pins the commit, the binaries and the exact strings used to
classify endings. A1 is committed before the first counted trial.

## Question

Does AOS's enforced, visible time budget change the outcome of the two pilot
failures, and what does it cost?

**The pilot.** The pilot (reports/terminal-bench/tb2-pilot.html, 567b142) ran
8 tasks, one trial each, with no AOS budget, and solved 6 of 8. Both failures,
polyglot-c-py and raman-fitting, were Harbor timeouts at 900 s. In each, the
agent made 33 or more tool calls while exploring and never committed an
answer.

## What is held fixed

- **Tasks.** The same 8 tasks as the pilot, from terminal-bench@2.0:
  fix-git, prove-plus-comm, cobol-modernization, openssl-selfsigned-cert,
  nginx-request-logging, polyglot-c-py, vulnerable-secret, raman-fitting.
  Every one has a 900 s agent timeout in its task.toml.
- **Harness.** Harbor 0.25.0. Harbor's default timeouts and multipliers: no
  timeout flags are passed. One trial at a time (`--n-concurrent 1`), and the
  same `--allow-agent-host host.docker.internal` as run-tb.sh.
- **Model and backend: the pilot's own, still running.**
  - The pilot used local Strata on this Mac, not phg-gpu.
  - The model is `qwen3.8-flash-next-unsloth-ud-q4_k_xl` (Unsloth
    UD-Q4_K_XL), engine `strata-main-7ddc17a3`, config
    `Strata/strata-unsloth-ud-q4_k_xl-upstream.json`.
  - The containers reach it at `host.docker.internal:8080`.
  - The Strata process that served the pilot started at
    2026-10-28 10:36:36 local, before the pilot (11:07), and it is still the
    one serving today. The pilot is therefore a valid reference for this
    backend.
  - So that the model matches the pilot, the run uses this Mac and not
    phg-gpu. No phg-gpu job is started.
  - Before each trial, the driver checks `/v1/models` and the engine
    process's start time. If either has changed, the run stops, and the
    change is reported.
- **Sampling.** The adapter's defaults, unchanged from the pilot: temperature
  0.6, top_p 0.95, max_tokens 8192, thinking budget 2048, context 262144,
  compaction ratio 0.8, LLM timeout 3600 s.
- **Machine.** Docker Desktop with Rosetta for amd64 images, 18 CPUs and
  8 GB, as in the pilot.
- **What is not held fixed.**
  - The AOS build is aos code's fix commit, so it has the new solver prompt
    in every arm.
  - The pilot is therefore a reference, not a baseline arm: it differs from
    the `off` arm in its prompt and its AOS version.
  - The comparison that counts is between the arms of this run.

## Arms

Each arm is set with the adapter switch `time_budget=...`. Its exact flag is
pinned in A1.

| Arm | Meaning |
|---|---|
| `off` | No AOS clock: the pilot's condition, with the new prompt. |
| `strict` | Clock notes, the low-time warning, and a hard stop: AOS ends the run with a checkpoint before Harbor's deadline. This is the adapter's default. |
| `advisory` | Clock notes and the warning, but no forced stop. This separates seeing the budget from having it enforced. |

The advisory arm is included because the cost estimate allows it (see
Estimate). It is a full arm in the interleaved schedule, not an add-on.

## Trials and order

- **Size.** 3 rounds × 8 tasks × 3 arms = 72 trials, one Harbor job each.
- **Order.** The order is frozen in `schedule.json`, written by
  `make_schedule.py` with seed 20261031.
  - Each round runs every task once in every arm, back to back.
  - The task order is shuffled per round.
  - Each task's arm order rotates so that, over the three rounds, every arm
    is first, second and third once for that task.
- **Interleaving.** Arms are interleaved at the level of single trials, so
  time of day and server load fall on them alike.
- **No reruns.** Trials are run in schedule order. A failed or errored trial
  is never rerun or replaced.
- **Smoke trial.** One smoke trial (`fix-git`, `strict`) runs before
  trial 1, to check the switch, the adapter and the collection script. It is
  labelled smoke and is not counted.

## What is recorded per trial

`results.jsonl` gets one line per trial, written by `collect.py` from
Harbor's job folder. The fields:

- **Identity.** `seq`, `round`, `task` and `arm`, plus the Harbor trial ID.
- **Reward.** The verifier's reward, 0 or 1. Each failed test is also
  recorded from `verifier/ctrf.json`.
- **Wall time.**
  - Agent execution time, from Harbor's `agent_execution` span.
  - Whole-trial time.
  - The budget handed to AOS, if any.
- **How the trial ended.** Exactly one of four classes:
  - `completed`: AOS exited on its own, not for the budget, and Harbor
    raised no exception.
  - `budget_exit`: AOS ended the run because the time budget ran out, and
    left a checkpoint. The marker string is pinned in A1. Only the strict
    arm can have this ending.
  - `harbor_timeout`: Harbor raised `AgentTimeoutError`.
  - `other`: anything else, with the detail recorded. It splits into
    `infra_error` and `agent_crash`:
    - `infra_error` is a Harbor exception other than `AgentTimeoutError`
      (environment, Docker, agent install or verifier), a missing reward,
      or any `backend_error` in the AOS trace.
    - `agent_crash` is an AOS exit with an error, such as a panic or a
      workflow failure, without a budget or timeout cause.
- **Tokens, from the AOS ledger** (`aos-state/run/calls.jsonl`): model calls
  and prompt, cached and completion tokens. The ledger is used even for
  killed trials. It is never estimated.
- **Steps.** The number of model calls, and the number of tool calls by tool,
  both from the AOS trace.
- **Candidate answer.** Did the agent write a candidate answer, and when?
  This is measured against two marks:
  - `T_warn`: the low-time warning.
    - In `strict` and `advisory`, it is the time of the first warning note
      in the trace.
    - In `off`, there is no warning. `T_warn` is set to the same offset the
      warning would have had, computed from the budget the adapter would
      have handed AOS and the warning fraction pinned in A1.
  - `T_end`: the end of agent execution.

  The fields are `answer_before_warn` and `answer_before_end` (yes/no), and
  `first_answer_s` (seconds from the agent's start).

### Candidate answer: frozen definition

A candidate answer is a write to the file the hidden tests read. The answer
paths come from each task's tests/test_outputs.py.

| Task | Answer path(s) |
|---|---|
| polyglot-c-py | `/app/polyglot/main.py.c` |
| raman-fitting | `/app/results.json` |
| vulnerable-secret | `/app/results.txt` |
| cobol-modernization | `/app/program.py` |
| prove-plus-comm | `plus_comm.v` with the proof completed, plus `plus_comm.vo` (both read by the tests) |
| openssl-selfsigned-cert | `/app/ssl/server.key`, `server.crt`, `server.pem`, `verification.txt`, `/app/check_cert.py` |
| fix-git | n/a: the answer is repository state |
| nginx-request-logging | n/a: the answer is a running server and its config |

**How a write is found in the trace.** A tool call writes an answer path if
it is one of these:

- a `write_file` or `edit_file` call on that path that returned a
  `tool_result`;
- a `run` call that returned a result, whose command names the path with a
  write operation: `>`, `>>`, `tee`, `cp`, `mv`, `-o`, `-out`, `coqc` for
  `.vo`, or a script run that the trace shows creating the file.

For tasks with several answer paths, the candidate exists once every path has
been written.

**Hand-checking.** The `run` rule is a heuristic, so every failed trial's
answer timing is checked by hand against its trace, and any correction is
listed in the report.

**Answer present at the end.** Whether the answer existed at the end is taken
from the verifier, not the trace: a failing existence test (for example
`test_result_file_exists`), or a `FileNotFoundError` in the test output.

## Measures and analysis

1. **Primary: reward per trial, `strict` versus `off`.**
   - Test: an exact stratified permutation test. Arm labels are permuted
     within each task across that task's 6 trials in the two arms. The
     statistic is the difference in mean reward. The test is two-sided.
   - The report gives the per-arm success rate, the difference, and the
     p-value.
2. **Per arm: pass@k and pass^k, for k = 1, 2, 3.**
   - Both come from each task's c successes in n = 3 trials:
     - pass@k = 1 − C(n−c, k) / C(n, k);
     - pass^k = C(c, k) / C(n, k).
   - Each is averaged over the 8 tasks. Per-task values are also shown.
3. **Key secondary: committed answers in failed trials.**
   - Among the failed trials (reward 0) of each arm: how many had a
     candidate answer before `T_warn`, how many before `T_end`, and how many
     had it present at the end.
   - Shown as counts: strict versus off, and advisory alongside.
   - Fisher's exact test is computed only for reference, because the
     numbers of failed trials are small and are themselves an outcome of the
     arm.
4. **Cost, per arm and per task.**
   - Agent wall time: median and total.
   - Tokens: prompt, cached and completion.
   - Model calls and tool calls.
   - The reward each arm gains or loses for its tokens and time.
5. **Endings.** The distribution of ending classes per arm.
6. **Advisory.** The same measures for the advisory arm. Comparisons
   involving advisory (`advisory` vs `off`, and `strict` vs `advisory`)
   are exploratory.

## What this run can and cannot show

- **The sample is small.** It has 8 hand-picked tasks, three trials each, one
  model, and one machine. It is a controlled comparison on these tasks, not
  a Terminal-Bench score, and it does not generalise to TB2.
- **The minimum detectable difference is large**, computed before the run
  for α = 0.05, two-sided, with Fisher's test as an approximation to the
  stratified test:
  - **Pooled, 24 trials per arm.** If `off` repeats the pilot's 18/24,
    `strict` must reach 24/24 to be significant. Generally, differences
    under about 21 to 33 points cannot be detected. Power to detect a true
    0.75 → 0.90 is about 0.19.
  - **The two pilot failures, 6 trials per arm.** Only 0/6 versus 5/6 or
    better, or 1/6 versus 6/6, would be significant. Power for a true 0 →
    0.67 is about 0.36, and for 0 → 0.8 about 0.66.
  - So a non-significant result is not evidence of no effect, and the report
    will say so. Effects are reported as counts with the per-task table, and
    claims stay at "on these tasks, with this model".
- **The secondary measure is descriptive.** Whether failed trials ended with
  a committed answer is reported as counts, not as a claim about causes.
- **Per-trial timing is not causal.** It is reported, but the same model on a
  shared machine can vary in speed.

## Estimate

The pilot took 45 m 29 s for 8 trials (2,729 s of trial time; 1 to 9 s of
environment setup per trial). The six passing tasks averaged 144 s per trial,
and the two failures took 917 s and 947 s.

- **Expected, three arms.** If the pilot failures still fail and run to about
  the 900 s limit, and the rest pass as in the pilot:
  - 2 tasks × 9 trials × ~950 s = 4.75 h;
  - 6 tasks × 9 trials × ~150 s = 2.25 h;
  - 72 trials × ~15 s for job start and teardown = 0.3 h.
  - **Total: about 7.3 h.** The two arms alone would take about 4.9 h.
- **Upper bound.** If every trial runs to its timeout, 72 × ~1,000 s is about
  20 h.
- **Timing check after round 1.** If round 1 (24 trials) takes more than
  4 h, the projection is sent to aos book and the run continues. The arms
  stay balanced because every round is complete.
- **Smoke trial and build:** about 10 minutes.

## Stop rule

The run stops, and the report is written on the trials completed, in any of
these cases:

- **Too many infrastructure errors.**
  - After 10 or more trials have finished, `infra_error` exceeds 10% of the
    finished trials.
  - Or 8 or more trials end in `infra_error`, which is more than 10% of 72,
    at any time.
- **The backend changed.** The model ID or the Strata engine process changed
  (see "What is held fixed").
- **A safety stop.** The Docker VM or Strata stops responding for more than
  10 minutes. This is counted as an infra stop and reported.

After a stop, nothing is rerun to fill gaps. Missing trials are reported as
missing.

## Procedure

1. Wait for aos code's fix commit hash (from aos book).
2. Merge local main into this branch, then check that the fix commit is an
   ancestor of HEAD.
3. Run `bench/harbor/run-tb.sh --self-test`.
4. Build the static Linux binaries once, into a dedicated dist folder.
   Record the SHA-256 of the binaries, of workflows/terminal-bench.json and
   of bench/harbor/aos_agent.py. These files are not edited during the run.
5. Write amendment A1 (the commit, the hashes, the switch flag, the ending
   and warning marker strings, the warning fraction), `run.sh` and
   `collect.py`. Commit them.
6. Run the smoke trial, and check that its record parses in each field. Fix
   only the collection script if needed, and record any fix in A1.
7. Run the 72 trials in schedule order with `run.sh`. It checks the backend
   and the stop rule after every trial, and appends to `results.jsonl`.
8. Hand-check the answer timing of every failed trial.
9. Write the HTML report: reports/terminal-bench/tb2-budget-rerun.html.
   Commit the results and the report on this branch. Do not push.

Job folders stay in reports/terminal-bench/jobs/ (gitignored). Only the
records derived from them are committed.

## Amendments

### A0, 2026-10-31, before any trial: backend is the Strata on phg-gpu

**Why.** aos book relayed a correction from the user ("后端要用phg gpu上的strata啊"):
the run uses the Strata on phg-gpu, not the local Strata on this Mac. This
replaces the "Model and backend" item under "What is held fixed", and the
sentences that rely on it.

**Backend.** The server reports, before the run (2026-10-31 11:40 PDT):

- **Server:** `http://phg-gpu.local:8080` (10.0.0.197), the server the AOS
  live and memory runs used. The API key is read from the `strata-phg-gpu`
  entry in `~/.hermes/config.yaml`, as in the memory runs' run.sh.
- **Model:** `qwen3.8-flash-next-nvfp4-nvfp4` (NVFP4), with n_ctx 262144.
- **Engine and server state:**
  - engine version `0.1.54` (`/v1/status`);
  - started at 1793427905 (2026-10-30 23:25:05 PDT);
  - concurrency 1 (serving 1, requested 1);
  - KV cache int8.
- **Hardware:** an NVIDIA GeForce RTX 5070 Ti with 16 GB.
- **Speed:** a test request through the relay decoded 800 tokens at
  62.7 tok/s; the previous logged request decoded at 56.7 tok/s. Prefill is
  about 1,020 tok/s on long prompts. The pilot measured 72 tok/s decode on
  the Mac.

**The pilot is not a baseline.** Its hardware (Apple Metal) and quantization
(Unsloth UD-Q4_K_XL) differ, as do its prompt and its AOS build. Its 6/8 is
context only. Every comparison is between this run's arms, and the report
says so.

**Reachability.** Docker Desktop's containers on this Mac cannot reach the
LAN. From inside a container:

- `phg-gpu.local` does not resolve;
- 10.0.0.197 refuses every port;
- the host itself reaches 10.0.0.197:8080, and containers reach
  `host.docker.internal` and the internet.

Strata's configuration on phg-gpu is not changed. Instead, `relay.py` (in
this folder, stdlib only) runs on the host and listens on 127.0.0.1:18080
only. It forwards each request to 10.0.0.197:8080. For each request it:

- sets `Host: phg-gpu.local:8080`, the Host that Strata's DNS-rebinding check
  already accepts from the memory runs;
- adds the API key itself, so that the key never enters a task container
  (`AOS_LLM_API_KEY` is empty in the containers);
- streams responses through;
- logs every forwarded request's time, path and status to
  `relay.log`, without the key.

The run sets `AOS_LLM_BASE_URL=http://127.0.0.1:18080`, which the adapter
rewrites to `host.docker.internal:18080` inside the containers, and
`AOS_LLM_MODEL=qwen3.8-flash-next-nvfp4-nvfp4`.

**Checked from a container:** `/v1/models` and a chat completion through the
relay returned 200. The pre-fix connectivity smoke trial is recorded under
"A0 smoke" below.

**Sampling.** The adapter's defaults, which are the pilot's: temperature 0.6,
top_p 0.95, max_tokens 8192, thinking budget 2048, context window 262144,
compaction ratio 0.8, LLM timeout 3600 s. A1 confirms them against the fix
commit, and records any that changed and which were used.

**Backend checks during the run.** These replace the local-Strata checks.

- **Before each trial,** the driver reads `/v1/status`. The run stops if the
  model ID or the server's `started` time has changed, or if the server does
  not respond (a safety stop under the stop rule).
- **After each trial,** the driver compares the server's `activity.requests`
  delta with the number of requests the relay forwarded during the trial.
  - Any surplus means another client used phg-gpu during the trial. The
    trial is marked `contended` and kept, and the count is reported.
  - If three trials in a row are contended, the run pauses, and aos book is
    told before it continues.
- **A relay failure** that ends a trial (HTTP 502 from the relay) is an
  `infra_error`.

**Other jobs on phg-gpu.** Checked at 11:42 PDT.

- The server's last request before my test was at 03:50 PDT, about 8 hours
  earlier, and `activity.requests` rose only by my own test request
  (1070 → 1071).
- The running aos-tui sessions are configured for `strata-phg-vllm`, a
  different machine.
- No other job is queued on phg-gpu. This session owns the phg-gpu queue.

**Estimate at phg-gpu's speed.**

- Decode is about 1.15 to 1.27 times slower than the pilot, and prefill is
  about the same.
- The two hard tasks still stop at about 900 s, so their trials take about
  the same time.
- Passing trials are scaled to about 190 s, from 144 s.
- Total: 2 × 9 × ~950 s (4.75 h) + 6 × 9 × ~190 s (2.85 h) + overhead
  (0.3 h) ≈ **7.9 h**, against 7.3 h on the Mac.
- The upper bound is still about 20 h. With only two arms it would be about
  5.3 h.
- 7.9 h is about the same as the frozen estimate, so all three arms are kept.

**A0 smoke, pre-fix connectivity check (not counted).**

- **Run:** `fix-git` with the branch's pre-fix build (the 567b142 adapter and
  workflow), job `tb2-budget-conn-smoke`, 2026-10-31 18:43:37–18:44:40Z.
- **Outcome:**
  - reward 1.0, no exception, `aos_exit=0`;
  - 63 s of agent time;
  - 10 model calls from the ledger: 62,644 prompt tokens and 2,635
    completion tokens.
- **Relay:** forwarded 12 requests, all 200: the 2 checks above plus the 10
  calls. The server's `activity.requests` went from 1071 to 1081, so no
  other client was active. `/v1/models` and `/v1/status` do not count as
  requests, so the per-trial check compares the server's delta with the
  relay's `POST /v1/chat/completions` count.
- **The key stayed out of the job.** It does not appear in any file under
  the job folder.
- **What this does not check:** the fix build's budget switch. The A1 smoke
  trial checks that.
