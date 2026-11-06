#!/usr/bin/env python3
"""What a run did with Chapter 8's contract. Read-only, stdlib. Usage: python3 -I analyze.py <state dir>

It reads kernel events, the artifact store and the request log, and writes nothing. A situation
that did not arise prints as NOT EXERCISED, which is never a pass: it says nothing about the
contract either way. A pass whose verifier did not read the evidence is an UNCLEAN PASS, and the
script exits 3, because the kernel judges a verifier by its exit alone.
"""
import collections, hashlib, json, os, re, sys

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
        self.root = root
        self.events = load_json(os.path.join(root, "kernel", "events.json"), [])
        processes = load_json(os.path.join(root, "kernel", "processes.json"), {})
        self.processes = {p["pid"]: p for p in (processes.values() if isinstance(processes, dict) else processes)}
        self.index = load_json(os.path.join(root, "artifacts", "index.json"), {})
        self.calls = load_lines(os.path.join(root, "run", "calls.jsonl"))
        self.outcomes = [c for c in self.calls if c.get("phase") == "outcome"]
        self.starts = [c for c in self.calls if c.get("phase") == "start"]
        self.actions = collections.defaultdict(list)
        self.unparsed = 0
        for call in self.outcomes:
            if not call.get("error"):
                action = parse_action(call.get("response", ""))
                self.unparsed += action.get("type") == "unparsed"
                self.actions[self.chain(call.get("pid", ""))].append((call["time"], action))

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
        # The index holds the path on the machine that ran it, and this directory may be a copy.
        path = os.path.join(self.root, "artifacts", "payloads", artifact_id + ".json")
        try:
            with open(path) as f:
                return json.load(f)
        except (OSError, ValueError) as error:
            sys.exit(f"analyze: {artifact_id}: cannot read its payload: {error}")

    def records(self, *types):
        return sorted((a for a in self.index.values() if a.get("type") in types), key=lambda a: a["id"])

    def of_type(self, *types):
        return [e for e in self.events if e.get("type") in types]


def parse_action(text):
    """A model often leaves off closing braces, which the runtime's repair puts back. What still
    will not parse counts as unparsed, and counts() says how many, since lines built on actions
    cannot see those."""
    start = text.find("{")
    if start < 0:
        return {"type": "unparsed"}
    body = text[start:text.rfind("}") + 1]
    for missing in range(4):
        try:
            return json.loads(body + "}" * missing, strict=False).get("action") or {"type": "unparsed"}
        except (ValueError, AttributeError):
            pass
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
            return "published, then exited" if published else "gave up (exited)"
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
    print(f"model calls: {len(state.starts)} started, {sum(1 for c in state.outcomes if c.get('error'))} failed; "
          f"{state.unparsed} replies not parsed here, which lines built on actions cannot see")


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
    """chain -> artifact -> list of (time, offset, whether a page reached the end, where the page ended)."""
    reads = collections.defaultdict(lambda: collections.defaultdict(list))
    for record in state.records("tool_result"):
        payload = state.payload(record["id"]) or {}
        if payload.get("tool") != "read_artifact":
            continue
        output = payload.get("output") or {}
        reads[state.chain(record["producer_pid"])][output.get("artifact_id")].append(
            (record["created_at"], output.get("offset"), not output.get("next_offset"), output.get("next_offset")))
    return reads


def read_whole(rows):
    """Whether the pages join up from the start to the end: the last page alone is not a reading."""
    reach = 0
    for offset, last, end in sorted((int(r[1] or 0), r[2], r[3]) for r in rows):
        if offset > reach:
            return False
        if last:
            return True
        reach = max(reach, int(end))
    return False


def waits_of(state, chains):
    """What each process of chains asked when it paused: the engine's record, whatever the reply looked like."""
    out = []
    for record in state.records("agent_run"):
        if record.get("metadata", {}).get("status") == "waiting" and state.chain(record["producer_pid"]) in chains:
            out.append((state.payload(record["id"]) or {}).get("question", ""))
    return out


def source_of(record):
    return "from a file" if record.get("metadata", {}).get("source") == "file" else "typed"


def declared_evidence(state):
    """What the workflow declares its verifier must read, from the run's own record. An older run declares nothing."""
    for record in state.records("run_spec"):
        spec = (state.payload(record["id"]) or {}).get("spec") or {}
        return (spec.get("verification") or {}).get("evidence") or []
    return []


def root_of(state, pid):
    seen = set()
    while pid in state.processes and pid not in seen:
        seen.add(pid)
        parent = state.processes[pid].get("parent_pid")
        if not parent:
            return pid
        pid = parent
    return pid


def evidence_checks(state, verifier):
    """Each declared record as the verifier's chain left it: internal/agentflow/evidence.go computes the same from the
    same records, and a Go test holds the two together. (type, publisher, artifact or None, status) for each."""
    reads = artifact_reads(state).get(state.chain(verifier), {})
    root = root_of(state, verifier)
    checks = []
    for declared in declared_evidence(state):
        matching = [r for r in state.index.values() if r.get("type") == "published." + declared["type"]
                    and state.agent(r["producer_pid"]) == declared["publisher"] and root_of(state, r["producer_pid"]) == root]
        latest = max(matching, key=lambda r: int(r["id"].split("-")[1])) if matching else None
        if latest is None:
            status = "not_published"
        elif declared.get("from_file") and latest.get("metadata", {}).get("source") != "file":
            status = "not_from_file"
        else:
            rows = reads.get(latest["id"], [])
            status = "unread" if not rows else "read" if read_whole(rows) else "partial"
        checks.append((declared["type"], declared["publisher"], latest["id"] if latest else None, status))
    return checks


def delegation_handles(state):
    """(time, handle, delegator, process delegated to) for each handle attached at a delegation."""
    out = []
    for record in state.records("delegation_handles"):
        metadata = record.get("metadata", {})
        for handle in filter(None, metadata.get("artifacts", "").split(",")):
            out.append((record["created_at"], handle, metadata.get("delegator"), record["producer_pid"]))
    return out


def handles(state):
    section("Handles: each attached, by whom, to whom, and whether it was read")
    reads = artifact_reads(state)
    attached = [(when, handle, sender, to) for when, sender, to, _, handles in sent_mail(state) for handle in handles]
    for when, artifact, sender, to in attached:
        chain = state.chain(to)
        got = reads.get(chain, {}).get(artifact, [])
        later = [r for r in got if r[0] >= when]
        finished = read_whole(later)
        said = f"read {len(later)} pages, {'to the end' if finished else 'NOT to the end'}, first at {later[0][0]}" if later else "never read by its receiver"
        print(f"{artifact} ({state.index.get(artifact, {}).get('type')}) from {state.label(sender)} to {state.label(to)} at {when}: {said}")
    at_delegation = delegation_handles(state)
    for when, artifact, delegator, to in at_delegation:
        got = reads.get(state.chain(to), {}).get(artifact, [])
        said = f"read {len(got)} pages, {'to the end' if read_whole(got) else 'NOT to the end'}, first at {min(r[0] for r in got)}" if got else "never read by its receiver"
        print(f"{artifact} ({state.index.get(artifact, {}).get('type')}) attached at delegation by {state.label(delegator)} to {state.label(to)} at {when}: {said}")
    if not attached and not at_delegation:
        print("no handle was attached in this run")
    for record in sorted(state.index.values(), key=lambda a: a["id"]):
        if record.get("type", "").startswith("published."):
            size = len(str((state.payload(record["id"]) or {}).get("body", "")))
            print(f"published: {record['id']} {record['type']} by {state.label(record['producer_pid'])}, {size} bytes, {source_of(record)}")
    denied = [e for e in state.of_type("capability.denied") if e["data"].get("action") in ("read_artifact", "message_agent", "delegate", "spawn_agent") and e["data"].get("capability") == "read_artifact"]
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
            print(f"    read {artifact} ({state.index.get(artifact, {}).get('type')}): {len(rows)} pages, {'to the end' if read_whole(rows) else 'NOT to the end'}")
        if not before:
            print("    read no artifact")
        waits = waits_of(state, {chain})
        if waits:
            print(f"    waited {len(waits)} time(s): {str(waits[0])[:120]!r}")
    if not verifiers:
        print("no verifier ran")
    for record in verdicts:
        verdict = state.payload(record["id"]) or {}
        print(f"verdict {record['id']}: {verdict.get('status')}: {str(verdict.get('reason'))[:160]}")
    if not verdicts:
        print("no verdict on record")


def judged_verifier(state):
    """The verifier the verdict rests on, or the last to run if the verdict does not say."""
    verdicts = state.records("verification")
    if verdicts:
        named = (verdicts[-1].get("metadata") or {}).get("verifier_pid") or (state.payload(verdicts[-1]["id"]) or {}).get("verifier_pid")
        if named:
            return named
    verifiers = sorted(p for p in state.processes if state.agent(p) == "verifier")
    return verifiers[-1] if verifiers else None


RECORD_TYPE = re.compile(r"raw|record|trial", re.I)


def latest_record(state):
    """The newest raw record the last publishing benchmarker put out: a model names the type, so this goes by the name
    it chose, and failing that, by everything it published."""
    published = [r for r in state.index.values() if r.get("type", "").startswith("published.") and state.agent(r["producer_pid"]) == "benchmarker"]
    if not published:
        return None
    last = max(published, key=lambda r: r["id"])["producer_pid"]
    own = sorted((r for r in published if r["producer_pid"] == last), key=lambda r: r["id"])
    named = [r for r in own if RECORD_TYPE.search(r["type"])]
    return (named or own)[-1]


def on_disk(state, record):
    """The published file as this directory holds it: the path on record is the machine's, and this may be a copy."""
    path = record.get("metadata", {}).get("source_path", "")
    run_dir = os.path.dirname(os.path.abspath(state.root))
    for marker in ("cuda-bench",):
        if f"/{marker}/" in path:
            return os.path.join(run_dir, marker, path.split(f"/{marker}/", 1)[1])
    return path


def last_changed_by(state, record):
    """The last tool call of the producer that changed the published file, as the runtime recorded it."""
    path = record.get("metadata", {}).get("source_path", "")
    found = None
    for item in state.records("tool_result"):
        if item["id"] >= record["id"] or item["producer_pid"] != record["producer_pid"]:
            continue
        payload = state.payload(item["id"]) or {}
        output = payload.get("output") or {}
        if payload.get("tool") in ("write_file", "edit_file") and output.get("path") == path:
            found = (item["id"], payload["tool"], "")
        changes = payload.get("changes") or {}
        root = changes.get("root", "")
        # By the tail: the root is resolved, and the path as the agent spelled it.
        if any(path == os.path.join(root, name) or path.endswith("/" + name) for name in (changes.get("created") or []) + (changes.get("modified") or [])):
            found = (item["id"], payload.get("tool"), str(output.get("command", "")))
    return found


def named_in_report(state):
    """The artifact IDs the verifier's own report names, which it says it read."""
    run_dir = os.path.dirname(os.path.abspath(state.root))
    named = set()
    try:
        entries = os.listdir(run_dir)
    except OSError:
        return named
    for entry in entries:
        if entry.endswith("-verification"):
            for name in os.listdir(os.path.join(run_dir, entry)):
                if name.endswith("_verification.txt"):
                    with open(os.path.join(run_dir, entry, name), errors="replace") as f:
                        named |= set(re.findall(r"art-\d+", f.read()))
    return named


def declared_lines(state, verdict, judged):
    """Lines for what the workflow declares, and whether they differ from what the verdict says. Returns the latter."""
    refused = state.of_type("verification.exit_refused")
    declared = declared_evidence(state)
    if not declared:
        return False
    print(f"exits refused for evidence: {len(refused)}")
    for event in refused:
        print(f"    {state.label(event['process_id'])} at {event.get('timestamp')}: {str(event['data'].get('unmet'))[:160]}")
    if not judged or not verdict.get("verifier_pid"):
        print(f"no verifier run to check the declared evidence against")
        return False
    checks = evidence_checks(state, judged)
    for kind, publisher, artifact, status in checks:
        print(f"declared {kind} by {publisher}: {artifact or '-'}: {status}")
    theirs = [(c.get("type"), c.get("publisher"), c.get("artifact_id"), c.get("status")) for c in verdict.get("evidence") or []]
    if theirs != checks:
        print(f"DISAGREE: the verdict says {theirs}, these records say {checks}")
        return True
    if verdict.get("status") == "pass" and any(c[3] != "read" for c in checks):
        print("DISAGREE: the verdict is a pass, and a declared record was not read")
        return True
    return False


def evidence(state):
    """Whether the verdict rests on the evidence. Returns whether it is an unclean pass, or differs from these records."""
    section("Evidence behind the verdict")
    verdicts = state.records("verification")
    verdict = (state.payload(verdicts[-1]["id"]) or {}) if verdicts else {}
    judged = judged_verifier(state)
    disagrees = declared_lines(state, verdict, judged)
    if verdict.get("status") == "unverified":
        print(f"UNVERIFIED: {str(verdict.get('reason'))[:200]}")
        return disagrees
    if verdict.get("status") != "pass":
        print(f"verdict: {verdict.get('status') or 'none'}, so no pass to look behind (verifier {judged})")
        return disagrees
    chain = state.chain(judged)
    reads = artifact_reads(state).get(chain, {})
    print(f"verdict: pass, by {state.label(judged)}")
    reasons = []
    declared = declared_evidence(state)
    record = state.index.get(evidence_checks(state, judged)[0][2]) if declared else latest_record(state)
    if record is None:
        reasons.append("the benchmarker published no record")
    else:
        rows = reads.get(record["id"], [])
        print(f"the record: {record['id']} {record['type']} by {state.label(record['producer_pid'])}, {source_of(record)}; "
              f"the judged verifier read {len(rows)} pages of it")
        if not rows:
            reasons.append(f"the judged verifier never read {record['id']}")
        elif not read_whole(rows):
            reasons.append(f"the judged verifier did not read {record['id']} to its end")
        elif min(r[0] for r in rows) < record["created_at"]:
            reasons.append(f"the judged verifier read {record['id']} before it was published")
        if record.get("metadata", {}).get("source") != "file":
            reasons.append(f"{record['id']} is typed, so the verifier audited a transcription")
        else:
            reasons += file_provenance(state, record)
    unread = sorted(a for a in named_in_report(state) if a not in reads and a in state.index and state.index[a].get("type", "").startswith("published."))
    if unread:
        reasons.append(f"its report names {', '.join(unread)}, which it never read")
    reasons_seen = bool(reasons)
    for reason in reasons:
        print(f"UNCLEAN PASS: {reason}")
    if not reasons_seen:
        print("CLEAN: the judged verifier read the latest record to its end, and the record is a file's bytes")
    return reasons_seen


def file_provenance(state, record):
    """What this copy of the run can say about the published file. Only what the runtime wrote counts against it."""
    reasons = []
    meta = record["metadata"]
    print(f"    file {meta.get('source_path')}, sha256 {meta.get('source_sha256')}")
    path = on_disk(state, record)
    try:
        with open(path, "rb") as f:
            matches = hashlib.sha256(f.read()).hexdigest() == meta.get("source_sha256")
        print(f"    on disk at the end: {'the same bytes' if matches else 'CHANGED after the publish'}")
    except OSError:
        print("    on disk at the end: not in this copy")
    changed = last_changed_by(state, record)
    if changed is None:
        print("    last changed by: no tool call of its producer on record")
    else:
        print(f"    last changed by: {changed[0]} {changed[1]} {changed[2][:120]!r}")
        if changed[1] in ("write_file", "edit_file"):
            reasons.append(f"{record['id']}'s file was last written by {changed[1]}, so its bytes were typed into the file")
    return reasons


def routes(state):
    """Not contract situations: what a run did about the route the handles variant closes. A denial inside
    a confined command leaves no kernel event, so this reads the commands."""
    section("Routes: attempts on another agent's private files, mail to parked processes, and numbers carried by hand")
    attempts = []
    for event in state.of_type("capability.denied"):
        if "cuda-bench" in event["data"].get("path", ""):
            attempts.append(f"{state.label(event['process_id'])} {event['data'].get('action')}: refused: {event['data'].get('reason')}")
    for kind in ("tool_result", "tool_error"):
        for record in state.records(kind):
            if state.agent(record["producer_pid"]) == "benchmarker":
                continue
            payload = state.payload(record["id"]) or {}
            output = payload.get("output") or {}
            command = str(output.get("command", ""))
            if payload.get("tool") == "run" and "cuda-bench" in command:
                attempts.append(f"{state.label(record['producer_pid'])} run, exit {output.get('exit_code')}: {command[:90]!r}: {str(output.get('stderr', '')).strip()[:90]!r}")
    for line in attempts:
        print(f"attempt: {line}")
    if not attempts:
        print("no attempt on the private directory")
    waiting = {}
    for record in state.records("agent_run"):
        if record.get("metadata", {}).get("status") == "waiting":
            waiting.setdefault(record["producer_pid"], record["created_at"])
    parked = [(when, to) for when, _, to, _, _ in sent_mail(state) if to in waiting and when >= waiting[to]]
    print(f"mail to a parked process: {len(parked)}" + (f", first to {state.label(parked[0][1])}" if parked else ""))
    goals = 0
    for pid, process in state.processes.items():
        if state.agent(pid) == "verifier":
            goals += len(re.findall(r"render_ms_best=", str(process.get("metadata", {}).get("goal", ""))))
    pasted = sum(len(re.findall(r"render_ms_best=", body)) for _, _, to, body, _ in sent_mail(state) if state.agent(to) == "verifier")
    print(f"trial lines in verifier goals: {goals}, in mail bodies to a verifier: {pasted}")
    record = latest_record(state)
    if record is not None:
        values = set(re.findall(r"\d+\.\d{3,}", str((state.payload(record["id"]) or {}).get("body", ""))))
        verifier_chains = {state.chain(p) for p in state.processes if state.agent(p) == "verifier"}
        typed = set()
        for chain in verifier_chains:
            for _, action in state.actions.get(chain, []):
                if tool_name(action) in ("run", "write_file", "edit_file"):
                    text = json.dumps((action.get("tool_call") or {}).get("args") or {})
                    typed |= values & set(re.findall(r"\d+\.\d{3,}", text))
        print(f"values from {record['id']} a verifier retyped into a command or a file: {len(typed)} of {len(values)}")


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
    # The verifier the verdict rests on: an earlier round's reading is no reading of this round's evidence.
    judged = judged_verifier(state)
    verifier_reads = sum(1 for rows in reads.get(state.chain(judged), {}).values() if read_whole(rows)) if judged else 0
    verifier_waits = len(waits_of(state, verifier_chains))
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
        ("a handle read to its end", sum(1 for rows in reads.values() for r in rows.values() if read_whole(r))),
        ("a read denied", sum(1 for e in state.of_type("capability.denied") if e["data"].get("capability") == "read_artifact")),
        ("the verifier reading evidence to its end", verifier_reads),
        ("the verifier pausing with a wait", verifier_waits),
        ("a question by mail to the coordinator", question),
        ("mail forwarded along a chain", kinds["ipc.forwarded"]),
        ("mail held for a process not running", sum(1 for e in state.of_type("ipc.sent") if e["data"].get("held"))),
        ("a topic publish", sum(1 for r in state.records("mailbox_publish"))),
        ("an artifact published", sum(1 for r in state.index.values() if r.get("type", "").startswith("published."))),
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
    print(f"{sum(1 for _, count in situations if count)} of {len(situations)} exercised. NOT EXERCISED says nothing about the contract, either way.")
    # Kept apart so the count above stays comparable with runs from before these existed.
    refused_attachments = sum(1 for e in state.of_type("capability.denied") if e["data"].get("capability") == "read_artifact" and e["data"].get("action") in ("delegate", "spawn_agent"))
    unverified = sum(1 for r in state.records("verification") if (state.payload(r["id"]) or {}).get("status") == "unverified")
    declared = [
        ("a handle attached at a delegation", len(delegation_handles(state))),
        ("an attachment refused at a delegation", refused_attachments),
        ("an exit 0 refused for unread evidence", len(state.of_type("verification.exit_refused"))),
        ("a verdict withheld as unverified", unverified),
    ]
    for name, count in declared:
        print(f"{'exercised ' + str(count) if count else 'NOT EXERCISED':<16} {name}")
    print(f"{sum(1 for _, count in declared if count)} of {len(declared)} of the delegation and evidence situations exercised, counted apart from those above.")


def main():
    if len(sys.argv) != 2:
        sys.exit(__doc__)
    state = State(sys.argv[1])
    print(f"{sys.argv[1]}: {len(state.events)} events, {len(state.index)} artifacts, {len(state.calls)} log lines, {len(state.processes)} processes")
    counts(state)
    behavior(state)
    handles(state)
    verification(state)
    unclean = evidence(state)
    routes(state)
    told_too_much = not_shown(state)
    found = imitation(state)
    not_exercised(state, told_too_much, found)
    if unclean:
        sys.exit(3)


main()
