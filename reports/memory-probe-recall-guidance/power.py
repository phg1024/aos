"""Power of the primary test, simulated before freezing (PLAN.md, "Primary
test"). Baseline per target cell: the re-test's A1 main of 3, shrunk as
(main + 0.5) / 4. Effect: in cells whose re-test misses were negative
claims (NEGATIVE_CELLS, from the re-test's grader notes), and in every
negative-claim probe cell (baseline NEGATIVE_PROBE_BASE, assumed), S1
turns a fraction q of misses into right answers; other cells are
unchanged. Test: stratified by probe, normal approximation of the
permutation test (hypergeometric variance per stratum), two-sided, alpha
0.05.

Usage, from this folder, after cells.py:
    python3 -I power.py
"""

import json
import math
import os
import random

HERE = os.path.dirname(os.path.abspath(__file__))
NEGATIVE_CELLS = {("h-1661-2", 1), ("h-1661-2", 3), ("h-414-1", 1), ("h-414-1", 2), ("h-414-1", 3),
                  ("h-557-3", 2), ("h-557-3", 3), ("h-3133-1", 1), ("h-1661-3", 3)}
NEGATIVE_PROBE_BASE = 0.4
SIMS = 4000


def power(cells, q, with_negative, seed=1):
    rng = random.Random(seed)
    hits = 0
    for _ in range(SIMS):
        strata = {}
        for c in cells:
            if c["kind"] == "negative-claim":
                if not with_negative:
                    continue
                p, affected = NEGATIVE_PROBE_BASE, True
            else:
                p, affected = (c["prior_a1_main"] + 0.5) / 4, (c["probe"], c["draw"]) in NEGATIVE_CELLS
            pg = p + q * (1 - p) if affected else p
            n = c["n"]
            s = strata.setdefault(c["probe"], [0, 0, 0])
            s[0] += sum(rng.random() < pg for _ in range(n))
            s[1] += sum(rng.random() < p for _ in range(n))
            s[2] += n
        num = var = 0.0
        for g, a, n in strata.values():
            total, k = 2 * n, g + a
            num += g - k / 2
            var += n * n * k * (total - k) / (total * total * (total - 1))
        hits += var > 0 and abs(num / math.sqrt(var)) > 1.96
    return hits / SIMS


def main():
    cells = [c for c in json.load(open(os.path.join(HERE, "cells.json"))) if c["set"] == "target"]
    for with_negative in (False, True):
        label = "re-test and negative-claim cells" if with_negative else "re-test cells only"
        print(label, {q: round(power(cells, q, with_negative), 2) for q in (0.3, 0.5, 0.7)})


if __name__ == "__main__":
    main()
