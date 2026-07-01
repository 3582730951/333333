#!/usr/bin/env python3
"""Browser-based ChatGPT/Codex registration for pool_server.

A self-contained Playwright flow (ported/adapted from other_project's browser track,
which the HTTP protocol flow cannot replace — OpenAI's signup authorize step needs real
browser JS, see the invalid_state diagnosis). It drives a real Chrome through a cliproxy
residential IP, signs up with a mail.tm temp address (email-OTP, passwordless), falls back
to hero-sms if a phone step appears, then extracts the ChatGPT session.

Output: a single JSON line on stdout: {"success":bool, "email":..., "access_token":...,
"account_id":..., "user_id":..., "plan_type":..., "error":...}. All human logs go to stderr.

Config via env:
  CLIPROXY_SPEC   host:port:user:pass  (user encodes -region-..-sid-..-t-..; sid is rotated)
  HEROSMS_KEY     hero-sms.com api_key (optional; only used if a phone step appears)
  CHROME_PATH     path to the chrome binary
  REG_HEADLESS    "0" to run headed (default headless=new)
"""
import json
import os
import random
import re
import secrets
import string
import sys
import time
import urllib.request
import urllib.parse

MAILTM = "https://api.mail.tm"
HEROSMS = "https://hero-sms.com/stubs/handler_api.php"
UA = ("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
      "(KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36")

FIRST = ["James", "John", "Robert", "Michael", "William", "David", "Mary", "Sarah",
         "Emma", "Olivia", "Sophia", "Daniel", "Andrew", "Joshua", "Emily", "Grace"]
LAST = ["Smith", "Johnson", "Williams", "Brown", "Jones", "Miller", "Davis", "Wilson",
        "Anderson", "Taylor", "Moore", "Martin", "Lee", "Clark", "Lewis", "Walker"]


def log(*a):
    print("[browser-reg]", *a, file=sys.stderr, flush=True)


# ── mail.tm temp email ──────────────────────────────────────────────────────
def _urlopen(url, data=None, method="GET", headers=None, timeout=20):
    """urllib with a browser UA — hero-sms.com / mail.tm sit behind Cloudflare, which
    403s the default Python-urllib User-Agent."""
    h = {"User-Agent": UA, "Accept": "*/*"}
    if headers:
        h.update(headers)
    req = urllib.request.Request(url, data=data, method=method, headers=h)
    return urllib.request.urlopen(req, timeout=timeout)


def _http_json(method, url, body=None, headers=None):
    data = json.dumps(body).encode() if body is not None else None
    h = {"Content-Type": "application/json", "Accept": "application/json"}
    if headers:
        h.update(headers)
    with _urlopen(url, data=data, method=method, headers=h) as r:
        return json.loads(r.read().decode())


def _members(resp):
    """mail.tm returns either a bare JSON list or a {"hydra:member":[...]} wrapper."""
    if isinstance(resp, list):
        return resp
    if isinstance(resp, dict):
        return resp.get("hydra:member", [])
    return []


def mailtm_create():
    domains = _members(_http_json("GET", f"{MAILTM}/domains"))
    if not domains:
        raise RuntimeError("mail.tm: no domains")
    domain = domains[0]["domain"]
    local = "".join(random.choices(string.ascii_lowercase + string.digits, k=12))
    addr = f"{local}@{domain}"
    pw = "".join(random.choices(string.ascii_letters + string.digits, k=14))
    _http_json("POST", f"{MAILTM}/accounts", {"address": addr, "password": pw})
    tok = _http_json("POST", f"{MAILTM}/token", {"address": addr, "password": pw})["token"]
    return addr, tok


def mailtm_wait_code(token, timeout=150):
    h = {"Authorization": f"Bearer {token}"}
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        try:
            msgs = _members(_http_json("GET", f"{MAILTM}/messages", headers=h))
            for m in msgs:
                full = _http_json("GET", f"{MAILTM}/messages/{m['id']}", headers=h)
                html = full.get("html") or []
                if isinstance(html, list):
                    html = " ".join(html)
                body = (full.get("text") or "") + " " + html + " " + (full.get("subject") or "")
                code = re.search(r"(?<!\d)(\d{6})(?!\d)", body)
                if code:
                    return code.group(1)
        except Exception as e:
            log("mailtm poll error:", e)
        time.sleep(4)
    return None


# ── cliproxy (fresh residential session per run) ────────────────────────────
def cliproxy_settings(spec):
    if not spec:
        return None, "?"
    parts = spec.split(":")
    if len(parts) < 4:
        raise RuntimeError("CLIPROXY_SPEC must be host:port:user:pass")
    host, port, user = parts[0], parts[1], parts[2]
    pw = ":".join(parts[3:])
    sid = secrets.token_hex(4)
    user = re.sub(r"-sid-[^-:@/]+", f"-sid-{sid}", user)
    if "-sid-" not in user:
        user = f"{user}-sid-{sid}"
    return {"server": f"http://{host}:{port}", "username": user, "password": pw}, sid


# ── hero-sms (only if a phone step appears) ─────────────────────────────────
def herosms_get_number(key, country="6", service="ot"):
    q = urllib.parse.urlencode({"api_key": key, "action": "getNumber",
                                "service": service, "country": country})
    with _urlopen(f"{HEROSMS}?{q}", timeout=20) as r:
        txt = r.read().decode().strip()
    if txt.startswith("ACCESS_NUMBER:"):
        _, oid, phone = txt.split(":", 2)
        return oid, phone
    raise RuntimeError(f"hero-sms getNumber: {txt}")


def herosms_wait(key, oid, timeout=150):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        q = urllib.parse.urlencode({"api_key": key, "action": "getStatus", "id": oid})
        try:
            with _urlopen(f"{HEROSMS}?{q}", timeout=15) as r:
                txt = r.read().decode().strip()
            if txt.startswith("STATUS_OK:"):
                return txt.split(":", 1)[1]
        except Exception:
            pass
        time.sleep(4)
    return None


def herosms_cancel(key, oid):
    try:
        q = urllib.parse.urlencode({"api_key": key, "action": "setStatus", "id": oid, "status": "8"})
        _urlopen(f"{HEROSMS}?{q}", timeout=15).read()
    except Exception:
        pass


STEALTH = """
Object.defineProperty(navigator,'webdriver',{get:()=>undefined});
if(!window.chrome){window.chrome={runtime:{},app:{}};}
Object.defineProperty(navigator,'languages',{get:()=>['en-US','en']});
Object.defineProperty(navigator,'hardwareConcurrency',{get:()=>8});
Object.defineProperty(navigator,'deviceMemory',{get:()=>8});
"""


def register(out):
    chrome = os.environ.get("CHROME_PATH", "")
    headless = os.environ.get("REG_HEADLESS", "1") != "0"
    proxy_spec = os.environ.get("CLIPROXY_SPEC", "")
    herokey = os.environ.get("HEROSMS_KEY", "")

    proxy, sid = cliproxy_settings(proxy_spec)
    log("proxy sid", sid, "->", proxy["server"] if proxy else "direct")
    # mail.tm is best-effort (phone flow doesn't need it; kept for a future email path).
    try:
        email, mail_token = mailtm_create()
        log("email", email)
    except Exception as e:
        log("mail.tm skipped:", e)
        email, mail_token = "", ""
    name = f"{random.choice(FIRST)} {random.choice(LAST)}"
    age = str(random.randint(20, 35))
    password = "Pw" + "".join(random.choices(string.ascii_letters + string.digits, k=12)) + "!9"

    from playwright.sync_api import sync_playwright
    try:
        from playwright_stealth import Stealth
        _stealth_cm = lambda pw: Stealth().use_sync(pw)
    except Exception:
        _stealth_cm = lambda pw: pw  # playwright-stealth optional; STEALTH init script still applies
    import tempfile
    udd = tempfile.mkdtemp(prefix="codexreg_")

    # playwright-stealth patches the many navigator/webgl/chrome fingerprint tells that
    # OpenAI's signup checks; combined with headed mode under xvfb it gets past the silent
    # form-action block that headless+minimal-stealth hits.
    with _stealth_cm(sync_playwright()) as p:
        ctx = p.chromium.launch_persistent_context(
            user_data_dir=udd, headless=headless, executable_path=chrome or None,
            args=["--no-sandbox", "--disable-gpu", "--disable-dev-shm-usage",
                  "--disable-blink-features=AutomationControlled"],
            proxy=proxy, user_agent=UA, locale="en-US",
            viewport={"width": 1280, "height": 800},
            ignore_default_args=["--enable-automation"],
        )
        ctx.add_init_script(STEALTH)
        page = ctx.pages[0]
        out["email"] = email
        out["proxy_sid"] = sid
        try:
            _run_flow(page, email, mail_token, password, name, age, herokey, out)
        finally:
            try:
                ctx.close()
            except Exception:
                pass


def _dump(page, label):
    """Log every visible input + button (and screenshot) so the live signup flow is visible."""
    try:
        info = page.evaluate("""() => {
            const vis = el => { const r = el.getBoundingClientRect(); const s = getComputedStyle(el);
                return r.width>0 && r.height>0 && s.visibility!=='hidden' && s.display!=='none'; };
            const inputs = [...document.querySelectorAll('input,textarea')].filter(vis).map(i =>
                ({tag:i.tagName, type:i.type||'', name:i.name||'', id:i.id||'',
                  ph:i.placeholder||'', auto:i.autocomplete||'', im:i.inputMode||''}));
            const buttons = [...document.querySelectorAll('button,[role=button],a')].filter(vis)
                .map(b => (b.innerText||b.textContent||'').trim()).filter(Boolean).slice(0,25);
            return {inputs, buttons};
        }""")
        log(f"DUMP[{label}] title={page.title()!r}")
        log(f"DUMP[{label}] inputs={json.dumps(info.get('inputs'))}")
        log(f"DUMP[{label}] buttons={json.dumps(info.get('buttons'))}")
        shot = f"/mnt/d/Code/R3_Code/MicliProxy/pool_server/.run/breg_{label}.png"
        page.screenshot(path=shot)
        log(f"DUMP[{label}] screenshot {shot}")
    except Exception as e:
        log(f"DUMP[{label}] error {e}")


def _click_continue(page):
    # Exact "Continue" only — a substring match would hit "Continue with Google/Apple/phone".
    try:
        b = page.get_by_role("button", name="Continue", exact=True)
        if b.count() and b.first.is_visible():
            b.first.click()
            return True
    except Exception:
        pass
    try:
        b = page.locator('button[type="submit"]').first
        if b.count() and b.is_visible():
            b.click(force=True)
            return True
    except Exception:
        pass
    return False


def _run_flow(page, email, mail_token, password, name, age, herokey, out):
    log("goto login")
    page.goto("https://chatgpt.com/auth/login", wait_until="domcontentloaded", timeout=60000)
    for _ in range(30):
        t = (page.title() or "").lower()
        if "just a moment" not in t and "moment" not in t:
            break
        page.wait_for_timeout(2000)
    log("title after login:", page.title(), "url:", page.url)
    _dump(page, "1_login")

    # ChatGPT blocks disposable email domains (mail.tm), so use the phone signup path,
    # which our hero-sms numbers satisfy. Click "Continue with phone".
    if not herokey:
        out["error"] = "no_herosms_key"
        return
    clicked = False
    for getter in (
        lambda: page.get_by_role("button", name="Continue with phone"),
        lambda: page.get_by_text("Continue with phone", exact=False),
        lambda: page.locator('button:has-text("phone"), [role="button"]:has-text("phone"), a:has-text("phone")'),
    ):
        try:
            loc = getter()
            if loc.count():
                loc.first.scroll_into_view_if_needed(timeout=3000)
                loc.first.click(force=True, timeout=6000)
                clicked = True
                log("clicked Continue with phone")
                break
        except Exception as e:
            log("phone click attempt:", e)
    if not clicked:
        out["error"] = "no_phone_button"
        return
    # wait for the phone step (URL change or a tel input) before spending a number
    tel_ok = False
    for _ in range(14):
        page.wait_for_timeout(2000)
        if page.locator('input[type="tel"]').count() or "phone" in page.url.lower():
            tel_ok = True
            break
    log("after phone click:", page.title(), page.url)
    _dump(page, "2_phone_page")
    if not tel_ok:
        log("no tel input after phone click")
        _dump(page, "2b_no_tel")
        out["error"] = "no_tel_input"
        return

    # Buy a number. hero-sms country 6 = Indonesia (+62, cheapest); phone like 62812...
    oid, phone = herosms_get_number(herokey, country="6")
    out["phone"] = phone
    log("hero-sms number", phone, "order", oid)
    # International dial: ensure a leading +. ChatGPT's tel input usually defaults to US;
    # entering the full +<country><number> sets the right country.
    intl = phone if phone.startswith("+") else "+" + phone
    try:
        tel = page.locator('input[type="tel"]').first
        tel.click()
        tel.fill(intl, force=True)
        page.wait_for_timeout(600)
        _dump(page, "3_phone_filled")
        _click_continue(page)
    except Exception as e:
        log("phone fill failed:", e)
        herosms_cancel(herokey, oid)
        out["error"] = "phone_fill_failed"
        return
    page.wait_for_timeout(5000)
    log("after phone continue:", page.title(), page.url)
    _dump(page, "4_after_phone")

    # SMS OTP
    try:
        page.wait_for_selector('input[name="code"], input[autocomplete="one-time-code"], input[inputmode="numeric"]', timeout=25000)
    except Exception:
        log("no SMS code field")
        herosms_cancel(herokey, oid)
        out["error"] = out.get("error") or "no_sms_field"
        return
    log("waiting for SMS code via hero-sms")
    smscode = herosms_wait(herokey, oid, timeout=150)
    if not smscode:
        out["error"] = "sms_timeout"
        return
    log("SMS code", smscode)
    sc = page.locator('input[name="code"], input[autocomplete="one-time-code"], input[inputmode="numeric"]').first
    sc.fill(smscode, force=True)
    page.wait_for_timeout(400)
    try:
        sc.press("Enter")
    except Exception:
        _click_continue(page)
    page.wait_for_timeout(6000)
    log("after SMS code:", page.title(), page.url)
    _dump(page, "5_after_sms")

    # profile: name + age/birthday (may also ask for an email here — fill mail.tm if so)
    try:
        if page.locator('input[name="name"]').first.count():
            log("profile step")
            page.locator('input[name="name"]').first.fill(name, force=True)
            for asel in ['input[name="age"]', 'input[type="number"]']:
                el = page.locator(asel).first
                if el.count():
                    el.fill(age, force=True)
                    break
            page.wait_for_timeout(400)
            page.locator('button[type="submit"]').last.click(force=True)
            page.wait_for_timeout(5000)
            log("after profile:", page.title(), page.url)
            _dump(page, "6_after_profile")
    except Exception as e:
        log("profile error:", e)

    # consent / workspace settle
    for _ in range(15):
        url = page.url
        if "chatgpt.com" in url and "/auth/" not in url:
            break
        if "consent" in url.lower() or "authorize" in url.lower():
            for label in ["Authorize", "Allow", "Accept", "Continue"]:
                try:
                    b = page.get_by_role("button", name=label)
                    if b.count():
                        b.first.click()
                        page.wait_for_timeout(2500)
                        break
                except Exception:
                    pass
        page.wait_for_timeout(2000)

    # extract session
    sess = page.evaluate(
        '''async () => { try { const r = await fetch("/api/auth/session",{credentials:"include"}); return await r.json(); } catch(e){ return {error:String(e)}; } }'''
    )
    user = (sess or {}).get("user", {}) or {}
    acct = (sess or {}).get("account", {}) or {}
    if user.get("id") and (sess or {}).get("accessToken"):
        out.update(success=True, user_id=user.get("id", ""),
                   account_id=acct.get("id", ""), plan_type=acct.get("planType", "free"),
                   access_token=sess.get("accessToken", ""), name=user.get("name", name))
    else:
        out["error"] = out.get("error") or "no_session"
        out["debug_title"] = page.title()
        out["debug_url"] = page.url
        _dump(page, "7_no_session")


def main():
    out = {"success": False, "email": "", "access_token": "", "account_id": "",
           "user_id": "", "plan_type": "free", "error": ""}
    try:
        register(out)
    except Exception as e:
        import traceback
        traceback.print_exc()
        out["error"] = str(e)
    print(json.dumps(out), flush=True)


if __name__ == "__main__":
    main()
