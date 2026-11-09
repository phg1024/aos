"""Analysis of the TB2 baseline, as PLAN.md's "Measures and analysis" fixes it.

Reads results.jsonl (collect.py's records), failures.json (the hand-check of
every failed trial) and replay.jsonl (replay.py), and writes analysis.json.
Stdlib only.

    python3 -I reports/terminal-bench/baseline-tb2/analyze.py
"""

import itertools
import json
import math
import os
import statistics

HERE = os.path.dirname(os.path.abspath(__file__))
ARMS = ["aos", "terminus-2"]
ENDINGS = ["completed", "budget_exit", "harbor_timeout", "infra_error", "agent_crash"]
BUCKETS = ["no_answer", "wrong_answer", "environment", "timeout", "infra"]


def jsonl(name):
    path = os.path.join(HERE, name)
    return [json.loads(line) for line in open(path)] if os.path.isfile(path) else []


def pass_at(n, c, k):
    return 1 - math.comb(n - c, k) / math.comb(n, k) if n >= k else None


def pass_hat(n, c, k):
    return math.comb(c, k) / math.comb(n, k) if n >= k else None


def reward(r):
    return r["reward"] or 0.0  # no reward counts as a failure


def perm_test(rows, tasks, a, b):
    """Exact stratified permutation test of mean reward, arm a minus arm b.
    Labels are permuted within each task; two-sided."""
    strata = []
    for task in tasks:
        ra = [reward(r) for r in rows if r["task"] == task and r["arm"] == a]
        rb = [reward(r) for r in rows if r["task"] == task and r["arm"] == b]
        if ra or rb:
            strata.append((ra, rb))
    na = sum(len(s[0]) for s in strata)
    nb = sum(len(s[1]) for s in strata)
    observed = sum(sum(s[0]) for s in strata) / na - sum(sum(s[1]) for s in strata) / nb
    dist, total = {0.0: 1.0}, 0.0
    for ra, rb in strata:
        pool = ra + rb
        total += sum(pool)
        combos = list(itertools.combinations(range(len(pool)), len(ra)))
        local = {}
        for idx in combos:
            s = sum(pool[i] for i in idx)
            local[s] = local.get(s, 0) + 1 / len(combos)
        new = {}
        for x, px in dist.items():
            for y, py in local.items():
                new[x + y] = new.get(x + y, 0) + px * py
        dist = new
    p = sum(px for sa, px in dist.items() if abs(sa / na - (total - sa) / nb) >= abs(observed) - 1e-12)
    return {"a": a, "b": b, "n_a": na, "n_b": nb, "mean_a": sum(sum(s[0]) for s in strata) / na,
            "mean_b": sum(sum(s[1]) for s in strata) / nb, "diff": observed, "p_two_sided": min(1.0, p)}


def tokens(r):
    u = r.get("usage") or {}
    return [u.get(k) or 0 for k in ("prompt_tokens", "cached_tokens", "completion_tokens")]


def main():
    rows = jsonl("results.jsonl")
    tasks = json.load(open(os.path.join(HERE, "drawn.json")))["drawn"]
    hand_path = os.path.join(HERE, "failures.json")
    hand = {f["seq"]: f for f in json.load(open(hand_path))["trials"]} if os.path.isfile(hand_path) else {}
    replays = {r["seq"]: r for r in jsonl("replay.jsonl")}
    out = {"trials": len(rows), "primary": perm_test(rows, tasks, "aos", "terminus-2")}

    per = {}
    for arm in ARMS:
        per[arm] = {}
        for task in tasks:
            rs = [reward(r) for r in rows if r["arm"] == arm and r["task"] == task]
            n, c = len(rs), int(sum(rs))
            per[arm][task] = {"n": n, "c": c, "pass_at": {k: pass_at(n, c, k) for k in (1, 2, 3)},
                              "pass_hat": {k: pass_hat(n, c, k) for k in (2, 3)}}
    out["per_task"] = per

    out["arms"] = {}
    for arm in ARMS:
        rs = [r for r in rows if r["arm"] == arm]
        agent = [r["agent_s"] for r in rs if r.get("agent_s") is not None]
        full = [t for t in tasks if per[arm][t]["n"] == 3]
        summary = {
            "n": len(rs), "successes": int(sum(reward(r) for r in rs)),
            "pass_at": {k: statistics.mean(per[arm][t]["pass_at"][k] for t in full) if full else None for k in (1, 2, 3)},
            "pass_hat": {k: statistics.mean(per[arm][t]["pass_hat"][k] for t in full) if full else None for k in (2, 3)},
            "endings": {e: sum(1 for r in rs if r["ending"] == e) for e in ENDINGS},
            "agent_s_median": statistics.median(agent) if agent else None, "agent_s_total": sum(agent),
            "trial_s_total": sum(r.get("trial_s") or 0 for r in rs),
            "prompt_tokens": sum(tokens(r)[0] for r in rs), "cached_tokens": sum(tokens(r)[1] for r in rs),
            "completion_tokens": sum(tokens(r)[2] for r in rs),
            "relay_posts": sum((r.get("relay") or {}).get("posts") or 0 for r in rs),
            "contended": sum(1 for r in rs if (r.get("contention") or {}).get("surplus", 0) > 0),
        }
        if arm == "aos":
            c = [r.get("contracts") or {} for r in rs]
            summary["contracts"] = {
                "trials_with_budget_notes": sum(1 for x in c if x.get("budget_notes")),
                "budget_notes": sum(x.get("budget_notes") or 0 for x in c),
                "time_warnings": sum(1 for x in c if x.get("time_warning_s") is not None),
                "budget_exits": sum(1 for x in c if x.get("budget_exit")),
                "checkpoints": sum(x.get("checkpoints") or 0 for x in c),
                "ledger_complete": sum(1 for x in c if x.get("ledger_complete")),
                "compactions": sum(x.get("compactions") or 0 for x in c),
                "handoffs": sum(x.get("handoffs") or 0 for x in c),
            }
            reps = [replays.get(r["seq"]) for r in rs]
            summary["replay"] = {"identical": sum(1 for x in reps if x and x["exit"] == 0),
                                 "diverged": sum(1 for x in reps if x and x["exit"] == 1),
                                 "refused": sum(1 for x in reps if x and x["exit"] == 2),
                                 "not_run": sum(1 for x in reps if not x)}
        failed = [r for r in rs if reward(r) == 0]
        summary["failures"] = {b: sum(1 for r in failed if (hand.get(r["seq"]) or {}).get("bucket") == b)
                               for b in BUCKETS}
        summary["failures"]["unexplained"] = sum(1 for r in failed if r["seq"] not in hand)
        out["arms"][arm] = summary

    out["per_trial"] = [{
        "seq": r["seq"], "round": r["round"], "task": r["task"], "arm": r["arm"], "reward": r["reward"],
        "ending": r["ending"], "agent_s": r.get("agent_s"), "trial_s": r.get("trial_s"),
        "tokens": tokens(r), "calls": r.get("model_calls") or (r.get("relay") or {}).get("posts"),
        "contracts": r.get("contracts"), "replay": (replays.get(r["seq"]) or {}).get("exit"),
        "bucket": (hand.get(r["seq"]) or {}).get("bucket")} for r in rows]
    json.dump(out, open(os.path.join(HERE, "analysis.json"), "w"), indent=1)

    for arm in ARMS:
        a = out["arms"][arm]
        print(f"{arm:10} {a['successes']}/{a['n']} pass@1 {a['pass_at'][1]} pass@3 {a['pass_at'][3]} "
              f"pass^3 {a['pass_hat'][3]} endings {a['endings']} failures {a['failures']}")
    print("primary", out["primary"])


if __name__ == "__main__":
    main()
