#!/usr/bin/env python3
"""A scripted model for dry runs of the comparison (run.sh --dry-run).

It answers both wire formats AOS speaks: chat completions
(POST /v1/chat/completions, the Strata arm) and the Anthropic Messages API
(POST /v1/messages, the Sonnet arm). It plays the four agents of
workflows/live-reviewed-merge.json from their system prompts, deciding each
step from how many assistant turns the transcript already holds:

  delivery_lead   delegate merger, file_writer, review_partner; message; exit
  merger          read_file each input; message with the HTML; exit
  file_writer     write_file the HTML from its goal; message; exit
  review_partner  read_file the output; message; exit 0

Two replies are wrapped in prose so the analyzer sees a ladder stage other
than a clean reply, and the merger's HTML holds characters that need
escaping in JSON. Usage is estimated at four characters a token; the
Anthropic replies carry a thinking block and report cache reads, so the
cost and reasoning columns have something to count. Nothing here reads a
key.

    fake_model.py PORT_FILE    (listens on a free port, writes it to PORT_FILE)
"""

import http.server
import json
import os
import re
import socketserver
import sys

HTML = ('<!doctype html><html><head><meta charset="utf-8"><title>Harbor Street Library</title></head>'
        '<body><h1>Harbor Street Library reopens</h1>'
        '<p>After an eight-month renovation the library has an upper reading room, a children&#39;s corner '
        '&amp; a ramp at the main entrance.</p><p>Hours: weekdays until 8 pm; Sundays 1&ndash;5 pm.</p>'
        '<p>Volunteers returned about 12,000 books &mdash; "thank you".</p></body></html>')

AGENTS = [
    ("delivery_lead", "You coordinate the task without using tools"),
    ("merger", "Merge the important content into one compact HTML document"),
    ("file_writer", "Use write_file to write exactly the HTML content"),
    ("review_partner", "inspect the produced output file"),
]


def action(kind, **fields):
    return json.dumps({"action": dict({"type": kind, "notes": "scripted step"}, **fields)})


def tool(name, **args):
    return action("tool", tool_call={"name": name, "args": args})


def exit_ok(reason):
    return action("exit", exit_status={"code": 0, "reason": reason})


def paths(text, suffix):
    seen = []
    for match in re.findall(r"(/[^\s\"'\\,]+" + re.escape(suffix) + r")", text):
        if match not in seen:
            seen.append(match)
    return seen


def html_in(text):
    match = re.search(r"<!doctype html>.*?</html>", text.replace('\\"', '"'), re.S)
    return match.group(0) if match else HTML


def step(agent, turns, text):
    """The reply for agent's next step, turns assistant turns in."""
    if agent == "delivery_lead":
        script = [
            action("delegate", delegate={"agent_id": "merger", "goal": "Read all input files and return one compact HTML document in a message action, then exit."}),
            "Merger is done. Next I hand the HTML to file_writer: " + action("delegate", delegate={
                "agent_id": "file_writer", "goal": "Write exactly this HTML document to the output path: " + html_in(text)}),
            action("delegate", delegate={"agent_id": "review_partner", "goal": "Read the produced file and return a short review note."}),
            action("message", message="Done: the note is written and review_partner passed it."),
            exit_ok("workflow complete"),
        ]
    elif agent == "merger":
        # Only files that exist: the system prompt's examples name others.
        inputs = [p for p in paths(text, ".txt") if os.path.isfile(p)]
        script = [tool("read_file", path=p) for p in inputs]
        script += ["Here is the merged document. " + action("message", message=HTML), exit_ok("merged")]
    elif agent == "file_writer":
        outputs = paths(text, ".html")
        script = [tool("write_file", path=outputs[0] if outputs else "output.html", content=html_in(text)),
                  action("message", message="Wrote the HTML document."), exit_ok("written")]
    else:
        outputs = paths(text, ".html")
        script = [tool("read_file", path=outputs[0] if outputs else "output.html"),
                  action("message", message="Coherent, no duplication, main points kept."), exit_ok("coherent, no duplication")]
    return script[min(turns, len(script) - 1)]


def agent_of(system):
    for name, phrase in AGENTS:
        if phrase in system:
            return name
    return "review_partner"


class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers.get("Content-Length", 0))) or b"{}")
        messages = body.get("messages", [])
        if self.path == "/v1/messages":
            system = body.get("system", "")
        else:
            system = next((m["content"] for m in messages if m.get("role") == "system"), "")
        text = "\n".join(m.get("content", "") for m in messages if isinstance(m.get("content"), str))
        turns = sum(1 for m in messages if m.get("role") == "assistant")
        reply = step(agent_of(system), turns, system + "\n" + text)
        prompt = (len(system) + len(text)) // 4
        completion = max(1, len(reply) // 4)
        cached = prompt * 8 // 10 if turns else 0
        if self.path == "/v1/messages":
            payload = {"type": "message", "role": "assistant", "model": body.get("model", "scripted"), "stop_reason": "end_turn",
                       "content": [{"type": "thinking", "thinking": "Following the script.", "signature": "s"}, {"type": "text", "text": reply}],
                       "usage": {"input_tokens": prompt - cached, "cache_read_input_tokens": cached,
                                 "cache_creation_input_tokens": 0 if turns else prompt // 2, "output_tokens": completion}}
        elif self.path == "/v1/chat/completions":
            payload = {"choices": [{"message": {"role": "assistant", "content": reply, "reasoning_content": "Following the script."}}],
                       "usage": {"prompt_tokens": prompt, "completion_tokens": completion, "prompt_tokens_details": {"cached_tokens": cached}}}
        else:
            self.send_error(404)
            return
        data = json.dumps(payload).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)


def main():
    server = socketserver.ThreadingTCPServer(("127.0.0.1", 0), Handler)
    with open(sys.argv[1], "w") as f:
        f.write(str(server.server_address[1]))
    server.serve_forever()


if __name__ == "__main__":
    main()
