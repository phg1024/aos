#!/usr/bin/env python3
"""A scripted OpenAI-compatible endpoint for ipc-step-4-e2e: no real model.

It tells the agents apart by the system prompt's "Agent instructions" line and
plays each one's script. PIDs are fixed by the order the workflow spawns them:
lead pid-000002, receiver pid-000003, stranger pid-000004.
"""
import json, re, sys
from http.server import BaseHTTPRequestHandler, HTTPServer

LEAD, RECEIVER, STRANGER = "pid-000002", "pid-000003", "pid-000004"
BIG = "x" * (1 << 20)


def tool(name, **args):
    return {"type": "tool", "tool_call": {"name": name, "args": args}}


def send(to, body):
    return {"type": "send", "send": {"to": to, "body": body}}


def done(reason):
    return {"type": "exit", "exit_status": {"code": 0, "reason": reason, "retryable": False}}


def delegate(agent, goal):
    return {"type": "delegate", "delegate": {"agent_id": agent, "goal": goal}}


handle = {}


def lead():
    yield delegate("receiver", "wait for mail")                            # parks, so it has a PID
    yield send(RECEIVER, BIG)                                              # refused: too large
    yield tool("message_agent", to=RECEIVER, body=BIG)                     # refused: too large
    text = yield tool("publish_artifact", type="report", body=BIG)
    handle["id"] = re.findall(r"art-\d+", text)[-1]
    yield tool("message_agent", to=RECEIVER, body="the report", artifacts=handle["id"])
    yield delegate("stranger", "try to read it")                           # refused: no right
    for i in range(255):                                                   # 256 with the report
        yield send(RECEIVER, f"filler {i}")
    yield send(RECEIVER, "the 257th")                                      # refused: full
    yield delegate("receiver", "read your mail")                           # drains
    yield send(RECEIVER, "after the drain")                                # accepted
    yield delegate("receiver", "last one")
    yield done("lead finished")


def stranger():
    yield tool("read_artifact", id=handle["id"])
    yield tool("list_artifacts")
    yield done("stranger finished")


def receiver():
    yield {"type": "wait", "message": "waiting for mail"}
    offset = "0"
    while True:
        text = yield tool("read_artifact", id=handle["id"], offset=offset)
        # While mail keeps coming, compaction under the default policy archives a page before
        # any call has it, and all the request holds is a reference: read it again.
        if "read_artifact:map[" not in text:
            continue
        match = re.search(r"next_offset:(\d+)", text[-200:])
        if not match:
            break
        offset = match.group(1)
    for _ in range(30):                                                    # a step takes a batch
        yield {"type": "message", "message": "reading my mail"}
    yield {"type": "wait", "message": "read it all"}
    yield done("receiver finished")


scripts = {"Lead.": lead(), "Stranger.": stranger(), "Receiver.": receiver()}
started = set()


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def do_POST(self):
        request = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        messages = request["messages"]
        agent = re.search(r"Agent instructions:\n(\S+)", messages[0]["content"]).group(1)
        last = messages[-1]["content"]
        # Mail and notes can come after a tool's result, so a reader looks for the result.
        for message in reversed(messages):
            if "read_artifact:map[" in str(message["content"]):
                last = message["content"]
                break
        script = scripts[agent]
        action = next(script) if agent not in started else script.send(last)
        started.add(agent)
        body = json.dumps({
            "choices": [{"message": {"content": json.dumps({"action": action})}}],
            "usage": {"prompt_tokens": 10, "completion_tokens": 1},
        }).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


HTTPServer(("127.0.0.1", int(sys.argv[1])), Handler).serve_forever()
