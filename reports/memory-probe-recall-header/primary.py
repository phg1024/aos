"""The primary test (PLAN.md): main score on the new probes, A1 against C,
by a two-sided stratified permutation test with the probe as the stratum.
Statistic: total A1 main minus total C main. Within each probe the arm
labels of its answers are permuted, 100,000 times with
random.Random(20261031). Also the pooled Fisher tests of the harm check.

Reads the blind grades (grading/grades-*.json: {gid: {"main": 0/1,
"exact": 0/1, "contra": 0/1, ...}}) and grading/key.json.

Usage, from this folder, after grading:
    python3 -I primary.py
"""

import glob
import json
import os
import random
from math import comb

HERE = os.path.dirname(os.path.abspath(__file__))
PERMUTATIONS = 100_000


def permutation_p(strata, rng):
    """strata: per probe, (A1 main flags, C main flags). Two-sided p for
    the total A1 - C difference with labels permuted within each probe."""
    observed = sum(sum(a) - sum(c) for a, c in strata)
    pooled = [(a + c, len(a)) for a, c in strata]
    extreme = 0
    for _ in range(PERMUTATIONS):
        total = 0
        for flags, n_a in pooled:
            shuffled = rng.sample(flags, len(flags))
            total += sum(shuffled[:n_a]) - sum(shuffled[n_a:])
        if abs(total) >= abs(observed):
            extreme += 1
    return observed, (extreme + 1) / (PERMUTATIONS + 1)


def fisher(a, b, c, d):
    """Two-sided Fisher exact p for [[a, b], [c, d]]."""
    n1, n2, k = a + b, c + d, a + c
    p = lambda x: comb(n1, x) * comb(n2, k - x) / comb(n1 + n2, k)
    observed = p(a)
    return sum(p(x) for x in range(max(0, k - n2), min(k, n1) + 1) if p(x) <= observed * (1 + 1e-9))


def load():
    key = json.load(open(os.path.join(HERE, "grading", "key.json")))
    grades = {}
    for path in glob.glob(os.path.join(HERE, "grading", "grades-*.json")):
        grades.update(json.load(open(path)))
    missing = set(key) - set(grades)
    if missing:
        raise SystemExit(f"{len(missing)} answers ungraded, e.g. {sorted(missing)[:5]}")
    return [dict(key[gid], gid=gid, **grades[gid]) for gid in key]


def main():
    rows = load()
    new = sorted({r["probe"] for r in rows if r["file"].endswith("-new.jsonl")})
    strata = []
    for probe in new:
        a = [r["main"] for r in rows if r["probe"] == probe and r["arm"] == "A1"]
        c = [r["main"] for r in rows if r["probe"] == probe and r["arm"] == "C"]
        strata.append((a, c))
    observed, p = permutation_p(strata, random.Random(20261031))
    n_a, n_c = sum(len(a) for a, _ in strata), sum(len(c) for _, c in strata)
    out = {"primary": {"probes": len(new), "A1_main": sum(sum(a) for a, _ in strata), "A1_n": n_a,
                       "C_main": sum(sum(c) for _, c in strata), "C_n": n_c,
                       "difference": observed, "permutation_p": p},
           "per_probe": {probe: {"A1": sum(a), "C": sum(c), "n": len(a)} for probe, (a, c) in zip(new, strata)}}
    frozen_kinds = ("r2-first-sah", "r2-root-exit", "r2-shadow-limit", "r5-empty-base", "r5-entered-stack",
                    "r5-tir-guard", "r8-trace-share", "r8-small-node")
    for measure in ("main", "contra"):
        a = sum(r[measure] for r in rows if r["probe"] in frozen_kinds and r["arm"] == "A1")
        c = sum(r[measure] for r in rows if r["probe"] in frozen_kinds and r["arm"] == "C")
        n = sum(1 for r in rows if r["probe"] in frozen_kinds and r["arm"] == "A1")
        out[f"regression_{measure}"] = {"A1": a, "C": c, "n": n, "fisher_p": fisher(a, n - a, c, n - c)}
    json.dump(out, open(os.path.join(HERE, "primary.json"), "w"), indent=1)
    print(json.dumps({k: v for k, v in out.items() if k != "per_probe"}, indent=1))


if __name__ == "__main__":
    main()
