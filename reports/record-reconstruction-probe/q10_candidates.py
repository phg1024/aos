"""Aid for the hand-answered Q10 (PLAN.md): from the AOS records only, list
each tool result whose stdout shows the answer, in order, for one trial.

raman-fitting: results that print "G" and "2D" x0 values.
polyglot-c-py: results that print a listing of /app/polyglot.

    python3 -I reports/record-reconstruction-probe/q10_candidates.py SEQ

The reader picks the last candidate after the last write to the answer path
and records the answer in q10_hand.json.
"""

import glob
import json
import os
import re
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
JOBS = os.path.join(HERE, "..", "terminal-bench", "jobs")


def main(seq):
    job = glob.glob(os.path.join(JOBS, f"tb2-budget-{seq:03d}-*"))[0]
    state = glob.glob(os.path.join(job, "*", "agent", "aos-state"))[0]
    index = json.load(open(os.path.join(state, "artifacts", "index.json")))
    for art, meta in sorted(index.items(), key=lambda kv: kv[1]["created_at"]):
        if meta["type"] != "tool_result":
            continue
        p = json.load(open(os.path.join(state, "artifacts", "payloads", art + ".json")))
        out = p.get("output") if isinstance(p.get("output"), dict) else {}
        cmd = str(out.get("command") or out.get("path") or "")
        stdout = str(out.get("stdout") or "")
        writes = "results.json" in cmd or "main.py.c" in cmd
        if "raman" in job:
            x0 = re.findall(r'"?x0"?\s*[:=]\s*([-\d.eE+]+)', stdout)
            if x0 or writes:
                print(meta["created_at"][11:19], art, p.get("tool"), "x0:", x0[:4], "| cmd:", cmd[:90].replace("\n", " "))
        else:
            listing = [l for l in stdout.splitlines() if re.search(r"(cmain|main\.py\.c|__pycache__)\s*$", l)]
            if ("ls" in cmd and "/app/polyglot" in cmd) or listing:
                print(meta["created_at"][11:19], art, p.get("tool"), "listing:", [l.split()[-1] for l in listing],
                      "| cmd:", cmd[:90].replace("\n", " "))


if __name__ == "__main__":
    main(int(sys.argv[1]))
