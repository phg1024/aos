"""Per-trial records and checks for the budget rerun (PLAN.md, A0 and A1).

  collect.py backend      prints the backend state as JSON: model, started,
                          the server's request count and the relay's POST
                          count; exits 1 if the server does not answer or its
                          model or start time differ from the pinned ones
  collect.py done SEQ     exits 0 if SEQ already has a record
  collect.py record SEQ ROUND TASK ARM JOBDIR BEFORE AFTER
                          appends the trial's record to results.jsonl (seq 0,
                          the smoke trial, to smoke.jsonl); exits 1 if the
                          stop rule is met
  collect.py show JOBDIR  prints a record without writing it

Everything is read from Harbor's job folder: result.json, the verifier's
ctrf.json, and the AOS state the adapter syncs (run/calls.jsonl, the calls
ledger; run/trace.jsonl; kernel/events.json).
"""

import json
import os
import re
import sys
import urllib.request
from datetime import datetime

HERE = os.path.dirname(os.path.abspath(__file__))
RELAY = "http://127.0.0.1:18080"
MODEL = "qwen3.8-flash-next-nvfp4-nvfp4"
STARTED = 1793427905  # the phg-gpu server's start time (A0)
PLANNED = 72

# The strings AOS shows or records (A1, from 1574f69's runtime/budget.go).
TIME_WARNING = "budget warning: less than 10% of the time is left"
STEPS_WARNING = re.compile(r"budget warning: \d+ of \d+ steps are left")
TIME_NOTE = re.compile(r"^budget time=(\d+)s used=(\d+)s left=(\d+)s")
BUDGET_EXIT = "time budget exceeded"
WARNING_FRACTION = 0.1
MARGIN_SEC = 20  # the adapter's TIME_BUDGET_MARGIN_SEC

# Answer paths (PLAN.md, "Candidate answer: frozen definition").
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


def parse_time(text):
    if not text:
        return None
    text = re.sub(r"(\.\d{6})\d+", r"\1", text.replace("Z", "+00:00"))
    value = datetime.fromisoformat(text)
    return value if value.tzinfo else value.replace(tzinfo=datetime.now().astimezone().tzinfo)


def span(result, key):
    part = result.get(key) or {}
    start, end = parse_time(part.get("started_at")), parse_time(part.get("finished_at"))
    return (start, end) if start and end else (None, None)


def jsonl(path):
    out = []
    if os.path.isfile(path):
        for line in open(path):
            try:
                out.append(json.loads(line))
            except json.JSONDecodeError:
                pass
    return out


def backend():
    try:
        with urllib.request.urlopen(RELAY + "/v1/status", timeout=20) as resp:
            status = json.load(resp)
    except Exception as exc:
        print(json.dumps({"error": str(exc)}))
        return 1
    posts = 0
    log = os.path.join(HERE, "relay.log")
    if os.path.isfile(log):
        posts = sum(1 for line in open(log) if " POST /v1/chat/completions " in line)
    state = {"model": status.get("model"), "started": status.get("started"),
             "requests": (status.get("activity") or {}).get("requests"), "relay_posts": posts,
             "engine": status.get("engine")}
    print(json.dumps(state))
    return 0 if state["model"] == MODEL and state["started"] == STARTED else 1


def trial_dir(job_dir):
    dirs = [d for d in os.listdir(job_dir) if os.path.isdir(os.path.join(job_dir, d))]
    if len(dirs) != 1:
        raise SystemExit(f"{job_dir}: expected one trial folder, found {dirs}")
    return os.path.join(job_dir, dirs[0])


PREFIX = re.compile(r"^\[#\d+\] ")
TOOL_KINDS = ("tool_result", "tool_error")


def text_of(message):
    content = message.get("content")
    if isinstance(content, list):
        content = " ".join(part.get("text", "") for part in content if isinstance(part, dict))
    return content or ""


def envelopes(calls):
    """The agent's tool calls, in order, from the assistant messages the
    ledger carries (the last step's reply is not in it: no call follows)."""
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
    """Each tool call in the trace, with its full arguments from the ledger
    where it has them (else the trace's shortened detail), its outcome kind
    and the time the outcome was traced."""
    full = envelopes(calls)
    steps = []
    records = trace
    for i, rec in enumerate(records):
        if rec.get("kind") != "tool_call":
            continue
        outcome = next((r for r in records[i + 1:] if r.get("kind") in TOOL_KINDS or r.get("kind") == "tool_call"), None)
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


def notes(calls):
    """Messages the runtime adds as user turns (tool results, budget notes,
    backend errors), prefix removed, each with the start time of the call
    that first carried it (record time minus latency)."""
    out = []
    for call in calls:
        when = parse_time(call.get("time"))
        if when is not None:
            from datetime import timedelta
            when = when - timedelta(milliseconds=call.get("latency_ms") or 0)
        for message in call.get("messages") or []:
            if message.get("role") in ("user", "tool"):
                out.append({"time": when, "content": PREFIX.sub("", text_of(message))})
    return out


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


def answer_timing(task, steps, agent_start):
    """Seconds from the agent's start until every answer path had been
    written by a tool call that succeeded, or None."""
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
    return (max(first.values()) - agent_start).total_seconds(), "written"


def record(seq, round_, task, arm, job_dir, before, after):
    try:
        tdir = trial_dir(job_dir)
    except (OSError, SystemExit) as exc:
        # Harbor failed before a trial folder existed: an infra error.
        return {"seq": seq, "round": round_, "task": task, "arm": arm, "trial": None, "reward": None,
                "ending": "infra_error", "exception": f"no trial folder: {exc}"}
    result = json.load(open(os.path.join(tdir, "result.json")))
    agent = result.get("agent_result") or {}
    meta = agent.get("metadata") or {}
    exc = (result.get("exception_info") or {}).get("exception_type")
    rewards = (result.get("verifier_result") or {}).get("rewards") or {}
    a_start, a_end = span(result, "agent_execution")
    t_start, t_end = parse_time(result.get("started_at")), parse_time(result.get("finished_at"))
    state = os.path.join(tdir, "agent", "aos-state")
    calls = jsonl(os.path.join(state, "run", "calls.jsonl"))
    trace = jsonl(os.path.join(state, "run", "trace.jsonl"))
    log = open(os.path.join(tdir, "agent", "aos.log"), errors="replace").read() if os.path.isfile(
        os.path.join(tdir, "agent", "aos.log")) else ""

    failed_tests = []
    ctrf = os.path.join(tdir, "verifier", "ctrf.json")
    if os.path.isfile(ctrf):
        for test in (json.load(open(ctrf)).get("results") or {}).get("tests") or []:
            if test.get("status") != "passed":
                failed_tests.append({"name": test.get("name"), "message": (test.get("message") or test.get("trace") or "")[:400]})

    tool_msgs = notes(calls)
    steps = tool_steps(calls, trace)
    # A model call cancelled by a strict budget is the budget exit, not a
    # backend error.
    backend_errors = sum(1 for c in calls if c.get("error") and BUDGET_EXIT not in c["error"]) + sum(
        1 for m in tool_msgs if m["content"].startswith("tool_error name=backend"))
    time_warn = next((m["time"] for m in tool_msgs if m["content"].startswith(TIME_WARNING)), None)
    steps_warn = next((m["time"] for m in tool_msgs if STEPS_WARNING.match(m["content"])), None)
    budget_notes = [m for m in tool_msgs if TIME_NOTE.match(m["content"])]
    budget_flag = meta.get("time_budget_flag") or ""
    m = re.search(r"--time-budget (\d+)s", budget_flag)
    budget = int(m.group(1)) if m else None
    exit_detail = next((r.get("detail") or "" for r in reversed(trace) if r.get("kind") == "exit"), "")
    exited_budget = BUDGET_EXIT in exit_detail or (not exit_detail and BUDGET_EXIT in log)

    if exc == "AgentTimeoutError":
        ending = "harbor_timeout"
    elif exc or "reward" not in rewards or backend_errors:
        ending = "infra_error"
    elif exited_budget:
        ending = "budget_exit"
    elif meta.get("aos_exit") in ("0", "1"):
        ending = "completed"
    else:
        ending = "agent_crash"

    # T_warn: the warning in strict/advisory; in off, the offset the warning
    # would have had: (agent timeout - margin) * (1 - fraction), from the
    # agent's start (the adapter's own setup time is under a second).
    if time_warn:
        t_warn = (time_warn - a_start).total_seconds()
    else:
        timeout = meta.get("agent_timeout_sec")
        nominal = budget or (int(timeout - MARGIN_SEC) if timeout else None)
        t_warn = nominal * (1 - WARNING_FRACTION) if nominal else None
    first_answer, answer_status = answer_timing(task, steps, a_start)
    agent_s = (a_end - a_start).total_seconds() if a_start else None

    tools = {}
    for step in steps:
        tools[step["name"]] = tools.get(step["name"], 0) + 1
    usage = {k: sum(c.get(k) or 0 for c in calls) for k in ("prompt_tokens", "cached_tokens", "completion_tokens")}
    params = (calls[0].get("params") if calls else {}) or {}
    contended = None
    try:
        b, a = json.loads(before), json.loads(after)
        server = a["requests"] - b["requests"]
        relay = a["relay_posts"] - b["relay_posts"]
        contended = {"server_requests": server, "relay_posts": relay, "surplus": server - relay}
    except Exception as exc_:
        contended = {"error": str(exc_)}

    return {
        "seq": seq, "round": round_, "task": task, "arm": arm, "trial": os.path.basename(tdir),
        "reward": rewards.get("reward"), "failed_tests": failed_tests,
        "ending": ending, "exception": exc, "aos_exit": meta.get("aos_exit"), "budget_exit_in_log": exited_budget,
        "time_budget": meta.get("time_budget"), "time_budget_flag": budget_flag,
        "agent_timeout_s": meta.get("agent_timeout_sec"),
        "agent_s": agent_s, "trial_s": (t_end - t_start).total_seconds() if t_start and t_end else None,
        "model_calls": len(calls), "calls_with_error": sum(1 for c in calls if c.get("error")),
        "exit_detail": exit_detail[:300],
        "backend_errors": backend_errors, "tool_calls": tools, "trace_records": len(trace),
        "usage": usage, "usage_source": meta.get("usage_source"),
        "models": sorted({c.get("model") for c in calls if c.get("model")}),
        "params": {k: params.get(k) for k in ("temperature", "top_p", "max_tokens", "reasoning_budget_tokens")},
        "budget_notes": len(budget_notes),
        "time_warning_s": (time_warn - a_start).total_seconds() if time_warn else None,
        "steps_warning_s": (steps_warn - a_start).total_seconds() if steps_warn else None,
        "t_warn_s": t_warn, "first_answer_s": first_answer, "answer_status": answer_status,
        "answer_before_warn": (first_answer is not None and t_warn is not None and first_answer <= t_warn)
        if ANSWERS.get(task) is not None else None,
        "answer_before_end": (first_answer is not None) if ANSWERS.get(task) is not None else None,
        "contention": contended,
    }


def stop_rule(path):
    rows = jsonl(path)
    infra = sum(1 for r in rows if r["ending"] == "infra_error")
    if infra >= 8:
        return f"{infra} infra errors"
    if len(rows) >= 10 and infra > 0.10 * len(rows):
        return f"{infra} infra errors in {len(rows)} trials"
    last3 = rows[-3:]
    if len(last3) == 3 and all((r.get("contention") or {}).get("surplus", 0) > 0 for r in last3):
        return "three contended trials in a row: pause and tell aos book"
    return None


def main(argv):
    cmd = argv[1]
    if cmd == "backend":
        return backend()
    if cmd == "done":
        seq = int(argv[2])
        path = os.path.join(HERE, "smoke.jsonl" if seq == 0 else "results.jsonl")
        return 0 if any(r["seq"] == seq for r in jsonl(path)) else 1
    if cmd == "show":
        print(json.dumps(record(0, 0, argv[3], argv[4], argv[2], "{}", "{}"), indent=1, default=str))
        return 0
    if cmd == "record":
        seq, round_, task, arm, job_dir, before, after = argv[2:9]
        rec = record(int(seq), int(round_), task, arm, job_dir, before, after)
        path = os.path.join(HERE, "smoke.jsonl" if rec["seq"] == 0 else "results.jsonl")
        with open(path, "a") as out:
            out.write(json.dumps(rec, default=str) + "\n")
        print(f"  reward {rec['reward']} {rec['ending']} {rec.get('agent_s')}s calls {rec.get('model_calls')} "
              f"answer {rec.get('answer_status')} contention {rec.get('contention')}")
        if rec["seq"] == 0:
            return 0
        reason = stop_rule(path)
        if reason:
            print("STOP:", reason)
            return 1
        return 0
    raise SystemExit(f"unknown command {cmd}")


if __name__ == "__main__":
    sys.exit(main(sys.argv))
