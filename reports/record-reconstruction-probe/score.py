"""Scores the records-only answers against the ground truth (PLAN.md).

Reads records.jsonl (reconstruct.py), truth.jsonl (truth.py) and
q10_hand.json, and writes scores.jsonl (one line per trial and question) and
summary.json (counts per question, and the known gaps).

    python3 -I reports/record-reconstruction-probe/score.py
"""

import json
import os
import re
from collections import Counter
from datetime import datetime, timedelta

HERE = os.path.dirname(os.path.abspath(__file__))
ANSWER_TASKS = {"polyglot-c-py", "raman-fitting", "vulnerable-secret", "cobol-modernization",
                "prove-plus-comm", "openssl-selfsigned-cert"}
CLAIM = re.compile(r"verif|written|wrote|complete|passed|all checks|identical|match", re.I)


def load(name):
    return {r["seq"]: r for r in map(json.loads, open(os.path.join(HERE, name)))}


def main():
    rec, tru = load("records.jsonl"), load("truth.jsonl")
    hand10 = {t["seq"]: t for t in json.load(open(os.path.join(HERE, "q10_hand.json")))["trials"]}
    scores = []

    def put(seq, q, score, records, truth, note=""):
        scores.append({"seq": seq, "task": tru[seq]["task"], "arm": tru[seq]["arm"], "q": q, "score": score,
                       "records": records, "truth": truth, "note": note})

    for seq in sorted(tru):
        r, t = rec[seq], tru[seq]
        # Q1
        put(seq, "Q1", "correct" if r["q1_end"] == t["q1_end"] else "wrong", r["q1_end"], t["q1_end"])
        # Q2
        if t["q1_end"] == "not_ended":
            named = [e for e in r["q2_outside_stop_events"] if e != "backend.retry"]
            put(seq, "Q2", "correct" if named else "not_answerable",
                {"exit_event": None, "inspect_not_exited": r["inspect_not_exited"],
                 "inspect_run_active": r["inspect_run_active"]}, t["q2_stop"])
        # Q3
        diff = r["q3_ledger_calls"] - t["q3_relay_posts"]
        ok = diff == 0 or diff == r["q3_dial_errors"]
        put(seq, "Q3", "correct" if ok else "wrong", r["q3_ledger_calls"], t["q3_relay_posts"],
            f"{r['q3_dial_errors']} ledger call(s) never reached the backend (dial tcp)" if ok and diff else "")
        # Q4
        put(seq, "Q4", "no_truth", r["q4_tokens"], None,
            "every request has a ledger record" if ok else
            f"{t['q3_relay_posts'] - r['q3_ledger_calls'] + r['q3_dial_errors']} request(s) have no ledger record, so no tokens")
        # Q5
        d5 = r["q5_agent_s"] - t["q5_agent_s"]
        put(seq, "Q5", "correct" if abs(d5) <= 5 else "wrong", round(r["q5_agent_s"], 1), round(t["q5_agent_s"], 1),
            f"records end {-d5:.1f} s before Harbor's span ends" if abs(d5) > 5 else "")
        # Q6
        last_start = datetime.fromisoformat(r["q6_last_call_start"])
        later = [p for p in t["q6_post_starts"] if datetime.fromisoformat(p) > last_start + timedelta(seconds=2)]
        agent_end = datetime.fromisoformat(t["agent_end"])
        after_kill = [p for p in later if datetime.fromisoformat(p) > agent_end]
        put(seq, "Q6", "correct" if not later else "wrong",
            {"last_record": r["q6_last_kind"], "detail": r["q6_last_detail"][:120]},
            {"requests_after_last_recorded_call": len(later), "of_them_after_harbor_ended_agent": len(after_kill)},
            "; ".join(f"request at {p[11:23]}" for p in later))
        # Q7
        if r["q7_refusals"] or r["q7_trace_tool_errors"]:
            consistent = (len(r["q7_refusals"]) == r["q7_trace_tool_errors"]
                          and all(x["told_in_ledger"] for x in r["q7_refusals"]))
            put(seq, "Q7", "no_truth", [{k: x[k] for k in ("tool", "error", "at_s", "told_in_ledger")}
                                        for x in r["q7_refusals"]], None,
                ("trace, artifacts and ledger agree" if consistent else "records disagree")
                + f"; kernel events: {len(r['q7_kernel_events'])}")
        # Q8
        put(seq, "Q8", "correct" if r["q8_kind"] == t["q8_kind"] else "wrong",
            {"kind": r["q8_kind"], "budget_s": r["q8_budget_s"], "warning_s": r["q8_warning_s"],
             "checkpoint_budget_warned": r["q8_checkpoint_budget_warned"]}, t["q8_kind"],
            "warning in ledger and checkpoint agree" if (r["q8_warning_s"] is not None) ==
            bool(r["q8_checkpoint_budget_warned"]) else "ledger and checkpoint disagree on the warning")
        # Q9, Q9b
        if t["task"] in ANSWER_TASKS and t["reward"] == 0.0:
            h = t["q9_first_answer_s"]
            if r["q9_first_answer_s"] is None:
                score = "wrong"
            else:
                score = "correct" if abs(r["q9_first_answer_s"] - h) <= 2 else "wrong"
            put(seq, "Q9", score, r["q9_first_answer_s"], h, r["q9_status"])
        if t["task"] in ANSWER_TASKS and t["reward"] == 1.0:
            put(seq, "Q9b", "correct" if r["q9_status"] == "written" else "wrong", r["q9_status"], "file existed")
        # Q10
        if seq in hand10:
            a = hand10[seq]["answer"]
            v = " ".join(t["q10_verifier"])
            if isinstance(a, dict):
                got = [float(x) for x in re.findall(r"Got: x0=([-\d.eE+]+)", v)]
                ok = len(got) == 2 and abs(got[0] - a["G_x0"]) <= 0.1 and abs(got[1] - a["2D_x0"]) <= 0.1
                truth = got
            else:
                m = re.search(r"found: (\[[^\]]*\])", v)
                truth = json.loads(m.group(1).replace("'", '"')) if m else None
                ok = truth == a
            put(seq, "Q10", "correct" if ok else "wrong", a, truth, hand10[seq]["method"])
        elif t["task"] in ANSWER_TASKS and t["reward"] == 0.0:
            put(seq, "Q10", "no_truth", None, None, "the verifier did not run")
        # Q11
        if t["reward"] == 0.0:
            claim = bool(CLAIM.search(r["q11_exit_reason"] or ""))
            put(seq, "Q11", "descriptive", {"claims_success": claim, "exit_reason": (r["q11_exit_reason"] or "")[:200]},
                t["reward"])

    with open(os.path.join(HERE, "scores.jsonl"), "w") as out:
        for s in scores:
            out.write(json.dumps(s) + "\n")

    summary = {"per_question": {}}
    for q in ["Q1", "Q2", "Q3", "Q4", "Q5", "Q6", "Q7", "Q8", "Q9", "Q9b", "Q10", "Q11"]:
        c = Counter(s["score"] for s in scores if s["q"] == q)
        summary["per_question"][q] = dict(c)
        summary["per_question"][q]["wrong_seqs"] = [s["seq"] for s in scores if s["q"] == q and s["score"] == "wrong"]
        summary["per_question"][q]["not_answerable_seqs"] = [s["seq"] for s in scores if s["q"] == q
                                                            and s["score"] == "not_answerable"]
    q10 = [s for s in scores if s["q"] == "Q10" and s["score"] != "no_truth"]
    summary["q10_methods"] = dict(Counter(s["note"] for s in q10))
    q11 = [s for s in scores if s["q"] == "Q11"]
    summary["q11_claims_in_failed"] = sum(1 for s in q11 if s["records"]["claims_success"])
    summary["q11_failed"] = len(q11)

    # The known gaps.
    killed = [seq for seq in tru if tru[seq]["q1_end"] == "not_ended"]
    summary["gap_killed"] = []
    for seq in killed:
        r, t = rec[seq], tru[seq]
        last = datetime.fromisoformat(r["q6_last_record_time"].replace("Z", "+00:00")[:26] + "+00:00")
        end = datetime.fromisoformat(t["agent_end"])
        after = [p for p in t["q6_post_starts"] if datetime.fromisoformat(p) > end]
        summary["gap_killed"].append({
            "seq": seq, "ledger_calls": r["q3_ledger_calls"], "relay_requests": t["q3_relay_posts"],
            "missing_requests": t["q3_relay_posts"] - r["q3_ledger_calls"],
            "last_record_to_kill_s": round((end - last).total_seconds(), 1),
            "requests_started_after_kill": [p[11:23] for p in after],
            "after_kill_s": [round((datetime.fromisoformat(p) - end).total_seconds(), 1) for p in after]})
    refusals = [(seq, x) for seq in rec for x in rec[seq]["q7_refusals"]]
    summary["gap_refusals"] = {
        "tool_errors": len(refusals), "trials": len({s for s, _ in refusals}),
        "by_tool": dict(Counter(x["tool"] for _, x in refusals)),
        "kernel_events": sum(len(rec[s]["q7_kernel_events"]) for s in rec),
        "told_in_ledger": sum(1 for _, x in refusals if x["told_in_ledger"])}
    summary["gap_run_writes"] = {
        "failed_missed": [s["seq"] for s in scores if s["q"] == "Q9" and s["records"] is None],
        "passed_missed": [s["seq"] for s in scores if s["q"] == "Q9b" and s["score"] == "wrong"],
        "passed_total": sum(1 for s in scores if s["q"] == "Q9b")}
    summary["backend_retry_events"] = {seq: rec[seq]["q2_outside_stop_events"].count("backend.retry")
                                       for seq in rec if "backend.retry" in rec[seq]["q2_outside_stop_events"]}
    json.dump(summary, open(os.path.join(HERE, "summary.json"), "w"), indent=1)
    print(json.dumps(summary, indent=1))


if __name__ == "__main__":
    main()
