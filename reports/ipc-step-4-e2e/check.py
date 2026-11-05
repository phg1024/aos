#!/usr/bin/env python3
"""Print what a state directory from run.sh shows: refusals, denials, processes, mail."""
import collections, json, sys

state = sys.argv[1]
events = json.load(open(f"{state}/kernel/events.json"))
print({k: v for k, v in collections.Counter(e["type"] for e in events).items() if k.startswith(("ipc.", "capability."))})
for event in events:
    if event["type"] in ("ipc.rejected", "capability.denied"):
        print(event["process_id"], event["type"], event["data"])
processes = json.load(open(f"{state}/kernel/processes.json"))
for process in processes.values() if isinstance(processes, dict) else processes:
    print(process["pid"], process["metadata"].get("agent_id"), process["state"], (process.get("exit_status") or {}).get("reason"))
print("left in mailboxes:", {pid: len(box) for pid, box in json.load(open(f"{state}/bus/mailboxes.json")).items()})
