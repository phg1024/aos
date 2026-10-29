"""Memory metrics of a pathtracer live run, per process.

Usage, from the directory the run started in:
    python3 -I analyze_live.py reports/pathtracer-bunny-phase3 [--json out.json]

Reads only the run's state directory and run.log. Works on runs from
before phase 1 (no memory events): counts that need events are then
reported as absent, not as zero.

Per process: agent, compactions (memory_segment artifacts), the smallest
segment in messages and characters, the peak live history in characters
over all checkpoints (role + content, as runtime.historyChars counts it),
memory.compacted events and how many were oversized, handoff notices and
how many were ignored (memory.compacted with handoff_updates=0, or a
handoff the process exited after), calls of search_memory, read_memory
and update_memory (tool_result and tool_error artifacts), and the latest
working memory's entries with and without evidence.
"""

import json
import os
import sys
from collections import Counter, defaultdict

EVIDENCE_SECTIONS = {"decisions", "rejected"}


def load(path):
    with open(path) as f:
        return json.load(f)


def main():
    run = sys.argv[1].rstrip("/")
    out = sys.argv[sys.argv.index("--json") + 1] if "--json" in sys.argv else None
    state = os.path.join(run, ".aos-state")
    index = load(os.path.join(state, "artifacts/index.json"))
    procs = {p["pid"]: p for p in procs_list(load(os.path.join(state, "kernel/processes.json")))}
    events = load(os.path.join(state, "kernel/events.json"))
    events = events if isinstance(events, list) else events.get("events", [])
    has_memory_events = any(str(e.get("type", "")).startswith("memory.") for e in events)

    stats = defaultdict(lambda: {
        "segments": [], "peak_live_chars": 0, "peak_live_at": None, "tools": Counter(),
        "tool_errors": Counter(), "compacted": [], "handoffs": [], "wm_versions": [],
    })
    for aid, record in sorted(index.items()):
        pid, kind = record["producer_pid"], record["type"]
        s = stats[pid]
        if kind == "memory_segment":
            seg = load(record["path"])
            msgs = seg.get("messages") or []
            chars = sum(len(m.get("role", "")) + len(m.get("content", "")) for m in msgs)
            s["segments"].append((aid, len(msgs), chars, record["created_at"]))
        elif kind == "checkpoint":
            ck = load(record["path"])
            chars = sum(len(m.get("role", "")) + len(m.get("content", "")) for m in ck.get("messages") or [])
            if chars > s["peak_live_chars"]:
                s["peak_live_chars"], s["peak_live_at"] = chars, aid
        elif kind == "tool_result":
            s["tools"][record["metadata"].get("tool", "?")] += 1
        elif kind == "tool_error":
            s["tool_errors"][record["metadata"].get("tool", "?")] += 1
        elif kind == "working_memory":
            s["wm_versions"].append((int(record["metadata"].get("version", 0)), aid))
    for e in events:
        pid, typ, data = e.get("pid") or e.get("process_id"), e.get("type"), e.get("data") or {}
        if typ == "memory.compacted":
            stats[pid]["compacted"].append(data)
        elif typ == "memory.handoff":
            stats[pid]["handoffs"].append(data)

    rows = []
    for pid in sorted(stats, key=lambda p: (len(p), p)):
        s = stats[pid]
        proc = procs.get(pid, {})
        agent = (proc.get("metadata") or {}).get("agent_id", "?")
        row = {
            "pid": pid, "agent": agent, "state": proc.get("state"),
            "compactions": len(s["segments"]),
            "smallest_segment": min(((n, c, a) for a, n, c, _ in s["segments"]), default=None),
            "peak_live_chars": s["peak_live_chars"], "peak_live_at": s["peak_live_at"],
            "search_memory": s["tools"]["search_memory"], "read_memory": s["tools"]["read_memory"],
            "update_memory": s["tools"]["update_memory"],
            "memory_tool_errors": {t: n for t, n in s["tool_errors"].items() if t.endswith("_memory")},
        }
        if has_memory_events:
            compacted = s["compacted"]
            row["compacted_events"] = len(compacted)
            row["oversized"] = sum(1 for d in compacted if d.get("oversized") == "true")
            row["handoffs"] = len(s["handoffs"])
            after = [d for d in compacted if "handoff_through" in d]
            row["handoff_ignored"] = sum(1 for d in after if d.get("handoff_updates") == "0")
            row["handoff_without_compaction"] = len(s["handoffs"]) - len(after)
            row["handoff_updates"] = [int(d.get("handoff_updates", 0)) for d in after]
        if s["wm_versions"]:
            version, aid = max(s["wm_versions"])
            wm = load(index[aid]["path"])
            entries = {"with_evidence": Counter(), "without_evidence": Counter()}
            for section in wm.get("sections") or []:
                for entry in section.get("entries") or []:
                    key = "with_evidence" if entry.get("evidence") else "without_evidence"
                    entries[key][section["name"]] += 1
            row["working_memory"] = {"latest": aid, "version": version,
                                     "with_evidence": dict(entries["with_evidence"]),
                                     "without_evidence": dict(entries["without_evidence"])}
        rows.append(row)

    print(f"memory events: {'yes' if has_memory_events else 'no (pre-phase-1 binary)'}")
    for r in rows:
        if r["agent"] == "?" and not r["compactions"] and not r["peak_live_chars"]:
            continue
        line = (f"{r['pid']} {r['agent']:<11} compactions={r['compactions']} smallest={r['smallest_segment']} "
                f"peak_live={r['peak_live_chars']} ({r['peak_live_at']}) search={r['search_memory']} "
                f"read={r['read_memory']} update={r['update_memory']}")
        if has_memory_events:
            line += (f" compacted_events={r['compacted_events']} oversized={r['oversized']} handoffs={r['handoffs']} "
                     f"ignored={r['handoff_ignored']} handoff_no_compaction={r['handoff_without_compaction']}")
        if r["memory_tool_errors"]:
            line += f" memory_tool_errors={r['memory_tool_errors']}"
        if "working_memory" in r:
            line += f" wm={r['working_memory']}"
        print(line)
    if out:
        with open(out, "w") as f:
            json.dump(rows, f, indent=2, default=str)


def procs_list(data):
    if isinstance(data, list):
        return data
    if isinstance(data, dict) and "processes" in data:
        data = data["processes"]
    return list(data.values()) if isinstance(data, dict) else data


if __name__ == "__main__":
    main()
