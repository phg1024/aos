"""Writes schedule.json: the frozen order of the baseline's 96 trials (PLAN.md).

Three rounds. Each round runs every drawn task once in each arm, back to
back. The task order is shuffled per round with a fixed seed. In round r
(1-3), the k-th task (k from 0) runs aos first when k + r is odd, and
terminus-2 first otherwise, so each round has 8 pairs of each kind.

Usage, from this folder: python3 -I make_schedule.py > schedule.json
"""

import json
import os

import random

HERE = os.path.dirname(os.path.abspath(__file__))
ARMS = ["aos", "terminus-2"]
ROUNDS = 3
SEED = 20261110


def schedule(tasks):
    trials = []
    for r in range(1, ROUNDS + 1):
        order = sorted(tasks)
        random.Random(SEED + r).shuffle(order)
        for k, task in enumerate(order):
            arms = ARMS if (k + r) % 2 == 1 else ARMS[::-1]
            for arm in arms:
                seq = len(trials) + 1
                trials.append({"seq": seq, "round": r, "task": task, "arm": arm,
                               "job": f"tb2-base-{seq:03d}-{task}-{arm}"})
    return trials


if __name__ == "__main__":
    tasks = json.load(open(os.path.join(HERE, "drawn.json")))["drawn"]
    print(json.dumps({"seed": SEED, "tasks": tasks, "arms": ARMS, "rounds": ROUNDS,
                      "trials": schedule(tasks)}, indent=1))
