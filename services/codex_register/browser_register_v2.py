#!/usr/bin/env python3
"""Browser-based ChatGPT/Codex registration with yescaptcha Turnstile solving.

Ported from other_project's proven browser flow + integrated with yescaptcha for
Turnstile/Arkose challenges that appear during real-browser signup (create_account in
the protocol flow hits registration_disallowed — the real browser carries the JS
signals + solves challenges the API-only path can't reproduce).

Config via env:
  Y_CAPTCHA_KEY   captcha provider API key
  CLIPROXY_SPEC   host:port:user:pass  (user encodes -region-{REGION}-sid-{SID}-t-{TTL})
  HEROSMS_KEY     hero-sms.com api_key
  CHROME_PATH     path to the chrome binary
  REG_HEADLESS    0=headed, 1=headless (default)
  HOTMAIL_BASE    base email for plus-addressing
  HOTMAIL_OTP_URL OTP reader endpoint
  CLIPROXY_REALIP real IP of cliproxy gateway (bypasses fake-IP DNS issues)

Output: single JSON line on stdout. Step logs to stderr.
"""
import json, os, random, re, secrets, string, sys, time, urllib.parse, urllib.request, threading, logging

logging.basicConfig(level=logging.INFO, format="[br-v2] %(message)s", stream=sys.stderr)
log = logging.info

# ── Config from env ─────────────────────────────────────────────────────────
YCAPTCHA_KEY     = os.environ.get("Y_CAPTCHA_KEY", "")
YCAPTCHA_API     = "https://api.yescaptcha.com"
HEROSMS          = "https://hero-sms.com/stubs/handler_api.php"
HEROSMS_KEY      = os.environ.get("HEROSMS_KEY", "")
HOTMAIL_BASE     = os.environ.get("HOTMAIL_BASE", "")
HOTMAIL_OTP_URL  = os.environ.get("HOTMAIL_OTP_URL", "")
CLIPROXY_REALIP  = os.environ.get("CLIPROXY_REALIP", "")
CLIPROXY_PORT    = os.environ.get("CLIPROXY_PORT", "")
CLIPROXY_ACCOUNT = os.environ.get("CLIPROXY_ACCOUNT", "")
CLIPROXY_PASS    = os.environ.get("CLIPROXY_PASS", "")
UA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36"
FIRST_NAMES = ["James","Robert","Michael","William","David","Mary","Sarah","Emma","Olivia","Sophia","Daniel","Andrew","Emily","Grace"]
LAST_NAMES  = ["Smith","Johnson","Williams","Brown","Jones","Miller","Davis","Wilson","Anderson","Taylor","Moore","Martin","Clark","Walker"]


# ── Utility functions ───────────────────────────────────────────────────────
def _urlopen(url, data=None, method="GET", headers=None, timeout=20):
    h = {"User-Agent": UA, "Accept": "*/*"}
    if headers: h.update(headers)
    req = urllib.request.Request(url, data=data, method=method, headers=h)
    return urllib.request.urlopen(req, timeout=timeout)


def hotmail_wait_otp(timeout_sec=150):
    """Poll the OTP reader, returning the first NEW (post-baseline) 6-digit code."""
    seen = set()
    deadline = time.monotonic() + timeout_sec
    while time.monotonic() < deadline:
        try:
            with _urlopen(HOTMAIL_OTP_URL, timeout=15) as r:
                emails = json.loads(r.read().decode()).get("emails", [])
            for e in emails:
                key = str(e.get("时间","")) + str(e.get("主题",""))
                if key in seen: continue
                seen.add(key)
                body = str(e.get("内容预览","")) + " " + str(e.get("html","")) + " " + str(e.get("主题",""))
                code = re.search(r"(?<!\d)(\d{6})(?!\d)", body)
                if code and code.group(1) != "177010":
                    return code.group(1)
        except Exception as err:
            log(f"OTP poll: {err}")
        time.sleep(4)
    return None


# ── cliproxy proxy settings (socks5h + region-Rand + real IP) ─────────────
def cliproxy_settings():
    if not all((CLIPROXY_REALIP, CLIPROXY_PORT, CLIPROXY_ACCOUNT, CLIPROXY_PASS)):
        raise RuntimeError("cliproxy configuration is incomplete")
    tag = secrets.token_hex(4)
    user = f"{CLIPROXY_ACCOUNT}-region-Rand-sid-{tag}"
    # Chromium/Playwright: auth must be passed via separate `username`/`password`
    # fields, NOT embedded in the proxy URL (Chrome rejects URL-embedded basic auth for proxies).
    proxy_cfg = {
        "server": f"http://{CLIPROXY_REALIP}:{CLIPROXY_PORT}",
        "username": user,
        "password": CLIPROXY_PASS,
    }
    return proxy_cfg, tag


# ── yescaptcha Turnstile solver (synchronous HTTP, no async needed) ────────
class TurnstileSolver:
    def __init__(self, api_key=YCAPTCHA_KEY):
        self.api_key = api_key

    def solve(self, sitekey, page_url, timeout_sec=90):
        if not self.api_key:
            return None
        try:
            create = json.dumps({"clientKey": self.api_key, "task": {
                "type": "TurnstileTaskProxyless", "websiteURL": page_url, "websiteKey": sitekey}}).encode()
            r = _urlopen(f"{YCAPTCHA_API}/createTask", data=create, method="POST",
                         headers={"Content-Type": "application/json"}, timeout=20)
            task_id = json.loads(r.read()).get("taskId", "")
            if not task_id:
                log(f"yescaptcha createTask failed")
                return None
            log(f"yescaptcha taskId={task_id}")
            deadline = time.monotonic() + timeout_sec
            while time.monotonic() < deadline:
                time.sleep(3)
                poll = json.dumps({"clientKey": self.api_key, "taskId": task_id}).encode()
                pr = _urlopen(f"{YCAPTCHA_API}/getTaskResult", data=poll, method="POST",
                              headers={"Content-Type": "application/json"}, timeout=15)
                data = json.loads(pr.read())
                if data.get("status") == "ready":
                    sol = data.get("solution", {})
                    token = sol.get("token") or sol.get("gRecaptchaResponse", "")
                    log(f"yescaptcha solved: {token[:30]}...")
                    return token
            log(f"yescaptcha timeout ({timeout_sec}s)")
        except Exception as err:
            log(f"yescaptcha error: {err}")
        return None


# ── Turnstile detection + injection helpers (Playwright page) ──────────────
def detect_turnstile(page):
    """Return (sitekey, page_url) if a Turnstile widget is on the page, else (None, None)."""
    try:
        iframes = page.locator('iframe[src*="challenges.cloudflare.com"]')
        if iframes.count() > 0:
            src = iframes.first.get_attribute("src", timeout=3000) or ""
            m = re.search(r"sitekey=([0-9a-zA-Z_-]+)", src)
            key = m.group(1) if m else ""
            if not key:
                body = iframes.first.content_frame().locator("body").inner_text(timeout=3000)
                m2 = re.search(r"sitekey.*?\"([0-9a-zA-Z_-]{20,})\"", body)
                key = m2.group(1) if m2 else ""
            if key:
                return key, page.url
        # also check for turnstile div in main page
        div = page.locator('div.cf-turnstile')
        if div.count() > 0:
            key = div.first.get_attribute("data-sitekey", timeout=3000) or ""
            if key:
                return key, page.url
    except Exception as e:
        log(f"turnstile detect err: {e}")
    return None, None


def inject_turnstile_token(page, token):
    """After yescaptcha returns a token, inject it into the Turnstile widget and resolve."""
    try:
        page.evaluate(f"""
            (()=>{{
                const t = '{token}';
                const cb = ()=>{{
                    const els = document.querySelectorAll('[name="cf-turnstile-response"]');
                    els.forEach(e=>e.value=t);
                    if(window.turnstile) window.turnstile.render();
                }};
                if(document.readyState==='loading') document.addEventListener('DOMContentLoaded',cb);
                else cb();
                // try the message callback too
                window.postMessage({{type:'turnstile-token',token:t}},'*');
            }})()
        """)
        log("turnstile token injected")
    except Exception as e:
        log(f"turnstile inject err: {e}")


def handle_turnstile_if_needed(page, solver, timeout_sec=90):
    """Check for Turnstile, solve it if present, and inject the token."""
    for attempt in range(20):
        sitekey, url = detect_turnstile(page)
        if not sitekey:
            return True  # no challenge
        log(f"Turnstile found (sitekey={sitekey[:20]}...), attempt {attempt+1}")
        token = solver.solve(sitekey, url, timeout_sec=timeout_sec)
        if token:
            inject_turnstile_token(page, token)
            page.wait_for_timeout(3000)
        else:
            log("Turnstile solve FAILED")
            break
    return detect_turnstile(page)[0] is None  # True if resolved


# ── Main browser registration flow ──────────────────────────────────────────
def register():
    chrome = os.environ.get("CHROME_PATH", "")
    headless = os.environ.get("REG_HEADLESS", "1") != "0"
    proxy, sid = cliproxy_settings()
    tag = secrets.token_hex(5)
    email = HOTMAIL_BASE.replace("@", f"+{tag}@")
    name = f"{random.choice(FIRST_NAMES)} {random.choice(LAST_NAMES)}"
    age = str(random.randint(22, 38))
    solver = TurnstileSolver()
    log(f"sid={sid} email={email} name={name} age={age}")

    # baseline the OTP reader
    try:
        with _urlopen(HOTMAIL_OTP_URL, timeout=12) as r:
            existing = json.loads(r.read()).get("emails", [])
            log(f"OTP baseline: {len(existing)} existing emails")
    except Exception as e:
        log(f"OTP baseline err: {e}")

    from playwright.sync_api import sync_playwright

    with sync_playwright() as p:
        ctx = p.chromium.launch_persistent_context(
            user_data_dir=f"/tmp/codexreg_{tag}",
            headless=headless,
            executable_path=chrome or None,
            args=["--no-sandbox", "--disable-gpu", "--disable-dev-shm-usage",
                  "--disable-blink-features=AutomationControlled"],
            proxy=proxy,
            user_agent=UA,
            locale="en-US",
            viewport={"width": 1280, "height": 800},
            ignore_default_args=["--enable-automation"],
        )
        # Anti-detection init script
        ctx.add_init_script("""
            Object.defineProperty(navigator,'webdriver',{get:()=>undefined});
            if(!window.chrome){window.chrome={runtime:{},app:{}};}
            Object.defineProperty(navigator,'plugins',{get:()=>[1,2,3,4,5]});
            Object.defineProperty(navigator,'languages',{get:()=>['en-US','en']});
            window.addEventListener('DOMContentLoaded',()=>{
                const f=document.createElement('iframe');
                f.style.display='none';
                document.body.appendChild(f);
            });
        """)
        page = ctx.pages[0]
        result = {"success": False, "email": email, "access_token": "", "account_id": "",
                  "user_id": "", "plan_type": "free", "error": "", "proxy_sid": sid}

        try:
            _run_flow(page, email, name, age, solver, result)
        except Exception as exc:
            import traceback; traceback.print_exc()
            result["error"] = str(exc)
        finally:
            try: ctx.close()
            except Exception: pass
        return result


def _run_flow(page, email, name, age, solver, result):
    log("1. goto login")
    page.goto("https://chatgpt.com/auth/login", wait_until="domcontentloaded", timeout=60000)
    for _ in range(35):
        t = (page.title() or "").lower()
        if "just a moment" not in t and "captcha" not in t and "verify" not in t:
            break
        handle_turnstile_if_needed(page, solver)
        page.wait_for_timeout(2000)
    log(f"   login page: {page.title()}")

    # 2. Enter email (passwordless path — email triggers OTP)
    page.wait_for_selector('input[type="email"], input[name="email"]', timeout=35000)
    page.locator('input[type="email"], input[name="email"]').first.fill(email)
    page.wait_for_timeout(500)
    # Try clicking the "Continue" button — on the real signup page this may trigger
    # a Turnstile challenge. Detect and solve it before proceeding.
    _click_continue(page)
    page.wait_for_timeout(2000)
    # Immediately check for Turnstile after clicking Continue
    resolved = handle_turnstile_if_needed(page, solver)
    if not resolved:
        # Turnstile found but couldn't solve — retry click
        log("   Turnstile unsolved, retry clicking + solving...")
        _click_continue(page)
        page.wait_for_timeout(2000)
        handle_turnstile_if_needed(page, solver)
    page.wait_for_timeout(4000)
    log(f"   after email: {page.title()} url={page.url[:80]}")

    # 3. Wait for OTP input OR password field (some regions get password-based signup)
    otp_field = page.locator('input[name="code"], input[autocomplete="one-time-code"], input[inputmode="numeric"]').first
    pw_field = page.locator('input[type="password"], input[name="password"]').first
    if otp_field.count() == 0 and pw_field.count() == 0:
        log("   waiting for OTP/password field (checking for Turnstile)...")
        for i in range(30):
            page.wait_for_timeout(2000)
            handle_turnstile_if_needed(page, solver)
            if page.locator('input[name="code"], input[inputmode="numeric"]').first.count() > 0:
                break
            if page.locator('input[type="password"], input[name="password"]').first.count() > 0:
                break
            # Check if "Continue with phone" is still visible (email was blocked, go phone)
            try:
                pbtn = page.get_by_text("Continue with phone", exact=False)
                if pbtn.count() > 0 and i > 5:
                    pbtn.first.click(force=True)
                    page.wait_for_timeout(3000)
                    handle_turnstile_if_needed(page, solver)
            except Exception:
                pass
    # Refresh locators
    otp_field = page.locator('input[name="code"], input[autocomplete="one-time-code"], input[inputmode="numeric"]').first
    pw_field = page.locator('input[type="password"], input[name="password"]').first

    if pw_field.count() > 0 and pw_field.is_visible():
        log("   password-based signup detected")
        import random as _r, string as _s
        pwd = "Pw" + "".join(_r.choices(_s.ascii_letters + _s.digits, k=12)) + "!9"
        pw_field.fill(pwd, force=True)
        page.wait_for_timeout(400)
        _click_continue(page)
        page.wait_for_timeout(5000)
        handle_turnstile_if_needed(page, solver)
        # after password, OTP field may appear
        otp_field = page.locator('input[name="code"], input[autocomplete="one-time-code"], input[inputmode="numeric"]').first
        if otp_field.count() == 0:
            for _ in range(20):
                page.wait_for_timeout(2000)
                handle_turnstile_if_needed(page, solver)
                if page.locator('input[name="code"], input[inputmode="numeric"]').first.count() > 0:
                    otp_field = page.locator('input[name="code"], input[inputmode="numeric"]').first
                    break
    log(f"   OTP/password step done. title={page.title()} url={page.url[:80]}")

    if otp_field.count() > 0 and otp_field.is_visible():
        log("   OTP field visible, polling email")
        code = hotmail_wait_otp(timeout_sec=150)
        if not code:
            result["error"] = "otp_timeout"
            return
        log(f"   OTP code: {code}")
        otp_field.fill(code, force=True)
        page.wait_for_timeout(500)
        otp_field.press("Enter")
        page.wait_for_timeout(6000)
        log(f"   after OTP: {page.title()} url={page.url[:80]}")
        handle_turnstile_if_needed(page, solver)

    # 4. Profile (about-you)
    for _ in range(25):
        if page.locator('input[name="name"]').first.count() > 0:
            break
        page.wait_for_timeout(2000)
    if page.locator('input[name="name"]').first.count() > 0:
        log("   filling profile")
        page.locator('input[name="name"]').first.fill(name, force=True)
        age_el = page.locator('input[name="age"], input[type="number"]').first
        if age_el.count() > 0:
            age_el.fill(age, force=True)
        page.wait_for_timeout(400)
        page.locator('button[type="submit"]').last.click(force=True)
        page.wait_for_timeout(5000)
        log(f"   after profile: {page.title()} url={page.url[:80]}")
        handle_turnstile_if_needed(page, solver)

    # 5. Oops recovery
    for rec in range(3):
        title = (page.title() or "").lower()
        if "oops" not in title:
            break
        log(f"   oops recovery #{rec+1}")
        page.reload()
        page.wait_for_timeout(3000)
        if page.locator('input[name="name"]').first.count() > 0:
            page.locator('input[name="name"]').first.fill(name, force=True)
            ae = page.locator('input[name="age"], input[type="number"]').first
            if ae.count() > 0: ae.fill(age, force=True)
            page.wait_for_timeout(400)
            page.locator('button[type="submit"]').last.click(force=True)
            page.wait_for_timeout(5000)

    # 6. Post-signup (consent / workspace)
    for _ in range(20):
        url = page.url
        if "chatgpt.com" in url and "/auth/" not in url:
            break
        handle_turnstile_if_needed(page, solver)
        if "consent" in url.lower() or "authorize" in url.lower():
            for label in ["Authorize", "Allow", "Accept", "Continue", "Continue to ChatGPT"]:
                try:
                    btn = page.get_by_role("button", name=label)
                    if btn.count() > 0:
                        btn.first.click()
                        log(f"   clicked {label}")
                        page.wait_for_timeout(3000)
                        break
                except Exception:
                    pass
        page.wait_for_timeout(2000)
    log(f"   final page: {page.title()} url={page.url[:80]}")

    # 7. Extract session
    sess = page.evaluate(
        'async () => { try { const r = await fetch("/api/auth/session",{credentials:"include"}); return await r.json(); } catch(e) { return {error:String(e)}; } }'
    )
    user = (sess or {}).get("user", {}) or {}
    acct = (sess or {}).get("account", {}) or {}
    at = (sess or {}).get("accessToken", "")
    if user.get("id") and (acct.get("id") or at):
        result.update(success=True, user_id=user.get("id", ""), account_id=acct.get("id", ""),
                      plan_type=acct.get("planType", "free"), name=user.get("name", name),
                      access_token=at)
        log(f"SUCCESS user={user.get('id')} account={acct.get('id')}")
    else:
        result["error"] = f"no_session: title={page.title()} url={page.url} sess_keys={(sess or {}).keys()}"


def _click_continue(page):
    try:
        b = page.locator('button[type="submit"]').first
        if b.count() > 0: b.click(force=True); return True
    except Exception: pass
    return False


if __name__ == "__main__":
    result = register()
    print(json.dumps(result, ensure_ascii=False), flush=True)
