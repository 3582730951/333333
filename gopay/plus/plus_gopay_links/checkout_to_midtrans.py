#!/usr/bin/env python3
"""Generate Midtrans snap URL from ChatGPT access_token (IDR + plus-1-month-free).

Used by gopay/opai pipeline — stops before GoPay linking/payment.
Requires: pip install curl_cffi requests  (see plus_gopay_links/requirements if any)
"""
from __future__ import annotations

import argparse
import json
import sys

from gopay import (
    DEFAULT_STRIPE_PK,
    GoPayCharger,
    GoPayError,
    _build_chatgpt_session,
)


def resolve_midtrans_url(
    access_token: str,
    *,
    proxy: str = "",
    email: str = "",
    stripe_pk: str = "",
) -> dict:
    auth_cfg = {"access_token": access_token.strip()}
    session = _build_chatgpt_session(auth_cfg)
    billing = {"email": email, "name": "John Doe", "country": "US"}
    gopay_stub = {"country_code": "62", "phone_number": "8000000000", "pin": "000000"}
    charger = GoPayCharger(
        session,
        gopay_stub,
        otp_provider=lambda: "",
        proxy=proxy or None,
        log=lambda msg: print(msg, flush=True, file=sys.stderr),
    )
    pk = stripe_pk or DEFAULT_STRIPE_PK
    if not pk:
        raise GoPayError("missing required runtime config: --stripe-pk or STRIPE_PUBLISHABLE_KEY")
    cs_id = charger._chatgpt_create_checkout()
    pm_id = charger._stripe_create_pm(cs_id, pk, billing)
    confirm_data = charger._stripe_confirm(cs_id, pm_id, pk)
    redirect_url = charger._extract_redirect_to_url(confirm_data)
    if redirect_url:
        snap_token = charger._fetch_pm_redirect_snap_token(redirect_url)
    else:
        charger._chatgpt_approve(cs_id)
        snap_token = charger._follow_redirect_to_midtrans(cs_id, pk)
    midtrans_url = GoPayCharger._midtrans_redirection_url(snap_token)
    return {
        "ok": True,
        "cs_id": cs_id,
        "snap_token": snap_token,
        "midtrans_url": midtrans_url,
        "checkout_url": f"https://checkout.stripe.com/c/pay/{cs_id}",
        "provider": "gopay",
    }


def main() -> None:
    parser = argparse.ArgumentParser(description="ChatGPT token → Midtrans snap URL (IDR GoPay checkout)")
    parser.add_argument("--access-token", default="", help="ChatGPT Bearer access_token")
    parser.add_argument("--token-file", default="", help="JSON file: {access_token, email?}")
    parser.add_argument("--email", default="", help="Billing email (optional)")
    parser.add_argument("--proxy", default="", help="HTTP/SOCKS proxy for ChatGPT+Stripe (Japan IP recommended)")
    parser.add_argument("--stripe-pk", default="", help="Override Stripe publishable key")
    args = parser.parse_args()

    access_token = args.access_token.strip()
    email = args.email.strip()
    if args.token_file:
        with open(args.token_file, encoding="utf-8") as f:
            data = json.load(f)
        if isinstance(data, dict):
            access_token = access_token or str(data.get("access_token") or data.get("accessToken") or "").strip()
            email = email or str(data.get("email") or data.get("account_email") or "").strip()
        elif isinstance(data, str):
            access_token = access_token or data.strip()

    if not access_token:
        print(json.dumps({"ok": False, "error": "missing access_token"}), file=sys.stderr)
        sys.exit(2)

    try:
        result = resolve_midtrans_url(
            access_token,
            proxy=args.proxy.strip(),
            email=email,
            stripe_pk=args.stripe_pk.strip(),
        )
        print(json.dumps(result, ensure_ascii=False, indent=2))
    except GoPayError as e:
        print(json.dumps({"ok": False, "error": str(e)}, ensure_ascii=False), file=sys.stderr)
        sys.exit(1)
    except Exception as e:
        print(json.dumps({"ok": False, "error": f"{type(e).__name__}: {e}"}, ensure_ascii=False), file=sys.stderr)
        sys.exit(1)


if __name__ == "__main__":
    main()
