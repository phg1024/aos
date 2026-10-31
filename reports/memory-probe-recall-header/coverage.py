"""Index recall for the new probes and gates G2 and G3 (PLAN.md), from a
folder's index draws (indexes-d1..3.json) and the header the frozen commit
renders for them (header.json, from headercheck). For each probe and draw:
the lines whose message range includes the probe's truth message
(range-covered), and which of them the header shows (it shows an index's
first lines). Writes coverage<suffix>.json and coverage<suffix>.md (each
covering line next to the probe's truth) for the hand grade of
fact-covered, and gates<suffix>.json.

G2: every segment with an index in a draw shows at least one line at every
probe checkpoint that holds it, under the pinned budget (BUDGET bytes). G3: at least 5 new probes are
range-covered by a shown line in at least one draw. Exit status 0 if both
hold (or if no header file is given), 1 otherwise.

Usage, from this folder:
    python3 -I coverage.py <indexes folder> [header.json] [suffix]
The re-test's run: python3 -I coverage.py . header.json
The draws of b87b59a, graded before the re-freeze (no header):
    python3 -I coverage.py ../memory-probe-recall-fix "" -old
"""

import json
import os
import re
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
# segment -> (from, to) of the probe checkpoints' segments
SEGMENTS = {"art-000414": (1, 57), "art-000507": (58, 105), "art-000557": (106, 194), "art-000646": (195, 249),
            "art-001608": (1, 29), "art-001661": (30, 120), "art-001766": (121, 167),
            "art-003062": (1, 17), "art-003133": (18, 72)}
CHECKPOINT = {"art-000648": ["art-000414", "art-000507", "art-000557", "art-000646"],
              "art-001768": ["art-001608", "art-001661", "art-001766"],
              "art-003135": ["art-003062", "art-003133"]}
G3_MIN = 5
BUDGET = 6144


def truth_segment(probe):
    seq = int(re.search(r"Truth: #(\d+)", probe["note"]).group(1))
    for segment in CHECKPOINT[probe["checkpoint"]]:
        lo, hi = SEGMENTS[segment]
        if lo <= seq <= hi:
            return seq, segment
    raise ValueError(f"{probe['id']}: truth #{seq} is in no archived segment")


def shown_counts(header, draw, checkpoint):
    """Lines shown per segment in this draw's header at this checkpoint."""
    by_segment = header[f"indexes-d{draw}.json"][checkpoint]["by_segment"]
    return {segment: v["shown"] for segment, v in by_segment.items()}


def main():
    folder = os.path.join(HERE, sys.argv[1])
    header = json.load(open(os.path.join(HERE, sys.argv[2]))) if len(sys.argv) > 2 and sys.argv[2] else None
    suffix = sys.argv[3] if len(sys.argv) > 3 else ""
    probes = json.load(open(os.path.join(HERE, "probes-new.json")))
    draws = {d: json.load(open(os.path.join(folder, f"indexes-d{d}.json"))) for d in (1, 2, 3)}
    rows, md = [], []
    for probe in probes:
        seq, segment = truth_segment(probe)
        row = {"probe": probe["id"], "truth": seq, "segment": segment, "draws": {}}
        md.append(f"## {probe['id']} ({segment} #{seq})\n\nQ: {probe['question']}\n\n{probe['note']}\n")
        for d, indexes in draws.items():
            index = indexes.get(segment, [])
            shown = shown_counts(header, d, probe["checkpoint"]).get(segment, 0) if header else None
            covering = []
            for n, l in enumerate(index):
                if l["from"] <= seq <= l["to"]:
                    covering.append(dict(l, position=n, shown=None if header is None else n < shown))
            row["draws"][d] = {"lines_in_index": len(index), "lines_shown": shown, "range_covered": covering}
            for l in covering:
                mark = "" if header is None else (" [shown]" if l["shown"] else " [hidden]")
                md.append(f"- d{d} {l['from']}-{l['to']}{mark}: {l['text']}")
        md.append("")
        rows.append(row)
    json.dump(rows, open(os.path.join(HERE, f"coverage{suffix}.json"), "w"), indent=1)
    open(os.path.join(HERE, f"coverage{suffix}.md"), "w").write("\n".join(md))
    covered = [r for r in rows if any(v["range_covered"] for v in r["draws"].values())]
    print(f"range-covered in at least one draw: {len(covered)} of {len(rows)}")
    for r in rows:
        print(r["probe"], r["segment"], f"#{r['truth']}", " ".join(
            f"d{d}:{len(v['range_covered'])}/{v['lines_in_index']}"
            + ("" if header is None else f" shown {sum(1 for l in v['range_covered'] if l['shown'])}")
            for d, v in r["draws"].items()))
    if header is None:
        return
    # G2: no segment with an index shows nothing.
    silent = [f"d{d} {checkpoint} {segment}"
              for d, indexes in draws.items() for checkpoint, segments in CHECKPOINT.items()
              for segment in segments
              if indexes.get(segment) and shown_counts(header, d, checkpoint).get(segment, 0) == 0]
    shown_covered = [r["probe"] for r in rows
                     if any(l["shown"] for v in r["draws"].values() for l in v["range_covered"])]
    budgets = sorted({h["budget"] for by_checkpoint in header.values() for h in by_checkpoint.values()})
    gates = {"G2": {"pass": not silent and budgets == [BUDGET], "segments_showing_nothing": silent, "budgets": budgets},
             "G3": {"pass": len(shown_covered) >= G3_MIN, "probes_covered_by_a_shown_line": shown_covered,
                    "need": G3_MIN}}
    json.dump(gates, open(os.path.join(HERE, f"gates{suffix}.json"), "w"), indent=1)
    print(f"G2 {'PASS' if gates['G2']['pass'] else 'FAIL'}: {len(silent)} indexed segments show nothing {silent}, "
          f"header budget {budgets} (pinned {BUDGET})")
    print(f"G3 {'PASS' if gates['G3']['pass'] else 'FAIL'}: {len(shown_covered)} of {len(rows)} new probes "
          f"range-covered by a shown line (need {G3_MIN})")
    sys.exit(0 if gates["G2"]["pass"] and gates["G3"]["pass"] else 1)


if __name__ == "__main__":
    main()
