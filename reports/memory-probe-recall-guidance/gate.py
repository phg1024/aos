"""Gates G1 and G2 of the guidance test (PLAN.md, "Gates"), from the
header dumps of the frozen binary's headercheck, without a model call.

G1: with the guidance off, every header (system prompt and task header,
per draw and checkpoint) is byte-identical to 6c55bdb's
(reference-6c55bdb/), and the same lines are shown and hidden per segment.
G2: with the guidance on, the same lines are shown and hidden; the header
differs from the guidance-off one only by inserted text (nothing removed
or changed); S1 is inserted in every header and S2 exactly where some
index lines are hidden.

Exit status 0 if both hold. First output line: "PASS ..." or "FAIL ...".

Usage, from this folder, after run.sh's headercheck step:
    python3 -I gate.py
"""

import difflib
import json
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
REF, OFF, ON = (os.path.join(HERE, d) for d in ("reference-6c55bdb", "headers-off", "headers-on"))
# Distinctive parts of the two sentences, from aos code's commit (PLAN.md).
S1_MARKER = "__S1_MARKER__"
S2_MARKER = "__S2_MARKER__"


def shown(path):
    h = json.load(open(os.path.join(path, "header.json")))
    return {f: {c: {s: (v["shown"], v["hidden"]) for s, v in x["by_segment"].items()} for c, x in by.items()}
            for f, by in h.items()}


def main():
    failures, notes = [], []
    names = sorted(n for n in os.listdir(REF) if n.endswith(".txt"))
    ref_shown, off_shown, on_shown = shown(REF), shown(OFF), shown(ON)
    if off_shown != ref_shown:
        failures.append("G1: guidance-off lines shown/hidden differ from 6c55bdb")
    if on_shown != ref_shown:
        failures.append("G2: guidance-on lines shown/hidden differ from 6c55bdb")
    for name in names:
        ref = open(os.path.join(REF, name), "rb").read()
        off = open(os.path.join(OFF, name), "rb").read()
        on = open(os.path.join(ON, name), "rb").read().decode()
        if off != ref:
            failures.append(f"G1: {name} guidance-off header differs from 6c55bdb")
        changed = [op for op in difflib.SequenceMatcher(None, off.decode(), on, autojunk=False).get_opcodes()
                   if op[0] != "equal"]
        inserted = "".join(on[j1:j2] for tag, _, _, j1, j2 in changed if tag == "insert")
        if any(tag != "insert" for tag, *_ in changed):
            failures.append(f"G2: {name} guidance-on header removes or changes text")
        file, checkpoint = name[:-len(".txt")].rsplit(".", 1)
        hidden = any(h > 0 for _, h in ref_shown[file][checkpoint].values())
        if S1_MARKER not in inserted:
            failures.append(f"G2: {name} has no S1")
        if (S2_MARKER in inserted) != hidden:
            failures.append(f"G2: {name} S2 {'missing' if hidden else 'present'} with {'some' if hidden else 'no'} lines hidden")
        notes.append(f"{name}: +{len(inserted)} bytes inserted, lines hidden: {hidden}")
    result = "FAIL" if failures else "PASS"
    print(f"{result}: {len(names)} headers; G1 guidance-off identical to 6c55bdb, G2 guidance-on inserts only S1/S2")
    for line in failures + notes:
        print(line)
    sys.exit(1 if failures else 0)


if __name__ == "__main__":
    main()
