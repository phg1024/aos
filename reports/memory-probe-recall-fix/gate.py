"""Decides whether the probe phase runs, from the indexes the handoff phase
kept (indexes-d1..3.json). The rule of the first A1 evaluation
(../memory-probe-recall/gate.py), unchanged, and frozen with this plan
before any handoff; see PLAN.md, "Stop rule".

The probe phase runs only if both hold:
  (a) at least 14 of the 27 draws (segment x draw) kept one line or more;
  (b) for each truth segment of the primary probes, art-000414
      (r2-verify-dir, #37-38) and art-001661 (r5-register-counts, #31-35),
      at least 2 of its 3 draws kept one line or more.
Otherwise the A1 arm would differ little from C, and phg-gpu is not spent
on it.

Prints PASS or FAIL with the counts; exit status 0 for PASS, 1 for FAIL.

Usage, from this folder:
    python3 -I gate.py
"""

import json
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
SEGMENTS = 9
PRIMARY = ["art-000414", "art-001661"]


def main():
    kept = {}
    for draw in (1, 2, 3):
        path = os.path.join(HERE, f"indexes-d{draw}.json")
        indexes = json.load(open(path)) if os.path.exists(path) else {}
        for segment, lines in indexes.items():
            kept[(segment, draw)] = len(lines)
    with_index = sum(1 for n in kept.values() if n > 0)
    primary = {s: sum(1 for d in (1, 2, 3) if kept.get((s, d), 0) > 0) for s in PRIMARY}
    ok = with_index >= 14 and all(n >= 2 for n in primary.values())
    print(f"{'PASS' if ok else 'FAIL'}: {with_index} of {3 * SEGMENTS} draws kept an index; "
          + ", ".join(f"{s} {n} of 3" for s, n in primary.items()))
    sys.exit(0 if ok else 1)


if __name__ == "__main__":
    main()
