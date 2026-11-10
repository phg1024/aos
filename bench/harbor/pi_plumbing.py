"""Local plumbing check for pi_agent.py: the pinned pi build against a scripted stub.

No Docker, no Harbor run, no model. On an Apple-silicon Mac this runs the
cached darwin-arm64 build of PI_VERSION with the models.json and command
pi_agent.py would use in a container, against a stub OpenAI-compatible
server on 127.0.0.1 that plays two scripted turns: a bash tool call, then a
final answer. It checks:

- the request bodies carry the arm's sampling fields and none of the fields
  pi would otherwise add;
- the only credential sent is the placeholder;
- nothing but the stub is contacted: HTTP(S)_PROXY points at a proxy that
  refuses and records every request, and the run is wrapped in
  sandbox-exec, which denies all other outbound connections;
- the session file records both turns with their usage, and Harbor's pi
  parsing totals it as the stub reported.

    PYTHONPATH=bench/harbor <harbor's python> bench/harbor/pi_plumbing.py [--keep DIR]

Writes the captured request bodies to DIR/requests.json.
"""

from __future__ import annotations

import argparse
import http.server
import json
import os
import platform
import shutil
import subprocess
import sys
import tempfile
import threading
from pathlib import Path

import pi_agent

MODEL = pi_agent.FORWARDED_ENV["AOS_LLM_MODEL"]
INSTRUCTION = "Create out.txt containing hello, then say done."

# Two scripted turns. Usage: prompt_tokens includes the cached tokens, as
# OpenAI reports them.
TURNS = [
    {
        "reasoning": "I will write the file with bash.",
        "tool_call": {"name": "bash", "arguments": json.dumps({"command": "echo hello > out.txt"})},
        "usage": {"prompt_tokens": 1200, "completion_tokens": 30, "prompt_tokens_details": {"cached_tokens": 0}},
    },
    {
        "content": "Done: out.txt contains hello.",
        "usage": {"prompt_tokens": 1300, "completion_tokens": 12, "prompt_tokens_details": {"cached_tokens": 1100}},
    },
]

# Fields the arm must send, with their values.
EXPECTED_FIELDS = {"temperature": 0.6, "top_p": 0.95, "max_tokens": 8192, "reasoning_budget_tokens": 2048}
# Fields pi would add with other settings; none may appear.
FORBIDDEN_FIELDS = ("max_completion_tokens", "reasoning_effort", "store", "prompt_cache_key", "thinking", "chat_template_kwargs")


def chunk(delta: dict, finish: str | None = None, usage: dict | None = None) -> bytes:
    body = {
        "id": "chatcmpl-stub",
        "object": "chat.completion.chunk",
        "created": 0,
        "model": MODEL,
        "choices": [] if usage else [{"index": 0, "delta": delta, "finish_reason": finish}],
    }
    if usage:
        body["usage"] = usage
    return b"data: " + json.dumps(body).encode() + b"\n\n"


def turn_stream(turn: dict) -> bytes:
    out = chunk({"role": "assistant"})
    if turn.get("reasoning"):
        out += chunk({"reasoning_content": turn["reasoning"]})
    if turn.get("tool_call"):
        call = turn["tool_call"]
        out += chunk({"tool_calls": [{"index": 0, "id": "call_1", "type": "function", "function": call}]})
        out += chunk({}, finish="tool_calls")
    else:
        out += chunk({"content": turn["content"]})
        out += chunk({}, finish="stop")
    return out + chunk({}, usage=turn["usage"]) + b"data: [DONE]\n\n"


class Stub(http.server.ThreadingHTTPServer):
    def __init__(self):
        self.requests: list[dict] = []
        self.unexpected: list[str] = []
        super().__init__(("127.0.0.1", 0), StubHandler)


class StubHandler(http.server.BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):
        pass

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers.get("Content-Length") or 0)) or b"{}")
        server = self.server
        if self.path != "/v1/chat/completions" or len(server.requests) >= len(TURNS):
            server.unexpected.append(f"POST {self.path}")
            self.send_error(404)
            return
        server.requests.append({"path": self.path, "authorization": self.headers.get("Authorization"), "body": body})
        payload = turn_stream(TURNS[len(server.requests) - 1])
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def do_GET(self):
        self.server.unexpected.append(f"GET {self.path}")
        self.send_error(404)


class RefusingProxy(http.server.ThreadingHTTPServer):
    """Records and refuses every request routed through HTTP(S)_PROXY."""

    def __init__(self):
        self.seen: list[str] = []
        super().__init__(("127.0.0.1", 0), RefusingProxyHandler)


class RefusingProxyHandler(http.server.BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):
        pass

    def refuse(self):
        self.server.seen.append(f"{self.command} {self.path}")
        self.send_error(403)

    do_GET = do_POST = do_CONNECT = do_HEAD = refuse


def sandbox_profile(stub_port: int, proxy_port: int) -> str:
    """Allow everything but outbound network, then only the stub and the proxy."""
    return (
        "(version 1)\n(allow default)\n(deny network-outbound)\n"
        f'(allow network-outbound (remote ip "localhost:{stub_port}"))\n'
        f'(allow network-outbound (remote ip "localhost:{proxy_port}"))\n'
        '(allow network-outbound (remote unix-socket))\n'
    )


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--keep", help="directory for the run's files (default: a temporary one, removed)")
    args = ap.parse_args()
    if (sys.platform, platform.machine()) != ("darwin", "arm64"):
        print("pi_plumbing: needs an Apple-silicon Mac (darwin-arm64 build)", file=sys.stderr)
        return 2

    root = Path(args.keep or tempfile.mkdtemp(prefix="pi-plumbing-")).resolve()
    root.mkdir(parents=True, exist_ok=True)
    bundle = pi_agent.extract_bundle(pi_agent.release_tarball("darwin-arm64"), root / "bundle")
    home, config, work, logs = root / "home", root / "config", root / "work", root / "logs"
    for d in (home, config, work, logs):
        d.mkdir(exist_ok=True)

    stub, proxy = Stub(), RefusingProxy()
    for server in (stub, proxy):
        threading.Thread(target=server.serve_forever, daemon=True).start()
    stub_port, proxy_port = stub.server_address[1], proxy.server_address[1]

    settings = pi_agent.endpoint_settings({})
    config_json = pi_agent.models_config(settings)
    # The container rewrites loopback to host.docker.internal; here the stub
    # is on this host's loopback.
    config_json["providers"][pi_agent.PROVIDER]["baseUrl"] = f"http://127.0.0.1:{stub_port}/v1"
    (config / "models.json").write_text(json.dumps(config_json, indent=2) + "\n")

    session_dir, output = logs / "pi" / "sessions", logs / "pi.txt"
    command = pi_agent.pi_command(settings["AOS_LLM_MODEL"], str(session_dir), str(output), binary=str(bundle / "pi"))
    proxy_url = f"http://127.0.0.1:{proxy_port}"
    env = {
        "PATH": "/usr/bin:/bin:/usr/sbin:/sbin",
        "HOME": str(home),
        "TMPDIR": str(root / "tmp"),
        **pi_agent.PI_ENV,
        "PI_CODING_AGENT_DIR": str(config),
        "PI_INSTRUCTION": INSTRUCTION,
        "HTTP_PROXY": proxy_url,
        "HTTPS_PROXY": proxy_url,
        "http_proxy": proxy_url,
        "https_proxy": proxy_url,
        "NO_PROXY": "127.0.0.1",
        "no_proxy": "127.0.0.1",
    }
    (root / "tmp").mkdir(exist_ok=True)
    profile = root / "sandbox.sb"
    profile.write_text(sandbox_profile(stub_port, proxy_port))
    run = subprocess.run(
        ["/usr/bin/sandbox-exec", "-f", str(profile), "/bin/bash", "-c", f"set -o pipefail; {command}"],
        cwd=work, env=env, capture_output=True, text=True, timeout=300,
    )
    stub.shutdown()
    proxy.shutdown()

    (root / "requests.json").write_text(json.dumps(stub.requests, indent=2) + "\n")
    failures = check(run, stub, proxy, work, output, session_dir, logs)
    print(json.dumps({"dir": str(root), "requests": len(stub.requests), "failures": failures}, indent=2))
    if not args.keep and not failures:
        shutil.rmtree(root)
    return 1 if failures else 0


def check(run, stub, proxy, work: Path, output: Path, session_dir: Path, logs: Path) -> list[str]:
    failures = []
    if run.returncode != 0:
        failures.append(f"shell exit {run.returncode}: {run.stderr[-2000:]}")
    if pi_agent.pi_exit_from_output(output) != "0":
        failures.append(f"pi_exit is {pi_agent.pi_exit_from_output(output)!r}")
    if len(stub.requests) != len(TURNS):
        failures.append(f"{len(stub.requests)} requests reached the stub, want {len(TURNS)}")
    if stub.unexpected:
        failures.append(f"unexpected stub requests: {stub.unexpected}")
    if proxy.seen:
        failures.append(f"requests through the proxy: {proxy.seen}")
    for i, request in enumerate(stub.requests, 1):
        body = request["body"]
        if request["authorization"] != f"Bearer {pi_agent.PLACEHOLDER_API_KEY}":
            failures.append(f"request {i}: Authorization {request['authorization']!r}")
        if body.get("model") != MODEL:
            failures.append(f"request {i}: model {body.get('model')!r}")
        for key, want in EXPECTED_FIELDS.items():
            if body.get(key) != want:
                failures.append(f"request {i}: {key}={body.get(key)!r}, want {want!r}")
        for key in FORBIDDEN_FIELDS:
            if key in body:
                failures.append(f"request {i}: unexpected field {key}={body[key]!r}")
        roles = [m.get("role") for m in body.get("messages", [])]
        if not roles or roles[0] != "system":
            failures.append(f"request {i}: first role {roles[:1]}, want system")
    written = work / "out.txt"
    if not written.is_file() or written.read_text() != "hello\n":
        failures.append("the bash tool call did not write out.txt")

    # Harbor's own parsing, as populate_context_post_run runs it after a trial.
    from harbor.models.agent.context import AgentContext

    agent = pi_agent.PiAgent(logs_dir=logs)
    context = AgentContext()
    agent.populate_context_post_run(context)
    want_input = sum(t["usage"]["prompt_tokens"] for t in TURNS)
    want_cache = sum(t["usage"]["prompt_tokens_details"]["cached_tokens"] for t in TURNS)
    want_output = sum(t["usage"]["completion_tokens"] for t in TURNS)
    got = (context.n_input_tokens, context.n_cache_tokens, context.n_output_tokens)
    if got != (want_input, want_cache, want_output):
        failures.append(f"usage {got}, want {(want_input, want_cache, want_output)}")
    if (context.metadata or {}).get("usage_source") != "session":
        failures.append(f"usage source {(context.metadata or {}).get('usage_source')!r}")
    if not list(session_dir.glob("*.jsonl")):
        failures.append("no session file")
    if not (logs / "trajectory.json").is_file():
        failures.append("no trajectory.json")
    return failures


if __name__ == "__main__":
    sys.exit(main())
