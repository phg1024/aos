"""A2: record seq 29 from its job folder, once, after the collector crash.

Seq 29 ran to the end, but collect.py crashed parsing its ledger, so run.sh
stopped before writing the record. Its before/after backend strings were not
saved, so the server-side request count is lost: contention is recorded as
an error. The relay slice is relay.log lines 671-752 (1-based), between the
status checks at 04:40:25Z and 05:19:49Z. Uses the fixed collect.py.

    python3 -I reports/terminal-bench/baseline-tb2/a2_record_seq29.py
"""

import json
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import collect  # noqa: E402

SEQ = 29
BEFORE, AFTER = json.dumps({"relay_lines": 670}), json.dumps({"relay_lines": 752})

trial = next(t for t in json.load(open(os.path.join(HERE, "schedule.json")))["trials"] if t["seq"] == SEQ)
path = os.path.join(HERE, "results.jsonl")
if any(r["seq"] == SEQ for r in collect.jsonl(path)):
    raise SystemExit("seq 29 is already recorded")
rec = collect.record(SEQ, trial["round"], trial["task"], trial["arm"],
                     os.path.join(HERE, "..", "jobs", trial["job"]), BEFORE, AFTER)
rec["contention"] = {"error": "backend counts not saved"}
rec["a2"] = "recorded after the run paused; relay slice from relay.log lines 671-752 by timestamp"
with open(path, "a") as out:
    out.write(json.dumps(rec, default=str) + "\n")
print(f"  reward {rec['reward']} {rec['ending']} agent {rec.get('agent_s')}s "
      f"posts {(rec.get('relay') or {}).get('posts')} contention {rec['contention']}")
reason = collect.stop_rule(path)
print("STOP:", reason) if reason else None
