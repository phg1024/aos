#!/usr/bin/env python3
"""Fill in workflows/rasterizer-bunny-metal-ipc.json for one run, with no hand edit.

Usage: render.py <run name>, from anywhere. Writes reports/ipc-live/runs/<name>/workflow.json
and creates its directory, which must be new, so a run never meets an old run's results.
It checks the CPU reference the prompts name, which is not in git, and copies nothing.
"""
import json, os, sys

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
REFERENCE = os.path.join(ROOT, "reports", "rasterizer-bunny")
NEEDS = [
    "baseline/rasterizer", "baseline/src", "baseline/include", "baseline_benchmark.json",
    "optimized_benchmark.json", "final-summary.txt", "assets/stanford-bunny.obj",
    "scene-closeup.json", "scene-side.json", "scene-small.json",
]


def main():
    if len(sys.argv) != 2 or not sys.argv[1] or "/" in sys.argv[1]:
        sys.exit(__doc__)
    missing = [need for need in NEEDS if not os.path.exists(os.path.join(REFERENCE, need))]
    if missing:
        sys.exit(f"the CPU reference {REFERENCE} is not in git and lacks: {', '.join(missing)}")
    run_dir = os.path.join(ROOT, "reports", "ipc-live", "runs", sys.argv[1])
    os.makedirs(run_dir, exist_ok=False)
    with open(os.path.join(ROOT, "workflows", "rasterizer-bunny-metal-ipc.json")) as f:
        text = f.read()
    # JSON-escaped, though neither path is expected to need it.
    fill = lambda path: json.dumps(path)[1:-1]
    text = text.replace("__RUN_DIR__", fill(run_dir)).replace("__AOS_ROOT__", fill(ROOT))
    json.loads(text)
    out = os.path.join(run_dir, "workflow.json")
    with open(out, "w") as f:
        f.write(text)
    print(out)


main()
