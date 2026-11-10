"""Analysis of the pi arm, as PLAN-pi.md's "Measures and analysis" fixes it.

Reads results-pi.jsonl, the baseline's records (results.jsonl with
corrections.jsonl), failures-pi.json and the baseline's AOS ledgers (for the
latency check), and writes analysis-pi.json. Stdlib only.

    python3 -I reports/terminal-bench/baseline-tb2/analyze_pi.py
"""

import json
import os
import statistics
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import analyze  # noqa: E402
import collect  # noqa: E402

JOBS = os.path.join(HERE, "..", "jobs")
SPLIT = ["headless-terminal", "winning-avg-corewars", "protein-assembly"]
ENDINGS = ["completed", "harbor_timeout", "infra_error", "agent_crash"]
BUCKETS = analyze.BUCKETS


def reward(r):
    return r["reward"] or 0.0


def holm(tests):
    """Holm's step-down adjustment over the named p-values, family alpha 0.05."""
    order = sorted(tests, key=lambda k: tests[k])
    out, running, stop = {}, 0.0, False
    for i, name in enumerate(order):
        adj = min(1.0, (len(order) - i) * tests[name])
        running = max(running, adj)
        level = 0.05 / (len(order) - i)
        reject = not stop and tests[name] < level
        stop = stop or not reject
        out[name] = {"p": tests[name], "p_holm": running, "level": level, "reject": reject}
    return out


def quantiles(xs):
    xs = sorted(xs)
    if not xs:
        return {"n": 0}
    return {"n": len(xs), "median": statistics.median(xs), "p90": xs[int(0.9 * (len(xs) - 1))], "max": xs[-1]}


def aos_latency(rows):
    """The baseline AOS arm's per-call latency, from each trial's ledger."""
    sched = {t["seq"]: t["job"] for t in json.load(open(os.path.join(HERE, "schedule.json")))["trials"]}
    out = []
    for r in rows:
        if r["arm"] != "aos" or not r.get("trial"):
            continue
        ledger = os.path.join(JOBS, sched[r["seq"]], r["trial"], "agent", "aos-state", "run", "calls.jsonl")
        out += [o["latency_ms"] / 1000 for o in collect.jsonl(ledger)
                if o.get("phase") == "outcome" and o.get("latency_ms")]
    return out


def main():
    tasks = json.load(open(os.path.join(HERE, "drawn.json")))["drawn"]
    pi = collect.jsonl(os.path.join(HERE, "results-pi.jsonl"))
    base = collect.corrected_rows(collect.jsonl(os.path.join(HERE, "results.jsonl")))
    rows = base + pi
    hand_path = os.path.join(HERE, "failures-pi.json")
    hand = {f["seq"]: f for f in json.load(open(hand_path))["trials"]} if os.path.isfile(hand_path) else {}

    primary = {"P1": analyze.perm_test(rows, tasks, "pi", "terminus-2"),
               "P2": analyze.perm_test(rows, tasks, "pi", "aos")}
    adjusted = holm({k: v["p_two_sided"] for k, v in primary.items()})
    for k in primary:
        primary[k].update(adjusted[k])
    out = {"trials": len(pi), "primary": primary}

    out["split_tasks"] = {arm: analyze.perm_test(rows, SPLIT, "pi", arm) for arm in ("terminus-2", "aos")}
    out["split_tasks"]["successes"] = {arm: int(sum(reward(r) for r in rows if r["arm"] == arm and r["task"] in SPLIT))
                                       for arm in ("pi", "aos", "terminus-2")}

    per = {}
    for task in tasks:
        rs = [reward(r) for r in pi if r["task"] == task]
        n, c = len(rs), int(sum(rs))
        per[task] = {"n": n, "c": c, "pass_at": {k: analyze.pass_at(n, c, k) for k in (1, 2, 3)},
                     "pass_hat": {k: analyze.pass_hat(n, c, k) for k in (2, 3)},
                     "aos": int(sum(reward(r) for r in base if r["arm"] == "aos" and r["task"] == task)),
                     "terminus-2": int(sum(reward(r) for r in base if r["arm"] == "terminus-2" and r["task"] == task))}
    out["per_task"] = per
    full = [t for t in tasks if per[t]["n"] == 3]
    agent = [r["agent_s"] for r in pi if r.get("agent_s") is not None]
    att = [r.get("attribution") or {} for r in pi]
    failed = [r for r in pi if reward(r) == 0]
    other = {}
    for r in pi:
        for k, v in (r.get("other_entries") or {}).items():
            other[k] = other.get(k, 0) + v
    tools = {}
    for r in pi:
        for k, v in (r.get("tool_calls") or {}).items():
            tools[k] = tools.get(k, 0) + v
    out["pi"] = {
        "n": len(pi), "successes": int(sum(reward(r) for r in pi)),
        "pass_at": {k: statistics.mean(per[t]["pass_at"][k] for t in full) if full else None for k in (1, 2, 3)},
        "pass_hat": {k: statistics.mean(per[t]["pass_hat"][k] for t in full) if full else None for k in (2, 3)},
        "endings": {e: sum(1 for r in pi if r["ending"] == e) for e in ENDINGS},
        "passed_by_ending": {e: sum(1 for r in pi if r["ending"] == e and reward(r) == 1) for e in ENDINGS},
        "agent_s_median": statistics.median(agent) if agent else None, "agent_s_total": sum(agent),
        "trial_s_total": sum(r.get("trial_s") or 0 for r in pi),
        "prompt_tokens": sum((r.get("usage") or {}).get("prompt_tokens") or 0 for r in pi),
        "cached_tokens": sum((r.get("usage") or {}).get("cached_tokens") or 0 for r in pi),
        "completion_tokens": sum((r.get("usage") or {}).get("completion_tokens") or 0 for r in pi),
        "model_calls": sum(r.get("model_calls") or 0 for r in pi),
        "relay_posts_started": sum(a.get("posts_started") or 0 for a in att),
        "retries_or_cancelled": sum(r.get("retries_or_cancelled") or 0 for r in pi),
        "cancelled_at_end": sum(len(a.get("cancelled_at_end") or []) for a in att),
        "own_502": sum(len(a.get("own_502_list") or []) for a in att),
        "unrecovered_502": sum(a.get("unrecovered_502") or 0 for a in att),
        "contended": sum(1 for r in pi if (r.get("contention") or {}).get("surplus", 0) > 0),
        "contention_unknown": sum(1 for r in pi if "error" in (r.get("contention") or {"error": 1})),
        "usage_source": {s: sum(1 for r in pi if r.get("usage_source") == s) for s in ("session", "output", None)},
        "other_session_entries": other, "tool_calls": tools,
        "tool_errors": sum(r.get("tool_errors") or 0 for r in pi),
        "failures": {b: sum(1 for r in failed if (hand.get(r["seq"]) or {}).get("bucket") == b) for b in BUCKETS},
        "unexplained": sum(1 for r in failed if r["seq"] not in hand),
    }
    out["baseline_failures"] = {arm: analyze_failures(arm) for arm in ("aos", "terminus-2")}
    out["latency_s"] = {"pi": quantiles([x for r in pi for x in (r.get("latency_s") or {}).get("all") or []]),
                        "aos_baseline": quantiles(aos_latency(base))}
    out["per_trial"] = [{
        "seq": r["seq"], "round": r["round"], "task": r["task"], "reward": r["reward"], "ending": r["ending"],
        "agent_s": r.get("agent_s"), "calls": r.get("model_calls"),
        "posts": (r.get("attribution") or {}).get("posts_started"), "pi_exit": r.get("pi_exit"),
        "usage": r.get("usage"), "usage_source": r.get("usage_source"),
        "other_entries": r.get("other_entries"), "contention": r.get("contention"),
        "cancelled_at_end": (r.get("attribution") or {}).get("cancelled_at_end"),
        "own_502": (r.get("attribution") or {}).get("own_502_list"),
        "bucket": (hand.get(r["seq"]) or {}).get("bucket")} for r in pi]
    json.dump(out, open(os.path.join(HERE, "analysis-pi.json"), "w"), indent=1)

    p = out["pi"]
    print(f"pi {p['successes']}/{p['n']} pass@1 {p['pass_at'][1]} endings {p['endings']} failures {p['failures']}")
    for k, v in primary.items():
        print(k, f"{v['a']} - {v['b']}: {v['diff']:+.4f} p {v['p']:.4f} holm {v['p_holm']:.4f} reject {v['reject']}")
    print("split tasks", out["split_tasks"]["successes"])


def analyze_failures(arm):
    data = json.load(open(os.path.join(HERE, "failures.json")))["trials"]
    return {b: sum(1 for f in data if f["arm"] == arm and f["bucket"] == b) for b in BUCKETS}


if __name__ == "__main__":
    main()
