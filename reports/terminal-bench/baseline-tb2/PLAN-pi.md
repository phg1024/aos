# TB2 baseline, third arm: pi (frozen, Option A)

**Status: frozen**, after Book's review and the user's choice of Option A
(2026-11-10). The draft is 19bc6e8, with review amendments in bfdd446.
The adapter was merged in as 151d07f.

- The smoke may run now.
- The counted 48-trial run needs the user's explicit go-ahead.

The freeze follows the same rules as the baseline's `PLAN.md`:

- amendments come only before the first counted trial;
- there are no reruns or replacements.

Times are Pacific.

## Question

Chapter 12's baseline (`PLAN.md`, results in `../tb2-baseline.html`,
commit fa79110) found:

- AOS solved 35/48 trials and Terminus-2 28/48. The exact stratified
  p was 0.0152.
- The whole gap came from 3 of the 16 tasks: headless-terminal 3/3 vs 1/3,
  winning-avg-corewars 3/3 vs 1/3, protein-assembly 3/3 vs 0/3.
- 17 of Terminus-2's 20 failures were cut off by Harbor's timeout
  mid-work, most with no answer written.

The baseline could not separate three explanations for the gap:

1. **Tool interface.** AOS runs inside the container with direct file and
   command tools. Terminus-2 drives a tmux session with keystrokes from the
   host.
2. **Early-answer instructions and warnings.** AOS's prompt asks for an
   answer early, and its budget notes and 10% warning press for one.
3. **Deadline.** AOS stops itself at a strict budget. Terminus-2 has none,
   and Harbor's timeout ends it.

**pi** (v1.5.0, via `bench/harbor/pi_agent.py`) runs inside the container
with read, write, edit and bash tools, like AOS. It has its own prompt, no
time budget and no warnings, like Terminus-2. Its interface resembles AOS's,
and its absence of deadline pressure resembles Terminus-2's. So it speaks
mainly to explanation 1.

**What each outcome would show, and what it would not.**

| Outcome | Reading | What it does not show |
|---|---|---|
| pi ≈ AOS, and pi > Terminus-2 | Consistent with the tool interface explaining most of the gap. Deadline pressure and early-answer prompting were not needed for it. | Not that the interface is the cause: pi's prompt, retries and context handling also differ from Terminus-2's. Not that AOS's contracts are useless: they may matter on other tasks or models. |
| pi ≈ Terminus-2, and pi < AOS | Direct tools alone do not close the gap. That points to what AOS has and both others lack: early-answer prompting, warnings and the deadline. | It cannot tell those three apart from each other. |
| pi between the two | Part of the gap may be interface and part the rest. | With 3 trials per task, "between" is mostly not resolvable; see "Detectable difference". |
| pi > AOS | Would say pi's design beats AOS here. | The ceiling makes this nearly undetectable (AOS is 3/3 on 12 tasks). |

The mechanism check matters as much as the pass rate (see "Secondary"):

- Does pi, like Terminus-2, fail by running into Harbor's timeout with no
  answer written?
- Does it lose answers it had in hand?

## Design

- **Tasks.** The baseline's 16 drawn tasks (`drawn.json`), with Harbor's
  default timeouts: 900 to 3600 s.
- **Trials.** 3 pi trials per task, 48 trials in all.
- **Held fixed, as in the baseline:**
  - Harbor 0.25.0 and terminal-bench@2.0;
  - `--n-concurrent 1` and `--allow-agent-host host.docker.internal`;
  - the same Docker Desktop with Rosetta (18 CPUs, 7,932 MiB);
  - the same Strata and model `qwen3.8-flash-next-nvfp4-nvfp4`, through the
    same `relay.py`, which adds the key itself;
  - a quiet machine for the run's whole duration, as in the baseline. No
    other session may start Docker, pull images, run Harbor, call the model
    or do heavy CPU work on the Mac. Book tells Coder when the run starts
    and ends.
- **Sampling, as in the other arms:**
  - temperature 0.6, top_p 0.95, max_tokens 8192;
  - `reasoning_budget_tokens` 2048;
  - a context window of 262144.
  - pi sends these through `models.json` `samplingParams` and `maxTokens`.
    The container check confirmed the fields on every request.

### Comparators: reuse the baseline's arms, or re-check them

The natural comparators are the baseline's 48 aos and 48 terminus-2 trials,
run 2026-11-09 15:16 to 2026-11-10 13:23. Reusing them is cheap, but they
were not interleaved with pi. Weighed honestly:

- **Server.** Same Strata process, if its `started` time is still
  1793522223 and the model and engine (0.1.54) are unchanged. The run checks
  this before every trial, as the baseline did.
  - A restart with the same model and engine would be disclosed. Load and
    cache state after a restart differ.
  - A model or engine change stops the run (see "Stop rule").
- **Time of day and load.** The server is shared with other services. The
  baseline measured no other client's requests during its trials, under A3.
  But the timeouts are wall-clock, so a slower server means less work per
  trial, and nothing in the baseline records per-call latency for
  Terminus-2. This is the drift that matters most, and it would go unseen.
  - The AOS ledger has per-call times, so the baseline's AOS latency is
    known. pi's session file has timestamps, so pi's latency will be known
    too.
  - The report compares the two latency distributions as a disclosed check,
    not as a correction.
  - This check compares pi against AOS only, because Terminus-2's per-call
    latency was never recorded. Drift that would affect the comparison with
    Terminus-2 specifically stays unmeasured.
- **Image cache.** All 16 images are already pulled. Image pulls happen in
  environment setup, before the agent's clock starts, so they do not touch
  agent time.
- **Machine.** Same host and Docker settings. Both runs kept the machine
  quiet (see "Held fixed"). Load that comes from outside the sessions is not
  controlled, in either run.
- **Order.** The baseline alternated aos and terminus-2 within each task
  pair. pi runs a day later, so any drift between the two days falls
  entirely on pi's comparisons.

**Option A: reuse the baseline arms, with the drift disclosed.**

- 48 trials.
- Expected about 10 to 14 h, upper bound about 20 h, plus the daily hold
  (see "Estimate").

**Option B: Option A plus one interleaved re-check round.**

- 16 aos and 16 terminus-2 trials, one per task, interleaved with pi's
  round 1 in random arm order within each task.
- 80 trials. About +7 h expected and +13 h upper bound, so about 17 to
  21 h expected in all.
- The re-check is a drift check only. It is not pooled into the primary
  tests. The report sets each arm's re-check successes beside its baseline
  round mean, per task.

**Recommendation: Option A.**

- The primary tests can only detect differences of about 30 points (see
  below).
- A 16-trial re-check per arm cannot detect drift much smaller than that,
  so it would catch only gross drift. The before-and-after backend checks
  already catch the gross causes: a restart, a model or engine change, or
  another client's requests.
- Option B costs about 50% more machine time for a check that would rarely
  change the reading.
- If the user prefers the stronger design, Option B is fully specified
  here and needs no other change.

**Decided: Option A.** The user chose it on 2026-11-10. Option B's
description is kept for the record, but none of its trials run.

## What is different about pi, recorded rather than tuned

- **Context handling.**
  - pi compacts when the context passes the window minus 16384, which is
    245,760 tokens (93.75%), and keeps the last 20,000 tokens unsummarized.
  - It does one compact-and-retry on an overflow error or an early length
    stop.
  - For comparison: AOS compacts at 0.8 of the window, and Terminus-2
    summarizes when fewer than 8000 tokens are free.
  - The baseline saw no compaction in either arm. The container check did
    not exercise pi's compaction.
- **Retries.** pi retries transient failures on its own. The relay's request
  count can therefore exceed the calls pi completed. The collector records
  both.
- **max_tokens** is 8192, but pi lowers it near the end of the window, to
  the room left.
- **Streaming.** pi streams (`stream: true` with
  `stream_options.include_usage`). The other arms do not. The relay passes
  the stream through. A pi request cut off by a timeout reaches the relay as
  a broken stream, which A3's attribution handles (see "Endings").
- **Reasoning in history.** pi sends earlier `reasoning_content` back in its
  assistant messages.
- **Project instructions.** pi reads an AGENTS.md or CLAUDE.md if present.
  The container check found none in any of the 16 images.
- **No deadline.** Like Terminus-2, only Harbor's timeout ends pi. Its
  prompt says nothing about time.
- **Request timeout.** pi's default per-request timeout.
- **Egress.** TB2's network policy is public, so Harbor restricts egress for
  no arm. The evidence that pi talks only to the relay is the container
  check on an internal network (42dbed7). The counted run cannot show it.
- **Child processes after a timeout.** Not tested for any arm: whether a
  process the agent started is still running when the verifier runs after
  Harbor's kill. The same holds for the baseline's two arms. This is
  disclosed, not fixed.

## Build and files

- **Adapter.** `bench/harbor/pi_agent.py` at 1ef6a84, with the container
  check at 42dbed7.
  - Both commits are on the AOS main checkout only.
  - **The run needs the adapter on this branch.** PYTHONPATH points at
    `bench/harbor`, and the plan pins file hashes at HEAD.
  - **Done:** main was merged into the eval branch as 151d07f, on the
    user's approval, and not pushed.
    - The eval branch's merge base with main is 9070498 (v0.1.10).
    - main is 9070498 plus exactly 1ef6a84 and 42dbed7.
    - So the merge brings in only those two commits. It is not a
      fast-forward, because the eval branch has its own commits, but it is
      clean: `git merge-tree` reports no conflicts.
  - Pushing the eval branch after the merge would also publish 1ef6a84 and
    42dbed7. That push needs the user's approval when it comes up.
- **Pi release: no new download.**
  - The main checkout already has the hash-checked release, fetched by
    Coder on 2026-11-09: `.cache/pi-v1.5.0/download/pi-linux-x64.tar.gz`
    (58,876,253 bytes) and its `SHA256SUMS`.
  - Its SHA-256 is df870ba0e344cb7b3358d8d8f19397b8b8d225a8669784736ad68a5c2169d53b,
    which equals `RELEASE_SHA256["linux-x64"]`. This was checked on
    2026-11-10.
  - **Done:** the tarball and `SHA256SUMS` were copied into this
    worktree's `.cache/pi-v1.5.0/download/`.
    - The copy's SHA-256 matches, and `pi_agent.release_tarball("linux-x64")`
      accepts it.
    - `run-pi.sh` sets `PI_HARBOR_CACHE` to this worktree's
      `.cache/pi-v1.5.0`.
  - The hash is verified at freeze, and by the adapter on every install.
- **Pinned at freeze** (SHA-256).
  - `bench/harbor/run-tb.sh --self-test` passed with 46 tests, and
    `test_collect_pi.py` with 10 tests. Both ran with ANTHROPIC_API_KEY and
    AOS_LLM_API_KEY unset.

| File | SHA-256 |
|---|---|
| `bench/harbor/pi_agent.py` | f9235fdf020496c13abfb2a52b141f2d8c8d159248baa38f9c51f1cb646492ca |
| `bench/harbor/aos_agent.py` | 3b85d008cef55b1e581c500b27aea6817fa0aaf448c692d8336dab740de64c42 |
| `.cache/pi-v1.5.0/download/pi-linux-x64.tar.gz` | df870ba0e344cb7b3358d8d8f19397b8b8d225a8669784736ad68a5c2169d53b |
| `run-pi.sh` | f10b9928a2200d71f425e35c184aaf7d851ab55b4fa40154a6bd25b883edb5a5 |
| `relay-start-pi.sh` | 5dff95ad44e58bd07ce7925e0c3df0e42b8dc5f9defa18c44abe0bd0c283be58 |
| `relay.py` (the baseline's, unchanged) | fb0e7814544f56667aaf7fd8f08466362d4301fba8f26ad40a6201736e593445 |
| `collect_pi.py` | 3f6b7cd908deb59df7ac2bb6b3d25a57180fa58dc005c9a8911fd37bba69792a |
| `collect.py` (the baseline's, at A3) | 9ee4be51a5f8f96d5e1d5dfacac77d6830e1d322207f27d6a9d8fea0be5ad980 |
| `test_collect_pi.py` | 374a4d43ce5ba22b55ff0989c97f8b11dd9569f8e5d56f62b268524d8fe15020 |
| `make_schedule_pi.py` | 9455eaccf0f7119293674e528382e6e62eed8f067fc76912f93b6edcc74bf143 |
| `schedule-pi.json` | bbacc73c005aee11131216961c2adebce54d437f36d2f2c8cb0d478e0bca12bb |
| `analyze_pi.py` | 5d8365200592bd2d5bcf3366a3efd52a9e43c735d6cf2831638f049d0edbba90 |
| `analyze.py` (the baseline's) | 58f427a2f3edb75204d032cdde68a1da863785115f407a69c110e8f1e94a2a6d |
| `power_pi.py` | 0f446930748a908cdcbffd7dfd669eac3b5dfea42493749bc3b8b152592c6583 |
| `survey.json` | 279229e49814f57fd7a6d68c9ccd05f5d101358549e61856c3d1653d9207cb22 |
| `drawn.json` | b9551e4a7cd1e9e0b6cad2bed3fb0621b82c11a5c89abef6410586c652b057ca |

- **New driver files, in this folder.** The baseline's files are not
  edited.
  - `make_schedule_pi.py` and `schedule-pi.json`.
  - `run-pi.sh`: the baseline's `run.sh` with the pi arm. It uses
    `--agent pi_agent:PiAgent`, with PYTHONPATH `bench/harbor` and these
    exported explicitly:
    - `AOS_LLM_BASE_URL=http://127.0.0.1:18080`;
    - `AOS_LLM_MODEL=qwen3.8-flash-next-nvfp4-nvfp4`;
    - the sampling variables at the adapter's defaults.

    Without an explicit `AOS_LLM_MODEL`, pi_agent.py falls back to an older
    default model.
  - `collect_pi.py`: imports `collect.py`, sets its relay log to
    `relay-pi.log`, and adds the pi fields, endings and stop rule. It
    writes `results-pi.jsonl`, or `smoke-pi.jsonl` for seq 0.
    - Its `backend` check also requires engine 0.1.54.
    - Its `done` honours the `PAUSE` file.
  - `relay-start-pi.sh`: the baseline's `relay-start.sh` with
    `--log relay-pi.log`.
  - `analyze_pi.py`, pinned here, fixes the analysis.
    - It was dry-run on a copy, with the baseline's terminus-2 records
      standing in for pi. P1 gave a difference of 0, and P2 reproduced the
      baseline's p = 0.0152 (Holm 0.0304).
  - `make_report_pi.py` only presents `analysis-pi.json`. It is written
    after the run, as the baseline's `make_report.py` was.
  - The relay runs with `--log relay-pi.log`, so the baseline's committed
    `relay.log` stays as it is.
- **Tests.** `test_collect_pi.py` has 10 tests, run on the adapter's
  `testdata/pi-session.jsonl`:
  - a whole run;
  - a recovered 502 (not infra) and an unrecovered 502 (infra);
  - a timeout whose last request is cancelled at the end;
  - a torn last line;
  - no session file (killed before the first reply);
  - no exit without a timeout (agent_crash);
  - an exception before the agent ran (infra);
  - unsaved backend counts;
  - the stop rule.

## Trials and order

- **Seed 20261120.** `make_schedule_pi.py` draws a fresh permutation of the
  16 tasks for each of rounds 1, 2 and 3, from one generator.
  - The draft's seed was 20261111. The baseline seeded round r with
    20261110 + r, so 20261111 reproduced the baseline's round-1 order
    exactly. It was changed before the freeze.
  - Under Option A all 48 trials are pi.
  - With Option B, each round-1 task gets three trials, aos, terminus-2 and
    pi, in an order drawn from the same seed.
  - Jobs are `tb2-pi-{seq:03d}-{task}-{arm}`.
- **Daily hold.** No trial starts that could still be running in the
  08:55–09:10 window, as in the baseline. A trial's worst case is its agent
  timeout plus its verifier timeout plus 10 minutes.
- **Pausing.** The run pauses only at a trial boundary, using the `PAUSE`
  file (A3).

## What is recorded per trial

As in the baseline, from Harbor's job folder, the relay log and the
backend checks before and after the trial:

- the reward and the failed tests;
- the agent and trial times;
- the A3 fields from the first trial: the window, the requests started in
  it, those cancelled at the trial's end, carried-in requests, own 502s,
  corrected contention, and both the frozen and the corrected ending;
- the backend strings, saved to `logs/backend-pi.jsonl` before each record
  (A2).

For pi, in addition:

- **Usage.** Harbor's pi parsing fills `n_input_tokens`, `n_cache_tokens`
  and `n_output_tokens` from the session file
  (`agent/pi/sessions/*.jsonl`).
  - pi appends to that file as it runs, so the usage survives a Harbor
    kill. Only the reply in progress is lost; the container check confirmed
    this with a stub.
  - If pi is killed before its first reply, Harbor falls back to the
    `message_end` events in `pi.txt`.
  - The record keeps the adapter's `usage_source` (`session` or `output`).
- **Exit.** `pi_exit` from `pi.txt`: null after a kill.
- **Calls.**
  - Assistant messages in the session file: completed model calls.
  - Relay requests started in the window: attempts, including retries and
    a cancelled last call.
  - The difference between the two counts is recorded as retries and
    cancellations.
- **Tools.** Tool calls by name (read, write, edit, bash), and tool results
  with `isError`.
- **Context.** Every session entry type other than `session`,
  `model_change`, `thinking_level_change` and `message`, counted by type.
  - Compaction should appear among these. The entry's type name is not yet
    confirmed, because the container check did not compact.
- **Timing.** The first and last assistant message times, and each
  assistant message's time since the previous entry. This is the latency
  check named under "Comparators".

## Endings

The baseline's endings are used, with pi's exit in place of AOS's:

- `harbor_timeout`: Harbor raised AgentTimeoutError.
- `infra_error`:
  - no reward;
  - or an **unrecovered** 502: one that started inside the window while
    the agent was running, after which the agent did not go on. Either the
    turn failed on it, or it is the agent's last request before the agent
    exited.
    - A 502 the agent retried past, or went on from with a later request,
      is recovered and does not make the trial an infra_error.
    - A request cut off at the trial's own end does not count either (A3).
    - Every 502 and every retry is recorded, recovered or not.
  - or a LiteLLM-style API error, for the arms that raise one;
  - or an exception raised before the agent ran, such as a failed install or
    version check.
  - The unrecovered-502 rule applies to all three arms (under Option A only
    pi trials run). So does AOS's
    ledger rule: a backend error counts only if the call was not followed by
    a later successful one.
  - The baseline is unaffected. Under A3, no baseline trial had an own 502
    (`own_502` is 0 in all 96), and no AOS trial recorded a backend error.
    So its endings are the same under this rule.
- `completed`: `pi_exit` "0" and a reward.
- `agent_crash`: `pi_exit` non-zero, or no `pi_exit` without a timeout.

The A3 cancellation rule applies as written. The request must:

- have started in the window;
- be the last started before the agent's end;
- belong to a trial that ended by harbor_timeout;
- and have returned 502, or been logged after the closing check.

## Measures and analysis

`analyze_pi.py` reads `results-pi.jsonl`, the baseline's records (with
`corrections.jsonl`), `failures-pi.json` and the baseline's `failures.json`.

**Primary: two comparisons, fixed in advance.**

- P1: pi − terminus-2 mean reward.
- P2: pi − aos mean reward.

Each uses the baseline's test:

- exact stratified permutation, with labels permuted within each task's 6
  trials (3 pi, 3 of the comparator);
- two-sided;
- the p-value computed exactly by convolution.

The comparator trials are the baseline's own: all 48 of the arm under
Option A. Under Option B the baseline's 48 are still used, and the re-check
round is reported apart.

**Multiple comparisons: Holm at family α = 0.05.** Order the two p-values;
the smaller is tested at 0.025, and, if rejected, the larger at 0.05. Both
p-values are reported raw and Holm-adjusted.

**Secondary, reported but not tested for the family.**

- **Per arm:** pass@1, 2 and 3; pass^2 and 3; endings, frozen and
  corrected; agent time (median, total); tokens and calls; retries.
- **The three split tasks** (headless-terminal, winning-avg-corewars,
  protein-assembly): pi's successes out of 9, beside aos 9/9 and
  terminus-2 2/9.
  - The exact stratified p on those 3 tasks is given for pi vs each arm,
    marked as secondary.
  - The three tasks were chosen from the baseline's results, so the
    comparators' numbers there are selected; pi's are not.
- **Mechanism.** These say which explanation pi's results support:
  - pi's endings, and its failure buckets set beside Terminus-2's (17 of its
    20 failures in timeout) and AOS's;
  - how many failed pi trials had an answer on disk before the end;
  - how many lost one they had (as in Terminus-2's seq 30).
- **Latency.** pi's per-call latency against AOS's in the baseline, as
  described under "Comparators".

## Detectable difference (before the run)

`power_pi.py` simulates each primary test (2000 runs per cell, seed
20261111). The scenarios use the comparator's per-task success rates from
the baseline, shifted by δ for pi and capped at 0 and 1.

- At the ceiling, a positive δ moves tasks already at 3/3 by nothing.
- The pooled difference is then smaller than δ.

Power at the two Holm levels:

| Comparison (comparator's per-task rates) | δ = −.40 | −.30 | −.20 | −.15 | −.10 | +.10 | +.15 | +.20 | +.30 | +.40 |
|---|---|---|---|---|---|---|---|---|---|---|
| P1, terminus-2 (mean .583), α .025 | .98 | .83 | .43 | .20 | .08 | .05 | .09 | .20 | .50 | .82 |
| P1, terminus-2, α .05 | .99 | .90 | .56 | .31 | .14 | .09 | .16 | .31 | .64 | .89 |
| P2, aos (mean .729), α .025 | .99 | .90 | .52 | .30 | .09 | .01 | .02 | .06 | .24 | .51 |
| P2, aos, α .05 | 1.00 | .94 | .66 | .44 | .17 | .02 | .05 | .13 | .36 | .63 |
| baseline's mixed scenario (mean .456), α .025 | .88 | .60 | .27 | .15 | .08 | .12 | .24 | .40 | .73 | .96 |

What this means:

- **P1 (pi vs Terminus-2)** detects pi doing 30 points worse with 83–90%
  power. It detects pi doing better only at about +40 points (82–89%): 9 of
  Terminus-2's 16 tasks are already at 3/3.
- **P2 (pi vs AOS)** detects pi doing 30 points worse with 90–94% power.
  pi doing better than AOS is practically undetectable.
- **A 15-point difference**, the baseline's observed gap, is detected with
  well under 50% power in every direction. So a non-significant result
  does not show that pi equals either arm. The per-task table and the
  three-task secondary carry the reading.

## Stop rule

The baseline's rule is judged on the corrected (A3) values from the first
trial. The run pauses at a trial boundary, and the user decides, if:

- infra errors reach 5, or exceed 10% of trials after 10 trials;
- three trials in a row show a corrected surplus above 0 (contention);
- the model ID, the engine or the server's start time changes;
- Strata or the Docker VM does not answer for more than 10 minutes;
- an agent_crash occurs. This is a pause for a look, not an exclusion: the
  trial stays counted.

## Failure buckets

Every failed pi trial (reward 0, or no reward) is read from its own trace
(the session file, `pi.txt` and the trajectory). It is given exactly one
bucket, with a one-line, trace-cited reason, in `failures-pi.json`. The
definitions and the tie rule are the baseline's final ones, word for word:

- `no_answer`: the answer the tests read was never written;
- `wrong_answer`: it was written, and a test failed on its content;
- `environment`: tooling or environment trouble the agent could not get
  past, such as a missing package, a broken tool or a tmux problem;
- `timeout`: ended by Harbor's timeout or AOS's budget, in the middle of
  the work;
- `infra`: an `infra_error` ending.

> Each failed trial was read from its trace and given one bucket by
> PLAN.md's definitions. Where two buckets applied, the earliest cause was
> chosen and the other noted. A trial ended by Harbor's timeout or AOS's
> budget while the agent was still working is timeout, even when an
> unfinished or wrong answer was already on disk; wrong_answer is a trial
> whose agent finished (exited, or marked the task complete) with a wrong
> answer. Under the strict reading, in which any written answer that a test
> failed on comes first, [the trials concerned] would be wrong_answer
> instead.

For pi, "finished" means pi exited by itself (`pi_exit` set). The report
gives the strict-reading counts beside the main ones, as the baseline's
does.

## Smoke (uncounted)

There is one uncounted pi trial on `git-leak-recovery`, the baseline's smoke
task, run through `run-pi.sh --smoke` before the first counted trial. It
must show:

- **Install.** The pinned build installs, and `pi --version` reports 1.5.0.
- **Requests.** Every request at the relay:
  - carries temperature 0.6, top_p 0.95, max_tokens 8192 and
    reasoning_budget_tokens 2048, the model ID and `stream: true`;
  - carries no `store`, `max_completion_tokens` or `reasoning_effort`;
  - has pi's User-Agent.
- **Streaming through relay.py.** The container check used a stub relay,
  so this is new. Streams pass through relay.py with status 200. The A3
  window attribution then gives a corrected surplus of 0.
- **Usage.**
  - `usage_source` is `session`.
  - The input and output token counts are non-zero.
  - The number of assistant messages equals the relay's requests in the
    window, less any retries.
- **Exit.** `pi_exit` "0", and a reward is present.
- **Collector.** `collect_pi.py record` writes a full record. Its unit tests
  pass.
- **The key.** It appears in no file under the smoke job folder, this folder
  or the pi cache.

Any change the smoke forces comes in as an amendment before the first
counted trial.

## Procedure

1. User decisions: Option A, and the merge (done). Approval of the run's
   length is still to come.
2. Copy the pi release and verify its hash. Build the driver files and
   their tests. Pin the hashes and freeze this plan in a commit (done).
3. Start the relay with `--log relay-pi.log`. Check the backend: model,
   engine and start time.
4. Run the smoke and check it against "Smoke". Amend if needed, before
   counting.
5. Run the counted trials with the stop rule. Status goes to Book after
   round 1, on any stop or anomaly, and at the end.
6. Hand-check every failed pi trial into `failures-pi.json`. Run
   `analyze_pi.py` and `make_report_pi.py`. Run a key check. Commit, and do
   not push.

## Estimate

From the baseline:

- **Per round** (16 tasks): 22,800 s of agent timeouts, and 22,800 s of
  verifier timeouts at most.
- **Overhead:** about 44 s per trial beyond agent time (setup and verifier),
  plus the checks between trials.

| | Trials | Expected | Upper bound |
|---|---|---|---|
| Option A (pi only) | 48 | 10–14 h. pi has no deadline, so it is likely to look like Terminus-2's 12.6 h or less; AOS took 8.0 h. | about 20 h: every trial runs to its timeout (19.0 h), plus about 0.6 h of overhead |
| Option B (adds a re-check round) | 80 | 17–21 h: about +7 h, from the baseline's per-round means (aos 2.7 h, terminus-2 4.2 h) | about 33 h |
| Daily hold | | 0–1.5 h, once per day crossed | |
| Smoke | 1 | about 5 min | 15 min |

**This is well over 30 minutes and needs the user's approval.** Started in
the early afternoon, Option A crosses one 08:55 hold. Option B may cross two.

## Decisions

- Option A. The user chose it on 2026-11-10.
- 1ef6a84 and 42dbed7 merged into the eval branch (151d07f), not pushed.
- Still open: the user's go-ahead for the counted run and its length.

Settled in review (2026-11-10):

- The pi release is copied from the existing cache. There is no download.
- A 502 the agent recovered from is not an infra_error, in any arm. Every
  502 and retry is recorded.
- An agent_crash pauses the run for a look, and the trial stays counted.
