#!/usr/bin/env python3
"""mitmproxy addon: record the real wire fingerprint of clients that terminate TLS
themselves and ignore SSLKEYLOGFILE / NODE_OPTIONS — notably the Rust `codex_cli_rs`
binary, which uses a WebSocket (wss://api.openai.com/v1/responses) that the plain
HTTP recorder cannot accept.

mitmproxy terminates TLS with a CA the clients trust, so we see the decrypted HTTP
requests AND the WebSocket upgrade + frames. (The client→mitmproxy TLS hop is still
the client's REAL ClientHello, so a passive tcpdump on that hop yields the genuine
JA3 — captured separately by capture.sh.)

Records are appended to $CAP_OUT as JSONL, in a shape parse.py understands:
  - HTTP request:   {label, kind:"http",          request_line, method, path, headers[], body_json|body_text}
  - WS handshake:   {label, kind:"ws_handshake",   request_line, method, path, headers[]}
  - WS frame:       {label, kind:"ws_msg",         path, direction:"send"|"recv", opcode, text}
"""
import json
import os
import base64

OUT = os.environ.get("CAP_OUT", "/cap/out/requests.jsonl")
MAX_FRAMES_PER_FLOW = int(os.environ.get("CAP_MAX_FRAMES", "12"))
_frame_counts = {}


def _label(host: str) -> str:
    h = (host or "").lower()
    if "anthropic" in h:
        return "anthropic"
    if "openai" in h:
        return "openai"
    if "chatgpt" in h:
        return "chatgpt"
    return host or "unknown"


def _headers(req) -> list:
    # req.headers.fields preserves on-wire order and casing as (bytes, bytes).
    out = []
    for k, v in req.headers.fields:
        out.append([k.decode("latin-1"), v.decode("latin-1")])
    return out


def _append(rec: dict):
    with open(OUT, "a") as f:
        f.write(json.dumps(rec) + "\n")


def _is_ws_upgrade(req) -> bool:
    return "websocket" in req.headers.get("upgrade", "").lower()


def request(flow):
    req = flow.request
    rec = {
        "label": _label(req.pretty_host),
        "host": req.pretty_host,
        "request_line": f"{req.method} {req.path} HTTP/{req.http_version.split('/')[-1] if '/' in req.http_version else req.http_version}",
        "method": req.method,
        "path": req.path,
        "headers": _headers(req),
    }
    if _is_ws_upgrade(req):
        rec["kind"] = "ws_handshake"
        _append(rec)
        return
    rec["kind"] = "http"
    body = req.get_text(strict=False) or ""
    bj = None
    if body:
        try:
            bj = json.loads(body)
        except Exception:
            bj = None
    rec["body_json"] = bj
    rec["body_text"] = None if bj else (body[:4000] if body else None)
    # EXACT decoded request bytes (untruncated), so analyze_billing.py can recompute
    # content hashes like the per-request cch byte-for-byte — body_json/body_text lose
    # the original key order / whitespace and are truncated, which defeats hashing.
    try:
        raw = req.content  # transfer/content-decoded, i.e. what the client serialized
        if raw:
            rec["body_b64"] = base64.b64encode(raw).decode("ascii")
    except Exception:
        pass
    _append(rec)


def websocket_message(flow):
    # flow.websocket.messages[-1] is the frame that just arrived.
    msgs = getattr(flow.websocket, "messages", None)
    if not msgs:
        return
    fid = id(flow)
    n = _frame_counts.get(fid, 0)
    if n >= MAX_FRAMES_PER_FLOW:
        return
    _frame_counts[fid] = n + 1
    m = msgs[-1]
    try:
        text = m.text if hasattr(m, "text") else m.content.decode("utf-8", "replace")
    except Exception:
        text = m.content.decode("utf-8", "replace") if getattr(m, "content", None) else ""
    _append({
        "label": _label(flow.request.pretty_host),
        "host": flow.request.pretty_host,
        "kind": "ws_msg",
        "path": flow.request.path,
        "direction": "send" if m.from_client else "recv",
        "opcode": int(getattr(m, "type", 0)) if not isinstance(getattr(m, "type", 0), int) else getattr(m, "type", 0),
        "text": text[:4000],
    })
