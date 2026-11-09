"""Replays every aos trial (PLAN.md, "Replay"), on a copy of its state.

For each aos record in results.jsonl (or smoke.jsonl with --smoke) that has
no line in replay.jsonl yet, it copies the trial's aos-state into
.cache/replay-baseline/<job>/state, runs the pinned host build's
`aos replay --json --keep <dir> <copy>`, and appends one line: the exit code
(0 identical, 1 diverged, 2 refused), the report's counts and verdict, and
for 1 or 2 the report's divergence or reason. Paths are left out. The job
folders are never changed. Stdlib only.

    python3 -I replay.py [--smoke]
"""

import json
import os
import shutil
import subprocess
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.abspath(os.path.join(HERE, "..", "..", ".."))
JOBS = os.path.join(ROOT, "reports", "terminal-bench", "jobs")
AOS = os.path.join(ROOT, ".cache", "harbor-dist-v0.1.10", "aos-host-darwin-arm64")
WORK = os.path.join(ROOT, ".cache", "replay-baseline")
DROP = ("state_dir", "replay_dir")


def main():
    smoke = "--smoke" in sys.argv
    src = os.path.join(HERE, "smoke.jsonl" if smoke else "results.jsonl")
    out = os.path.join(HERE, "replay-smoke.jsonl" if smoke else "replay.jsonl")
    schedule = json.load(open(os.path.join(HERE, "schedule.json")))["trials"] if not smoke else []
    jobs = {t["seq"]: t["job"] for t in schedule}
    done = {json.loads(line)["seq"] for line in open(out)} if os.path.isfile(out) else set()
    for line in open(src):
        rec = json.loads(line)
        if rec["arm"] != "aos" or rec["seq"] in done or not rec.get("trial"):
            continue
        job = jobs.get(rec["seq"]) or f"tb2-base-smoke-{rec['task']}-aos"
        state = os.path.join(JOBS, job, rec["trial"], "agent", "aos-state")
        work = os.path.join(WORK, job)
        shutil.rmtree(work, ignore_errors=True)
        shutil.copytree(state, os.path.join(work, "state"))
        proc = subprocess.run([AOS, "replay", "--json", "--keep", os.path.join(work, "keep"),
                               os.path.join(work, "state")], capture_output=True, text=True,
                              env={k: v for k, v in os.environ.items()
                                   if k not in ("ANTHROPIC_API_KEY", "AOS_LLM_API_KEY")})
        try:
            report = json.loads(proc.stdout)
            report = {k: v for k, v in report.items() if k not in DROP}
        except ValueError:
            report = {"stdout": proc.stdout[-500:], "stderr": proc.stderr[-500:]}
        row = {"seq": rec["seq"], "task": rec["task"], "trial": rec["trial"], "exit": proc.returncode,
               "identical": report.get("identical"), "report": report}
        with open(out, "a") as f:
            f.write(json.dumps(row) + "\n")
        print(rec["seq"], rec["task"], "exit", proc.returncode, "identical", report.get("identical"))


if __name__ == "__main__":
    main()
