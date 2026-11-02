"""Reconstruction re-check on the ca78ad9 trials (reports/replay-check/PLAN.md).

Answers the frozen probe's Q1-Q9b on the three replay-check trials in two
ways, and gets their ground truth by the probe's rules:

  (a) "as before": the frozen records-only method of
      reports/record-reconstruction-probe/reconstruct.py, unchanged except
      that the two-phase ledger is read as one call per start line, with
      latency, usage and error from the outcome line that names it;
  (b) "with the new records": the signal exit, start and outcome lines,
      run.started and the changes on run results.

    python3 -I reports/replay-check/recheck.py

Writes recheck.jsonl next to this script. The frozen scripts are imported,
not edited.
"""

import glob
import json
import os
import re
import sys
from datetime import datetime, timedelta

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.abspath(os.path.join(HERE, "..", ".."))
sys.path.insert(0, os.path.join(ROOT, "reports", "record-reconstruction-probe"))
import reconstruct as R  # noqa: E402

JOBS = os.path.join(ROOT, "reports", "terminal-bench", "jobs")
SRC = os.path.join(ROOT, ".cache", "replay-ca78ad9")
RELAY_LOG = os.path.join(HERE, "relay.log")
TRIALS = [("tb2-replay-fix-git-strict", "fix-git", "strict"),
          ("tb2-replay-vulnerable-secret-strict", "vulnerable-secret", "strict"),
          ("tb2-replay-cobol-modernization-off", "cobol-modernization", "off")]
SIGNAL = "terminated by signal"

# (a) reads the ledger through the frozen script, with the two-phase lines
# folded into one record per call. inspect is the ca78ad9 build, since the
# v0.1.1 one would count both lines of each call.
R.AOS = os.path.join(SRC, ".cache", "aos-host")
_plain_jsonl = R.jsonl


def folded_ledger(path):
    lines = _plain_jsonl(path)
    if not any(l.get("phase") for l in lines):
        return lines
    outcomes = {l.get("start"): l for l in lines if l.get("phase") == "outcome"}
    calls = []
    for start in (l for l in lines if l.get("phase") == "start"):
        out = outcomes.get(start["seq"])
        call = dict(start)
        if out is not None:
            for key in ("time", "latency_ms", "prompt_tokens", "cached_tokens", "completion_tokens", "error"):
                call[key] = out.get(key)
        else:
            call["in_flight"] = True
        calls.append(call)
    return calls


def jsonl_for_a(path):
    return folded_ledger(path) if path.endswith("calls.jsonl") else _plain_jsonl(path)


def parse_time(text):
    return R.parse_time(text)


def new_records(state, task):
    """(b): the answers the ca78ad9 records give directly."""
    events = json.load(open(os.path.join(state, "kernel", "events.json")))
    processes = json.load(open(os.path.join(state, "kernel", "processes.json")))
    index = json.load(open(os.path.join(state, "artifacts", "index.json")))
    lines = _plain_jsonl(os.path.join(state, "run", "calls.jsonl"))
    trace = _plain_jsonl(os.path.join(state, "run", "trace.jsonl"))
    start = parse_time(events[0]["timestamp"])
    agent_pid = next(p for p, v in processes.items() if (v.get("role") or "").startswith("agent:"))
    exits = {e["process_id"]: e for e in events if e["type"] == "process.exited"}
    agent_exit = exits.get(agent_pid)
    reason = (agent_exit or {}).get("data", {}).get("reason") or ""
    if agent_exit is None:
        end_class = "not_ended"
    elif SIGNAL in reason:
        end_class = "not_ended"  # stopped from outside, by a signal
    elif R.BUDGET_EXIT in reason:
        end_class = "budget_exit"
    elif str(agent_exit["data"].get("code")) == "0":
        end_class = "agent_exit"
    else:
        end_class = "error_exit"
    starts = [l for l in lines if l.get("phase") == "start"]
    outcomes = [l for l in lines if l.get("phase") == "outcome"]
    named = {o.get("start") for o in outcomes}
    in_flight = [s["seq"] for s in starts if s["seq"] not in named]
    run_started = next((e for e in events if e["type"] == "run.started"), None)
    rs = (run_started or {}).get("data", {})
    budget_s = int(rs.get("budget_seconds") or 0)
    kind = "off" if budget_s == 0 else ("strict" if rs.get("budget_strict_time") == "true" else "advisory")

    # Q9 from the changes recorded on run results, and write_file/edit_file.
    paths = R.ANSWERS.get(task)
    first = {}
    changes_seen = 0
    for art, meta in sorted(index.items(), key=lambda kv: kv[1]["created_at"]):
        if meta["type"] != "tool_result":
            continue
        p = json.load(open(os.path.join(state, "artifacts", "payloads", art + ".json")))
        out = p.get("output") if isinstance(p.get("output"), dict) else {}
        touched = []
        ch = out.get("changes") or p.get("changes")
        if isinstance(ch, dict):
            changes_seen += 1
            root = ch.get("root") or ""
            touched = [os.path.join(root, x) for x in (ch.get("created") or []) + (ch.get("modified") or [])]
        if p.get("tool") in ("write_file", "edit_file") and out.get("path"):
            touched.append(str(out.get("path")))
        for path in paths or []:
            hit = any(t == path or (not path.startswith("/") and t.endswith("/" + path)) for t in touched)
            if hit and path not in first:
                first[path] = parse_time(meta["created_at"])
    if paths is None:
        q9, status = None, "n/a"
    elif len(first) < len(paths):
        q9, status = None, "not written"
    else:
        q9, status = (max(first.values()) - start).total_seconds(), "written"

    end = parse_time(agent_exit["timestamp"]) if agent_exit else parse_time(trace[-1]["time"])
    checkpoint = (agent_exit or {}).get("data", {}).get("checkpoint")
    return {
        "q1_end": end_class, "exit_reason": reason, "exit_code": (agent_exit or {}).get("data", {}).get("code"),
        "exit_checkpoint": checkpoint,
        "exit_checkpoint_exists": bool(checkpoint and checkpoint in index and index[checkpoint]["type"] == "checkpoint"),
        "all_processes_exited": all(p in exits for p in processes),
        "q2_stop": reason if SIGNAL in reason else None,
        "q3_starts": len(starts), "q3_outcomes": len(outcomes), "q3_in_flight": in_flight,
        "q4_tokens": {k: sum(o.get(k) or 0 for o in outcomes)
                      for k in ("prompt_tokens", "cached_tokens", "completion_tokens")},
        "q5_agent_s": (end - start).total_seconds(),
        "q6_last_record": (trace[-1].get("kind"), (trace[-1].get("detail") or "")[:160]) if trace else None,
        "q6_last_call_start": parse_time(starts[-1]["time"]).isoformat() if starts else None,
        "q8_kind": kind, "q8_budget_s": budget_s or None, "run_started": rs,
        "q9_first_answer_s": q9, "q9_status": status, "results_with_changes": changes_seen,
    }


def truth(job_dir, posts, next_start):
    tdir = [os.path.join(job_dir, d) for d in os.listdir(job_dir) if os.path.isdir(os.path.join(job_dir, d))][0]
    r = json.load(open(os.path.join(tdir, "result.json")))
    exc = (r.get("exception_info") or {}).get("exception_type")
    meta = (r.get("agent_result") or {}).get("metadata") or {}
    a0, a1 = parse_time(r["agent_execution"]["started_at"]), parse_time(r["agent_execution"]["finished_at"])
    if exc == "AgentTimeoutError":
        end = "not_ended"
    elif meta.get("aos_exit") == "0":
        end = "agent_exit"
    elif meta.get("aos_exit") == "1" and (a1 - a0).total_seconds() >= 877:
        end = "budget_exit"
    else:
        end = "error_exit"
    mine = [t for t in posts if t >= a0 and (next_start is None or t < next_start)]
    reward = ((r.get("verifier_result") or {}).get("rewards") or {}).get("reward")
    return tdir, {"q1_end": end, "exception": exc, "aos_exit": meta.get("aos_exit"),
                  "q3_relay_posts": len(mine), "q5_agent_s": (a1 - a0).total_seconds(),
                  "agent_start": a0.isoformat(), "agent_end": a1.isoformat(),
                  "posts": [t.isoformat() for t in mine],
                  "posts_after_agent_end": [t.isoformat() for t in mine if t > a1],
                  "reward": reward}


def main():
    posts = []
    for line in open(RELAY_LOG):
        when, method, path, _ = line.split()
        if method == "POST" and path == "/v1/chat/completions":
            posts.append(parse_time(when))
    dirs = [os.path.join(JOBS, job) for job, _, _ in TRIALS]
    starts = []
    for d in dirs:
        t = [os.path.join(d, x) for x in os.listdir(d) if os.path.isdir(os.path.join(d, x))][0]
        starts.append(parse_time(json.load(open(os.path.join(t, "result.json")))["agent_execution"]["started_at"]))
    rows = []
    for i, (job, task, arm) in enumerate(TRIALS):
        tdir, tr = truth(dirs[i], posts, starts[i + 1] if i + 1 < len(starts) else None)
        state = os.path.join(tdir, "agent", "aos-state")
        # (a) the frozen method; the job folder name gives seq and task, so
        # it is called on the state folder through a stand-in name.
        R.jsonl = jsonl_for_a
        try:
            a = answer_a(dirs[i], task)
        finally:
            R.jsonl = _plain_jsonl
        b = new_records(state, task)
        rows.append({"job": job, "task": task, "arm": arm, "a": a, "b": b, "truth": tr})
    for row in rows:
        row["scores"] = score(row)
    with open(os.path.join(HERE, "recheck.jsonl"), "w") as out:
        for row in rows:
            out.write(json.dumps(row) + "\n")
    for row in rows:
        print(row["job"], json.dumps(row["scores"]))


# Q9's ground truth, hand-read from each trace: seconds from Harbor's agent
# start to the result of the first write of the answer path.
HAND_Q9 = {
    "tb2-replay-vulnerable-secret-strict": 37.2,     # write_file /app/results.txt, 18:49:46.091Z
    "tb2-replay-cobol-modernization-off": 305.6,     # write_file /app/program.py, 18:55:24.690Z
}


def score(row):
    """The frozen probe's rules (record-reconstruction-probe/PLAN.md), for
    (a) and (b) against the truth."""
    a, b, t = row["a"], row["b"], row["truth"]
    out = {}
    for name, ans, q1, calls, last_start, budget_kind, q9, q9_status in (
            ("a", a, a["q1_end"], a["q3_ledger_calls"], a["q6_last_call_start"], a["q8_kind"],
             a["q9_first_answer_s"], a["q9_status"]),
            ("b", b, b["q1_end"], b["q3_starts"], b["q6_last_call_start"], b["q8_kind"],
             b["q9_first_answer_s"], b["q9_status"])):
        s = {"Q1": "correct" if q1 == t["q1_end"] else "wrong"}
        if t["q1_end"] == "not_ended":
            s["Q2"] = "correct" if name == "b" and b["q2_stop"] else "not_answerable"
        s["Q3"] = "correct" if calls == t["q3_relay_posts"] else "wrong"
        s["Q5"] = "correct" if abs(ans["q5_agent_s"] - t["q5_agent_s"]) <= 5 else "wrong"
        later = [p for p in t["posts"] if parse_time(p) > parse_time(last_start) + timedelta(seconds=2)]
        s["Q6"] = "correct" if not later else "wrong"
        s["Q8"] = "correct" if budget_kind == row["arm"] else "wrong"
        if row["job"] in HAND_Q9:
            s["Q9"] = ("correct" if q9 is not None and abs(q9 - HAND_Q9[row["job"]]) <= 2 else "wrong")
            if t["reward"] == 1.0:
                s["Q9b"] = "correct" if q9_status == "written" else "wrong"
        out[name] = s
    return out


def answer_a(job_dir, task):
    """reconstruct.answer() reads seq and task from the job folder's name
    (tb2-budget-NNN-<task>-<arm>); these folders are named
    tb2-replay-<task>-<arm>, so a temporary link gives it the shape it
    expects."""
    import tempfile
    with tempfile.TemporaryDirectory() as tmp:
        link = os.path.join(tmp, f"tb2-budget-000-{task}-x")
        os.symlink(job_dir, link)
        return R.answer(link)


if __name__ == "__main__":
    main()
