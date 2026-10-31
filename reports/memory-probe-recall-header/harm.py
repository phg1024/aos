"""Harm check, per answer (PLAN.md, "Harm check"): which A1 answers could
have taken a fact from a shown index line without reading what the line
cites, and the A1 answers graded contra, each with the shown lines of its
draw that cover the probe's truth message. Writes harm.json (counts) and
harm.md (the answers to read by hand, with their lines).

For a new probe, an A1 answer "used a shown line without reading" when it
is main 1, its draw's hand grade says a shown line states the fact, and
no read_memory call covered the truth message. Searches are counted
separately: a search hit shows a snippet of the message itself.

Usage, from this folder, after grading:
    python3 -I harm.py
"""

import json
import os
import re
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)  # python3 -I leaves this folder off the path
from analyze import read_ranges, samples  # noqa: E402
from primary import load  # noqa: E402

CHECKPOINT_SEGMENTS = {"art-000648": ["art-000414", "art-000507", "art-000557", "art-000646"],
                       "art-001768": ["art-001608", "art-001661", "art-001766"],
                       "art-003135": ["art-003062", "art-003133"]}


def main():
    rows = load()
    recs = samples()
    probes = {p["id"]: p for p in json.load(open(os.path.join(HERE, "probes-new.json")))}
    probes.update({p["id"]: p for p in json.load(open(os.path.join(
        HERE, "..", "..", "bench", "memory", "pathtracer-bunny-phase3-own-memory-probes.json")))})
    indexes = {d: json.load(open(os.path.join(HERE, f"indexes-d{d}.json"))) for d in (1, 2, 3)}
    grades = json.load(open(os.path.join(HERE, "coverage_grades.json")))
    out, md = {"new": {}, "frozen": {}}, ["# A1 answers for the harm check\n"]
    for r in sorted((r for r in rows if r["arm"] == "A1"), key=lambda r: (r["probe"], r["draw"], r["sample"])):
        s = recs[(r["file"], r["probe"], r["sample"])]
        probe = probes[r["probe"]]
        kind = "new" if r["probe"] in grades else "frozen"
        truths = [int(t) for t in re.findall(r"#(\d+)", probe["note"].split("Truth:")[-1])[:1]]
        shown = s.get("agent_index_shown") or {}
        lines = [dict(l, segment=seg) for seg in CHECKPOINT_SEGMENTS[probe["checkpoint"]]
                 for l in indexes[r["draw"]].get(seg, [])[:shown.get(seg, 0)]]
        covering = [l for l in lines if any(l["from"] <= t <= l["to"] for t in truths)]
        ranges = read_ranges(s.get("reads"))
        read_truth = any(lo <= t <= hi for lo, hi in ranges for t in truths)
        searched = s.get("searches", 0) > 0
        if kind == "new":
            g = grades[r["probe"]][str(r["draw"])]
            stated_shown = g["grade"] == "states" and g["shown"]
            cell = ("read the truth" if read_truth else "read other messages" if s.get("memory_reads", 0)
                    else "searched only" if searched else "no memory call")
            key = f"main{r['main']}-contra{r['contra']}-{'stated_shown' if stated_shown else 'not_stated_shown'}"
            out["new"].setdefault(key, {}).setdefault(cell, 0)
            out["new"][key][cell] += 1
        else:
            cell = "read" if s.get("memory_reads", 0) else ("searched only" if searched else "no memory call")
            key = f"main{r['main']}-contra{r['contra']}"
            out["frozen"].setdefault(r["probe"], {}).setdefault(key, {}).setdefault(cell, 0)
            out["frozen"][r["probe"]][key][cell] += 1
        if r["contra"] or (kind == "new" and r["main"] and not read_truth):
            md.append(f"## {r['probe']} d{r['draw']} s{r['sample']} ({r['gid']}): main {r['main']} exact {r['exact']} "
                      f"contra {r['contra']}; reads {s.get('reads') or []}\n")
            md.append(f"Grader: {r.get('note', '')}\n")
            md.append("Shown lines covering the truth: " + ("none" if not covering else ""))
            md.extend(f"- {l['segment']} {l['from']}-{l['to']}: {l['text']}" for l in covering)
            md.append(f"\nAnswer:\n\n> " + s["answer"].replace("\n", "\n> ") + "\n")
    json.dump(out, open(os.path.join(HERE, "harm.json"), "w"), indent=1)
    open(os.path.join(HERE, "harm.md"), "w").write("\n".join(md))
    print(json.dumps(out, indent=1))


if __name__ == "__main__":
    main()
