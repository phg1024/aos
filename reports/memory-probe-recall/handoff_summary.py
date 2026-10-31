"""Classifies each of the 27 handoffs by why it did or did not leave an
index, from handoffs.jsonl and the tool errors in the handoff work copy,
and writes handoff_summary.json.

Run from the directory the live run started in, since artifact paths in
the work copy are relative to it:
    cd /Users/peihongguo/Code/aos
    python3 -I <worktree>/reports/memory-probe-recall/handoff_summary.py
"""

import collections
import json
import os

HERE = os.path.dirname(os.path.abspath(__file__))


def main():
    handoffs = [json.loads(line) for line in open(os.path.join(HERE, "handoffs.jsonl"))]
    index = json.load(open(os.path.join(HERE, "work-handoff", "artifacts", "index.json")))
    records = index.get("artifacts", index)
    records = list(records.values()) if isinstance(records, dict) else records
    events = json.load(open(os.path.join(HERE, "work-handoff", "kernel", "events.json")))
    events = events if isinstance(events, list) else events.get("events", events)
    compacted = {e["process_id"]: e["data"] for e in events if e.get("type") == "memory.compacted"}
    errors = collections.defaultdict(list)
    for r in records:
        if r["type"] == "tool_error":
            error = json.load(open(r["path"])).get("error", "")
            if error.startswith("index_memory"):
                errors[r["producer_pid"]].append(error)

    rows = []
    for h in handoffs:
        tools = h.get("tools") or []
        refusals = errors[h["pid"]]
        if h.get("agent_index"):
            outcome = "indexed"
        elif "index_memory" not in tools:
            outcome = "never called index_memory"
        elif refusals and all(e.startswith("index_memory: the index is") and "-byte cap" in e for e in refusals):
            outcome = "refused: over the byte cap"
        elif refusals and all(e.startswith("index_memory line") and "outside the ones being archived" in e for e in refusals):
            outcome = "refused: a line past the notice's range"
        else:
            outcome = "other"
        data = compacted.get(h["pid"], {})
        rows.append({
            "checkpoint": h["checkpoint"], "draw": h["sample"], "pid": h["pid"], "outcome": outcome,
            "lines": len(h.get("agent_index") or []), "model_calls": h["steps"],
            "handoff_steps": int(data.get("handoff_steps", 0)), "handoff_through": int(data.get("handoff_through", 0)),
            "archived": f"{h.get('from_seq')}-{h.get('to_seq')}", "tokens": h.get("tokens", 0), "tools": tools,
            "refusals": refusals, "index_call_last": bool(tools) and tools[-1] == "index_memory",
        })
    counts = collections.Counter(r["outcome"] for r in rows)
    refused = [r for r in rows if r["outcome"].startswith("refused")]
    out = {
        "handoffs": rows,
        "outcomes": dict(counts),
        "refused_with_retry": sum(1 for r in refused if not r["index_call_last"]),
        "refused_ended_under_4_steps": sum(1 for r in refused if r["handoff_steps"] < 4),
        "tokens_total": sum(r["tokens"] for r in rows),
        "tokens_mean": sum(r["tokens"] for r in rows) / len(rows),
        "model_calls_mean": sum(r["model_calls"] for r in rows) / len(rows),
    }
    with open(os.path.join(HERE, "handoff_summary.json"), "w") as f:
        json.dump(out, f, indent=1)
    print(json.dumps({k: v for k, v in out.items() if k != "handoffs"}, indent=1))


if __name__ == "__main__":
    main()
