"""Number provenance for the phase-3 evaluation answers.

For each sample, every decimal number in the answer (timings, RMSE; not
percentages or integers, which answers derive) is looked up in what the agent was shown: the probe's working
memory, the question, the live history of the probe's checkpoint in the run, and
the owner process's goal (part of the system prompt), and the sample's
own checkpoint and tool results (anything it searched or read). A
number found nowhere there is "unsupported": the agent did not have it,
so it was recalled wrongly or made up, unless it is a mean, sum or
difference of shown numbers the answer also states. Each flagged sample is then read
by hand; the verdicts are in the report, not here.

Usage, from this directory: python3 -I wmcheck.py <run-root> [--json out.json]
where <run-root> is the directory the memfix run started in.
"""

import json
import os
import re
import sys

CONDITIONS = ["memory", "no-check-rule", "no-memory"]
NUMBER = re.compile(r"(?<![\w.])(\d+\.\d+)(?![\d.]|\s?%|\s?[x×])")


def numbers(text):
    """Decimal numbers (timings, RMSE): percentages, ratios ("1.1x") and
    integers are left out, since answers derive them from what they were
    shown."""
    return {m.group(1) for m in NUMBER.finditer(text)}


def shown(text):
    """Every decimal number in the shown text, at every precision it can be
    rounded to, so a "47.18" stated from a shown "47.182" is supported."""
    out = set()
    for n in re.findall(r"\d+\.\d+", text):
        whole, frac = n.split(".")
        for d in range(0, len(frac) + 1):
            r = f"{float(n):.{d}f}"
            out.add(r)
            out.add(r.rstrip("0").rstrip(".") if "." in r else r)
    return out


def supported(n, have):
    return n in have or n.rstrip("0").rstrip(".") in have


def derived(n, sources):
    """n is the mean (of 2 to 5), sum or difference of numbers the answer
    states and was shown, at n's precision."""
    from itertools import combinations
    digits = len(n.split(".")[1])
    target = float(n)
    values = sorted({float(v) for v in sources})

    def close(x):
        return abs(round(x, digits) - target) < 10 ** -digits / 2 + 1e-9

    for k in range(2, 6):
        for combo in combinations(values, k):
            if close(sum(combo) / k):
                return True
    return any(close(abs(a - b)) or close(a + b) for a, b in combinations(values, 2))


def main():
    probes = {p["id"]: p for p in json.load(open("../../bench/memory/pathtracer-bunny-memfix-phase3-eval-probes.json"))}
    root = sys.argv[1]
    run_index = json.load(open(os.path.join(root, "reports/pathtracer-bunny-memfix/.aos-state/artifacts/index.json")))

    def live(checkpoint):
        path = run_index[checkpoint]["path"]
        return "\n".join(m.get("content", "") for m in json.load(open(os.path.join(root, path)))["messages"])

    processes = json.load(open(os.path.join(root, "reports/pathtracer-bunny-memfix/.aos-state/kernel/processes.json")))
    processes = processes.get("processes", processes) if isinstance(processes, dict) else processes
    processes = processes if isinstance(processes, list) else list(processes.values())
    goals = {p["pid"]: "\n".join(map(str, (p.get("metadata") or {}).values())) for p in processes}
    # The system prompt carries the owner's goal (the task it was given).
    lives = {p["checkpoint"]: live(p["checkpoint"]) + "\n" + goals.get(run_index[p["checkpoint"]]["producer_pid"], "")
             for p in probes.values()}
    rows = []
    for c in CONDITIONS:
        index = json.load(open(f"work-{c}/artifacts/index.json"))
        by_pid, tool_text = {}, {}
        for aid, r in index.items():
            if r["type"] == "checkpoint":
                by_pid.setdefault(r["producer_pid"], []).append((aid, r["path"]))
            elif r["type"] in ("tool_result", "tool_error") and os.path.isabs(r["path"]):
                tool_text[r["producer_pid"]] = tool_text.get(r["producer_pid"], "") + open(r["path"]).read()
        for line in open(f"samples-{c}.jsonl"):
            s = json.loads(line)
            probe = probes[s["probe"]]
            aid, path = max(by_pid[s["pid"]])
            messages = json.load(open(path))["messages"]
            if messages and messages[-1]["role"] == "assistant":
                messages = messages[:-1]
            context = lives[probe["checkpoint"]] + "\n" + "\n".join(m.get("content", "") for m in messages) + "\n" + tool_text.get(s["pid"], "") + "\n" + probe["question"]
            if c != "no-memory":
                context += "\n" + "\n".join(e["content"] + " " + e.get("evidence", "") for e in probe.get("working_memory", []))
            have = shown(context)
            stated = numbers(s["answer"])
            backed = [n for n in stated if supported(n, have)]
            missing = sorted(n for n in stated if not supported(n, have) and not derived(n, backed))
            rows.append({"condition": c, "probe": s["probe"], "sample": s["sample"], "steps": s["steps"],
                         "correct": s["correct"], "unsupported": missing})
    for c in CONDITIONS:
        flagged = [r for r in rows if r["condition"] == c and r["unsupported"]]
        print(f"{c}: {len(flagged)} of {sum(r['condition'] == c for r in rows)} answers state a number they were not shown")
        for r in flagged:
            print(f"  {r['probe']} #{r['sample']} steps={r['steps']}: {', '.join(r['unsupported'])}")
    if "--json" in sys.argv:
        json.dump(rows, open(sys.argv[sys.argv.index("--json") + 1], "w"), indent=1)


if __name__ == "__main__":
    main()
