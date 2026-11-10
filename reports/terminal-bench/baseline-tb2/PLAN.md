# Terminal-Bench baseline: AOS against Terminus-2 on 16 sampled tasks

Plan by the AOS eval session for aos book (Chapter 12, "Application:
Terminal Tasks Under a Deadline"). It follows the budget rerun's discipline
(reports/terminal-bench/budget-rerun/):

- This plan is committed in stages, each before the step it governs.
  - The sampling section is committed before the draw.
  - The rest is committed, frozen, before the first counted trial.
- After the freeze, changes go only in a dated "Amendments" section, and
  only before the first counted trial.
- No trial is rerun or replaced.

## Question

Does one durable AOS agent, with an enforced time budget, solve real
terminal tasks, and how does it compare with Harbor's reference agent on
the same model? The chapters before this one used the pilot and the
72-trial budget rerun. Those had no baseline arm, and their 8 tasks were
hand-picked. This run adds a baseline arm (Terminus-2) and a random sample
of tasks.

## Sampling

Committed before the draw.

### Feasibility filter

The dataset is terminal-bench@2.0 as Harbor 0.25.0 downloads it: 89 tasks.
`survey.py` reads every task.toml as Harbor resolves it, and `draw.py
eligible` applies the filter. A task is excluded if it meets any of the
criteria below.

| Criterion | Rule | Excluded |
|---|---|---|
| GPU | Harbor's resolved `environment.gpus` is above 0. No task asks for one. sam-cell-seg mentions a GPU only to say the script must run on CPU. | 0 |
| Memory | `environment.memory_mb` is above the Docker Desktop VM's memory, 8,317,267,968 bytes (7,932 MiB). | 2: mcmc-sampling-stan, rstan-to-pystan (8 GB each) |
| CPU | `environment.cpus` is above the VM's 18 CPUs. The largest request is 4. | 0 |
| Agent timeout | `agent.timeout_sec` is above 3600 s. | 2: build-pov-ray, sam-cell-seg |
| Rosetta | The image cannot run under Rosetta on this host. See "Rosetta start check" below. It is applied in draw order. | decided during the draw |

**Eligible before the Rosetta check: 85 tasks.** The full list, sorted, is
in `eligible.json`. The pilot's 8 tasks are all eligible and are treated
like any other: they are not forced in or out.

**Image platforms.** The registry manifests were read for 39 images, and
every one is a single-platform `linux/amd64` image. The other 50 hit
Docker Hub's anonymous rate limit (HTTP 429) and were not read; they are
marked so in `survey.json`.

- Every TB2 image therefore runs under Rosetta on this Apple-silicon host,
  as the pilot's and the rerun's did.
- A search of every task's Dockerfile, instruction, task.toml and scripts
  found no requirement that Rosetta or Docker Desktop's VM lacks: no
  `/dev/kvm`, no AVX-512, no privileged mode, no capabilities and no
  platform pin.
  - The QEMU tasks (qemu-startup, qemu-alpine-ssh, install-windows-3.11)
    run QEMU in software, which needs none of these.
  - The one hit, in install-windows-3.11's Dockerfile, is a comment that the
    image avoids a privileged mount.

### Rosetta start check

Whether an image runs is settled by trying it, not by the metadata.

- `startcheck.sh` pulls the image for `linux/amd64`, then runs `uname -m`
  in it through `/bin/sh`, with a 300 s limit.
- The image passes if the run exits 0 and prints `x86_64`.
- A registry rate limit or network error is not a failure: the script
  waits 10 minutes and tries the same image again.
- Any other pull or run failure fails the image, with its detail recorded
  in `startcheck.jsonl`.

The check is applied only as the draw walks the seeded order, so that only
the images that could be drawn are pulled. The pulls also mean that no
image is pulled during the counted run.

### The draw

- **Seed: 20261109.** `draw.py order` sorts the eligible list and shuffles
  it with `random.Random(20261109)` (CPython's Mersenne Twister).
- `draw.py draw` walks that order. It takes each task whose image passed the
  start check and skips each one that failed, keeping the reason. It stops
  at 16.
- The draw is written to `drawn.json` and committed with the start-check
  results.
- **Disclosure.** The seed, the filter and both scripts were written before
  the order was first printed. The order was then printed once, during the
  scripts' own check and before this commit. Nothing was changed after
  that.

### Smoke task

The smoke task is fixed now, before the draw. It is the first task after
the 16 drawn, in the seeded order, whose agent timeout is 900 s and whose
`expert_time_estimate_min` is 30 or less. It is never one of the 16.

## What is held fixed

- **Harness.** Harbor 0.25.0, terminal-bench@2.0, and Harbor's default task
  timeouts and multipliers (no timeout flags).
  - One trial at a time (`--n-concurrent 1`).
  - `--allow-agent-host host.docker.internal` for both arms, as in the
    rerun.
  - Every task resolves to verifier mode `shared`: the verifier runs in the
    agent's environment, since no task has a verifier environment. Each
    trial's record repeats the task's mode.
- **Model and backend.** The Strata on phg-gpu, model
  `qwen3.8-flash-next-nvfp4-nvfp4`, the rerun's model.
  - Both arms reach it through `relay.py`, on the host's loopback only. The
    relay is this folder's copy of the rerun's, with one addition: it also
    logs, per chat completion, the sampling fields the client sent and its
    User-Agent. It logs no messages and no other header.
  - The relay adds the key itself. The key stays in its config file. It is
    never in a container, a command line, a log, a settings file or this
    folder.
  - Strata is shared with other services. It is never restarted or
    reconfigured, and no trial is started that could still be running in
    the 08:55–09:10 Pacific daily window (see "Order").
- **Machine.** Docker Desktop with Rosetta for amd64 images, 18 CPUs and
  7,932 MiB.

## Arms

| Arm | Agent | Settings |
|---|---|---|
| `aos` | AOS v0.1.10 (9070498, tagged and pushed), through `bench/harbor/aos_agent.py` at that tag | The adapter's default `strict` time budget: `--time-budget <B>s --strict-time-budget`, where B is the task's agent timeout minus setup minus 20 s, as in the rerun's strict arm. |
| `terminus-2` | Harbor's built-in Terminus-2 | Its defaults, except for the settings needed to reach the model and match sampling (below). |

**AOS build.** The static Linux binaries are built from the v0.1.10 tag,
with the flags of `bench/harbor/run-tb.sh` and version string `v0.1.10`,
into `.cache/harbor-dist-v0.1.10/`. Their SHA-256:

- `aos-linux-amd64`:
  4c12d9309ea35f54e3d25e6e04dc115d7a8f6b8c13d712626bd61221b5e3a854
- `aos-linux-arm64`:
  9552d0610a873f0c14eb1927a65b024fc097104fc6d8d1d2056a40d9275c3d10
- The host build used for `aos replay` (darwin/arm64, same flags):
  2f1c777e9de0ce5d0bd4716c30ae895ab2294f285d85c98335c136418239a1e8
- `workflows/terminal-bench.json`:
  72b4d50e6e782ea96b0d809e1bcdf3f142522e477425f5c16cdcb98050042331
- `bench/harbor/aos_agent.py`:
  3b85d008cef55b1e581c500b27aea6817fa0aaf448c692d8336dab740de64c42

The eval branch was fast-forwarded to v0.1.10, so these are the files at
HEAD. `bench/harbor/run-tb.sh --self-test` passes, with 22 tests.

**Sampling, both arms.**

| Setting | aos (adapter default) | terminus-2 | How terminus-2 gets it |
|---|---|---|---|
| temperature | 0.6 | 0.6 | `--ak temperature=0.6` |
| top_p | 0.95 | 0.95 | `llm_call_kwargs` |
| max_tokens | 8192 | 8192 | `llm_call_kwargs`, and `model_info.max_output_tokens` |
| thinking budget | 2048 (`reasoning_budget_tokens`) | 2048 | `llm_call_kwargs.extra_body.reasoning_budget_tokens` |
| context window | 262144 | 262144 | `model_info.max_input_tokens` |

**Terminus-2's model setting.**

- The model is `hosted_vllm/qwen3.8-flash-next-nvfp4-nvfp4`, with
  `api_base` set to the relay.
- Harbor requires `model_info` (token limits and costs) for a `hosted_vllm`
  model. The costs are set to 0.
- Without `max_input_tokens`, LiteLLM falls back to a 1,000,000-token
  context, and Terminus-2's summarization would never trigger before the
  server's 262144 limit. So `model_info` is a needed setting, not a tuning.
- These settings are in `terminus2-settings.json`, which holds no key.

**What cannot be matched, recorded as such.**

- **Context management.**
  - AOS compacts at 0.8 of the window.
  - Terminus-2 keeps its own default: summarize when fewer than 8000
    tokens are free (`proactive_summarization_threshold`), and also on a
    context-length error.
- **Request timeout.**
  - AOS sets 3600 s per call.
  - Terminus-2 uses LiteLLM's default.
- **Prompts, tools and turn structure** are each agent's own, by design.
  - Terminus-2 drives a tmux session with keystrokes, from a host process.
  - AOS runs inside the container with its own tools.
- **Deadline.**
  - Terminus-2 has no time budget, so only Harbor's timeout ends it.
  - AOS has the strict budget. This is the difference the chapter is about.

The smoke trial checks every row of the sampling table from the relay log,
which records what each client actually sent (see "Smoke").

## Trials and order

- **Size.** 3 rounds × 16 tasks × 2 arms = 96 trials, one Harbor job each,
  one at a time.
- **Order.** The order is frozen in `schedule.json`, written by
  `make_schedule.py` with seed 20261110.
  - Each round runs every task once in each arm, back to back.
  - The task order is shuffled per round.
  - The first arm alternates. In round r (1–3), the k-th task (k from 0)
    runs `aos` first when k + r is odd, and `terminus-2` first otherwise.
    Each round has 8 tasks of each kind, and the run has 24 pairs of each
    kind.
- **The daily window.** Before a trial starts, the driver checks whether it
  could still be running in 08:55–09:10 Pacific. Its worst case is the
  task's agent timeout, plus the verifier timeout, plus 10 minutes. If it
  could, the driver waits until 09:10 and starts it then. The order never
  changes.
- **No reruns.** A failed or errored trial is never rerun or replaced.

## What is recorded per trial

`results.jsonl` gets one line per trial, written by `collect.py` from the
Harbor job folder, the relay log and Strata's status.

- **Identity.** `seq`, `round`, `task` and `arm`, plus the Harbor trial ID.
  - The task's agent and verifier timeouts.
  - The task's verifier mode.
- **Reward.** The verifier's reward, 0 or 1, and the failed tests from
  `verifier/ctrf.json`.
- **Wall time.**
  - Agent execution, from Harbor's `agent_execution` span.
  - The whole trial.
  - For `aos`, the budget handed to AOS.
- **Ending.** Exactly one of the classes below.
  - `completed`: the agent ended on its own, not for a budget, and Harbor
    raised no exception.
  - `budget_exit` (`aos` only): AOS stopped the run because the strict
    budget ran out, and left a checkpoint. Its exit record carries `time
    budget exceeded`, as pinned in the rerun's A1 (the string is unchanged
    at v0.1.10; A1 below confirms it).
  - `harbor_timeout`: Harbor raised `AgentTimeoutError`.
  - `infra_error`: anything that is not the agent's own behaviour.
    - A Harbor exception other than `AgentTimeoutError` (environment,
      Docker, agent setup or verifier).
    - A missing reward.
    - A relay 502.
    - A backend error: an AOS ledger call with a non-budget error, or a
      LiteLLM API or connection error raised in Terminus-2.
  - `agent_crash`: the agent failed without one of the causes above, such
    as an AOS panic or workflow failure, or a Terminus-2 exception that is
    not an API error.
- **Tokens and calls.**
  - For `aos`, from its ledger (`run/calls.jsonl`): model calls, prompt
    tokens, cached tokens and completion tokens. Never estimated.
  - For `terminus-2`, from Harbor's agent context (`n_input_tokens`,
    `n_cache_tokens`, `n_output_tokens`) and its trajectory.
  - For both arms, the relay's count of `POST /v1/chat/completions` during
    the trial.
- **Contention.** Strata's `activity.requests` before and after the trial,
  against the relay's count. A surplus marks the trial `contended`; it is
  kept and counted.
- **AOS contracts** (`aos` trials, from the trial's own records):
  - the number of budget notes;
  - the time of the low-time warning, if any;
  - whether a budget exit occurred;
  - the checkpoints written;
  - whether every ledger start line has an outcome line;
  - any context compaction (`memory.compacted` or a handoff).
- **Replay** (`aos` trials). `aos replay --json --keep <dir>` runs with the
  host build on a copy of the trial's state. The exit code is recorded:
  0 identical, 1 diverged, 2 refused. For 1, the first divergence is
  recorded; for 2, the reason. The job folder is never changed.

## Measures and analysis

`analyze.py` reads `results.jsonl` and `failures.json` (the hand-check) and
writes `analysis.json`. It uses the standard library only.

1. **Primary: aos − terminus-2 mean reward.**
   - Test: an exact stratified permutation test. Arm labels are permuted
     within each task across that task's 6 trials. The statistic is the
     difference in mean reward. Two-sided, α = 0.05.
   - The p-value is computed exactly, by convolving the per-task
     distributions (20 splits per task), as in the rerun's `analyze.py`.
   - Reported with each arm's success rate and the difference.
2. **Per arm: pass@1, pass@2, pass@3 and pass^2, pass^3.**
   - From each task's c successes in n = 3:
     - pass@k = 1 − C(n−c, k)/C(n, k);
     - pass^k = C(c, k)/C(n, k).
   - Averaged over the 16 tasks, with the per-task table.
3. **Endings per arm**: completed, budget_exit, harbor_timeout, infra_error,
   agent_crash.
4. **Cost per trial and per arm**: agent wall time (median, total) and
   tokens (prompt, cached, completion), with model calls.
5. **AOS contracts**: per trial and in total, the fields under "AOS
   contracts" above.
6. **Replay**: identical, diverged or refused, per `aos` trial.
7. **Every failed trial gets a cause, in both arms.** Each failed trial
   (reward 0, or no reward) is read from its trace: the AOS trace and
   ledger, or Terminus-2's trajectory and terminal recording. It is
   assigned exactly one bucket:
   - `no_answer`: the answer the tests read was never written;
   - `wrong_answer`: it was written, and a test failed on its content;
   - `environment`: tooling or environment trouble the agent could not get
     past, such as a missing package, a broken tool or a tmux problem;
   - `timeout`: ended by Harbor's timeout or AOS's budget, in the middle of
     the work;
   - `infra`: an `infra_error` ending.

   Each bucket comes with a one-line reason citing the trace (step and
   evidence) in `failures.json`. None may be left unexplained. Where two
   buckets could apply, the earliest cause in the trace is chosen, and the
   other is noted.

## Detectable difference (before the run)

`power.py` simulates the primary test (2000 runs per cell, seed 20261109)
for 16 tasks × 3 trials per arm. It gives the power to detect a true
difference d in success rate:

| Scenario (terminus-2's rate per task) | d = .10 | .15 | .20 | .25 | .30 | .35 | .40 |
|---|---|---|---|---|---|---|---|
| all tasks 0.30 | 0.12 | 0.23 | 0.44 | 0.64 | 0.82 | 0.93 | 0.97 |
| all tasks 0.50 | 0.12 | 0.26 | 0.42 | 0.64 | 0.83 | 0.94 | 0.99 |
| mixed: 6 tasks 0.05, 5 at 0.5, 5 at 0.9 | 0.18 | 0.35 | 0.51 | 0.69 | 0.82 | 0.93 | 0.98 |

**The detectable difference is about 30 percentage points** (80% power at
α = 0.05). Differences of 10 to 15 points have power of 0.12 to 0.35, and
will usually not be significant.

- The best case: 5 extra successes can reach significance if they fall on
  tasks where the other arm has none.
- A non-significant result is not evidence of no difference, and the report
  will say so.
- Claims stay at "on these 16 tasks, with this model, on this machine".
  The sample is random within the feasible set, but small. It estimates
  performance on the 85 feasible tasks only loosely, and it is not a
  Terminal-Bench score.

## Estimate

See "Estimate, from the draw" below.

## Stop rule

The run pauses, and aos book is told, in any of these cases:

- **Infrastructure errors.**
  - After 10 or more trials, `infra_error` is above 10% of the finished
    trials.
  - Or 10 trials end in `infra_error` at any time (above 10% of 96).
- **The backend changed.** The model ID or Strata's `started` time changed.
- **Strata or the Docker VM** does not answer for more than 10 minutes.
- **Contention.** Three trials in a row are contended.

After a pause, nothing is rerun. Continuing needs aos book's word, and
missing trials are reported as missing.

## Procedure

1. Sampling. Commit this section, then run the start check and the draw,
   then commit `startcheck.jsonl` and `drawn.json`.
2. Write `make_schedule.py`, `schedule.json`, `run.sh`, `collect.py`,
   `terminus2-settings.json` and the Estimate.
3. Smoke, counted for nothing: one `terminus-2` trial and one `aos` trial
   on the smoke task, through the relay.
   - Check that both reach Strata.
   - Check the sampling table against the relay log.
   - Check that every field of both records parses.
   - Results go to `smoke.jsonl`.
   - Only the collection scripts may be fixed after the smoke. Any fix is
     listed in A1.
4. Freeze: commit A1 (the smoke, any fixes, the judging strings) and every
   driver file. Send aos book the commit, the draw, the smoke and the
   estimate. Wait for aos book's go-ahead.
5. Run the 96 trials with `run.sh`, which checks the backend, the daily
   window and the stop rule around each trial.
6. Replay every `aos` trial. Hand-check every failed trial into
   `failures.json`. Run `analyze.py`.
7. Write the HTML report (reports/terminal-bench/tb2-baseline.html), with
   no hosts, IPs or home paths in its text. Commit on the eval branch, and
   do not push.

Job folders stay in reports/terminal-bench/jobs/ (gitignored). Only the
records derived from them are committed.

## Draw

Done on 2026-11-09, 15:08–15:09 PST.

**Start check.** Every candidate image passed `startcheck.sh` (`uname -m`
printed `x86_64`). The first attempt at 14:57 hit the registry rate limit,
and the script waited and tried again. No task was skipped for Rosetta.

**The 16 tasks**, in draw order, with their agent timeouts:

| # | Task | Agent timeout | Pilot task |
|---|---|---|---|
| 1 | mailman | 1800 s | |
| 2 | feal-linear-cryptanalysis | 1800 s | |
| 3 | make-doom-for-mips | 900 s | |
| 4 | distribution-search | 3600 s | |
| 5 | headless-terminal | 900 s | |
| 6 | winning-avg-corewars | 3600 s | |
| 7 | prove-plus-comm | 900 s | yes |
| 8 | polyglot-c-py | 900 s | yes |
| 9 | vulnerable-secret | 900 s | yes |
| 10 | protein-assembly | 1800 s | |
| 11 | gpt2-codegolf | 900 s | |
| 12 | log-summary-date-ranges | 900 s | |
| 13 | break-filter-js-from-html | 1200 s | |
| 14 | fix-git | 900 s | yes |
| 15 | regex-log | 900 s | |
| 16 | adaptive-rejection-sampler | 900 s | |

Four of the 16 are pilot tasks, against an expected 16 × 8/85 ≈ 1.5. They
are kept, as the sampling rule says.

**Smoke task:** git-leak-recovery. It is the 17th in the order, with an
agent timeout of 900 s and an expert estimate of 30 min.

**Schedule.** `schedule.json` (seed 20261110) has 96 trials, with `aos`
first in 24 of the 48 task pairs.

## Estimate, from the draw

Each arm-round's agent timeouts sum to 22,800 s: 10 tasks at 900 s, 1 at
1200 s, 3 at 1800 s and 2 at 3600 s.

- **Upper bound.** Every trial runs to its timeout: 6 × 22,800 s = 38.0 h.
  Adding about 90 s per trial for setup and the verifier (2.4 h) gives
  about **40 h**.
- **Expected.** About **27 h**.
  - Trials that fail tend to run to the deadline (both rerun failures did),
    and TB2's random tasks are harder than the pilot's.
  - So trials are taken to use about 65% of their timeout on average:
    0.65 × 38 h ≈ 25 h.
  - Overhead adds 2.4 h.
- **Daily window.** Each morning, a trial that could still be running at
  08:55 is held until 09:10. This costs at most about 1.4 h a day, on the
  3600 s tasks.
- **Timing check.** If round 1 (32 trials) takes more than 15 h, the
  projection is sent to aos book, and the run continues.

## Amendments

### A1, 2026-11-09, before the first counted trial: smoke, judging strings, drivers

**Smoke, counted for nothing.** Both trials ran on git-leak-recovery
through the relay on 2026-11-09, with records in `smoke.jsonl` (seq 0 and
−1).

| | terminus-2 (seq 0) | aos (seq −1) |
|---|---|---|
| Start (PST) | 15:09:28 | 15:12:37 |
| Reward, ending | 1.0, completed | 1.0, completed |
| Agent time / whole trial | 147 s / 184 s | 53 s / 72 s |
| Model calls (relay POSTs, all 200) | 8 | 10 (ledger: 10, none unfinished) |
| Tokens: prompt / cached / completion | 38,148 / 29,818 / 6,948 (Harbor) | 52,366 / 44,535 / 2,505 (ledger) |
| Contention: server delta / relay | 8 / 8 | 10 / 10 |

- **Terminus-2 reaches Strata through the relay.** All 8 of its requests
  returned 200, with User-Agent `litellm/1.106.5`.
- **The sampling table holds.** Every request of both trials carried the
  same fields, as logged by the relay: `max_tokens` 8192, `model`
  `qwen3.8-flash-next-nvfp4-nvfp4`, `reasoning_budget_tokens` 2048,
  `temperature` 0.6, `top_p` 0.95.
  - Neither client sent `stream`, so neither arm streams.
  - LiteLLM merges `extra_body` into the request, so Strata sees the same
    `reasoning_budget_tokens` field AOS sends.
  - A direct check through Harbor's `LiteLLM` class at 15:00, before the
    smoke, also reported a context limit of 262144 and an output limit of
    8192.
- **AOS contracts in the smoke.**
  - The flag was `--time-budget 879s --strict-time-budget`.
  - There were 10 budget notes with the strict suffix, 10 checkpoints and
    a complete ledger.
  - There was no warning, no budget exit and no compaction.
- **Replay.** The smoke trial replayed identical: exit 0, 10 of 10 calls
  matched. Recorded build 4c12d930… (v0.1.10), replay build 2f1c777e….
- **The key stayed out.** It appears in none of the 155 files under the two
  smoke job folders, this folder and the replay copies.
- **Terminus-2's records** for the hand-check are `agent/trajectory.json`
  (8 agent steps), `agent/recording.cast` and `agent/terminus_2.pane`.

**Judging strings, confirmed in v0.1.10's `internal/runtime/budget.go`.**
They are unchanged from the rerun's A1:

- the time note `budget time=<L>s used=<U>s left=<R>s`, with the strict
  suffix;
- the warning, `budget warning: less than 10% of the time is left. …`,
  at fraction 0.1;
- the exit, `time budget exceeded: <L>s strict budget of <owner>`, from
  `ErrTimeBudgetExceeded`.

In v0.1.10 the notes are user messages with the `[#N] ` prefix, which
`collect.py` strips before matching.

**Changes made after the freeze of the sampling section, before any
counted trial.**

- `draw.py` gained the `smoke_task` step, which implements the smoke rule
  stated in the sampling section. It ran once, with the draw.
- `replay.py` now also drops the report's `replay_dir` (a local path) from
  its output. It was fixed after the smoke, and `replay-smoke.jsonl` was
  rewritten without that field. Only the collection side changed.

**Driver files at this commit.** These are not edited during the run:

- `run.sh`, `collect.py`, `relay.py`, `relay-start.sh`, `replay.py`,
  `analyze.py`, `terminus2-settings.json` and `schedule.json`.
- If a parsing bug turns up mid-run, the raw job folders are kept. The
  records are recomputed from them after the run, and the fix is listed in
  the report.
- `relay.log` and the Harbor logs in `logs/` match the repository's `*.log`
  ignore rule. `relay.log` is force-added with the results, because the
  contention and sampling checks rest on it.

### A2, 2026-11-09, after 29 counted trials: timestamp parsing, seq 29, backend log

**Timing.** This amendment comes after 29 counted trials, not before the
first, so it breaks the rule above that amendments come only before the
first counted trial. The user approved it on 2026-11-09 at about 21:30 PST,
while the run was paused. It changes no trial, measure, test or stop rule.

**What happened.** Seq 29 (winning-avg-corewars, aos) ran to the end.
`collect.py` then crashed on an AOS ledger timestamp whose fraction had 5
digits, `…04:49:00.83241+00:00`. Go trims trailing zeros, and Python 3.10's
`fromisoformat` accepts only 3 or 6 digits. `run.sh` treats a failed record
as a stop, so the run halted at 21:19 PST with seq 29 unrecorded.

**Changes.**

- `collect.py`'s `parse_time` pads or truncates the fraction to 6 digits.
  Recomputing all 28 stored records with the fix gives field-for-field
  identical results. The fields that need the backend strings (relay,
  contention, ending) were left out of that check.
- Seq 29 is recorded once, from its job folder, by `a2_record_seq29.py`.
  There is no rerun. Its before/after backend strings were not saved, so its
  server-side request count is lost and its contention is recorded as an
  error: "backend counts not saved". Its relay slice is taken from
  `relay.log` by timestamp. The slice has 81 posts, all 200, matching the
  ledger's 81 calls.
- `run.sh` appends each trial's before/after backend strings to
  `logs/backend.jsonl` before calling `record`, so a collector crash cannot
  lose them again.
- The run resumes at seq 30. `run.sh` skips recorded trials. The 08:55–09:10
  Pacific hold still applies.
