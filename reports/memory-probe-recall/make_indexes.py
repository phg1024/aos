"""Turns handoffs.jsonl into indexes-d1..3.json: for each draw, the index
lines the agent wrote at each original segment's handoff, keyed by that
segment's ID. A line is kept only if it lies wholly in the original
segment's range (PLAN.md); dropped lines are counted in indexes.log.

Usage, from this folder:
    python3 -I make_indexes.py
"""

import json
import os

HERE = os.path.dirname(os.path.abspath(__file__))

# re-run checkpoint -> (original segment, from_seq, to_seq), from PLAN.md
SEGMENTS = {
    "art-000410": ("art-000414", 1, 57),
    "art-000503": ("art-000507", 58, 105),
    "art-000553": ("art-000557", 106, 194),
    "art-000642": ("art-000646", 195, 249),
    "art-001605": ("art-001608", 1, 29),
    "art-001657": ("art-001661", 30, 120),
    "art-001762": ("art-001766", 121, 167),
    "art-003059": ("art-003062", 1, 17),
    "art-003129": ("art-003133", 18, 72),
}


def main():
    handoffs = [json.loads(line) for line in open(os.path.join(HERE, "handoffs.jsonl"))]
    # A re-run of a failed draw is appended after it with the same
    # checkpoint and sample; the last successful attempt is the draw's.
    chosen = {}
    for h in handoffs:
        if not h.get("error"):
            chosen[(h["checkpoint"], h["sample"])] = h
    log = []
    for draw in (1, 2, 3):
        out = {}
        for checkpoint, (segment, lo, hi) in SEGMENTS.items():
            h = chosen.get((checkpoint, draw))
            if h is None:
                log.append(f"d{draw} {segment}: no successful handoff, no index")
                continue
            lines = h.get("agent_index") or []
            kept = [line for line in lines if lo <= line["from"] <= line["to"] <= hi]
            out[segment] = kept
            log.append(f"d{draw} {segment} ({lo}-{hi}): handoff archived {h['from_seq']}-{h['to_seq']}, "
                       f"{len(lines)} lines, kept {len(kept)}, dropped {len(lines) - len(kept)}")
        with open(os.path.join(HERE, f"indexes-d{draw}.json"), "w") as f:
            json.dump(out, f, indent=1)
    with open(os.path.join(HERE, "indexes.log"), "w") as f:
        f.write("\n".join(log) + "\n")
    print("\n".join(log))


if __name__ == "__main__":
    main()
