"""Detectable difference for the pi arm's two primary comparisons (PLAN-pi.md).

Each comparison (pi vs aos, pi vs terminus-2) is the baseline's exact
stratified permutation test of mean reward: labels permuted within each
task, two-sided, 16 tasks x 3 trials per arm. With Holm's correction over the
two, the first comparison is tested at 0.025 and the second at 0.05. This
script simulates the test at both levels. Its scenarios take the comparator
arm's per-task success rates from the baseline (c/3 per task) and shift every
task by delta, capped at 0 and 1. Stdlib only; seed fixed.

    python3 -I power_pi.py
"""

import json
import os
import random
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
from power import exact_p  # noqa: E402
N, SIMS, SEED = 3, 2000, 20261111


def power(base_rates, delta, rng, alphas):
    hits = {a: 0 for a in alphas}
    for _ in range(SIMS):
        strata = []
        for p0 in base_rates:
            p1 = min(1.0, max(0.0, p0 + delta))
            strata.append(([int(rng.random() < p1) for _ in range(N)], [int(rng.random() < p0) for _ in range(N)]))
        p = exact_p(strata)
        for a in alphas:
            hits[a] += p < a
    return {a: h / SIMS for a, h in hits.items()}


def main():
    analysis = json.load(open(os.path.join(HERE, "analysis.json")))
    tasks = json.load(open(os.path.join(HERE, "drawn.json")))["drawn"]
    rng = random.Random(SEED)
    scenarios = {f"{arm} rates": [analysis["per_task"][arm][t]["c"] / N for t in tasks]
                 for arm in ("terminus-2", "aos")}
    scenarios["mixed 6x0.05 5x0.5 5x0.9"] = [0.05] * 6 + [0.5] * 5 + [0.9] * 5
    deltas = [-0.40, -0.30, -0.20, -0.15, -0.10, 0.10, 0.15, 0.20, 0.30, 0.40]
    print(f"power of the exact stratified test, two-sided, {SIMS} simulations per cell")
    for name, rates in scenarios.items():
        print(f"{name}: mean {sum(rates) / len(rates):.3f}")
        for d in deltas:
            p = power(rates, d, rng, (0.025, 0.05))
            print(f"  delta {d:+.2f}: alpha 0.025 {p[0.025]:.2f}   alpha 0.05 {p[0.05]:.2f}")


if __name__ == "__main__":
    main()
