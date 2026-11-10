"""Per-image container checks for the pi arm (bench/harbor/pi_agent.py).

For each of the baseline's 16 drawn task images, under linux/amd64 (Rosetta):

- probe the image: architecture, glibc, bash, GNU grep, user, working
  directory, writable directories, and every AGENTS.md or CLAUDE.md on its
  root filesystem;
- install pi as PiAgent.install does (docker cp of the pinned linux-x64
  bundle, the same chmods and config directory), timing the copy, and check
  `pi --version`;
- run pi_agent.pi_command against stub.py, which plays the relay on an
  internal Docker network with no route out. HTTP(S)_PROXY points at the
  stub container's refusing proxy, so any request pi makes to another host
  is recorded there, and anything that ignores the proxy has nowhere to go;
- check each request as pi_plumbing.py does, the system prompt for context
  files, and the usage Harbor's pi parsing reads from the session file.

No model, no key, no Harbor. Run from the repository root with Harbor's
python:

    PYTHONPATH=bench/harbor <harbor's python> reports/terminal-bench/pi-container-check/check.py \
        --drawn <eval worktree>/reports/terminal-bench/baseline-tb2/drawn.json --out <dir>

Writes <dir>/images.json, and the per-task logs under <dir>/<task>/.
"""

from __future__ import annotations

import argparse
import json
import shlex
import subprocess
import sys
import tempfile
import time
from pathlib import Path

import pi_agent
import pi_plumbing

HERE = Path(__file__).resolve().parent
NETWORK = "pi-check-net"
STUB = "pi-check-stub"
STUB_IMAGE = "python:3.12-bookworm"
STUB_PORT, PROXY_PORT = 8000, 8001
INSTRUCTION = "List the files in the working directory, then say done."

PROBE = r"""
echo "uname=$(uname -m)"
echo "glibc=$(getconf GNU_LIBC_VERSION 2>/dev/null || echo unknown)"
echo "bash=$(command -v bash || echo none)"
echo "grep=$(grep --version 2>&1 | head -1)"
echo "id=$(id -u):$(id -g)"
echo "cwd=$(pwd)"
echo "home=${HOME:-}"
echo "cwd_writable=$( (touch .pi-check-probe && rm -f .pi-check-probe) 2>/dev/null && echo yes || echo no)"
echo "tmp_writable=$( (touch /tmp/.pi-check-probe && rm -f /tmp/.pi-check-probe) 2>/dev/null && echo yes || echo no)"
echo "logs_writable=$( (touch /logs/agent/.pi-check-probe && rm -f /logs/agent/.pi-check-probe) 2>/dev/null && echo yes || echo no)"
find / -xdev \( -iname AGENTS.md -o -iname CLAUDE.md \) 2>/dev/null | sed 's/^/ctxfile=/'
"""


def docker(*args: str, timeout: float = 600, check: bool = True) -> subprocess.CompletedProcess:
    run = subprocess.run(["docker", *args], capture_output=True, text=True, timeout=timeout)
    if check and run.returncode != 0:
        raise RuntimeError(f"docker {' '.join(args[:3])}: exit {run.returncode}: {run.stderr[-1000:]}")
    return run


def start_stub(out: Path) -> None:
    docker("rm", "-f", STUB, check=False)
    if docker("network", "inspect", NETWORK, check=False).returncode != 0:
        docker("network", "create", "--internal", NETWORK)
    docker(
        "run", "-d", "--rm", "--name", STUB, "--network", NETWORK,
        "-v", f"{HERE}:/check:ro", "-v", f"{out}:/out",
        STUB_IMAGE, "python3", "-I", "/check/stub.py", "--host", "0.0.0.0",
        "--stub-port", str(STUB_PORT), "--stub-record", "/out/stub.jsonl",
        "--proxy-port", str(PROXY_PORT), "--proxy-record", "/out/proxy.jsonl",
    )
    time.sleep(2)


def stop_stub() -> None:
    docker("rm", "-f", STUB, check=False)
    docker("network", "rm", NETWORK, check=False)


def records(path: Path) -> list[dict]:
    return [json.loads(line) for line in path.read_text().splitlines()] if path.is_file() else []


def probe_fields(text: str) -> dict:
    fields: dict = {"ctxfiles": []}
    for line in text.splitlines():
        key, _, value = line.partition("=")
        if key == "ctxfile":
            fields["ctxfiles"].append(value)
        elif key:
            fields[key] = value
    return fields


def check_image(task: str, image: str, bundle: Path, models_json: Path, out: Path) -> dict:
    task_out = out / task
    logs = task_out / "logs"
    logs.mkdir(parents=True, exist_ok=True)
    name = f"pi-check-{task}"
    docker("rm", "-f", name, check=False)
    docker(
        "run", "-d", "--platform", "linux/amd64", "--name", name, "--network", NETWORK,
        "-v", f"{logs}:/logs/agent", "--entrypoint", "/bin/sh", image, "-c", "sleep infinity",
    )
    result: dict = {"task": task, "image": image}
    try:
        probe = docker("exec", name, "/bin/sh", "-c", PROBE, check=False)
        (task_out / "probe.txt").write_text(probe.stdout + probe.stderr)
        result["probe"] = probe_fields(probe.stdout)

        def sh(command: str, env: dict | None = None, timeout: float = 600) -> subprocess.CompletedProcess:
            flags = [x for k, v in (env or {}).items() for x in ("-e", f"{k}={v}")]
            return docker("exec", *flags, name, "bash", "-c", command, timeout=timeout, check=False)

        sh(f"mkdir -p {pi_agent.REMOTE_DIR}")
        started = time.monotonic()
        docker("cp", str(bundle), f"{name}:{pi_agent.REMOTE_BUNDLE}")
        result["upload_s"] = round(time.monotonic() - started, 2)
        setup = sh(
            f"chmod -R a+rX {pi_agent.REMOTE_BUNDLE} && chmod 755 {pi_agent.REMOTE_DIR} {pi_agent.REMOTE_BIN}"
            f" && mkdir -p {pi_agent.REMOTE_CONFIG_DIR} && chmod 700 {pi_agent.REMOTE_CONFIG_DIR}"
        )
        result["setup_exit"] = setup.returncode
        docker("cp", str(models_json), f"{name}:{pi_agent.REMOTE_CONFIG_DIR}/models.json")
        started = time.monotonic()
        version = sh(f"{pi_agent.REMOTE_BIN} --version", env=pi_agent.PI_ENV, timeout=120)
        result["version_s"] = round(time.monotonic() - started, 2)
        result["version"] = (version.stdout.strip().splitlines() or [""])[-1]
        result["version_stderr"] = version.stderr.strip()[-500:]

        proxy = f"http://{STUB}:{PROXY_PORT}"
        env = {
            **pi_agent.PI_ENV,
            "PI_INSTRUCTION": INSTRUCTION,
            "HTTP_PROXY": proxy, "HTTPS_PROXY": proxy, "http_proxy": proxy, "https_proxy": proxy,
            "NO_PROXY": STUB, "no_proxy": STUB,
        }
        command = pi_agent.pi_command(
            pi_agent.endpoint_settings({})["AOS_LLM_MODEL"], "/logs/agent/pi/sessions", "/logs/agent/pi.txt"
        )
        before = len(records(out / "stub.jsonl")), len(records(out / "proxy.jsonl"))
        started = time.monotonic()
        run = sh(f"set -o pipefail; {command}", env=env, timeout=300)
        result["run_s"] = round(time.monotonic() - started, 2)
        result["run_exit"] = run.returncode
        result["run_stderr"] = (run.stdout + run.stderr).strip()[-500:]
        time.sleep(0.5)
        result["requests"] = records(out / "stub.jsonl")[before[0]:]
        result["proxied"] = records(out / "proxy.jsonl")[before[1]:]
    finally:
        docker("rm", "-f", name, check=False)
    return result


def summarize(result: dict, out: Path) -> dict:
    """Check one image's run; returns the summary row, with its failures."""
    logs = out / result["task"] / "logs"
    probe = result.get("probe", {})
    failures = []
    if probe.get("uname") != "x86_64":
        failures.append(f"uname {probe.get('uname')!r}")
    if probe.get("bash", "none") == "none":
        failures.append("no bash")
    if "GNU grep" not in probe.get("grep", ""):
        failures.append(f"grep {probe.get('grep')!r}")
    if result.get("version") != pi_agent.PI_VERSION:
        failures.append(f"pi --version {result.get('version')!r}: {result.get('version_stderr')}")
    exit_code = pi_agent.pi_exit_from_output(logs / "pi.txt")
    if exit_code != "0":
        failures.append(f"pi_exit {exit_code!r}; {result.get('run_stderr')}")

    requests = result.get("requests", [])
    chats = [r for r in requests if r.get("path") == "/v1/chat/completions"]
    if len(chats) != 2 or len(requests) != 2:
        failures.append(f"{len(requests)} stub requests ({len(chats)} chat completions), want 2")
    if result.get("proxied"):
        failures.append(f"requests through the proxy: {[(p['method'], p['path']) for p in result['proxied']]}")
    for i, request in enumerate(chats, 1):
        body = request["body"]
        if request.get("authorization") != f"Bearer {pi_agent.PLACEHOLDER_API_KEY}":
            failures.append(f"request {i}: Authorization {request.get('authorization')!r}")
        for key, want in pi_plumbing.EXPECTED_FIELDS.items():
            if body.get(key) != want:
                failures.append(f"request {i}: {key}={body.get(key)!r}")
        for key in pi_plumbing.FORBIDDEN_FIELDS:
            if key in body:
                failures.append(f"request {i}: unexpected {key}")

    system = ""
    if chats:
        first = chats[0]["body"].get("messages", [{}])[0]
        system = first.get("content") if isinstance(first.get("content"), str) else json.dumps(first.get("content"))
    in_prompt = [name for name in ("AGENTS.md", "CLAUDE.md") if name in system]
    # The tool result in the second request: the bash call ran in the
    # task's working directory.
    tool_output = ""
    if len(chats) > 1:
        tool_output = next((m.get("content") for m in chats[1]["body"].get("messages", []) if m.get("role") == "tool"), "")
        tool_output = tool_output if isinstance(tool_output, str) else json.dumps(tool_output)

    from harbor.models.agent.context import AgentContext

    context = AgentContext()
    pi_agent.PiAgent(logs_dir=logs).populate_context_post_run(context)
    usage = (context.n_input_tokens, context.n_cache_tokens, context.n_output_tokens)
    if usage != (2500, 1100, 42):
        failures.append(f"usage {usage}")
    if (context.metadata or {}).get("usage_source") != "session":
        failures.append(f"usage source {(context.metadata or {}).get('usage_source')!r}")
    return {
        "task": result["task"],
        "image": result["image"],
        "glibc": probe.get("glibc"),
        "bash": probe.get("bash"),
        "user": probe.get("id"),
        "cwd": probe.get("cwd"),
        "cwd_writable": probe.get("cwd_writable"),
        "tmp_writable": probe.get("tmp_writable"),
        "logs_writable": probe.get("logs_writable"),
        "context_files": probe.get("ctxfiles", []),
        "system_prompt_chars": len(system),
        "system_prompt_mentions": in_prompt,
        "tool_output_head": tool_output[:200],
        "upload_s": result.get("upload_s"),
        "version_s": result.get("version_s"),
        "run_s": result.get("run_s"),
        "pi_exit": exit_code,
        "requests": len(requests),
        "proxied": len(result.get("proxied", [])),
        "usage": usage,
        "failures": failures,
    }


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--drawn", required=True, help="the baseline's drawn.json")
    ap.add_argument("--survey", help="the baseline's survey.json (default: next to drawn.json)")
    ap.add_argument("--out", required=True)
    ap.add_argument("--task", action="append", help="only these tasks")
    args = ap.parse_args()
    drawn = json.loads(Path(args.drawn).read_text())["drawn"]
    survey = json.loads(Path(args.survey or Path(args.drawn).with_name("survey.json")).read_text())
    image_of = {t["task"]: t["image"] for t in survey}
    tasks = [t for t in drawn if not args.task or t in args.task]
    out = Path(args.out).resolve()
    out.mkdir(parents=True, exist_ok=True)

    settings = pi_agent.endpoint_settings({})
    config = pi_agent.models_config(settings)
    config["providers"][pi_agent.PROVIDER]["baseUrl"] = f"http://{STUB}:{STUB_PORT}/v1"
    models_json = out / "models.json"
    models_json.write_text(json.dumps(config, indent=2) + "\n")

    rows = []
    with tempfile.TemporaryDirectory(prefix="pi-bundle-") as tmp:
        bundle = pi_agent.extract_bundle(pi_agent.release_tarball("linux-x64"), Path(tmp))
        start_stub(out)
        try:
            for task in tasks:
                result = check_image(task, image_of[task], bundle, models_json, out)
                (out / task / "result.json").write_text(json.dumps(result, indent=2) + "\n")
                row = summarize(result, out)
                rows.append(row)
                print(f"{task}: {'ok' if not row['failures'] else row['failures']} "
                      f"(upload {row['upload_s']}s, run {row['run_s']}s, ctx {row['context_files']})", flush=True)
        finally:
            stop_stub()
    (out / "images.json").write_text(json.dumps(rows, indent=2) + "\n")
    return 1 if any(r["failures"] for r in rows) else 0


if __name__ == "__main__":
    sys.exit(main())
