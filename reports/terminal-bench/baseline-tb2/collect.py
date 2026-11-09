"""Per-trial records and checks for the TB2 baseline (PLAN.md).

  collect.py backend      prints the backend state as JSON: model, started,
                          the server's request count, and the relay log's
                          line count and POST count; exits 1 if the server
                          does not answer, or its model or start time differ
                          from the pinned ones
  collect.py done SEQ     exits 0 if SEQ already has a record
  collect.py record SEQ ROUND TASK ARM JOBDIR BEFORE AFTER
                          appends the trial's record to results.jsonl (seq 0
                          or below, smoke trials, to smoke.jsonl); exits 1
                          if the stop rule is met
  collect.py show ARM TASK JOBDIR
                          prints a record without writing it

Everything is read from Harbor's job folder (result.json, the verifier's
ctrf.json; for aos, the AOS state the adapter syncs: run/calls.jsonl, the
two-phase call ledger, run/trace.jsonl, kernel/events.json and the artifact
index; for terminus-2, its trajectory), from the slice of relay.log the
trial wrote, and from Strata's status before and after. Stdlib only.
"""

import json
import os
import re
import sys
import urllib.request
from datetime import datetime, timedelta

HERE = os.path.dirname(os.path.abspath(__file__))
RELAY = "http://127.0.0.1:18080"
RELAY_LOG = os.path.join(HERE, "relay.log")
MODEL = "qwen3.8-flash-next-nvfp4-nvfp4"
STARTED = 1793522223  # the phg-gpu server's start time, read before the run
PLANNED = 96

# The strings AOS shows or records (the rerun's A1; confirmed at v0.1.10 in A1).
TIME_WARNING = "budget warning: less than 10% of the time is left"
TIME_NOTE = re.compile(r"^budget time=(\d+)s used=(\d+)s left=(\d+)s")
BUDGET_EXIT = "time budget exceeded"

# Terminus-2 exceptions that are the model server's or the network's, not the
# agent's: LiteLLM's API and connection errors (A1 lists any added after the smoke).
API_ERRORS = re.compile(r"APIError|APIConnectionError|ServiceUnavailable|InternalServerError|RateLimit|"
                        r"Timeout|BadGateway|NotFoundError|AuthenticationError|PermissionDenied")


def parse_time(text):
    if not text:
        return None
    text = re.sub(r"(\.\d{6})\d+", r"\1", str(text).replace("Z", "+00:00"))
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


def relay_lines():
    return open(RELAY_LOG).read().splitlines() if os.path.isfile(RELAY_LOG) else []


def backend():
    try:
        with urllib.request.urlopen(RELAY + "/v1/status", timeout=20) as resp:
            status = json.load(resp)
    except Exception as exc:
        print(json.dumps({"error": str(exc)}))
        return 1
    lines = relay_lines()
    state = {"model": status.get("model"), "started": status.get("started"),
             "requests": (status.get("activity") or {}).get("requests"), "engine": status.get("engine"),
             "relay_lines": len(lines),
             "relay_posts": sum(1 for line in lines if " POST /v1/chat/completions " in line)}
    print(json.dumps(state))
    return 0 if state["model"] == MODEL and state["started"] == STARTED else 1


def trial_dir(job_dir):
    dirs = [d for d in os.listdir(job_dir) if os.path.isdir(os.path.join(job_dir, d))]
    if len(dirs) != 1:
        raise SystemExit(f"{job_dir}: expected one trial folder, found {dirs}")
    return os.path.join(job_dir, dirs[0])


def relay_slice(before, after):
    """The relay's lines for this trial: chat completions, statuses and the
    distinct sampling field sets clients sent."""
    try:
        lo, hi = json.loads(before)["relay_lines"], json.loads(after)["relay_lines"]
    except Exception:
        return None
    posts = [line for line in relay_lines()[lo:hi] if " POST /v1/chat/completions " in line]
    statuses, sampling = {}, {}
    for line in posts:
        parts = line.split(" ", 4)
        statuses[parts[3]] = statuses.get(parts[3], 0) + 1
        m = re.search(r" (\{.*?\}) messages=", line)
        if m:
            sampling[m.group(1)] = sampling.get(m.group(1), 0) + 1
    return {"posts": len(posts), "statuses": statuses, "sampling": sampling}


PREFIX = re.compile(r"^\[#\d+\] ")


def text_of(message):
    content = message.get("content")
    if isinstance(content, list):
        content = " ".join(part.get("text", "") for part in content if isinstance(part, dict))
    return content or ""


def aos_fields(tdir, a_start, meta):
    state = os.path.join(tdir, "agent", "aos-state")
    ledger = jsonl(os.path.join(state, "run", "calls.jsonl"))
    starts = [r for r in ledger if r.get("phase") == "start"]
    outcomes = {r.get("start"): r for r in ledger if r.get("phase") == "outcome"}
    trace = jsonl(os.path.join(state, "run", "trace.jsonl"))
    log_path = os.path.join(tdir, "agent", "aos.log")
    log = open(log_path, errors="replace").read() if os.path.isfile(log_path) else ""
    events = []
    events_path = os.path.join(state, "kernel", "events.json")
    if os.path.isfile(events_path):
        loaded = json.load(open(events_path))
        events = loaded if isinstance(loaded, list) else loaded.get("events", [])
    index_path = os.path.join(state, "artifacts", "index.json")
    index = json.load(open(index_path)) if os.path.isfile(index_path) else {}

    # Notes the runtime adds as user turns; a start line carries the messages
    # new since its base call. Each note's time is its call's start.
    notes = []
    for call in starts:
        when = parse_time(call.get("time"))
        for message in call.get("messages") or []:
            if message.get("role") in ("user", "tool"):
                notes.append({"time": when, "content": PREFIX.sub("", text_of(message))})
    errors = [o.get("error") for o in outcomes.values() if o.get("error")]
    backend_errors = sum(1 for e in errors if BUDGET_EXIT not in e) + sum(
        1 for n in notes if n["content"].startswith("tool_error name=backend"))
    warning = next((n["time"] for n in notes if n["content"].startswith(TIME_WARNING)), None)
    exit_detail = next((r.get("detail") or "" for r in reversed(trace) if r.get("kind") == "exit"), "")
    exited_budget = BUDGET_EXIT in exit_detail or (not exit_detail and BUDGET_EXIT in log)
    usage = {k: sum(o.get(k) or 0 for o in outcomes.values())
             for k in ("prompt_tokens", "cached_tokens", "completion_tokens")}
    tools = {}
    for r in trace:
        if r.get("kind") == "tool_call":
            name = (r.get("detail") or "").split(" ", 1)[0]
            tools[name] = tools.get(name, 0) + 1
    checkpoints = sorted(k for k, v in index.items() if v.get("type") == "checkpoint")
    kinds = {}
    for e in events:
        kinds[e.get("type")] = kinds.get(e.get("type"), 0) + 1
    return {
        "aos_exit": meta.get("aos_exit"), "exit_detail": exit_detail[:300], "budget_exit": exited_budget,
        "time_budget": meta.get("time_budget"), "time_budget_flag": meta.get("time_budget_flag") or "",
        "model_calls": len(starts), "calls_unfinished": sum(1 for s in starts if s.get("seq") not in outcomes),
        "calls_with_error": len(errors), "backend_errors": backend_errors,
        "usage": usage, "usage_source": meta.get("usage_source"),
        "params": [dict(t) for t in {tuple(sorted((s.get("params") or {}).items())) for s in starts}],
        "tool_calls": tools,
        "contracts": {
            "budget_notes": sum(1 for n in notes if TIME_NOTE.match(n["content"])),
            "time_warning_s": (warning - a_start).total_seconds() if warning and a_start else None,
            "budget_exit": exited_budget,
            "checkpoints": len(checkpoints),
            "ledger_complete": all(s.get("seq") in outcomes for s in starts),
            "compactions": kinds.get("memory.compacted", 0), "handoffs": kinds.get("memory.handoff", 0),
        },
    }


def terminus_fields(tdir, agent):
    traj_path = os.path.join(tdir, "agent", "trajectory.json")
    steps, episodes = None, None
    if os.path.isfile(traj_path):
        traj = json.load(open(traj_path))
        steps = traj.get("steps") or []
        episodes = sum(1 for s in steps if s.get("source") == "agent")
    usage = {"prompt_tokens": agent.get("n_input_tokens"), "cached_tokens": agent.get("n_cache_tokens"),
             "completion_tokens": agent.get("n_output_tokens")}
    return {"usage": usage, "usage_source": "harbor", "agent_steps": episodes,
            "trajectory_steps": len(steps) if steps is not None else None}


def record(seq, round_, task, arm, job_dir, before, after):
    base = {"seq": seq, "round": round_, "task": task, "arm": arm}
    survey = {t["task"]: t for t in json.load(open(os.path.join(HERE, "survey.json")))}
    base.update({"agent_timeout_s": survey[task]["agent_timeout_s"],
                 "verifier_timeout_s": survey[task]["verifier_timeout_s"],
                 "verifier_mode": survey[task]["verifier_mode"]})
    relay = relay_slice(before, after)
    try:
        tdir = trial_dir(job_dir)
    except (OSError, SystemExit) as exc:
        return {**base, "trial": None, "reward": None, "ending": "infra_error",
                "exception": f"no trial folder: {exc}", "relay": relay}
    result = json.load(open(os.path.join(tdir, "result.json")))
    agent = result.get("agent_result") or {}
    meta = agent.get("metadata") or {}
    info = result.get("exception_info") or {}
    exc = info.get("exception_type")
    rewards = (result.get("verifier_result") or {}).get("rewards") or {}
    a_start, a_end = span(result, "agent_execution")
    t_start, t_end = parse_time(result.get("started_at")), parse_time(result.get("finished_at"))

    failed_tests = []
    ctrf = os.path.join(tdir, "verifier", "ctrf.json")
    if os.path.isfile(ctrf):
        for test in (json.load(open(ctrf)).get("results") or {}).get("tests") or []:
            if test.get("status") != "passed":
                failed_tests.append({"name": test.get("name"),
                                     "message": (test.get("message") or test.get("trace") or "")[:400]})

    fields = aos_fields(tdir, a_start, meta) if arm == "aos" else terminus_fields(tdir, agent)
    relay_502 = (relay or {}).get("statuses", {}).get("502", 0)
    api_error = bool(exc and API_ERRORS.search(exc + " " + (info.get("exception_message") or "")[:2000]))

    if exc == "AgentTimeoutError":
        ending = "harbor_timeout"
    elif "reward" not in rewards or relay_502 or fields.get("backend_errors") or api_error:
        ending = "infra_error"
    elif exc and arm == "terminus-2" and not a_end:
        ending = "infra_error"  # raised before the agent ran: environment or setup
    elif exc and arm == "aos":
        ending = "infra_error"
    elif arm == "aos" and fields["budget_exit"]:
        ending = "budget_exit"
    elif arm == "aos":
        ending = "completed" if meta.get("aos_exit") in ("0", "1") else "agent_crash"
    else:
        ending = "agent_crash" if exc else "completed"

    contention = None
    try:
        b, a = json.loads(before), json.loads(after)
        server = a["requests"] - b["requests"]
        posts = a["relay_posts"] - b["relay_posts"]
        contention = {"server_requests": server, "relay_posts": posts, "surplus": server - posts}
    except Exception as exc_:
        contention = {"error": str(exc_)}

    return {
        **base, "trial": os.path.basename(tdir), "reward": rewards.get("reward"), "failed_tests": failed_tests,
        "ending": ending, "exception": exc, "exception_message": (info.get("exception_message") or "")[:300],
        "agent_s": (a_end - a_start).total_seconds() if a_start and a_end else None,
        "trial_s": (t_end - t_start).total_seconds() if t_start and t_end else None,
        **fields, "relay": relay, "contention": contention,
    }


def stop_rule(path):
    rows = jsonl(path)
    infra = sum(1 for r in rows if r["ending"] == "infra_error")
    if infra >= 10:
        return f"{infra} infra errors"
    if len(rows) >= 10 and infra > 0.10 * len(rows):
        return f"{infra} infra errors in {len(rows)} trials"
    last3 = rows[-3:]
    if len(last3) == 3 and all((r.get("contention") or {}).get("surplus", 0) > 0 for r in last3):
        return "three contended trials in a row"
    return None


def main(argv):
    cmd = argv[1]
    if cmd == "backend":
        return backend()
    if cmd == "done":
        seq = int(argv[2])
        path = os.path.join(HERE, "smoke.jsonl" if seq <= 0 else "results.jsonl")
        return 0 if any(r["seq"] == seq for r in jsonl(path)) else 1
    if cmd == "show":
        print(json.dumps(record(0, 0, argv[3], argv[2], argv[4], "{}", "{}"), indent=1, default=str))
        return 0
    if cmd == "record":
        seq, round_, task, arm, job_dir, before, after = argv[2:9]
        rec = record(int(seq), int(round_), task, arm, job_dir, before, after)
        path = os.path.join(HERE, "smoke.jsonl" if rec["seq"] <= 0 else "results.jsonl")
        with open(path, "a") as out:
            out.write(json.dumps(rec, default=str) + "\n")
        print(f"  reward {rec['reward']} {rec['ending']} agent {rec.get('agent_s')}s "
              f"posts {(rec.get('relay') or {}).get('posts')} contention {rec.get('contention')}")
        if rec["seq"] <= 0:
            return 0
        reason = stop_rule(path)
        if reason:
            print("STOP:", reason)
            return 1
        return 0
    raise SystemExit(f"unknown command {cmd}")


if __name__ == "__main__":
    sys.exit(main(sys.argv))
