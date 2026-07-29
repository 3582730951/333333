#!/usr/bin/env python3
"""High-concurrency, per-account-isolated Codex account registrar.

Runs N concurrent `reg_v3.py` registrations, each fully isolated:
  - its own cliproxy session id  -> its own residential exit IP
  - proxy region pinned to the SMS country (IP country == phone country)
  - its own plus-addressed email tag
  - its own Chrome profile dir (reg_v3.py uses /tmp/regv3_<rand>)
  - its own hero-sms number (only drawn if OpenAI demands add-phone)

Each successful run writes a ready-to-use ~/.codex/auth.json-style file (with a
refresh_token, via the in-flow PKCE OAuth exchange) so you NEVER have to open
`codex login` in a browser by hand.

Provider credentials and the email source are required via environment variables.

Examples
--------
  # 20 accounts, 5 in parallel, headless, rotating through cheap SMS countries
  HOTMAIL_BASE=operator@example.com \
  REG_OTP_URL=https://otp-relay.example/one-time-token \
  python3 run_concurrent.py --count 20 --concurrency 5

  # pin a single country, headed (watch it), custom chrome
  python3 run_concurrent.py --count 3 --concurrency 3 --country PH \
      --headed --chrome /usr/bin/google-chrome
"""
import argparse
import concurrent.futures as cf
import json
import os
import re
import socket
import subprocess
import sys
import threading
import time
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))

# ── credentials (required via the worker's isolated environment) ─────────────
PROXY_HOST    = os.environ.get("CLIPROXY_HOST", "")
PROXY_PORT    = os.environ.get("CLIPROXY_PORT", "")
PROXY_ACCOUNT = os.environ.get("CLIPROXY_ACCOUNT", "")
PROXY_PASS    = os.environ.get("CLIPROXY_PASS", "")
PROXY_TTL     = os.environ.get("CLIPROXY_TTL", "15")  # minutes; one sticky IP per account
HEROSMS_KEY   = os.environ.get("HEROSMS_KEY", "")
YESCAPTCHA_KEY = os.environ.get("YESCAPTCHA_KEY", "")
HOTMAIL_BASE  = os.environ.get("HOTMAIL_BASE", "")
REG_OTP_URL   = os.environ.get("REG_OTP_URL", "")
CHROME        = os.environ.get("REG_CHROME", os.environ.get("CHROME_PATH", ""))

# Cheap-first SMS countries that work for OpenAI on hero-sms; proxy region matches each.
# Keep this in sync with phone_verify.COUNTRY_CFG ISO codes.
DEFAULT_COUNTRIES = ["PH", "ID", "BR", "VN", "MY", "ZA"]

_print_lock = threading.Lock()


def log(msg):
    with _print_lock:
        print(f"[run {time.strftime('%H:%M:%S')}] {msg}", flush=True)


def resolve_real_ip(host):
    """Resolve host to a real public IP, bypassing Clash/mihomo fake-IP (198.18.x)."""
    if re.match(r"^\d+\.\d+\.\d+\.\d+$", host):
        return host
    try:
        ip = socket.gethostbyname(host)
    except Exception:
        ip = ""
    if ip and not ip.startswith(("198.18.", "198.19.")):
        return ip
    # Fall back to DoH (Cloudflare) for the real A record.
    try:
        req = urllib.request.Request(
            f"https://1.1.1.1/dns-query?name={host}&type=A",
            headers={"Accept": "application/dns-json"},
        )
        with urllib.request.urlopen(req, timeout=10) as r:
            data = json.loads(r.read().decode())
        for a in data.get("Answer", []):
            if a.get("type") == 1:
                return a["data"]
    except Exception:
        pass
    return ip or host


def fresh_sid():
    return os.urandom(4).hex()


def build_proxy(region):
    """Return (server_url, username, password) with a fresh sticky session for `region`."""
    user = f"{PROXY_ACCOUNT}-region-{region}-sid-{fresh_sid()}-t-{PROXY_TTL}"
    server = f"http://{REAL_IP}:{PROXY_PORT}"
    return server, user, PROXY_PASS


def plus_email(base, tag):
    if "@" not in base:
        return base
    local, domain = base.split("@", 1)
    return f"{local}+{tag}@{domain}"


def jwt_account_id(id_token):
    """Pull chatgpt_account_id out of the id_token JWT (best-effort)."""
    try:
        payload = id_token.split(".")[1]
        payload += "=" * (-len(payload) % 4)
        import base64
        claims = json.loads(base64.urlsafe_b64decode(payload).decode())
        auth = claims.get("https://api.openai.com/auth", {}) or {}
        return auth.get("chatgpt_account_id") or auth.get("user_id") or ""
    except Exception:
        return ""


def write_auth_json(out_dir, acct):
    """Write a Codex CLI auth.json-style file for one account."""
    os.makedirs(out_dir, exist_ok=True)
    email = acct.get("email", "acct")
    safe = re.sub(r"[^A-Za-z0-9_.+-]", "_", email)
    account_id = acct.get("account_id") or jwt_account_id(acct.get("id_token", ""))
    auth = {
        "OPENAI_API_KEY": None,
        "tokens": {
            "id_token": acct.get("id_token", ""),
            "access_token": acct.get("access_token", ""),
            "refresh_token": acct.get("refresh_token", ""),
            "account_id": account_id,
        },
        "last_refresh": time.strftime("%Y-%m-%dT%H:%M:%S+00:00", time.gmtime()),
    }
    path = os.path.join(out_dir, f"auth_{safe}.json")
    with open(path, "w", encoding="utf-8") as f:
        json.dump(auth, f, ensure_ascii=False, indent=2)
    # Append a one-line ledger entry too.
    with open(os.path.join(out_dir, "accounts.jsonl"), "a", encoding="utf-8") as f:
        f.write(json.dumps(acct, ensure_ascii=False) + "\n")
    return path


def run_one(idx, country, headed, out_dir, timeout):
    """Run a single isolated reg_v3.py registration. Returns (ok, info)."""
    tag = f"{int(time.time())}{idx:03d}{os.urandom(2).hex()}"
    email = plus_email(HOTMAIL_BASE, tag)
    server, user, pw = build_proxy(country)
    log(f"#{idx} start country={country} region-IP={REAL_IP} sid-user={user} email={email}")

    env = dict(os.environ)
    env.update({
        "REG_PROXY_SERVER": server,
        "REG_PROXY_USER": user,
        "REG_PROXY_PASS": pw,
        "REG_EMAIL": email,
        "REG_OTP_URL": REG_OTP_URL,
        "REG_CHROME": CHROME,
        "REG_HEADLESS": "0" if headed else "1",
        "HEROSMS_KEY": HEROSMS_KEY,
        "HEROSMS_API_KEY": HEROSMS_KEY,
        "Y_CAPTCHA_KEY": YESCAPTCHA_KEY,
        "REG_SMS_COUNTRY": country,
    })
    script = os.path.join(HERE, "reg_v3.py")
    try:
        proc = subprocess.run(
            [sys.executable, "-u", script],
            env=env, capture_output=True, text=True, timeout=timeout,
        )
    except subprocess.TimeoutExpired:
        log(f"#{idx} TIMEOUT after {timeout}s")
        return False, {"idx": idx, "error": "timeout"}

    out = proc.stdout or ""
    err = proc.stderr or ""
    acct = None
    for line in out.splitlines():
        i = line.find("__CODEX_ACCOUNT__ ")
        if i >= 0:
            try:
                acct = json.loads(line[i + len("__CODEX_ACCOUNT__ "):])
            except Exception:
                pass
    if acct and acct.get("access_token"):
        acct.setdefault("email", email)
        acct["_country"] = country
        acct["_has_refresh"] = bool(acct.get("refresh_token"))
        path = write_auth_json(out_dir, acct)
        log(f"#{idx} ✅ SUCCESS user={acct.get('user_id','?')} refresh={'yes' if acct.get('refresh_token') else 'NO'} -> {path}")
        return True, acct
    # Failure: surface the last few stderr lines for debugging.
    tail = "\n      ".join(err.strip().splitlines()[-6:])
    log(f"#{idx} ❌ no account. stderr tail:\n      {tail}")
    return False, {"idx": idx, "error": "no_account", "stderr_tail": tail}


def main():
    ap = argparse.ArgumentParser(description="Concurrent isolated Codex registrar")
    ap.add_argument("--count", type=int, default=5, help="total accounts to register")
    ap.add_argument("--concurrency", type=int, default=3, help="parallel workers")
    ap.add_argument("--country", default="", help="pin a single SMS country (else rotate cheap list)")
    ap.add_argument("--countries", default="", help="comma list to rotate, e.g. PH,ID,BR")
    ap.add_argument("--headed", action="store_true", help="show the browser (default headless)")
    ap.add_argument("--out", default=os.path.join(HERE, "accounts_out"), help="output dir for auth_*.json")
    ap.add_argument("--timeout", type=int, default=420, help="per-account timeout seconds")
    args = ap.parse_args()

    if not HOTMAIL_BASE or not REG_OTP_URL:
        log("FATAL: set HOTMAIL_BASE and REG_OTP_URL (your Hotmail base + OTP reader URL).")
        log("       supply an operator inbox and authenticated OTP relay URL")
        sys.exit(2)

    global REAL_IP
    REAL_IP = resolve_real_ip(PROXY_HOST)
    log(f"proxy {PROXY_HOST} -> {REAL_IP}:{PROXY_PORT} (account {PROXY_ACCOUNT})")

    if args.country:
        countries = [args.country.upper()]
    elif args.countries:
        countries = [c.strip().upper() for c in args.countries.split(",") if c.strip()]
    else:
        countries = DEFAULT_COUNTRIES

    log(f"registering {args.count} account(s), {args.concurrency} parallel, countries={countries}")
    ok = 0
    results = []
    with cf.ThreadPoolExecutor(max_workers=args.concurrency) as ex:
        futs = {
            ex.submit(run_one, i + 1, countries[i % len(countries)], args.headed, args.out, args.timeout): i
            for i in range(args.count)
        }
        for fut in cf.as_completed(futs):
            success, info = fut.result()
            results.append(info)
            if success:
                ok += 1

    log(f"DONE: {ok}/{args.count} succeeded. auth files in {args.out}/")
    sys.exit(0 if ok > 0 else 1)


REAL_IP = ""

if __name__ == "__main__":
    main()
