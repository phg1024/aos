#!/usr/bin/env python3
"""Did Metal work inside the confined run? Prints what each scripted command really did."""
import json, os, sys

state = sys.argv[1]
index = json.load(open(os.path.join(state, "artifacts", "index.json")))
for record in sorted(index.values(), key=lambda a: a["id"]):
    if record["type"] not in ("tool_result", "tool_error"):
        continue
    payload = json.load(open(record["path"]))
    if payload.get("tool") != "run":
        continue
    output = payload.get("output") or {}
    stdout = output.get("stdout", "")
    device = [line for line in stdout.splitlines() if line.startswith("device=")]
    noise = sum(1 for line in output.get("stderr", "").splitlines() if "sandbox_extension_issue_file" in line)
    first = stdout.splitlines()[0][:100] if stdout else ""
    print(f"{record['id']} {record['producer_pid']} exit={output.get('exit_code')} {device[0] if device else first} {output.get('error', payload.get('error', ''))}")
    if noise:
        print(f"    with {noise} lines of sandbox_extension_issue_file on stderr")
    if "touch=" in stdout:
        print("    " + [line for line in stdout.splitlines() if line.startswith("touch=")][0] + " (non-zero: the read-only workspace held for the verifier)")
