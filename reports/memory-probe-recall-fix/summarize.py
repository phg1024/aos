"""Per probe and arm: the frozen regex score next to the hand-read main,
exact and contradicted counts (handread.py), steps, tokens and memory
reads; the primary Fisher test; and the A1 answers that state an
index-only fact without reading the archive. Writes summary.json.

Usage, from this folder:
    python3 -I summarize.py
"""

import json
import os
import sys
from math import comb
from statistics import mean

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)  # python3 -I leaves the script's folder off the path

from handread import load  # noqa: E402
PRIMARY = ["r2-verify-dir", "r5-register-counts"]

# Facts that are in an index line but not in the probe checkpoint's working
# memory (checked against art-000643, art-001763, art-003130): an A1 answer
# that states one with no memory read took it from the index.
INDEX_ONLY = {
    "r2-root-exit": "107.004",
    "r5-empty-base": "round-3",
    "r5-register-counts": "1024",
    "r8-sibling-pairs": "0.104",
}


def fisher(a, b, c, d):
    """Two-sided Fisher exact p for [[a, b], [c, d]]."""
    n1, n2, k = a + b, c + d, a + c
    p = lambda x: comb(n1, x) * comb(n2, k - x) / comb(n1 + n2, k)
    observed = p(a)
    return sum(p(x) for x in range(max(0, k - n2), min(k, n1) + 1) if p(x) <= observed * (1 + 1e-9))


def samples():
    arms = {"C": [json.loads(line) for line in open(os.path.join(HERE, "samples-C.jsonl"))], "A1": []}
    for draw in (1, 2, 3):
        for line in open(os.path.join(HERE, f"samples-A1-d{draw}.jsonl")):
            s = json.loads(line)
            s["draw"], s["sample"] = draw, (draw - 1) * 3 + s["sample"]
            arms["A1"].append(s)
    return arms


def main():
    arms = samples()
    grades = load()
    probes = []
    for s in arms["C"]:
        if s["probe"] not in probes:
            probes.append(s["probe"])
    rows = []
    for probe in probes:
        for arm in ("C", "A1"):
            ss = [s for s in arms[arm] if s["probe"] == probe]
            g = grades[probe][arm]
            rows.append({
                "probe": probe, "kind": ss[0]["kind"], "arm": arm, "n": len(ss),
                "frozen": sum(s["correct"] for s in ss),
                "main": len(g["main"]), "exact": len(g["exact"]), "contra": len(g["contra"]),
                "repeats_wrong_entry": len(g.get("repeats_wrong_entry", [])),
                "steps": mean(s["steps"] for s in ss), "tokens": mean(s.get("tokens", 0) for s in ss),
                "reads": mean(s.get("memory_reads", 0) for s in ss), "bytes_read": mean(s.get("bytes_read", 0) for s in ss),
            })
    main_of = lambda arm: sum(len(grades[p][arm]["main"]) for p in PRIMARY)
    a, c = main_of("A1"), main_of("C")
    trusted = []
    for s in arms["A1"]:
        token = INDEX_ONLY.get(s["probe"])
        if token and token in s["answer"] and not s.get("memory_reads"):
            g = grades[s["probe"]]["A1"]
            trusted.append({"probe": s["probe"], "sample": s["sample"], "draw": s["draw"],
                            "main": s["sample"] in g["main"], "exact": s["sample"] in g["exact"],
                            "contra": s["sample"] in g["contra"]})
    kinds = {}
    for arm in ("C", "A1"):
        for kind in ("recorded", "recorded-wrong", "unrecorded", "all"):
            ss = [s for s in arms[arm] if kind in ("all", s["kind"])]
            kinds[f"{arm} {kind}"] = {"n": len(ss), "steps": mean(s["steps"] for s in ss),
                                      "tokens": mean(s.get("tokens", 0) for s in ss),
                                      "reads": mean(s.get("memory_reads", 0) for s in ss),
                                      "bytes_read": mean(s.get("bytes_read", 0) for s in ss)}
    out = {
        "rows": rows,
        "primary": {"A1_main": a, "C_main": c, "n": 18, "fisher_p": fisher(a, 18 - a, c, 18 - c)},
        "by_kind": kinds,
        "trusted_index_without_read": trusted,
        "header_agent_index_bytes": sorted({(s["draw"], s["probe"][:2], s["agent_index_segments"], s["agent_index_bytes"]) for s in arms["A1"]}),
    }
    with open(os.path.join(HERE, "summary.json"), "w") as f:
        json.dump(out, f, indent=1)
    print(f"{'probe':20} {'kind':15} arm  frozen main exact contra steps  tokens reads")
    for r in rows:
        print(f"{r['probe']:20} {r['kind']:15} {r['arm']:3} {r['frozen']:6} {r['main']:4} {r['exact']:5} {r['contra']:6} "
              f"{r['steps']:5.2f} {r['tokens']:7.0f} {r['reads']:5.2f}")
    print("primary", out["primary"])
    for k, v in kinds.items():
        print(k, {x: round(y, 2) for x, y in v.items()})
    print("trusted index without read:", len(trusted), "right (main):", sum(t["main"] for t in trusted),
          "contra:", sum(t["contra"] for t in trusted))


if __name__ == "__main__":
    main()
