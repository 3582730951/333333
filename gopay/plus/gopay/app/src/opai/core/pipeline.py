"""
Three-project pipeline: PP自动注册 (ChatGPT) → Midtrans checkout → opai (GoPay pay).

Projects:
  1. PP自动注册     — ChatGPT 注册，产出 access_token（MySQL / JSON 导出）
  2. plus_gopay_links — access_token → IDR Stripe → Midtrans snap URL
  3. gopay/opai     — GoPay 注册 + 1 Rp + Midtrans 协议付款
"""
from __future__ import annotations

import json
import logging
import os
import subprocess
import sys
import time
from pathlib import Path
from typing import Any, Optional

log = logging.getLogger(__name__)

_GOPAY_ROOT = Path(__file__).resolve().parents[4]  # .../gopay
_REPO_ROOT = _GOPAY_ROOT.parent
_PLUS_LINKS = _REPO_ROOT / "plus_gopay_links"
_CHECKOUT_SCRIPT = _PLUS_LINKS / "checkout_to_midtrans.py"
_DEFAULT_PIPELINE_CONFIG = _GOPAY_ROOT / "config" / "pipeline.json"


def _load_json(path: Path) -> dict:
    with open(path, encoding="utf-8") as f:
        return json.load(f)


def load_pipeline_config(path: str = "") -> dict:
    cfg_path = Path(path or os.environ.get("OPAI_PIPELINE_CONFIG", "") or _DEFAULT_PIPELINE_CONFIG)
    if not cfg_path.exists():
        return {}
    return _load_json(cfg_path)


_env_applied = False


def apply_pipeline_env(path: str = "") -> dict:
    """Load config/pipeline.json into os.environ (only unset keys). Mirrors scripts/load_env.ps1."""
    global _env_applied
    cfg_path = Path(path or os.environ.get("OPAI_PIPELINE_CONFIG", "") or _DEFAULT_PIPELINE_CONFIG)
    if not cfg_path.exists():
        return {}

    cfg = _load_json(cfg_path)
    root = cfg_path.parent.parent

    def _set(key: str, val: str) -> None:
        if val and not os.environ.get(key):
            os.environ[key] = val

    _set("OPAI_PIPELINE_CONFIG", str(cfg_path))

    mysql = cfg.get("pp_mysql") or {}
    _set("PP_MYSQL_HOST", str(mysql.get("host") or ""))
    _set("PP_MYSQL_PORT", str(mysql.get("port") or ""))
    _set("PP_MYSQL_USER", str(mysql.get("user") or ""))
    _set("PP_MYSQL_PASSWORD", str(mysql.get("password") or ""))
    _set("PP_MYSQL_DATABASE", str(mysql.get("database") or ""))

    key_rel = str(cfg.get("herosms_api_key_file") or "config/herosms_api_key")
    key_file = root / key_rel.replace("/", os.sep)
    if key_file.exists():
        _set("OPAI_HEROSMS_API_KEY_FILE", str(key_file))

    provider = str(cfg.get("sms_provider") or "herosms").strip().lower()
    _set("OPAI_SMS_PROVIDER", provider)
    fivesim_rel = str(cfg.get("fivesim_api_token_file") or "config/fivesim_api_token")
    fivesim_file = root / fivesim_rel.replace("/", os.sep)
    if fivesim_file.exists():
        _set("OPAI_5SIM_API_TOKEN_FILE", str(fivesim_file))
    _set("OPAI_5SIM_COUNTRY", str(cfg.get("fivesim_country") or "indonesia"))
    _set("OPAI_5SIM_OPERATOR", str(cfg.get("fivesim_operator") or "any"))
    _set("OPAI_5SIM_PRODUCT", str(cfg.get("fivesim_product") or "gojek"))

    clip_rel = str(cfg.get("cliproxy_url_file") or "config/cliproxy.url")
    clip_file = root / clip_rel.replace("/", os.sep)
    if clip_file.exists():
        _set("OPAI_CLIPROXY_API_URL", clip_file.read_text(encoding="utf-8").strip())
    _set("OPAI_CLIPROXY_ENABLED", "1")
    _set("OPAI_PROXY_CHAIN_ENABLED", "1")
    upstream = str(cfg.get("gopay_proxy_chain_upstream") or "http://127.0.0.1:10809")
    _set("OPAI_PROXY_CHAIN_UPSTREAM", upstream)
    regions = cfg.get("cliproxy_regions")
    if isinstance(regions, list) and regions:
        _set("OPAI_CLIPROXY_REGIONS", ",".join(str(r) for r in regions))
    elif cfg.get("cliproxy_regions"):
        _set("OPAI_CLIPROXY_REGIONS", str(cfg.get("cliproxy_regions")))
    _set("OPAI_CHECKOUT_PROXY", str(cfg.get("chatgpt_checkout_proxy") or upstream))

    inbox = cfg.get("payment_inbox") or {}
    if inbox.get("url"):
        _set("OPAI_PAYMENT_INBOX_BASE_URL", str(inbox["url"]))
    elif inbox.get("host") and inbox.get("port"):
        _set("OPAI_PAYMENT_INBOX_BASE_URL", f"http://{inbox['host']}:{inbox['port']}")
    _set("OPAI_PAYMENT_INBOX_BASIC_USER", str(inbox.get("user") or ""))
    _set("OPAI_PAYMENT_INBOX_BASIC_PASS", str(inbox.get("pass") or ""))
    _set("OPAI_PAYMENT_INBOX_HOST", str(inbox.get("host") or ""))
    _set("OPAI_PAYMENT_INBOX_PORT", str(inbox.get("port") or ""))

    acct_rel = str(cfg.get("gopay_accounts_file") or "")
    if acct_rel:
        _set("OPAI_GOPAY_ACCOUNTS_FILE", str(root / acct_rel.replace("/", os.sep)))
    env_rel = str(cfg.get("envelope_links_file") or "")
    if env_rel:
        _set("OPAI_GOPAY_ENVELOPE_LINKS_FILE", str(root / env_rel.replace("/", os.sep)))

    _set("OPAI_GOPAY_DEFAULT_PIN", str(cfg.get("default_pin") or ""))
    if cfg.get("gopay_welcome_wait_sec") is not None:
        _set("OPAI_GOPAY_WELCOME_WAIT_SEC", str(cfg.get("gopay_welcome_wait_sec")))
    if cfg.get("gopay_balance_wait_sec") is not None:
        _set("OPAI_GOPAY_BALANCE_WAIT_SEC", str(cfg.get("gopay_balance_wait_sec")))
    if cfg.get("gopay_welcome_wait_transfer_sec") is not None:
        _set("OPAI_GOPAY_WELCOME_WAIT_TRANSFER_SEC", str(cfg.get("gopay_welcome_wait_transfer_sec")))
    master_rel = str(cfg.get("gopay_master_file") or "")
    if master_rel:
        _set("OPAI_GOPAY_MASTER_FILE", str(root / master_rel.replace("/", os.sep)))
    if cfg.get("gopay_fund_transfer") is False:
        _set("OPAI_GOPAY_FUND_TRANSFER", "0")
    _set("OPAI_PAYMENT_INBOX_PATH", str(root / "config" / "payment_inbox.json"))

    _env_applied = True
    return cfg


def load_chatgpt_token(source: str = "", *, email: str = "") -> dict[str, str]:
    """Load {access_token, email} from JSON file or inline token string."""
    source = (source or os.environ.get("OPAI_CHATGPT_TOKEN_FILE", "")).strip()
    if not source:
        raise ValueError("ChatGPT token source required (--token-file or OPAI_CHATGPT_TOKEN_FILE)")

    path = Path(source)
    if path.exists():
        data = _load_json(path)
        if isinstance(data, str):
            return {"access_token": data.strip(), "email": email}
        if isinstance(data, dict):
            tok = str(
                data.get("access_token")
                or data.get("accessToken")
                or data.get("token")
                or ""
            ).strip()
            em = email or str(data.get("email") or data.get("account_email") or "").strip()
            if not tok:
                raise ValueError(f"No access_token in {path}")
            return {"access_token": tok, "email": em}
        raise ValueError(f"Unsupported token file format: {path}")

    # Treat as raw token string
    if len(source) > 100 and source.startswith("eyJ"):
        return {"access_token": source, "email": email}
    raise FileNotFoundError(f"Token file not found: {source}")


def checkout_to_midtrans(
    access_token: str,
    *,
    email: str = "",
    checkout_proxy: str = "",
) -> dict[str, Any]:
    """Run plus_gopay_links/checkout_to_midtrans.py subprocess."""
    if not _CHECKOUT_SCRIPT.exists():
        raise FileNotFoundError(f"Missing checkout script: {_CHECKOUT_SCRIPT}")

    cmd = [
        sys.executable,
        str(_CHECKOUT_SCRIPT),
        "--access-token",
        access_token,
    ]
    if email:
        cmd.extend(["--email", email])
    if checkout_proxy:
        cmd.extend(["--proxy", checkout_proxy])

    log.info("Running checkout bridge: %s", _CHECKOUT_SCRIPT.name)
    proc = subprocess.run(
        cmd,
        cwd=str(_PLUS_LINKS),
        capture_output=True,
        text=True,
        encoding="utf-8",
        errors="replace",
    )
    stdout = (proc.stdout or "").strip()
    stderr = (proc.stderr or "").strip()
    if proc.returncode != 0:
        err_body = stdout or stderr or f"exit {proc.returncode}"
        try:
            err_json = json.loads(stdout or stderr)
            err_body = err_json.get("error") or err_body
        except Exception:
            pass
        raise RuntimeError(f"Checkout failed: {err_body}")

    try:
        return json.loads(stdout)
    except json.JSONDecodeError as e:
        raise RuntimeError(f"Invalid checkout JSON: {stdout[:300]}") from e


def enqueue_gopay_job(
    *,
    account_email: str,
    midtrans_url: str,
    account_name: str = "",
    checkout_url: str = "",
    notes: str = "",
    inbox_url: str = "",
    inbox_user: str = "",
    inbox_pass: str = "",
) -> dict[str, Any]:
    from .payment_inbox import PaymentInboxClient

    cfg = load_pipeline_config()
    inbox_cfg = cfg.get("payment_inbox") or {}
    base = inbox_url or os.environ.get("OPAI_PAYMENT_INBOX_BASE_URL", "")
    if not base and inbox_cfg.get("url"):
        base = str(inbox_cfg["url"])
    elif not base and inbox_cfg.get("host") and inbox_cfg.get("port"):
        base = f"http://{inbox_cfg['host']}:{inbox_cfg['port']}"
    if not base:
        raise ValueError("Payment Inbox URL required (OPAI_PAYMENT_INBOX_BASE_URL)")

    user = inbox_user or os.environ.get("OPAI_PAYMENT_INBOX_BASIC_USER", "") or str(inbox_cfg.get("user") or "")
    pwd = inbox_pass or os.environ.get("OPAI_PAYMENT_INBOX_BASIC_PASS", "") or str(inbox_cfg.get("pass") or "")
    auth = (user, pwd) if user and pwd else None

    client = PaymentInboxClient(base_url=base, basic_auth=auth)
    job = client.push_job(
        account_name=account_name or account_email.split("@")[0],
        account_email=account_email,
        plan_kind="plus",
        checkout_url=checkout_url,
        provider="gopay",
        provider_url=midtrans_url,
        notes=notes or "pipeline enqueue",
    )
    log.info("Inbox job created: id=%s email=%s", job.get("id", "?")[:8], account_email)
    return job


def pay_with_gopay_account(
    midtrans_url: str,
    phone_local: str,
    pin: str,
    api_key: str,
    activation_id: str,
    proxy: str = "",
) -> dict[str, Any]:
    from .gopay_payment_protocol import GoPayPayment
    from .sms_helpers import sms_request_another, sms_wait_code

    payment = GoPayPayment(proxy=proxy)
    otp_poll_aid = activation_id

    def prepare_otp(ph: str) -> None:
        nonlocal otp_poll_aid
        if activation_id and api_key:
            otp_poll_aid = sms_request_another(api_key, activation_id, phone=ph)
            time.sleep(2)

    def wait_otp(_ph: str, timeout: int = 120) -> Optional[str]:
        if otp_poll_aid and api_key:
            return sms_wait_code(api_key, otp_poll_aid, timeout=timeout)
        return None

    return payment.pay(
        midtrans_url=midtrans_url,
        phone=phone_local.lstrip("+").removeprefix("62"),
        country_code="62",
        pin=pin,
        prepare_otp=prepare_otp if activation_id else None,
        wait_otp=wait_otp if activation_id else None,
    )


def run_single(
    token_file: str = "",
    *,
    gpt: Optional[dict[str, Any]] = None,
    pool_id: Optional[int] = None,
    pin: str = "147258",
    api_key: str = "",
    register: bool = True,
    resume_phone: str = "",
    checkout_proxy: str = "",
    gopay_proxy: str = "",
    skip_fund: bool = False,
    enqueue_only: bool = False,
    email: str = "",
    mark_pool_on_success: bool = True,
) -> dict[str, Any]:
    """End-to-end: ChatGPT token → Midtrans → (register) GoPay → pay."""
    from .gopay_protocol_worker import (
        MIN_BALANCE_RP,
        _fund_account,
        _get_envelope_did,
        _make_proxy,
        _register_with_retries,
        _resume_account,
    )

    cfg = load_pipeline_config()
    checkout_proxy = checkout_proxy or cfg.get("chatgpt_checkout_proxy") or os.environ.get("OPAI_CHECKOUT_PROXY", "")
    fixed_proxy = str(
        gopay_proxy or cfg.get("gopay_indonesia_proxy") or os.environ.get("OPAI_GOPAY_REGISTER_PROXY", "")
    ).strip()

    if gpt is None:
        if not token_file:
            raise ValueError("token_file or gpt dict required")
        gpt = load_chatgpt_token(token_file, email=email)
    if pool_id is None and gpt.get("pool_id"):
        pool_id = int(gpt["pool_id"])

    account_email = gpt.get("email") or email
    try:
        checkout = checkout_to_midtrans(
            gpt["access_token"],
            email=account_email,
            checkout_proxy=checkout_proxy,
        )
    except Exception as e:
        if pool_id is not None:
            from .pp_pool import mark_result
            mark_result(pool_id, success=False, error=f"checkout: {e}")
        raise
    midtrans_url = checkout["midtrans_url"]
    result: dict[str, Any] = {
        "step": "checkout",
        "midtrans_url": midtrans_url,
        "checkout_url": checkout.get("checkout_url"),
        "email": account_email,
        "pool_id": pool_id,
    }

    if enqueue_only:
        if not account_email:
            raise ValueError("enqueue requires account email")
        job = enqueue_gopay_job(
            account_email=account_email,
            midtrans_url=midtrans_url,
            checkout_url=checkout.get("checkout_url") or "",
            notes=(f"pool_id={pool_id}" if pool_id else "pipeline enqueue"),
        )
        result["step"] = "enqueued"
        result["job"] = job
        return result

    account: Optional[dict] = None
    try:
        if resume_phone:
            gopay_proxy = fixed_proxy or _make_proxy(required=True)
            log.info("GoPay/Midtrans proxy: %s", gopay_proxy.split("@")[-1] if gopay_proxy else "none")
            account = _resume_account(resume_phone, gopay_proxy)
            if not account:
                raise RuntimeError(f"Cannot resume GoPay account: {resume_phone}")
        elif register:
            if not api_key:
                raise ValueError("SMS API token required for GoPay registration")
            account = _register_with_retries(
                api_key, pin, _get_envelope_did(), skip_fund=skip_fund, proxy=fixed_proxy,
            )
            if not account:
                raise RuntimeError("GoPay registration failed")
            gopay_proxy = account.get("proxy") or fixed_proxy or _make_proxy(required=True)
            log.info("GoPay/Midtrans proxy: %s", gopay_proxy.split("@")[-1] if gopay_proxy else "none")
        else:
            raise ValueError("Need --resume-phone or GoPay registration")

        result["gopay_phone"] = account["phone"]
        result["gopay_local"] = account["local"]

        if not skip_fund:
            bal = account.get("balance_rp", -1)
            funded_via = str(account.get("funded_via") or "")
            if isinstance(bal, int) and bal >= MIN_BALANCE_RP and funded_via:
                log.info(
                    "Registration already funded: %d Rp  funded_via: %s",
                    bal, funded_via,
                )
                result["balance_rp"] = bal
                result["funded_via"] = funded_via
            else:
                fund = _fund_account(
                    account["client"],
                    account["phone"],
                    _get_envelope_did(),
                    proxy=gopay_proxy,
                )
                result["balance_rp"] = fund["balance"]
                result["funded_via"] = fund["funded_via"]
                if fund["balance"] < MIN_BALANCE_RP:
                    log.warning(
                        "Balance %d < %d Rp — payment may fail  funded_via: %s",
                        fund["balance"], MIN_BALANCE_RP, fund["funded_via"],
                    )

        pay_result = pay_with_gopay_account(
            midtrans_url,
            account["local"],
            pin,
            api_key,
            account.get("aid", ""),
            proxy=gopay_proxy,
        )
        result["step"] = "paid" if pay_result.get("success") else "pay_failed"
        result["payment"] = pay_result

        if pool_id is not None:
            if pay_result.get("success") and mark_pool_on_success:
                from .pp_pool import mark_result
                mark_result(pool_id, success=True)
            elif not pay_result.get("success"):
                from .pp_pool import mark_result
                mark_result(pool_id, success=False, error=pay_result.get("detail", "pay failed"))

        return result
    except Exception as e:
        if pool_id is not None:
            from .pp_pool import mark_result
            mark_result(pool_id, success=False, error=str(e))
        raise


def run_from_pool(
    *,
    email: str = "",
    count: int = 1,
    enqueue_only: bool = False,
    pin: str = "147258",
    api_key: str = "",
    resume_phone: str = "",
    checkout_proxy: str = "",
    skip_fund: bool = False,
    require_free_offer: bool = True,
    lock: bool = True,
) -> list[dict[str, Any]]:
    """Claim registered-but-not-Plus accounts from PP pool and run pipeline."""
    from .pp_pool import account_to_token_dict, claim_next, release_claim

    results = []
    for _ in range(max(1, count)):
        acct = claim_next(email=email, require_free_offer=require_free_offer, lock=lock)
        if not acct:
            log.warning("No pending pool account available")
            break
        gpt = account_to_token_dict(acct)
        try:
            r = run_single(
                gpt=gpt,
                pool_id=acct.id if lock else None,
                pin=pin,
                api_key=api_key,
                register=not resume_phone and not enqueue_only,
                resume_phone=resume_phone,
                checkout_proxy=checkout_proxy,
                skip_fund=skip_fund,
                enqueue_only=enqueue_only,
                mark_pool_on_success=lock and not enqueue_only,
            )
            results.append(r)
            if enqueue_only and lock:
                # Keep activating until worker completes — inbox job has pool_id in notes
                pass
        except Exception as e:
            if lock and enqueue_only:
                release_claim(acct.id)
            results.append({"step": "error", "email": acct.email, "pool_id": acct.id, "error": str(e)})
            if count == 1:
                raise
    return results
