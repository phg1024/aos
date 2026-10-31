"""Classifies each of the 27 handoffs by whether it left an index and why
not, from handoffs.jsonl and the handoff work copy's events, tool results
and tool errors, and writes handoff_summary.json. Also tallies how each
handoff ended (memory.compacted handoff_end), the retries it got, the
index lines the runtime rejected and why, and whether the archive matched
the notice's range.

Run from the directory the live run started in, since artifact paths in
the work copy are relative to it:
    cd /Users/peihongguo/Code/aos
    python3 -I <worktree>/reports/memory-probe-recall-fix/handoff_summary.py
"""

import collections
import json
import os

HERE = os.path.dirname(os.path.abspath(__file__))

# Reasons the runtime gives for a rejected index line (recall.go).
REASONS = [
    ("cap", "over the index's"),
    ("range", "cites messages outside"),
    ("format", ": not \"N: what is there\""),
]


def reasons(report):
    """Counts the rejected lines in an index_memory report by reason."""
    counts = collections.Counter()
    for line in report.splitlines():
        if line.startswith("... and "):
            counts["unlisted"] += int(line.split()[2])
            continue
        for reason, marker in REASONS:
            if marker in line:
                counts[reason] += 1
                break
    return counts


def main():
    handoffs = [json.loads(line) for line in open(os.path.join(HERE, "handoffs.jsonl"))]
    work = os.path.join(HERE, "work-handoff")
    index = json.load(open(os.path.join(work, "artifacts", "index.json")))
    records = index.get("artifacts", index)
    records = list(records.values()) if isinstance(records, dict) else records
    events = json.load(open(os.path.join(work, "kernel", "events.json")))
    events = events if isinstance(events, list) else events.get("events", events)
    compacted = {e["process_id"]: e["data"] for e in events if e.get("type") == "memory.compacted"}
    noticed = {e["process_id"]: int(e["data"]["through_seq"]) for e in events if e.get("type") == "memory.handoff"}
    refusals = collections.defaultdict(list)  # index_memory calls that kept no line
    results = collections.defaultdict(list)   # index_memory calls that kept lines
    for r in records:
        if r["type"] == "tool_error":
            error = json.load(open(r["path"])).get("error", "")
            if error.startswith("index_memory"):
                refusals[r["producer_pid"]].append(error)
        elif r["type"] == "tool_result" and (r.get("metadata") or {}).get("tool") == "index_memory":
            results[r["producer_pid"]].append(json.load(open(r["path"])).get("output", {}))

    rows = []
    for h in handoffs:
        pid = h["pid"]
        tools = h.get("tools") or []
        rejected = collections.Counter()
        for error in refusals[pid]:
            rejected.update(reasons(error.partition("Rejected:\n")[2]))
        for output in results[pid]:
            rejected.update(reasons(output.get("rejected", "")))
        if h.get("agent_index"):
            outcome = "indexed"
        elif "index_memory" not in tools:
            outcome = "never called index_memory"
        elif refusals[pid] and not results[pid]:
            outcome = "every index_memory call refused"
        else:
            outcome = "other"
        data = compacted.get(pid, {})
        rows.append({
            "checkpoint": h["checkpoint"], "draw": h["sample"], "pid": pid, "outcome": outcome,
            "lines": len(h.get("agent_index") or []),
            # "bytes" is "N of CAP" in the last index_memory result kept.
            "index_bytes": results[pid][-1].get("bytes", "") if results[pid] else "",
            "model_calls": h["steps"], "handoff_steps": int(data.get("handoff_steps", 0)),
            "handoff_end": data.get("handoff_end", ""), "handoff_retried": data.get("handoff_retried") == "true",
            "notice_through": noticed.get(pid), "archived": f"{h.get('from_seq')}-{h.get('to_seq')}",
            "archive_matches_notice": noticed.get(pid) == h.get("to_seq"),
            "index_calls": tools.count("index_memory"), "index_refusals": len(refusals[pid]),
            "rejected_lines": dict(rejected), "first_tool": tools[0] if tools else "",
            "tokens": h.get("tokens", 0), "tools": tools, "error": h.get("error", ""),
        })
    total_rejected = collections.Counter()
    for r in rows:
        total_rejected.update(r["rejected_lines"])
    out = {
        "handoffs": rows,
        "outcomes": dict(collections.Counter(r["outcome"] for r in rows)),
        "handoff_end": dict(collections.Counter(r["handoff_end"] for r in rows)),
        "retried": sum(r["handoff_retried"] for r in rows),
        "with_rejected_lines": sum(1 for r in rows if r["rejected_lines"]),
        "rejected_lines": dict(total_rejected),
        "first_tool": dict(collections.Counter(r["first_tool"] for r in rows)),
        "archive_matches_notice": sum(r["archive_matches_notice"] for r in rows),
        "tokens_total": sum(r["tokens"] for r in rows),
        "tokens_mean": sum(r["tokens"] for r in rows) / len(rows),
        "model_calls_mean": sum(r["model_calls"] for r in rows) / len(rows),
    }
    with open(os.path.join(HERE, "handoff_summary.json"), "w") as f:
        json.dump(out, f, indent=1)
    print(json.dumps({k: v for k, v in out.items() if k != "handoffs"}, indent=1))


if __name__ == "__main__":
    main()
