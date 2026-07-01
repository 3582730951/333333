"""Midtrans Snap X-Snap-Signature (HMAC-SHA256)."""
from __future__ import annotations

import base64
import hashlib
import hmac
import json
import os
import time
from typing import Any, Mapping

SIGNING_KEY = os.environ.get("MIDTRANS_SNAP_SIGNING_KEY", "")
ROOT_PATH = "/snap"
DEFAULT_MIDTRANS_CLIENT_KEY = os.environ.get("MIDTRANS_CLIENT_KEY", "")


def _require_runtime_value(name: str, value: str) -> str:
    if not value or value.startswith("CHANGE_ME"):
        raise RuntimeError(f"missing required runtime config: {name}")
    return value


def permute_signature(signature: str) -> str:
    """Snap JS `ta` interceptor permutes hex signature in 4-char blocks."""
    chars = list(signature)
    i = 0
    while i + 3 < len(chars):
        chars[i], chars[i + 1], chars[i + 2], chars[i + 3] = (
            chars[i + 2],
            chars[i + 3],
            chars[i],
            chars[i + 1],
        )
        i += 4
    return "".join(chars)


def _body_string(body: Any) -> str:
    if body is None or body == "":
        return ""
    if isinstance(body, (dict, list)):
        return json.dumps(body, separators=(",", ":"), ensure_ascii=False)
    return str(body)


def sign_snap_request(path: str, body: Any = None, *, timestamp: int | None = None) -> dict[str, str]:
    """Return X-Snap-Signature and X-Timestamp headers for a Snap API call."""
    if not path.startswith("/snap"):
        path = f"{ROOT_PATH}{path}" if path.startswith("/") else f"{ROOT_PATH}/{path}"
    ts = str(int(time.time()) if timestamp is None else timestamp)
    msg = f"{path}:{ts}:{_body_string(body)}"
    signing_key = _require_runtime_value("MIDTRANS_SNAP_SIGNING_KEY", SIGNING_KEY)
    raw_sig = hmac.new(signing_key.encode(), msg.encode(), hashlib.sha256).hexdigest()
    sig = permute_signature(raw_sig)
    return {"X-Snap-Signature": sig, "X-Timestamp": ts}


def snap_source_headers() -> dict[str, str]:
    return {
        "X-Source": "snap",
        "X-Source-App-Type": "redirection",
        "X-Source-Version": "2.3.0",
    }


def midtrans_basic_auth(client_key: str = DEFAULT_MIDTRANS_CLIENT_KEY) -> dict[str, str]:
    client_key = _require_runtime_value("MIDTRANS_CLIENT_KEY", client_key or DEFAULT_MIDTRANS_CLIENT_KEY)
    token = base64.b64encode(f"{client_key}:".encode("ascii")).decode("ascii")
    return {"Authorization": f"Basic {token}"}


def midtrans_snap_headers(
    path: str,
    body: Any = None,
    *,
    snap_token: str | None = None,
    with_source: bool = True,
    with_auth: bool = False,
    client_key: str = DEFAULT_MIDTRANS_CLIENT_KEY,
) -> Mapping[str, str]:
    headers = dict(sign_snap_request(path, body))
    if with_source:
        headers.update(snap_source_headers())
    if snap_token:
        headers["Referer"] = f"https://app.midtrans.com/snap/v4/redirection/{snap_token}"
    if with_auth:
        headers.update(midtrans_basic_auth(client_key))
        headers["Origin"] = "https://app.midtrans.com"
    return headers


def snap_json_body(body: Any) -> str:
    return json.dumps(body, separators=(",", ":"), ensure_ascii=False)
