"""Per-trial records and checks for the TB2 baseline's pi arm (PLAN-pi.md).

  collect_pi.py backend      as collect.py's, for relay-pi.log; also exits 1
                             if the engine differs from the baseline's
  collect_pi.py done SEQ     exits 0 if SEQ already has a record, or if a
                             PAUSE file holds a number below SEQ
  collect_pi.py record SEQ ROUND TASK JOBDIR BEFORE AFTER
                             appends the record to results-pi.jsonl (seq 0,
                             the smoke trial, to smoke-pi.jsonl); exits 1 if
                             the stop rule is met
  collect_pi.py show TASK JOBDIR
                             prints a record without writing it

It reuses collect.py's helpers and A3's attribution by start time, from the
first trial. The ending has one rule (PLAN-pi.md, "Endings"): only an
unrecovered 502 makes a trial an infra_error. Usage, the exit and the call
counts come from Harbor's result and pi's session file. Stdlib only.
"""

import json
import os
import statistics
import sys
import time
import urllib.request
from datetime import datetime

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import collect  # noqa: E402

collect.RELAY_LOG = os.path.join(HERE, "relay-pi.log")
ENGINE = "0.1.54"
KNOWN_ENTRIES = {"session", "model_change", "thinking_level_change", "message"}


def backend():
    try:
        with urllib.request.urlopen(collect.RELAY + "/v1/status", timeout=20) as resp:
            status = json.load(resp)
    except Exception as exc:
        print(json.dumps({"error": str(exc)}))
        return 1
    lines = collect.relay_lines()
    state = {"model": status.get("model"), "started": status.get("started"),
             "requests": (status.get("activity") or {}).get("requests"), "engine": status.get("engine"),
             "relay_lines": len(lines),
             "relay_posts": sum(1 for line in lines if " POST /v1/chat/completions " in line)}
    print(json.dumps(state))
    ok = state["model"] == collect.MODEL and state["started"] == collect.STARTED and state["engine"] == ENGINE
    return 0 if ok else 1


def entry_time(entry):
    return collect.parse_time(entry.get("timestamp"))


def session_fields(tdir):
    """Counts from pi's session file(s); a torn last line is skipped."""
    sessions = os.path.join(tdir, "agent", "pi", "sessions")
    entries = []
    if os.path.isdir(sessions):
        for name in sorted(os.listdir(sessions)):
            if name.endswith(".jsonl"):
                entries += collect.jsonl(os.path.join(sessions, name))
    other, stops, tools, tool_errors, latency = {}, {}, {}, 0, []
    assistant_times = []
    prev = None
    for entry in entries:
        kind = entry.get("type")
        if kind not in KNOWN_ENTRIES:
            other[kind] = other.get(kind, 0) + 1
        message = entry.get("message") or {}
        role = message.get("role")
        when = entry_time(entry)
        if role == "assistant":
            reason = message.get("stopReason")
            stops[reason] = stops.get(reason, 0) + 1
            assistant_times.append(when)
            if prev and when:
                latency.append((when - prev).total_seconds())
        elif role == "toolResult":
            name = message.get("toolName")
            tools[name] = tools.get(name, 0) + 1
            tool_errors += bool(message.get("isError"))
        if when:
            prev = when
    lat = sorted(latency)
    return {
        "session_entries": len(entries), "model_calls": len(assistant_times), "stop_reasons": stops,
        "tool_calls": tools, "tool_errors": tool_errors, "other_entries": other,
        "latency_s": {"n": len(lat), "median": statistics.median(lat) if lat else None,
                      "p90": lat[int(0.9 * (len(lat) - 1))] if lat else None, "max": lat[-1] if lat else None,
                      "all": [round(x, 3) for x in latency]},
        "first_reply": assistant_times[0].isoformat() if assistant_times and assistant_times[0] else None,
        "last_reply": assistant_times[-1].isoformat() if assistant_times and assistant_times[-1] else None,
    }


def attribute(lines, t_start, t_end, a_end, kind):
    """A3's attribution, plus each own 502 marked recovered or not. A 502 is
    recovered when the agent started another request after it, before the
    agent's end."""
    att = collect.attribute(lines, t_start, t_end, a_end, kind)
    if att is None:
        return None
    w0, w1 = (collect.parse_time(x) for x in att["window"])
    cancelled = set(att["cancelled_at_end"])
    starts = [collect.line_time(l) for l in lines if " POST /v1/chat/completions " in l]
    end = a_end or t_end
    own = []
    for l in lines:
        if " POST /v1/chat/completions 502 " not in l:
            continue
        t = collect.line_time(l)
        if not (w0 <= t < w1) or t.isoformat() in cancelled:
            continue
        later = any(t < s < end for s in starts)
        own.append({"start": t.isoformat(), "recovered": later})
    att["own_502_list"] = own
    att["unrecovered_502"] = sum(1 for x in own if not x["recovered"])
    return att


def record(seq, round_, task, job_dir, before, after, wait=0):
    survey = {t["task"]: t for t in json.load(open(os.path.join(HERE, "survey.json")))}
    base = {"seq": seq, "round": round_, "task": task, "arm": "pi",
            "agent_timeout_s": survey[task]["agent_timeout_s"],
            "verifier_timeout_s": survey[task]["verifier_timeout_s"],
            "verifier_mode": survey[task]["verifier_mode"]}
    relay = collect.relay_slice(before, after)
    try:
        tdir = collect.trial_dir(job_dir)
    except (OSError, SystemExit) as exc:
        return {**base, "trial": None, "reward": None, "ending": "infra_error",
                "exception": f"no trial folder: {exc}", "relay": relay}
    result = json.load(open(os.path.join(tdir, "result.json")))
    agent = result.get("agent_result") or {}
    meta = agent.get("metadata") or {}
    info = result.get("exception_info") or {}
    exc = info.get("exception_type")
    rewards = (result.get("verifier_result") or {}).get("rewards") or {}
    a_start, a_end = collect.span(result, "agent_execution")
    t_start, t_end = collect.parse_time(result.get("started_at")), collect.parse_time(result.get("finished_at"))

    failed_tests = []
    ctrf = os.path.join(tdir, "verifier", "ctrf.json")
    if os.path.isfile(ctrf):
        for test in (json.load(open(ctrf)).get("results") or {}).get("tests") or []:
            if test.get("status") != "passed":
                failed_tests.append({"name": test.get("name"),
                                     "message": (test.get("message") or test.get("trace") or "")[:400]})

    server = None
    try:
        server = json.loads(after)["requests"] - json.loads(before)["requests"]
    except Exception:
        pass
    kind = "harbor_timeout" if exc == "AgentTimeoutError" else "other"
    deadline = time.time() + wait
    while True:
        att = attribute(collect.relay_lines(), t_start, t_end, a_end, kind) if t_start and t_end else None
        short = server is not None and att is not None and server > att["posts_started"]
        if not short or time.time() >= deadline:
            break
        time.sleep(10)

    pi_exit = meta.get("pi_exit")
    unrecovered = (att or {}).get("unrecovered_502", 0)
    if exc == "AgentTimeoutError":
        ending = "harbor_timeout"
    elif "reward" not in rewards or unrecovered:
        ending = "infra_error"
    elif exc and not a_end:
        ending = "infra_error"  # raised before the agent ran: install, version check, setup
    elif exc:
        ending = "agent_crash"
    elif pi_exit == "0":
        ending = "completed"
    else:
        ending = "agent_crash"

    if att is None:
        contention = {"error": "no window"}
    elif server is None:
        contention = {"error": "backend counts not saved"}
    else:
        contention = {"server_requests": server, "posts_started": att["posts_started"],
                      "surplus": server - att["posts_started"]}
    fields = session_fields(tdir)
    posts = (att or {}).get("posts_started")
    return {
        **base, "trial": os.path.basename(tdir), "reward": rewards.get("reward"), "failed_tests": failed_tests,
        "ending": ending, "exception": exc, "exception_message": (info.get("exception_message") or "")[:300],
        "agent_s": (a_end - a_start).total_seconds() if a_start and a_end else None,
        "trial_s": (t_end - t_start).total_seconds() if t_start and t_end else None,
        "pi_exit": pi_exit, "pi_version": meta.get("pi_version"), "model": meta.get("model"),
        "usage_source": meta.get("usage_source"), "session_files": meta.get("session_files"),
        "usage": {"prompt_tokens": agent.get("n_input_tokens"), "cached_tokens": agent.get("n_cache_tokens"),
                  "completion_tokens": agent.get("n_output_tokens")},
        **fields,
        "retries_or_cancelled": (posts - fields["model_calls"]) if posts is not None else None,
        "relay": relay, "contention": contention, "attribution": att,
    }


def stop_rule(rows):
    infra = sum(1 for r in rows if r["ending"] == "infra_error")
    if infra >= 5:
        return f"{infra} infra errors"
    if len(rows) >= 10 and infra > 0.10 * len(rows):
        return f"{infra} infra errors in {len(rows)} trials"
    last3 = rows[-3:]
    if len(last3) == 3 and all((r.get("contention") or {}).get("surplus", 0) > 0 for r in last3):
        return "three contended trials in a row"
    if rows and rows[-1]["ending"] == "agent_crash":
        return f"agent_crash in seq {rows[-1]['seq']}: pause for a look (the trial stays counted)"
    return None


def path_for(seq):
    return os.path.join(HERE, "smoke-pi.jsonl" if seq <= 0 else "results-pi.jsonl")


def main(argv):
    cmd = argv[1]
    if cmd == "backend":
        return backend()
    if cmd == "done":
        seq = int(argv[2])
        pause = os.path.join(HERE, "PAUSE")
        if seq > 0 and os.path.isfile(pause) and seq > int(open(pause).read()):
            return 0
        return 0 if any(r["seq"] == seq for r in collect.jsonl(path_for(seq))) else 1
    if cmd == "show":
        print(json.dumps(record(0, 0, argv[2], argv[3], "{}", "{}"), indent=1, default=str))
        return 0
    if cmd == "record":
        seq, round_, task, job_dir, before, after = argv[2:8]
        rec = record(int(seq), int(round_), task, job_dir, before, after, wait=600)
        path = path_for(rec["seq"])
        with open(path, "a") as out:
            out.write(json.dumps(rec, default=str) + "\n")
        att = rec.get("attribution") or {}
        print(f"  reward {rec['reward']} {rec['ending']} agent {rec.get('agent_s')}s calls {rec.get('model_calls')} "
              f"posts {att.get('posts_started')} contention {rec.get('contention')} "
              f"cancelled {att.get('cancelled_at_end')} 502s {att.get('own_502_list')}")
        if rec["seq"] <= 0:
            return 0
        reason = stop_rule(collect.jsonl(path))
        if reason:
            print("STOP:", reason)
            return 1
        return 0
    raise SystemExit(f"unknown command {cmd}")


if __name__ == "__main__":
    sys.exit(main(sys.argv))
