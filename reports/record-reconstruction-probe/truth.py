"""Ground truth for the record-reconstruction probe (PLAN.md), from outside AOS.

Reads only: Harbor's result.json (exception, agent execution span, reward,
and the aos_exit code the adapter's shell captured), the verifier's
ctrf.json, the budget rerun's relay.log, schedule.json and handcheck.json.
It never opens the aos-state folder or aos.log.

    python3 -I reports/record-reconstruction-probe/truth.py

Writes truth.jsonl next to this script.
"""

import glob
import json
import os
import re
from datetime import datetime

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.abspath(os.path.join(HERE, "..", ".."))
RERUN = os.path.join(ROOT, "reports", "terminal-bench", "budget-rerun")
JOBS = os.path.join(ROOT, "reports", "terminal-bench", "jobs")
BUDGET_S = 879


def parse_time(text):
    if not text:
        return None
    return datetime.fromisoformat(re.sub(r"(\.\d{6})\d+", r"\1", text.replace("Z", "+00:00")))


def relay_posts():
    out = []
    for line in open(os.path.join(RERUN, "relay.log")):
        when, method, path, status = line.split()
        if method == "POST" and path == "/v1/chat/completions":
            out.append((parse_time(when), status))
    return out


def main():
    schedule = {t["seq"]: t for t in json.load(open(os.path.join(RERUN, "schedule.json")))["trials"]}
    hand = {t["seq"]: t for t in json.load(open(os.path.join(RERUN, "handcheck.json")))["trials"]}
    posts = relay_posts()
    trials = {}
    for seq in sorted(schedule):
        job = glob.glob(os.path.join(JOBS, f"tb2-budget-{seq:03d}-*"))[0]
        trial = [d for d in os.listdir(job) if os.path.isdir(os.path.join(job, d))][0]
        trials[seq] = os.path.join(job, trial)
    starts = {}
    results = {}
    for seq, tdir in trials.items():
        results[seq] = json.load(open(os.path.join(tdir, "result.json")))
        starts[seq] = parse_time(results[seq]["agent_execution"]["started_at"])

    rows = []
    for seq, tdir in trials.items():
        r = results[seq]
        arm = schedule[seq]["arm"]
        exc = (r.get("exception_info") or {}).get("exception_type")
        meta = (r.get("agent_result") or {}).get("metadata") or {}
        a0 = starts[seq]
        a1 = parse_time(r["agent_execution"]["finished_at"])
        agent_s = (a1 - a0).total_seconds()
        reward = ((r.get("verifier_result") or {}).get("rewards") or {}).get("reward")

        # Q1, Q2.
        if exc == "AgentTimeoutError":
            end = "not_ended"
        elif meta.get("aos_exit") == "0":
            end = "agent_exit"
        elif meta.get("aos_exit") == "1" and arm == "strict" and agent_s >= BUDGET_S - 2:
            end = "budget_exit"
        else:
            end = "error_exit"

        # Q3, Q6: requests by start time, from this trial's agent start to the
        # next trial's (only the driver's GETs fall between trials).
        nxt = starts.get(seq + 1)
        mine = [(t, s) for t, s in posts if t >= a0 and (nxt is None or t < nxt)]

        # Q10 truth from the verifier.
        ctrf_path = os.path.join(tdir, "verifier", "ctrf.json")
        verifier_lines = []
        if os.path.isfile(ctrf_path):
            for test in json.load(open(ctrf_path))["results"]["tests"]:
                if test["status"] != "passed":
                    verifier_lines += [l.strip() for l in (test.get("trace") or "").splitlines()
                                       if l.startswith("E ")][:1]
        h = hand.get(seq)
        rows.append({
            "seq": seq, "task": schedule[seq]["task"], "arm": arm, "reward": reward,
            "exception": exc, "aos_exit": meta.get("aos_exit"),
            "q1_end": end, "q2_stop": exc if end == "not_ended" else None,
            "q3_relay_posts": len(mine), "q3_relay_non200": sum(1 for _, s in mine if s != "200"),
            "q5_agent_s": agent_s, "agent_start": a0.isoformat(), "agent_end": a1.isoformat(),
            "q6_post_starts": [t.isoformat() for t, _ in mine],
            "q8_kind": arm,
            "q9_first_answer_s": h["first_answer_s"] if h else None,
            "q9b_file_existed": True if reward == 1.0 else None,
            "verifier_failure": bool(h and h.get("verifier_failure")),
            "q10_verifier": verifier_lines,
            "q11_reward": reward,
        })
    with open(os.path.join(HERE, "truth.jsonl"), "w") as out:
        for row in rows:
            out.write(json.dumps(row) + "\n")
    print(f"{len(rows)} trials of ground truth")


if __name__ == "__main__":
    main()
