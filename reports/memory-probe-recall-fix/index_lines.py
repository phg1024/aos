"""Draws the index lines to grade (PLAN.md, "Index quality", unchanged
from the first plan): 40 lines uniformly from all kept lines of all
draws, with random.Random(20261030) over the lines sorted by (draw,
segment, from, to, text), plus every line in any draw that cites a truth
message of an unrecorded probe. Writes index_lines.json (the lines) and
index_lines.md (each line with the archived messages it cites, for
grading by hand), and prints the per-index statistics.

Usage, from this folder:
    python3 -I index_lines.py
"""

import json
import os
import random

HERE = os.path.dirname(os.path.abspath(__file__))
ARCHIVE = "/Users/peihongguo/Code/aos/reports/pathtracer-bunny-phase3/.aos-state/artifacts/payloads"

# unrecorded probe -> (segment, truth messages), from the probe file
TRUTH = {
    "r2-verify-dir": ("art-000414", {37, 38}),
    "r5-register-counts": ("art-001661", {31, 32, 33, 34, 35}),
    "r8-sibling-pairs": ("art-003133", {29, 31}),
}
SEGMENT_RANGES = {"art-000414": (1, 57), "art-000507": (58, 105), "art-000557": (106, 194), "art-000646": (195, 249),
                  "art-001608": (1, 29), "art-001661": (30, 120), "art-001766": (121, 167),
                  "art-003062": (1, 17), "art-003133": (18, 72)}
MAX_MESSAGE = 1500


def main():
    lines = []
    for draw in (1, 2, 3):
        for segment, kept in json.load(open(os.path.join(HERE, f"indexes-d{draw}.json"))).items():
            for line in kept:
                lines.append({"draw": draw, "segment": segment, "from": line["from"], "to": line["to"], "text": line["text"]})
    lines.sort(key=lambda l: (l["draw"], l["segment"], l["from"], l["to"], l["text"]))
    drawn = random.Random(20261030).sample(range(len(lines)), 40)
    chosen = {i: ["sample"] for i in drawn}
    for i, line in enumerate(lines):
        for probe, (segment, truth) in TRUTH.items():
            if line["segment"] == segment and any(line["from"] <= m <= line["to"] for m in truth):
                chosen.setdefault(i, []).append(probe)
    out = []
    md = []
    for n, i in enumerate(sorted(chosen)):
        line = dict(lines[i], id=n + 1, why=chosen[i])
        out.append(line)
        segment = json.load(open(os.path.join(ARCHIVE, line["segment"] + ".json")))
        md.append(f"## L{n + 1} d{line['draw']} {line['segment']} {line['from']}-{line['to']} ({', '.join(line['why'])})\n")
        md.append(f"> {line['text']}\n")
        for message in segment["messages"]:
            if line["from"] <= int(message["seq"]) <= line["to"]:
                content = message["content"]
                cut = content[:MAX_MESSAGE] + (f" ... [{len(content)} chars]" if len(content) > MAX_MESSAGE else "")
                md.append(f"#{message['seq']} {message['role']}: {cut}\n")
        md.append("")
    json.dump(out, open(os.path.join(HERE, "index_lines.json"), "w"), indent=1)
    open(os.path.join(HERE, "index_lines.md"), "w").write("\n".join(md))

    print(f"{len(lines)} kept lines in all; {len(out)} to grade "
          f"({sum('sample' in l['why'] for l in out)} drawn, "
          f"{sum(any(w != 'sample' for w in l['why']) for l in out)} citing a truth message)")
    for draw in (1, 2, 3):
        for segment, kept in json.load(open(os.path.join(HERE, f"indexes-d{draw}.json"))).items():
            lo, hi = SEGMENT_RANGES[segment]
            covered = set()
            for line in kept:
                covered.update(range(line["from"], line["to"] + 1))
            size = sum(len(f"{l['from']}-{l['to']}: {l['text']}") + 1 for l in kept)
            print(f"d{draw} {segment}: {len(kept)} lines, ~{size} B, cites {len(covered)}/{hi - lo + 1} messages")


if __name__ == "__main__":
    main()
