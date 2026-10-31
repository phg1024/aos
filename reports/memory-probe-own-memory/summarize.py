"""Summarizes the own-memory probe samples: frozen regex scores and hand-read
grades side by side, per probe and per kind, with two-sided Fisher exact
tests and sign tests. Writes summary.json next to this file.

Usage, from this folder:
    python3 -I summarize.py
"""

import json
import math
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
from handread import GRADES, REPEATS_WRONG_ENTRY  # noqa: E402

ARMS = ["own-memory", "no-memory"]


def fisher(a, n1, b, n2):
    """Two-sided Fisher exact p for a/n1 against b/n2."""
    k, n = a + b, n1 + n2

    def prob(x):
        return math.comb(n1, x) * math.comb(n2, k - x) / math.comb(n, k)

    observed = prob(a)
    return min(1.0, sum(prob(x) for x in range(max(0, k - n2), min(k, n1) + 1) if prob(x) <= observed * (1 + 1e-9)))


def sign_test(wins, losses):
    """Two-sided exact sign test, ties dropped."""
    n = wins + losses
    if n == 0:
        return 1.0
    tail = sum(math.comb(n, i) for i in range(0, min(wins, losses) + 1)) / 2 ** n
    return min(1.0, 2 * tail)


def main():
    samples = {arm: [json.loads(line) for line in open(os.path.join(HERE, f"samples-{arm}.jsonl"))] for arm in ARMS}
    probes = []
    for r in samples["own-memory"]:
        if r["probe"] not in [p["id"] for p in probes]:
            probes.append({"id": r["probe"], "kind": r["kind"]})

    rows = []
    for p in probes:
        row = {"id": p["id"], "kind": p["kind"]}
        for arm in ARMS:
            rs = [r for r in samples[arm] if r["probe"] == p["id"]]
            assert len(rs) == 9 and sorted(r["sample"] for r in rs) == list(range(1, 10))
            main_s, exact_s, contra_s, _ = GRADES[p["id"]][arm]
            row[arm] = {
                "frozen_correct": sum(r["correct"] for r in rs),
                "frozen_superseded": sum(r["superseded"] for r in rs),
                "main": len(main_s),
                "exact": len(exact_s),
                "contra": len(contra_s),
                "steps": sum(r["steps"] for r in rs) / 9,
                "one_step": sum(r["steps"] == 1 for r in rs),
                "memory_reads": sum(r["memory_reads"] for r in rs) / 9,
                "bytes_read": sum(r["bytes_read"] for r in rs) / 9,
                "tool_errors": sum(r["tool_errors"] for r in rs),
                "disagree": [r["sample"] for r in rs if bool(r["correct"]) != (r["sample"] in exact_s)],
            }
            if p["id"] in REPEATS_WRONG_ENTRY:
                row[arm]["repeats_wrong_entry"] = len(REPEATS_WRONG_ENTRY[p["id"]][arm])
        a, b = row["own-memory"], row["no-memory"]
        row["p_frozen"] = fisher(a["frozen_correct"], 9, b["frozen_correct"], 9)
        row["p_main"] = fisher(a["main"], 9, b["main"], 9)
        row["p_exact"] = fisher(a["exact"], 9, b["exact"], 9)
        rows.append(row)

    kinds = {}
    for kind in ["recorded", "unrecorded", "recorded-wrong"]:
        ks = [r for r in rows if r["kind"] == kind]
        entry = {"probes": len(ks), "n": 9 * len(ks)}
        for score in ["frozen_correct", "main", "exact", "contra"]:
            a = sum(r["own-memory"][score] for r in ks)
            b = sum(r["no-memory"][score] for r in ks)
            better = sum(r["own-memory"][score] > r["no-memory"][score] for r in ks)
            worse = sum(r["own-memory"][score] < r["no-memory"][score] for r in ks)
            if score == "contra":
                better, worse = worse, better
            entry[score] = {"own-memory": a, "no-memory": b, "p_fisher": fisher(a, entry["n"], b, entry["n"]),
                            "A_better": better, "equal": len(ks) - better - worse, "A_worse": worse,
                            "p_sign": sign_test(better, worse)}
        for arm in ARMS:
            entry[f"steps_{arm}"] = sum(r[arm]["steps"] for r in ks) / len(ks)
        kinds[kind] = entry

    totals = {arm: {"steps": sum(r[arm]["steps"] for r in rows) / len(rows),
                    "one_step": sum(r[arm]["one_step"] for r in rows),
                    "bytes_read": sum(r[arm]["bytes_read"] for r in rows) / len(rows),
                    "tool_errors": sum(r[arm]["tool_errors"] for r in rows)} for arm in ARMS}
    out = {"probes": rows, "kinds": kinds, "totals": totals}
    with open(os.path.join(HERE, "summary.json"), "w") as f:
        json.dump(out, f, indent=1)

    print(f"{'probe':20} {'kind':15} {'frozen A/B':>10} {'main A/B':>9} {'exact A/B':>9} {'contra A/B':>10} {'p_exact':>8} {'steps A/B':>10} disagree A | B")
    for r in rows:
        a, b = r["own-memory"], r["no-memory"]
        print(f"{r['id']:20} {r['kind']:15} {a['frozen_correct']:>4}/{b['frozen_correct']:<5} {a['main']:>4}/{b['main']:<4} {a['exact']:>4}/{b['exact']:<4} {a['contra']:>5}/{b['contra']:<4} {r['p_exact']:8.3f} {a['steps']:4.1f}/{b['steps']:<5.1f} {a['disagree']} | {b['disagree']}")
    for kind, e in kinds.items():
        print(kind, {s: (e[s]["own-memory"], e[s]["no-memory"], round(e[s]["p_fisher"], 4), e[s]["A_better"], e[s]["equal"], e[s]["A_worse"], round(e[s]["p_sign"], 3)) for s in ["frozen_correct", "main", "exact", "contra"]})
    print(totals)


if __name__ == "__main__":
    main()
