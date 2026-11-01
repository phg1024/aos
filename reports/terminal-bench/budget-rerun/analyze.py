"""Analysis of the budget rerun, as PLAN.md's "Measures and analysis" fixes it.

Reads results.jsonl (collect.py's records) and handcheck.json (the hand-check
of every failed trial) and writes analysis.json. Stdlib only.

    python3 -I reports/terminal-bench/budget-rerun/analyze.py
"""

import itertools
import json
import math
import os
import statistics

HERE = os.path.dirname(os.path.abspath(__file__))
ARMS = ["off", "strict", "advisory"]
TASKS = ["fix-git", "prove-plus-comm", "cobol-modernization", "openssl-selfsigned-cert",
         "nginx-request-logging", "polyglot-c-py", "vulnerable-secret", "raman-fitting"]
ENDINGS = ["completed", "budget_exit", "harbor_timeout", "infra_error", "agent_crash"]


def load():
    rows = [json.loads(line) for line in open(os.path.join(HERE, "results.jsonl"))]
    hand = {t["seq"]: t for t in json.load(open(os.path.join(HERE, "handcheck.json")))["trials"]}
    return rows, hand


def pass_at(n, c, k):
    return 1 - math.comb(n - c, k) / math.comb(n, k)


def pass_hat(n, c, k):
    return math.comb(c, k) / math.comb(n, k)


def perm_test(rows, a, b, exclude=()):
    """Exact stratified permutation test of mean reward, arm a minus arm b.
    Labels are permuted within each task; two-sided."""
    strata = []
    for task in TASKS:
        ra = [r["reward"] for r in rows if r["task"] == task and r["arm"] == a and r["seq"] not in exclude]
        rb = [r["reward"] for r in rows if r["task"] == task and r["arm"] == b and r["seq"] not in exclude]
        strata.append((ra, rb))
    na = sum(len(s[0]) for s in strata)
    nb = sum(len(s[1]) for s in strata)
    observed = sum(sum(s[0]) for s in strata) / na - sum(sum(s[1]) for s in strata) / nb
    # Distribution of the a-arm reward sum: convolve each task's distribution.
    dist = {0.0: 1.0}
    total = 0.0
    for ra, rb in strata:
        pool = ra + rb
        total += sum(pool)
        local = {}
        combos = list(itertools.combinations(range(len(pool)), len(ra)))
        for idx in combos:
            s = sum(pool[i] for i in idx)
            local[s] = local.get(s, 0) + 1 / len(combos)
        new = {}
        for x, px in dist.items():
            for y, py in local.items():
                new[x + y] = new.get(x + y, 0) + px * py
        dist = new
    p = sum(px for sa, px in dist.items()
            if abs(sa / na - (total - sa) / nb) >= abs(observed) - 1e-12)
    return {"a": a, "b": b, "n_a": na, "n_b": nb, "diff": observed, "p_two_sided": min(1.0, p)}


def fisher_two_sided(a, b, c, d):
    """Fisher's exact test for [[a, b], [c, d]], two-sided."""
    n = a + b + c + d
    r1, c1 = a + b, a + c

    def prob(x):
        return math.comb(c1, x) * math.comb(n - c1, r1 - x) / math.comb(n, r1)

    p0 = prob(a)
    lo, hi = max(0, r1 + c1 - n), min(r1, c1)
    return min(1.0, sum(prob(x) for x in range(lo, hi + 1) if prob(x) <= p0 + 1e-12))


def tokens(r):
    u = r.get("usage") or {}
    return u.get("prompt_tokens") or 0, u.get("cached_tokens") or 0, u.get("completion_tokens") or 0


def main():
    rows, hand = load()
    out = {"trials": len(rows)}

    # Reward by arm and task, pass@k and pass^k.
    per = {}
    for arm in ARMS:
        per[arm] = {}
        for task in TASKS:
            rs = [r["reward"] for r in rows if r["arm"] == arm and r["task"] == task]
            n, c = len(rs), int(sum(rs))
            per[arm][task] = {"n": n, "c": c,
                              "pass_at": {k: pass_at(n, c, k) for k in (1, 2, 3)},
                              "pass_hat": {k: pass_hat(n, c, k) for k in (1, 2, 3)}}
    out["per_task"] = per
    out["arms"] = {}
    for arm in ARMS:
        rs = [r for r in rows if r["arm"] == arm]
        agent = [r["agent_s"] for r in rs]
        p, cached, comp = (sum(tokens(r)[i] for r in rs) for i in range(3))
        out["arms"][arm] = {
            "n": len(rs), "successes": int(sum(r["reward"] for r in rs)),
            "pass_at": {k: statistics.mean(per[arm][t]["pass_at"][k] for t in TASKS) for k in (1, 2, 3)},
            "pass_hat": {k: statistics.mean(per[arm][t]["pass_hat"][k] for t in TASKS) for k in (1, 2, 3)},
            "endings": {e: sum(1 for r in rs if r["ending"] == e) for e in ENDINGS},
            "endings_by_task": {t: [r["ending"] for r in sorted(rs, key=lambda r: r["seq"]) if r["task"] == t]
                                for t in TASKS},
            "harbor_timeout_rewards": [r["reward"] for r in rs if r["ending"] == "harbor_timeout"],
            "time_warnings": sum(1 for r in rs if r.get("time_warning_s") is not None),
            "steps_warnings": sum(1 for r in rs if r.get("steps_warning_s") is not None),
            "agent_s_median": statistics.median(agent), "agent_s_total": sum(agent),
            "agent_s_max": max(agent),
            "prompt_tokens": p, "cached_tokens": cached, "completion_tokens": comp,
            "model_calls": sum(r["model_calls"] for r in rs),
            "tool_calls": sum(sum((r.get("tool_calls") or {}).values()) for r in rs),
            "contended": sum(1 for r in rs if (r.get("contention") or {}).get("surplus", 0) > 0),
        }
        out["arms"][arm]["by_task"] = {
            t: {"agent_s_median": statistics.median([r["agent_s"] for r in rs if r["task"] == t]),
                "agent_s_total": sum(r["agent_s"] for r in rs if r["task"] == t),
                "prompt_tokens": sum(tokens(r)[0] for r in rs if r["task"] == t),
                "completion_tokens": sum(tokens(r)[2] for r in rs if r["task"] == t),
                "model_calls": sum(r["model_calls"] for r in rs if r["task"] == t)}
            for t in TASKS}

    # Primary test and exploratory ones.
    out["primary"] = perm_test(rows, "strict", "off")
    out["exploratory"] = [perm_test(rows, "advisory", "off"), perm_test(rows, "strict", "advisory")]
    infra = [r["seq"] for r in rows if r["ending"] == "infra_error"]
    verifier = [s for s, h in hand.items() if h.get("verifier_failure")]
    out["sensitivity"] = {"excluded": infra + verifier,
                          "strict_vs_off": perm_test(rows, "strict", "off", exclude=set(infra + verifier))}

    # Key secondary: committed answers in failed trials, with the hand-check.
    sec = {}
    for arm in ARMS:
        failed = [r for r in rows if r["arm"] == arm and r["reward"] == 0]
        items = []
        for r in failed:
            h = hand[r["seq"]]
            fa = h["first_answer_s"]
            t_warn = r["time_warning_s"] if r.get("time_warning_s") is not None else r["t_warn_s"]
            if t_warn is None:
                # A Harbor-killed trial has no agent metadata, so collect.py
                # left T_warn empty. Every task's agent timeout is 900 s.
                t_warn = (900 - 20) * 0.9
            items.append({"seq": r["seq"], "task": r["task"], "ending": r["ending"], "agent_s": r["agent_s"],
                          "t_warn_s": t_warn, "warning_given": r.get("time_warning_s") is not None,
                          "first_answer_s": fa, "collect_first_answer_s": r["first_answer_s"],
                          "answer_path": fa is not None or h["present_at_end"] is not None,
                          "before_warn": None if fa is None else fa < t_warn,
                          "before_end": None if fa is None else fa < r["agent_s"],
                          "present_at_end": h["present_at_end"], "cause": h["cause"],
                          "corrected": not h["agrees"]})
        with_path = [i for i in items if i["answer_path"]]
        sec[arm] = {"failed": len(items), "with_answer_path": len(with_path),
                    "before_warn": sum(1 for i in with_path if i["before_warn"]),
                    "before_end": sum(1 for i in with_path if i["before_end"]),
                    "present_at_end": sum(1 for i in with_path if i["present_at_end"]),
                    "trials": items}
    s, o = sec["strict"], sec["off"]
    sec["fisher_before_warn_strict_vs_off"] = fisher_two_sided(
        s["before_warn"], s["with_answer_path"] - s["before_warn"],
        o["before_warn"], o["with_answer_path"] - o["before_warn"])
    out["secondary"] = sec

    # Every collect.py answer field that the hand-check changed.
    out["corrections"] = [{"seq": q, **{k: v for k, v in h.items() if k in ("first_answer_s", "correction")}}
                          for q, h in sorted(hand.items()) if not h["agrees"]]
    json.dump(out, open(os.path.join(HERE, "analysis.json"), "w"), indent=1)

    for arm in ARMS:
        a = out["arms"][arm]
        print(f"{arm:8} {a['successes']}/{a['n']}  pass@1 {a['pass_at'][1]:.3f} pass@3 {a['pass_at'][3]:.3f} "
              f"pass^3 {a['pass_hat'][3]:.3f}  endings {a['endings']}  median {a['agent_s_median']:.0f}s "
              f"total {a['agent_s_total']:.0f}s  tokens {a['prompt_tokens']}/{a['cached_tokens']}/{a['completion_tokens']}")
    print("primary", out["primary"])
    print("exploratory", out["exploratory"])
    print("sensitivity", out["sensitivity"])
    for arm in ARMS:
        print("secondary", arm, {k: v for k, v in sec[arm].items() if k != "trials"})
    print("fisher", sec["fisher_before_warn_strict_vs_off"])


if __name__ == "__main__":
    main()
