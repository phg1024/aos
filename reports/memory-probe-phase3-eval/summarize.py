"""Per-probe counts for the phase-3 evaluation, with two-sided Fisher exact
p-values against phase 2 and between conditions.

Usage, from the worktree root:
    python3 -I reports/memory-probe-phase3-eval/summarize.py [--json out.json]

Scores are the frozen ones recorded in each sample (correct, superseded,
off_topic); nothing is rescored here. Phase-2 samples are read from the
committed phase-2 files named in PLAN.md.
"""

import json
import os
import sys
from math import comb

HERE = os.path.dirname(os.path.abspath(__file__))
REPORTS = os.path.dirname(HERE)
CONDITIONS = ["memory", "no-check-rule", "no-memory"]
PHASE2 = {
    "leaf2-speedup": ["memory-probe-phase2/precedence/samples-rescored.jsonl"],
    "leaf2-status": ["memory-probe-phase2/precedence/samples-rescored.jsonl"],
    "tridata-order": ["memory-probe-phase2/samples-rescored.jsonl", "memory-probe-phase2/archive-x6/samples-phase2.jsonl"],
    "tg-sweep": ["memory-probe-phase2/samples-rescored.jsonl"],
    "w4-first-ab": ["memory-probe-phase2/samples-rescored.jsonl"],
}


def fisher(a, n1, b, n2):
    """Two-sided Fisher exact p for a/n1 vs b/n2 successes: the sum of the
    probabilities of all tables with the same margins that are no more
    likely than the observed one."""
    k = a + b
    total = comb(n1 + n2, k)

    def p(x):
        return comb(n1, x) * comb(n2, k - x) / total

    observed = p(a)
    return min(1.0, sum(p(x) for x in range(max(0, k - n2), min(k, n1) + 1) if p(x) <= observed * (1 + 1e-9)))


def read(path):
    with open(path) as f:
        return [json.loads(line) for line in f if line.strip()]


def summarize(samples):
    out = {}
    for s in samples:
        r = out.setdefault(s["probe"], {"n": 0, "correct": 0, "superseded": 0, "off_topic": 0, "errors": 0,
                                         "steps": [], "bytes": [], "searches": [], "reads": [], "updates": []})
        r["n"] += 1
        r["correct"] += bool(s.get("correct"))
        r["superseded"] += bool(s.get("superseded"))
        r["off_topic"] += bool(s.get("off_topic"))
        r["errors"] += bool(s.get("error"))
        r["steps"].append(s.get("steps", 0))
        r["bytes"].append(s.get("bytes_read", 0))
        r["searches"].append(s.get("searches", 0))
        r["reads"].append(s.get("memory_reads", 0))
        r["updates"].append(s.get("memory_updates", 0))
    return out


def stats(values):
    return {"mean": sum(values) / len(values), "min": min(values), "max": max(values)} if values else None


def main():
    assert abs(fisher(0, 9, 8, 9) - 0.0004) < 0.0001 and abs(fisher(8, 9, 6, 9) - 0.58) < 0.01
    cond = {}
    for c in CONDITIONS:
        path = os.path.join(HERE, f"samples-{c}.jsonl")
        if os.path.exists(path):
            cond[c] = summarize(read(path))
    phase2 = {}
    for probe, files in PHASE2.items():
        samples = [s for f in files for s in read(os.path.join(REPORTS, f)) if s["probe"] == probe]
        phase2[probe] = summarize(samples).get(probe)
    probes = list(dict.fromkeys(p for c in cond.values() for p in c))
    rows = []
    for probe in probes:
        row = {"probe": probe}
        for c, by in cond.items():
            r = by.get(probe)
            if r:
                row[c] = {k: r[k] for k in ("n", "correct", "superseded", "off_topic", "errors")}
                row[c].update({k: stats(r[k]) for k in ("steps", "bytes", "searches", "reads", "updates")})
        m = cond.get("memory", {}).get(probe)
        p2 = phase2.get(probe)
        if m and p2:
            row["phase2"] = {k: p2[k] for k in ("n", "correct", "superseded", "off_topic", "errors")}
            row["phase2"].update({k: stats(p2[k]) for k in ("steps", "bytes", "searches", "reads")})
            row["p_vs_phase2"] = fisher(m["correct"], m["n"], p2["correct"], p2["n"])
        for other in ("no-check-rule", "no-memory"):
            o = cond.get(other, {}).get(probe)
            if m and o:
                row[f"p_vs_{other}"] = fisher(m["correct"], m["n"], o["correct"], o["n"])
        rows.append(row)

    def cell(r):
        return f"{r['correct']}/{r['n']}" + (f" s{r['superseded']}" if r["superseded"] else "") + (f" o{r['off_topic']}" if r["off_topic"] else "") + (f" e{r['errors']}" if r["errors"] else "")

    print(f"{'probe':<20} {'phase2':>10} {'memory':>10} {'p':>7} {'no-check':>10} {'p':>7} {'no-memory':>10} {'p':>7}  steps(mem) bytes(mem)")
    for row in rows:
        def get(c):
            return cell(row[c]) if c in row else "-"

        def pv(k):
            return f"{row[k]:.3g}" if k in row else "-"

        mem = row.get("memory")
        cost = f"{mem['steps']['mean']:.1f} [{mem['steps']['min']}-{mem['steps']['max']}] {mem['bytes']['mean']:.0f}" if mem else ""
        print(f"{row['probe']:<20} {get('phase2'):>10} {get('memory'):>10} {pv('p_vs_phase2'):>7} {get('no-check-rule'):>10} {pv('p_vs_no-check-rule'):>7} {get('no-memory'):>10} {pv('p_vs_no-memory'):>7}  {cost}")
    if "--json" in sys.argv:
        with open(sys.argv[sys.argv.index("--json") + 1], "w") as f:
            json.dump(rows, f, indent=2)


if __name__ == "__main__":
    main()
