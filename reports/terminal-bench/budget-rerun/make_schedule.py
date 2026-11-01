"""Writes schedule.json: the frozen order of the budget rerun's trials.

Three rounds. Each round runs every task once in every arm. The task order
is shuffled per round with a fixed seed. Within a task, the arm order rotates
so that over the three rounds each arm takes each position (first, second,
third) once for that task. Time of day and server load therefore fall on
every arm alike.

Usage, from this folder: python3 -I make_schedule.py > schedule.json
"""

import json
import random

TASKS = [
    "fix-git", "prove-plus-comm", "cobol-modernization", "openssl-selfsigned-cert",
    "nginx-request-logging", "polyglot-c-py", "vulnerable-secret", "raman-fitting",
]
ARMS = ["off", "strict", "advisory"]
ROUNDS = 3
SEED = 20261031


def schedule():
    trials = []
    for r in range(1, ROUNDS + 1):
        order = TASKS[:]
        random.Random(SEED + r).shuffle(order)
        for task in order:
            shift = (r - 1 + TASKS.index(task)) % len(ARMS)
            for arm in ARMS[shift:] + ARMS[:shift]:
                seq = len(trials) + 1
                trials.append({"seq": seq, "round": r, "task": task, "arm": arm,
                               "job": f"tb2-budget-{seq:03d}-{task}-{arm}"})
    return trials


if __name__ == "__main__":
    print(json.dumps({"seed": SEED, "tasks": TASKS, "arms": ARMS, "rounds": ROUNDS,
                      "trials": schedule()}, indent=1))
