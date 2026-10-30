"""Step 1 of the fact selection (PLAN.md, "New probes"): lists, for each
of the 9 archived segments, the candidate messages a new unrecorded-fact
probe may be drawn from, in a fixed seeded order. It reads only the run's
archive, the probe checkpoints and their working memory; it never opens an
agent index.

A candidate is a tool result (role "tool") of a tool that reports what
happened (run, read_file, list_dir, search), not a write, edit, memory
call, error or notice, whose content is at least 80 characters and holds
an outcome: a decimal number, or one of the outcome words in OUTCOME.
Messages within 2 of a message a frozen probe's note cites are excluded.
Each segment's candidates are shuffled with random.Random("a1-facts-" +
segment) and written in that order to candidates.json; the probe writer
takes them in order (PLAN.md).

Also writes context/<checkpoint>.txt, the probe checkpoint's working
memory and live history as text, which the absence check searches.

Usage, from this folder:
    python3 -I select_candidates.py
"""

import json
import os
import random
import re

HERE = os.path.dirname(os.path.abspath(__file__))
PAYLOADS = "/Users/peihongguo/Code/aos/reports/pathtracer-bunny-phase3/.aos-state/artifacts/payloads"

# segment -> probe checkpoint, from the first A1 plan
SEGMENTS = {
    "art-000414": "art-000648", "art-000507": "art-000648", "art-000557": "art-000648", "art-000646": "art-000648",
    "art-001608": "art-001768", "art-001661": "art-001768", "art-001766": "art-001768",
    "art-003062": "art-003135", "art-003133": "art-003135",
}
PROBES = os.path.join(HERE, "..", "..", "bench", "memory", "pathtracer-bunny-phase3-own-memory-probes.json")
REPORTING_TOOLS = ("run:", "read_file:", "list_dir:", "search")
OUTCOME = re.compile(r"\d+\.\d+|(?i:\b(error|errors|failed|fail|denied|not permitted|identical|differs|pass|wins|regress\w*)\b)")


def text(path):
    return json.load(open(os.path.join(PAYLOADS, path + ".json")))


def frozen_truth():
    """Message numbers within 2 of any message a frozen probe's note cites,
    per probe checkpoint: excluded so new facts are distinct from old ones."""
    probes = json.load(open(PROBES))
    probes = probes if isinstance(probes, list) else probes["probes"]
    near = {}
    for probe in probes:
        for cited in re.findall(r"#(\d+)", probe["note"]):
            near.setdefault(probe["checkpoint"], set()).update(range(int(cited) - 2, int(cited) + 3))
    return near


def main():
    near = frozen_truth()
    out = {}
    os.makedirs(os.path.join(HERE, "context"), exist_ok=True)
    for checkpoint in sorted(set(SEGMENTS.values())):
        session = text(checkpoint)
        memory = text(session["working_memory"])
        with open(os.path.join(HERE, "context", checkpoint + ".txt"), "w") as f:
            f.write("== working memory " + session["working_memory"] + " ==\n")
            f.write(json.dumps(memory, indent=1, ensure_ascii=False) + "\n")
            f.write("== live history ==\n")
            for message in session["messages"]:
                f.write(f"#{message['seq']} {message['role']}: {message['content']}\n")
    for segment, checkpoint in SEGMENTS.items():
        excluded = near.get(checkpoint, set())
        candidates = []
        for message in text(segment)["messages"]:
            seq, content = int(message["seq"]), message["content"]
            if message["role"] != "tool" or seq in excluded or len(content) < 80:
                continue
            if not content.startswith(REPORTING_TOOLS) or not OUTCOME.search(content):
                continue
            candidates.append({"seq": seq, "content": content})
        random.Random("a1-facts-" + segment).shuffle(candidates)
        out[segment] = {"checkpoint": checkpoint, "candidates": candidates}
        print(f"{segment} ({checkpoint}): {len(candidates)} candidates, first {[c['seq'] for c in candidates[:6]]}")
    with open(os.path.join(HERE, "candidates.json"), "w") as f:
        json.dump(out, f, indent=1, ensure_ascii=False)


if __name__ == "__main__":
    main()
