#!/usr/bin/env python3
"""What a run did with Chapter 8's contract. Read-only, stdlib. Usage: python3 -I analyze.py <state dir>

It reads kernel events, the artifact store and the request log, and writes nothing. A situation
that did not arise prints as NOT EXERCISED, which is never a pass: it says nothing about the
contract either way.
"""
import collections, json, os, re, sys

# framing.openers, with the two forms it matches by shape.
OPENERS = ("message from ", "budget steps=", "budget time=", "budget tokens=", "budget warning:", "runtime notice:",
           "tool_error ", "tool_notice ", "tool_cancelled ", "tool_interrupted ", "message_recorded ", "delegate_result ",
           "delegate_cancelled ", "send:to=", "publish:topic=", "subscribe:topic=", "[tool result truncated:",
           "undelivered:", "archived unread:", "subscribed to:")
SHAPES = re.compile(r"^(\[#\d+\]|[a-z_]+:map\[)")
WAITING = re.compile(r"runtime notice: \d+ more messages? (is|are) waiting in your mailbox")


def load_json(path, default):
    try:
        with open(path) as f:
            return json.load(f)
    except (OSError, ValueError):
        return default


def load_lines(path):
    out = []
    try:
        with open(path) as f:
            for line in f:
                try:
                    out.append(json.loads(line))
                except ValueError:
                    pass
    except OSError:
        pass
    return out


class State:
    def __init__(self, root):
        self.events = load_json(os.path.join(root, "kernel", "events.json"), [])
        processes = load_json(os.path.join(root, "kernel", "processes.json"), {})
        self.processes = {p["pid"]: p for p in (processes.values() if isinstance(processes, dict) else processes)}
        self.index = load_json(os.path.join(root, "artifacts", "index.json"), {})
        self.calls = load_lines(os.path.join(root, "run", "calls.jsonl"))
        self.outcomes = [c for c in self.calls if c.get("phase") == "outcome"]
        self.starts = [c for c in self.calls if c.get("phase") == "start"]
        self.actions = collections.defaultdict(list)
        for call in self.outcomes:
            if not call.get("error"):
                self.actions[self.chain(call.get("pid", ""))].append((call["time"], parse_action(call.get("response", ""))))

    def chain(self, pid):
        """A chain's first PID names it. Recoveries point back through resumed_from."""
        seen = set()
        while pid in self.processes and pid not in seen:
            seen.add(pid)
            back = self.processes[pid].get("metadata", {}).get("resumed_from")
            if not back:
                return pid
            pid = back
        return pid

    def agent(self, pid):
        return self.processes.get(pid, {}).get("metadata", {}).get("agent_id", "?")

    def label(self, pid):
        return f"{self.agent(pid)} ({pid})"

    def payload(self, artifact_id):
        return load_json(self.index.get(artifact_id, {}).get("path", ""), None)

    def records(self, *types):
        return sorted((a for a in self.index.values() if a.get("type") in types), key=lambda a: a["id"])

    def of_type(self, *types):
        return [e for e in self.events if e.get("type") in types]


def parse_action(text):
    start = text.find("{")
    if start < 0:
        return {"type": "unparsed"}
    try:
        return json.loads(text[start:text.rfind("}") + 1]).get("action") or {"type": "unparsed"}
    except (ValueError, AttributeError):
        return {"type": "unparsed"}


def describe(action):
    kind = action.get("type", "?")
    if kind == "tool":
        call = action.get("tool_call") or {}
        args = call.get("args") or {}
        return f"{call.get('name')}({', '.join(f'{k}={len(str(v))}b' if k in ('body', 'content') else f'{k}={str(v)[:40]}' for k, v in args.items())})"
    if kind == "send":
        send = action.get("send") or {}
        return f"send(to={send.get('to')}, body={len(str(send.get('body', '')))}b)"
    if kind == "delegate":
        return f"delegate({(action.get('delegate') or {}).get('agent_id')})"
    if kind == "exit":
        return f"exit({(action.get('exit_status') or {}).get('code')})"
    return kind


def outgoing(action):
    """The (to, body, handles) of an action that sends mail, or None."""
    if action.get("type") == "send":
        send = action.get("send") or {}
        return send.get("to"), str(send.get("body", "")), ""
    if action.get("type") == "tool" and (action.get("tool_call") or {}).get("name") == "message_agent":
        args = (action["tool_call"].get("args")) or {}
        return args.get("to") or args.get("pid"), str(args.get("body", "")), str(args.get("artifacts", ""))
    return None


def sent_mail(state):
    """(time, from, to, body, handles) for each message sent: a send action leaves a record, and
    message_agent a tool result."""
    out = []
    for record in state.records("mailbox_message"):
        if record.get("metadata", {}).get("direction") == "send":
            message = state.payload(record["id"]) or {}
            out.append((record["created_at"], message.get("from", ""), message.get("to", ""), str(message.get("body", "")), message.get("artifacts") or []))
    for record in state.records("tool_result"):
        payload = state.payload(record["id"]) or {}
        if payload.get("tool") == "message_agent":
            output = payload.get("output") or {}
            handles = [h for h in str(output.get("artifacts", "")).split(",") if h]
            out.append((record["created_at"], record["producer_pid"], output.get("to", ""), str(output.get("body", "")), handles))
    return sorted(out, key=lambda row: row[0])


def tool_name(action):
    return (action.get("tool_call") or {}).get("name") if action.get("type") == "tool" else None


def next_actions(state, pid, time, count=3):
    return [a for t, a in state.actions.get(state.chain(pid), []) if t > time][:count]


def last_action_before(state, pid, time):
    before = [a for t, a in state.actions.get(state.chain(pid), []) if t <= time]
    return before[-1] if before else {}


def after_refusal(refused, following):
    sent = outgoing(refused)
    published = False
    for action in following:
        name, out = tool_name(action), outgoing(action)
        if name == "publish_artifact":
            published = True
        if out and out[2]:
            return "published and attached" if published else "attached a handle it already had"
        if action == refused:
            return "retried the same call"
        if out and sent and out[0] == sent[0] and len(out[1]) < len(sent[1]):
            return "shortened"
        if action.get("type") == "exit":
            return "gave up (exited)"
        if action.get("type") == "wait":
            return "waited"
    if published:
        return "published, not attached"
    return "none of these" if following else "no action followed"


def after_note(following):
    if not following:
        return "no action followed"
    first = following[0]
    if first.get("type") == "exit":
        return "exited at once"
    if first.get("type") == "wait":
        return "waited"
    if any(outgoing(a) for a in following):
        return "sent mail"
    return "went on"


def section(title):
    print(f"\n== {title}")


def counts(state):
    section("Counts")
    kinds = collections.Counter(e.get("type") for e in state.events)
    held = sum(1 for e in state.of_type("ipc.sent") if e["data"].get("held"))
    acked = sum(len(e["data"].get("ids", "").split(",")) for e in state.of_type("ipc.acked"))
    print(f"ipc.sent {kinds['ipc.sent']} (held {held}), ipc.delivered {kinds['ipc.delivered']}, ipc.redelivered {kinds['ipc.redelivered']}, "
          f"ipc.forwarded {kinds['ipc.forwarded']}, ipc.transferred {kinds['ipc.transferred']}, ipc.acked {kinds['ipc.acked']} events, {acked} messages")
    rejected = collections.Counter(reason_of(e) for e in state.of_type("ipc.rejected"))
    print("ipc.rejected:", dict(rejected) or "none")
    letters = collections.Counter((e["data"].get("reason"), e["data"].get("detail", "-")) for e in state.of_type("ipc.dead_letter"))
    print("dead letters (reason, detail):", dict(letters) or "none")
    closed = collections.Counter(re.sub(r"pid-\d+", "pid", e["data"].get("reason", "")) for e in state.of_type("process.closed"))
    print("process.closed:", dict(closed) or "none")
    denied = collections.Counter((e["data"].get("action"), re.sub(r"^art-\d+: ", "", e["data"].get("reason", ""))) for e in state.of_type("capability.denied"))
    print("capability.denied:", dict(denied) or "none")
    print(f"backend.retry {kinds['backend.retry']}, memory.compacted {kinds['memory.compacted']}, memory.handoff {kinds['memory.handoff']}")
    notes = find_notes(state)
    print(f"in requests: archived unread {len(notes['archived'])}, undelivered {len(notes['undelivered'])}, waiting notes {len(notes['waiting'])}")
    print(f"model calls: {len(state.starts)} started, {sum(1 for c in state.outcomes if c.get('error'))} failed")


def reason_of(event):
    reason = event["data"].get("reason", "")
    if "has exited" in reason:
        return "recipient_exited"
    if "not a known process" in reason:
        return "unknown_recipient"
    return reason or "?"


def request_turns(state):
    """(start record, message) for every turn the request log holds, once."""
    for start in state.starts:
        for message in start.get("messages") or []:
            yield start, message


def find_notes(state):
    notes = {"archived": {}, "undelivered": {}, "waiting": {}}
    for start, message in request_turns(state):
        for line in str(message.get("content", "")).split("\n"):
            bare = re.sub(r"^\[#\d+\] ", "", line)
            key = (state.chain(start.get("pid", "")), bare)
            if bare.startswith("archived unread:"):
                notes["archived"].setdefault(key, start)
            elif bare.startswith("undelivered:"):
                notes["undelivered"].setdefault(key, start)
            elif WAITING.match(bare):
                notes["waiting"].setdefault(key, start)
    return notes


def behavior(state):
    section("Behavior after a refusal: the same process's next 3 actions")
    any_row = False
    for event in state.of_type("ipc.rejected"):
        reason = reason_of(event)
        if reason not in ("too_large", "mailbox_full"):
            continue
        any_row = True
        pid = event["process_id"]
        refused = last_action_before(state, pid, event["timestamp"])
        following = next_actions(state, pid, event["timestamp"])
        print(f"{event['id']} {reason} {state.label(pid)} after {describe(refused)}: {after_refusal(refused, following)}")
        print("    next: " + ("; ".join(describe(a) for a in following) or "none"))
    if not any_row:
        print("no too_large or mailbox_full refusal in this run")
    for title, kind in (("undelivered:", "undelivered"), ("a waiting note", "waiting")):
        section(f"Behavior after {title}: the response to that request, and the next 2")
        rows = find_notes(state)[kind]
        for (chain, line), start in rows.items():
            following = [a for t, a in state.actions.get(chain, []) if t >= start["time"]][:3]
            print(f"{state.label(start.get('pid', ''))}: {after_note(following)} | {line[:110]}")
            print("    next: " + ("; ".join(describe(a) for a in following) or "none"))
        if not rows:
            print("none in this run")


def artifact_reads(state):
    """chain -> artifact -> list of (time, offset, finished) from read_artifact results."""
    reads = collections.defaultdict(lambda: collections.defaultdict(list))
    for record in state.records("tool_result"):
        payload = state.payload(record["id"]) or {}
        if payload.get("tool") != "read_artifact":
            continue
        output = payload.get("output") or {}
        reads[state.chain(record["producer_pid"])][output.get("artifact_id")].append(
            (record["created_at"], output.get("offset"), not output.get("next_offset")))
    return reads


def handles(state):
    section("Handles: each attached, by whom, to whom, and whether it was read")
    reads = artifact_reads(state)
    attached = [(when, handle, sender, to) for when, sender, to, _, handles in sent_mail(state) for handle in handles]
    for when, artifact, sender, to in attached:
        chain = state.chain(to)
        got = reads.get(chain, {}).get(artifact, [])
        later = [r for r in got if r[0] >= when]
        finished = any(r[2] for r in later)
        said = f"read {len(later)} pages, {'to the end' if finished else 'NOT to the end'}, first at {later[0][0]}" if later else "never read by its receiver"
        print(f"{artifact} ({state.index.get(artifact, {}).get('type')}) from {state.label(sender)} to {state.label(to)} at {when}: {said}")
    if not attached:
        print("no handle was attached in this run")
    for record in sorted(state.index.values(), key=lambda a: a["id"]):
        if record.get("type", "").startswith("published."):
            size = len(str((state.payload(record["id"]) or {}).get("body", "")))
            print(f"published: {record['id']} {record['type']} by {state.label(record['producer_pid'])}, {size} bytes")
    denied = [e for e in state.of_type("capability.denied") if e["data"].get("action") in ("read_artifact", "message_agent") and e["data"].get("capability") == "read_artifact"]
    for event in denied:
        print(f"denied: {state.label(event['process_id'])} {event['data'].get('action')}: {event['data'].get('reason')}")
    if not denied:
        print("no read was denied in this run")


def verification(state):
    section("Verification: the verifier's reads before its exit, and the verdict")
    verdicts = state.records("verification")
    verifiers = sorted({p for p in state.processes if state.agent(p) == "verifier"})
    reads = artifact_reads(state)
    for pid in verifiers:
        process = state.processes[pid]
        status = process.get("exit_status") or {}
        print(f"{pid}: {process.get('state')}, exit {status.get('code')} {str(status.get('reason'))[:100]!r}")
        chain = state.chain(pid)
        before = {a: [r for r in rows] for a, rows in reads.get(chain, {}).items()}
        for artifact, rows in before.items():
            print(f"    read {artifact} ({state.index.get(artifact, {}).get('type')}): {len(rows)} pages, {'to the end' if any(r[2] for r in rows) else 'NOT to the end'}")
        if not before:
            print("    read no artifact")
        waits = [a for t, a in state.actions.get(chain, []) if a.get("type") == "wait"]
        if waits:
            print(f"    waited {len(waits)} time(s): {str(waits[0].get('message'))[:120]!r}")
    if not verifiers:
        print("no verifier ran")
    for record in verdicts:
        verdict = state.payload(record["id"]) or {}
        print(f"verdict {record['id']}: {verdict.get('status')}: {str(verdict.get('reason'))[:160]}")
    if not verdicts:
        print("no verdict on record")


def not_shown(state):
    section("not_shown dead letters: a returned model call after the delivery on record?")
    delivered = collections.defaultdict(list)
    for event in state.of_type("ipc.delivered", "ipc.redelivered"):
        delivered[event["data"].get("id")].append(event["timestamp"])
    letters = [e for e in state.of_type("ipc.dead_letter") if e["data"].get("reason") == "not_shown"]
    told_too_much = 0
    for event in letters:
        chain = state.chain(event["data"].get("to", ""))
        times = delivered.get(event["data"].get("id"), [])
        returned = [c for c in state.outcomes if not c.get("error") and state.chain(c.get("pid", "")) == chain and times and c["time"] > times[0]]
        if returned:
            told_too_much += 1
        print(f"{event['data'].get('id')} to {event['data'].get('to')}, detail {event['data'].get('detail', '-')}: {len(returned)} returned calls after its delivery")
    if letters:
        print(f"{told_too_much} of {len(letters)} have a returned call after the delivery, so their sender may have been told too much")
    else:
        print("no not_shown dead letter in this run")
    return told_too_much


def imitation(state):
    section("Bodies or tool output that look like a runtime opener")
    # By chain, the turns its own requests held, since a runtime line of another process's may
    # read the same. A value may end its line with more of the tool's output, so this looks for
    # the start of a line.
    requests = collections.defaultdict(list)
    for start, message in request_turns(state):
        if message.get("role") != "assistant":
            requests[state.chain(start.get("pid", ""))].append(str(message.get("content", "")))
    text = {chain: "\n" + "\n".join(turns) for chain, turns in requests.items()}
    found = collections.Counter()
    shown = set()
    def check(pid, origin, value):
        chain = state.chain(pid)
        seen = text.get(chain, "")
        for number, line in enumerate(str(value).split("\n")):
            if (number == 0 and origin == "tool") or not (line.startswith(OPENERS) or SHAPES.match(line)):
                continue
            if (chain, line) in shown:
                continue
            shown.add((chain, line))
            if "\n| " + line in seen or "\n\\" + line in seen:
                status = "escaped"
            elif "\n" + line in seen:
                status = "NOT ESCAPED"
            else:
                status = "not in a request"
            found[(origin, status)] += 1
            if status == "NOT ESCAPED":
                print(f"NOT ESCAPED ({origin}, {state.label(pid)}): {line[:120]}")
    for record in state.records("mailbox_message"):
        if record.get("metadata", {}).get("direction") == "receive":
            message = state.payload(record["id"]) or {}
            if message.get("from") != "user":
                check(record["producer_pid"], "mail", message.get("body", ""))
    for record in state.records("tool_result"):
        for value in ((state.payload(record["id"]) or {}).get("output") or {}).values():
            check(record["producer_pid"], "tool", value)
    print(dict(found) or "none in this run")
    return found


def not_exercised(state, told_too_much, found):
    section("Contract situations")
    kinds = collections.Counter(e.get("type") for e in state.events)
    rejected = collections.Counter(reason_of(e) for e in state.of_type("ipc.rejected"))
    letters = collections.Counter(e["data"].get("reason") for e in state.of_type("ipc.dead_letter"))
    details = collections.Counter(e["data"].get("detail") for e in state.of_type("ipc.dead_letter"))
    notes = find_notes(state)
    reads = artifact_reads(state)
    attached = 0
    question = 0
    for _, sender, to, body, handles in sent_mail(state):
        attached += len(handles)
        if state.agent(sender) in ("writer", "benchmarker") and state.agent(to) == "coordinator" and "?" in body:
            question += 1
    verifier_chains = {state.chain(p) for p in state.processes if state.agent(p) == "verifier"}
    verifier_reads = sum(1 for chain in verifier_chains for rows in reads.get(chain, {}).values() if any(r[2] for r in rows))
    verifier_waits = sum(1 for chain in verifier_chains for _, a in state.actions.get(chain, []) if a.get("type") == "wait")
    exit_over_waiting = 0
    for (chain, _), start in notes["waiting"].items():
        following = [a for t, a in state.actions.get(chain, []) if t >= start["time"]][:1]
        if following and following[0].get("type") == "exit":
            exit_over_waiting += 1
    redelegated = sum(1 for e in state.of_type("process.closed") if e["data"].get("reason", "").startswith("re-delegated"))
    situations = [
        ("a body refused as too large", rejected["too_large"]),
        ("a full mailbox refused a send", rejected["mailbox_full"]),
        ("a handle attached to mail", attached),
        ("a handle read to its end", sum(1 for rows in reads.values() for r in rows.values() if any(x[2] for x in r))),
        ("a read denied", sum(1 for e in state.of_type("capability.denied") if e["data"].get("capability") == "read_artifact")),
        ("the verifier reading evidence to its end", verifier_reads),
        ("the verifier pausing with a wait", verifier_waits),
        ("a question by mail to the coordinator", question),
        ("mail forwarded along a chain", kinds["ipc.forwarded"]),
        ("mail held for a process not running", sum(1 for e in state.of_type("ipc.sent") if e["data"].get("held"))),
        ("a publish", sum(1 for r in state.records("mailbox_publish"))),
        ("a dead letter, unread", letters["unread"]),
        ("a dead letter, not_shown", letters["not_shown"]),
        ("a not_shown letter with a returned call on record", told_too_much),
        ("a dead letter, archived", details["archived"]),
        ("a dead letter, rolled back", details["rolled_back"]),
        ("an undelivered: notice in a request", len(notes["undelivered"])),
        ("an archived unread: line in a request", len(notes["archived"])),
        ("a waiting note in a request", len(notes["waiting"])),
        ("a model exiting with a waiting note in its request", exit_over_waiting),
        ("a chain closed by a re-delegation", redelegated),
        ("a backend retry", kinds["backend.retry"]),
        ("compaction", kinds["memory.compacted"]),
        ("a handoff", kinds["memory.handoff"]),
        ("a body or tool output imitating a runtime line", sum(found.values())),
    ]
    for name, count in situations:
        print(f"{'exercised ' + str(count) if count else 'NOT EXERCISED':<16} {name}")
    print("NOT EXERCISED says nothing about the contract, either way.")


def main():
    if len(sys.argv) != 2:
        sys.exit(__doc__)
    state = State(sys.argv[1])
    print(f"{sys.argv[1]}: {len(state.events)} events, {len(state.index)} artifacts, {len(state.calls)} log lines, {len(state.processes)} processes")
    counts(state)
    behavior(state)
    handles(state)
    verification(state)
    told_too_much = not_shown(state)
    found = imitation(state)
    not_exercised(state, told_too_much, found)


main()
