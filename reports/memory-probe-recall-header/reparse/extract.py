"""Collects every index_memory call of the follow-up's 27 handoffs
(../../memory-probe-recall-fix/, b87b59a) with its raw argument, the
archived range, the byte cap its tool result reports, and what was
stored, for the offline
re-parse under the old (50a7cfe) and new (6c55bdb) parsers (main.go).
Writes calls.json.

Usage, from this folder:
    python3 -I extract.py
"""

import json
import os

HERE = os.path.dirname(os.path.abspath(__file__))
FIX = os.path.join(HERE, "..", "..", "memory-probe-recall-fix")


def main():
    handoffs = [json.loads(line) for line in open(os.path.join(FIX, "handoffs.jsonl"))]
    out = []
    for h in handoffs:
        work = os.path.join(FIX, "work-handoff" if not h.get("retry") else f"work-handoff-{h['checkpoint']}-d{h['sample']}")
        index = json.load(open(os.path.join(work, "artifacts", "index.json")))
        mine = sorted((a for a in index.values() if a["producer_pid"] == h["pid"] and a["type"] == "checkpoint"),
                      key=lambda a: a["created_at"])
        payload = lambda a: json.load(open(os.path.join(work, "artifacts", "payloads", a["id"] + ".json")))
        # The latest checkpoint before the segment was written holds every call.
        calls, seen = [], set()
        for a in mine:
            for m in payload(a).get("messages", []):
                if m.get("role") != "assistant" or "index_memory" not in m.get("content", ""):
                    continue
                try:
                    action = json.loads(m["content"])
                except ValueError:
                    continue
                call = (action.get("action") or {}).get("tool_call") or {}
                if call.get("name") == "index_memory" and m["seq"] not in seen:
                    seen.add(m["seq"])
                    calls.append({"seq": int(m["seq"]), "index": (call.get("args") or {}).get("index", "")})
        # The cap each call got, from its tool result's "N of CAP".
        caps = set()
        for a in index.values():
            if a["producer_pid"] == h["pid"] and a["type"] == "tool_result":
                result = payload(a)
                if result.get("tool") == "index_memory":
                    caps.add(int(result["output"]["bytes"].split(" of ")[1]))
        assert len(caps) <= 1, (h["pid"], caps)
        out.append({"checkpoint": h["checkpoint"], "draw": h["sample"], "pid": h["pid"], "error": h.get("error", ""),
                    "from": h.get("from_seq"), "to": h.get("to_seq"), "cap": caps.pop() if caps else 0,
                    "stored": h.get("agent_index") or [], "calls": sorted(calls, key=lambda c: c["seq"])})
    json.dump(out, open(os.path.join(HERE, "calls.json"), "w"), indent=1)
    print(f"{len(out)} handoffs, {sum(len(h['calls']) for h in out)} index_memory calls")


if __name__ == "__main__":
    main()
