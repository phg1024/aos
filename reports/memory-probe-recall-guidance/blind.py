"""Prepares blind grading (PLAN.md, "Scoring"): every answer of both arms
(A1 and A1+G), shuffled with random.Random(20261031) and stripped of arm,
draw, sample, condition, reads and header fields, to
grading/blind-<probe>.json as {gid, probe, question, answer}. The key from
gid to arm, draw, sample and file is written to grading/key.json, which
graders do not open.

Usage, from this folder, after the run:
    python3 -I blind.py
"""

import glob
import json
import os
import random

HERE = os.path.dirname(os.path.abspath(__file__))


def main():
    questions = {}
    for path in (os.path.join(HERE, "..", "memory-probe-recall-header", "probes-new.json"),
                 os.path.join(HERE, "probes-negative.json")):
        for probe in json.load(open(path)):
            questions[probe["id"]] = probe["question"]
    answers = []
    for path in sorted(glob.glob(os.path.join(HERE, "samples-*.jsonl"))):
        name = os.path.basename(path)[len("samples-"):-len(".jsonl")]   # A1-d2-n6, A1G-d1-n2
        arm, draw = name.split("-")[0], int(name.split("-")[1][1:])
        for line in open(path):
            s = json.loads(line)
            answers.append({"probe": s["probe"], "arm": arm, "draw": draw, "sample": s["sample"],
                            "file": os.path.basename(path), "answer": s["answer"]})
    random.Random(20261031).shuffle(answers)
    key, by_probe = {}, {}
    for n, a in enumerate(answers, 1):
        gid = f"g{n:04d}"
        key[gid] = {k: a[k] for k in ("probe", "arm", "draw", "sample", "file")}
        by_probe.setdefault(a["probe"], []).append({"gid": gid, "probe": a["probe"],
                                                    "question": questions[a["probe"]], "answer": a["answer"]})
    os.makedirs(os.path.join(HERE, "grading"), exist_ok=True)
    for probe, items in by_probe.items():
        json.dump(items, open(os.path.join(HERE, "grading", f"blind-{probe}.json"), "w"), indent=1, ensure_ascii=False)
    json.dump(key, open(os.path.join(HERE, "grading", "key.json"), "w"), indent=1)
    print(f"{len(answers)} answers, {len(by_probe)} probes")


if __name__ == "__main__":
    main()
