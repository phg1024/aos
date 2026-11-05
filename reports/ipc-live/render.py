#!/usr/bin/env python3
"""Fill in a rasterizer IPC workflow for one run, with no hand edit.

Usage: render.py <run name> [metal|cuda], from anywhere. Writes <root>/reports/ipc-live/runs/<name>/workflow.json
and creates its directory, which must be new, so a run never meets an old run's results. The root is this
checkout, or AOS_IPC_ROOT for a machine that holds only what a run needs. It checks the CPU reference the
prompts name, which is not in git, and copies nothing. cuda also needs AOS_CUDA_HOME and AOS_GPU_UUID.
"""
import json, os, sys

ROOT = os.environ.get("AOS_IPC_ROOT") or os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
REFERENCE = os.path.join(ROOT, "reports", "rasterizer-bunny")
NEEDS = [
    "baseline/src", "baseline/include", "baseline_benchmark.json",
    "optimized_benchmark.json", "final-summary.txt", "assets/stanford-bunny.obj",
    "scene-closeup.json", "scene-side.json", "scene-small.json",
]


def main():
    if len(sys.argv) not in (2, 3) or not sys.argv[1] or "/" in sys.argv[1]:
        sys.exit(__doc__)
    kind = sys.argv[2] if len(sys.argv) == 3 else "metal"
    if kind not in ("metal", "cuda"):
        sys.exit(__doc__)
    fills = {"__RUN_DIR__": None, "__AOS_ROOT__": ROOT}
    if kind == "cuda":
        for variable, placeholder in (("AOS_CUDA_HOME", "__CUDA_HOME__"), ("AOS_GPU_UUID", "__GPU_UUID__")):
            if not os.environ.get(variable):
                sys.exit(f"{variable} is not set")
            fills[placeholder] = os.environ[variable]
    missing = [need for need in NEEDS if not os.path.exists(os.path.join(REFERENCE, need))]
    if missing:
        sys.exit(f"the CPU reference {REFERENCE} is not in git and lacks: {', '.join(missing)}")
    with open(os.path.join(ROOT, "workflows", f"rasterizer-bunny-{kind}-ipc.json")) as f:
        text = f.read()
    run_dir = os.path.join(ROOT, "reports", "ipc-live", "runs", sys.argv[1])
    os.makedirs(run_dir, exist_ok=False)
    fills["__RUN_DIR__"] = run_dir
    # JSON-escaped, though none is expected to need it.
    for placeholder, value in fills.items():
        text = text.replace(placeholder, json.dumps(value)[1:-1])
    json.loads(text)
    out = os.path.join(run_dir, "workflow.json")
    with open(out, "w") as f:
        f.write(text)
    print(out)


main()
