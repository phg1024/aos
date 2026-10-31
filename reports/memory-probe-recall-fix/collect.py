"""Merges the A1 draws into one sample list per arm, numbering A1 samples
1..9 as (draw - 1) * 3 + sample, and writes answers.md, every answer with
its reads, for hand-reading.

Usage, from this folder:
    python3 -I collect.py
"""

import json
import os

HERE = os.path.dirname(os.path.abspath(__file__))


def load_arms():
    arms = {"C": [json.loads(line) for line in open(os.path.join(HERE, "samples-C.jsonl"))], "A1": []}
    for draw in (1, 2, 3):
        for line in open(os.path.join(HERE, f"samples-A1-d{draw}.jsonl")):
            sample = json.loads(line)
            sample["draw"] = draw
            sample["sample"] = (draw - 1) * 3 + sample["sample"]
            arms["A1"].append(sample)
    return arms


def main():
    arms = load_arms()
    probes = []
    for s in arms["C"]:
        if s["probe"] not in probes:
            probes.append(s["probe"])
    with open(os.path.join(HERE, "answers.md"), "w") as out:
        for probe in probes:
            for arm in ("C", "A1"):
                for s in sorted((s for s in arms[arm] if s["probe"] == probe), key=lambda s: s["sample"]):
                    draw = f" d{s['draw']}" if "draw" in s else ""
                    out.write(f"## {probe} {arm} #{s['sample']}{draw} steps={s['steps']} tokens={s.get('tokens', 0)} "
                              f"frozen={'correct' if s['correct'] else '-'}{' superseded' if s['superseded'] else ''}"
                              f"{' error=' + s['error'] if s.get('error') else ''}\n")
                    for read in s.get("reads") or []:
                        out.write(f"- {read}\n")
                    out.write("\n" + s["answer"].strip() + "\n\n")
    print({arm: len(samples) for arm, samples in arms.items()})


if __name__ == "__main__":
    main()
