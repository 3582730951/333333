#!/usr/bin/env python3
import base64
import hashlib
import http.server
import json
import os
import queue
import re
import signal
import socketserver
import sys
import tempfile
import threading
import time
from http import HTTPStatus
from typing import Dict, Any

from curl_cffi import requests


COOKIE_DIR = os.environ.get("CODEX_POOL_SIDECAR_COOKIE_DIR", os.path.join(tempfile.gettempdir(), "codex-pool-sidecar-cookies"))
IMPERSONATE = os.environ.get("CODEX_POOL_SIDECAR_IMPERSONATE", "chrome120")
TIMEOUT = int(os.environ.get("CODEX_POOL_SIDECAR_TIMEOUT", "120"))
# On SIGTERM/SIGINT (e.g. an update restart) the sidecar stops accepting new requests
# and waits up to this many seconds for in-flight proxied streams to finish before
# exiting, so a redeploy does not sever active upstream streams. Keep the sidecar
# unit's TimeoutStopSec >= this value.
DRAIN_SECONDS = int(os.environ.get("CODEX_POOL_SIDECAR_DRAIN_SECONDS", "20"))

# Count of in-flight /proxy requests, so the graceful-drain loop knows when the
# active streams have finished.
_inflight_lock = threading.Lock()
_inflight = 0


def _inflight_inc() -> None:
    global _inflight
    with _inflight_lock:
        _inflight += 1


def _inflight_dec() -> None:
    global _inflight
    with _inflight_lock:
        _inflight -= 1


def _inflight_count() -> int:
    with _inflight_lock:
        return _inflight
# Content-Encoding we ask the upstream for AND that we rely on libcurl to decode
# transparently. We deliberately do NOT inherit the impersonation default
# ("gzip, deflate, br, zstd"): in streaming mode (stream=True / iter_content)
# curl_cffi converts libcurl's write callback into an iterator, and libcurl only
# auto-decompresses encodings its build actually links (zstd in particular, and
# sometimes br, is frequently absent). When the upstream then answers br/zstd we
# would forward STILL-COMPRESSED bytes while reporting them as plaintext (we strip
# content-encoding below), and the downstream — Claude Code — sees a 200 whose
# body is unreadable: "Failed to parse JSON" / "empty or malformed response".
# gzip+deflate are backed by zlib, which is universally compiled into libcurl, so
# decoding is reliable. Setting it via the dedicated accept_encoding option (not a
# raw header) is what arms libcurl's CURLOPT_ACCEPT_ENCODING decoder. Accept-Encoding
# is not part of the TLS/JA3 fingerprint, and the real Claude Code transport is
# Node/undici (not Chrome) anyway, so narrowing it costs no mimicry. Operators whose
# libcurl is built with brotli/zstd can widen it back via the env var.
ACCEPT_ENCODING = os.environ.get("CODEX_POOL_SIDECAR_ACCEPT_ENCODING", "gzip, deflate")
# Max idle curl_cffi Sessions retained PER fingerprint+account bucket. Each pooled
# Session keeps its upstream TCP+TLS+HTTP2 connection warm (see SessionPool below),
# so the dominant per-request cost — a fresh DNS+TLS handshake to the upstream — is
# paid once per bucket instead of on every call. 0 disables pooling (a fresh Session
# per request, the pre-pool behavior).
POOL_MAX_IDLE = int(os.environ.get("CODEX_POOL_SIDECAR_POOL_MAX_IDLE", "8"))


def safe_key(value: str) -> str:
    digest = hashlib.sha256(value.encode("utf-8")).hexdigest()
    return digest


def load_cookie_dict(key: str) -> Dict[str, str]:
    os.makedirs(COOKIE_DIR, exist_ok=True)
    path = os.path.join(COOKIE_DIR, safe_key(key) + ".json")
    try:
        with open(path, "r", encoding="utf-8") as f:
            data = json.load(f)
        if isinstance(data, dict):
            return {str(k): str(v) for k, v in data.items()}
    except FileNotFoundError:
        return {}
    except Exception:
        return {}
    return {}


# Background queue + single worker thread for non-blocking cookie persistence.
# Writing cookies to disk synchronously inside relay_response adds ~0.5-2 ms of
# I/O latency to every proxied request (fsync via os.replace on a temp file).
# Instead, the handler pushes (key, cookies) dicts onto this queue and returns
# immediately; a dedicated daemon thread drains the queue and writes to disk
# asynchronously, so response forwarding never waits on filesystem I/O.
_cookie_queue: "queue.Queue" = queue.Queue()
_cookie_dirty: Dict[str, Dict[str, str]] = {}
_cookie_dirty_lock = threading.Lock()

def _cookie_writer() -> None:
    """Drain the cookie queue, merging updates and writing to disk in batches."""
    while True:
        try:
            key, cookies = _cookie_queue.get()
            if key is None:  # sentinel for shutdown
                break
            # Merge queued updates for the same key: coalesce successive writes
            # into a single disk flush instead of N separate os.replace calls.
            with _cookie_dirty_lock:
                _cookie_dirty[key] = cookies
            # Drain any additional queued items without blocking (batch write).
            drained = True
            while drained:
                try:
                    nk, nc = _cookie_queue.get_nowait()
                    if nk is None:
                        break
                    with _cookie_dirty_lock:
                        _cookie_dirty[nk] = nc
                except queue.Empty:
                    drained = False
            # Flush all dirty entries to disk.
            with _cookie_dirty_lock:
                to_write = dict(_cookie_dirty)
                _cookie_dirty.clear()
            for wk, wc in to_write.items():
                os.makedirs(COOKIE_DIR, exist_ok=True)
                path = os.path.join(COOKIE_DIR, safe_key(wk) + ".json")
                tmp = path + ".tmp"
                try:
                    with open(tmp, "w", encoding="utf-8") as f:
                        json.dump(wc, f, sort_keys=True)
                    os.replace(tmp, path)
                except Exception:
                    pass
        except Exception:
            pass

_cookie_thread = threading.Thread(target=_cookie_writer, daemon=True)
_cookie_thread.start()

def save_cookie_dict(key: str, cookies: Dict[str, str]) -> None:
    """Enqueue cookie data for async persistence; returns immediately."""
    _cookie_queue.put((key, cookies))


# Fingerprints (ja3/akamai) the local curl_cffi/BoringSSL build has already proven it
# cannot replay. The first request that hits an ImpersonateError/NotImplementedError for
# a fingerprint records it here so every subsequent request skips the doomed attempt and
# goes straight to native impersonation — no per-request wasted handshake setup.
_UNREPLAYABLE_FP: set = set()
_UNREPLAYABLE_LOCK = threading.Lock()


def fingerprint_known_unreplayable(impersonate: str, ja3: str, akamai: str) -> bool:
    with _UNREPLAYABLE_LOCK:
        return (impersonate, ja3, akamai) in _UNREPLAYABLE_FP


def remember_fingerprint_unreplayable(impersonate: str, ja3: str, akamai: str) -> None:
    with _UNREPLAYABLE_LOCK:
        _UNREPLAYABLE_FP.add((impersonate, ja3, akamai))


def is_impersonation_error(exc: BaseException) -> bool:
    """True when an exception is curl_cffi/BoringSSL refusing to REPLAY a requested
    ja3/akamai fingerprint (a client-hello construction failure), as opposed to a real
    network/upstream failure. Only the former is safe to retry without the fingerprint —
    a connection error must still surface as a 502 so the operator sees the true cause.

    Matched conservatively: an explicit NotImplementedError, or a message that names the
    fingerprint machinery (cipher/ja3/impersonate/extension/akamai) together with a
    can't-do qualifier. libcurl connection errors ("Failed to connect", "Could not
    resolve host", "curl: (7)", timeouts) contain none of these tokens, so they re-raise.
    """
    if isinstance(exc, NotImplementedError):
        return True
    msg = str(exc).lower()
    markers = ("cipher", "ja3", "impersonate", "extension", "akamai", "clienthello")
    qualifiers = ("not found", "not support", "unsupported", "not implement", "invalid", "unknown")
    return any(m in msg for m in markers) and any(q in msg for q in qualifiers)


def clean_headers(headers: Dict[str, Any]) -> Dict[str, str]:
    out: Dict[str, str] = {}
    for key, value in headers.items():
        lower = key.lower()
        if lower in {"host", "content-length", "connection", "accept-encoding"}:
            continue
        if isinstance(value, list):
            if value:
                out[key] = str(value[-1])
        else:
            out[key] = str(value)
    return out


# Bound on the number of distinct pool keys to prevent unbounded memory growth when
# (account, egress, host) combinations proliferate (e.g. many accounts × many egresses).
# When the bound is hit we stop pooling new keys and close the session instead —
# no cross-account cookie contamination is possible because we only ever close, never
# hand out an existing pooled session under a different key.
POOL_MAX_KEYS = int(os.environ.get("CODEX_POOL_SIDECAR_POOL_MAX_KEYS", "4096"))


class SessionPool:
    """Reuses curl_cffi Sessions (and their warm upstream connections) across
    requests, keyed by the fingerprint axes that determine the TLS/HTTP2 client
    hello plus the cookie jar — so a connection is only ever reused for an
    identical fingerprint AND the same account (no cookie cross-talk).

    A Session is checked out for the lifetime of one request (including the SSE
    stream) and returned only after the body is fully consumed, so it is never
    used by two threads at once — concurrent requests on the same bucket simply
    get distinct Sessions, which return to the idle pool afterward. A request
    that errors mid-stream discards its Session (the half-read upstream
    connection is not safe to reuse) instead of returning it.

    libcurl detects and re-dials a connection the upstream has since dropped, so
    a pooled Session whose keep-alive expired transparently reconnects on next
    use; no explicit idle expiry is needed here.

    CRITICAL FIX: When JA3 fingerprint is used, we ALWAYS include the cookie_jar_key
    in the pool key to guarantee per-account cookie isolation. Previously, the key
    was correctly designed but the fallback path could acquire a session from the
    wrong bucket. Now we ensure strict isolation by using the full key tuple and
    never sharing sessions across different cookie_jar_keys even with the same JA3.
    """

    def __init__(self, max_idle: int, max_keys: int = 4096) -> None:
        self._max_idle = max_idle
        self._max_keys = max_keys
        self._lock = threading.Lock()
        self._idle: Dict[Any, list] = {}
        self._key_count = 0

    def acquire(self, key: Any) -> "requests.Session":
        if self._max_idle > 0:
            with self._lock:
                bucket = self._idle.get(key)
                if bucket:
                    return bucket.pop()
        return requests.Session(impersonate=IMPERSONATE)

    def release(self, key: Any, session: "requests.Session", healthy: bool) -> None:
        if session is None:
            return
        if healthy and self._max_idle > 0:
            with self._lock:
                # Check if we've hit the key count limit. When at capacity we close
                # instead of pooling to avoid unbounded memory growth. Closing is safe:
                # we never hand out a session under a different key, so no cross-account
                # cookie contamination is possible.
                if self._key_count >= self._max_keys and key not in self._idle:
                    try:
                        session.close()
                    except Exception:
                        pass
                    return
                bucket = self._idle.setdefault(key, [])
                if len(bucket) < self._max_idle:
                    bucket.append(session)
                    # Track new key creation
                    if len(bucket) == 1:
                        self._key_count += 1
                    return
        try:
            session.close()
        except Exception:
            pass

    def close_all(self) -> None:
        """Close all pooled sessions. Call on shutdown to release resources."""
        with self._lock:
            for bucket in self._idle.values():
                for session in bucket:
                    try:
                        session.close()
                    except Exception:
                        pass
            self._idle.clear()
            self._key_count = 0


SESSION_POOL = SessionPool(POOL_MAX_IDLE, POOL_MAX_KEYS)


class Handler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt: str, *args: Any) -> None:
        sys.stderr.write("%s - - [%s] %s\n" % (self.client_address[0], self.log_date_time_string(), fmt % args))

    def do_GET(self) -> None:
        if self.path == "/healthz":
            self.send_json(HTTPStatus.OK, {"ok": True, "impersonate": IMPERSONATE})
            return
        self.send_json(HTTPStatus.NOT_FOUND, {"error": "not found"})

    def do_POST(self) -> None:
        if self.path == "/cookies":
            length = int(self.headers.get("content-length", "0"))
            try:
                payload = json.loads(self.rfile.read(length))
                self.seed_cookies(payload)
            except Exception as exc:
                self.send_json(HTTPStatus.BAD_GATEWAY, {"error": str(exc)})
            return
        if self.path != "/proxy":
            self.send_json(HTTPStatus.NOT_FOUND, {"error": "not found"})
            return
        length = int(self.headers.get("content-length", "0"))
        _inflight_inc()
        try:
            raw = self.rfile.read(length)
            meta_b64 = self.headers.get("x-sidecar-meta")
            if meta_b64:
                # New protocol: routing metadata rides the X-Sidecar-Meta header and the
                # request body IS the raw HTTP body — no base64 round-trip on a large
                # (1M-context) payload.
                payload = json.loads(base64.b64decode(meta_b64))
                body = raw
            else:
                # Legacy protocol: one JSON object carrying the base64'd body. Kept so a
                # newer sidecar still serves an older relay during a rolling deploy.
                payload = json.loads(raw)
                body_b64 = str(payload.get("body_b64") or "")
                body = base64.b64decode(body_b64) if body_b64 else b""
            self.proxy(payload, body)
        except Exception as exc:
            self.send_json(HTTPStatus.BAD_GATEWAY, {"error": str(exc)})
        finally:
            _inflight_dec()

    def seed_cookies(self, payload: Dict[str, Any]) -> None:
        """Merge an operator/FlareSolverr-supplied cookie set into the on-disk store
        for a cookie_jar_key, so a subsequently /proxy'd request under the same key
        carries it (e.g. an injected cf_clearance). Merge — never replace — so cookies
        the sidecar accumulated from upstream Set-Cookie are preserved."""
        cookie_jar_key = str(payload.get("cookie_jar_key") or "default")
        incoming = payload.get("cookies") or {}
        if not isinstance(incoming, dict):
            self.send_json(HTTPStatus.BAD_REQUEST, {"error": "cookies must be an object"})
            return
        merged = load_cookie_dict(cookie_jar_key)
        for k, v in incoming.items():
            merged[str(k)] = str(v)
        save_cookie_dict(cookie_jar_key, merged)
        self.send_json(HTTPStatus.OK, {"ok": True, "count": len(merged)})

    def proxy(self, payload: Dict[str, Any], body: bytes) -> None:
        method = str(payload.get("method") or "POST").upper()
        url = str(payload["url"])
        headers = clean_headers(payload.get("headers") or {})
        cookie_jar_key = str(payload.get("cookie_jar_key") or "default")
        cookies = load_cookie_dict(cookie_jar_key)
        # Optional caller-supplied TLS fingerprint. When the relay wants the upstream
        # to see a SPECIFIC ClientHello (e.g. the real Codex/Rust binary's JA3 rather
        # than the chrome impersonation default), it passes a ja3 string. curl_cffi
        # replays it on top of the base impersonation. We then disable default_headers
        # so the impersonation profile does not inject browser-default headers that
        # would contradict the exact (Codex) header set the relay already built — the
        # caller is fully responsible for the header set in that mode.
        ja3 = str(payload.get("ja3") or "").strip()
        akamai = str(payload.get("akamai") or "").strip()
        # Optional upstream proxy (e.g. a WARP exit's local SOCKS5). When set, the
        # impersonated request is routed THROUGH it, so the upstream sees the real
        # JA3 fingerprint AND a different (clean) exit IP — the JA3+IP combination a
        # CF-blocked account needs. "socks5h://" resolves DNS at the proxy.
        proxy = str(payload.get("proxy") or "").strip()

        # Base request kwargs WITHOUT any custom fingerprint — the proven native Chrome
        # impersonation path, and the graceful-degradation target when a requested
        # fingerprint can't be replayed (see below).
        base_kwargs: Dict[str, Any] = dict(
            headers=headers,
            data=body if body else None,
            cookies=cookies,
            timeout=TIMEOUT,
            stream=True,
            # Force a libcurl-decodable encoding so the streamed bytes are truly
            # plaintext (see ACCEPT_ENCODING note above). Passing it as the
            # dedicated option — not a header — enables CURLOPT_ACCEPT_ENCODING
            # decoding rather than just advertising the value.
            accept_encoding=ACCEPT_ENCODING,
        )
        # allow_redirects defaults True (the relay relies on the sidecar following an
        # upstream 3xx transparently). The registration flow sets it False so the Go
        # client follows redirects itself with a domain-scoped cookie jar — the sidecar's
        # flat name→value jar collides cookies across the chatgpt.com / auth.openai.com /
        # sentinel.openai.com hops of an OAuth signup, which breaks the auth "state".
        if payload.get("allow_redirects") is False:
            base_kwargs["allow_redirects"] = False
        if proxy:
            # requests-style mapping; curl_cffi (libcurl) honors socks5h/http(s)
            # proxy URLs and applies it per request over the pooled Session.
            base_kwargs["proxies"] = {"http": proxy, "https": proxy}

        # Don't even attempt a fingerprint this build has already proven it can't replay
        # (avoids paying the failed-setup cost on every request once we've learned it).
        want_fp = bool(ja3 or akamai)
        if want_fp and fingerprint_known_unreplayable(IMPERSONATE, ja3, akamai):
            want_fp = False

        # Check a warm Session out of the pool for this exact fingerprint+account bucket
        # so its upstream connection is reused; it is returned only after the body below
        # is fully streamed (see SessionPool). The key MUST include every axis that shaped
        # the live connection — impersonation profile, ja3, akamai, the upstream proxy —
        # plus the cookie jar, so a connection is never reused for a different ClientHello,
        # a different exit IP, or a different account. When we are NOT applying a
        # fingerprint, the ja3/akamai axes are empty so we share the native-impersonation
        # bucket (and never pool a fingerprinted connection under it).
        used_key = (IMPERSONATE, ja3 if want_fp else "", akamai if want_fp else "", proxy, cookie_jar_key)
        session = SESSION_POOL.acquire(used_key)
        healthy = False
        try:
            kwargs = dict(base_kwargs)
            if want_fp:
                if ja3:
                    kwargs["ja3"] = ja3
                    kwargs["default_headers"] = False
                if akamai:
                    kwargs["akamai"] = akamai
            try:
                response = session.request(method, url, **kwargs)
            except Exception as exc:
                if not (want_fp and is_impersonation_error(exc)):
                    raise
                # The local curl_cffi/BoringSSL build cannot replay this fingerprint
                # (e.g. rustls' 0xFF SCSV pseudo-cipher in the real Codex JA3). The real
                # Codex client does NO JA3 spoofing — it uses vanilla reqwest 0.12 +
                # rustls 0.23, verified against the Codex source — and chatgpt.com sits
                # behind Cloudflare, which whitelists the Chrome JA3 curl_cffi reproduces
                # natively; the OAuth token is the real auth. So the fingerprint carries
                # no detection value, and degrading to native impersonation is strictly
                # better than failing the request with a 502. This is the graceful
                # fallback the relay's resolveCodexJA3/postViaSidecar documents.
                remember_fingerprint_unreplayable(IMPERSONATE, ja3, akamai)
                sys.stderr.write(
                    "[sidecar] fingerprint replay unsupported (%s); falling back to native impersonation\n" % exc
                )
                SESSION_POOL.release(used_key, session, False)  # discard the tainted session
                used_key = (IMPERSONATE, "", "", proxy, cookie_jar_key)
                session = SESSION_POOL.acquire(used_key)
                response = session.request(method, url, **base_kwargs)
            healthy = self.relay_response(response, cookies, cookie_jar_key)
        finally:
            SESSION_POOL.release(used_key, session, healthy)

    def relay_response(self, response: Any, cookies: Dict[str, str], cookie_jar_key: str) -> bool:
        """Stream an upstream curl_cffi response back to the caller verbatim. Returns
        True only when the body drained cleanly (so the pooled connection is safe to
        reuse). The upstream status/headers are carried in x-sidecar-* headers; the
        body bytes are plaintext (libcurl auto-decompressed them — see ACCEPT_ENCODING)."""
        merged = dict(cookies)
        try:
            merged.update(response.cookies.get_dict())
            save_cookie_dict(cookie_jar_key, merged)
        except Exception:
            pass

        # curl_cffi (libcurl) auto-decompresses the body transparently because we
        # request a build-supported encoding via the accept_encoding option above
        # (gzip/deflate, backed by zlib). The bytes we stream below are therefore
        # PLAINTEXT. We must not advertise the upstream's content-encoding/length to
        # the caller, or it would try to decompress already-decompressed bytes (and
        # the length would be wrong). Strip them from the reported upstream headers.
        reported = {}
        for k, v in response.headers.items():
            if k.lower() in {"content-encoding", "content-length", "transfer-encoding"}:
                continue
            reported[k] = [v]
        encoded_headers = base64.b64encode(json.dumps(reported).encode("utf-8")).decode("ascii")
        self.close_connection = True
        self.send_response(HTTPStatus.OK)
        self.send_header("x-sidecar-upstream-status", str(response.status_code))
        self.send_header("x-sidecar-upstream-headers-b64", encoded_headers)
        self.send_header("content-type", response.headers.get("content-type", "application/octet-stream"))
        self.send_header("connection", "close")
        self.end_headers()
        for chunk in response.iter_content(chunk_size=65536):
            if chunk:
                self.wfile.write(chunk)
                self.wfile.flush()
        # Reached only when the upstream body drained cleanly: the connection is in
        # a known-good state and safe to return to the pool for reuse.
        return True

    def send_json(self, status: int, value: Dict[str, Any]) -> None:
        raw = json.dumps(value).encode("utf-8")
        self.send_response(status)
        self.send_header("content-type", "application/json")
        self.send_header("content-length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)


class ThreadingServer(socketserver.ThreadingMixIn, http.server.HTTPServer):
    daemon_threads = True


def run(addr: str) -> None:
    host, port_s = addr.rsplit(":", 1)
    server = ThreadingServer((host, int(port_s)), Handler)
    print(f"curl_cffi sidecar listening on {addr}", flush=True)

    # Graceful drain on SIGTERM/SIGINT (an update restart): stop accepting new
    # connections, then let in-flight proxied streams finish (up to DRAIN_SECONDS)
    # before exiting, so a redeploy does not sever active upstream streams.
    stopping = threading.Event()

    def _graceful(signum, _frame):
        if stopping.is_set():
            return
        stopping.set()
        print(f"sidecar received signal {signum}; draining up to {DRAIN_SECONDS}s", flush=True)
        # server.shutdown() blocks until serve_forever() returns and must run off the
        # serve_forever thread.
        threading.Thread(target=server.shutdown, daemon=True).start()

    signal.signal(signal.SIGTERM, _graceful)
    signal.signal(signal.SIGINT, _graceful)

    server.serve_forever()

    # No longer accepting new requests; wait for in-flight streams to complete.
    deadline = time.monotonic() + DRAIN_SECONDS
    while _inflight_count() > 0 and time.monotonic() < deadline:
        time.sleep(0.1)
    remaining = _inflight_count()
    if remaining > 0:
        print(f"sidecar drain deadline reached with {remaining} stream(s) still active; exiting", flush=True)

    # Close all pooled sessions to release resources (TCP connections, memory).
    SESSION_POOL.close_all()

    server.server_close()


def selftest() -> None:
    class Upstream(http.server.BaseHTTPRequestHandler):
        def do_POST(self) -> None:
            raw = self.rfile.read(int(self.headers.get("content-length", "0")))
            response = b'{"ok":true,"body_len":' + str(len(raw)).encode() + b"}"
            self.send_response(200)
            self.send_header("content-type", "application/json")
            self.send_header("set-cookie", "cf_clearance=test-cookie")
            self.send_header("content-length", str(len(response)))
            self.end_headers()
            self.wfile.write(response)

        def log_message(self, fmt: str, *args: Any) -> None:
            return

    upstream = ThreadingServer(("127.0.0.1", 0), Upstream)
    upstream_thread = threading.Thread(target=upstream.serve_forever, daemon=True)
    upstream_thread.start()

    sidecar = ThreadingServer(("127.0.0.1", 0), Handler)
    sidecar_thread = threading.Thread(target=sidecar.serve_forever, daemon=True)
    sidecar_thread.start()

    target = f"http://127.0.0.1:{upstream.server_address[1]}/test"
    proxy_url = f"http://127.0.0.1:{sidecar.server_address[1]}/proxy"
    payload = {
        "method": "POST",
        "url": target,
        "headers": {"content-type": ["application/json"]},
        "body_b64": base64.b64encode(b'{"hello":"world"}').decode("ascii"),
        "cookie_jar_key": "acc:test:127.0.0.1",
        "stream": True,
    }
    response = requests.post(proxy_url, json=payload, timeout=10)
    assert response.status_code == 200, response.status_code
    assert response.headers.get("x-sidecar-upstream-status") == "200", response.headers
    assert response.json()["ok"] is True, response.text
    assert re.match(r"^[a-f0-9]{64}$", safe_key(payload["cookie_jar_key"]))

    # New protocol: routing metadata in the X-Sidecar-Meta header, request body sent as
    # the raw HTTP body (no base64). The upstream echoes back body_len, so a correct
    # body_len proves the raw bytes reached the upstream intact through the sidecar.
    new_body = b'{"hello":"raw-protocol"}'
    meta = {
        "method": "POST",
        "url": target,
        "headers": {"content-type": ["application/json"]},
        "cookie_jar_key": payload["cookie_jar_key"],
        "stream": True,
    }
    new_resp = requests.post(
        proxy_url,
        data=new_body,
        headers={
            "X-Sidecar-Meta": base64.b64encode(json.dumps(meta).encode("utf-8")).decode("ascii"),
            "content-type": "application/octet-stream",
        },
        timeout=10,
    )
    assert new_resp.status_code == 200, new_resp.status_code
    assert new_resp.headers.get("x-sidecar-upstream-status") == "200", new_resp.headers
    assert new_resp.json()["body_len"] == len(new_body), new_resp.text

    # /cookies seeding: an injected cookie must land in the store under the same key
    # and merge with whatever the proxy call already saved.
    cookies_url = f"http://127.0.0.1:{sidecar.server_address[1]}/cookies"
    seed = requests.post(cookies_url, json={"cookie_jar_key": payload["cookie_jar_key"], "cookies": {"cf_clearance": "injected-xyz"}}, timeout=10)
    assert seed.status_code == 200, seed.status_code
    stored = load_cookie_dict(payload["cookie_jar_key"])
    assert stored.get("cf_clearance") == "injected-xyz", stored

    sidecar.shutdown()
    upstream.shutdown()

    # is_impersonation_error: only fingerprint-replay failures are retryable; real
    # network errors must propagate (so the operator sees the true 502 cause).
    assert is_impersonation_error(NotImplementedError("TLS extension not supported"))
    assert is_impersonation_error(Exception("Cipher 0xff is not found"))
    assert is_impersonation_error(Exception("Impersonate ja3 string invalid"))
    assert not is_impersonation_error(Exception("Failed to connect to chatgpt.com port 443"))
    assert not is_impersonation_error(Exception("curl: (7) Couldn't connect to server"))
    assert not is_impersonation_error(Exception("Could not resolve host"))

    # unreplayable-fingerprint cache round-trips per (impersonate, ja3, akamai) bucket.
    assert not fingerprint_known_unreplayable("chrome120", "bad-ja3", "")
    remember_fingerprint_unreplayable("chrome120", "bad-ja3", "")
    assert fingerprint_known_unreplayable("chrome120", "bad-ja3", "")
    assert not fingerprint_known_unreplayable("chrome120", "other-ja3", "")

    print("All tests passed")


if __name__ == "__main__":
    if "--selftest" in sys.argv:
        selftest()
    else:
        listen = os.environ.get("CODEX_POOL_SIDECAR_ADDR", "127.0.0.1:8790")
        run(listen)
