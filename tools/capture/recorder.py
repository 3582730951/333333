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

ANTHROPIC_SSE = (b'event: message_start\n'
                 b'data: {"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","model":"claude-opus-4-8","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":0}}}\n\n'
                 b'event: content_block_start\n'
                 b'data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}\n\n'
                 b'event: content_block_delta\n'
                 b'data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}\n\n'
                 b'event: content_block_stop\n'
                 b'data: {"type":"content_block_stop","index":0}\n\n'
                 b'event: message_delta\n'
                 b'data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}\n\n'
                 b'event: message_stop\n'
                 b'data: {"type":"message_stop"}\n\n')


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

        return rec.get("body_json") or {}

    def _respond(self, body_json):
        payload = ANTHROPIC_SSE if body_json.get("stream") else ANTHROPIC_JSON
        content_type = "text/event-stream" if body_json.get("stream") else "application/json"
        self.send_response(200)
        self.send_header("content-type", content_type)
        self.send_header("content-length", str(len(payload)))
        self.end_headers()
        try:
            self.wfile.write(payload)
        except Exception:
            pass

    def do_POST(self):
        body_json = self._record()
        self._respond(body_json)

    do_GET = do_POST
    do_PUT = do_POST

    def log_message(self, *a):
        pass


if __name__ == "__main__":
    ThreadingHTTPServer(("127.0.0.1", PORT), H).serve_forever()
