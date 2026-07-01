#!/usr/bin/env python3
"""Mock Anthropic relay for capturing what Claude Code actually sends.

Point Claude Code at this server (ANTHROPIC_BASE_URL=http://127.0.0.1:8788) with any
non-empty token. It:
  - logs every request (method, path, headers in wire order, EXACT raw body as base64)
    to <outdir>/requests.jsonl in the shape analyze_billing.py understands, and
  - returns a VALID Anthropic response (SSE stream or JSON, plus count_tokens / models)
    so Claude Code completes its turn instead of erroring — letting us observe the full
    flow without any real credential or upstream.

Usage: python3 mock_relay.py [port] [outdir]
"""
import sys, os, json, base64, time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(sys.argv[1]) if len(sys.argv) > 1 else 8788
OUTDIR = sys.argv[2] if len(sys.argv) > 2 else os.path.join(os.path.dirname(__file__), "out_mock")
os.makedirs(OUTDIR, exist_ok=True)
REQS = os.path.join(OUTDIR, "requests.jsonl")


def log(rec):
    with open(REQS, "a") as f:
        f.write(json.dumps(rec) + "\n")


def sse(events):
    out = []
    for ev, data in events:
        out.append(f"event: {ev}\ndata: {json.dumps(data)}\n\n")
    return "".join(out).encode()


class H(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def _read_body(self):
        n = int(self.headers.get("content-length", "0") or "0")
        return self.rfile.read(n) if n else b""

    def _record(self, body):
        bj = None
        try:
            bj = json.loads(body) if body else None
        except Exception:
            bj = None
        log({
            "label": "anthropic",
            "request_line": f"{self.command} {self.path} {self.request_version}",
            "method": self.command,
            "path": self.path,
            "headers": [[k, v] for k, v in self.headers.items()],
            "body_json": bj,
            "body_text": None if bj else (body.decode("utf-8", "replace")[:4000] if body else None),
            "body_b64": base64.b64encode(body).decode("ascii") if body else None,
            "ts": time.time(),
        })
        return bj

    def _send(self, code, ctype, payload: bytes, extra=None):
        self.send_response(code)
        self.send_header("content-type", ctype)
        self.send_header("content-length", str(len(payload)))
        for k, v in (extra or {}).items():
            self.send_header(k, v)
        self.end_headers()
        self.wfile.write(payload)

    def _send_stream(self, payload: bytes):
        self.send_response(200)
        self.send_header("content-type", "text/event-stream")
        self.send_header("content-length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def do_GET(self):
        self._record(b"")
        if self.path.startswith("/v1/models"):
            self._send(200, "application/json", json.dumps({
                "data": [{"type": "model", "id": "claude-opus-4-8", "display_name": "Claude Opus 4.8"},
                         {"type": "model", "id": "claude-sonnet-4-6", "display_name": "Claude Sonnet 4.6"}],
                "has_more": False,
            }).encode())
            return
        self._send(200, "application/json", b"{}")

    def do_POST(self):
        body = self._read_body()
        bj = self._record(body)
        model = (bj or {}).get("model", "claude-opus-4-8")
        stream = bool((bj or {}).get("stream"))

        if self.path.startswith("/v1/messages/count_tokens"):
            self._send(200, "application/json", json.dumps({"input_tokens": 42}).encode())
            return

        if self.path.startswith("/v1/messages"):
            text = "hi"
            if stream:
                self._send_stream(sse([
                    ("message_start", {"type": "message_start", "message": {
                        "id": "msg_mock", "type": "message", "role": "assistant", "model": model,
                        "content": [], "stop_reason": None, "stop_sequence": None,
                        "usage": {"input_tokens": 10, "output_tokens": 1}}}),
                    ("content_block_start", {"type": "content_block_start", "index": 0,
                                             "content_block": {"type": "text", "text": ""}}),
                    ("content_block_delta", {"type": "content_block_delta", "index": 0,
                                             "delta": {"type": "text_delta", "text": text}}),
                    ("content_block_stop", {"type": "content_block_stop", "index": 0}),
                    ("message_delta", {"type": "message_delta",
                                       "delta": {"stop_reason": "end_turn", "stop_sequence": None},
                                       "usage": {"output_tokens": 1}}),
                    ("message_stop", {"type": "message_stop"}),
                ]))
            else:
                self._send(200, "application/json", json.dumps({
                    "id": "msg_mock", "type": "message", "role": "assistant", "model": model,
                    "content": [{"type": "text", "text": text}],
                    "stop_reason": "end_turn", "stop_sequence": None,
                    "usage": {"input_tokens": 10, "output_tokens": 1},
                }).encode())
            return
        self._send(200, "application/json", b"{}")

    def log_message(self, *a):
        pass


if __name__ == "__main__":
    open(REQS, "w").close()
    print(f"mock relay on 127.0.0.1:{PORT} -> {REQS}", flush=True)
    ThreadingHTTPServer(("127.0.0.1", PORT), H).serve_forever()
