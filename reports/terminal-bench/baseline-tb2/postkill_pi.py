"""Model calls made after Harbor's agent timeout, and a sensitivity check.

Added after round 1 of the pi run, as Book's review asked; it changes no
record and no score. The relay logs each request's start time, written when
its reply ends. For every harbor_timeout trial this lists the POSTs that
started after the agent's end and before the trial finished: offset from the
agent's end, status, message count, and whether it started inside the
verifier's window. It counts the same for the baseline's harbor_timeout
trials, and reruns P1 and P2 (same exact test, same Holm step) with the pi
passes that had post-kill calls counted as failures. Writes postkill-pi.json.
Stdlib only.

    python3 -I reports/terminal-bench/baseline-tb2/postkill_pi.py
"""

import datetime
import glob
import json
import os
import re
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import analyze  # noqa: E402
import analyze_pi  # noqa: E402
import collect  # noqa: E402

JOBS = os.path.join(HERE, "..", "jobs")


def ts(s):
    return datetime.datetime.fromisoformat(s.replace("Z", "+00:00"))


def posts(log):
    out = []
    for line in open(os.path.join(HERE, log)):
        p = line.split()
        if len(p) > 3 and p[1] == "POST":
            m = re.search(r"messages=(\d+)", line)
            out.append({"start": ts(p[0]), "status": p[3], "messages": int(m.group(1)) if m else None})
    return out


def session_messages(tdir):
    n = 0
    for f in glob.glob(os.path.join(tdir, "agent", "pi", "sessions", "*.jsonl")):
        for line in open(f):
            try:
                n += json.loads(line).get("type") == "message"
            except ValueError:
                pass
    return n


def windows(rows, job_of):
    """Each trial's result.json times, in run order."""
    out = []
    for r in rows:
        path = glob.glob(os.path.join(JOBS, job_of(r), "*", "result.json"))[0]
        res = json.load(open(path))
        v = res.get("verifier") or {}
        out.append({"row": r, "dir": os.path.dirname(path),
                    "a_start": ts(res["agent_execution"]["started_at"]),
                    "a_end": ts(res["agent_execution"]["finished_at"]), "end": ts(res["finished_at"]),
                    "v": (ts(v["started_at"]), ts(v["finished_at"])) if v.get("started_at") else None})
    return out


def late(w, ps, nxt):
    """POSTs started after the agent's end, up to the next trial's agent start."""
    stop = nxt["a_start"] if nxt else w["end"] + datetime.timedelta(minutes=10)
    out = []
    for p in ps:
        if w["a_end"] < p["start"] < stop:
            off = (p["start"] - w["a_end"]).total_seconds()
            out.append({"offset_s": round(off, 1), "status": p["status"], "messages": p["messages"],
                        "before_trial_end": p["start"] <= w["end"],
                        "in_verifier": bool(w["v"] and w["v"][0] <= p["start"] <= w["v"][1])})
    return out


def main():
    pi = collect.jsonl(os.path.join(HERE, "results-pi.jsonl"))
    sched_pi = {t["seq"]: t["job"] for t in json.load(open(os.path.join(HERE, "schedule-pi.json")))["trials"]}
    ws = windows(pi, lambda r: sched_pi[r["seq"]])
    ps = posts("relay-pi.log")
    table = []
    for i, w in enumerate(ws):
        r = w["row"]
        if r["ending"] != "harbor_timeout":
            continue
        lp = late(w, ps, ws[i + 1] if i + 1 < len(ws) else None)
        v = w["v"]
        table.append({"seq": r["seq"], "task": r["task"], "reward": r["reward"],
                      "verifier_s": [round((v[0] - w["a_end"]).total_seconds(), 1),
                                     round((v[1] - w["a_end"]).total_seconds(), 1)] if v else None,
                      "trial_end_s": round((w["end"] - w["a_end"]).total_seconds(), 1),
                      "session_messages": session_messages(w["dir"]), "late_posts": lp})
    active = {t["seq"] for t in table if t["late_posts"]}

    base = collect.corrected_rows(collect.jsonl(os.path.join(HERE, "results.jsonl")))
    sched = {t["seq"]: t["job"] for t in json.load(open(os.path.join(HERE, "schedule.json")))["trials"]}
    bws = windows(base, lambda r: sched[r["seq"]])
    bps = posts("relay.log")
    baseline = {}
    for i, w in enumerate(bws):
        r = w["row"]
        if r["ending"] != "harbor_timeout":
            continue
        n = len(late(w, bps, bws[i + 1] if i + 1 < len(bws) else None))
        b = baseline.setdefault(r["arm"], {"harbor_timeout_trials": 0, "late_posts": 0, "trials_with_late_posts": 0})
        b["harbor_timeout_trials"] += 1
        b["late_posts"] += n
        b["trials_with_late_posts"] += n > 0

    tasks = json.load(open(os.path.join(HERE, "drawn.json")))["drawn"]
    flipped = [s for s in sorted(active) if any(r["seq"] == s and r["reward"] == 1 for r in pi)]
    pi_s = [dict(r, reward=0.0) if r["seq"] in flipped else r for r in pi]
    sens = {"P1": analyze.perm_test(base + pi_s, tasks, "pi", "terminus-2"),
            "P2": analyze.perm_test(base + pi_s, tasks, "pi", "aos")}
    adj = analyze_pi.holm({k: v["p_two_sided"] for k, v in sens.items()})
    for k in sens:
        sens[k].update(adj[k])

    out = {"relay_time": "request start, written when the reply ends",
           "pi": table, "pi_trials_with_late_posts": sorted(active),
           "pi_late_posts": sum(len(t["late_posts"]) for t in table),
           "pi_late_posts_in_verifier": sum(p["in_verifier"] for t in table for p in t["late_posts"]),
           "baseline": baseline,
           "sensitivity": {"label": "sensitivity: pi passes with post-kill calls counted as failures",
                           "flipped": flipped, "successes": int(sum(r["reward"] or 0 for r in pi_s)), **sens}}
    json.dump(out, open(os.path.join(HERE, "postkill-pi.json"), "w"), indent=1)

    for t in table:
        if t["late_posts"]:
            print(t["seq"], t["task"], t["reward"], t["verifier_s"], t["session_messages"],
                  [(p["offset_s"], p["status"], p["messages"], p["in_verifier"]) for p in t["late_posts"]])
    print("pi timeouts", len(table), "with late posts", sorted(active), "baseline", baseline)
    for k, v in sens.items():
        print("sensitivity", k, f"{v['diff']:+.4f} p {v['p']:.4f} holm {v['p_holm']:.4f} reject {v['reject']}")


if __name__ == "__main__":
    main()
