"""Writes reports/terminal-bench/tb2-pi.html from analysis-pi.json,
postkill-pi.json, failures-pi.json, results-pi.jsonl, the pi session files
and the baseline's analysis.json. It only presents: every number in a table
is read from the data, and the narrative is in this file. Stdlib only.

    python3 -I reports/terminal-bench/baseline-tb2/analyze_pi.py
    python3 -I reports/terminal-bench/baseline-tb2/postkill_pi.py
    python3 -I reports/terminal-bench/baseline-tb2/make_report_pi.py
"""

import datetime
import glob
import html
import json
import os
import sys
import zoneinfo

HERE = os.path.dirname(os.path.abspath(__file__))
OUT = os.path.join(HERE, "..", "tb2-pi.html")
JOBS = os.path.join(HERE, "..", "jobs")
sys.path.insert(0, HERE)
import collect  # noqa: E402

A = json.load(open(os.path.join(HERE, "analysis-pi.json")))
B = json.load(open(os.path.join(HERE, "analysis.json")))
K = json.load(open(os.path.join(HERE, "postkill-pi.json")))
FJ = json.load(open(os.path.join(HERE, "failures-pi.json")))
F = {f["seq"]: f for f in FJ["trials"]}
PI = collect.jsonl(os.path.join(HERE, "results-pi.jsonl"))
SCHED = {t["seq"]: t["job"] for t in json.load(open(os.path.join(HERE, "schedule-pi.json")))["trials"]}
TASKS = json.load(open(os.path.join(HERE, "drawn.json")))["drawn"]
BUCKETS = ["no_answer", "wrong_answer", "environment", "timeout", "infra"]
ENDINGS = ["completed", "budget_exit", "harbor_timeout", "infra_error", "agent_crash"]
PST = zoneinfo.ZoneInfo("America/Los_Angeles")
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


def ts(s):
    return datetime.datetime.fromisoformat(s.replace("Z", "+00:00"))


def result(seq):
    path = glob.glob(os.path.join(JOBS, SCHED[seq], "*", "result.json"))[0]
    return path, json.load(open(path))


def open_tool(seq):
    """The tool call still running at the agent's end: (start offset, seconds to the end)."""
    path, res = result(seq)
    a0, a1 = ts(res["agent_execution"]["started_at"]), ts(res["agent_execution"]["finished_at"])
    calls, done = [], 0
    for f in glob.glob(os.path.join(os.path.dirname(path), "agent", "pi", "sessions", "*.jsonl")):
        for line in open(f):
            try:
                x = json.loads(line)
            except ValueError:
                continue
            m = x.get("message") or {}
            if m.get("role") == "assistant":
                calls += [ts(x["timestamp"]) for c in m.get("content") or [] if c.get("type") == "toolCall"]
            done += m.get("role") == "toolResult"
    if len(calls) > done:
        return round((calls[-1] - a0).total_seconds()), round((a1 - calls[-1]).total_seconds())
    return None


P1, P2 = A["primary"]["P1"], A["primary"]["P2"]
S1, S2 = K["sensitivity"]["P1"], K["sensitivity"]["P2"]
p = A["pi"]
aos, t2 = B["arms"]["aos"], B["arms"]["terminus-2"]
per = A["per_task"]
first = min(ts(result(r["seq"])[1]["started_at"]) for r in PI).astimezone(PST)
last = max(ts(result(r["seq"])[1]["finished_at"]) for r in PI).astimezone(PST)

# Reward
pass_rows = [[e(name), f"{a['successes']}/48", pct(a['pass_at']['1']), pct(a['pass_at']['2']), pct(a['pass_at']['3']),
              pct(a['pass_hat']['2']), pct(a['pass_hat']['3'])]
             for name, a in (("pi", p), ("aos (baseline)", aos), ("terminus-2 (baseline)", t2))]
task_rows = []
for t in TASKS:
    c = per[t]
    task_rows.append([e(t), f"{c['c']}/3", f"{c['aos']}/3", f"{c['terminus-2']}/3",
                      f"{c['c'] - c['terminus-2']:+d}", f"{c['c'] - c['aos']:+d}"])
better_t2 = [t for t in TASKS if per[t]["c"] > per[t]["terminus-2"]]
worse_t2 = [t for t in TASKS if per[t]["c"] < per[t]["terminus-2"]]
worse_aos = [t for t in TASKS if per[t]["c"] < per[t]["aos"]]
better_aos = [t for t in TASKS if per[t]["c"] > per[t]["aos"]]
test_rows = []
for name, m, s in (("P1: pi − terminus-2", P1, S1), ("P2: pi − aos", P2, S2)):
    test_rows.append([e(name), f"{m['mean_a']:.4f}", f"{m['mean_b']:.4f}", f"{100 * m['diff']:+.1f}",
                      f"{m['p']:.4f}", f"{m['p_holm']:.4f}", f"{m['level']:.3f}", "yes" if m["reject"] else "no",
                      f"{100 * s['diff']:+.1f}", f"{s['p']:.4f}", f"{s['p_holm']:.4f}", "yes" if s["reject"] else "no"])
sp = A["split_tasks"]

# Endings
ending_rows = [["pi"] + [str(p["endings"].get(k, 0)) if k in p["endings"] else "—" for k in ENDINGS]]
for name, a in (("aos (baseline, A3)", aos), ("terminus-2 (baseline, A3)", t2)):
    ending_rows.append([e(name)] + [str(a["endings_corrected"][k]) for k in ENDINGS])

# Failures
fail_counts = [["pi"] + [str(p["failures"][b]) for b in BUCKETS] + [str(p["unexplained"])]]
for name, a in (("aos (baseline)", aos), ("terminus-2 (baseline)", t2)):
    fail_counts.append([e(name)] + [str(a["failures"][b]) for b in BUCKETS] + [str(a["failures"].get("unexplained", 0))])
strict = sorted(s for s, f in F.items() if f["other"] == "wrong_answer")
on_disk = sorted(s for s, f in F.items() if f["bucket"] == "wrong_answer" or f["other"] == "wrong_answer")
fail_rows = []
for r in PI:
    f = F.get(r["seq"])
    if f:
        fail_rows.append([str(r["seq"]), e(r["task"]), e(r["ending"]), f"<b>{e(f['bucket'])}</b>", e(f["reason"]),
                          e(f["other"] or "")])
hung = []
for r in PI:
    if r["ending"] == "harbor_timeout":
        o = open_tool(r["seq"])
        if o and o[1] >= 300:
            hung.append((r["seq"], r["task"], o[0], o[1], r["reward"]))
hung_rows = [[str(s), e(t), num(a), num(d), reward_cell(rw)] for s, t, a, d, rw in hung]

# Post-kill
kill_rows = []
for t in K["pi"]:
    lp = t["late_posts"]
    kill_rows.append([str(t["seq"]), e(t["task"]), reward_cell(t["reward"]),
                      f"+{t['verifier_s'][0]}–{t['verifier_s'][1]}" if t["verifier_s"] else "—",
                      str(t["session_messages"]), str(len(lp)),
                      "<br>".join(f"+{x['offset_s']} s, {x['status']}, messages={x['messages']}" +
                                  (", in verifier" if x["in_verifier"] else ", after verifier")
                                  for x in lp) or "<span class='muted'>none</span>"])
active = K["pi_trials_with_late_posts"]
flipped = K["sensitivity"]["flipped"]
bt = K["baseline"]["terminus-2"]
seq11, seq19, seq40 = (next(t for t in K["pi"] if t["seq"] == s) for s in (11, 19, 40))

# Cost and per-trial
cost_rows = [["pi", num(p["agent_s_median"]), num(p["agent_s_total"] / 3600, 1), num(p["model_calls"]),
              num(p["prompt_tokens"]), num(p["cached_tokens"]), num(p["completion_tokens"])]]
for name, a, calls in (("aos (baseline)", aos, aos.get("relay_posts")), ("terminus-2 (baseline)", t2, t2.get("relay_posts"))):
    cost_rows.append([e(name), num(a["agent_s_median"]), num(a["agent_s_total"] / 3600, 1), num(calls),
                      num(a["prompt_tokens"]), num(a["cached_tokens"]), num(a["completion_tokens"])])
lat = A["latency_s"]
lat_rows = [[e(n), num(lat[k]["n"]), num(lat[k]["median"], 1), num(lat[k]["p90"], 1), num(lat[k]["max"], 1)]
            for n, k in (("pi (relay-side, per call)", "pi"), ("aos (baseline ledger)", "aos_baseline"))]
trial_rows = []
for r in A["per_trial"]:
    u = r["usage"] or {}
    trial_rows.append([str(r["seq"]), str(r["round"]), e(r["task"]), reward_cell(r["reward"]), e(r["ending"]),
                       num(r["agent_s"]), num(r["calls"]), num(r["posts"]), e(str(r["pi_exit"])),
                       num(u.get("prompt_tokens")), num(u.get("completion_tokens")),
                       str(len(r["cancelled_at_end"] or [])), str(len(r["own_502"] or [])), e(r["bucket"] or "")])

body = f"""<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <title>Terminal-Bench 2 pi Arm</title>
  <style>
    body {{ font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; margin: 40px auto; max-width: 1040px; line-height: 1.5; color: #1f2937; padding: 0 16px; }}
    code {{ background: #f3f4f6; padding: 2px 5px; border-radius: 4px; }}
    .hero {{ background: linear-gradient(135deg, #ecfdf5, #eff6ff); padding: 24px; border-radius: 16px; }}
    .note {{ background: #fefce8; border-left: 4px solid #ca8a04; padding: 10px 14px; }}
    .warn {{ background: #fef2f2; border-left: 4px solid #b91c1c; padding: 10px 14px; }}
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
    <h1>Terminal-Bench 2.0: pi as a third arm</h1>
    <p><b>On the baseline's 16 tasks, with the same model and sampling, pi solved {p['successes']} of 48 trials: the same count as Terminus-2 ({t2['successes']}/48) and {aos['successes'] - p['successes']} fewer than AOS ({aos['successes']}/48).</b></p>
    <p><b>Neither frozen comparison is significant at family α = 0.05.</b> P1 (pi − Terminus-2) is {100 * P1['diff']:+.1f} points, p = {P1['p']:.4f}. P2 (pi − AOS) is {100 * P2['diff']:+.1f} points, raw p = {P2['p']:.4f}; it is tested first, at Holm's 0.025, and not rejected (Holm-adjusted p = {P2['p_holm']:.4f}). The plan's power table gives under 50% power for a 15-point difference, so this does not show that pi equals either arm.</p>
    <p><b>The P2 result is fragile.</b> pi kept running after Harbor's timeout in {len(active)} of its {len(K['pi'])} timed-out trials, and {len(flipped)} of those passed. Counted as failures, as a labelled sensitivity check, P2 becomes {100 * S2['diff']:+.1f} points, p = {S2['p']:.4f} (Holm {S2['p_holm']:.4f}), which would be rejected. The measured result above is the primary one; see "pi after Harbor's timeout".</p>
    <p><b>Most of pi's failures were timeouts.</b> {p['endings']['harbor_timeout']} of 48 pi trials ran into Harbor's timeout (Terminus-2: {t2['endings_corrected']['harbor_timeout']}; AOS: {aos['endings_corrected']['harbor_timeout']}), and {p['failures']['timeout']} of its {48 - p['successes']} failures are timeouts.</p>
  </div>

  <p class="note"><b>Scope.</b> One model ({e('qwen3.8-flash-next-nvfp4-nvfp4')}, served by Strata on a shared GPU server), the baseline's 16 tasks, 3 pi trials per task. The comparators are the baseline's own 48 trials per arm, run just before this one against the same server, engine and sampling; they were not rerun. pi runs with its defaults apart from the settings needed to reach the model and match sampling.</p>

  <h2>What was run</h2>
  <ul>
    <li><b>Plan.</b> <code>reports/terminal-bench/baseline-tb2/PLAN-pi.md</code>, frozen at 35c5f70 (Option A: pi only, 3 rounds). One uncounted smoke trial passed every check (533aa2d). There were no amendments, pauses, reruns or replacements: all 48 scheduled trials ran once, in the frozen order (seed 20261120).</li>
    <li><b>Arm.</b> pi v1.5.0 (release tarball pinned by hash) through <code>bench/harbor/pi_agent.py</code>. Like Terminus-2, pi has no deadline of its own: only Harbor's timeout ends it.</li>
    <li><b>Sampling.</b> All {num(p['relay_posts_started'])} chat completions of the counted trials, and the smoke's, carried temperature 0.6, top_p 0.95, max_tokens 8192, reasoning_budget_tokens 2048 and stream true, with pi's User-Agent. The relay does not log <code>store</code>; its absence rests on the container check (42dbed7).</li>
    <li><b>Harness.</b> Harbor 0.25.0, one trial at a time, Docker Desktop with Rosetta, as in the baseline.</li>
    <li><b>Time.</b> The first trial started {first:%Y-%m-%d %H:%M} PST and the last ended {last:%Y-%m-%d %H:%M} PST.</li>
  </ul>

  <h2>Reward</h2>
  {table(["Arm", "Solved", "pass@1", "pass@2", "pass@3", "pass^2", "pass^3"], pass_rows, nums=(1, 2, 3, 4, 5, 6))}
  <p class="muted">pass@k and pass^k are computed per task from c successes in n = 3 and averaged over the 16 tasks.</p>
  {table(["Task", "pi", "aos", "terminus-2", "pi − terminus-2", "pi − aos"], task_rows, nums=(1, 2, 3, 4, 5))}
  <ul>
    <li><b>Against Terminus-2:</b> pi solved more trials on {len(better_t2)} tasks ({e(', '.join(better_t2))}) and fewer on {len(worse_t2)} ({e(', '.join(worse_t2))}).</li>
    <li><b>Against AOS:</b> pi solved fewer on {len(worse_aos)} tasks ({e(', '.join(worse_aos))}) and more on {len(better_aos)} ({e(', '.join(better_aos)) or 'none'}).</li>
  </ul>
  <h3>Primary tests</h3>
  {table(["Comparison", "pi mean", "Comparator mean", "Diff (pts)", "p", "p (Holm)", "Holm level", "Rejected", "Sensitivity diff", "Sensitivity p", "Sensitivity p (Holm)", "Sensitivity rejected"], test_rows, nums=(1, 2, 3, 4, 5, 6, 8, 9, 10))}
  <p class="muted">Exact stratified permutation test: labels permuted within each task's 6 trials (3 pi, 3 comparator), two-sided, p computed exactly by convolution; Holm at family α = 0.05. The sensitivity columns count pi seqs {e(', '.join(map(str, flipped)))} as failures (see "pi after Harbor's timeout"); they are not the result.</p>
  <h3>Secondary: the three split tasks</h3>
  <p>On headless-terminal, winning-avg-corewars and protein-assembly, pi solved {sp['successes']['pi']} of 9, against AOS {sp['successes']['aos']}/9 and Terminus-2 {sp['successes']['terminus-2']}/9. Exact stratified p on these three tasks: pi − Terminus-2 {100 * sp['terminus-2']['diff']:+.1f} points, p = {sp['terminus-2']['p_two_sided']:.4f}; pi − AOS {100 * sp['aos']['diff']:+.1f} points, p = {sp['aos']['p_two_sided']:.4f}. These tasks were chosen from the baseline's results, so the comparators' numbers there are selected and pi's are not; the p-values are not part of the family.</p>

  <h2>Endings</h2>
  {table(["Arm"] + ENDINGS, ending_rows, nums=(1, 2, 3, 4, 5))}
  <ul>
    <li><b>pi:</b> {p['passed_by_ending']['completed']} of {p['endings']['completed']} completed trials passed, and {p['passed_by_ending']['harbor_timeout']} of {p['endings']['harbor_timeout']} trials ended by Harbor's timeout passed: the verifier still runs, and a trial passes if the answer was in place.</li>
    <li>No infra errors and no agent crashes, so the stop rule never fired. pi has no budget exit; that column is AOS's.</li>
  </ul>

  <h2>Why each failed trial failed</h2>
  <p>Every failed pi trial was read from its session file, <code>pi.txt</code> and the verifier's output, and given one bucket by the baseline's definitions and tie rule.</p>
  {table(["Arm"] + BUCKETS + ["unexplained"], fail_counts, nums=(1, 2, 3, 4, 5, 6))}
  <ul>
    <li><b>How the tie rule was applied.</b> {e(FJ['rule'])}</li>
    <li><b>An answer on disk.</b> {len(on_disk)} of pi's {48 - p['successes']} failed trials had a written answer that a test failed on (seqs {e(', '.join(map(str, on_disk)))}); the rest never wrote one. No failed trial was seen to have had a passing answer and lost it, as Terminus-2 did in its seq 30; this is from the traces only, as intermediate answers were not run against the tests.</li>
    <li><b>Two exits without an answer.</b> In gpt2-codegolf seqs 30 and 47, pi exited by itself after a model reply that held only reasoning, ending mid-sentence with stop reason "stop", and no text or tool call. pi treats such a reply as the end of its turn.</li>
    <li><b>polyglot-c-py.</b> pi solved it once (seq 34), the first pass on this task in any arm. Its two failures were the baseline's: a compiled <code>cmain</code> left in <code>/app/polyglot</code>.</li>
    <li><b>Long tool calls at the timeout.</b> In {len(hung)} timed-out trials, pi's last tool call had been running for 5 minutes or more when Harbor stopped it. pi's bash tool has no time limit of its own, so a command that does not return holds the agent until Harbor's timeout:</li>
  </ul>
  {table(["Seq", "Task", "Call started (s after agent start)", "Running at the timeout (s)", "Reward"], hung_rows, nums=(0, 2, 3))}
  {table(["Seq", "Task", "Ending", "Bucket", "Reason, from the trace", "Also"], fail_rows)}

  <h2>pi after Harbor's timeout</h2>
  <p class="warn"><b>Harbor's timeout stops waiting for pi; it does not stop the pi process in the container.</b> In {len(active)} of pi's {len(K['pi'])} timed-out trials (seqs {e(', '.join(map(str, active)))}), the relay logged {K['pi_late_posts']} chat completions that started after the agent's end, {K['pi_late_posts_in_verifier']} of them inside the verifier's window, which runs in the same container. Each carries one or two more messages than the session saved at the kill, so in each case a tool finished after the kill before the request was sent. The plan disclosed the child-process form of this as untested; this is the agent itself still acting.</p>
  {table(["Seq", "Task", "Reward", "Verifier (s after agent end)", "Messages in the saved session", "Late POSTs", "Each: start after agent end, status, messages"], kill_rows, nums=(0, 4, 5))}
  <ul>
    <li><b>How to read it.</b> The relay logs each request's start time, written when its reply ends. The saved session file is a copy taken at the agent's end, so a request carrying more messages than the saved session shows activity after the kill: two more is a reply and a tool result, one more is the result of a tool that was running at the kill. What those tools did is not recorded anywhere: the session copy stops at the kill and the relay logs no bodies. Every late request that failed ended with a 502 when the container stopped; these are all {p['own_502']} of pi's 502s, so none is an infra error.</li>
    <li><b>The baseline does not have this.</b> In the baseline's relay log, the {bt['harbor_timeout_trials']} Terminus-2 trials that ended by Harbor's timeout have {bt['late_posts']} chat completions that started after the agent's end. Terminus-2 runs in Harbor's own process and stops when cancelled; AOS never reached Harbor's timeout.</li>
    <li><b>The direction of any effect is unknown.</b> A late action could have made a pass (an answer finished or fixed during the verifier) or a failure (a working answer changed or a service disturbed while the tests ran). The second case cannot be detected from the data: {len(active) - len(flipped)} of the late-active trials failed, and any of them might have passed without the late actions. For seq 40, the answer that passed (<code>/app/plaintexts.txt</code>) was written and checked against the reference binary at +1772–1780 s, before the kill; seq 13's last saved test run, at +891 s, raised an error in pi's own test script, and what pi changed afterwards is unknown.</li>
    <li><b>Sensitivity.</b> Counting the {len(flipped)} late-active passes (seqs {e(', '.join(map(str, flipped)))}) as failures gives pi {K['sensitivity']['successes']}/48. Through the same exact test and Holm step: P1 {100 * S1['diff']:+.1f} points, p = {S1['p']:.4f}; P2 {100 * S2['diff']:+.1f} points, p = {S2['p']:.4f}, Holm {S2['p_holm']:.4f}, rejected. This is a bound in one direction only, not a corrected result.</li>
    <li><b>Usage.</b> Token counts come from the saved session, so they miss the late calls.</li>
    <li><b>Long verifiers.</b> Seq 11's verifier ran from +{seq11['verifier_s'][0]} to +{seq11['verifier_s'][1]} s after the agent's end, although pytest reports 5.4 s, and seq 19's from +{seq19['verifier_s'][0]} to +{seq19['verifier_s'][1]} s; no chat completion started in either window. Both are feal-linear-cryptanalysis trials killed while pi's key search was still running (see the long tool calls above); seq 40, whose search had finished, took {seq40['verifier_s'][1]} s. That fits the search holding the CPU during verification, but it was not confirmed.</li>
    <li><b>For future runs (a recommendation only; the harness was not changed during this run):</b> stop the pi process, and its children, in the container before the verifier starts, and then check the relay for requests after the agent's end. The same check would settle the child-process question for every arm.</li>
  </ul>

  <h2>Cost and latency</h2>
  {table(["Arm", "Median agent s", "Total agent h", "Model calls", "Prompt tokens", "Cached tokens", "Completion tokens"], cost_rows, nums=(1, 2, 3, 4, 5, 6))}
  <p class="muted">pi's calls are assistant messages in its saved sessions ({num(p['model_calls'])}); the relay saw {num(p['relay_posts_started'])} start in pi's trial windows, the difference being the requests in flight at a kill and the late ones above. pi's tokens are from its session files (all 48 had one), AOS's from its ledger and Terminus-2's from Harbor. pi's tool calls: {e(', '.join(f'{k} {v}' for k, v in sorted(p['tool_calls'].items(), key=lambda kv: -kv[1])))}; {p['tool_errors']} returned an error.</p>
  {table(["Per-call latency", "Calls", "Median s", "p90 s", "Max s"], lat_rows, nums=(1, 2, 3, 4))}
  <p class="muted">pi's latency is per streamed call from its session; AOS's is from its ledger. Both are dominated by the same server and model; the difference reflects prompt and output sizes as much as the agents.</p>

  <h2>Data quality</h2>
  <ul>
    <li><b>Contention.</b> For each trial, the server's request count before and after was compared with the requests the relay saw start inside it (A3 attribution). The surplus is 0 in all 48 trials; no other client used the model during any trial.</li>
    <li><b>Backend.</b> The model ID, the engine (0.1.54) and the server's start time were checked before and after every trial, the smoke included, and never changed. They match the baseline's.</li>
    <li><b>Requests cut off at the kill.</b> {p['cancelled_at_end']} trials had a request in flight at Harbor's timeout, logged after the closing check; they are recorded as cancelled, as A3 defines.</li>
    <li><b>The key stayed out.</b> It appears in none of the {{KEYFILES}} files under the pi job folders, the baseline folder and the pi release cache.</li>
  </ul>

  <h2>What this run can and cannot show</h2>
  <ul>
    <li>It compares pi with the baseline's two arms on the same model, sampling, tasks and machine, one day apart. It does not separate pi's prompt, tools, context handling and lack of a deadline from each other.</li>
    <li>The comparators were not rerun. Server load, network for tasks that fetch data, and anything else that changed overnight are outside the comparison; the backend checks show the model server itself did not.</li>
    <li>With 3 trials per task the per-task numbers are wide. The primary tests are a summary of the per-task table, and they are underpowered for a 15-point difference.</li>
    <li>The post-kill activity above is a known flaw in how this run ended pi, disclosed rather than corrected.</li>
  </ul>

  <h2>Per trial</h2>
  {table(["Seq", "Round", "Task", "Reward", "Ending", "Agent s", "Calls", "POSTs", "pi_exit", "Prompt tokens", "Completion tokens", "Cancelled", "502s", "Bucket"], trial_rows, nums=(0, 1, 5, 6, 7, 9, 10, 11, 12))}

  <h2>Files</h2>
  <ul>
    <li><code>baseline-tb2/PLAN-pi.md</code>: the frozen plan.</li>
    <li><code>baseline-tb2/results-pi.jsonl</code>: one record per trial, as written during the run; <code>smoke-pi.jsonl</code>: the smoke.</li>
    <li><code>baseline-tb2/failures-pi.json</code>: the hand-check; <code>analysis-pi.json</code>: the frozen analysis; <code>postkill-pi.json</code> (from <code>postkill_pi.py</code>, added after round 1): the late-request table and the sensitivity check.</li>
    <li><code>baseline-tb2/relay-pi.log</code>: the relay's per-request log (no messages, no headers but the User-Agent); <code>logs/backend-pi.jsonl</code>: the backend checks.</li>
  </ul>

  <h2>Reproduce</h2>
  <p><code>python3 -I reports/terminal-bench/baseline-tb2/analyze_pi.py</code>, then <code>postkill_pi.py</code>, then <code>make_report_pi.py</code>. The job folders are under <code>reports/terminal-bench/jobs/tb2-pi-*</code>.</p>
</body>
</html>
"""
open(OUT, "w").write(body.replace("{KEYFILES}", os.environ.get("KEYFILES", "?")))
print("wrote", os.path.relpath(OUT))
