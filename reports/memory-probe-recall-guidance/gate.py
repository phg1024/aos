"""Gates G1 and G2 of the guidance test (PLAN.md, "Gates"), from the
header dumps of the frozen binary's headercheck, without a model call.

G1: with the guidance off, every header (system prompt and task header,
per draw and checkpoint) is byte-identical to 6c55bdb's
(reference-6c55bdb/), and the same lines are shown and hidden per segment.
G2: with the guidance on, the same lines are shown and hidden; the header
is the guidance-off one with S1 inserted (every header) and S2 inserted
(exactly where some index lines are hidden), and nothing else.

Exit status 0 if both hold. First output line: "PASS ..." or "FAIL ...".

Usage, from this folder, after run.sh's headercheck step:
    python3 -I gate.py
"""

import json
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
REF, OFF, ON = (os.path.join(HERE, d) for d in ("reference-6c55bdb", "headers-off", "headers-on"))
# The two sentences, from 283a92e (internal/agentflow/backend.go).
S1_MARKER = ("This index is partial: a fact missing from it may still be in the archive. "
             "Before you answer that something was never done, measured or seen, search_memory for it.")
S2_MARKER = ("Some segments show only their first lines; \"(+K more lines ...)\" marks the rest, "
             "and read_memory artifact_id=<segment> returns the whole index.")


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
        file, checkpoint = name[:-len(".txt")].rsplit(".", 1)
        hidden = any(h > 0 for _, h in ref_shown[file][checkpoint].values())
        has_s1, has_s2 = on.count(" " + S1_MARKER) == 1, on.count(" " + S2_MARKER) == 1
        if not has_s1:
            failures.append(f"G2: {name} has no S1")
        if has_s2 != hidden:
            failures.append(f"G2: {name} S2 {'missing' if hidden else 'present'} with {'some' if hidden else 'no'} lines hidden")
        # Taking the sentences out must give back the guidance-off header, byte for byte.
        if on.replace(" " + S1_MARKER, "", 1).replace(" " + S2_MARKER, "", 1) != off.decode():
            failures.append(f"G2: {name} differs from the guidance-off header by more than S1/S2")
        notes.append(f"{name}: +{len(on) - len(off.decode())} bytes, S1 {has_s1}, S2 {has_s2}, lines hidden: {hidden}")
    result = "FAIL" if failures else "PASS"
    print(f"{result}: {len(names)} headers; G1 guidance-off identical to 6c55bdb, G2 guidance-on inserts only S1/S2")
    for line in failures + notes:
        print(line)
    sys.exit(1 if failures else 0)


if __name__ == "__main__":
    main()
