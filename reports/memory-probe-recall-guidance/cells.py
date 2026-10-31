"""The probe x draw cells of the guidance test and their samples per arm
(PLAN.md, "Cells"). Inputs fixed before this test: the header re-test's
index-recall grades (../memory-probe-recall-header/coverage_grades.json)
and its A1 grades per cell (primary inputs of c0c3471), and, for the
negative-claim probes, coverage_grades-negative.json (graded here before
any model call).

- target: no shown line states the fact (grade not "states", or the
  stating line hidden). Samples per arm: 6 where A1 missed in c0c3471
  (main < 3 of 3), 2 where it did not; negative-claim cells: 4.
- harm: a shown line states the fact. 8 of the re-test's 32 such cells,
  drawn with random.Random("guidance-harm"), 3 samples per arm; every
  negative-claim cell of this kind, 3 per arm.

Writes cells.json and, per draw and sample count, the probe subsets the
runs ask (probes-d<draw>-n<n>.json).

Usage, from this folder:
    python3 -I cells.py
"""

import json
import os
import random

HERE = os.path.dirname(os.path.abspath(__file__))
PREV = os.path.join(HERE, "..", "memory-probe-recall-header")
N_MISSED, N_CLEAN, N_NEGATIVE, N_HARM, HARM_CELLS = 6, 2, 4, 3, 8


def prior_a1():
    """A1 main per (probe, draw) in the header re-test, from its grades and key."""
    key = json.load(open(os.path.join(PREV, "grading", "key.json")))
    grades = {}
    for name in sorted(os.listdir(os.path.join(PREV, "grading"))):
        if name.startswith("grades-"):
            grades.update(json.load(open(os.path.join(PREV, "grading", name))))
    out = {}
    for gid, k in key.items():
        if k["arm"] == "A1":
            out[(k["probe"], k["draw"])] = out.get((k["probe"], k["draw"]), 0) + grades[gid]["main"]
    return out


def stated_shown(grade):
    return grade["grade"] == "states" and grade["shown"]


def main():
    prior = prior_a1()
    cells = []
    harm_pool = []
    for probe, by_draw in json.load(open(os.path.join(PREV, "coverage_grades.json"))).items():
        for draw, grade in sorted(by_draw.items()):
            cell = {"probe": probe, "draw": int(draw), "kind": "new", "grade": grade["grade"], "shown": grade["shown"],
                    "prior_a1_main": prior[(probe, int(draw))]}
            if stated_shown(grade):
                harm_pool.append(cell)
            else:
                cells.append(dict(cell, set="target", n=N_MISSED if cell["prior_a1_main"] < 3 else N_CLEAN))
    harm_pool.sort(key=lambda c: (c["probe"], c["draw"]))
    for cell in random.Random("guidance-harm").sample(harm_pool, HARM_CELLS):
        cells.append(dict(cell, set="harm", n=N_HARM))
    negative = os.path.join(HERE, "coverage_grades-negative.json")
    if os.path.exists(negative):
        for probe, by_draw in json.load(open(negative)).items():
            for draw, grade in sorted(by_draw.items()):
                cell = {"probe": probe, "draw": int(draw), "kind": "negative-claim", "grade": grade["grade"],
                        "shown": grade["shown"], "prior_a1_main": None}
                cells.append(dict(cell, set="harm", n=N_HARM) if stated_shown(grade)
                             else dict(cell, set="target", n=N_NEGATIVE))
    cells.sort(key=lambda c: (c["set"], c["probe"], c["draw"]))
    json.dump(cells, open(os.path.join(HERE, "cells.json"), "w"), indent=1)

    probes = {p["id"]: p for p in json.load(open(os.path.join(PREV, "probes-new.json")))}
    if os.path.exists(os.path.join(HERE, "probes-negative.json")):
        probes.update({p["id"]: p for p in json.load(open(os.path.join(HERE, "probes-negative.json")))})
    for name in os.listdir(HERE):
        if name.startswith("probes-d") and name.endswith(".json"):
            os.remove(os.path.join(HERE, name))
    groups = {}
    for c in cells:
        groups.setdefault((c["draw"], c["n"]), []).append(probes[c["probe"]])
    for (draw, n), group in sorted(groups.items()):
        json.dump(group, open(os.path.join(HERE, f"probes-d{draw}-n{n}.json"), "w"), indent=1, ensure_ascii=False)
    for s in ("target", "harm"):
        sel = [c for c in cells if c["set"] == s]
        print(f"{s}: {len(sel)} cells, {sum(c['n'] for c in sel)} answers per arm")
    print("runs per arm:", {f"d{d}-n{n}": len(g) for (d, n), g in sorted(groups.items())})


if __name__ == "__main__":
    main()
