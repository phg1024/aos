"""Results of the guidance test (PLAN.md): gate G3 on the samples, the
primary test, its confidence interval, the secondary measures and the
harm and cost check. Reads cells.json, the samples, the blind grades
(grading/grades-*.json: {gid: {"main", "exact", "contra",
"negative_claim", ...}}) and grading/key.json. Writes results.json.

Usage, from this folder, after grading (G3 alone, before grading:
python3 -I analyze.py --g3):
    python3 -I analyze.py
"""

import glob
import json
import math
import os
import random
import statistics
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
ARMS = ("A1", "A1G")
PERMUTATIONS = 100_000
BOOTSTRAPS = 10_000
CHECKPOINT = {}


def checkpoints():
    for path in (os.path.join(HERE, "..", "memory-probe-recall-header", "probes-new.json"),
                 os.path.join(HERE, "probes-negative.json")):
        for p in json.load(open(path)):
            CHECKPOINT[p["id"]] = p["checkpoint"]


def samples():
    """(file, probe, sample) -> sample record."""
    out = {}
    for path in glob.glob(os.path.join(HERE, "samples-*.jsonl")):
        for line in open(path):
            s = json.loads(line)
            out[(os.path.basename(path), s["probe"], s["sample"])] = s
    return out


def g3(recs):
    """Every sample shows the lines G1 predicts for its draw and checkpoint."""
    ref = json.load(open(os.path.join(HERE, "reference-6c55bdb", "header.json")))
    bad = []
    for (name, probe, sample), s in recs.items():
        draw = name.split("-")[2][1:]   # samples-A1G-d2-n6.jsonl
        want = ref[f"indexes-d{draw}.json"][CHECKPOINT[probe]]["by_segment"]
        got = {k: (s.get("agent_index_shown", {}).get(k, 0), s.get("agent_index_hidden", {}).get(k, 0)) for k in want}
        if got != {k: (v["shown"], v["hidden"]) for k, v in want.items()} or s.get("agent_index_budget") != 6144:
            bad.append(f"{name} {probe} s{sample}")
    return {"pass": not bad, "samples": len(recs), "mismatches": bad[:20]}


def fisher(a, b, c, d):
    n1, n2, k = a + b, c + d, a + c
    p = lambda x: math.comb(n1, x) * math.comb(n2, k - x) / math.comb(n1 + n2, k)
    observed = p(a)
    return sum(p(x) for x in range(max(0, k - n2), min(k, n1) + 1) if p(x) <= observed * (1 + 1e-9))


def permutation_p(rows, measure="main"):
    """Two-sided, stratified by probe: A1G total minus A1 total."""
    strata = {}
    for r in rows:
        strata.setdefault(r["probe"], []).append(r)
    pooled = [([r[measure] for r in v], sum(1 for r in v if r["arm"] == "A1G")) for v in strata.values()]
    observed = sum(r[measure] for r in rows if r["arm"] == "A1G") - sum(r[measure] for r in rows if r["arm"] == "A1")
    rng, extreme = random.Random(20261031), 0
    for _ in range(PERMUTATIONS):
        total = 0
        for flags, n_g in pooled:
            shuffled = rng.sample(flags, len(flags))
            total += 2 * sum(shuffled[:n_g]) - sum(flags)
        extreme += abs(total) >= abs(observed)
    return observed, (extreme + 1) / (PERMUTATIONS + 1)


def bootstrap_ci(rows):
    """95% interval of the A1G minus A1 main rate, resampling answers within
    each cell and arm."""
    cells = {}
    for r in rows:
        cells.setdefault((r["probe"], r["draw"], r["arm"]), []).append(r["main"])
    keys = sorted(cells)
    n = {arm: sum(len(cells[k]) for k in keys if k[2] == arm) for arm in ARMS}
    rng, diffs = random.Random(20261031), []
    for _ in range(BOOTSTRAPS):
        tot = {arm: 0 for arm in ARMS}
        for k in keys:
            v = cells[k]
            tot[k[2]] += sum(rng.choice(v) for _ in v)
        diffs.append(tot["A1G"] / n["A1G"] - tot["A1"] / n["A1"])
    diffs.sort()
    return [diffs[int(0.025 * BOOTSTRAPS)], diffs[int(0.975 * BOOTSTRAPS) - 1]]


def summary(rows):
    out = {}
    for arm in ARMS:
        sel = [r for r in rows if r["arm"] == arm]
        out[arm] = {"n": len(sel), **{m: sum(r[m] for r in sel) for m in ("main", "exact", "contra", "negative_claim")},
                    **{f"mean_{k}": round(statistics.mean(r[k] for r in sel), 2) if sel else None
                       for k in ("tokens", "steps", "searches", "memory_reads", "memory_calls", "bytes_read")},
                    "answers_with_a_memory_call": sum(1 for r in sel if r["memory_calls"])}
    return out


def main():
    checkpoints()
    recs = samples()
    gate = g3(recs)
    if "--g3" in sys.argv:
        print(json.dumps(gate, indent=1))
        sys.exit(0 if gate["pass"] else 1)
    key = json.load(open(os.path.join(HERE, "grading", "key.json")))
    grades = {}
    for path in glob.glob(os.path.join(HERE, "grading", "grades-*.json")):
        grades.update(json.load(open(path)))
    missing = set(key) - set(grades)
    if missing:
        raise SystemExit(f"{len(missing)} answers ungraded, e.g. {sorted(missing)[:5]}")
    cells = {(c["probe"], c["draw"]): c for c in json.load(open(os.path.join(HERE, "cells.json")))}
    rows = []
    for gid, k in key.items():
        s = recs[(k["file"], k["probe"], k["sample"])]
        g = grades[gid]
        c = cells[(k["probe"], k["draw"])]
        rows.append(dict(k, gid=gid, set=c["set"], kind=c["kind"], index_grade=c["grade"], shown=c["shown"],
                         checkpoint=CHECKPOINT[k["probe"]],
                         main=g["main"], exact=g["exact"], contra=g["contra"], negative_claim=g.get("negative_claim", 0),
                         **{f: s.get(f, 0) for f in ("tokens", "steps", "searches", "memory_reads", "bytes_read")},
                         memory_calls=s.get("searches", 0) + s.get("memory_reads", 0)))
    target = [r for r in rows if r["set"] == "target"]
    harm = [r for r in rows if r["set"] == "harm"]
    observed, p = permutation_p(target)
    out = {"g3": gate,
           "primary": {"cells": len({(r["probe"], r["draw"]) for r in target}), **summary(target),
                       "difference": observed, "permutation_p": p, "rate_difference_ci95": bootstrap_ci(target)}}
    a = sum(r["negative_claim"] for r in target if r["arm"] == "A1")
    g = sum(r["negative_claim"] for r in target if r["arm"] == "A1G")
    n = sum(1 for r in target if r["arm"] == "A1")
    out["negative_claim_errors"] = {"A1": a, "A1G": g, "n": n, "fisher_p": fisher(g, n - g, a, n - a)}
    splits = {
        "retest_probes": lambda r: r["kind"] == "new", "negative_claim_probes": lambda r: r["kind"] == "negative-claim",
        "art-000648 (S2 shown)": lambda r: r["checkpoint"] == "art-000648",
        "other checkpoints": lambda r: r["checkpoint"] != "art-000648",
        "points, shown": lambda r: r["index_grade"] == "points" and r["shown"],
        "points or states, hidden": lambda r: r["index_grade"] in ("points", "states") and not r["shown"],
        "no covering line": lambda r: r["index_grade"] == "no",
        **{f"draw {d}": (lambda d: lambda r: r["draw"] == d)(d) for d in (1, 2, 3)},
    }
    out["target_splits"] = {name: summary([r for r in target if f(r)]) for name, f in splits.items()}
    out["target_cells"] = {f"{p} d{d}": {arm: [r["main"] for r in target if (r["probe"], r["draw"], r["arm"]) == (p, d, arm)]
                                         for arm in ARMS}
                           for p, d in sorted({(r["probe"], r["draw"]) for r in target})}
    ha = sum(r["main"] for r in harm if r["arm"] == "A1")
    hg = sum(r["main"] for r in harm if r["arm"] == "A1G")
    hn = sum(1 for r in harm if r["arm"] == "A1")
    out["harm"] = {**summary(harm), "main_fisher_p": fisher(hg, hn - hg, ha, hn - ha),
                   "flagged_cells": [f"{p} d{d}" for p, d in sorted({(r["probe"], r["draw"]) for r in harm})
                                     if sum(r["main"] for r in harm if (r["probe"], r["draw"], r["arm"]) == (p, d, "A1G"))
                                     <= sum(r["main"] for r in harm if (r["probe"], r["draw"], r["arm"]) == (p, d, "A1")) - 2]}
    json.dump(out, open(os.path.join(HERE, "results.json"), "w"), indent=1)
    print(json.dumps({k: v for k, v in out.items() if k != "target_cells"}, indent=1))


if __name__ == "__main__":
    main()
