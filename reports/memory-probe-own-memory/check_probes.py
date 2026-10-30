"""Checks the frozen own-memory probes against the phase-3 live run.

Usage, from the directory the run started in (/Users/peihongguo/Code/aos):
    python3 -I <worktree>/reports/memory-probe-own-memory/check_probes.py <worktree>

For each probe it checks that:
- the checkpoint is the one right after a compaction of its process, and
  its header snapshot is the working-memory version the note names;
- each cited truth message is archived (seq <= the compaction's to_seq)
  and holds the quoted text;
- the answer's key token is not in the checkpoint's live history;
- "recorded" probes: the key token is in the working-memory snapshot;
  "unrecorded": it is not; "recorded-wrong": the reject pattern matches
  the snapshot (the entry states the wrong fact);
- the expect pattern matches the archived truth text, and the reject
  pattern, if any, does not.
Exit status 1 if any check fails.
"""

import json
import os
import re
import sys

RUN = "reports/pathtracer-bunny-phase3/.aos-state"
# probe id -> (process, compaction segment, snapshot, [(seq, quoted text)], key token regex)
TRUTH = {
    "r2-first-sah": ("pid-000006", "art-000646", "art-000643", [(93, "4416"), (94, "7107"), (103, "max_leaf=5398")], r"4416"),
    "r2-root-exit": ("pid-000006", "art-000646", "art-000643", [(144, "REGRESSED (107.0")], r"107\.0"),
    "r2-shadow-limit": ("pid-000006", "art-000646", "art-000643", [(94, "never checks t < t_limit"), (112, "beyond t_limit")], r"(?i)over-occlu|behind the light|never checks t < t_limit"),
    "r2-verify-dir": ("pid-000006", "art-000646", "art-000643", [(37, "Operation not permitted"), (38, "Verification dir not readable")], r"(?i)verification dir not readable|pathtracer-bunny-phase3-verification/: Operation not permitted"),
    "r5-empty-base": ("pid-000015", "art-001766", "art-001763", [(75, "include/include")], r"include/include|nesting bug"),
    "r5-entered-stack": ("pid-000015", "art-001766", "art-001763", [(89, "6/8 pairs favour cand, mean 45.607 vs 46.042")], r"45\.607|44\.425"),
    "r5-register-counts": ("pid-000015", "art-001766", "art-001763", [(35, "No offline metal compiler")], r"(?i)offline metal compiler|max=1024|\b1024\b"),
    "r5-tir-guard": ("pid-000015", "art-001766", "art-001763", [(14, "const float eta = ior_t / ior_i;"), (63, "air→glass fresnel-sat")], r"(?i)TIR guard never fires"),
    "r8-trace-share": ("pid-000025", "art-003133", "art-003130", [(26, "98.6% of GPU time")], r"98\.6"),
    "r8-small-node": ("pid-000025", "art-003133", "art-003130", [(53, "0/16 pairs, +0.90%")], r"\+0\.90%|1\.00900"),
    "r8-sibling-pairs": ("pid-000025", "art-003133", "art-003130", [(31, "sibling-pair 32B node blocks were a wash")], r"(?i)sibling|wash"),
}


def load(path):
    with open(path) as f:
        return json.load(f)


def render(wm):
    lines = []
    for section in wm.get("sections") or []:
        for entry in section.get("entries") or []:
            lines.append(f"[{section['name']}/{entry['key']}] {entry['content']} (evidence {entry.get('evidence', '')})")
    return "\n".join(lines)


def main():
    worktree = sys.argv[1]
    probes = load(os.path.join(worktree, "bench/memory/pathtracer-bunny-phase3-own-memory-probes.json"))
    index = load(os.path.join(RUN, "artifacts/index.json"))
    events = load(os.path.join(RUN, "kernel/events.json"))
    events = events if isinstance(events, list) else events.get("events", [])
    failures = 0

    def check(ok, what):
        nonlocal failures
        print(("ok   " if ok else "FAIL ") + what)
        failures += not ok

    assert set(TRUTH) == {p["id"] for p in probes}, "probe ids and TRUTH differ"
    for probe in probes:
        pid, segment, snapshot, quotes, key = TRUTH[probe["id"]]
        name = probe["id"]
        ck = load(index[probe["checkpoint"]]["path"])
        check(index[probe["checkpoint"]]["producer_pid"] == pid, f"{name}: checkpoint belongs to {pid}")
        compacted = [e for e in events if e["type"] == "memory.compacted" and e["data"]["segment"] == segment]
        check(len(compacted) == 1 and compacted[0]["process_id"] == pid, f"{name}: {segment} is a compaction of {pid}")
        to_seq = int(compacted[0]["data"]["to_seq"])
        later = [a for a, r in sorted(index.items()) if r["type"] == "checkpoint" and r["producer_pid"] == pid and r["created_at"] > compacted[0]["timestamp"]]
        check(later and later[0] == probe["checkpoint"], f"{name}: {probe['checkpoint']} is the first checkpoint after {segment}")
        check(ck.get("memory_snapshot") == snapshot, f"{name}: header snapshot is {snapshot}")
        wm = render(load(index[snapshot]["path"]))
        archived = {}
        for aid, record in index.items():
            if record["type"] == "memory_segment" and record["producer_pid"] == pid:
                for m in load(record["path"]).get("messages") or []:
                    archived[m["seq"]] = m["content"]
        truth_text = ""
        for seq, quote in quotes:
            text = archived.get(seq, "")
            truth_text += text
            check(seq <= to_seq and quote in text, f"{name}: archived #{seq} holds {quote!r}")
        live = "\n".join(m["content"] for m in ck["messages"])
        check(not re.search(key, live), f"{name}: key /{key}/ is not in live history")
        if probe["kind"] == "recorded":
            check(bool(re.search(key, wm)), f"{name}: key is in working memory {snapshot}")
        elif probe["kind"] == "unrecorded":
            check(not re.search(key, wm), f"{name}: key is not in working memory {snapshot}")
        elif probe["kind"] == "recorded-wrong":
            check(bool(re.search(key, wm)), f"{name}: the wrong entry is in working memory {snapshot}")
        check(bool(re.search(probe["expect"], truth_text)), f"{name}: expect matches the archived truth")
        if probe.get("reject") and probe["kind"] != "recorded-wrong":
            check(not re.search(probe["reject"], truth_text), f"{name}: reject does not match the archived truth")
        check(not re.search(probe["expect"], probe["question"]) or probe["kind"] == "recorded-wrong", f"{name}: expect does not match the question itself")
    print(f"{failures} failures")
    sys.exit(1 if failures else 0)


if __name__ == "__main__":
    main()
