"""
SMS verification helpers — Hero-SMS and 5sim.net.

Set OPAI_SMS_PROVIDER=herosms (default) or 5sim.
"""
from __future__ import annotations

import json
import logging
import os
import re
import time
from typing import Any

import tls_client

log = logging.getLogger(__name__)

HEROSMS_API = "https://hero-sms.com"
FIVESIM_API = "https://5sim.net/v1"
SMS_TIMEOUT = 120

# 5sim: order_id -> number of SMS already consumed on that order
_seen_sms_count: dict[str, int] = {}


def sms_provider() -> str:
    return os.environ.get("OPAI_SMS_PROVIDER", "herosms").strip().lower()


def resolve_sms_token(cli_key: str = "") -> str:
    if cli_key:
        return cli_key.strip()
    provider = sms_provider()
    if provider == "5sim":
        env_key = os.environ.get("OPAI_5SIM_API_TOKEN", "").strip()
        if env_key:
            return env_key
        key_file = os.environ.get("OPAI_5SIM_API_TOKEN_FILE", "").strip()
        if key_file and os.path.exists(key_file):
            return open(key_file, encoding="utf-8").read().strip()
        return ""
    env_key = os.environ.get("OPAI_HEROSMS_API_KEY", "").strip()
    if env_key:
        return env_key
    key_file = os.environ.get("OPAI_HEROSMS_API_KEY_FILE", "").strip()
    if key_file and os.path.exists(key_file):
        return open(key_file, encoding="utf-8").read().strip()
    return ""


def _extract_otp(text: str) -> str:
    m = re.search(r"\b(\d{4,6})\b", text or "")
    return m.group(1) if m else (text or "").strip()


# ---------------------------------------------------------------------------
# Hero-SMS
# ---------------------------------------------------------------------------

def _herosms_api(api_key: str, action: str, params: dict | None = None, retries: int = 3) -> str:
    p = {"api_key": api_key, "action": action}
    if params:
        p.update(params)
    for i in range(1, retries + 1):
        try:
            s = tls_client.Session(client_identifier="chrome_120")
            r = s.get(f"{HEROSMS_API}/stubs/handler_api.php", params=p, timeout_seconds=30)
            return r.text.strip()
        except Exception as e:
            log.debug("herosms %s attempt %d: %s", action, i, e)
            if i < retries:
                time.sleep(3)
    raise RuntimeError(f"herosms {action} failed after {retries} retries")


def _herosms_get_number(api_key: str, attempts: int = 15, delay: float = 4.0) -> tuple[str | None, str | None]:
    for i in range(1, attempts + 1):
        resp = _herosms_api(api_key, "getNumber", {"service": "ni", "country": "6"})
        log.info("[herosms] getNumber: %s", resp)
        if resp.startswith("ACCESS_NUMBER:"):
            parts = resp.split(":")
            return f"+{parts[2]}", parts[1]
        if resp != "NO_NUMBERS" or i == attempts:
            log.warning("[herosms] getNumber failed: %s", resp)
            return None, None
        log.info("[herosms] NO_NUMBERS, retry %d/%d in %.0fs", i, attempts, delay)
        time.sleep(delay)
    return None, None


def _herosms_wait_code(api_key: str, aid: str, timeout: int = SMS_TIMEOUT) -> str | None:
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            resp = _herosms_api(api_key, "getStatus", {"id": aid})
        except Exception:
            time.sleep(5)
            continue
        if resp.startswith("STATUS_OK:"):
            return _extract_otp(resp.split(":", 1)[1])
        if resp == "STATUS_CANCEL":
            log.warning("[herosms] activation cancelled")
            return None
        time.sleep(5)
    return None


def _herosms_request_another(api_key: str, aid: str, phone: str = "") -> str:
    try:
        resp = _herosms_api(api_key, "setStatus", {"id": aid, "status": "3"})
        log.info("[herosms] request_another: %s", resp)
    except Exception as e:
        log.warning("[herosms] request_another failed: %s", e)
    return aid


def _herosms_cancel(api_key: str, aid: str) -> None:
    try:
        _herosms_api(api_key, "setStatus", {"id": aid, "status": "8"})
    except Exception:
        pass


def _herosms_done(api_key: str, aid: str) -> None:
    try:
        _herosms_api(api_key, "setStatus", {"id": aid, "status": "6"})
    except Exception:
        pass


# ---------------------------------------------------------------------------
# 5sim.net
# ---------------------------------------------------------------------------

def _fivesim_cfg() -> tuple[str, str, str]:
    country = os.environ.get("OPAI_5SIM_COUNTRY", "indonesia").strip() or "indonesia"
    operator = os.environ.get("OPAI_5SIM_OPERATOR", "any").strip() or "any"
    product = os.environ.get("OPAI_5SIM_PRODUCT", "gojek").strip() or "gojek"
    return country, operator, product


def _fivesim_request(token: str, path: str, *, retries: int = 3) -> dict[str, Any] | None:
    url = f"{FIVESIM_API}{path}"
    headers = {"Authorization": f"Bearer {token}", "Accept": "application/json"}
    last_err: Exception | None = None
    for i in range(1, retries + 1):
        try:
            s = tls_client.Session(client_identifier="chrome_120")
            r = s.get(url, headers=headers, timeout_seconds=30)
            text = (r.text or "").strip()
            if r.status_code >= 400:
                log.warning("[5sim] %s -> %d %s", path, r.status_code, text[:200])
                if text == "no free phones":
                    return None
                if r.status_code in (400, 503):
                    return None
            if not text:
                return None
            if text == "no free phones":
                return None
            try:
                return r.json()
            except Exception:
                log.warning("[5sim] non-JSON response: %s", text[:200])
                return None
        except Exception as e:
            last_err = e
            log.debug("[5sim] %s attempt %d: %s", path, i, e)
            if i < retries:
                time.sleep(3)
    if last_err:
        raise RuntimeError(f"5sim GET {path} failed: {last_err}")
    return None


def _fivesim_phone_digits(phone: str) -> str:
    digits = re.sub(r"\D", "", phone or "")
    if digits.startswith("62"):
        return digits[2:]
    return digits.lstrip("0")


def _fivesim_get_number(token: str, attempts: int = 15, delay: float = 4.0) -> tuple[str | None, str | None]:
    country, operator, product = _fivesim_cfg()
    path = f"/user/buy/activation/{country}/{operator}/{product}?reuse=1"
    for i in range(1, attempts + 1):
        data = _fivesim_request(token, path)
        log.info("[5sim] buy: %s", json.dumps(data, ensure_ascii=False)[:300] if data else "null")
        if data and data.get("id") and data.get("phone"):
            phone = str(data["phone"]).strip()
            if not phone.startswith("+"):
                phone = f"+{phone.lstrip('+')}"
            order_id = str(data["id"])
            _seen_sms_count[order_id] = 0
            return phone, order_id
        if i == attempts:
            log.warning("[5sim] buy failed after %d attempts", attempts)
            return None, None
        log.info("[5sim] no number, retry %d/%d in %.0fs", i, attempts, delay)
        time.sleep(delay)
    return None, None


def _fivesim_check(token: str, order_id: str) -> dict[str, Any]:
    data = _fivesim_request(token, f"/user/check/{order_id}", retries=2)
    return data or {}


def _fivesim_wait_code(token: str, order_id: str, timeout: int = SMS_TIMEOUT) -> str | None:
    seen = _seen_sms_count.get(str(order_id), 0)
    deadline = time.time() + timeout
    while time.time() < deadline:
        data = _fivesim_check(token, order_id)
        status = str(data.get("status") or "").upper()
        if status == "CANCELED":
            log.warning("[5sim] order %s cancelled", order_id)
            return None
        sms_list = data.get("sms") or []
        if isinstance(sms_list, list) and len(sms_list) > seen:
            entry = sms_list[-1]
            code = entry.get("code") if isinstance(entry, dict) else ""
            text = entry.get("text", "") if isinstance(entry, dict) else str(entry)
            _seen_sms_count[str(order_id)] = len(sms_list)
            otp = _extract_otp(str(code or text))
            if otp:
                return otp
        time.sleep(5)
    return None


def _fivesim_reuse(token: str, phone: str) -> str | None:
    _, _, product = _fivesim_cfg()
    number = _fivesim_phone_digits(phone)
    data = _fivesim_request(token, f"/user/reuse/{product}/{number}")
    if data and data.get("id"):
        order_id = str(data["id"])
        _seen_sms_count[order_id] = 0
        log.info("[5sim] reuse %s -> order %s", phone, order_id)
        return order_id
    log.warning("[5sim] reuse failed for %s: %s", phone, data)
    return None


def _fivesim_request_another(token: str, aid: str, phone: str = "") -> str:
    if phone:
        new_id = _fivesim_reuse(token, phone)
        return new_id or aid
    data = _fivesim_check(token, aid)
    sms_list = data.get("sms") or []
    _seen_sms_count[str(aid)] = len(sms_list) if isinstance(sms_list, list) else 0
    log.info("[5sim] request_another order=%s seen=%d", aid, _seen_sms_count[str(aid)])
    return aid


def _fivesim_cancel(token: str, aid: str) -> None:
    try:
        _fivesim_request(token, f"/user/cancel/{aid}", retries=1)
    except Exception:
        pass


def _fivesim_done(token: str, aid: str) -> None:
    try:
        _fivesim_request(token, f"/user/finish/{aid}", retries=1)
    except Exception:
        pass


def fivesim_balance(token: str) -> dict[str, Any]:
    return _fivesim_request(token, "/user/profile") or {}


# ---------------------------------------------------------------------------
# Public API (provider-agnostic)
# ---------------------------------------------------------------------------

def sms_get_number(api_key: str, attempts: int = 15, delay: float = 4.0) -> tuple[str | None, str | None]:
    if sms_provider() == "5sim":
        return _fivesim_get_number(api_key, attempts=attempts, delay=delay)
    return _herosms_get_number(api_key, attempts=attempts, delay=delay)


def sms_wait_code(api_key: str, aid: str, timeout: int = SMS_TIMEOUT) -> str | None:
    if sms_provider() == "5sim":
        return _fivesim_wait_code(api_key, aid, timeout=timeout)
    return _herosms_wait_code(api_key, aid, timeout=timeout)


def sms_request_another(api_key: str, aid: str, phone: str = "") -> str:
    """Prepare for next OTP. Returns order id to poll (may change on 5sim reuse)."""
    if sms_provider() == "5sim":
        return _fivesim_request_another(api_key, aid, phone=phone)
    return _herosms_request_another(api_key, aid, phone=phone)


def sms_cancel(api_key: str, aid: str) -> None:
    if sms_provider() == "5sim":
        _fivesim_cancel(api_key, aid)
    else:
        _herosms_cancel(api_key, aid)


def sms_done(api_key: str, aid: str) -> None:
    if sms_provider() == "5sim":
        _fivesim_done(api_key, aid)
    else:
        _herosms_done(api_key, aid)


def sms_reactivate(api_key: str, activation_id: str, phone: str = "") -> str | None:
    """Extend/reopen number for more SMS. Returns new order id if applicable."""
    if sms_provider() == "5sim":
        if phone:
            return _fivesim_reuse(api_key, phone)
        return activation_id or None
    try:
        s = tls_client.Session(client_identifier="chrome_120")
        r = s.post(f"{HEROSMS_API}/stubs/handler_api.php", params={
            "api_key": api_key, "action": "reactivate", "id": activation_id,
        }, timeout_seconds=15)
        log.info("[herosms] reactivate aid=%s -> %d: %s", activation_id, r.status_code, r.text[:200])
        if r.status_code == 200:
            data = r.json()
            new_aid = str(data.get("activationId", ""))
            if new_aid:
                return new_aid
    except Exception as e:
        log.warning("[herosms] reactivate aid=%s failed: %s", activation_id, e)
    return None


# Back-compat for deferred cancel loop
def sms_api(api_key: str, action: str, params: dict | None = None, retries: int = 3) -> str:
    if sms_provider() == "5sim":
        if action == "setStatus" and params and params.get("status") == "8":
            sms_cancel(api_key, str(params.get("id", "")))
            return "CANCELED"
        raise RuntimeError(f"sms_api({action}) not supported for 5sim")
    return _herosms_api(api_key, action, params, retries=retries)


# ========== API Error Helpers ==========

def is_waf_block(result: dict) -> bool:
    body = result.get("body", {})
    if isinstance(body, dict) and "raw" in body:
        return "WAF Block Page" in body["raw"]
    return False


def is_rate_limited(result: dict) -> bool:
    errors = result.get("body", {}).get("errors", [])
    if errors:
        code = errors[0].get("code", "")
        return "ratelimit" in code.lower() or "rate_limit" in code.lower()
    return result.get("status") == 429


def get_error_code(result: dict) -> str:
    errors = result.get("body", {}).get("errors", [])
    return errors[0].get("code", "") if errors else ""


def api_call_with_retry(fn, *args, max_retries: int = 2, **kwargs) -> dict:
    """Retry API call on WAF block or transient errors."""
    result = {}
    for attempt in range(max_retries + 1):
        result = fn(*args, **kwargs)
        if result["status"] in (200, 201, 204):
            return result
        if is_waf_block(result):
            if attempt < max_retries:
                wait = 5 * (attempt + 1)
                log.warning("WAF blocked, retrying in %ds... (%d/%d)", wait, attempt + 1, max_retries)
                time.sleep(wait)
                continue
        if is_rate_limited(result):
            if attempt < max_retries:
                wait = 30 * (attempt + 1)
                log.warning("Rate limited, retrying in %ds...", wait)
                time.sleep(wait)
                continue
        return result
    return result
