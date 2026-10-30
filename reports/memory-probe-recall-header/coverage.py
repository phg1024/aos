"""Index recall for the new probes (PLAN.md, "Index recall"), from the
follow-up's index draws: for each probe and draw, the lines whose message
range includes the probe's truth message (range-covered). Writes
coverage.json and coverage.md (each covering line next to the probe's
truth) for the hand grade of fact-covered; the shown column is added from
the header renderer of the frozen commit.

Usage, from this folder:
    python3 -I coverage.py
"""

import json
import os
import re

HERE = os.path.dirname(os.path.abspath(__file__))
INDEXES = os.path.join(HERE, "..", "memory-probe-recall-fix")
# segment -> (from, to) of the probe checkpoints' segments
SEGMENTS = {"art-000414": (1, 57), "art-000507": (58, 105), "art-000557": (106, 194), "art-000646": (195, 249),
            "art-001608": (1, 29), "art-001661": (30, 120), "art-001766": (121, 167),
            "art-003062": (1, 17), "art-003133": (18, 72)}
CHECKPOINT = {"art-000648": ["art-000414", "art-000507", "art-000557", "art-000646"],
              "art-001768": ["art-001608", "art-001661", "art-001766"],
              "art-003135": ["art-003062", "art-003133"]}


def truth_segment(probe):
    seq = int(re.search(r"Truth: #(\d+)", probe["note"]).group(1))
    for segment in CHECKPOINT[probe["checkpoint"]]:
        lo, hi = SEGMENTS[segment]
        if lo <= seq <= hi:
            return seq, segment
    raise ValueError(f"{probe['id']}: truth #{seq} is in no archived segment")


def main():
    probes = json.load(open(os.path.join(HERE, "probes-new.json")))
    draws = {d: json.load(open(os.path.join(INDEXES, f"indexes-d{d}.json"))) for d in (1, 2, 3)}
    rows, md = [], []
    for probe in probes:
        seq, segment = truth_segment(probe)
        row = {"probe": probe["id"], "truth": seq, "segment": segment, "draws": {}}
        md.append(f"## {probe['id']} ({segment} #{seq})\n\nQ: {probe['question']}\n\n{probe['note']}\n")
        for d, indexes in draws.items():
            lines = [l for l in indexes.get(segment, []) if l["from"] <= seq <= l["to"]]
            row["draws"][d] = {"lines_in_index": len(indexes.get(segment, [])), "range_covered": lines}
            for l in lines:
                md.append(f"- d{d} {l['from']}-{l['to']}: {l['text']}")
        md.append("")
        rows.append(row)
    json.dump(rows, open(os.path.join(HERE, "coverage.json"), "w"), indent=1)
    open(os.path.join(HERE, "coverage.md"), "w").write("\n".join(md))
    covered = [r for r in rows if any(v["range_covered"] for v in r["draws"].values())]
    print(f"range-covered in at least one draw: {len(covered)} of {len(rows)}")
    for r in rows:
        print(r["probe"], r["segment"], f"#{r['truth']}", " ".join(f"d{d}:{len(v['range_covered'])}/{v['lines_in_index']}" for d, v in r["draws"].items()))


if __name__ == "__main__":
    main()
