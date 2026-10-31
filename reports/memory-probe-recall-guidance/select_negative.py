"""Candidates for the negative-claim probes (PLAN.md, "Negative-claim
probes"): per archived segment, the results of `run` tool calls (commands
the agent ran: builds, benchmarks, checks), at least 80 characters,
excluding messages within 2 of any message an existing probe's note cites
(the 11 frozen probes and the header re-test's 23). Each segment's list is
shuffled with random.Random("a1-negative-" + segment) and written in that
order to negative-candidates.json, with the assistant turn that issued the
command. It reads only the run's archive and the probe files; it never
opens an agent index.

Usage, from this folder:
    python3 -I select_negative.py
"""

import json
import os
import random
import re

HERE = os.path.dirname(os.path.abspath(__file__))
PAYLOADS = "/Users/peihongguo/Code/aos/reports/pathtracer-bunny-phase3/.aos-state/artifacts/payloads"
SEGMENTS = {
    "art-000414": "art-000648", "art-000507": "art-000648", "art-000557": "art-000648", "art-000646": "art-000648",
    "art-001608": "art-001768", "art-001661": "art-001768", "art-001766": "art-001768",
    "art-003062": "art-003135", "art-003133": "art-003135",
}
PROBE_FILES = [os.path.join(HERE, "..", "..", "bench", "memory", "pathtracer-bunny-phase3-own-memory-probes.json"),
               os.path.join(HERE, "..", "memory-probe-recall-header", "probes-new.json")]


def near_existing():
    near = {}
    for path in PROBE_FILES:
        for probe in json.load(open(path)):
            for cited in re.findall(r"#(\d+)", probe["note"]):
                near.setdefault(probe["checkpoint"], set()).update(range(int(cited) - 2, int(cited) + 3))
    return near


def main():
    near = near_existing()
    out = {}
    for segment, checkpoint in SEGMENTS.items():
        messages = json.load(open(os.path.join(PAYLOADS, segment + ".json")))["messages"]
        by_seq = {m["seq"]: m for m in messages}
        candidates = []
        for m in messages:
            if m["role"] != "tool" or not m["content"].startswith("run:") or len(m["content"]) < 80:
                continue
            if m["seq"] in near.get(checkpoint, set()):
                continue
            issued = by_seq.get(m["seq"] - 1, {})
            candidates.append({"seq": m["seq"], "issued_by": issued.get("content", ""), "content": m["content"]})
        random.Random("a1-negative-" + segment).shuffle(candidates)
        out[segment] = {"checkpoint": checkpoint, "candidates": candidates}
        print(segment, len(candidates))
    json.dump(out, open(os.path.join(HERE, "negative-candidates.json"), "w"), indent=1, ensure_ascii=False)


if __name__ == "__main__":
    main()
