"""Secondary analyses and the per-answer tables of the harm check
(PLAN.md, "Secondary" and "Harm check"), after primary.py. Reads the blind
grades and the key (grading/), the samples, coverage.json and
coverage_grades.json (fixed before any probe call), and the follow-up's
grades for the continuity probes. Writes analysis.json.

Usage, from this folder, after grading:
    python3 -I analyze.py
"""

import glob
import json
import os
import random
import re
import statistics
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)  # python3 -I leaves this folder off the path
from primary import fisher, load, permutation_p  # noqa: E402

UNRECORDED_FROZEN = ("r2-verify-dir", "r5-register-counts", "r8-sibling-pairs")
REGRESSION = ("r2-first-sah", "r2-root-exit", "r2-shadow-limit", "r5-empty-base", "r5-entered-stack",
              "r5-tir-guard", "r8-trace-share", "r8-small-node")


def samples():
    """(file, probe, sample) -> sample record."""
    out = {}
    for path in glob.glob(os.path.join(HERE, "samples-*.jsonl")):
        for line in open(path):
            s = json.loads(line)
            out[(os.path.basename(path), s["probe"], s["sample"])] = s
    return out


def rates(rows):
    n = len(rows)
    return {"n": n, **{m: sum(r[m] for r in rows) for m in ("main", "exact", "contra")}}


def test(rows, probes, seed=20261031):
    strata = [([r["main"] for r in rows if r["probe"] == p and r["arm"] == "A1"],
               [r["main"] for r in rows if r["probe"] == p and r["arm"] == "C"]) for p in probes]
    observed, p = permutation_p(strata, random.Random(seed))
    return {"probes": len(probes), "A1": rates([r for r in rows if r["probe"] in probes and r["arm"] == "A1"]),
            "C": rates([r for r in rows if r["probe"] in probes and r["arm"] == "C"]),
            "difference": observed, "permutation_p": p}


def read_ranges(reads):
    out = []
    for r in reads or []:
        m = re.match(r"read_memory from=(\d+)(?: to=(\d+))?", r)
        if m:
            out.append((int(m.group(1)), int(m.group(2) or m.group(1))))
    return out


def main():
    rows = load()
    recs = samples()
    for r in rows:
        s = recs[(r["file"], r["probe"], r["sample"])]
        r.update(regex=int(bool(s.get("correct"))), reads=s.get("reads") or [], index_bytes=s.get("agent_index_bytes", 0),
                 **{k: s.get(k, 0) for k in ("steps", "tokens", "bytes_read", "memory_reads", "searches")})
    new_probes = json.load(open(os.path.join(HERE, "probes-new.json")))
    new = sorted(p["id"] for p in new_probes)
    second = {p["id"] for p in new_probes if "Second pass" in p["note"]}
    truth = {p["id"]: int(re.search(r"Truth: #(\d+)", p["note"]).group(1)) for p in new_probes}
    coverage = {c["probe"]: c for c in json.load(open(os.path.join(HERE, "coverage.json")))}
    grades = json.load(open(os.path.join(HERE, "coverage_grades.json")))
    out = {}

    # Secondary: splits fixed before any probe call.
    shown_range = [p for p in new if any(l["shown"] for v in coverage[p]["draws"].values() for l in v["range_covered"])]
    shown_states = [p for p in new if any(g["grade"] == "states" and g["shown"] for g in grades[p].values())]
    states_any = [p for p in new if any(g["grade"] == "states" for g in grades[p].values())]
    out["by_split"] = {
        "range_covered_by_shown_line": test(rows, shown_range),
        "not_range_covered_by_shown_line": test(rows, [p for p in new if p not in shown_range]),
        "stated_by_shown_line": test(rows, shown_states),
        "not_stated_by_shown_line": test(rows, [p for p in new if p not in shown_states]),
        "stated_by_any_line": test(rows, states_any),
        "not_stated_by_any_line": test(rows, [p for p in new if p not in states_any]),
        "first_pass": test(rows, [p for p in new if p not in second]),
        "second_pass": test(rows, [p for p in new if p in second]),
    }
    # A1 answers by what their own draw's header showed for the probe.
    per_draw_cell = {}
    for r in rows:
        if r["arm"] != "A1" or r["probe"] not in new:
            continue
        g = grades[r["probe"]][str(r["draw"])]
        cell = "stated_shown" if g["grade"] == "states" and g["shown"] else (
            "stated_hidden" if g["grade"] == "states" else g["grade"])
        per_draw_cell.setdefault(cell, []).append(r)
    out["a1_by_own_draw_grade"] = {k: rates(v) for k, v in per_draw_cell.items()}
    out["per_draw_new"] = {d: rates([r for r in rows if r["arm"] == "A1" and r["draw"] == d and r["probe"] in new])
                           for d in (1, 2, 3)}
    out["per_draw_regression"] = {d: rates([r for r in rows if r["arm"] == "A1" and r["draw"] == d
                                            and r["probe"] in REGRESSION]) for d in (1, 2, 3)}

    # Continuity, next to the follow-up's hand grades.
    follow = {(f["probe"], f["arm"]): {m: f[m] for m in ("n", "main", "exact", "contra")}
              for f in json.load(open(os.path.join(HERE, "..", "memory-probe-recall-fix", "summary.json")))["rows"]}
    out["continuity"] = {p: {arm: {"now": rates([r for r in rows if r["probe"] == p and r["arm"] == arm]),
                                   "follow_up": follow[(p, arm)]}
                             for arm in ("C", "A1")} for p in UNRECORDED_FROZEN}

    # Harm check: regression probes.
    reg = {}
    for p in REGRESSION:
        reg[p] = {}
        for arm in ("C", "A1"):
            sel = [r for r in rows if r["probe"] == p and r["arm"] == arm]
            reg[p][arm] = dict(rates(sel), repeats_wrong_entry=sum(r.get("repeats_wrong_entry", 0) for r in sel))
        reg[p]["flag"] = reg[p]["A1"]["main"] <= reg[p]["C"]["main"] - 3
    out["regression"] = reg
    for measure in ("main", "exact", "contra"):
        a = sum(reg[p]["A1"][measure] for p in REGRESSION)
        c = sum(reg[p]["C"][measure] for p in REGRESSION)
        n = sum(reg[p]["A1"]["n"] for p in REGRESSION)
        out.setdefault("regression_pooled", {})[measure] = {"A1": a, "C": c, "n": n,
                                                            "fisher_p": fisher(a, n - a, c, n - c)}

    # Reading the truth: did the answer read a range holding the truth message?
    for r in rows:
        if r["probe"] in truth:
            r["read_truth"] = any(lo <= truth[r["probe"]] <= hi for lo, hi in read_ranges(r["reads"]))
    out["new_by_read_truth"] = {arm: {str(flag): rates([r for r in rows if r["probe"] in new and r["arm"] == arm
                                                         and r["read_truth"] == flag]) for flag in (True, False)}
                                for arm in ("C", "A1")}

    # Process: per arm and probe set.
    proc = {}
    for arm in ("C", "A1"):
        for name, probes in (("new", new), ("frozen", REGRESSION + UNRECORDED_FROZEN)):
            sel = [r for r in rows if r["arm"] == arm and r["probe"] in probes]
            proc[f"{arm}-{name}"] = {
                "n": len(sel),
                **{f"mean_{k}": round(statistics.mean(r[k] for r in sel), 2)
                   for k in ("steps", "tokens", "bytes_read", "memory_reads", "searches", "index_bytes")},
                "answers_with_a_read": sum(1 for r in sel if r["memory_reads"] > 0),
                "regex_correct": sum(r["regex"] for r in sel),
                "main": sum(r["main"] for r in sel),
            }
    out["process"] = proc
    out["regex_vs_hand_new"] = {arm: {"regex": sum(r["regex"] for r in rows if r["arm"] == arm and r["probe"] in new),
                                      "main": sum(r["main"] for r in rows if r["arm"] == arm and r["probe"] in new),
                                      "agree": sum(r["regex"] == r["main"] for r in rows
                                                   if r["arm"] == arm and r["probe"] in new)}
                                for arm in ("C", "A1")}
    json.dump(out, open(os.path.join(HERE, "analysis.json"), "w"), indent=1)
    print(json.dumps({k: v for k, v in out.items() if k not in ("regression",)}, indent=1))


if __name__ == "__main__":
    main()
