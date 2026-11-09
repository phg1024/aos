"""Surveys every terminal-bench@2.0 task for the feasibility filter (PLAN.md).

For each task it records, from task.toml as Harbor 0.25.0 resolves it: CPUs,
memory, GPUs, agent and verifier timeouts and the verifier's environment
mode; and, from the image's registry manifest (no pull): its platforms and
compressed size. Run with Harbor's own Python (it needs harbor and tomllib):

  <harbor python> -I survey.py <dataset dir> > survey.json
"""

import json
import subprocess
import sys
from pathlib import Path

from harbor.models.task.config import TaskConfig


def image_info(ref):
    def inspect(fmt):
        out = subprocess.run(["docker", "buildx", "imagetools", "inspect", "--format", fmt, ref],
                             capture_output=True, text=True, timeout=120)
        if out.returncode != 0:
            raise RuntimeError(out.stderr.strip()[:300])
        return json.loads(out.stdout)
    manifest = inspect("{{json .Manifest}}")
    if manifest.get("manifests"):  # a list: one entry per platform
        platforms = sorted(f'{m["platform"]["os"]}/{m["platform"]["architecture"]}'
                           for m in manifest["manifests"] if m.get("platform"))
        size = None
    else:
        image = inspect("{{json .Image}}")
        platforms = [f'{image["os"]}/{image["architecture"]}']
        raw = json.loads(subprocess.run(["docker", "buildx", "imagetools", "inspect", "--raw", ref],
                                        capture_output=True, text=True, timeout=120).stdout)
        size = sum(layer["size"] for layer in raw.get("layers", []))
    return platforms, size


def main():
    rows = []
    for toml in sorted(Path(sys.argv[1]).glob("*/task.toml")):
        cfg = TaskConfig.model_validate_toml(toml.read_text())
        env = cfg.environment
        row = {"task": toml.parent.name, "cpus": env.cpus, "memory_mb": env.memory_mb,
               "gpus": env.gpus, "agent_timeout_s": cfg.agent.timeout_sec,
               "verifier_timeout_s": cfg.verifier.timeout_sec,
               # Harbor's rule when environment_mode is omitted: separate if the
               # verifier has its own environment, otherwise shared.
               "verifier_mode": (cfg.verifier.environment_mode.value if cfg.verifier.environment_mode
                                 else "separate" if cfg.verifier.environment else "shared"),
               "image": env.docker_image}
        try:
            row["platforms"], row["compressed_bytes"] = image_info(env.docker_image)
        except Exception as exc:
            row["platforms"], row["compressed_bytes"], row["image_error"] = [], None, str(exc)
        rows.append(row)
        print(row["task"], row["platforms"], file=sys.stderr, flush=True)
    print(json.dumps(rows, indent=1))


if __name__ == "__main__":
    main()
