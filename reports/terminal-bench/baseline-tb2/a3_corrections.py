"""A3: corrected endings and contention for the records written before A3.

Records in results.jsonl are not edited. For each record without the A3
fields, recompute it from its job folder with collect.py's A3 code and write
seq, ending_corrected, contention_corrected and attribution to
corrections.jsonl. The server's request delta comes from the stored
contention (seq 29 has none, see A2). The frozen ending is recomputed too and
must equal the stored one, apart from the slice's 502s.

    python3 -I reports/terminal-bench/baseline-tb2/a3_corrections.py
"""

import json
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import collect  # noqa: E402

sched = {t["seq"]: t for t in json.load(open(os.path.join(HERE, "schedule.json")))["trials"]}
out = []
for r in collect.jsonl(os.path.join(HERE, "results.jsonl")):
    if "ending_corrected" in r:
        continue
    server = (r.get("contention") or {}).get("server_requests")
    before = json.dumps({"requests": 0})
    after = json.dumps({"requests": server}) if server is not None else "{}"
    t = sched[r["seq"]]
    rec = collect.record(r["seq"], r["round"], r["task"], r["arm"],
                         os.path.join(HERE, "..", "jobs", t["job"]), before, after)
    # The relay slice's line counts were not saved before A2, so the frozen
    # ending recomputes without the slice's 502s; those must be the only gap.
    slice_502 = ((r.get("relay") or {}).get("statuses") or {}).get("502", 0)
    if rec["ending"] != r["ending"] and not (slice_502 and r["ending"] == "infra_error"):
        raise SystemExit(f"seq {r['seq']}: frozen ending recomputes as {rec['ending']}, stored {r['ending']}")
    out.append({"seq": r["seq"], "ending": r["ending"], "ending_corrected": rec["ending_corrected"],
                "contention_corrected": rec["contention_corrected"], "attribution": rec["attribution"]})
with open(os.path.join(HERE, "corrections.jsonl"), "w") as f:
    for row in out:
        f.write(json.dumps(row) + "\n")
for row in out:
    att = row["attribution"] or {}
    if row["ending"] != row["ending_corrected"] or att.get("cancelled_at_end") or att.get("carried_in") \
            or (row["contention_corrected"].get("surplus") not in (0, None)):
        print(row["seq"], row["ending"], "->", row["ending_corrected"], row["contention_corrected"],
              "cancelled", att.get("cancelled_at_end"), "carried_in", att.get("carried_in"))
print(len(out), "corrections written")
