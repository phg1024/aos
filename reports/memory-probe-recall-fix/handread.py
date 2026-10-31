"""Hand-read grades for every answer of the A1 follow-up (198), with the
own-memory rubric (../memory-probe-own-memory/handread.py: RUBRIC, and the
same main / exact / contra rules). A1 samples are numbered 1..9 as
(draw - 1) * 3 + sample, so A1 1-3 had draw 1's indexes, 4-6 draw 2's and
7-9 draw 3's.

The three unrecorded probes were graded by the evaluation session, below.
The eight others were graded by two reader agents briefed with the rubric,
the earlier grades as calibration and the archive, in grading/grades-a.json
(r2-shadow-limit, r5-empty-base, r5-entered-stack, r5-tir-guard) and
grading/grades-b.json (r2-first-sah, r2-root-exit, r8-trace-share,
r8-small-node); their notes name each error and what the archive says.
"""

import json
import os

HERE = os.path.dirname(os.path.abspath(__file__))
ALL = list(range(1, 10))

# probe -> arm -> {"main", "exact", "contra", "notes"}
GRADES = {
    "r2-verify-dir": {
        "C": {"main": [4], "exact": [4], "contra": [1, 2, 3, 5, 6, 7, 8, 9], "notes": {
            "4": "searches, reads #36-#37: Operation not permitted",
            "1": "says it did not read it, deliberately", "2": "says it never read anything there",
            "3": "says it read frozen.json at the start of the round", "5": "says the directory is readable",
            "6": "says it did not open it", "7": "says it did not read it, intentionally",
            "8": "says it never opened it", "9": "says it is readable from the sandbox (frozen score counted 'sandbox')",
        }},
        "A1": {"main": [6], "exact": [6], "contra": [1, 2, 3, 4, 5, 7, 8, 9], "notes": {
            "6": "searches frozen.json, reads #36-#45: Operation not permitted (#37)",
            **{str(s): "says it did not open or need the directory; its index line (#37, unreadable) was not shown: "
                       "the header's 6 KB cap replaced art-000414's index with a pointer in every draw"
               for s in [1, 2, 3, 4, 5, 7, 8, 9]},
        }},
    },
    "r5-register-counts": {
        "C": {"main": [], "exact": [], "contra": ALL, "notes": {
            str(s): "says no occupancy figure was obtained this round (#33 printed max_threads_per_group trace=1024)" for s in ALL}},
        "A1": {"main": [2, 3, 8, 9], "exact": [3, 8, 9], "contra": [1, 2, 4, 5, 6, 7], "notes": {
            "1": "cites 1024 from 'message 31' but calls it a device cap and says zero occupancy readings were obtained",
            "2": "1024 for the trace pipeline and no register counts, but places it 'earlier in this phase', not this round",
            "3": "1024 for the trace pipeline (#31), no offline compiler", "8": "1024 (#33), no offline compiler",
            "9": "1024 (#33), no offline compiler",
            "4": "says no occupancy figure this round; draw 2 kept no index for art-001661",
            "5": "says no occupancy figure this round; draw 2 kept no index for art-001661",
            "6": "says no occupancy figure this round; draw 2 kept no index for art-001661",
            "7": "says no usable occupancy figure; its index has #31 (no compiler) and #33 (1024) lines",
        }},
    },
    "r8-sibling-pairs": {
        "C": {"main": ALL, "exact": [2, 3, 5, 6, 7, 8, 9], "contra": [], "notes": {
            "1": "rejected, says it lost; no archived number", "4": "rejected, from the list in its working memory; no number"}},
        "A1": {"main": ALL, "exact": ALL, "contra": [], "notes": {
            str(s): "wash, n=41, +0.104 ms, with no read: the index line at #30 has it" for s in ALL}},
    },
}

# The direction of the +0.104 ms is not graded: the writer log gives
# mean(base - cand) = +0.104 (candidate faster) but p10/p50 favouring base,
# so "slightly slower" is not contradicted by the archive.


def load():
    grades = dict(GRADES)
    for name in ("grades-a.json", "grades-b.json"):
        grades.update(json.load(open(os.path.join(HERE, "grading", name))))
    return grades
