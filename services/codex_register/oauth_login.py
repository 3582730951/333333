#!/usr/bin/env python3
"""OAuth-first login: start at chatgpt.com, then navigate to the Codex PKCE
authorize URL. Let OpenAI handle the login redirect flow, complete phone
verification if needed, capture the OAuth code, exchange for tokens.

This mirrors the exact Codex CLI flow + GuJumpgate browser OAuth approach.
Output: __CODEX_ACCOUNT__ {json} with access_token, refresh_token, id_token.
"""
import base64, hashlib, json, os, re, random, secrets, sys, time, urllib.parse, urllib.request

PROXY_SERVER = os.environ.get("REG_PROXY_SERVER", "")
PROXY_USER   = os.environ.get("REG_PROXY_USER", "")
PROXY_PASS   = os.environ.get("REG_PROXY_PASS", "d6kfytmo")
EMAIL        = os.environ.get("REG_EMAIL", "")
OTP_URL      = os.environ.get("REG_OTP_URL", "")
CHROME       = os.environ.get("REG_CHROME", os.environ.get("CHROME_PATH", ""))
HEADLESS     = os.environ.get("REG_HEADLESS", "1") != "0"
HEROSMS_KEY  = os.environ.get("HEROSMS_API_KEY", "810154d173c3c562B1ed124418c8f7B3")
SMS_COUNTRY  = os.environ.get("SMS_COUNTRY", "PH")  # PH=Philippines, ID=Indonesia
PASSWORD     = os.environ.get("REG_PASSWORD", "")

# Sibling module: the correct GuJumpgate-ported add-phone handler (replaces the broken
# inline double-dial-code / no-WhatsApp / no-loop-detection logic that used to live here).
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
try:
    from phone_verify import verify_phone
except Exception as _pv_err:  # pragma: no cover
    verify_phone = None
    print(f"[OA] WARN: phone_verify import failed: {_pv_err}", file=sys.stderr, flush=True)

UA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"

STEALTH = """
Object.defineProperty(navigator, 'webdriver', {get: () => undefined});
delete Object.getOwnPropertyDescriptor(navigator.__proto__, 'webdriver');
if (!window.chrome) { window.chrome = {runtime: {}, app: {}}; }
Object.defineProperty(navigator, 'hardwareConcurrency', {get: () => 8});
Object.defineProperty(navigator, 'deviceMemory', {get: () => 8});
Object.defineProperty(navigator, 'languages', {get: () => ['en-US', 'en']});

// React controlled-input fill (GuJumpgate technique with _valueTracker sync).
// React Fiber attaches a _valueTracker to each controlled <input>. Without syncing
// it before the native setter call, React's reconciliation sees the old value and
// resets the field on next render — causing form validation to fail silently.
window._fillReactInput = function(el, value) {
    if (!el) return;
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

// Consent checkbox handler (for about-you page)
window._checkConsent = function() {
    const cb = document.querySelector('input[type="checkbox"]');
    if (cb && !cb.checked) {
        cb.click();
        cb.dispatchEvent(new Event('change', { bubbles: true }));
        return true;
    }
    return !cb; // no checkbox = ok
};
"""

OAUTH_AUTH_URL  = "https://auth.openai.com/oauth/authorize"
OAUTH_TOKEN_URL = "https://auth.openai.com/oauth/token"
OAUTH_CLIENT_ID = "app_EMoamEEZ73f0CkXaXp7hrann"
OAUTH_REDIRECT  = "http://localhost:1455/auth/callback"
OAUTH_SCOPE     = "openid profile email offline_access api.connectors.read api.connectors.invoke"
HEROSMS_BASE    = "https://hero-sms.com/stubs/handler_api.php"

# HeroSMS country IDs (from getPrices API). Service "dr" (OpenAI) prices in USD.
# Cheapest: PH($0.025) > ID($0.045)=BR($0.045) > CL($0.10)=ZA($0.10) > TH($0.30)
COUNTRY_MAP = {
    "PH": "4",   # Philippines  $0.025 ⭐ cheapest
    "ID": "6",   # Indonesia    $0.045
    "BR": "73",  # Brazil       $0.045
    "CL": "56",  # Chile        $0.10
    "ZA": "27",  # South Africa $0.10
    "VN": "10",  # Vietnam      $0.25
    "TH": "52",  # Thailand     $0.30
    "MY": "7",   # Malaysia     $0.10
    "IN": "22",  # India        $0.35
    "UK": "16",  # UK           $0.03 (often no numbers)
}

def log(msg):
    print(f"[OA {time.strftime('%H:%M:%S')}] {msg}", file=sys.stderr, flush=True)

def rf(page, sel, val):
    """React-safe fill using native value setter."""
    page.evaluate(f'_fillReactInput(document.querySelector({json.dumps(sel)}), {json.dumps(str(val))})')

# ── Email OTP ──────────────────────────────────────────────────────────
def fetch_otp(since_ts):
    deadline = time.time() + 180
    while time.time() < deadline:
        try:
            req = urllib.request.Request(OTP_URL, headers={"User-Agent": UA})
            with urllib.request.urlopen(req, timeout=10) as r:
                data = json.loads(r.read().decode())
        except: time.sleep(5); continue
        for m in data.get("emails", []):
            mt = m.get("时间","") or m.get("receivedAt","") or "2000-01-01"
            if mt < since_ts: continue
            txt = f"{m.get('主题','')} {m.get('内容预览','')} {m.get('html','')} {m.get('subject','')} {m.get('body','')}"
            mx = re.search(r"(?<!\d)(\d{6})(?!\d)", txt)
            if mx and mx.group(1) != "177010": return mx.group(1).strip()
        time.sleep(5)
    return None

# ── HeroSMS ─────────────────────────────────────────────────────────────
# Phone prefix → dial code mapping (longest-match-first for accuracy)
KNOWN_DIAL_CODES = [
    "1246","1264","1268","1284","1340","1345","1441","1473","1649","1664","1670","1671","1684",
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
]
KNOWN_DIAL_CODES.sort(key=lambda x: -len(x))  # longest first

# Dial code → country code/name mapping for OpenAI's country selector
DIAL_TO_COUNTRY = {
    "62": "ID",     # Indonesia
    "63": "PH",     # Philippines
    "84": "VN",     # Vietnam
    "66": "TH",     # Thailand
    "60": "MY",     # Malaysia
    "91": "IN",     # India
    "1": "US",      # United States
    "44": "GB",     # United Kingdom
    "86": "CN",     # China
    "81": "JP",     # Japan
    "82": "KR",     # South Korea
    "7": "RU",      # Russia
    "55": "BR",     # Brazil
    "52": "MX",     # Mexico
    "61": "AU",     # Australia
    "49": "DE",     # Germany
    "33": "FR",     # France
    "39": "IT",     # Italy
    "34": "ES",     # Spain
    "31": "NL",     # Netherlands
    "46": "SE",     # Sweden
    "47": "NO",     # Norway
    "45": "DK",     # Denmark
    "358": "FI",    # Finland
    "48": "PL",     # Poland
    "43": "AT",     # Austria
    "41": "CH",     # Switzerland
    "32": "BE",     # Belgium
    "351": "PT",    # Portugal
    "30": "GR",     # Greece
    "36": "HU",     # Hungary
    "420": "CZ",    # Czech Republic
    "40": "RO",     # Romania
    "359": "BG",    # Bulgaria
    "353": "IE",    # Ireland
    "64": "NZ",     # New Zealand
    "65": "SG",     # Singapore
    "852": "HK",    # Hong Kong
    "886": "TW",    # Taiwan
    "90": "TR",     # Turkey
    "971": "AE",    # UAE
    "972": "IL",    # Israel
    "20": "EG",     # Egypt
    "27": "ZA",     # South Africa
    "234": "NG",    # Nigeria
    "254": "KE",    # Kenya
    "212": "MA",    # Morocco
    "213": "DZ",    # Algeria
    "216": "TN",    # Tunisia
    "92": "PK",     # Pakistan
    "880": "BD",    # Bangladesh
    "94": "LK",     # Sri Lanka
    "95": "MM",     # Myanmar
    "98": "IR",     # Iran
    "964": "IQ",    # Iraq
    "966": "SA",    # Saudi Arabia
    "962": "JO",    # Jordan
    "961": "LB",    # Lebanon
    "963": "SY",    # Syria
    "380": "UA",    # Ukraine
    "375": "BY",    # Belarus
    "373": "MD",    # Moldova
    "370": "LT",    # Lithuania
    "371": "LV",    # Latvia
    "372": "EE",    # Estonia
    "374": "AM",    # Armenia
    "994": "AZ",    # Azerbaijan
    "995": "GE",    # Georgia
    "996": "KG",    # Kyrgyzstan
    "998": "UZ",    # Uzbekistan
    "63": "PH",     # Philippines ⭐ $0.025 cheapest
    "56": "CL",     # Chile        $0.10
    "57": "CO",     # Colombia     (no numbers for dr)
    "27": "ZA",     # South Africa $0.10
    "51": "PE",     # Peru
    "54": "AR",     # Argentina
    "58": "VE",     # Venezuela
    "593": "EC",    # Ecuador
    "591": "BO",    # Bolivia
    "595": "PY",    # Paraguay
    "598": "UY",    # Uruguay
    "506": "CR",    # Costa Rica
    "507": "PA",    # Panama
    "53": "CU",     # Cuba
    "509": "HT",    # Haiti
    "1809": "DO",   # Dominican Republic
    "1829": "DO",
    "1849": "DO",
}

def extract_dial_code(phone_number):
    """Extract the country dial code from a phone number.
    e.g. '6285138449173' → '62', '639695702184' → '63', '14155551234' → '1'
    """
    digits = re.sub(r'\D', '', str(phone_number))
    for code in KNOWN_DIAL_CODES:
        if digits.startswith(code) and len(digits) > len(code):
            return code
    return ""

def hero_get_phone(country="PH"):
    cid = COUNTRY_MAP.get(str(country).upper(), "6")
    u = f"{HEROSMS_BASE}?{urllib.parse.urlencode({'api_key':HEROSMS_KEY,'action':'getNumber','service':'dr','country':cid})}"
    try:
        with urllib.request.urlopen(urllib.request.Request(u, headers={"User-Agent":UA}), timeout=15) as r:
            result = r.read().decode().strip()
            log(f"   HeroSMS: {result}")
            if result.startswith("ACCESS_NUMBER"):
                parts = result.split(":", 2)
                if len(parts) == 3: return parts[2], parts[1]
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
            if r.status != 200: log(f"token exchange {r.status}: {(r.read()[:300])}"); return None
            return json.loads(r.read().decode())
    except Exception as e: log(f"token exchange err: {e}"); return None

# ── Main ─────────────────────────────────────────────────────────────────
def main():
    log(f"OAuth: {EMAIL} country={SMS_COUNTRY}")

    from playwright.sync_api import sync_playwright

    # Generate PKCE before launching browser
    code_verifier, code_challenge = gen_pkce()
    oauth_state = secrets.token_hex(16)
    oauth_url = build_oauth_url(code_challenge, oauth_state)
    log(f"oauth: {oauth_url[:120]}...")

    with sync_playwright() as p:
        launch_args = {
            "user_data_dir": f"/tmp/oa_{os.urandom(4).hex()}",
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
            # Go directly to OAuth URL — auth.openai.com handles login redirect
            # (proven: UK proxy reaches auth.openai.com in 3s with zero CF)
            log("1. → OAuth URL directly")
            page.goto(oauth_url, wait_until="domcontentloaded", timeout=60000)
            for _ in range(60):
                if "just a moment" not in page.title().lower(): break
                time.sleep(3)
            log(f"   {page.title()} | {page.url[:120]}")

            since_ts = time.strftime("%Y-%m-%dT%H:%M", time.gmtime())
            phone_order_id = None

            # 3. Handle pages round-robin
            for rnd in range(8):
                url = page.url.lower()
                log(f"   R{rnd+1}: {page.title()} | {url[:120]}")

                # CF wait — auth.openai.com can take 2+ minutes
                for _ in range(60):
                    if "just a moment" not in page.title().lower(): break
                    time.sleep(3)
                # Still on CF? Skip all handlers — don't click buttons on challenge page
                if "just a moment" in page.title().lower():
                    log("   still CF, waiting...")
                    time.sleep(5)
                    continue

                # ── HANDLERS (priority order) ──

                # DONE: localhost redirect with code
                if "localhost:1455" in page.url and "code=" in page.url:
                    log("   🎯 OAuth code captured!")
                    break

                # Phone step (add-phone or phone-verification) — delegated to the correct
                # GuJumpgate-ported handler. The old inline code filled the FULL number
                # after selecting the country (double dial code -> invalid -> endless loop),
                # never forced the SMS channel (WhatsApp-only -> SMS never arrives), and had
                # no reject/loop detection. phone_verify fixes all three.
                if verify_phone and ("phone-verification" in url or "add-phone" in url or "contact-verification" in url):
                    log("   📱 phone step — running phone_verify")
                    pr = verify_phone(page, hero_key=HEROSMS_KEY, country=SMS_COUNTRY, log=log)
                    log(f"   📱 phone_verify: ok={pr.get('ok')} phone={pr.get('phone','')} err={pr.get('error','')}")
                    time.sleep(2)
                    continue

                # Consent/authorize (only if NOT on CF challenge page)
                if ("consent" in url or "authorize" in url) and "just a moment" not in page.title().lower():
                    log("   consent page — clicking...")
                    for t in ["Authorize","Allow","Accept","Continue","Confirm"]:
                        try:
                            b = page.get_by_role("button", name=t)
                            if b.count() > 0 and b.first.is_visible():
                                b.first.click(); log(f"   '{t}'"); time.sleep(3); break
                        except: pass
                    for s in ['button[type="submit"]']:
                        try:
                            el = page.locator(s).first
                            if el.count() > 0 and el.is_visible():
                                el.click(); log(f"   clicked {s}"); time.sleep(3); break
                        except: pass

                # Login/auth (but NOT add-phone or phone-verification)
                elif ("login" in url or "signin" in url or
                      ("/log-in" in url or "/signup" in url or "email-verification" in url)):
                    log("   login/auth page")

                    # Email — use native fill first, fallback to react_fill
                    if page.locator('input[name="email"], input[type="email"]').first.count() > 0:
                        try:
                            page.locator('input[name="email"]').first.fill(EMAIL, force=True, timeout=3000)
                            log(f"   email (native): {EMAIL}")
                        except Exception as e:
                            log(f"   native fill err: {e}, trying rf")
                            rf(page, 'input[name="email"]', EMAIL)
                            log(f"   email (rf): {EMAIL}")
                        time.sleep(0.5)
                        # Try Continue button OR press Enter
                        clicked = False
                        for t in ["Continue","Next"]:
                            try:
                                b = page.get_by_role("button", name=t)
                                if b.count() > 0 and b.first.is_visible():
                                    b.first.click(); log(f"   '{t}'"); clicked = True; break
                            except: pass
                        if not clicked:
                            try:
                                page.locator('input[name="email"]').first.press("Enter")
                                log("   pressed Enter")
                            except: pass
                        since_ts = time.strftime("%Y-%m-%dT%H:%M", time.gmtime())
                        time.sleep(5)

                    # OTP input
                    for _ in range(10):
                        if page.locator('input[name="code"]').first.count() > 0: break
                        if page.locator('input[type="password"]').first.count() > 0: break
                        time.sleep(1)

                    if page.locator('input[name="code"]').first.count() > 0:
                        log("   OTP...")
                        c = fetch_otp(since_ts)
                        if not c: log("OTP TIMEOUT"); return
                        log(f"   code: {c}")
                        rf(page, 'input[name="code"]', c)
                        time.sleep(0.5)
                        page.locator('input[name="code"]').first.press("Enter")
                        time.sleep(5)
                    elif page.locator('input[type="password"]').first.count() > 0:
                        if PASSWORD:
                            # Use the provided password
                            log(f"   password page — filling (pw len={len(PASSWORD)})")
                            rf(page, 'input[type="password"]', PASSWORD)
                            time.sleep(0.5)
                            # Try button click first, then Enter
                            clicked = False
                            for t in ["Continue","Log in","Sign in","Next","Submit"]:
                                try:
                                    b = page.get_by_role("button", name=t)
                                    if b.count() > 0 and b.first.is_visible():
                                        b.first.click(); log(f"   clicked '{t}'"); clicked = True; break
                                except: pass
                            if not clicked:
                                page.locator('input[type="password"]').first.press("Enter")
                                log("   pressed Enter")
                            log("   password submitted")
                            time.sleep(6)
                            log(f"   → {page.title()} | {page.url[:120]}")
                        else:
                            log("   password page — switch to OTP")
                            for t in ["Use a code","Verify with a code","Try another way",
                                       "Use a one-time code","Log in with a code",
                                       "Email me a code","Send code","code"]:
                                try:
                                    l = page.get_by_text(t)
                                    if l.count() > 0: l.first.click(); log(f"   '{t}'"); time.sleep(3); break
                                except: pass
                                try:
                                    b = page.get_by_role("button", name=t)
                                    if b.count() > 0: b.first.click(); log(f"   btn:'{t}'"); time.sleep(3); break
                                except: pass

                # Already logged in at chatgpt.com
                elif "chatgpt.com" in url and "/auth/" not in url and "login" not in url:
                    log("   logged in! → OAuth URL")
                    page.goto(oauth_url, wait_until="domcontentloaded", timeout=30000)
                    time.sleep(4)
                    continue

                # About-you (profile) — MUST complete to continue OAuth flow
                elif "about-you" in url:
                    log("   profile page — filling with valueTracker fix...")
                    name_val = random.choice(["Alex","Jordan","Taylor","Morgan","Casey","Riley"])
                    age_val = str(random.randint(20, 30))

                    # 1. Fill name with _valueTracker sync (via rf/STEALTH _fillReactInput)
                    if page.locator('input[name="name"]').first.count() > 0:
                        rf(page, 'input[name="name"]', name_val)
                        log(f"   name: {name_val}")
                        time.sleep(random.uniform(0.3, 0.7))

                    # 2. Fill age with _valueTracker sync
                    if page.locator('input[name="age"][type="number"]').first.count() > 0:
                        rf(page, 'input[name="age"][type="number"]', age_val)
                        log(f"   age: {age_val}")
                        time.sleep(random.uniform(0.3, 0.7))

                    # 3. Check consent checkbox (GuJumpgate technique)
                    cb_result = page.evaluate('_checkConsent()')
                    if cb_result is True:
                        log("   consent: checked")
                    time.sleep(0.3)

                    # 4. Submit — try keyboard Enter first, then button click
                    try:
                        page.locator('input[name="age"][type="number"]').first.press("Enter")
                        log("   pressed Enter")
                    except: pass
                    time.sleep(3)

                    if "about-you" in page.url.lower():
                        for t in ["Continue","Next","Submit","Agree and continue"]:
                            try:
                                b = page.get_by_role("button", name=t)
                                if b.count() > 0 and b.first.is_visible():
                                    b.first.click()
                                    log(f"   clicked '{t}'")
                                    time.sleep(4)
                                    break
                            except: pass
                    log(f"   → {page.title()} | {page.url[:120]}")

                # Error
                elif "error" in url:
                    log(f"   ⚠️ error: {page.title()}")

                time.sleep(3)

            # ── 4. Extract code + exchange ─────────────────────────────
            log(f"4. final: {page.title()} | {page.url[:120]}")

            # Parse code from URL
            oauth_code = None
            if "localhost:1455" in page.url and "code=" in page.url:
                p = urllib.parse.urlparse(page.url)
                q = urllib.parse.parse_qs(p.query)
                oauth_code = q.get("code", [None])[0]
                if not oauth_code and p.fragment:
                    oauth_code = urllib.parse.parse_qs(p.fragment).get("code", [None])[0]

            # Extract session for metadata
            sess = {}
            try:
                sess = page.evaluate(
                    'async () => { try { const r = await fetch("/api/auth/session",{credentials:"include"}); return await r.json(); } catch(e) { return {error:e.message}; } }'
                )
            except: pass
            acct = (sess or {}).get("account", {}) or {}
            user = (sess or {}).get("user", {}) or {}
            session_at = (sess or {}).get("accessToken", "")

            # Exchange code for OAuth tokens
            oa_at = oa_rt = oa_id = ""
            if oauth_code:
                log(f"   exchanging code ({oauth_code[:20]}...)")
                tr = exchange_code(oauth_code, code_verifier)
                if tr:
                    oa_at = tr.get("access_token","")
                    oa_rt = tr.get("refresh_token","")
                    oa_id = tr.get("id_token","")
                    log(f"✅ OAuth: at={'✓' if oa_at else '✗'} rt={'✓' if oa_rt else '✗'} id={'✓' if oa_id else '✗'} exp={tr.get('expires_in',0)}s")
                else:
                    log("⚠️ token exchange failed")
            else:
                log("⚠️ no OAuth code — session token only")

            final_at = oa_at or session_at
            if not final_at:
                log("❌ no token")
                return

            result = {"type":"codex","email":EMAIL,"account_id":acct.get("id",""),
                      "user_id":user.get("id",""),"plan_type":acct.get("planType","free"),
                      "name":user.get("name",""),"access_token":final_at,
                      "refresh_token":oa_rt,"id_token":oa_id,"session_token":session_at}
            log(f"✅ DONE user={user.get('id','?')} rt={'yes' if oa_rt else 'no'}")
            print("__CODEX_ACCOUNT__ " + json.dumps(result, ensure_ascii=False), flush=True)

        except Exception as e:
            log(f"ERROR: {e}")
            import traceback; traceback.print_exc()
        finally:
            ctx.close()

if __name__ == "__main__":
    main()
