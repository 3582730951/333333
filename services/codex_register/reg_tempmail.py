#!/usr/bin/env python3
"""Register with tempmail.lol temp email + PKCE OAuth token exchange.

1. Get temp email from tempmail.lol API
2. Register on chatgpt.com with email OTP
3. Complete profile + phone verification (if needed)
4. Do PKCE OAuth flow → get tokens

Env: REG_PROXY_SERVER, REG_PROXY_USER, REG_PROXY_PASS, REG_CHROME, REG_HEADLESS
     SMS_COUNTRY (default PH), HEROSMS_API_KEY
Output: __CODEX_ACCOUNT__ {json}
"""
import base64, hashlib, json, os, re, random, secrets, sys, time, urllib.parse, urllib.request

PROXY_SERVER = os.environ.get("REG_PROXY_SERVER", "")
PROXY_USER   = os.environ.get("REG_PROXY_USER", "")
PROXY_PASS   = os.environ.get("REG_PROXY_PASS", "")
CHROME       = os.environ.get("REG_CHROME", os.environ.get("CHROME_PATH", ""))
HEADLESS     = os.environ.get("REG_HEADLESS", "1") != "0"
HEROSMS_KEY  = os.environ.get("HEROSMS_API_KEY", "")
SMS_COUNTRY  = os.environ.get("SMS_COUNTRY", "PH")

UA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"

STEALTH = """
Object.defineProperty(navigator, 'webdriver', {get: () => undefined});
delete Object.getOwnPropertyDescriptor(navigator.__proto__, 'webdriver');
if (!window.chrome) { window.chrome = {runtime: {}, app: {}}; }
Object.defineProperty(navigator, 'hardwareConcurrency', {get: () => 8});
Object.defineProperty(navigator, 'deviceMemory', {get: () => 8});
Object.defineProperty(navigator, 'languages', {get: () => ['en-US', 'en']});
window._fillReactInput = function(el, value) {
    const prev = String(el?.value ?? '');
    const tracker = el?._valueTracker;
    if (tracker && typeof tracker.setValue === 'function') {
        try { tracker.setValue.call(tracker, prev); } catch(e) {}
    }
    const ns = Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, 'value').set;
    el.focus();
    ns.call(el, value);
    el.dispatchEvent(new Event('input', { bubbles: true }));
    el.dispatchEvent(new Event('change', { bubbles: true }));
    el.dispatchEvent(new Event('blur', { bubbles: true }));
};
window._checkConsent = function() {
    const cb = document.querySelector('input[type="checkbox"]');
    if (cb && !cb.checked) { cb.click(); cb.dispatchEvent(new Event('change', {bubbles:true})); return true; }
    return !cb;
};
"""

OAUTH_AUTH_URL  = "https://auth.openai.com/oauth/authorize"
OAUTH_TOKEN_URL = "https://auth.openai.com/oauth/token"
OAUTH_CLIENT_ID = "app_EMoamEEZ73f0CkXaXp7hrann"
OAUTH_REDIRECT  = "http://localhost:1455/auth/callback"
OAUTH_SCOPE     = "openid profile email offline_access api.connectors.read api.connectors.invoke"
HEROSMS_BASE    = "https://hero-sms.com/stubs/handler_api.php"

COUNTRY_MAP = {"PH":"4","ID":"6","BR":"73","CL":"56","ZA":"27","TH":"52","VN":"10"}
DIAL_TO_CC = {"63":"PH","62":"ID","55":"BR","56":"CL","27":"ZA","66":"TH","84":"VN"}

FIRST_NAMES = ['James','John','Robert','Michael','William','David','Richard','Joseph',
    'Mary','Patricia','Jennifer','Linda','Barbara','Elizabeth','Susan','Jessica']
LAST_NAMES  = ['Smith','Johnson','Williams','Brown','Jones','Garcia','Miller','Davis']

def log(msg):
    print(f"[RT {time.strftime('%H:%M:%S')}] {msg}", file=sys.stderr, flush=True)

def rf(page, sel, val):
    page.evaluate(f'_fillReactInput(document.querySelector({json.dumps(sel)}), {json.dumps(str(val))})')

# ── tempmail.lol ────────────────────────────────────────────────────────
def tempmail_create():
    """Create a temp email via tempmail.lol API. Returns (email, token)."""
    req = urllib.request.Request('https://api.tempmail.lol/v2/inbox/create', data=b'{}',
        headers={'Content-Type':'application/json','User-Agent':UA})
    with urllib.request.urlopen(req, timeout=15) as r:
        data = json.loads(r.read().decode())
        return data['address'], data['token']

def tempmail_fetch_code(token, since_ts=0):
    """Poll tempmail.lol for OTP code. Returns code string or None."""
    deadline = time.time() + 180
    while time.time() < deadline:
        try:
            url = f'https://api.tempmail.lol/v2/inbox?token={token}'
            req = urllib.request.Request(url, headers={'User-Agent':UA})
            with urllib.request.urlopen(req, timeout=10) as r:
                data = json.loads(r.read().decode())
                emails = data.get('emails') or data.get('messages') or []
                if isinstance(data, list):
                    emails = data
                for msg in emails:
                    subject = msg.get('subject','')
                    body = msg.get('body','') or msg.get('html','') or msg.get('text','') or ''
                    combined = f"{subject} {body}"
                    m = re.search(r'(?<!\d)(\d{6})(?!\d)', combined)
                    if m:
                        return m.group(1).strip()
        except Exception as e:
            pass
        time.sleep(5)
    return None

# ── HeroSMS ─────────────────────────────────────────────────────────────
def hero_get_phone(country="PH"):
    cid = COUNTRY_MAP.get(str(country).upper(), "4")
    u = f"{HEROSMS_BASE}?{urllib.parse.urlencode({'api_key':HEROSMS_KEY,'action':'getNumber','service':'dr','country':cid})}"
    try:
        with urllib.request.urlopen(urllib.request.Request(u, headers={"User-Agent":UA}), timeout=15) as r:
            result = r.read().decode().strip()
            if result.startswith("ACCESS_NUMBER"):
                parts = result.split(":", 2)
                if len(parts) == 3: return parts[2], parts[1]
            log(f"   HeroSMS: {result}")
    except Exception as e: log(f"   HeroSMS err: {e}")
    return None, None

def hero_wait_sms(oid, timeout_s=120):
    deadline = time.time() + timeout_s
    while time.time() < deadline:
        try:
            u = f"{HEROSMS_BASE}?{urllib.parse.urlencode({'api_key':HEROSMS_KEY,'action':'getStatus','id':oid})}"
            with urllib.request.urlopen(urllib.request.Request(u, headers={"User-Agent":UA}), timeout=10) as r:
                result = r.read().decode().strip()
                if result.startswith("STATUS_OK"):
                    parts = result.split(":", 1)
                    if len(parts) == 2: return parts[1]
                elif result == "STATUS_CANCEL": return None
        except: pass
        time.sleep(5)
    return None

def hero_cancel(oid):
    try:
        u = f"{HEROSMS_BASE}?{urllib.parse.urlencode({'api_key':HEROSMS_KEY,'action':'setStatus','id':oid,'status':'8'})}"
        urllib.request.urlopen(urllib.request.Request(u, headers={"User-Agent":UA}), timeout=10)
    except: pass

# ── PKCE + OAuth ────────────────────────────────────────────────────────
def b64url(data): return base64.urlsafe_b64encode(data).rstrip(b"=").decode()
def gen_pkce():
    v = secrets.token_bytes(96)
    return b64url(v), b64url(hashlib.sha256(b64url(v).encode()).digest())

def build_oauth_url(challenge, state):
    p = {"client_id":OAUTH_CLIENT_ID,"response_type":"code","redirect_uri":OAUTH_REDIRECT,
         "scope":OAUTH_SCOPE,"state":state,"code_challenge":challenge,"code_challenge_method":"S256",
         "id_token_add_organizations":"true","codex_cli_simplified_flow":"true","originator":"codex_cli_rs"}
    return f"{OAUTH_AUTH_URL}?{urllib.parse.urlencode(p)}"

def exchange_code(code, verifier):
    body = urllib.parse.urlencode({"grant_type":"authorization_code","client_id":OAUTH_CLIENT_ID,
        "code":code,"redirect_uri":OAUTH_REDIRECT,"code_verifier":verifier}).encode()
    req = urllib.request.Request(OAUTH_TOKEN_URL, data=body, headers={
        "Content-Type":"application/x-www-form-urlencoded","Accept":"application/json","User-Agent":UA})
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            if r.status != 200: log(f"token exchange {r.status}"); return None
            return json.loads(r.read().decode())
    except Exception as e: log(f"token exchange err: {e}"); return None

# ── Dial code extraction ────────────────────────────────────────────────
KNOWN_DIAL_CODES = sorted([
    "1246","1264","1340","1345","1441","1473","1649","1664","1670","1671","1684",
    "1721","1758","1767","1784","1809","1829","1849","1868","1869","1876",
    "971","962","886","880","856","855","852","853","673","672","670","599","598","597","596",
    "595","594","593","592","591","590","509","508","507","506","505","504","503","502","501",
    "423","421","420","389","387","386","385","383","382","381","380","379","378","377","376",
    "375","374","373","372","371","370","359","358","357","356","355","354","353","352","351",
    "350","299","298","297","291","290","269","268","267","266","265","264","263","262","261",
    "260","258","257","256","255","254","253","252","251","250","249","248","247","246","245",
    "244","243","242","241","240","239","238","237","236","235","234","233","232","231","230",
    "229","228","227","226","225","224","223","222","221","220","218","216","213","212","211",
    "98","95","94","93","92","91","90","89","88","86","84","82","81","66","65","64","63",
    "62","61","60","58","57","56","55","54","53","52","51","49","48","47","46","45","44",
    "43","41","40","39","36","34","33","32","31","30","27","20","7","1",
], key=lambda x: -len(x))

def extract_dial(phone):
    digits = re.sub(r'\D', '', str(phone))
    for code in KNOWN_DIAL_CODES:
        if digits.startswith(code) and len(digits) > len(code):
            return code
    return ""

# ── Main ─────────────────────────────────────────────────────────────────
def main():
    # 0. Get temp email
    log("0. getting temp email from tempmail.lol...")
    email, tm_token = tempmail_create()
    log(f"   email: {email}")

    fn, ln = random.choice(FIRST_NAMES), random.choice(LAST_NAMES)
    age = str(random.randint(20, 30))
    full_name = f"{fn} {ln}"
    log(f"   profile: {full_name}, age {age}")

    # Generate PKCE for OAuth step
    code_verifier, code_challenge = gen_pkce()
    oauth_state = secrets.token_hex(16)
    oauth_url = build_oauth_url(code_challenge, oauth_state)

    from playwright.sync_api import sync_playwright

    with sync_playwright() as p:
        launch_args = {
            "user_data_dir": f"/tmp/rt_{os.urandom(4).hex()}",
            "headless": HEADLESS, "executable_path": CHROME or None,
            "args": ["--no-sandbox","--disable-gpu","--disable-dev-shm-usage",
                     "--disable-blink-features=AutomationControlled"],
            "user_agent": UA, "locale": "en-US", "viewport": {"width":1280,"height":720},
            "ignore_default_args": ["--enable-automation"],
        }
        if PROXY_SERVER and PROXY_SERVER.strip():
            launch_args["proxy"] = {"server": PROXY_SERVER, "username": PROXY_USER, "password": PROXY_PASS}
        ctx = p.chromium.launch_persistent_context(**launch_args)
        ctx.add_init_script(STEALTH)
        page = ctx.pages[0]

        try:
            # 1. Warm up chatgpt.com, then go to OAuth URL (proven working path)
            log("1. warmup chatgpt.com")
            page.goto("https://chatgpt.com/auth/login", wait_until="domcontentloaded", timeout=60000)
            for _ in range(30):
                if "just a moment" not in page.title().lower(): break
                time.sleep(3)
            log(f"   {page.title()}")

            # 2. Go to OAuth URL → auth.openai.com handles login (proven to work)
            log("2. → OAuth URL")
            page.goto(oauth_url, wait_until="domcontentloaded", timeout=60000)
            for _ in range(40):
                if "just a moment" not in page.title().lower(): break
                time.sleep(3)
            log(f"   {page.title()} | {page.url[:100]}")

            # 3. Handle login on auth.openai.com (proven working path)
            log(f"3. fill email: {email}")
            page.locator('input[name="email"]').first.fill(email, force=True, timeout=5000)
            time.sleep(0.5)
            for t in ["Continue","Next"]:
                try:
                    b = page.get_by_role("button", name=t)
                    if b.count() > 0: b.first.click(); break
                except: pass
            log("   submitted")
            time.sleep(5)
            for _ in range(20):
                if "just a moment" not in page.title().lower(): break
                time.sleep(3)
            log(f"   → {page.title()} | {page.url[:100]}")

            # 3. Wait for OTP input, fetch from tempmail.lol
            for _ in range(15):
                if page.locator('input[name="code"]').first.count() > 0: break
                if page.locator('input[name="password"], input[type="password"]').first.count() > 0:
                    log("   password page — this email already has an account")
                    return
                time.sleep(2)

            if page.locator('input[name="code"]').first.count() > 0:
                log("3. OTP page (SIGNUP!) — fetching from tempmail.lol...")
                code = tempmail_fetch_code(tm_token)
                if not code:
                    log("OTP TIMEOUT")
                    return
                log(f"   code: {code}")
                rf(page, 'input[name="code"]', code)
                time.sleep(0.5)
                page.locator('input[name="code"]').first.press("Enter")
                time.sleep(6)
                log(f"   → {page.title()} | {page.url[:120]}")
            elif page.locator('input[type="password"]').first.count() > 0:
                log("   ⚠️ email already has account — restarting with new temp email")
                ctx.close()
                time.sleep(1)
                main()  # retry with fresh email
                return

            # 4. Handle what comes next
            phone_order_id = None
            for rnd in range(12):
                url = page.url.lower()
                log(f"   R{rnd+1}: {page.title()} | {url[:100]}")

                for _ in range(30):
                    if "just a moment" not in page.title().lower(): break
                    time.sleep(3)

                # Done: OAuth redirect
                if "localhost:1455" in page.url and "code=" in page.url:
                    log("   🎯 OAuth code!")
                    break

                # Profile page
                if "about-you" in url:
                    log("   profile — filling...")
                    if page.locator('input[name="name"]').first.count() > 0:
                        rf(page, 'input[name="name"]', full_name)
                    if page.locator('input[name="age"][type="number"]').first.count() > 0:
                        rf(page, 'input[name="age"][type="number"]', age)
                    time.sleep(0.3)
                    page.evaluate('_checkConsent()')
                    for t in ["Continue","Next","Submit","Agree and continue"]:
                        try:
                            b = page.get_by_role("button", name=t)
                            if b.count() > 0 and b.first.is_visible():
                                b.first.click(); log(f"   '{t}'"); time.sleep(4); break
                        except: pass
                    log(f"   → {page.title()}")

                # Phone SMS verification
                elif "phone-verification" in url:
                    if phone_order_id:
                        sc = hero_wait_sms(phone_order_id, timeout_s=120)
                        if sc and page.locator('input[name="code"]').first.count() > 0:
                            log(f"   SMS: {sc}")
                            rf(page, 'input[name="code"]', sc)
                            time.sleep(0.5)
                            page.locator('input[name="code"]').first.press("Enter")
                            time.sleep(5)
                        hero_cancel(phone_order_id); phone_order_id = None
                    else:
                        for t in ["Continue","Authorize","Allow"]:
                            try:
                                b = page.get_by_role("button", name=t)
                                if b.count() > 0: b.first.click(); time.sleep(3); break
                            except: pass

                # Phone number entry
                elif "add-phone" in url:
                    if not phone_order_id:
                        log(f"   phone ({SMS_COUNTRY})...")
                        ph, phone_order_id = hero_get_phone(SMS_COUNTRY)
                        if ph:
                            # Select country
                            dial = extract_dial(ph)
                            cc = DIAL_TO_CC.get(dial, "")
                            if cc:
                                page.evaluate(f'''
                                    const s=document.querySelector('select');
                                    if(s){{for(const o of s.options){{if(o.value==="{cc}"){{s.value=o.value;
                                    s.dispatchEvent(new Event("change",{{bubbles:true}}));break;}}}}}}
                                ''')
                            # Fill phone
                            sel = 'input[name="phone"], input[type="tel"]'
                            if page.locator(sel).first.count() > 0:
                                rf(page, sel, ph)
                                log(f"   phone: {ph}")
                                time.sleep(0.5)
                                for t in ["Send code","Continue","Next"]:
                                    try:
                                        b = page.get_by_role("button", name=t)
                                        if b.count() > 0: b.first.click(); break
                                    except: pass
                                time.sleep(4)

                # Consent/authorize
                elif ("consent" in url or "authorize" in url) and "just a moment" not in page.title().lower():
                    log("   consent...")
                    for t in ["Authorize","Allow","Accept","Continue","Confirm"]:
                        try:
                            b = page.get_by_role("button", name=t)
                            if b.count() > 0 and b.first.is_visible():
                                b.first.click(); log(f"   '{t}'"); time.sleep(3); break
                        except: pass

                # On chatgpt.com — logged in, go to OAuth
                elif "chatgpt.com" in url and "/auth/" not in url and "login" not in url:
                    log("   ✅ logged in — → OAuth URL")
                    sess = page.evaluate(
                        'async () => { try { const r = await fetch("/api/auth/session",{credentials:"include"}); return await r.json(); } catch(e) { return {}; } }'
                    )
                    acct = (sess or {}).get("account", {}) or {}
                    user = (sess or {}).get("user", {}) or {}
                    log(f"   session: user={user.get('id','?')} plan={acct.get('planType','free')}")
                    page.goto(oauth_url, wait_until="domcontentloaded", timeout=30000)
                    time.sleep(4)
                    continue

                # Error page
                elif "error" in url:
                    log(f"   ⚠️ error: {page.title()}")

                time.sleep(2)

            # ── Extract code + exchange ──────────────────────────────────
            oauth_code = None
            if "localhost:1455" in page.url and "code=" in page.url:
                p_url = urllib.parse.urlparse(page.url)
                q = urllib.parse.parse_qs(p_url.query)
                oauth_code = q.get("code", [None])[0]

            sess = {}
            try:
                sess = page.evaluate(
                    'async () => { try { const r = await fetch("/api/auth/session",{credentials:"include"}); return await r.json(); } catch(e) { return {}; } }'
                )
            except: pass
            acct = (sess or {}).get("account", {}) or {}
            user = (sess or {}).get("user", {}) or {}
            session_at = (sess or {}).get("accessToken", "")

            oa_at = oa_rt = oa_id = ""
            if oauth_code:
                log(f"   exchanging code...")
                tr = exchange_code(oauth_code, code_verifier)
                if tr:
                    oa_at = tr.get("access_token","")
                    oa_rt = tr.get("refresh_token","")
                    oa_id = tr.get("id_token","")
                    log(f"✅ OAuth: at={'✓' if oa_at else '✗'} rt={'✓' if oa_rt else '✗'} id={'✓' if oa_id else '✗'}")

            final_at = oa_at or session_at
            if final_at:
                result = {"type":"codex","email":email,"account_id":acct.get("id",""),
                          "user_id":user.get("id",""),"plan_type":acct.get("planType","free"),
                          "name":user.get("name",full_name),"access_token":final_at,
                          "refresh_token":oa_rt,"id_token":oa_id,"session_token":session_at}
                log(f"✅ DONE rt={'yes' if oa_rt else 'no'}")
                print("__CODEX_ACCOUNT__ " + json.dumps(result, ensure_ascii=False), flush=True)
            else:
                log(f"❌ no token — final page: {page.title()} | {page.url[:100]}")

        except Exception as e:
            log(f"ERROR: {e}")
            import traceback; traceback.print_exc()
        finally:
            ctx.close()

if __name__ == "__main__":
    main()
