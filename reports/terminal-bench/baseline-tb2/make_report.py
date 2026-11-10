"""Writes reports/terminal-bench/tb2-baseline.html from analysis.json,
failures.json, results.jsonl, replay.jsonl and corrections.jsonl. The
narrative is in this file; every number in a table is read from the data.
Stdlib only.

    python3 -I reports/terminal-bench/baseline-tb2/analyze.py
    python3 -I reports/terminal-bench/baseline-tb2/make_report.py
"""

import html
import json
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
OUT = os.path.join(HERE, "..", "tb2-baseline.html")
sys.path.insert(0, HERE)
import collect  # noqa: E402

A = json.load(open(os.path.join(HERE, "analysis.json")))
F = {f["seq"]: f for f in json.load(open(os.path.join(HERE, "failures.json")))["trials"]}
ROWS = collect.corrected_rows(collect.jsonl(os.path.join(HERE, "results.jsonl")))
REPLAY = {r["seq"]: r for r in collect.jsonl(os.path.join(HERE, "replay.jsonl"))}
TASKS = json.load(open(os.path.join(HERE, "drawn.json")))["drawn"]
ARMS = ["aos", "terminus-2"]
BUCKETS = ["no_answer", "wrong_answer", "environment", "timeout", "infra"]
ENDINGS = ["completed", "budget_exit", "harbor_timeout", "infra_error", "agent_crash"]
e = html.escape


def pct(x):
    return "—" if x is None else f"{100 * x:.1f}%"


def num(x, digits=0):
    return "—" if x is None else f"{x:,.{digits}f}"


def table(head, rows, nums=()):
    out = ["<div class='wrap'><table><tr>"]
    out += [f"<th class='num'>{e(h)}</th>" if i in nums else f"<th>{e(h)}</th>" for i, h in enumerate(head)]
    out.append("</tr>")
    for row in rows:
        out.append("<tr>" + "".join(f"<td class='num'>{c}</td>" if i in nums else f"<td>{c}</td>"
                                     for i, c in enumerate(row)) + "</tr>")
    out.append("</table></div>")
    return "\n".join(out)


def reward_cell(r):
    return "<span class='pass'>1</span>" if r == 1 else "<span class='fail'>0</span>"


P = A["primary"]
ARM = A["arms"]
aos, t2 = ARM["aos"], ARM["terminus-2"]

# Per-task table
task_rows = []
for t in TASKS:
    a, b = A["per_task"]["aos"][t], A["per_task"]["terminus-2"][t]
    task_rows.append([e(t), f"{a['c']}/3", f"{b['c']}/3", f"{a['c'] - b['c']:+d}"])

# Pass@k table
pass_rows = []
for arm in ARMS:
    s = ARM[arm]
    pass_rows.append([e(arm), f"{s['successes']}/{s['n']}", pct(s["pass_at"]["1"]), pct(s["pass_at"]["2"]),
                      pct(s["pass_at"]["3"]), pct(s["pass_hat"]["2"]), pct(s["pass_hat"]["3"])])

# Endings
ending_rows = []
for arm in ARMS:
    s = ARM[arm]
    ending_rows.append([e(arm)] + [f"{s['endings'][k]} → {s['endings_corrected'][k]}"
                                   if s['endings'][k] != s['endings_corrected'][k] else str(s['endings'][k])
                                   for k in ENDINGS])

# Ending x reward
def ending_reward(arm):
    out = []
    for k in ENDINGS:
        rs = [r for r in ROWS if r["arm"] == arm and r["ending_corrected"] == k]
        if rs:
            out.append(f"{k}: {sum(1 for r in rs if r['reward'] == 1)} of {len(rs)} passed")
    return "; ".join(out)

# Failures
fail_counts = []
for arm in ARMS:
    s = ARM[arm]["failures"]
    fail_counts.append([e(arm)] + [str(s[b]) for b in BUCKETS] + [str(s["unexplained"])])
fail_rows = []
for r in sorted(ROWS, key=lambda r: (r["task"], r["seq"])):
    if r["reward"] == 1:
        continue
    f = F.get(r["seq"], {})
    fail_rows.append([str(r["seq"]), e(r["task"]), e(r["arm"]), e(r["ending_corrected"]),
                      f"<b>{e(f.get('bucket', 'UNEXPLAINED'))}</b>", e(f.get("reason", "")),
                      e(" ".join(x for x in (f.get("other"), f.get("note")) if x))])

# Cost
cost_rows = []
for arm in ARMS:
    s = ARM[arm]
    cost_rows.append([e(arm), num(s["agent_s_median"]), num(s["agent_s_total"] / 3600, 1),
                      num(s["relay_posts"]), num(s["prompt_tokens"]), num(s["cached_tokens"]),
                      num(s["completion_tokens"])])

# A3 list
a3_rows = []
for x in A["a3"]:
    a3_rows.append([str(x["seq"]), e(x["task"]), e(x["arm"]), e(x["ending"]), e(x["ending_corrected"]),
                    e(", ".join(t[11:19] + "Z" for t in x["cancelled_at_end"] or [])),
                    e(", ".join(t[11:19] + "Z" for t in x["carried_in"] or [])),
                    e(str((x["contention"] or {}).get("surplus", "—"))),
                    e(str((x["contention_corrected"] or {}).get("surplus", "unknown")))])

# Per-trial AOS contract accounting
aos_rows = []
for r in sorted((r for r in ROWS if r["arm"] == "aos"), key=lambda r: r["seq"]):
    c = r.get("contracts") or {}
    rp = REPLAY.get(r["seq"]) or {}
    rep = rp.get("report") or {}
    aos_rows.append([str(r["seq"]), e(r["task"]), reward_cell(r["reward"]), e(r["ending_corrected"]),
                     num(r.get("agent_s")), str(r.get("model_calls")),
                     str(c.get("budget_notes")), str(c.get("checkpoints")),
                     num(c.get("time_warning_s")) if c.get("time_warning_s") is not None else "—",
                     "yes" if c.get("budget_exit") else "no", "yes" if c.get("ledger_complete") else "<b>no</b>",
                     str(c.get("compactions", 0) + c.get("handoffs", 0)),
                     f"{rep.get('matched', '?')}/{rep.get('calls', '?')} " +
                     ("identical" if rp.get("exit") == 0 else f"<b>exit {rp.get('exit')}</b>")])

fa, ft = ARM["aos"]["failures"], ARM["terminus-2"]["failures"]
wins = [t for t in TASKS if A["per_task"]["aos"][t]["c"] > A["per_task"]["terminus-2"][t]["c"]]
losses = [t for t in TASKS if A["per_task"]["aos"][t]["c"] < A["per_task"]["terminus-2"][t]["c"]]
both0 = [t for t in TASKS if A["per_task"]["aos"][t]["c"] == 0 and A["per_task"]["terminus-2"][t]["c"] == 0]
k = aos["contracts"]

body = f"""<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <title>AOS Terminal-Bench 2 Baseline</title>
  <style>
    body {{ font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; margin: 40px auto; max-width: 1040px; line-height: 1.5; color: #1f2937; padding: 0 16px; }}
    code {{ background: #f3f4f6; padding: 2px 5px; border-radius: 4px; }}
    .hero {{ background: linear-gradient(135deg, #ecfdf5, #eff6ff); padding: 24px; border-radius: 16px; }}
    .note {{ background: #fefce8; border-left: 4px solid #ca8a04; padding: 10px 14px; }}
    ul {{ padding-left: 20px; }}
    table {{ border-collapse: collapse; width: 100%; font-size: 13px; margin: 8px 0 12px; }}
    th, td {{ border: 1px solid #e5e7eb; padding: 5px 8px; text-align: left; vertical-align: top; }}
    th {{ background: #f9fafb; }}
    td.num, th.num {{ text-align: right; font-variant-numeric: tabular-nums; }}
    .pass {{ color: #047857; font-weight: 600; }}
    .fail {{ color: #b91c1c; font-weight: 600; }}
    .muted {{ color: #6b7280; }}
    .wrap {{ overflow-x: auto; }}
  </style>
</head>
<body>
  <div class="hero">
    <h1>Terminal-Bench 2.0 baseline: AOS against Terminus-2</h1>
    <p><b>On 16 tasks drawn at random from Terminal-Bench 2.0, with the same model and sampling, AOS solved {aos['successes']} of 48 trials and Terminus-2 {t2['successes']} of 48.</b>
    That is {pct(P['mean_a'])} against {pct(P['mean_b'])}, a difference of {100 * P['diff']:+.1f} points. The frozen primary test, an exact stratified permutation test within tasks, gives p = {P['p_two_sided']:.4f} (two-sided, α = 0.05).</p>
    <p><b>AOS never did worse on a task.</b> It solved more trials on {len(wins)} tasks ({e(', '.join(wins))}) and fewer on {len(losses)}. Both arms failed every trial of {len(both0)} tasks ({e(', '.join(both0))}); the other {16 - len(wins) - len(losses) - len(both0)} tasks were tied.</p>
    <p><b>AOS finished inside its own deadline every time; Terminus-2 often did not.</b> {t2['endings_corrected']['harbor_timeout']} of 48 Terminus-2 trials ran into Harbor's timeout. AOS had no Harbor timeouts: {aos['endings_corrected']['budget_exit']} trials ended at its strict budget and the rest finished. AOS's median agent time was {num(aos['agent_s_median'])} s against {num(t2['agent_s_median'])} s.</p>
    <p><b>Every AOS trial replayed identically</b> ({aos['replay']['identical']} of 48), and every AOS ledger is complete.</p>
  </div>

  <p class="note"><b>Scope.</b> This is one model ({e('qwen3.8-flash-next-nvfp4-nvfp4')}, served by Strata on a shared GPU server), 16 of the 85 eligible tasks, and 3 trials per arm per task. The run was sized to detect a difference of about 30 points with 80% power. The observed difference, 14.6 points, is smaller than that and comes from 3 of the 16 tasks, so read the p-value together with the per-task table. Terminus-2 runs with its defaults apart from the settings needed to reach the model and match sampling.</p>

  <h2>What was run</h2>
  <ul>
    <li><b>Plan.</b> <code>reports/terminal-bench/baseline-tb2/PLAN.md</code>. Sampling was frozen before the draw (cdd6452); the draw, start check and schedule came next (3e3d9d4); the plan was frozen with amendment A1 before any counted trial (1bcd318).</li>
    <li><b>Two amendments came during the run</b>, both approved by the user while the run was paused between trials, and both changing measurement only, never a trial:
      <ul>
        <li><b>A2</b> (5515155, after 29 trials): the collector crashed on a 5-digit timestamp fraction in seq 29's ledger. The parser was fixed (the 28 stored records recompute identically), seq 29 was recorded from its job folder without a rerun, and the backend checks are now saved before each record. Seq 29's server-side request count was lost.</li>
        <li><b>A3</b> (9a2ac87, after 39 trials): relay requests are attributed by start time, and a request cut off at its trial's own end is recorded as cancelled. See "Data quality". Stored records were not edited; both the frozen and the corrected endings are shown below. From seq 40 on, the stop rule used the corrected values.</li>
      </ul></li>
    <li><b>Tasks.</b> 85 of the 89 TB2 tasks fit the machine's memory and the agent-time cap. 16 were drawn with seed 20261109, after a start check of each image under Rosetta. Their agent timeouts range from 900 s to 3600 s.</li>
    <li><b>Arms.</b> <code>aos</code>: AOS v0.1.10 (9070498) through the Harbor adapter, with the adapter's default strict time budget (agent timeout − setup − 20 s). <code>terminus-2</code>: Harbor's built-in Terminus-2. Both used temperature 0.6, top_p 0.95, max_tokens 8192 and a thinking budget of 2048 tokens. The relay logged every request's sampling fields: all 2,089 chat completions in the run carried the same set.</li>
    <li><b>Harness.</b> Harbor 0.25.0, one trial at a time, Docker Desktop with Rosetta (18 CPUs, 7,932 MiB). Trials alternated arms within each task pair, in an order fixed by seed 20261110.</li>
    <li><b>Time.</b> The first trial started 2026-11-09 15:16 PST and the last ended 2026-11-10 13:23 PST. That includes the two amendment pauses and an 81-minute hold for the server's daily 08:55–09:10 PST window.</li>
  </ul>

  <h2>Reward</h2>
  {table(["Arm", "Solved", "pass@1", "pass@2", "pass@3", "pass^2", "pass^3"], pass_rows, nums=(1, 2, 3, 4, 5, 6))}
  <p class="muted">pass@k and pass^k are computed per task from c successes in n = 3 and averaged over the 16 tasks. pass^3 is the share of tasks solved in all three trials.</p>
  {table(["Task", "aos", "terminus-2", "difference"], task_rows, nums=(1, 2, 3))}
  <p>Primary test: n = {P['n_a']} per arm, mean reward {P['mean_a']:.4f} against {P['mean_b']:.4f}, difference {P['diff']:+.4f}, exact two-sided p = {P['p_two_sided']:.4f}. Labels were permuted within each task's 6 trials; the p-value is computed exactly by convolving the 16 per-task distributions.</p>

  <h2>Endings</h2>
  {table(["Arm"] + ENDINGS, ending_rows)}
  <p class="muted">"a → b" shows the frozen rule's count and the A3-corrected count. The three frozen infra errors (seqs 16, 31 and 38) were requests from the previous trial logged late; under A3 there are no infra errors and no agent crashes.</p>
  <ul>
    <li><b>aos:</b> {e(ending_reward('aos'))}.</li>
    <li><b>terminus-2:</b> {e(ending_reward('terminus-2'))}. A Harbor timeout is not always a failure: the verifier still runs, and a trial passes if the answer was already in place.</li>
  </ul>

  <h2>Why each failed trial failed</h2>
  <p>Every failed trial was read from its own trace (the AOS ledger and trace, or Terminus-2's trajectory and pane) and given one bucket, by the plan's rules: the earliest cause wins, and a second cause is noted.</p>
  {table(["Arm"] + BUCKETS + ["unexplained"], fail_counts, nums=(1, 2, 3, 4, 5, 6))}
  <ul>
    <li><b>How the tie rule was applied.</b> {e(json.load(open(os.path.join(HERE, "failures.json")))["rule"])}</li>
    <li><b>polyglot-c-py failed the same way in all 6 trials, in both arms.</b> Each agent compiled the C side into <code>/app/polyglot/cmain</code> to test it and left it there; the one test requires <code>main.py.c</code> alone in that folder ("Expected only main.py.c, found: ['cmain', 'main.py.c']"). The polyglot itself worked in every trial.</li>
    <li><b>AOS's wrong answers include two exits after a failed self-check.</b> In gpt2-codegolf seqs 25 and 96, AOS's own test of <code>gpt2.c</code> timed out or printed garbage, and it exited with code 0 anyway. In adaptive-rejection-sampler seq 93 it exited knowing its sample standard deviation was too large.</li>
    <li><b>adaptive-rejection-sampler.</b> The test calls <code>ars(f, c(-5, 5), n = 1000)</code>. In every sampler trial but seq 11, the agent's <code>ars()</code> read <code>c(-5, 5)</code> as a different argument, so that test would have failed on the interface however good the sampler was.</li>
    <li><b>Most failures in both arms were trials cut off mid-work with no answer written</b>: make-doom-for-mips (6 of 6), gpt2-codegolf for Terminus-2 (3 of 3), Terminus-2's two winning-avg-corewars failures and two of its three protein-assembly failures. In seq 30, Terminus-2 had a warrior that met the thresholds about 15 minutes before its timeout and never saved it.</li>
  </ul>
  {table(["Seq", "Task", "Arm", "Ending", "Bucket", "Reason, from the trace", "Also"], fail_rows)}

  <h2>Cost</h2>
  {table(["Arm", "Median agent s", "Total agent h", "Model calls", "Prompt tokens", "Cached tokens", "Completion tokens"], cost_rows, nums=(1, 2, 3, 4, 5, 6))}
  <p class="muted">Model calls are the relay's chat completions attributed to each arm's trials. AOS tokens are from its ledger; Terminus-2 tokens are Harbor's counts. Both arms ran against a server with prefix caching, so most prompt tokens were cached.</p>

  <h2>AOS contracts, per trial</h2>
  <p>Across the 48 AOS trials: {k['budget_notes']} budget notes and {k['checkpoints']} checkpoints, {k['time_warnings']} trials that reached the 10% time warning, {k['budget_exits']} budget exits, {k['ledger_complete']} of 48 complete ledgers, {k['compactions']} compactions and {k['handoffs']} handoffs. Replay: {aos['replay']['identical']} identical, {aos['replay']['diverged']} diverged, {aos['replay']['refused']} refused.</p>
  {table(["Seq", "Task", "Reward", "Ending", "Agent s", "Calls", "Budget notes", "Checkpoints", "Warning at s", "Budget exit", "Ledger complete", "Compactions", "Replay"], aos_rows, nums=(0, 4, 5, 6, 7, 8))}

  <h2>Data quality</h2>
  <ul>
    <li><b>Contention.</b> The server is shared. For each trial, its request count before and after was compared with the requests the relay saw start inside the trial. Under A3, the surplus is 0 in every trial with saved counts; only seq 29's is unknown (A2). No other client used the model during any trial.</li>
    <li><b>Why A3 was needed.</b> The relay logs a request when its reply ends. A request in flight when a trial ended, because Harbor's timeout killed Terminus-2 or AOS left a call at its budget exit, was counted by the server inside that trial but logged after the trial's closing check, so the frozen collector put it in the next trial's slice, sometimes as a 502 to the departed client. Under the frozen rule that made seqs 16, 31 and 38 infra errors and moved the 10% stop rule within one trial. The cancelled requests and carry-overs are listed here:</li>
  </ul>
  {table(["Seq", "Task", "Arm", "Ending (frozen)", "Ending (A3)", "Cancelled at its end (start)", "Carried in (start)", "Surplus (frozen)", "Surplus (A3)"], a3_rows, nums=(0, 7, 8))}
  <ul>
    <li><b>Sampling.</b> All 2,089 chat completions in the run carried temperature 0.6, top_p 0.95, max_tokens 8192 and reasoning_budget_tokens 2048: 1,109 from AOS and 980 from Terminus-2, by User-Agent.</li>
    <li><b>Backend.</b> The model ID and the server's start time were checked before and after every trial and never changed.</li>
    <li><b>The key stayed out.</b> It appears in none of the {{KEYFILES}} files under the 96 job folders, this folder and the replay copies.</li>
    <li><b>No reruns or replacements.</b> All 96 scheduled trials ran once, in the frozen order.</li>
  </ul>

  <h2>What this run can and cannot show</h2>
  <ul>
    <li>It compares two agents driving the same model with the same sampling. It does not separate the reasons: AOS's tools, prompts, in-container execution and strict time budget all differ from Terminus-2's, by design.</li>
    <li>The tasks are a random draw from the eligible 85, so the result speaks for that population, at this model, on this machine. Four tasks (both arms 0/3) gave no information about the difference.</li>
    <li>Three trials per arm per task leaves wide per-task uncertainty. The per-task table is the evidence; the pooled test is a summary of it.</li>
  </ul>

  <h2>Files</h2>
  <ul>
    <li><code>baseline-tb2/PLAN.md</code>: the frozen plan with amendments A1–A3.</li>
    <li><code>baseline-tb2/results.jsonl</code>: one record per trial, as written during the run; <code>corrections.jsonl</code>: the A3 fields for seqs 1–39.</li>
    <li><code>baseline-tb2/failures.json</code>: the hand-check; <code>replay.jsonl</code>: the replays; <code>analysis.json</code>: everything above.</li>
    <li><code>baseline-tb2/relay.log</code>: the relay's per-request log (no messages, no headers but the User-Agent); <code>logs/backend.jsonl</code>: the backend checks from seq 30 on.</li>
  </ul>

  <h2>Reproduce</h2>
  <p><code>python3 -I reports/terminal-bench/baseline-tb2/a3_corrections.py</code>, then <code>analyze.py</code>, then <code>make_report.py</code>. The job folders are under <code>reports/terminal-bench/jobs/tb2-base-*</code>.</p>
</body>
</html>
"""
open(OUT, "w").write(body.replace("{KEYFILES}", os.environ.get("KEYFILES", "?")))
print("wrote", os.path.relpath(OUT))
