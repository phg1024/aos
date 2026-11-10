"""Order of the pi arm's 48 trials (PLAN-pi.md, "Trials and order").

Option A: pi only, 3 rounds; each round is a fresh permutation of the 16
drawn tasks, drawn from one random.Random(SEED) in round order. Writes
schedule-pi.json. Stdlib only.

    python3 -I reports/terminal-bench/baseline-tb2/make_schedule_pi.py
"""

import json
import os
import random

HERE = os.path.dirname(os.path.abspath(__file__))
SEED = 20261120  # not 20261111-20261113: the baseline shuffled round r with 20261110 + r
ROUNDS = 3


def main():
    tasks = sorted(json.load(open(os.path.join(HERE, "drawn.json")))["drawn"])
    rng = random.Random(SEED)
    trials = []
    for round_ in range(1, ROUNDS + 1):
        order = tasks[:]
        rng.shuffle(order)
        for task in order:
            seq = len(trials) + 1
            trials.append({"seq": seq, "round": round_, "task": task, "arm": "pi",
                           "job": f"tb2-pi-{seq:03d}-{task}-pi"})
    out = {"seed": SEED, "option": "A", "trials": trials}
    with open(os.path.join(HERE, "schedule-pi.json"), "w") as f:
        json.dump(out, f, indent=1)
        f.write("\n")
    for t in trials:
        print(t["seq"], t["round"], t["task"])


if __name__ == "__main__":
    main()
