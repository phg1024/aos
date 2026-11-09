"""The feasibility filter and the seeded draw (PLAN.md, "Sampling").

  python3 -I draw.py eligible        # survey.json -> eligible.json (the filter)
  python3 -I draw.py order           # the seeded candidate order, one task per line
  python3 -I draw.py draw            # eligible.json + startcheck.jsonl -> drawn.json

The filter reads survey.json (survey.py's output). The draw shuffles the
sorted eligible list with SEED and walks it in that order; a candidate is
taken if its image passed the start check (startcheck.sh), and skipped,
with the reason kept, if it failed. It stops at 16.
"""

import json
import os
import random
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
SEED = 20261109
DRAW = 16
# The Docker Desktop VM on the run host: 18 CPUs, 8317267968 bytes of memory.
VM_CPUS = 18
VM_MEMORY_MB = 8317267968 / 2**20
MAX_AGENT_TIMEOUT_S = 3600


def criteria(task):
    """The exclusion criteria a task meets, by name; empty if it is eligible."""
    met = []
    if (task["gpus"] or 0) > 0:
        met.append("gpu")
    if (task["memory_mb"] or 0) > VM_MEMORY_MB:
        met.append("memory")
    if (task["cpus"] or 0) > VM_CPUS:
        met.append("cpu")
    if (task["agent_timeout_s"] or 0) > MAX_AGENT_TIMEOUT_S:
        met.append("agent_timeout")
    return met


def eligible():
    survey = json.load(open(os.path.join(HERE, "survey.json")))
    excluded = {t["task"]: criteria(t) for t in survey if criteria(t)}
    counts = {}
    for met in excluded.values():
        for c in met:
            counts[c] = counts.get(c, 0) + 1
    tasks = sorted(t["task"] for t in survey if t["task"] not in excluded)
    return {"seed": SEED, "surveyed": len(survey), "excluded": excluded, "excluded_counts": counts,
            "eligible_count": len(tasks), "eligible": tasks}


def order():
    tasks = json.load(open(os.path.join(HERE, "eligible.json")))["eligible"]
    tasks = sorted(tasks)
    random.Random(SEED).shuffle(tasks)
    return tasks


def draw():
    checks = {}
    for line in open(os.path.join(HERE, "startcheck.jsonl")):
        row = json.loads(line)
        checks[row["task"]] = row
    drawn, skipped = [], []
    for task in order():
        if len(drawn) == DRAW:
            break
        if task not in checks:
            sys.exit(f"draw: {task} has no start check yet")
        if checks[task]["passed"]:
            drawn.append(task)
        else:
            skipped.append({"task": task, "reason": checks[task]["detail"]})
    return {"seed": SEED, "drawn": drawn, "skipped_for_rosetta": skipped}


if __name__ == "__main__":
    what = sys.argv[1] if len(sys.argv) > 1 else ""
    if what == "eligible":
        print(json.dumps(eligible(), indent=1))
    elif what == "order":
        print("\n".join(order()))
    elif what == "draw":
        print(json.dumps(draw(), indent=1))
    else:
        sys.exit(__doc__)
