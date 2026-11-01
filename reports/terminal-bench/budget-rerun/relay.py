"""Host-side HTTP relay from the task containers to the Strata on phg-gpu.

Docker Desktop's containers on this Mac cannot open connections to the LAN
(10.0.0.197 refuses them from inside a container, while the host reaches it),
but they do reach services on the host's loopback through
host.docker.internal. This relay listens on 127.0.0.1 only and forwards each
request to the upstream server. It does three things to each request:

- sets the Host header to the upstream name, so that Strata's
  DNS-rebinding check sees the same Host as in the memory runs;
- adds the API key itself, so the key never enters a task container;
- counts the requests it forwards (relay.log), so that requests from any
  other client to phg-gpu can be detected by comparing counts.

Response bodies are streamed back as they arrive.

Usage: STRATA_KEY=... python3 -I relay.py --listen 18080 \
           --upstream 10.0.0.197:8080 --host-header phg-gpu.local:8080
"""

import argparse
import datetime
import http.client
import http.server
import os
import sys
import threading

HOP = {"connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te",
       "trailers", "transfer-encoding", "upgrade", "host", "authorization", "content-length"}


def make_handler(upstream, host_header, key, log, lock):
    host, port = upstream.rsplit(":", 1)

    class Relay(http.server.BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def log_message(self, fmt, *args):
            pass

        def _forward(self):
            length = int(self.headers.get("Content-Length") or 0)
            body = self.rfile.read(length) if length else None
            headers = {k: v for k, v in self.headers.items() if k.lower() not in HOP}
            headers["Host"] = host_header
            if key:
                headers["Authorization"] = f"Bearer {key}"
            started = datetime.datetime.now(datetime.timezone.utc)
            conn = http.client.HTTPConnection(host, int(port), timeout=3700)
            try:
                conn.request(self.command, self.path, body=body, headers=headers)
                resp = conn.getresponse()
                self.send_response(resp.status, resp.reason)
                for k, v in resp.getheaders():
                    if k.lower() not in HOP:
                        self.send_header(k, v)
                self.send_header("Transfer-Encoding", "chunked")
                self.send_header("Connection", "close")
                self.end_headers()
                while True:
                    chunk = resp.read1(65536)
                    if not chunk:
                        break
                    self.wfile.write(b"%x\r\n%s\r\n" % (len(chunk), chunk))
                    self.wfile.flush()
                self.wfile.write(b"0\r\n\r\n")
                status = resp.status
            except Exception as exc:  # report upstream failures to the client
                status = 502
                try:
                    msg = f"relay: upstream error: {exc}".encode()
                    self.send_response(502)
                    self.send_header("Content-Length", str(len(msg)))
                    self.send_header("Connection", "close")
                    self.end_headers()
                    self.wfile.write(msg)
                except Exception:
                    pass
            finally:
                conn.close()
                self.close_connection = True
            with lock:
                log.write(f"{started.isoformat()} {self.command} {self.path} {status}\n")
                log.flush()

        do_GET = do_POST = _forward

    return Relay


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--listen", type=int, default=18080)
    ap.add_argument("--upstream", required=True)
    ap.add_argument("--host-header", required=True)
    ap.add_argument("--log", default=os.path.join(os.path.dirname(os.path.abspath(__file__)), "relay.log"))
    a = ap.parse_args()
    log = open(a.log, "a")
    handler = make_handler(a.upstream, a.host_header, os.environ.get("STRATA_KEY", ""), log, threading.Lock())
    server = http.server.ThreadingHTTPServer(("127.0.0.1", a.listen), handler)
    print(f"relay 127.0.0.1:{a.listen} -> {a.upstream} (Host: {a.host_header})", file=sys.stderr, flush=True)
    server.serve_forever()


if __name__ == "__main__":
    main()
