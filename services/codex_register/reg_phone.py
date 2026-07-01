#!/usr/bin/env python3
"""
ChatGPT phone-signup: hero-sms + yescaptcha + cliproxy US (指定代理).
Ported from other_project's working patterns + other_gpt's GuJumpgate techniques.

Providers (all from env, pool_server injects):
  - Proxy: cliproxy US residential (sg2.cliproxy.io:3010, user zdvw1182255-region-US-sid-8ijY8peJ-t-15)
  - SMS: hero-sms (hero-sms.com, api_key 810154d173c3c562B1ed124418c8f7B3)
  - Captcha: yescaptcha (api.yescaptcha.com, key 63bd2418e3ba87a501e06efe45820c65646a8c79111595)

Output: __CODEX_ACCOUNT__ {json} on stdout (imported by pool_server Go pipeline).
"""
import json, os, random, re, sys, time, urllib.error, urllib.parse, urllib.request

# ── Providers (from env or defaults matching user's specified credentials) ──
REALIP        = os.environ.get("CLIPROXY_REALIP", "")
PROXY_USER    = os.environ.get("REG_PROXY_USER", "zdvw1182255-region-US-sid-8ijY8peJ-t-15")
PROXY_PASS    = os.environ.get("REG_PROXY_PASS", "d6kfytmo")
HEROSMS_KEY   = os.environ.get("HEROSMS_KEY", "810154d173c3c562B1ed124418c8f7B3")
YCAPTCHA_KEY  = os.environ.get("Y_CAPTCHA_KEY", "63bd2418e3ba87a501e06efe45820c65646a8c79111595")
YCAPTCHA_API  = "https://api.yescaptcha.com"
HEROSMS       = "https://hero-sms.com/stubs/handler_api.php"
CHROME        = os.environ.get("REG_CHROME", os.environ.get("CHROME_PATH", ""))
HEADLESS      = os.environ.get("REG_HEADLESS", "1") != "0"
EMAIL_BASE    = os.environ.get("HOTMAIL_BASE", "xnzsilq@hotmail.com")
OTP_URL       = os.environ.get("REG_OTP_URL", "")
UA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"

FN = ["James","John","Robert","Michael","William","David","Mary","Sarah","Emma","Olivia","Sophia","Daniel","Andrew","Emily","Grace","Christopher","Jennifer","Jessica","Amanda","Joshua"]
LN = ["Smith","Johnson","Williams","Brown","Jones","Garcia","Miller","Davis","Rodriguez","Martinez","Wilson","Anderson","Thomas","Taylor","Moore","Jackson","Lee","White","Harris","Clark"]

STEALTH = """
Object.defineProperty(navigator,'webdriver',{get:()=>undefined});
delete Object.getOwnPropertyDescriptor(navigator.__proto__,'webdriver');
if(!window.chrome){window.chrome={runtime:{},app:{}};}
Object.defineProperty(navigator,'hardwareConcurrency',{get:()=>8});
Object.defineProperty(navigator,'deviceMemory',{get:()=>8});
Object.defineProperty(navigator,'languages',{get:()=>['en-US','en']});
"""

def log(msg): print(f"[phone {time.strftime('%H:%M:%S')}] {msg}", file=sys.stderr, flush=True)
def _url(url, data=None, method="GET", timeout=15):
    h = {"User-Agent": UA, "Accept": "*/*"}
    req = urllib.request.Request(url, data=data, method=method, headers=h)
    return urllib.request.urlopen(req, timeout=timeout)

# ── hero-sms ────────────────────────────────────────────────────────────
def sms_get_number(country="6", service="ot"):
    q = urllib.parse.urlencode({"api_key": HEROSMS_KEY, "action": "getNumber", "service": service, "country": country})
    with _url(f"{HEROSMS}?{q}", timeout=20) as r:
        txt = r.read().decode().strip()
    if txt.startswith("ACCESS_NUMBER:"):
        _, oid, phone = txt.split(":", 2)
        return oid, phone
    raise RuntimeError(f"hero-sms: {txt}")

def sms_wait(oid, timeout_sec=150):
    dl = time.time() + timeout_sec
    while time.time() < dl:
        q = urllib.parse.urlencode({"api_key": HEROSMS_KEY, "action": "getStatus", "id": oid})
        try:
            with _url(f"{HEROSMS}?{q}", timeout=15) as r:
                txt = r.read().decode().strip()
            if txt.startswith("STATUS_OK:"): return txt.split(":", 1)[1]
        except Exception: pass
        time.sleep(4)
    return None

def sms_cancel(oid):
    try:
        q = urllib.parse.urlencode({"api_key": HEROSMS_KEY, "action": "setStatus", "id": oid, "status": "8"})
        _url(f"{HEROSMS}?{q}", timeout=10)
    except: pass

# ── yescaptcha Turnstile ────────────────────────────────────────────────
def turnstile_solve(sitekey, page_url):
    if not YCAPTCHA_KEY: return None
    try:
        create = json.dumps({"clientKey": YCAPTCHA_KEY, "task": {"type": "TurnstileTaskProxyless", "websiteURL": page_url, "websiteKey": sitekey}}).encode()
        cr = _url(f"{YCAPTCHA_API}/createTask", data=create, method="POST", timeout=20)
        task_id = json.loads(cr.read()).get("taskId", "")
        if not task_id: return None
        log(f"yescaptcha: taskId={task_id}")
        dl = time.time() + 90
        while time.time() < dl:
            time.sleep(3)
            poll = json.dumps({"clientKey": YCAPTCHA_KEY, "taskId": task_id}).encode()
            pr = _url(f"{YCAPTCHA_API}/getTaskResult", data=poll, method="POST", timeout=15)
            data = json.loads(pr.read())
            if data.get("status") == "ready":
                sol = data.get("solution", {})
                token = sol.get("token") or sol.get("gRecaptchaResponse", "")
                log(f"yescaptcha: solved {token[:20]}...")
                return token
        log("yescaptcha: timeout")
    except Exception as e: log(f"yescaptcha err: {e}")
    return None

# ── Hotmail OTP (fallback for email step) ───────────────────────────────
def hotmail_wait_otp(since_ts):
    dl = time.time() + 120
    while time.time() < dl:
        try:
            with _url(OTP_URL, timeout=10) as r:
                data = json.loads(r.read().decode())
        except: time.sleep(4); continue
        for m in data.get("emails", []):
            ts = m.get("时间", "") or "2000-01-01"
            if ts < since_ts: continue
            c = f"{m.get('主题','')} {m.get('内容预览','')} {m.get('html','')}"
            mm = re.search(r"(?<!\d)(\d{6})(?!\d)", c)
            if mm and mm.group(1) != "177010": return mm.group(1)
        time.sleep(4)
    return None

# ── Main flow ────────────────────────────────────────────────────────────
def main():
    # Resolve proxy host to real IP
    real_ip = REALIP
    if not real_ip:
        import socket
        real_ip = socket.gethostbyname("sg2.cliproxy.io")
        if real_ip.startswith("198.18."):
            # Clash fake-IP — resolve externally
            import json as _j
            r = urllib.request.urlopen(urllib.request.Request("https://1.1.1.1/dns-query?name=sg2.cliproxy.io&type=A", headers={"Accept": "application/dns-json"}), timeout=10)
            for a in _j.loads(r.read()).get("Answer", []):
                if a.get("type") == 1: real_ip = a["data"]; break
    proxy_server = f"http://{real_ip}:3010"

    fn, ln = random.choice(FN), random.choice(LN)
    age = str(random.randint(19, 25))
    log(f"US proxy: {real_ip} | hero-sms: OK | yescaptcha: OK | {fn} {ln} age={age}")

    from playwright.sync_api import sync_playwright
    with sync_playwright() as p:
        ctx = p.chromium.launch_persistent_context(
            user_data_dir=f"/tmp/phone_reg_{os.urandom(4).hex()}",
            headless=HEADLESS, executable_path=CHROME or None,
            args=["--no-sandbox","--disable-gpu","--disable-dev-shm-usage","--disable-blink-features=AutomationControlled"],
            proxy={"server": proxy_server, "username": PROXY_USER, "password": PROXY_PASS},
            user_agent=UA, locale="en-US", viewport={"width":1280,"height":720},
            ignore_default_args=["--enable-automation"],
        )
        ctx.add_init_script(STEALTH)
        page = ctx.pages[0]
        result = {"success": False}

        try:
            # ── Step 0: Login page ──
            log("0. goto login")
            page.goto("https://chatgpt.com/auth/login", wait_until="domcontentloaded", timeout=60000)
            for _ in range(30):
                t = (page.title() or "").lower()
                if "chatgpt" in t or "get started" in t: break
                # Check for Turnstile
                _check_turnstile(page)
                time.sleep(2)
            log(f"   page: {page.title()}")

            # ── Step 1: "Continue with phone" ──
            log("1. phone signup")
            clicked = False
            for s in ["Continue with phone", "Continue with phone"]:
                try:
                    b = page.get_by_text(s).first
                    if b.count() > 0:
                        b.click(force=True); clicked = True; break
                except: pass
            if not clicked:
                try:
                    page.get_by_role("button", name="Continue with phone").first.click(force=True); clicked = True
                except: pass
            time.sleep(4)
            log(f"   after phone click: {page.title()} url={page.url[:80]}")
            _check_turnstile(page)

            # ── Step 2: Get phone number from hero-sms ──
            log("2. hero-sms getNumber")
            oid, phone = sms_get_number("6")
            intl = phone if phone.startswith("+") else "+" + phone
            log(f"   phone: {intl} (order: {oid})")

            # ── Step 3: Fill phone ──
            log("3. fill phone")
            for _ in range(15):
                tel = page.locator('input[type="tel"], input[name="phone"]').first
                if tel.count() > 0:
                    try:
                        tel.click(); time.sleep(0.3)
                        # Type digit by digit (bypasses React validation)
                        for ch in intl:
                            page.keyboard.type(ch, delay=random.randint(30, 80))
                        log(f"   typed: {intl}")
                        break
                    except Exception as e:
                        log(f"   type err: {e}")
                time.sleep(2)
                _check_turnstile(page)
            time.sleep(1)
            _check_turnstile(page)
            # Click Continue
            try:
                page.locator('button[type="submit"]').first.click(force=True)
            except:
                page.keyboard.press("Enter")
            time.sleep(4)
            log(f"   after phone submit: {page.title()} url={page.url[:80]}")
            _check_turnstile(page)

            # ── Step 4: Wait for SMS code + enter ──
            log("4. wait SMS")
            code_field = page.locator('input[name="code"], input[autocomplete="one-time-code"], input[inputmode="numeric"]').first
            if code_field.count() == 0:
                for _ in range(20):
                    time.sleep(2)
                    _check_turnstile(page)
                    if page.locator('input[name="code"]').first.count() > 0:
                        code_field = page.locator('input[name="code"]').first; break
            sms_code = sms_wait(oid, timeout_sec=150)
            if not sms_code:
                log("SMS TIMEOUT"); sms_cancel(oid); result["error"] = "sms_timeout"; return result
            log(f"   SMS: {sms_code}")
            code_field.fill(sms_code, force=True)
            time.sleep(0.5)
            code_field.press("Enter")
            time.sleep(6)
            log(f"   after SMS: {page.title()} url={page.url[:80]}")
            _check_turnstile(page)
            sms_cancel(oid)  # cancel order — code used

            # ── Step 5: Handle "Check your inbox" or go to profile ──
            log("5. post-SMS navigation")
            for i in range(20):
                url = page.url; title = (page.title() or "").lower()
                if "about-you" in url or page.locator('input[name="name"]').first.count() > 0: break
                if "chatgpt.com" in url and "/auth/" not in url: break
                if "check your inbox" in title and "email" in url:
                    # Some flows require email verification too
                    if OTP_URL:
                        since = time.strftime("%Y-%m-%dT%H:%M", time.gmtime())
                        eml_code = hotmail_wait_otp(since)
                        if eml_code:
                            log(f"   email OTP: {eml_code}")
                            el = page.locator('input[name="code"]').first
                            if el.count() > 0:
                                el.fill(eml_code, force=True); time.sleep(0.5)
                                el.press("Enter"); time.sleep(5)
                time.sleep(2)
                _check_turnstile(page)
            log(f"   post-SMS: {page.title()} url={page.url[:80]}")

            # ── Step 6: Profile ──
            log("6. profile")
            name_el = page.locator('input[name="name"]').first
            if name_el.count() > 0:
                full_name = f"{fn} {ln}"
                for _ in range(5):
                    try:
                        if name_el.is_visible(timeout=3000): break
                    except: time.sleep(2)
                name_el.fill(full_name, force=True); log(f"   name: {full_name}")
                ae = page.locator('input[name="age"][type="number"]').first
                if ae.count() > 0: ae.fill(age, force=True); log(f"   age: {age}")
                time.sleep(1)
                page.locator('button[type="submit"]').last.click(force=True)
                log("   submitted")
                time.sleep(5)
                log(f"   after profile: {page.title()} url={page.url[:80]}")
                _check_turnstile(page)

            # ── Step 7: Oops recovery ──
            for rec in range(3):
                if "oops" not in (page.title() or "").lower(): break
                log(f"   oops recovery #{rec+1}")
                page.reload(); time.sleep(3)
                if page.locator('input[name="name"]').first.count() > 0:
                    page.locator('input[name="name"]').first.fill(full_name, force=True)
                    ae2 = page.locator('input[name="age"][type="number"]').first
                    if ae2.count() > 0: ae2.fill(age, force=True)
                    time.sleep(1)
                    page.locator('button[type="submit"]').last.click(force=True)
                    time.sleep(5)

            # ── Step 8: Post-signup ──
            log("8. post-signup")
            for _ in range(25):
                url = page.url
                if "chatgpt.com" in url and "/auth/" not in url: log("   ✅ in ChatGPT!"); break
                if "consent" in url.lower() or "workspace" in url.lower():
                    for t in ["Authorize","Allow","Accept","Continue","Continue to ChatGPT"]:
                        try:
                            btn = page.get_by_role("button", name=t)
                            if btn.count() > 0: btn.first.click(); log(f"   clicked {t}"); time.sleep(3); break
                        except: pass
                _check_turnstile(page)
                time.sleep(2)

            # ── Step 9: Extract session ──
            log("9. session")
            cookies = [{"name":c["name"],"value":c["value"],"domain":c["domain"],"path":c.get("path","/")} for c in page.context.cookies()]
            sc = next((c["value"] for c in cookies if c["name"] == "__Secure-next-auth.session-token"), "")
            sess = page.evaluate('async()=>{try{const r=await fetch("/api/auth/session",{credentials:"include"});return await r.json()}catch(e){return{error:e.message}}}')
            user = (sess or {}).get("user", {}) or {}
            acct = (sess or {}).get("account", {}) or {}
            at = (sess or {}).get("accessToken", "")

            if user.get("id") and (at or sc):
                result.update(success=True, email=EMAIL_BASE, user_id=user.get("id",""),
                    account_id=acct.get("id",""), plan_type=acct.get("planType","free"),
                    name=user.get("name",full_name), access_token=at, session_cookie=sc,
                    cookies=cookies, phone=intl)
                log(f"✅ SUCCESS user={user.get('id')} account={acct.get('id')}")
                print("__CODEX_ACCOUNT__ " + json.dumps(result, ensure_ascii=False), flush=True)
            else:
                result["error"] = f"no_session: title={page.title()}"
                log(f"no_session: {page.title()}")
            return result
        except Exception as e:
            log(f"ERROR: {e}"); import traceback; traceback.print_exc()
            result["error"] = str(e)
            return result
        finally:
            ctx.close()

def _check_turnstile(page):
    """Detect and solve Turnstile via yescaptcha."""
    try:
        ifs = page.locator('iframe[src*="challenges.cloudflare.com"]')
        if ifs.count() > 0:
            src = ifs.first.get_attribute("src", timeout=3000) or ""
            m = re.search(r"sitekey=([0-9a-zA-Z_-]+)", src)
            key = m.group(1) if m else ""
            if key:
                log(f"Turnstile detected! sitekey={key[:20]}... solving via yescaptcha")
                token = turnstile_solve(key, page.url)
                if token:
                    page.evaluate(f"""
                        (()=>{{
                            const els=document.querySelectorAll('[name="cf-turnstile-response"]');
                            els.forEach(e=>e.value='{token}');
                            if(window.turnstile)window.turnstile.render();
                        }})()
                    """)
                    log("   Turnstile token injected")
                    page.wait_for_timeout(4000)
                    return True
        div = page.locator('div.cf-turnstile')
        if div.count() > 0:
            key2 = div.first.get_attribute("data-sitekey", timeout=3000) or ""
            if key2:
                log(f"Turnstile div! sitekey={key2[:20]}... solving via yescaptcha")
                token = turnstile_solve(key2, page.url)
                if token:
                    page.evaluate(f"""
                        document.querySelectorAll('[name="cf-turnstile-response"]').forEach(e=>e.value='{token}')
                    """)
                    page.wait_for_timeout(3000)
    except Exception as e:
        pass
    return False

if __name__ == "__main__":
    r = main()
    if not r.get("success"):
        log(f"FAILED: {r.get('error','?')}")
