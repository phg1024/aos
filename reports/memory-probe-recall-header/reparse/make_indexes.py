"""The follow-up's index draws as the 6c55bdb parser would store them:
indexes-d1..3.json from ../../memory-probe-recall-fix/, with each handoff
whose stored index differs under the new parser (reparsed.json) replaced
by its new parse, kept to the original segment's range as
make_indexes.py does. Logs what was replaced.

Usage, from this folder, after main.go:
    python3 -I make_indexes.py
"""

import json
import os

HERE = os.path.dirname(os.path.abspath(__file__))
FIX = os.path.join(HERE, "..", "..", "memory-probe-recall-fix")
SEGMENTS = {
    "art-000410": ("art-000414", 1, 57), "art-000503": ("art-000507", 58, 105),
    "art-000553": ("art-000557", 106, 194), "art-000642": ("art-000646", 195, 249),
    "art-001605": ("art-001608", 1, 29), "art-001657": ("art-001661", 30, 120),
    "art-001762": ("art-001766", 121, 167), "art-003059": ("art-003062", 1, 17),
    "art-003129": ("art-003133", 18, 72),
}


def main():
    reparsed = json.load(open(os.path.join(HERE, "reparsed.json")))
    for draw in (1, 2, 3):
        indexes = json.load(open(os.path.join(FIX, f"indexes-d{draw}.json")))
        for r in reparsed:
            if r["draw"] != draw or (r["same_stored"] and r["same_rejections"]):
                continue
            segment, lo, hi = SEGMENTS[r["checkpoint"]]
            kept = [line for line in r["new_stored"] or [] if lo <= line["from"] <= line["to"] <= hi]
            print(f"d{draw} {segment}: {len(indexes.get(segment, []))} lines under the old parser, "
                  f"{len(kept)} under the new (of {len(r['new_stored'] or [])})")
            indexes[segment] = kept
        json.dump(indexes, open(os.path.join(HERE, f"indexes-d{draw}.json"), "w"), indent=1)


if __name__ == "__main__":
    main()
