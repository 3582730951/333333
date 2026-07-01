from __future__ import annotations

import argparse
import json
import logging
import os


def _resolve_herosms_api_key(cli_key: str = "") -> str:
    """Back-compat alias — resolves token for active SMS provider."""
    from opai.core.sms_helpers import resolve_sms_token
    return resolve_sms_token(cli_key)


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="opai", description="GoPay protocol automation (no browser)")
    parser.add_argument("--debug", action="store_true", help="enable debug logging")
    sub = parser.add_subparsers(dest="command")

    # === worker (protocol-based, no browser) ===
    p_worker = sub.add_parser("worker", help="GoPay protocol worker (register + pay)")
    worker_sub = p_worker.add_subparsers(dest="worker_command")

    p_w_run = worker_sub.add_parser("run", help="Start parallel register+pay worker threads")
    p_w_run.add_argument("--workers", type=int, default=3, help="Number of parallel workers")
    p_w_run.add_argument("--pin", default="147258", help="GoPay PIN to set")
    p_w_run.add_argument("--poll", type=float, default=10, help="Inbox poll interval (seconds)")
    p_w_run.add_argument("--api-key", default="", help="Hero-SMS API key")
    p_w_run.add_argument("--resume", nargs="+", metavar="PHONE", help="Resume from existing accounts")

    p_w_dry = worker_sub.add_parser("dry-run", help="Register one account only (no payment)")
    p_w_dry.add_argument("--pin", default="147258", help="GoPay PIN to set")
    p_w_dry.add_argument("--api-key", default="", help="Hero-SMS API key")
    p_w_dry.add_argument("--register-attempts", type=int, default=5, help="Max number switches on OTP failure")
    p_w_dry.add_argument("--keep-sms-open", action="store_true", help="Keep SMS activation open for payment OTP")

    p_w_register = worker_sub.add_parser("register", help="Register a single GoPay account")
    p_w_register.add_argument("--pin", default="147258", help="GoPay PIN to set")
    p_w_register.add_argument("--api-key", default="", help="Hero-SMS API key")
    p_w_register.add_argument("--proxy", default="", help="Proxy URL")
    p_w_register.add_argument("--keep-sms-open", action="store_true", help="Keep SMS activation open for payment OTP")

    p_w_balance = worker_sub.add_parser("balance", help="Check balance of a saved account")
    p_w_balance.add_argument("phone", help="Phone number")
    p_w_balance.add_argument("--proxy", default="", help="Proxy URL")

    p_w_fund = worker_sub.add_parser("fund", help="Claim 1 Rp for a saved account (envelope/welcome)")
    p_w_fund.add_argument("phone", help="Phone number")
    p_w_fund.add_argument("--proxy", default="", help="Proxy URL")

    p_w_xfer = worker_sub.add_parser("fund-transfer", help="Test master → child GoPay transfer")
    p_w_xfer.add_argument("phone", help="Child phone (saved account or +62...)")
    p_w_xfer.add_argument("--proxy", default="", help="Proxy URL")
    p_w_xfer.add_argument("--amount", type=int, default=0, help="Override transfer amount (default from master config)")

    p_w_login = worker_sub.add_parser("login-master", help="Login master GoPay account and save tokens")
    p_w_login.add_argument("--config", default="", help="gopay_master.json path")
    p_w_login.add_argument("--proxy", default="", help="Proxy URL")
    p_w_login.add_argument("--access-token", default="", help="Paste access_token (skip SMS login)")
    p_w_login.add_argument("--refresh-token", default="", help="Paste refresh_token")
    p_w_login.add_argument("--customer-id", default="", help="Optional User-uuid")
    p_w_login.add_argument("--from-har", default="", help="Extract tokens from HAR file")
    p_w_login.add_argument("--guide", action="store_true", help="Print HAR capture guide")

    # === pay (protocol-based single payment test) ===
    p_pay = sub.add_parser("pay", help="Run a single protocol payment against Midtrans URL")
    p_pay.add_argument("midtrans_url", help="Midtrans snap redirect URL")
    p_pay.add_argument("--phone", required=True, help="GoPay local phone (no +62)")
    p_pay.add_argument("--pin", required=True, help="6-digit PIN")
    p_pay.add_argument("--proxy", default="", help="Proxy URL")
    p_pay.add_argument("--activation-id", default="", help="Hero-SMS activation_id for payment OTP (3rd SMS)")
    p_pay.add_argument("--api-key", default="", help="Hero-SMS API key (or env OPAI_HEROSMS_API_KEY_FILE)")
    p_pay.add_argument("--reactivate-sms", action="store_true", help="Reactivate SMS order before triggering payment OTP")

    # === pipeline (PP自动注册 + checkout + GoPay) ===
    p_pipe = sub.add_parser("pipeline", help="Cross-project Plus pipeline (ChatGPT → Midtrans → GoPay)")
    pipe_sub = p_pipe.add_subparsers(dest="pipeline_command")

    p_p_checkout = pipe_sub.add_parser("checkout", help="ChatGPT token → Midtrans snap URL")
    p_p_checkout.add_argument("--token-file", required=True, help="JSON: {access_token, email?}")
    p_p_checkout.add_argument("--email", default="", help="Billing email override")
    p_p_checkout.add_argument("--checkout-proxy", default="", help="Proxy for ChatGPT/Stripe (Japan IP)")

    p_p_enqueue = pipe_sub.add_parser("enqueue", help="Push Midtrans job to Payment Inbox for worker")
    p_p_enqueue.add_argument("--token-file", required=True)
    p_p_enqueue.add_argument("--email", default="", help="ChatGPT account email (or in token file)")
    p_p_enqueue.add_argument("--checkout-proxy", default="")

    p_p_run = pipe_sub.add_parser("run-one", help="Single account: checkout + GoPay register/pay")
    p_p_run.add_argument("--token-file", default="", help="JSON token file（与 from-pool 二选一）")
    p_p_run.add_argument("--email", default="", help="ChatGPT email (required if not in token file)")
    p_p_run.add_argument("--pin", default="147258")
    p_p_run.add_argument("--api-key", default="")
    p_p_run.add_argument("--resume-phone", default="", help="Use saved GoPay account instead of registering")
    p_p_run.add_argument("--checkout-proxy", default="")
    p_p_run.add_argument("--skip-fund", action="store_true", help="Skip 1 Rp wait/claim")
    p_p_run.add_argument("--enqueue-only", action="store_true", help="Only checkout + inbox, no GoPay pay")

    p_p_inbox = pipe_sub.add_parser("inbox", help="Start Payment Inbox HTTP server")
    p_p_inbox.add_argument("--host", default="127.0.0.1")
    p_p_inbox.add_argument("--port", type=int, default=8765)

    p_p_pool = pipe_sub.add_parser("pool", help="PP自动注册邮箱池（已注册、未开 Plus）")
    pool_sub = p_p_pool.add_subparsers(dest="pool_command")
    p_pool_stats = pool_sub.add_parser("stats", help="统计可激活数量")
    p_pool_stats.add_argument("--all-offers", action="store_true", help="含未检测/无试用标记的号")
    p_pool_list = pool_sub.add_parser("list", help="列出待激活账号")
    p_pool_list.add_argument("--limit", type=int, default=20)
    p_pool_list.add_argument("--all-offers", action="store_true")

    p_p_from = pipe_sub.add_parser("from-pool", help="从邮箱池取号 → checkout → GoPay（不新注册 ChatGPT）")
    p_p_from.add_argument("--email", default="", help="指定邮箱；默认自动取下一个")
    p_p_from.add_argument("--count", type=int, default=1, help="批量处理数量")
    p_p_from.add_argument("--enqueue-only", action="store_true", help="只入 Inbox，由 worker 付 GoPay")
    p_p_from.add_argument("--pin", default="147258")
    p_p_from.add_argument("--api-key", default="")
    p_p_from.add_argument("--resume-phone", default="", help="复用已有 GoPay 号")
    p_p_from.add_argument("--checkout-proxy", default="")
    p_p_from.add_argument("--skip-fund", action="store_true")
    p_p_from.add_argument("--all-offers", action="store_true", help="不过滤 zero_dollar_offer")
    p_p_from.add_argument("--no-lock", action="store_true", help="只读预览，不写 MySQL 锁定")

    return parser


def cmd_worker_run(args: argparse.Namespace) -> None:
    from opai.core.gopay_protocol_worker import run_worker
    run_worker(
        max_workers=args.workers,
        pin=args.pin,
        poll_interval=args.poll,
        resume_phones=args.resume,
        api_key=args.api_key,
    )


def cmd_worker_dry_run(args: argparse.Namespace) -> None:
    from opai.core.gopay_protocol_worker import (
        _register_with_retries, _get_envelope_did, MIN_BALANCE_RP,
    )
    from opai.core.sms_helpers import sms_done

    api_key = _resolve_herosms_api_key(args.api_key)
    if not api_key:
        raise SystemExit("No API key. Set --api-key, OPAI_HEROSMS_API_KEY, or OPAI_HEROSMS_API_KEY_FILE")
    envelope_did = _get_envelope_did()
    result = _register_with_retries(
        api_key, args.pin, envelope_did, max_attempts=args.register_attempts,
    )
    if result:
        bal = result.get("balance_rp", 0)
        print(f"SUCCESS: {result['phone']} pin={args.pin} balance={bal} Rp")
        if args.keep_sms_open:
            print(f"SMS used: 2/3 (signup + PIN). Activation kept open for payment OTP: {result['aid']}")
        else:
            print("SMS used: 2/3 (signup + PIN). Activation marked done; use --keep-sms-open when continuing to payment.")
        if bal < MIN_BALANCE_RP:
            print(f"WARN: balance < {MIN_BALANCE_RP} Rp — add links to gopay/config/envelope_links.txt")
        if not args.keep_sms_open:
            sms_done(api_key, result["aid"])
    else:
        raise SystemExit("FAILED")


def cmd_worker_register(args: argparse.Namespace) -> None:
    from opai.core.gopay_protocol_worker import _register_one, _make_proxy, _get_envelope_did
    from opai.core.sms_helpers import sms_done

    api_key = _resolve_herosms_api_key(args.api_key)
    if not api_key:
        raise SystemExit("No API key. Set --api-key, OPAI_HEROSMS_API_KEY, or OPAI_HEROSMS_API_KEY_FILE")
    proxy = args.proxy or _make_proxy()
    envelope_did = _get_envelope_did()
    result = _register_one(api_key, args.pin, proxy, envelope_did)
    if result:
        print(json.dumps({
            "phone": result["phone"],
            "pin": args.pin,
            "local": result["local"],
            "activation_id": result["aid"],
            "sms_activation_open": bool(args.keep_sms_open),
        }, indent=2))
        if not args.keep_sms_open:
            sms_done(api_key, result["aid"])
    else:
        raise SystemExit("FAILED")


def cmd_worker_balance(args: argparse.Namespace) -> None:
    from opai.core.gopay_protocol_worker import _resume_account, _check_balance

    account = _resume_account(args.phone, proxy=args.proxy)
    if not account:
        raise SystemExit(f"Account {args.phone} not found")
    bal = _check_balance(account["client"])
    print(json.dumps({"phone": account["phone"], "balance_rp": bal}, indent=2))


def cmd_worker_fund(args: argparse.Namespace) -> None:
    from opai.core.gopay_protocol_worker import (
        _resume_account, _fund_account, _get_envelope_did, MIN_BALANCE_RP,
    )

    account = _resume_account(args.phone, proxy=args.proxy or None)
    if not account:
        raise SystemExit(f"Account {args.phone} not found")
    fund = _fund_account(account["client"], account["phone"], _get_envelope_did(), proxy=args.proxy or "")
    print(json.dumps({
        "phone": account["phone"],
        "balance_rp": fund["balance"],
        "funded_via": fund["funded_via"],
        "ready_for_payment": fund["balance"] >= MIN_BALANCE_RP,
    }, indent=2))
    if fund["balance"] < MIN_BALANCE_RP:
        raise SystemExit(f"Balance {fund['balance']} Rp < {MIN_BALANCE_RP} Rp required for payment")


def cmd_worker_login_master(args: argparse.Namespace) -> None:
    import subprocess
    import sys
    from pathlib import Path

    script = Path(__file__).resolve().parents[4] / "scripts" / "login_gopay_master.py"
    cmd = [sys.executable, str(script)]
    if args.config:
        cmd += ["--config", args.config]
    if args.proxy:
        cmd += ["--proxy", args.proxy]
    if args.access_token:
        cmd += ["--access-token", args.access_token]
    if args.refresh_token:
        cmd += ["--refresh-token", args.refresh_token]
    if args.customer_id:
        cmd += ["--customer-id", args.customer_id]
    if args.from_har:
        cmd += ["--from-har", args.from_har]
    if args.guide:
        cmd += ["--guide"]
    raise SystemExit(subprocess.call(cmd))


def cmd_worker_fund_transfer(args: argparse.Namespace) -> None:
    from opai.core.gopay_protocol_worker import (
        _resume_account, _transfer_from_master, _check_balance, MIN_BALANCE_RP,
        _load_master_config,
    )

    if not _load_master_config():
        raise SystemExit("No master config. Copy config/gopay_master.example.json → config/gopay_master.json")

    child = _resume_account(args.phone, proxy=args.proxy or None)
    child_phone = child["phone"] if child else args.phone
    amount = args.amount if args.amount > 0 else None
    result = _transfer_from_master(child_phone, proxy=args.proxy or "", amount=amount)
    bal = _check_balance(child["client"]) if child else -1
    print(json.dumps({
        "child_phone": child_phone,
        "transfer": result,
        "child_balance_rp": bal,
        "ready_for_payment": bal >= MIN_BALANCE_RP,
    }, indent=2))
    if not result.get("ok"):
        raise SystemExit(f"Transfer failed: {result.get('reason', 'unknown')}")


def _activation_id_for_phone(phone: str) -> str:
    import json as _json
    from opai.core.gopay_protocol_worker import ACCOUNTS_FILE

    if not os.path.exists(ACCOUNTS_FILE):
        return ""
    digits = phone.strip().lstrip("+")
    for entry in _json.loads(open(ACCOUNTS_FILE, encoding="utf-8").read()):
        local = str(entry.get("local", ""))
        full = str(entry.get("phone", "")).lstrip("+")
        if digits == local or digits == full or full.endswith(digits):
            return str(entry.get("activation_id", ""))
    return ""


def cmd_pay(args: argparse.Namespace) -> None:
    import time
    from opai.core.gopay_payment_protocol import GoPayPayment
    from opai.core.gopay_protocol_worker import _make_proxy
    from opai.core.sms_helpers import sms_reactivate, sms_request_another, sms_wait_code

    api_key = _resolve_herosms_api_key(args.api_key)
    aid = args.activation_id.strip() or _activation_id_for_phone(args.phone)
    proxy = args.proxy or _make_proxy()
    otp_poll_aid = aid

    def prepare_otp(ph: str):
        nonlocal aid, otp_poll_aid
        if not aid or not api_key:
            return
        if args.reactivate_sms:
            new_aid = sms_reactivate(api_key, aid, phone=ph)
            if new_aid:
                aid = new_aid
        otp_poll_aid = sms_request_another(api_key, aid, phone=ph)
        time.sleep(2)

    def wait_otp(_ph: str, timeout: int = 120):
        if not otp_poll_aid or not api_key:
            return None
        return sms_wait_code(api_key, otp_poll_aid, timeout=timeout)

    payment = GoPayPayment(proxy=proxy)
    result = payment.pay(
        midtrans_url=args.midtrans_url,
        phone=args.phone,
        country_code="62",
        pin=args.pin,
        prepare_otp=prepare_otp if aid and api_key else None,
        wait_otp=wait_otp if aid and api_key else None,
    )
    print(json.dumps(result, ensure_ascii=False, indent=2))
    if not result.get("success"):
        raise SystemExit(1)


def cmd_pipeline_checkout(args: argparse.Namespace) -> None:
    from opai.core.pipeline import checkout_to_midtrans, load_chatgpt_token

    gpt = load_chatgpt_token(args.token_file, email=args.email)
    out = checkout_to_midtrans(
        gpt["access_token"],
        email=gpt.get("email") or args.email,
        checkout_proxy=args.checkout_proxy,
    )
    print(json.dumps(out, ensure_ascii=False, indent=2))


def cmd_pipeline_enqueue(args: argparse.Namespace) -> None:
    from opai.core.pipeline import checkout_to_midtrans, enqueue_gopay_job, load_chatgpt_token

    gpt = load_chatgpt_token(args.token_file, email=args.email)
    email = args.email or gpt.get("email", "")
    if not email:
        raise SystemExit("--email required (or include email in token file)")
    checkout = checkout_to_midtrans(
        gpt["access_token"],
        email=email,
        checkout_proxy=args.checkout_proxy,
    )
    job = enqueue_gopay_job(
        account_email=email,
        midtrans_url=checkout["midtrans_url"],
        checkout_url=checkout.get("checkout_url") or "",
    )
    print(json.dumps({"checkout": checkout, "job": job}, ensure_ascii=False, indent=2))


def cmd_pipeline_run_one(args: argparse.Namespace) -> None:
    from opai.core.pipeline import run_single

    if not args.token_file:
        raise SystemExit("pipeline run-one 需要 --token-file（或用 pipeline from-pool）")
    api_key = _resolve_herosms_api_key(args.api_key)
    if not args.enqueue_only and not args.resume_phone and not api_key:
        raise SystemExit("Need --api-key for GoPay registration, or --resume-phone / --enqueue-only")
    result = run_single(
        args.token_file,
        pin=args.pin,
        api_key=api_key,
        register=not args.resume_phone and not args.enqueue_only,
        resume_phone=args.resume_phone,
        checkout_proxy=args.checkout_proxy,
        skip_fund=args.skip_fund,
        enqueue_only=args.enqueue_only,
        email=args.email,
    )
    print(json.dumps(result, ensure_ascii=False, indent=2))
    if result.get("step") == "pay_failed":
        raise SystemExit(1)


def cmd_pipeline_pool_stats(args: argparse.Namespace) -> None:
    from opai.core.pp_pool import pool_stats

    print(json.dumps(
        pool_stats(require_free_offer=not args.all_offers),
        ensure_ascii=False, indent=2,
    ))


def cmd_pipeline_pool_list(args: argparse.Namespace) -> None:
    from opai.core.pp_pool import list_pending

    rows = list_pending(limit=args.limit, require_free_offer=not args.all_offers)
    print(json.dumps(rows, ensure_ascii=False, indent=2))


def cmd_pipeline_from_pool(args: argparse.Namespace) -> None:
    from opai.core.pipeline import run_from_pool

    api_key = _resolve_herosms_api_key(args.api_key)
    if not args.enqueue_only and not args.resume_phone and not api_key:
        raise SystemExit("GoPay 注册需要 Hero-SMS；或指定 --resume-phone / --enqueue-only")
    results = run_from_pool(
        email=args.email,
        count=args.count,
        enqueue_only=args.enqueue_only,
        pin=args.pin,
        api_key=api_key,
        resume_phone=args.resume_phone,
        checkout_proxy=args.checkout_proxy,
        skip_fund=args.skip_fund,
        require_free_offer=not args.all_offers,
        lock=not args.no_lock,
    )
    print(json.dumps(results, ensure_ascii=False, indent=2))
    if results and results[-1].get("step") == "pay_failed":
        raise SystemExit(1)
    if not results:
        raise SystemExit("邮箱池无可用账号")


def cmd_pipeline_inbox(args: argparse.Namespace) -> None:
    from pathlib import Path
    from opai.core.payment_inbox import InboxStore, run_inbox_server

    storage = os.environ.get("OPAI_PAYMENT_INBOX_PATH", "")
    store = InboxStore(Path(storage).expanduser().resolve()) if storage else InboxStore()
    run_inbox_server(host=args.host, port=args.port, store=store)


def main() -> None:
    from opai.core.pipeline import apply_pipeline_env

    apply_pipeline_env()
    parser = build_parser()
    args = parser.parse_args()
    logging.basicConfig(
        level=logging.DEBUG if args.debug else logging.INFO,
        format="%(asctime)s [%(levelname)s] %(name)s: %(message)s",
        datefmt="%H:%M:%S",
    )
    if args.command == "worker":
        if args.worker_command == "run":
            cmd_worker_run(args)
        elif args.worker_command == "dry-run":
            cmd_worker_dry_run(args)
        elif args.worker_command == "register":
            cmd_worker_register(args)
        elif args.worker_command == "balance":
            cmd_worker_balance(args)
        elif args.worker_command == "fund":
            cmd_worker_fund(args)
        elif args.worker_command == "fund-transfer":
            cmd_worker_fund_transfer(args)
        elif args.worker_command == "login-master":
            cmd_worker_login_master(args)
        else:
            parser.parse_args(["worker", "--help"])
    elif args.command == "pay":
        cmd_pay(args)
    elif args.command == "pipeline":
        if args.pipeline_command == "checkout":
            cmd_pipeline_checkout(args)
        elif args.pipeline_command == "enqueue":
            cmd_pipeline_enqueue(args)
        elif args.pipeline_command == "run-one":
            cmd_pipeline_run_one(args)
        elif args.pipeline_command == "inbox":
            cmd_pipeline_inbox(args)
        elif args.pipeline_command == "pool":
            if args.pool_command == "stats":
                cmd_pipeline_pool_stats(args)
            elif args.pool_command == "list":
                cmd_pipeline_pool_list(args)
            else:
                parser.parse_args(["pipeline", "pool", "--help"])
        elif args.pipeline_command == "from-pool":
            cmd_pipeline_from_pool(args)
        else:
            parser.parse_args(["pipeline", "--help"])
    else:
        parser.print_help()
