"""Detectable difference for the primary test, computed before the run (PLAN.md).

The primary test is the exact stratified permutation test of mean reward,
aos minus terminus-2, labels permuted within each task, two-sided,
alpha = 0.05; 16 tasks, 3 trials per arm per task. This script simulates
that test under fixed scenarios and prints its power for each difference.
Stdlib only; seed fixed.

    python3 -I power.py
"""

import itertools
import math
import random

TASKS, N, ALPHA, SIMS, SEED = 16, 3, 0.05, 2000, 20261109


def exact_p(strata):
    """Two-sided exact stratified permutation p-value; strata are (a, b) lists of 0/1."""
    na = sum(len(a) for a, _ in strata)
    nb = sum(len(b) for _, b in strata)
    observed = sum(map(sum, (a for a, _ in strata))) / na - sum(map(sum, (b for _, b in strata))) / nb
    dist, total = {0: 1.0}, 0
    for a, b in strata:
        pool = a + b
        total += sum(pool)
        combos = list(itertools.combinations(range(len(pool)), len(a)))
        local = {}
        for idx in combos:
            s = sum(pool[i] for i in idx)
            local[s] = local.get(s, 0) + 1 / len(combos)
        new = {}
        for x, px in dist.items():
            for y, py in local.items():
                new[x + y] = new.get(x + y, 0) + px * py
        dist = new
    return min(1.0, sum(px for sa, px in dist.items()
                        if abs(sa / na - (total - sa) / nb) >= abs(observed) - 1e-12))


def power(base_rates, delta, rng):
    hits = 0
    for _ in range(SIMS):
        strata = []
        for p0 in base_rates:
            p1 = min(1.0, max(0.0, p0 + delta))
            strata.append(([int(rng.random() < p1) for _ in range(N)], [int(rng.random() < p0) for _ in range(N)]))
        hits += exact_p(strata) < ALPHA
    return hits / SIMS


def main():
    rng = random.Random(SEED)
    scenarios = {
        # Every task has the same terminus-2 success rate.
        "homogeneous p0=0.30": [0.30] * TASKS,
        "homogeneous p0=0.50": [0.50] * TASKS,
        # Mixed difficulty, as TB2 is: 6 tasks near 0, 5 in the middle, 5 near 1;
        # mean about 0.44. A shift of delta is applied to every task, capped at 0 and 1.
        "mixed 6x0.05 5x0.5 5x0.9": [0.05] * 6 + [0.5] * 5 + [0.9] * 5,
    }
    deltas = [0.10, 0.15, 0.20, 0.25, 0.30, 0.35, 0.40]
    print("power of the exact stratified test (alpha 0.05, two-sided), %d simulations each" % SIMS)
    for name, rates in scenarios.items():
        row = []
        for d in deltas:
            row.append(f"{d:+.2f}: {power(rates, d, rng):.2f}")
        print(f"{name:28s} " + "  ".join(row))
    # The smallest pooled difference that can reach p < 0.05 at all, from the
    # most favourable split (every success in one arm), for reference.
    for k in range(1, 49):
        strata = [([1] * N, [0] * N) if i < k // N else ([0] * N, [0] * N) for i in range(TASKS)]
        r = k % N
        if r:
            strata[k // N] = ([1] * r + [0] * (N - r), [0] * N)
        if exact_p(strata) < ALPHA:
            print(f"smallest separation that can be significant: {k} more successes "
                  f"({k / (TASKS * N):.3f} of 48) when the other arm has none on those tasks")
            break


if __name__ == "__main__":
    main()
