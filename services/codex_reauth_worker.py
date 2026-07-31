#!/usr/bin/env python3
"""Lightweight Codex OAuth reauth worker.

POST /v1/codex/reauth with encrypted credentials supplied by the pool server
(already decrypted in memory) and return OAuth tokens. The worker is intentionally
stateless: it starts the browser/login subprocess only while a job is running.
"""

from __future__ import annotations

import argparse
import base64
import json
import os
import shlex
import subprocess
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any, Dict, Tuple


class WorkerError(Exception):
    def __init__(self, code: str, message: str, http_status: int = 502):
        super().__init__(message)
        self.code = code
        self.message = message
        self.http_status = http_status


def _b64decode_urlsafe(raw: str) -> bytes:
    raw = raw.strip()
    raw += "=" * (-len(raw) % 4)
    return base64.urlsafe_b64decode(raw.encode())


def id_token_claims(id_token: str) -> Dict[str, Any]:
    parts = (id_token or "").split(".")
    if len(parts) < 2:
        return {}
    try:
        return json.loads(_b64decode_urlsafe(parts[1]).decode())
    except Exception:
        return {}


def workspace_id_from_id_token(id_token: str) -> str:
    claims = id_token_claims(id_token)
    auth = claims.get("https://api.openai.com/auth") or {}
    if isinstance(auth, dict):
        return str(auth.get("chatgpt_account_id") or "").strip()
    return ""


def user_id_from_id_token(id_token: str) -> str:
    claims = id_token_claims(id_token)
    auth = claims.get("https://api.openai.com/auth") or {}
    if isinstance(auth, dict):
        return str(auth.get("chatgpt_user_id") or auth.get("user_id") or "").strip()
    return ""


def email_from_id_token(id_token: str) -> str:
    claims = id_token_claims(id_token)
    email = str(claims.get("email") or "").strip()
    profile = claims.get("https://api.openai.com/profile") or {}
    if not email and isinstance(profile, dict):
        email = str(profile.get("email") or "").strip()
    return email


def plan_from_id_token(id_token: str) -> str:
    claims = id_token_claims(id_token)
    auth = claims.get("https://api.openai.com/auth") or {}
    if not isinstance(auth, dict):
        return ""
    plan = auth.get("chatgpt_plan_type")
    if isinstance(plan, dict):
        return str(plan.get("Known") or plan.get("known") or plan.get("Unknown") or plan.get("unknown") or "").strip()
    return str(plan or "").strip()


def parse_login_output(output: str) -> Dict[str, Any]:
    marker = "__CODEX_ACCOUNT__"
    for line in output.splitlines():
        if marker not in line:
            continue
        raw = line.split(marker, 1)[1].strip()
        if not raw:
            continue
        try:
            parsed = json.loads(raw)
        except json.JSONDecodeError as exc:
            raise WorkerError("bad_login_output", f"login output marker was not JSON: {exc}") from exc
        if not isinstance(parsed, dict):
            raise WorkerError("bad_login_output", "login output marker was not a JSON object")
        return parsed
    raise classify_login_failure(output)


def classify_login_failure(output: str) -> WorkerError:
    hay = (output or "").lower()
    if "otp timeout" in hay or "cannot login" in hay and "otp" in hay:
        return WorkerError("otp_timeout", "OTP timed out while logging in", 408)
    if "password" in hay and ("wrong" in hay or "invalid" in hay or "incorrect" in hay or "failed" in hay):
        return WorkerError("password_error", "password login failed", 401)
    if "cloudflare" in hay or "just a moment" in hay:
        return WorkerError("cloudflare_blocked", "browser login was blocked by Cloudflare", 502)
    return WorkerError("login_failed", "login subprocess did not produce OAuth tokens", 502)


def validate_workspace(result: Dict[str, Any], target_workspace_id: str) -> None:
    target = (target_workspace_id or "").strip()
    if not target:
        return
    got = workspace_id_from_id_token(str(result.get("id_token") or ""))
    if not got:
        raise WorkerError("workspace_mismatch", f"id_token has no chatgpt_account_id; expected {target}", 409)
    if got != target:
        raise WorkerError("workspace_mismatch", f"id_token chatgpt_account_id {got} != target {target}", 409)


def normalize_result(result: Dict[str, Any]) -> Dict[str, Any]:
    access = str(result.get("access_token") or result.get("accessToken") or "").strip()
    refresh = str(result.get("refresh_token") or result.get("refreshToken") or "").strip()
    id_token = str(result.get("id_token") or result.get("idToken") or "").strip()
    if not access:
        raise WorkerError("missing_access_token", "login completed but returned no access_token", 502)
    workspace_id = workspace_id_from_id_token(id_token) or str(result.get("workspace_id") or result.get("account_id") or "").strip()
    user_id = user_id_from_id_token(id_token) or str(result.get("user_id") or "").strip()
    email = email_from_id_token(id_token) or str(result.get("email") or "").strip()
    plan = plan_from_id_token(id_token) or str(result.get("plan_type") or result.get("planType") or "").strip()
    return {
        "status": "succeeded",
        "access_token": access,
        "refresh_token": refresh,
        "id_token": id_token,
        "session_cookie": str(
            result.get("session_cookie")
            or result.get("cookie_header")
            or result.get("session_token")
            or ""
        ).strip(),
        "email": email,
        "user_id": user_id,
        "workspace_id": workspace_id,
        "plan_type": plan,
    }


def default_login_command() -> str:
    here = Path(__file__).resolve().parent
    return f"{shlex.quote(sys.executable)} {shlex.quote(str(here / 'codex_register' / 'login_oauth.py'))}"


def run_login(payload: Dict[str, Any]) -> Dict[str, Any]:
    cmd = os.environ.get("CODEX_REAUTH_LOGIN_COMMAND") or default_login_command()
    timeout = int(os.environ.get("CODEX_REAUTH_TIMEOUT_SECONDS") or "420")
    env = os.environ.copy()
    env.update({
        "REG_EMAIL": str(payload.get("email") or ""),
        "REG_PASSWORD": str(payload.get("password") or ""),
        "REG_OTP_URL": str(payload.get("otp_url") or ""),
        "REG_PROXY_SERVER": str(payload.get("proxy") or ""),
        "REG_COOKIE_HEADER": str(payload.get("cookie_header") or ""),
        "REG_HEROSMS_KEY": str(payload.get("hero_sms_key") or ""),
        "REG_SMS_COUNTRY": str(payload.get("sms_country") or "PH"),
        "REG_SMS_MIN_PRICE": str(payload.get("sms_min_price") or ""),
        "REG_SMS_MAX_PRICE": str(payload.get("sms_max_price") or ""),
        "REG_TARGET_WORKSPACE_ID": str(payload.get("target_workspace_id") or ""),
        "REG_HEADLESS": env.get("REG_HEADLESS", "1"),
    })
    started = time.time()
    proc = subprocess.run(
        shlex.split(cmd),
        env=env,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
        timeout=timeout,
        check=False,
    )
    output = (proc.stdout or "") + "\n" + (proc.stderr or "")
    if proc.returncode != 0:
        raise classify_login_failure(output)
    parsed = parse_login_output(output)
    parsed["duration_seconds"] = round(time.time() - started, 3)
    return parsed


def handle_reauth(payload: Dict[str, Any]) -> Dict[str, Any]:
    result = run_login(payload)
    validate_workspace(result, str(payload.get("target_workspace_id") or ""))
    normalized = normalize_result(result)
    validate_workspace(normalized, str(payload.get("target_workspace_id") or ""))
    return normalized


class ReauthHTTPServer(ThreadingHTTPServer):
    daemon_threads = True

    def __init__(self, server_address: Tuple[str, int], RequestHandlerClass, concurrency: int = 1):
        super().__init__(server_address, RequestHandlerClass)
        self.semaphore = threading.BoundedSemaphore(max(1, int(concurrency or 1)))


class ReauthHandler(BaseHTTPRequestHandler):
    server_version = "CodexReauthWorker/1.0"

    def log_message(self, fmt: str, *args: Any) -> None:  # keep stderr concise
        if os.environ.get("CODEX_REAUTH_DEBUG"):
            super().log_message(fmt, *args)

    def do_GET(self) -> None:
        if self.path == "/healthz":
            self.write_json(200, {"ready": True, "service": "codex-reauth-worker"})
            return
        self.write_json(404, {"status": "failed", "code": "not_found", "error": "not found"})

    def do_POST(self) -> None:
        if self.path != "/v1/codex/reauth":
            self.write_json(404, {"status": "failed", "code": "not_found", "error": "not found"})
            return
        if not self.server.semaphore.acquire(timeout=1):
            self.write_json(429, {"status": "failed", "code": "busy", "error": "worker is busy"})
            return
        try:
            payload = self.read_json()
            result = handle_reauth(payload)
            self.write_json(200, result)
        except WorkerError as exc:
            self.write_json(exc.http_status, {"status": "failed", "code": exc.code, "error": exc.message})
        except subprocess.TimeoutExpired:
            self.write_json(504, {"status": "failed", "code": "timeout", "error": "login subprocess timed out"})
        except Exception as exc:
            self.write_json(500, {"status": "failed", "code": "internal_error", "error": str(exc)})
        finally:
            self.server.semaphore.release()

    def read_json(self) -> Dict[str, Any]:
        n = int(self.headers.get("Content-Length") or "0")
        if n <= 0:
            return {}
        if n > 1 << 20:
            raise WorkerError("request_too_large", "request body too large", 413)
        raw = self.rfile.read(n)
        try:
            payload = json.loads(raw.decode())
        except json.JSONDecodeError as exc:
            raise WorkerError("bad_json", f"invalid JSON: {exc}", 400) from exc
        if not isinstance(payload, dict):
            raise WorkerError("bad_json", "request body must be a JSON object", 400)
        return payload

    def write_json(self, status: int, payload: Dict[str, Any]) -> None:
        raw = json.dumps(payload, ensure_ascii=False).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.send_header("Cache-Control", "no-store")
        self.send_header("X-Content-Type-Options", "nosniff")
        self.end_headers()
        self.wfile.write(raw)


def make_server(host: str = "127.0.0.1", port: int = 8802, concurrency: int = 1) -> ReauthHTTPServer:
    return ReauthHTTPServer((host, int(port)), ReauthHandler, concurrency=concurrency)


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description="Codex OAuth reauth HTTP worker")
    ap.add_argument("--host", default=os.environ.get("CODEX_REAUTH_HOST", "127.0.0.1"))
    ap.add_argument("--port", type=int, default=int(os.environ.get("CODEX_REAUTH_PORT", "8802")))
    ap.add_argument("--concurrency", type=int, default=int(os.environ.get("CODEX_REAUTH_CONCURRENCY", "1")))
    args = ap.parse_args(argv)
    httpd = make_server(args.host, args.port, args.concurrency)
    print(f"codex reauth worker listening on http://{args.host}:{args.port}", flush=True)
    try:
        httpd.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        httpd.server_close()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
