"""Records-only answers for the record-reconstruction probe (PLAN.md).

For each of the 72 budget-rerun trials, this reads only the trial's
agent/aos-state folder and the output of `aos inspect --json --tail -1`
(v0.1.1) run on a copy of it, and answers questions Q1 to Q9b and Q11.
It never opens result.json, the verifier's output, aos.log, relay.log or
any budget-rerun result file. The job folder's name gives the trial's seq
and task, as an operator would know which task a state folder belongs to.

    python3 -I reports/record-reconstruction-probe/reconstruct.py

Writes records.jsonl next to this script.
"""

import glob
import json
import os
import re
import shutil
import subprocess
import tempfile
from datetime import datetime, timedelta

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.abspath(os.path.join(HERE, "..", ".."))
JOBS = os.path.join(ROOT, "reports", "terminal-bench", "jobs")
AOS = os.path.join(ROOT, ".cache", "bin", "aos-v0.1.1")

TIME_NOTE = re.compile(r"^budget time=(\d+)s used=(\d+)s left=(\d+)s")
STRICT_SUFFIX = "(strict: the run is stopped when it runs out)"
TIME_WARNING = "budget warning: less than 10% of the time is left"
BUDGET_EXIT = "time budget exceeded"
PREFIX = re.compile(r"^\[#\d+\] ")

# The frozen write rule of the budget-rerun plan ("Candidate answer: frozen
# definition"), copied: it reads only trace and ledger records.
ANSWERS = {
    "polyglot-c-py": ["/app/polyglot/main.py.c"],
    "raman-fitting": ["/app/results.json"],
    "vulnerable-secret": ["/app/results.txt"],
    "cobol-modernization": ["/app/program.py"],
    "prove-plus-comm": ["plus_comm.v", "plus_comm.vo"],
    "openssl-selfsigned-cert": ["/app/ssl/server.key", "/app/ssl/server.crt", "/app/ssl/server.pem",
                                "/app/ssl/verification.txt", "/app/check_cert.py"],
    "fix-git": None,
    "nginx-request-logging": None,
}
WRITE_OPS = re.compile(r"(>>?|\btee\b|\bcp\b|\bmv\b|\s-o\s|\s-out\s|\bcoqc\b|\binstall\b)")
TOOL_KINDS = ("tool_result", "tool_error")


def parse_time(text):
    if not text:
        return None
    return datetime.fromisoformat(re.sub(r"(\.\d{6})\d+", r"\1", text.replace("Z", "+00:00")))


def jsonl(path):
    out = []
    if os.path.isfile(path):
        for line in open(path):
            try:
                out.append(json.loads(line))
            except json.JSONDecodeError:
                pass
    return out


def text_of(message):
    content = message.get("content")
    if isinstance(content, list):
        content = " ".join(part.get("text", "") for part in content if isinstance(part, dict))
    return content or ""


def envelopes(calls):
    out = []
    for call in calls:
        for message in call.get("messages") or []:
            if message.get("role") != "assistant":
                continue
            try:
                action = json.loads(text_of(message)).get("action") or {}
            except (json.JSONDecodeError, AttributeError):
                continue
            if action.get("type") == "tool":
                tc = action.get("tool_call") or {}
                out.append({"name": tc.get("name"), "args": tc.get("args") or {}})
    return out


def tool_steps(calls, trace):
    full = envelopes(calls)
    steps = []
    for i, rec in enumerate(trace):
        if rec.get("kind") != "tool_call":
            continue
        outcome = next((r for r in trace[i + 1:] if r.get("kind") in TOOL_KINDS or r.get("kind") == "tool_call"), None)
        n = len(steps)
        if n < len(full):
            name, args = full[n]["name"], full[n]["args"]
        else:
            detail = rec.get("detail") or ""
            name, args = detail.split(" ", 1)[0], {"_detail": detail}
        steps.append({"name": name, "args": args,
                      "outcome": outcome.get("kind") if outcome and outcome.get("kind") in TOOL_KINDS else None,
                      "time": parse_time((outcome or rec).get("time"))})
    return steps


def writes_answer(step, path):
    name, args = step["name"], step["args"]
    if "_detail" in args:
        detail = args["_detail"]
        return path in detail and (name in ("write_file", "edit_file") or bool(WRITE_OPS.search(detail)))
    if name in ("write_file", "edit_file"):
        target = str(args.get("path") or "")
        return target == path or (not path.startswith("/") and target.endswith("/" + path))
    if name == "run":
        command = str(args.get("command") or "")
        return path in command and bool(WRITE_OPS.search(command))
    return False


def answer_timing(task, steps, start):
    paths = ANSWERS.get(task)
    if paths is None:
        return None, "n/a"
    first = {}
    for step in steps:
        if step["outcome"] != "tool_result":
            continue
        for path in paths:
            if path not in first and writes_answer(step, path):
                first[path] = step["time"]
    if len(first) < len(paths):
        return None, "not written"
    return (max(first.values()) - start).total_seconds(), "written"


def call_start(call):
    return parse_time(call["time"]) - timedelta(milliseconds=call.get("latency_ms") or 0)


def inspect(state):
    """aos inspect on a copy, so that nothing can touch the original."""
    with tempfile.TemporaryDirectory() as tmp:
        copy = os.path.join(tmp, "aos-state")
        shutil.copytree(state, copy)
        out = subprocess.run([AOS, "inspect", "--json", "--tail", "-1", copy],
                             capture_output=True, text=True, check=True).stdout
    return json.loads(out)


def answer(job_dir):
    name = os.path.basename(job_dir)
    seq = int(name.split("-")[2])
    task = "-".join(name.split("-")[3:-1])
    trial = [d for d in os.listdir(job_dir) if os.path.isdir(os.path.join(job_dir, d))][0]
    state = os.path.join(job_dir, trial, "agent", "aos-state")
    events = json.load(open(os.path.join(state, "kernel", "events.json")))
    processes = json.load(open(os.path.join(state, "kernel", "processes.json")))
    index = json.load(open(os.path.join(state, "artifacts", "index.json")))
    trace = jsonl(os.path.join(state, "run", "trace.jsonl"))
    calls = jsonl(os.path.join(state, "run", "calls.jsonl"))
    insp = inspect(state)

    def payload(art):
        return json.load(open(os.path.join(state, "artifacts", "payloads", art + ".json")))

    agent_pid = next(p for p, v in processes.items() if (v.get("role") or "").startswith("agent:"))
    start = parse_time(events[0]["timestamp"])
    exit_event = next((e for e in events if e["type"] == "process.exited" and e["process_id"] == agent_pid), None)

    # Q1, Q2: how the run ended, and what ended it.
    if exit_event is None:
        q1 = "not_ended"
        exit_reason = None
    else:
        data = exit_event["data"]
        exit_reason = data.get("reason") or ""
        if BUDGET_EXIT in exit_reason:
            q1 = "budget_exit"
        elif str(data.get("code")) == "0":
            q1 = "agent_exit"
        else:
            q1 = "error_exit"
    outside_stop = [e for e in events if e["type"] not in (
        "process.spawned", "process.transition", "process.exited", "memory.header")]

    # Q3, Q4: requests and tokens.
    dial_errors = sum(1 for c in calls if "dial tcp" in (c.get("error") or ""))
    tokens = {k: sum(c.get(k) or 0 for c in calls) for k in ("prompt_tokens", "cached_tokens", "completion_tokens")}

    # Q5: duration.
    end = parse_time(exit_event["timestamp"]) if exit_event else parse_time(trace[-1]["time"]) if trace else start

    # Q6: the last thing recorded.
    last = trace[-1] if trace else {}
    last_call_start = call_start(calls[-1]) if calls else None

    # Q7: refusals.
    later_user_text = []
    for call in calls:
        when = call_start(call)
        text = " ".join(text_of(m) for m in call.get("messages") or [] if m.get("role") in ("user", "tool"))
        later_user_text.append((when, text))
    refusals = []
    for art, meta in sorted(index.items()):
        if meta["type"] != "tool_error":
            continue
        err = payload(art)
        created = parse_time(meta["created_at"])
        needle = (err.get("error") or "")[:60]
        told = any(when >= created - timedelta(seconds=1) and needle and needle in text
                   for when, text in later_user_text)
        refusals.append({"artifact": art, "tool": err.get("tool"), "error": (err.get("error") or "")[:200],
                         "at_s": (created - start).total_seconds(), "told_in_ledger": told})
    trace_errors = [r for r in trace if r.get("kind") == "tool_error"]
    refusal_events = [e for e in events if "tool" in e["type"] or "refus" in e["type"] or "error" in e["type"]]

    # Q8: budget notes and the warning, from the ledger; and the checkpoint.
    notes = []
    for call in calls:
        for m in call.get("messages") or []:
            if m.get("role") in ("user", "tool"):
                notes.append((call_start(call), PREFIX.sub("", text_of(m))))
    time_notes = [(w, t) for w, t in notes if TIME_NOTE.match(t)]
    warning = next((w for w, t in notes if t.startswith(TIME_WARNING)), None)
    if not time_notes:
        kind, budget_s = "off", None
    else:
        kind = "strict" if STRICT_SUFFIX in time_notes[0][1] else "advisory"
        budget_s = int(TIME_NOTE.match(time_notes[0][1]).group(1))
    checkpoints = sorted((meta["created_at"], art) for art, meta in index.items() if meta["type"] == "checkpoint")
    last_cp = payload(checkpoints[-1][1]) if checkpoints else {}

    # Q9, Q9b: the answer write.
    first_answer_s, answer_status = answer_timing(task, tool_steps(calls, trace), start)

    return {
        "seq": seq, "task": task,
        "q1_end": q1, "q1_exit_reason": exit_reason,
        "inspect_not_exited": insp.get("not_exited"), "inspect_run_active": insp.get("run_active"),
        "q2_outside_stop_events": [e["type"] for e in outside_stop],
        "q3_ledger_calls": len(calls), "q3_inspect_calls": (insp.get("total_usage") or {}).get("calls"),
        "q3_dial_errors": dial_errors,
        "q3_call_errors": [(c.get("error") or "")[:160] for c in calls if c.get("error")],
        "q4_tokens": tokens,
        "q5_agent_s": (end - start).total_seconds(),
        "start": start.isoformat(), "end": end.isoformat(),
        "q6_last_kind": last.get("kind"), "q6_last_detail": (last.get("detail") or "")[:200],
        "q6_last_record_time": last.get("time"),
        "q6_last_call_start": last_call_start.isoformat() if last_call_start else None,
        "q7_refusals": refusals, "q7_trace_tool_errors": len(trace_errors),
        "q7_kernel_events": [e["type"] for e in refusal_events],
        "q8_kind": kind, "q8_budget_s": budget_s, "q8_time_notes": len(time_notes),
        "q8_warning_s": (warning - start).total_seconds() if warning else None,
        "q8_checkpoint_budget_warned": last_cp.get("budget_warned"),
        "q8_checkpoint_usage": last_cp.get("usage"),
        "q9_first_answer_s": first_answer_s, "q9_status": answer_status,
        "q11_exit_reason": exit_reason,
    }


def main():
    rows = []
    for job_dir in sorted(glob.glob(os.path.join(JOBS, "tb2-budget-[0-9][0-9][0-9]-*"))):
        rows.append(answer(job_dir))
    with open(os.path.join(HERE, "records.jsonl"), "w") as out:
        for row in rows:
            out.write(json.dumps(row) + "\n")
    print(f"{len(rows)} trials answered from records")


if __name__ == "__main__":
    main()
