"""
GoPay Pure-Protocol Worker — registration + payment parallel pipeline.

Self-contained deployment version — all imports are local (no C:\\tools dependency).

Each worker thread loops independently:
  1. Register GoPay account (rent phone → signup → refresh → PIN)
  2. Push account to inbox, wait for balance > 0
  3. Claim inbox job → pure-protocol Midtrans payment
  4. Done or failed → loop back to step 1
"""
from __future__ import annotations

import base64
import json
import logging
import os
import random
import re
import string
import threading
import time
import uuid
import urllib.parse
import urllib.request
from datetime import datetime, timezone
from pathlib import Path
from typing import Optional

import tls_client

from .sms_helpers import (
    sms_get_number, sms_wait_code, sms_request_another,
    sms_cancel, sms_done, sms_reactivate, api_call_with_retry, get_error_code,
    is_waf_block, is_rate_limited,
)
from .gojek_client import GojekClient, CLIENT_ID as _GOJEK_CLIENT_ID, CLIENT_SECRET as _GOJEK_CLIENT_SECRET

from .envelope_manager import EnvelopeManager
from .gopay_payment_protocol import GoPayPayment, GoPayFraudDenyError
from .chain_proxy import get_chain_proxy_url, reset_chain_proxy_manager

log = logging.getLogger(__name__)

# ---------------------------------------------------------------------------
# Config
# ---------------------------------------------------------------------------

INBOX_URL = os.environ.get("OPAI_PAYMENT_INBOX_BASE_URL", "")
INBOX_USER = os.environ.get("OPAI_PAYMENT_INBOX_BASIC_USER", "")
INBOX_PASS = os.environ.get("OPAI_PAYMENT_INBOX_BASIC_PASS", "")
POLL_INTERVAL = float(os.environ.get("OPAI_GOPAY_POLL_INTERVAL", "10"))
MIN_REMAINING_SEC = int(os.environ.get("OPAI_GOPAY_MIN_REMAINING_SEC", "300"))
DEFAULT_PIN = os.environ.get("OPAI_GOPAY_DEFAULT_PIN", "147258")
MIN_BALANCE_RP = int(os.environ.get("OPAI_GOPAY_MIN_BALANCE_RP", "1"))

GOPAY_ACCOUNT_TTL = int(os.environ.get("OPAI_GOPAY_ACCOUNT_TTL_SEC", "1200"))
REGISTER_MAX_ATTEMPTS = int(os.environ.get("OPAI_GOPAY_REGISTER_ATTEMPTS", "5"))
SIGNUP_OTP_TIMEOUT = int(os.environ.get("OPAI_GOPAY_SIGNUP_OTP_TIMEOUT", "120"))
BALANCE_WAIT_SEC = int(os.environ.get("OPAI_GOPAY_BALANCE_WAIT_SEC", "180"))
WELCOME_GIFT_WAIT_SEC = int(os.environ.get("OPAI_GOPAY_WELCOME_WAIT_SEC", "300"))
WELCOME_WAIT_TRANSFER_SEC = int(os.environ.get("OPAI_GOPAY_WELCOME_WAIT_TRANSFER_SEC", "30"))
_FUND_TRANSFER_ENABLED = os.environ.get("OPAI_GOPAY_FUND_TRANSFER", "1").strip().lower() not in ("0", "false", "no")
_CONFIG_DIR = Path(__file__).resolve().parent.parent.parent.parent.parent / "config"
_MASTER_FILE = os.environ.get(
    "OPAI_GOPAY_MASTER_FILE",
    str(_CONFIG_DIR / "gopay_master.json"),
)
_ENVELOPE_LINKS_FILE = os.environ.get(
    "OPAI_GOPAY_ENVELOPE_FILE",
    str(_CONFIG_DIR / "envelope_links.txt"),
)

_NOVPROXY_TPL = os.environ.get("OPAI_GOPAY_PROXY_TEMPLATE", "")
_CLIPROXY_API = os.environ.get(
    "OPAI_CLIPROXY_API_URL",
    "https://api.cliproxy.io/white/api?region=ID&num=1&time=15&format=n&type=txt",
).strip()
_CLIPROXY_ENABLED = os.environ.get("OPAI_CLIPROXY_ENABLED", "1").strip().lower() not in ("0", "false", "no")
_PROXY_CHAIN_UPSTREAM = os.environ.get(
    "OPAI_PROXY_CHAIN_UPSTREAM",
    "http://127.0.0.1:10809",
).strip()
_PROXY_CHAIN_ENABLED = os.environ.get("OPAI_PROXY_CHAIN_ENABLED", "1").strip().lower() not in ("0", "false", "no")


def _cliproxy_regions() -> list[str]:
    """Region codes to try in order (e.g. ID, SG). Fresh IP fetched per call."""
    raw = os.environ.get("OPAI_CLIPROXY_REGIONS", "ID,SG").strip()
    regions = [r.strip().upper() for r in raw.split(",") if r.strip()]
    return regions or ["ID"]


def _cliproxy_api_url(region: str) -> str:
    import re
    base = _CLIPROXY_API
    if re.search(r"region=", base, re.I):
        return re.sub(r"region=[^&]+", f"region={region.upper()}", base, count=1, flags=re.I)
    sep = "&" if "?" in base else "?"
    return f"{base}{sep}region={region.upper()}"


def _fetch_cliproxy_raw(region: str = "") -> str:
    """Return ip:port or empty string from Cliproxy API."""
    if not _CLIPROXY_ENABLED:
        return ""
    api_url = _cliproxy_api_url(region) if region else _CLIPROXY_API
    if not api_url:
        return ""

    import subprocess

    try:
        ps = subprocess.run(
            [
                "curl.exe", "-s", "-m", "20",
                "--preproxy", _PROXY_CHAIN_UPSTREAM,
                api_url,
            ],
            capture_output=True,
            text=True,
            timeout=30,
            check=False,
        )
        raw = (ps.stdout or "").strip()
        if raw and "whitelist" not in raw.lower() and "not added" not in raw.lower():
            if raw.startswith("["):
                entries = json.loads(raw)
                if entries:
                    return f"{entries[0]['host']}:{entries[0]['port']}"
            return raw
    except Exception as exc:
        log.debug("Cliproxy fetch via curl chain failed: %s", exc)

    try:
        req = urllib.request.Request(api_url, headers={"User-Agent": "Mozilla/5.0"})
        if _PROXY_CHAIN_UPSTREAM:
            opener = urllib.request.build_opener(
                urllib.request.ProxyHandler(
                    {"http": _PROXY_CHAIN_UPSTREAM, "https": _PROXY_CHAIN_UPSTREAM},
                ),
            )
            raw = opener.open(req, timeout=20).read().decode().strip()
        else:
            raw = urllib.request.urlopen(req, timeout=15).read().decode().strip()
    except Exception as exc:
        log.warning("Cliproxy fetch failed: %s", exc)
        return ""

    if not raw or "whitelist" in raw.lower() or "not added" in raw.lower():
        log.warning("Cliproxy API rejected: %s", raw[:120])
        return ""

    if raw.startswith("["):
        try:
            entries = json.loads(raw)
            if entries:
                return f"{entries[0]['host']}:{entries[0]['port']}"
        except Exception as exc:
            log.warning("Cliproxy JSON parse failed: %s", exc)
            return ""

    return raw


def _fetch_cliproxy(region: str = "") -> str:
    """Fetch ip:port from Cliproxy whitelist API and return http:// proxy URL."""
    raw = _fetch_cliproxy_raw(region)
    if not raw:
        return ""
    if "://" not in raw:
        raw = f"http://{raw}"
    label = region.upper() if region else "?"
    log.info("Cliproxy [%s] fetched: %s", label, raw.split("@")[-1])
    return raw


def _make_chained_proxy(cliproxy: str) -> str:
    if not _PROXY_CHAIN_ENABLED or not _PROXY_CHAIN_UPSTREAM:
        return cliproxy
    try:
        local = get_chain_proxy_url(cliproxy, _PROXY_CHAIN_UPSTREAM)
        log.info("Using chained proxy: %s -> %s -> target", _PROXY_CHAIN_UPSTREAM, cliproxy)
        return local
    except Exception as exc:
        log.warning("Chain proxy setup failed: %s", exc)
        return cliproxy


def _probe_proxy_once(proxy: str) -> bool:
    if not proxy:
        return False
    try:
        s = tls_client.Session(client_identifier="okhttp4_android_13", force_http1=True)
        s.proxies = {"http": proxy, "https": proxy}
        r = s.post(
            "https://accounts.goto-products.com/goto-auth/login/methods",
            json={
                "client_id": "gojek:consumer:app",
                "client_secret": "x",
                "phone_number": "8000000000",
                "country_code": "+62",
            },
            timeout_seconds=30,
        )
        ok = r.status_code in (200, 401, 403, 404, 405, 422, 429)
        if not ok:
            log.warning("Proxy probe HTTP %d via %s", r.status_code, proxy.split("@")[-1])
        else:
            log.info("Proxy probe OK HTTP %d via %s", r.status_code, proxy.split("@")[-1])
        return ok
    except Exception as exc:
        log.warning("Proxy probe failed via %s: %s", proxy.split("@")[-1], exc)
        return False


def _probe_proxy(proxy: str, *, attempts: int = 2) -> bool:
    tries = max(1, attempts)
    for i in range(tries):
        if _probe_proxy_once(proxy):
            return True
        if i + 1 < tries:
            time.sleep(0.8)
    return False


def _upstream_reachable() -> bool:
    if not _PROXY_CHAIN_UPSTREAM:
        return True
    try:
        from urllib.parse import urlparse
        import socket

        u = urlparse(_PROXY_CHAIN_UPSTREAM)
        host = u.hostname or "127.0.0.1"
        port = u.port or (10809 if (u.scheme or "http").startswith("http") else 10808)
        with socket.create_connection((host, port), timeout=3):
            return True
    except OSError as exc:
        log.warning("Proxy chain upstream unreachable (%s): %s", _PROXY_CHAIN_UPSTREAM, exc)
        return False


def _make_proxy(*, required: bool = False, region: str = "") -> str:
    override = os.environ.get("OPAI_GOPAY_REGISTER_PROXY", "").strip()
    if override:
        return override

    skip_probe = os.environ.get("OPAI_CLIPROXY_SKIP_PROBE", "").strip().lower() in ("1", "true", "yes")
    max_attempts = max(1, int(os.environ.get("OPAI_CLIPROXY_PROBE_ATTEMPTS", "5")))
    probe_tries = max(1, int(os.environ.get("OPAI_CLIPROXY_PROBE_RETRIES", "3")))

    if _CLIPROXY_ENABLED and _PROXY_CHAIN_ENABLED and not _upstream_reachable():
        log.warning("Skipping Cliproxy chain (upstream %s down)", _PROXY_CHAIN_UPSTREAM)
    elif _CLIPROXY_ENABLED:
        regions = [region.upper()] if region else _cliproxy_regions()
        for reg in regions:
            for attempt in range(1, max_attempts + 1):
                reset_chain_proxy_manager()
                clip = _fetch_cliproxy(reg)
                if not clip:
                    log.warning("Cliproxy [%s] fetch empty attempt %d/%d", reg, attempt, max_attempts)
                    if attempt < max_attempts:
                        time.sleep(0.5)
                    continue
                proxy = _make_chained_proxy(clip)
                if skip_probe or _probe_proxy(proxy, attempts=probe_tries):
                    log.info("Cliproxy [%s] selected: %s", reg, proxy.split("@")[-1])
                    return proxy
                log.warning(
                    "Cliproxy [%s] probe failed attempt %d/%d (clip=%s chain=%s)",
                    reg, attempt, max_attempts,
                    clip.split("@")[-1], proxy.split("@")[-1],
                )
                reset_chain_proxy_manager()
                if attempt < max_attempts:
                    time.sleep(0.8)
            log.warning("Cliproxy region %s exhausted after %d fetch(es)", reg, max_attempts)
        log.warning("Cliproxy all regions failed: %s", ", ".join(regions))

    if _NOVPROXY_TPL:
        sid = "gp" + "".join(random.choices(string.ascii_letters + string.digits, k=6))
        return _NOVPROXY_TPL.format(sid=sid)

    if required:
        raise RuntimeError(
            "Indonesia GoPay proxy unavailable (Cliproxy chain failed; "
            "check v2rayN 10809 and config/cliproxy.url)"
        )
    return ""


# ---------------------------------------------------------------------------
# Inbox account sync
# ---------------------------------------------------------------------------

_INBOX_AUTH = None


def _inbox_auth_header() -> str:
    global _INBOX_AUTH
    if _INBOX_AUTH is None:
        _INBOX_AUTH = "Basic " + base64.b64encode(f"{INBOX_USER}:{INBOX_PASS}".encode()).decode()
    return _INBOX_AUTH


def _inbox_push_account(phone: str, data: dict):
    try:
        url = f"{INBOX_URL}/api/gopay-accounts"
        req = urllib.request.Request(url, data=json.dumps(data).encode(), method="POST")
        req.add_header("Content-Type", "application/json")
        req.add_header("Authorization", _inbox_auth_header())
        urllib.request.urlopen(req, timeout=10)
        log.info("[inbox] %s pushed", phone)
    except Exception as e:
        log.warning("[inbox] %s push failed: %s", phone, e)


def _inbox_delete_account(phone: str):
    try:
        url = f"{INBOX_URL}/api/gopay-accounts/{urllib.parse.quote(phone, safe='')}"
        req = urllib.request.Request(url, method="DELETE")
        req.add_header("Authorization", _inbox_auth_header())
        urllib.request.urlopen(req, timeout=10)
        log.info("[inbox] %s deleted", phone)
    except Exception as e:
        log.debug("[inbox] %s delete failed: %s", phone, e)


def _inbox_ttl_cleanup():
    def _loop():
        while True:
            time.sleep(60)
            try:
                url = f"{INBOX_URL}/api/gopay-accounts"
                req = urllib.request.Request(url)
                req.add_header("Authorization", _inbox_auth_header())
                resp = urllib.request.urlopen(req, timeout=10)
                data = json.loads(resp.read().decode())
                now = time.time()
                for a in data.get("accounts", []):
                    added = a.get("added_at", "")
                    if not added:
                        continue
                    try:
                        ts = datetime.fromisoformat(added.replace("Z", "+00:00")).timestamp()
                    except Exception:
                        continue
                    if now - ts > GOPAY_ACCOUNT_TTL:
                        phone = a.get("phone", "")
                        if phone:
                            log.info("[inbox-ttl] %s expired (%.0fs old), removing", phone, now - ts)
                            _inbox_delete_account(phone)
            except Exception as e:
                log.debug("[inbox-ttl] cleanup error: %s", e)

    t = threading.Thread(target=_loop, daemon=True, name="inbox-ttl")
    t.start()


# ---------------------------------------------------------------------------
# Deferred phone cancel
# ---------------------------------------------------------------------------

_CANCEL_MIN_AGE = 130


def _deferred_cancel_phone(api_key: str, activation_id: str, phone: str, rented_at: float):
    def _loop():
        _inbox_delete_account(phone)
        wait = max(0, _CANCEL_MIN_AGE - (time.time() - rented_at))
        if wait > 0:
            time.sleep(wait + 5)
        deadline = rented_at + 1200
        while time.time() < deadline:
            try:
                sms_cancel(api_key, activation_id)
                log.info("[cancel] %s OK", phone)
                return
            except Exception as e:
                log.debug("[cancel] %s error: %s", phone, e)
            time.sleep(180)
        log.info("[cancel] %s gave up", phone)

    t = threading.Thread(target=_loop, daemon=True, name=f"cancel-{phone}")
    t.start()


# ---------------------------------------------------------------------------
# Account persistence
# ---------------------------------------------------------------------------

ACCOUNTS_FILE = os.environ.get(
    "OPAI_GOPAY_ACCOUNTS_FILE",
    str(Path(__file__).resolve().parent.parent.parent.parent.parent / "config" / "gopay_worker_accounts.json"),
)
_accounts_lock = threading.Lock()


def _save_account(phone: str, local: str, pin: str, aid: str, client: GojekClient):
    entry = {
        "phone": phone,
        "local": local,
        "pin": pin,
        "activation_id": aid,
        "customer_id": client.user_uuid,
        "access_token": client.auth.access_token,
        "refresh_token": client.auth.refresh_token,
        "registered_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "balance": 0,
    }
    with _accounts_lock:
        accounts = []
        if os.path.exists(ACCOUNTS_FILE):
            try:
                accounts = json.loads(open(ACCOUNTS_FILE, encoding="utf-8").read())
            except Exception:
                pass
        accounts.append(entry)
        open(ACCOUNTS_FILE, "w", encoding="utf-8").write(json.dumps(accounts, indent=2, ensure_ascii=False))
    log.info("[save] %s saved locally", phone)
    _inbox_push_account(phone, {**entry, "added_at": entry["registered_at"]})


def _update_account_balance(phone: str, balance: int, client: GojekClient):
    with _accounts_lock:
        accounts = []
        if os.path.exists(ACCOUNTS_FILE):
            try:
                accounts = json.loads(open(ACCOUNTS_FILE, encoding="utf-8").read())
            except Exception:
                return
        for a in accounts:
            if a["phone"] == phone:
                a["balance"] = balance
                a["access_token"] = client.auth.access_token
                a["refresh_token"] = client.auth.refresh_token
                break
        open(ACCOUNTS_FILE, "w", encoding="utf-8").write(json.dumps(accounts, indent=2, ensure_ascii=False))
    log.info("[save] %s balance=%d updated locally", phone, balance)


def _check_balance(client: GojekClient) -> int:
    try:
        r = client.get_balance()
        if r["status"] == 200:
            data = r["body"].get("data", [])
            if isinstance(data, list) and data:
                return data[0].get("balance", {}).get("value", 0)
        return -1
    except Exception:
        return -1


# ---------------------------------------------------------------------------
# Register one GoPay account
# ---------------------------------------------------------------------------

def _proxy_label(proxy: str) -> str:
    if not proxy:
        return "direct"
    return proxy.replace("http://", "").replace("https://", "").split("@")[-1]


def _register_one(api_key: str, pin: str, proxy: str, envelope_did: str, *, skip_fund: bool = False) -> Optional[dict]:
    """Full registration flow: rent phone -> signup -> refresh -> PIN."""
    phone, aid = sms_get_number(api_key)
    if not phone:
        log.error("No phone number available")
        return None

    rented_at = time.time()
    local = phone.lstrip("+")
    if local.startswith("62"):
        local = local[2:]

    log.info("[%s] Proxy: %s", phone, _proxy_label(proxy))
    client = GojekClient.from_phone(phone, proxy=proxy)
    success = False
    cancel_immediately = False

    try:
        # === Phase 1: Login check ===
        time.sleep(2)
        methods = api_call_with_retry(client.get_login_methods, "+62", local)

        if methods["status"] in (200, 201):
            log.info("[%s] Already registered, skipping", phone)
            return None

        err_code = get_error_code(methods)
        if methods["status"] == 403 or is_waf_block(methods):
            log.warning("[%s] WAF 403, need new proxy IP", phone)
            return None

        # === Signup ===
        log.info("[%s] New number -> signup", phone)
        otp_result = client.signup_request_otp(phone)
        if otp_result["status"] not in (200, 201):
            log.error("[%s] Signup OTP failed: %d", phone, otp_result["status"])
            return None

        otp = sms_wait_code(api_key, aid, timeout=SIGNUP_OTP_TIMEOUT)
        if not otp:
            log.error("[%s] Signup OTP timeout (%ds), will switch number", phone, SIGNUP_OTP_TIMEOUT)
            cancel_immediately = True
            return None
        log.info("[%s] Signup OTP: %s", phone, otp)

        time.sleep(2)
        verify = api_call_with_retry(client.signup_verify_otp, otp, phone)
        if verify["status"] not in (200, 201):
            log.error("[%s] Signup verify failed: %d", phone, verify["status"])
            return None

        time.sleep(2)
        names = [
            "Budi Santoso", "Adi Pratama", "Siti Rahayu", "Dewi Lestari",
            "Rizky Ramadhan", "Putri Wulandari", "Agus Setiawan", "Rina Kusuma",
            "Hendra Wijaya", "Novi Anggraini", "Dian Permata", "Wahyu Hidayat",
            "Fitri Handayani", "Joko Susilo", "Ratna Sari", "Bambang Prasetyo",
            "Mega Puspita", "Eko Nugroho", "Sari Indah", "Yusuf Maulana",
            "Lina Marlina", "Arief Rahman", "Wati Suryani", "Dedi Kurniawan",
            "Ayu Lestari", "Rudi Hartono", "Nisa Fitriani", "Bayu Anggara",
            "Sri Mulyani", "Fajar Setiadi", "Indra Gunawan", "Tika Rahmawati",
        ]
        signup = api_call_with_retry(client.signup_create_account,
                                     name=random.choice(names), phone=phone, email="", country="ID")
        if signup["status"] not in (200, 201):
            err = get_error_code(signup)
            if "phone_already_taken" not in err:
                log.error("[%s] Signup failed: %s", phone, signup["body"])
                return None
        log.info("[%s] Signup success (uid=%s)", phone, client.user_uuid)

        # === Phase 2: Refresh ===
        # Exchange signup refresh_token for SSO access_token (required for PIN CVS).
        # Stale CVS verification headers cause "Revoked verification token" on refresh.
        time.sleep(3)
        client.auth.verification_token = ""
        client.auth.onefa_token = ""
        refresh = api_call_with_retry(client.refresh_token)
        if refresh["status"] not in (200, 201):
            log.error("[%s] Token refresh failed: %d %s", phone, refresh["status"], refresh.get("body"))
            return None
        log.info("[%s] Token refreshed", phone)

        # === Phase 3: GoPay Init ===
        time.sleep(2)
        api_call_with_retry(client.gopay_init)
        time.sleep(2)
        api_call_with_retry(client.gopay_get_profiles)
        time.sleep(2)
        profile = api_call_with_retry(client.get_user_profile)
        is_pin_set = profile["body"].get("data", {}).get("is_pin_setup", False) if profile["status"] == 200 else False

        if is_pin_set:
            log.info("[%s] PIN already set", phone)
        else:
            log.info("[%s] Setting PIN...", phone)
            poll_aid = sms_request_another(api_key, aid)
            time.sleep(2)

            pin_otp_r = api_call_with_retry(client.pin_request_otp)
            if pin_otp_r["status"] not in (200, 201):
                log.error("[%s] PIN OTP request failed: %d", phone, pin_otp_r["status"])
                return None

            pin_code = sms_wait_code(api_key, poll_aid, timeout=60)
            if not pin_code:
                log.warning("[%s] PIN OTP timeout 60s, resending...", phone)
                resend_body = {
                    "client_id": _GOJEK_CLIENT_ID,
                    "client_secret": _GOJEK_CLIENT_SECRET,
                    "flow": "goto_pin_wa_sms",
                    "verification_id": client.auth.verification_id,
                    "verification_method": "otp_sms",
                }
                time.sleep(2)
                resend = client._sso_post("/cvs/v1/initiate", resend_body)
                if resend["status"] in (200, 201):
                    inner = resend["body"].get("data", resend["body"])
                    client.auth.otp_token = inner.get("otp_token", "")
                    poll_aid = sms_request_another(api_key, poll_aid)
                    pin_code = sms_wait_code(api_key, poll_aid, timeout=180)

            if not pin_code:
                log.error("[%s] PIN OTP not received, will switch number", phone)
                cancel_immediately = True
                return None
            log.info("[%s] PIN OTP: %s", phone, pin_code)

            time.sleep(2)
            pin_verify = api_call_with_retry(client.pin_verify_otp, pin_code)
            if pin_verify["status"] not in (200, 201):
                log.error("[%s] PIN verify failed: %d", phone, pin_verify["status"])
                return None

            time.sleep(2)
            pin_result = api_call_with_retry(client.pin_setup, pin)
            if pin_result["status"] not in (200, 201):
                log.error("[%s] PIN setup failed: %d", phone, pin_result["status"])
                return None
            log.info("[%s] PIN set OK", phone)

        # === Phase 5: Wait for 1 Rp (welcome gift / red envelope) ===
        skip = skip_fund or os.environ.get("OPAI_GOPAY_SKIP_FUND", "").strip().lower() in ("1", "true", "yes")
        funded_via = "skip"
        if skip:
            balance = _check_balance(client)
            if balance < 0:
                balance = 0
            log.info("[%s] Skip fund: balance=%d Rp  funded_via: skip", phone, balance)
        else:
            fund = _fund_account(client, phone, envelope_did, proxy=proxy)
            balance = fund["balance"]
            funded_via = fund["funded_via"]

        # === Save account ===
        _save_account(phone, local, pin, aid, client)
        if balance >= MIN_BALANCE_RP:
            _update_account_balance(phone, balance, client)

        success = True
        return {
            "phone": phone, "aid": aid, "pin": pin, "client": client,
            "local": local, "balance_rp": balance, "funded_via": funded_via, "proxy": proxy,
        }

    except Exception as e:
        log.exception("[%s] Registration exception: %s", phone, e)
        return None
    finally:
        if not success:
            if cancel_immediately:
                try:
                    sms_cancel(api_key, aid)
                    log.info("[cancel] %s cancelled immediately for number switch", phone)
                except Exception:
                    pass
            else:
                _deferred_cancel_phone(api_key, aid, phone, rented_at)


def _register_with_retries(
    api_key: str,
    pin: str,
    envelope_did: str,
    max_attempts: int | None = None,
    *,
    skip_fund: bool = False,
    proxy: str = "",
) -> Optional[dict]:
    """Register with automatic number switch on OTP timeout or transient failure."""
    attempts = max_attempts or REGISTER_MAX_ATTEMPTS
    fixed_proxy = proxy.strip()
    for i in range(1, attempts + 1):
        use_proxy = fixed_proxy or _make_proxy(required=True)
        log.info(
            "Registration attempt %d/%d proxy=%s",
            i, attempts, use_proxy.split("@")[-1] if use_proxy else "direct",
        )
        result = _register_one(api_key, pin, use_proxy, envelope_did, skip_fund=skip_fund)
        if result:
            return result
        if i < attempts:
            log.warning("Attempt %d/%d failed, switching to new number in 5s", i, attempts)
            time.sleep(5)
    log.error("Registration failed after %d attempts", attempts)
    return None


# ---------------------------------------------------------------------------
# Job handling
# ---------------------------------------------------------------------------

def _job_remaining_sec(job: dict) -> float:
    expires = job.get("expires_at", "")
    if not expires:
        return 3600
    try:
        exp = datetime.fromisoformat(expires.replace("Z", "+00:00"))
        return (exp - datetime.now(timezone.utc)).total_seconds()
    except Exception:
        return 3600


def _get_envelope_did() -> str:
    env_did = (os.environ.get("OPAI_GOPAY_ENVELOPE_DEEPLINK") or "").strip()
    if env_did:
        return env_did
    try:
        url = f"{INBOX_URL}/api/envelopes"
        req = urllib.request.Request(url)
        cred = base64.b64encode(f"{INBOX_USER}:{INBOX_PASS}".encode()).decode()
        req.add_header("Authorization", f"Basic {cred}")
        with urllib.request.urlopen(req, timeout=10) as resp:
            data = json.loads(resp.read().decode())
        for e in data.get("envelopes", []):
            if e.get("status") == "active":
                return e["deeplink_id"]
    except Exception as exc:
        log.debug("Failed to fetch envelope from inbox: %s", exc)
    return ""


def _build_envelope_manager(envelope_did: str = "") -> EnvelopeManager:
    mgr = EnvelopeManager()
    path = Path(_ENVELOPE_LINKS_FILE)
    if path.exists():
        for line in path.read_text(encoding="utf-8").splitlines():
            line = line.strip()
            if not line or line.startswith("#"):
                continue
            if line.startswith("http"):
                mgr.add_url(line)
            elif re.fullmatch(r"[0-9a-fA-F]{24}", line):
                mgr.add_deeplink_id(line)
    if envelope_did:
        mgr.add_deeplink_id(envelope_did)
    inbox_did = _get_envelope_did()
    if inbox_did and inbox_did != envelope_did:
        mgr.add_deeplink_id(inbox_did)
    return mgr


def _wait_for_balance(
    client: GojekClient,
    phone: str,
    min_rp: int = MIN_BALANCE_RP,
    timeout_sec: int = BALANCE_WAIT_SEC,
) -> int:
    start = time.time()
    while time.time() - start < timeout_sec:
        bal = _check_balance(client)
        if bal >= min_rp:
            _update_account_balance(phone, bal, client)
            return bal
        if bal >= 0:
            elapsed = int(time.time() - start)
            log.info("[%s] Balance=%d Rp (need >=%d), waiting 15s... (%ds)", phone, bal, min_rp, elapsed)
        else:
            try:
                client.refresh_token()
            except Exception:
                pass
        time.sleep(15)
    bal = _check_balance(client)
    if bal >= min_rp:
        _update_account_balance(phone, bal, client)
    return bal


def _fund_account(
    client: GojekClient,
    phone: str,
    envelope_did: str = "",
    *,
    proxy: str = "",
) -> dict:
    """Wait for welcome 1 Rp; try master transfer; then red envelope if needed.

    Returns {"balance": int, "funded_via": "welcome"|"transfer"|"envelope"|"none"}.
    """
    master_cfg = _load_master_config()
    use_transfer = _FUND_TRANSFER_ENABLED and master_cfg is not None
    welcome_sec = WELCOME_WAIT_TRANSFER_SEC if use_transfer else WELCOME_GIFT_WAIT_SEC

    log.info(
        "[%s] Waiting for auto welcome >= %d Rp (up to %ds%s)...",
        phone, MIN_BALANCE_RP, welcome_sec,
        ", then master transfer" if use_transfer else ", skip envelope if credited",
    )
    bal = _wait_for_balance(client, phone, MIN_BALANCE_RP, timeout_sec=welcome_sec)
    if bal >= MIN_BALANCE_RP:
        log.info("[%s] funded_via: welcome  balance=%d Rp", phone, bal)
        return {"balance": bal, "funded_via": "welcome"}

    if use_transfer:
        log.info("[%s] No welcome after %ds — trying master transfer", phone, welcome_sec)
        xfer = _transfer_from_master(phone, proxy=proxy)
        if xfer.get("ok"):
            time.sleep(3)
            bal = _wait_for_balance(client, phone, MIN_BALANCE_RP, timeout_sec=BALANCE_WAIT_SEC)
            if bal >= MIN_BALANCE_RP:
                log.info("[%s] funded_via: transfer  balance=%d Rp", phone, bal)
                return {"balance": bal, "funded_via": "transfer"}
        if not master_cfg.get("fallback_envelope", True):
            log.warning("[%s] funded_via: none  balance=%d Rp (transfer failed, envelope disabled)", phone, bal)
            return {"balance": bal, "funded_via": "none"}
        log.info("[%s] Master transfer failed (%s) — falling back to envelope", phone, xfer.get("reason", "?"))

    log.info("[%s] Trying red envelope", phone)
    mgr = _build_envelope_manager(envelope_did)
    active = mgr.get_active()
    if active:
        log.info("[%s] Claiming red envelope (%d link(s))...", phone, len(active))
        claim = mgr.claim_one(client)
        if claim and claim.get("status") in (200, 201):
            log.info("[%s] Envelope claim OK", phone)
        time.sleep(3)
        bal = _wait_for_balance(client, phone, MIN_BALANCE_RP, timeout_sec=BALANCE_WAIT_SEC)
        if bal >= MIN_BALANCE_RP:
            log.info("[%s] funded_via: envelope  balance=%d Rp", phone, bal)
            return {"balance": bal, "funded_via": "envelope"}
    else:
        log.warning(
            "[%s] No envelope links (%s); waiting longer for welcome gift",
            phone, _ENVELOPE_LINKS_FILE,
        )
        bal = _wait_for_balance(client, phone, MIN_BALANCE_RP, timeout_sec=BALANCE_WAIT_SEC)
        if bal >= MIN_BALANCE_RP:
            log.info("[%s] funded_via: welcome  balance=%d Rp (late credit)", phone, bal)
            return {"balance": bal, "funded_via": "welcome"}

    log.warning("[%s] funded_via: none  balance=%d Rp", phone, bal)
    return {"balance": bal, "funded_via": "none"}


# ---------------------------------------------------------------------------
# Payment
# ---------------------------------------------------------------------------

def _pp_pool_id_from_job(job: dict) -> Optional[int]:
    notes = str(job.get("notes") or "")
    m = re.search(r"pool_id=(\d+)", notes)
    return int(m.group(1)) if m else None


def _mark_pp_pool_job(job: dict, *, success: bool, detail: str = "") -> None:
    pid = _pp_pool_id_from_job(job)
    if not pid:
        return
    try:
        from .pp_pool import mark_result
        mark_result(pid, success=success, error=detail)
        log.info("[pool] pool_id=%s marked %s", pid, "activated" if success else "failed")
    except Exception as e:
        log.warning("[pool] failed to update pool_id=%s: %s", pid, e)


def _pay_job(job: dict, account: dict, inbox_client, api_key: str, pin: str, proxy: str = "") -> tuple[bool, str]:
    job_id = job["id"]
    midtrans_url = job.get("provider_url") or job.get("paypal_url") or ""
    phone = account["local"]
    log.info("[job:%s] Paying with %s (protocol)", job_id[:8], account["phone"])

    try:
        payment = GoPayPayment(proxy=proxy)
        otp_poll_aid = account["aid"]

        def prepare_otp(ph: str) -> None:
            nonlocal otp_poll_aid
            otp_poll_aid = sms_request_another(api_key, account["aid"], phone=account["phone"])
            time.sleep(2)

        def wait_otp(ph: str, timeout: int = 120) -> Optional[str]:
            return sms_wait_code(api_key, otp_poll_aid, timeout=timeout)

        result = payment.pay(
            midtrans_url=midtrans_url,
            phone=phone,
            country_code="62",
            pin=pin,
            prepare_otp=prepare_otp,
            wait_otp=wait_otp,
        )

        detail = result.get("detail", "")
        if result.get("success"):
            log.info("[job:%s] Payment SUCCESS!", job_id[:8])
            _mark_pp_pool_job(job, success=True)
            try:
                inbox_client._req("PUT", f"/api/jobs/{job_id}/paid")
            except Exception as e:
                log.error("[job:%s] Mark paid failed: %s", job_id[:8], e)
            return True, detail
        else:
            log.warning("[job:%s] Payment failed: %s", job_id[:8], detail)
            _mark_pp_pool_job(job, success=False, detail=detail)
            try:
                inbox_client._req("PUT", f"/api/jobs/{job_id}/cancel")
            except Exception:
                pass
            return False, detail

    except GoPayFraudDenyError as e:
        log.warning("[job:%s] FRAUD DENIED: %s", job_id[:8], e)
        _mark_pp_pool_job(job, success=False, detail=str(e))
        try:
            inbox_client._req("PUT", f"/api/jobs/{job_id}/cancel")
        except Exception:
            pass
        return False, "fraud_deny -- phone burned"

    except Exception as e:
        log.exception("[job:%s] Payment exception: %s", job_id[:8], e)
        _mark_pp_pool_job(job, success=False, detail=str(e))
        try:
            inbox_client._req("PUT", f"/api/jobs/{job_id}/cancel")
        except Exception:
            pass
        return False, str(e)


def _claim_job(inbox, min_remaining: float = MIN_REMAINING_SEC) -> Optional[dict]:
    try:
        job = inbox._req("POST", "/api/jobs/claim_next", data={
            "prefer_paypal_url": False, "prefer_oldest": True, "provider": "gopay",
        })
    except RuntimeError as e:
        if "HTTP 404" not in str(e):
            log.warning("Inbox poll error: %s", e)
        return None
    except Exception as e:
        log.warning("Inbox poll error: %s", e)
        return None

    if job is None:
        return None

    url = job.get("provider_url") or job.get("paypal_url") or ""
    if "midtrans" not in url:
        return None

    remaining = _job_remaining_sec(job)
    if remaining < min_remaining:
        log.info("Job %s: %.0fs left < %ds, cancelling", job["id"][:8], remaining, min_remaining)
        try:
            inbox._req("PUT", f"/api/jobs/{job['id']}/cancel")
        except Exception:
            pass
        return None

    return job


# ---------------------------------------------------------------------------
# Phone reactivation
# ---------------------------------------------------------------------------

_PHONE_LIFETIME = 1080


def _sms_reactivate(api_key: str, activation_id: str, phone: str = "") -> Optional[str]:
    return sms_reactivate(api_key, activation_id, phone=phone)


def _resume_account(phone: str, proxy: str = "") -> Optional[dict]:
    if not os.path.exists(ACCOUNTS_FILE):
        log.error("[resume] %s not found", ACCOUNTS_FILE)
        return None
    accounts = json.loads(open(ACCOUNTS_FILE, encoding="utf-8").read())
    digits = phone.strip().lstrip("+")
    entry = None
    for a in accounts:
        a_digits = a["phone"].strip().lstrip("+")
        if a_digits == digits or a.get("local", "") == digits or digits.endswith(a.get("local", "\x00")):
            entry = a
            break
    if not entry:
        log.error("[resume] phone %s not found in %s", phone, ACCOUNTS_FILE)
        return None

    if not proxy:
        proxy = _make_proxy()
    client = GojekClient.from_phone(entry["phone"], proxy=proxy)
    client.auth.access_token = entry["access_token"]
    client.auth.refresh_token = entry["refresh_token"]
    client.user_uuid = entry.get("customer_id", "")

    log.info("[resume] Refreshing token for %s...", entry["phone"])
    try:
        r = client.refresh_token()
        if r["status"] in (200, 201):
            log.info("[resume] Token refreshed OK for %s", entry["phone"])
        else:
            log.warning("[resume] Token refresh returned %d, trying with existing token", r["status"])
    except Exception as e:
        log.warning("[resume] Token refresh failed: %s, trying with existing token", e)

    return {
        "phone": entry["phone"],
        "client": client,
        "aid": entry.get("activation_id", ""),
        "pin": entry.get("pin", DEFAULT_PIN),
        "local": entry.get("local", ""),
        "resumed": True,
    }


def _load_master_config() -> Optional[dict]:
    path = Path(_MASTER_FILE)
    if not path.exists():
        return None
    try:
        raw = json.loads(path.read_text(encoding="utf-8"))
        cfg = raw[0] if isinstance(raw, list) else raw
        if not isinstance(cfg, dict) or not cfg.get("phone"):
            return None
        if cfg.get("enabled") is False:
            return None
        return cfg
    except Exception as exc:
        log.warning("Failed to load master config from %s: %s", _MASTER_FILE, exc)
        return None


def _load_master_client(proxy: str = "") -> Optional[dict]:
    cfg = _load_master_config()
    if not cfg:
        return None
    phone = str(cfg["phone"]).strip()
    pin = str(cfg.get("pin") or DEFAULT_PIN)
    use_proxy = proxy or _make_proxy()
    resumed = _resume_account(phone, proxy=use_proxy)
    if resumed:
        return {**resumed, "pin": pin, "config": cfg}

    token = str(cfg.get("access_token") or "").strip()
    if not token:
        log.warning(
            "[master] %s not in %s and no access_token in %s",
            phone, ACCOUNTS_FILE, _MASTER_FILE,
        )
        return None

    client = GojekClient.from_phone(phone, proxy=use_proxy)
    client.auth.access_token = token
    client.auth.refresh_token = str(cfg.get("refresh_token") or "")
    client.user_uuid = str(cfg.get("customer_id") or "")
    try:
        r = client.refresh_token()
        if r["status"] not in (200, 201):
            log.warning("[master] token refresh returned %d, using stored token", r["status"])
    except Exception as exc:
        log.warning("[master] token refresh failed: %s", exc)

    local = str(cfg.get("local") or GojekClient.normalize_p2p_phone(phone))
    return {
        "phone": phone,
        "client": client,
        "pin": pin,
        "local": local,
        "config": cfg,
    }


def _transfer_from_master(child_phone: str, proxy: str = "", *, amount: int | None = None) -> dict:
    """Send transfer_amount Rp from configured master account to child phone."""
    master = _load_master_client(proxy=proxy)
    if not master:
        return {"ok": False, "reason": "no_master"}

    cfg = master["config"]
    xfer_amount = int(amount if amount is not None else cfg.get("transfer_amount") or MIN_BALANCE_RP)
    client: GojekClient = master["client"]
    pin = master["pin"]
    child_local = GojekClient.normalize_p2p_phone(child_phone)

    profile = api_call_with_retry(client.p2p_profile, child_phone)
    if profile["status"] != 200:
        body = profile.get("body") or {}
        log.warning("[master] p2p-profile %s → %d %s", child_phone, profile["status"], body)
        return {"ok": False, "reason": "p2p_lookup_failed", "status": profile["status"]}

    data = (profile["body"] or {}).get("data") or {}
    if data.get("is_blocked"):
        return {"ok": False, "reason": "payee_blocked"}
    qr_id = data.get("qr_id")
    if not qr_id:
        return {"ok": False, "reason": "no_qr_id"}

    xfer = api_call_with_retry(
        client.transfer_funds,
        qr_id,
        xfer_amount,
        pin,
        idempotency_key=str(uuid.uuid4()),
    )
    if xfer["status"] not in (200, 201):
        body = xfer.get("body") or {}
        log.warning("[master] transfer → %d %s", xfer["status"], body)
        return {"ok": False, "reason": "transfer_failed", "status": xfer["status"]}

    log.info("[master] %s sent %d Rp to %s", master["phone"], xfer_amount, child_phone)
    return {"ok": True, "amount": xfer_amount}


# ---------------------------------------------------------------------------
# Worker loop
# ---------------------------------------------------------------------------

def _worker_loop(
    inbox, api_key: str, pin: str, stop: threading.Event,
    worker_id: int,
    resume_phone: str = "",
):
    tag = f"[w{worker_id}]"
    envelope_did = _get_envelope_did()

    while not stop.is_set():
        # === Register or resume ===
        if resume_phone:
            log.info("%s Resuming account %s...", tag, resume_phone)
            proxy = _make_proxy()
            account = _resume_account(resume_phone, proxy)
            resume_phone = ""
        else:
            new_did = _get_envelope_did()
            if new_did:
                envelope_did = new_did
            log.info("%s Registering new GoPay account...", tag)
            account = _register_with_retries(api_key, pin, envelope_did)

        if not account:
            log.warning("%s Registration/resume failed, retry in 10s", tag)
            stop.wait(10)
            continue

        phone = account["phone"]
        client = account["client"]
        aid = account["aid"]
        is_resumed = account.get("resumed", False)
        register_time = 0 if is_resumed else time.time()
        proxy = _make_proxy()
        log.info("%s Account ready: %s%s", tag, phone, " (resumed)" if is_resumed else "")

        # === Wait for balance >= MIN_BALANCE_RP ===
        balance_ok = False
        max_wait = 3600
        wait_start = time.time()
        phone_activated_at = register_time
        reactivate_count = 0
        max_reactivates = 3
        while not stop.is_set():
            if time.time() - wait_start > max_wait:
                log.warning("%s Waited %ds for balance, giving up", tag, max_wait)
                break

            phone_age = time.time() - phone_activated_at
            if phone_age > _PHONE_LIFETIME - 120:
                if reactivate_count < max_reactivates:
                    log.info("%s Phone expiring during balance wait, reactivating (%d/%d)...",
                             tag, reactivate_count + 1, max_reactivates)
                    new_aid = _sms_reactivate(api_key, aid, phone=phone)
                    if new_aid:
                        aid = new_aid
                        account["aid"] = new_aid
                        phone_activated_at = time.time()
                        reactivate_count += 1
                    else:
                        log.warning("%s Reactivate failed during balance wait, phone may be lost", tag)
                        reactivate_count += 1

            bal = _check_balance(client)
            if bal >= MIN_BALANCE_RP:
                log.info("%s Balance=%d Rp (>=%d), ready!", tag, bal, MIN_BALANCE_RP)
                _update_account_balance(phone, bal, client)
                _inbox_delete_account(phone)
                balance_ok = True
                break
            elif bal >= 0:
                waited = int(time.time() - wait_start)
                log.info("%s Balance=%d Rp (need >=%d), waiting 15s... (%ds elapsed)", tag, bal, MIN_BALANCE_RP, waited)
                stop.wait(15)
            else:
                log.warning("%s Balance check failed, trying token refresh", tag)
                try:
                    client.refresh_token()
                except Exception:
                    pass
                stop.wait(30)

        if not balance_ok:
            log.info("%s No balance after waiting, registering new account", tag)
            continue

        # === Payment loop ===
        while not stop.is_set():
            phone_age = time.time() - phone_activated_at
            if phone_age > _PHONE_LIFETIME - 120:
                if reactivate_count >= max_reactivates:
                    log.info("%s Max reactivates (%d) reached, retiring phone", tag, max_reactivates)
                    break
                log.info("%s Phone expiring, reactivating (%d/%d)...", tag, reactivate_count + 1, max_reactivates)
                new_aid = _sms_reactivate(api_key, aid, phone=phone)
                if new_aid:
                    aid = new_aid
                    account["aid"] = new_aid
                    phone_activated_at = time.time()
                    reactivate_count += 1
                    log.info("%s Reactivated, new aid=%s", tag, new_aid)
                else:
                    log.warning("%s Reactivate failed, retiring phone", tag)
                    break

            job = _claim_job(inbox)
            if not job:
                stop.wait(POLL_INTERVAL)
                continue

            remaining = _job_remaining_sec(job)
            phone_left = _PHONE_LIFETIME - (time.time() - phone_activated_at)
            log.info("%s Job %s -> %s (job %.0fs, phone %.0fs)",
                     tag, job["id"][:8], phone, remaining, phone_left)

            success, detail = _pay_job(job, account, inbox, api_key, pin, proxy=proxy)
            if success:
                log.info("%s Job %s paid!", tag, job["id"][:8])
                break

            if "fraud_deny" in detail.lower() or "fraud denied" in detail.lower() or "burned" in detail.lower():
                log.warning("%s FRAUD DENIED, retiring phone", tag)
                break

            if "already linked" in detail.lower():
                log.warning("%s Already linked, retiring phone", tag)
                break

            log.warning("%s Job %s failed (%s), next job", tag, job["id"][:8], detail[:60])

        # === Release phone ===
        try:
            sms_done(api_key, aid)
        except Exception:
            pass


# ---------------------------------------------------------------------------
# Entry points
# ---------------------------------------------------------------------------

def run_worker(
    max_workers: int = 3,
    pin: str = DEFAULT_PIN,
    poll_interval: float = POLL_INTERVAL,
    resume_phones: Optional[list] = None,
    api_key: str = "",
):
    from .payment_inbox import PaymentInboxClient
    from .sms_helpers import resolve_sms_token, sms_provider

    if not api_key:
        api_key = resolve_sms_token()
    if not api_key:
        log.error("No SMS API token. Set OPAI_SMS_PROVIDER and matching key env/file")
        return
    log.info("SMS provider: %s", sms_provider())

    inbox = PaymentInboxClient(base_url=INBOX_URL, basic_auth=(INBOX_USER, INBOX_PASS))
    stop = threading.Event()

    resume_phones = resume_phones or []
    actual_workers = max(max_workers, len(resume_phones))
    log.info("Worker started: workers=%d poll=%.0fs resume=%s ttl=%ds",
             actual_workers, poll_interval, resume_phones or "(none)", GOPAY_ACCOUNT_TTL)
    _inbox_ttl_cleanup()

    threads = []
    for i in range(actual_workers):
        rp = resume_phones[i] if i < len(resume_phones) else ""
        t = threading.Thread(
            target=_worker_loop,
            args=(inbox, api_key, pin, stop, i),
            kwargs={"resume_phone": rp},
            daemon=True, name=f"w{i}",
        )
        t.start()
        threads.append(t)
        time.sleep(2)

    try:
        while True:
            alive = sum(1 for t in threads if t.is_alive())
            if alive == 0:
                log.error("All workers dead, exiting")
                break
            time.sleep(30)
    except KeyboardInterrupt:
        log.info("Shutting down")
        stop.set()


def main():
    import argparse
    parser = argparse.ArgumentParser(description="GoPay Protocol Worker")
    parser.add_argument("--workers", type=int, default=3)
    parser.add_argument("--pin", default=DEFAULT_PIN)
    parser.add_argument("--poll", type=float, default=POLL_INTERVAL)
    parser.add_argument("--api-key", default="", help="Hero-SMS API key (or set OPAI_HEROSMS_API_KEY)")
    parser.add_argument("--dry-run", action="store_true", help="Register one account only, no inbox")
    parser.add_argument("--keep-sms-open", action="store_true", help="Do not finish SMS order after dry-run")
    parser.add_argument("--resume", nargs="+", metavar="PHONE", help="Resume from existing accounts")
    args = parser.parse_args()

    logging.basicConfig(level=logging.INFO, format="%(asctime)s [%(levelname)s] %(name)s: %(message)s", datefmt="%H:%M:%S")

    if args.dry_run:
        log.info("=== DRY RUN: register one account ===")
        from .sms_helpers import resolve_sms_token
        api_key = resolve_sms_token(args.api_key)
        if not api_key:
            log.error("No SMS API token")
            return
        proxy = _make_proxy()
        envelope_did = _get_envelope_did()
        result = _register_one(api_key, args.pin, proxy, envelope_did)
        if result:
            log.info("SUCCESS: %s pin=%s", result["phone"], args.pin)
            if args.keep_sms_open:
                log.info("SMS activation kept open for payment OTP: %s", result["aid"])
            else:
                sms_done(api_key, result["aid"])
        else:
            log.error("FAILED")
        return

    run_worker(max_workers=args.workers, pin=args.pin, poll_interval=args.poll,
               resume_phones=args.resume, api_key=args.api_key)


if __name__ == "__main__":
    main()
