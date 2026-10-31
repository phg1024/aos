"""Blind subsample for reader grading of reasons: 10 answers per memory
phase (1, 2, 3), two per probe that all three phases share, none of the
ten already graded. Writes blind.json and key.json into the directory
given; the Markdown file for the grader is written by hand around
blind.json. Do not run before grading: it reads the key's sources.

Usage, from the worktree: python3 -I make_reasons.py <out-dir>
"""

import ast
import json
import os
import random
import re
import sys

HERE = os.path.dirname(os.path.abspath(__file__))

# The same redaction as the first blind file: make_blind.py's redact(),
# taken from its source (the script runs on import).
_source = open(os.path.join(os.path.dirname(HERE), "make_blind.py")).read()
_redact = next(n for n in ast.parse(_source).body if isinstance(n, ast.FunctionDef) and n.name == "redact")
_namespace = {"re": re}
exec(compile(ast.Module(body=[_redact], type_ignores=[]), "make_blind.py", "exec"), _namespace)
_redact_first = _namespace["redact"]


def redact(answer):
    """make_blind.py's redaction, plus the names of keyed entries and
    sections that phase 3's answers quote."""
    answer = _redact_first(answer)
    answer = re.sub(r"(?i)\s*(under|in|at) the key [\"'][^\"']*[\"'](\s+in the \w+ section)?", "", answer)
    answer = re.sub(r"(?i)\s*in the (rejected|decisions|measurements|open|next) (section|list)\b", "", answer)
    answer = re.sub(r"(?i)\b(entry|key) [\"'][^\"']*[\"']", "notes", answer)
    answer = re.sub(r"\[ref\](?=\w)", "[ref] ", answer)
    return answer

ROOT = "/Users/peihongguo/Code/aos/"
WORKTREE = os.path.dirname(os.path.dirname(os.path.dirname(HERE))) + "/"
PROBES = ["tridata-order", "leaf2-speedup", "leaf2-status", "tg-sweep", "w4-first-ab"]


def read(root, path):
    return [dict(json.loads(line), _src=path) for line in open(root + path) if line.strip()]


pools = {
    "phase 1": read(ROOT, "reports/memory-probe-baseline/samples-rescored.jsonl")
    + read(ROOT, "reports/memory-probe-phase2/archive-x6/samples-phase1.jsonl"),
    "phase 2": read(ROOT, "reports/memory-probe-phase2/samples-rescored.jsonl")
    + read(ROOT, "reports/memory-probe-phase2/archive-x6/samples-phase2.jsonl")
    + read(ROOT, "reports/memory-probe-phase2/precedence/samples-rescored.jsonl"),
    "phase 3": read(WORKTREE, "reports/memory-probe-phase3-eval/samples-memory.jsonl"),
}
graded = {(k["source"], k["probe"], k["sample"]) for k in json.load(open(os.path.join(os.path.dirname(HERE), "key.json")))}
rng = random.Random(20261030)
chosen = []
for phase, pool in pools.items():
    for probe in PROBES:
        candidates = [s for s in pool if s["probe"] == probe and s.get("answer") and (s["_src"], probe, s["sample"]) not in graded]
        for s in rng.sample(candidates, 2):
            chosen.append((phase, s))
rng.shuffle(chosen)

questions = {p["id"]: p["question"] for p in json.load(open(WORKTREE + "bench/memory/pathtracer-bunny-memfix-phase3-eval-probes.json"))}
blind, key = [], []
for i, (phase, s) in enumerate(chosen, 1):
    blind.append({"item": i, "question": questions[s["probe"]], "answer": redact(s["answer"])})
    score = "correct" if s["correct"] else "superseded" if s.get("superseded") else "off-topic" if s.get("off_topic") else "not correct"
    key.append({"item": i, "phase": phase, "source": s["_src"], "probe": s["probe"], "sample": s["sample"],
                "pid": s.get("pid"), "condition": s.get("condition"), "frozen_pattern_score": score,
                "steps": s["steps"], "bytes_read": s.get("bytes_read")})
out = sys.argv[1]
json.dump(blind, open(os.path.join(out, "blind.json"), "w"), indent=2, ensure_ascii=False)
json.dump(key, open(os.path.join(out, "key.json"), "w"), indent=2, ensure_ascii=False)
for b, k in zip(blind, key):
    print(k["item"], k["phase"], k["probe"], k["frozen_pattern_score"], len(b["answer"]))
