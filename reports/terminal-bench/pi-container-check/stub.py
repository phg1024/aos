"""A scripted stand-in for the relay, and a proxy that refuses and records.

Stdlib only: it runs on the host for the Harbor trials and inside a
python:3.12 container for the per-image checks. No model is involved.

The stub plays two turns and keeps no state between requests, so several
runs can use it one after another: a request with no assistant message yet
gets a bash tool call; any later request gets a final answer. In hang mode
the tool call is a long sleep, so the agent is still running when Harbor's
timeout ends it.

    python3 stub.py --stub-port 8000 --stub-record stub.jsonl [--mode hang]
        [--proxy-port 8001 --proxy-record proxy.jsonl] [--host 0.0.0.0]
"""

from __future__ import annotations

import argparse
import http.server
import json
import threading
import time

MODEL = "qwen3.8-flash-next-nvfp4-nvfp4"

COMMANDS = {"normal": "pwd; ls -la | head -20", "hang": "sleep 3600"}

# prompt_tokens includes the cached tokens, as OpenAI reports them.
TOOL_USAGE = {"prompt_tokens": 1200, "completion_tokens": 30, "prompt_tokens_details": {"cached_tokens": 0}}
FINAL_USAGE = {"prompt_tokens": 1300, "completion_tokens": 12, "prompt_tokens_details": {"cached_tokens": 1100}}


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


def reply(body: dict, mode: str) -> bytes:
    """The streamed answer to one chat-completions request."""
    out = chunk({"role": "assistant"})
    if not any(m.get("role") == "assistant" for m in body.get("messages", [])):
        out += chunk({"reasoning_content": "I will look at the working directory."})
        call = {"name": "bash", "arguments": json.dumps({"command": COMMANDS[mode]})}
        out += chunk({"tool_calls": [{"index": 0, "id": "call_1", "type": "function", "function": call}]})
        out += chunk({}, finish="tool_calls")
        usage = TOOL_USAGE
    else:
        out += chunk({"content": "Done."})
        out += chunk({}, finish="stop")
        usage = FINAL_USAGE
    return out + chunk({}, usage=usage) + b"data: [DONE]\n\n"


class Recorder(http.server.ThreadingHTTPServer):
    def __init__(self, address, handler, record: str, mode: str = "normal"):
        self.record_path, self.mode, self.lock = record, mode, threading.Lock()
        super().__init__(address, handler)

    def record(self, entry: dict) -> None:
        with self.lock, open(self.record_path, "a") as f:
            f.write(json.dumps({"time": time.time(), **entry}) + "\n")


class StubHandler(http.server.BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):
        pass

    def do_POST(self):
        raw = self.rfile.read(int(self.headers.get("Content-Length") or 0))
        try:
            body = json.loads(raw or b"{}")
        except ValueError:
            body = {"unparsed": raw[:2000].decode(errors="replace")}
        entry = {"method": "POST", "path": self.path, "client": self.client_address[0],
                 "authorization": self.headers.get("Authorization"), "body": body}
        self.server.record(entry)
        if self.path.rstrip("/") != "/v1/chat/completions":
            self.send_error(404)
            return
        payload = reply(body, self.server.mode)
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def do_GET(self):
        self.server.record({"method": "GET", "path": self.path, "client": self.client_address[0]})
        self.send_error(404)


class ProxyHandler(http.server.BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):
        pass

    def refuse(self):
        self.server.record({"method": self.command, "path": self.path, "client": self.client_address[0]})
        self.send_error(403)

    do_GET = do_POST = do_PUT = do_HEAD = do_CONNECT = refuse


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--host", default="127.0.0.1")
    ap.add_argument("--stub-port", type=int, required=True)
    ap.add_argument("--stub-record", required=True)
    ap.add_argument("--mode", choices=sorted(COMMANDS), default="normal")
    ap.add_argument("--proxy-port", type=int)
    ap.add_argument("--proxy-record")
    args = ap.parse_args()
    servers = [Recorder((args.host, args.stub_port), StubHandler, args.stub_record, args.mode)]
    if args.proxy_port:
        servers.append(Recorder((args.host, args.proxy_port), ProxyHandler, args.proxy_record))
    for server in servers[1:]:
        threading.Thread(target=server.serve_forever, daemon=True).start()
    print(f"stub on {args.host}:{args.stub_port} ({args.mode})", flush=True)
    servers[0].serve_forever()


if __name__ == "__main__":
    main()
