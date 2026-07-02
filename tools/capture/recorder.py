#!/usr/bin/env python3
"""Local request recorder. The clients are pointed here via *_BASE_URL so they
transmit their real request (headers in wire order + body) in plaintext — we log
it and return a minimal valid-ish response. The client builds the same request
regardless of destination, so this faithfully captures the HTTP-layer fingerprint
(the TLS JA3 comes separately from the passive pcap)."""
import json
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(sys.argv[1])
OUT = sys.argv[2]
LABEL = sys.argv[3] if len(sys.argv) > 3 else "?"

ANTHROPIC_JSON = (b'{"id":"msg_01","type":"message","role":"assistant",'
                  b'"model":"claude","content":[{"type":"text","text":"hi"}],'
                  b'"stop_reason":"end_turn","stop_sequence":null,'
                  b'"usage":{"input_tokens":1,"output_tokens":1}}')


class H(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def _record(self):
        n = int(self.headers.get("content-length", "0") or 0)
        body = self.rfile.read(n) if n else b""
        rec = {
            "label": LABEL,
            "request_line": f"{self.command} {self.path} {self.request_version}",
            "method": self.command,
            "path": self.path,
            # email.message preserves on-wire header order and casing.
            "headers": [[k, v] for (k, v) in self.headers.items()],
            "body_text": body.decode("utf-8", "replace"),
        }
        try:
            rec["body_json"] = json.loads(body.decode("utf-8"))
            rec["body_text"] = None
        except Exception:
            rec["body_json"] = None
        with open(OUT, "a") as f:
            f.write(json.dumps(rec) + "\n")

    def _respond(self):
        self.send_response(200)
        self.send_header("content-type", "application/json")
        self.send_header("content-length", str(len(ANTHROPIC_JSON)))
        self.end_headers()
        try:
            self.wfile.write(ANTHROPIC_JSON)
        except Exception:
            pass

    def do_POST(self):
        self._record()
        self._respond()

    do_GET = do_POST
    do_PUT = do_POST

    def log_message(self, *a):
        pass


if __name__ == "__main__":
    ThreadingHTTPServer(("127.0.0.1", PORT), H).serve_forever()
