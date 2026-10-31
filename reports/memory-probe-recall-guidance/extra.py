"""Descriptive checks of the guidance test, run after analyze.py and not
part of the frozen plan (the report labels them post hoc): per-probe
results, grader group f alone and the primary test without its probes,
memory calls behind negative claims, the h-3062-2 strict reading, the
harm-set misses and the artifact_id read. Writes extra.json.

Usage, from this folder:
    python3 -I extra.py
"""

import glob
import json
import os
import random
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import analyze  # noqa: E402

GROUP_F = ("h-1661-3", "n-1661-1")


def rows():
    analyze.checkpoints()
    recs = analyze.samples()
    key = json.load(open(os.path.join(HERE, "grading", "key.json")))
    grades = {}
    for path in glob.glob(os.path.join(HERE, "grading", "grades-*.json")):
        grades.update(json.load(open(path)))
    cells = {(c["probe"], c["draw"]): c for c in json.load(open(os.path.join(HERE, "cells.json")))}
    out = []
    for gid, k in key.items():
        s = recs[(k["file"], k["probe"], k["sample"])]
        g, c = grades[gid], cells[(k["probe"], k["draw"])]
        out.append(dict(k, gid=gid, set=c["set"], index_grade=c["grade"], shown=c["shown"],
                        main=g["main"], exact=g["exact"], contra=g["contra"], negative_claim=g.get("negative_claim", 0),
                        note=g.get("note", ""), searches=s.get("searches", 0), memory_reads=s.get("memory_reads", 0),
                        tokens=s.get("tokens", 0), reads=s.get("reads") or []))
    return out


def count(sel, arm, m):
    return sum(r[m] for r in sel if r["arm"] == arm)


def main():
    rs = rows()
    target = [r for r in rs if r["set"] == "target"]
    harm = [r for r in rs if r["set"] == "harm"]
    out = {}
    out["per_probe_target"] = {
        p: {arm: {m: count([r for r in target if r["probe"] == p], arm, m) for m in ("main", "exact", "contra", "negative_claim")}
            | {"n": sum(1 for r in target if r["probe"] == p and r["arm"] == arm)} for arm in analyze.ARMS}
        for p in sorted({r["probe"] for r in target})}
    # 95% interval resampling whole probes (the frozen interval resamples answers within cells).
    per, rng, diffs = out["per_probe_target"], random.Random(20261031), []
    for _ in range(analyze.BOOTSTRAPS):
        pick = [rng.choice(list(per)) for _ in per]
        diffs.append((sum(per[p]["A1G"]["main"] - per[p]["A1"]["main"] for p in pick)) / sum(per[p]["A1"]["n"] for p in pick))
    diffs.sort()
    out["probe_bootstrap_ci95"] = [diffs[int(0.025 * analyze.BOOTSTRAPS)], diffs[int(0.975 * analyze.BOOTSTRAPS) - 1]]
    f_rows = [r for r in rs if r["probe"] in GROUP_F]
    out["group_f"] = {arm: {m: count(f_rows, arm, m) for m in ("main", "exact", "contra", "negative_claim")}
                      | {"n": sum(1 for r in f_rows if r["arm"] == arm)} for arm in analyze.ARMS}
    without_f = [r for r in target if r["probe"] not in GROUP_F]
    d, p = analyze.permutation_p(without_f)
    out["primary_without_group_f"] = {"A1": count(without_f, "A1", "main"), "A1G": count(without_f, "A1G", "main"),
                                      "n": sum(1 for r in without_f if r["arm"] == "A1"), "difference": d, "permutation_p": p}
    # Negative claims by what the answer did first: no memory call, searched only, or read memory.
    out["negative_claims_by_calls"] = {
        arm: {"none": sum(1 for r in target if r["arm"] == arm and r["negative_claim"] and not r["searches"] and not r["memory_reads"]),
              "searched, no read": sum(1 for r in target if r["arm"] == arm and r["negative_claim"] and r["searches"] and not r["memory_reads"]),
              "read memory": sum(1 for r in target if r["arm"] == arm and r["negative_claim"] and r["memory_reads"])}
        for arm in analyze.ARMS}
    out["target_answers_by_calls"] = {
        arm: {"none": sum(1 for r in target if r["arm"] == arm and not r["searches"] and not r["memory_reads"]),
              "searched": sum(1 for r in target if r["arm"] == arm and r["searches"]),
              "main_when_none": sum(r["main"] for r in target if r["arm"] == arm and not r["searches"] and not r["memory_reads"]),
              "main_when_searched": sum(r["main"] for r in target if r["arm"] == arm and r["searches"])}
        for arm in analyze.ARMS}
    # h-3062-2 strict reading: main 1 needs 10/10; answers whose grade note says the 10/10 is missing become 0.
    strict = [dict(r, main=0) if r["probe"] == "h-3062-2" and r["main"] and not r["exact"] else r for r in target]
    d, p = analyze.permutation_p(strict)
    out["primary_h3062_2_strict"] = {"A1": count(strict, "A1", "main"), "A1G": count(strict, "A1G", "main"),
                                     "difference": d, "permutation_p": p,
                                     "changed": {arm: sum(1 for r in target if r["probe"] == "h-3062-2" and r["main"] and not r["exact"] and r["arm"] == arm)
                                                 for arm in analyze.ARMS}}
    out["harm_misses_and_contra"] = [{k: r[k] for k in ("gid", "probe", "draw", "arm", "main", "exact", "contra", "note")}
                                     for r in harm if not r["main"] or r["contra"]]
    out["artifact_id_reads"] = [{k: r[k] for k in ("gid", "probe", "draw", "arm", "main", "exact", "contra", "reads")}
                                for r in rs if any(x.startswith("read_memory artifact_id=") for x in r["reads"])]
    json.dump(out, open(os.path.join(HERE, "extra.json"), "w"), indent=1)
    print(json.dumps({k: v for k, v in out.items() if k != "per_probe_target"}, indent=1))
    for p, v in out["per_probe_target"].items():
        print(p, "A1", v["A1"]["main"], "/", v["A1"]["n"], "neg", v["A1"]["negative_claim"],
              "| A1G", v["A1G"]["main"], "/", v["A1G"]["n"], "neg", v["A1G"]["negative_claim"])


if __name__ == "__main__":
    main()
