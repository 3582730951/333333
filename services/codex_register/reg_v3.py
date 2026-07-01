#!/usr/bin/env python3
"""ChatGPT registration — GuJumpgate v3 techniques, dynamic proxy from env/pool_server.

Reads proxy + email + OTP URL from env vars (injected by pool_server's Go pipeline from
the admin-configured egress profiles), so no hardcoded IPs. See browser_register_v3.py
in other_project/scripts for the original that inspired this adaptation.

After registration, performs a full PKCE OAuth token exchange (matching the real Codex
CLI flow) so the account gets a refresh_token and id_token — the web-session accessToken
alone expires in ~6h and cannot refresh. This mirrors the GuJumpgate browser OAuth flow.

Env vars (all set by pool_server):
  REG_PROXY_SERVER    e.g. http://152.32.235.240:3010  (or 43.230.8.144:3010)
  REG_PROXY_USER      e.g. zdvw1182255-region-Rand-sid-XXXX
  REG_PROXY_PASS      e.g. d6kfytmo
  REG_EMAIL           e.g. xnzsilq+tag@hotmail.com
  REG_OTP_URL         e.g. http://185.242.234.133:8000/get_email/TOKEN
  REG_CHROME          chrome binary path (default: CHROME_PATH env)
  REG_HEADLESS        0=headed, 1=headless (default headless)
Output: __CODEX_ACCOUNT__ {json} on stdout, step logs to stderr.
"""
import base64
import hashlib
import json
import os
import re
import random
import secrets
import sys
import time
import urllib.parse
import urllib.request

PROXY_SERVER = os.environ.get("REG_PROXY_SERVER", "")
PROXY_USER   = os.environ.get("REG_PROXY_USER", "")
PROXY_PASS   = os.environ.get("REG_PROXY_PASS", "d6kfytmo")
EMAIL        = os.environ.get("REG_EMAIL", "")
OTP_URL      = os.environ.get("REG_OTP_URL", "")
CHROME       = os.environ.get("REG_CHROME", os.environ.get("CHROME_PATH", ""))
HEADLESS     = os.environ.get("REG_HEADLESS", "1") != "0"
# hero-sms for the OAuth-stage add-phone step (only used if OpenAI demands a phone).
HEROSMS_KEY  = os.environ.get("HEROSMS_KEY", os.environ.get("HEROSMS_API_KEY", "810154d173c3c562B1ed124418c8f7B3"))
SMS_COUNTRY  = os.environ.get("REG_SMS_COUNTRY", "PH")  # must match the proxy region

# Sibling module: the correct GuJumpgate-ported add-phone handler.
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
try:
    from phone_verify import verify_phone
except Exception as _pv_err:  # pragma: no cover
    verify_phone = None
    print(f"[v3] WARN: phone_verify import failed: {_pv_err}", file=sys.stderr, flush=True)


def maybe_handle_phone(page, log_fn):
    """If the current page is the add-phone / phone-verification step, complete it.
    Returns True if a phone step was handled (caller should re-loop), False otherwise."""
    if verify_phone is None:
        return False
    low = (page.url or "").lower()
    if not any(k in low for k in ("add-phone", "phone-verification", "contact-verification")):
        # Also catch the form even when the URL hasn't changed (SPA navigation).
        try:
            has_form = page.evaluate(
                "() => !!document.querySelector('form[action*=\"/add-phone\" i], form[action*=\"/phone-verification\" i]')"
            )
        except Exception:
            has_form = False
        if not has_form:
            return False
    log_fn(f"   📱 add-phone step detected (country={SMS_COUNTRY}) — running phone_verify")
    res = verify_phone(page, hero_key=HEROSMS_KEY, country=SMS_COUNTRY, log=log_fn)
    log_fn(f"   📱 phone_verify: ok={res.get('ok')} phone={res.get('phone','')} err={res.get('error','')}")
    time.sleep(2)
    return True

FIRST_NAMES = ['James','John','Robert','Michael','William','David','Richard','Joseph','Thomas',
    'Mary','Patricia','Jennifer','Linda','Barbara','Elizabeth','Susan','Jessica','Sarah',
    'Daniel','Matthew','Anthony','Mark','Donald','Steven','Andrew','Paul','Joshua','Emma']
LAST_NAMES  = ['Smith','Johnson','Williams','Brown','Jones','Garcia','Miller','Davis','Rodriguez',
    'Martinez','Hernandez','Lopez','Gonzalez','Wilson','Anderson','Thomas','Taylor','Jackson']
UA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"
STEALTH = """
Object.defineProperty(navigator, 'webdriver', {get: () => undefined});
delete Object.getOwnPropertyDescriptor(navigator.__proto__, 'webdriver');
if (!window.chrome) { window.chrome = {runtime: {}, app: {}}; }
Object.defineProperty(navigator, 'hardwareConcurrency', {get: () => 8});
Object.defineProperty(navigator, 'deviceMemory', {get: () => 8});
Object.defineProperty(navigator, 'languages', {get: () => ['en-US', 'en']});

// React controlled-input hack (GuJumpgate technique with _valueTracker sync):
// ChatGPT uses React, which tracks input values through its synthetic event
// system. Playwright's fill() only sets the DOM .value property — React never
// sees it and submits stale/empty data, causing the server to bounce back to
// the login page (looks like a "refresh"). This native-setter + dispatchEvent
// combo makes React believe the user typed the value.
//
// CRITICAL: React Fiber attaches _valueTracker to controlled inputs. Without
// syncing it before the native setter, React resets the field on next render.
window._fillReactInput = function(el, value) {
    if (!el) return;
    const prev = String(el?.value ?? '');
    const tracker = el?._valueTracker;
    if (tracker && typeof tracker.setValue === 'function') {
        try { tracker.setValue.call(tracker, prev); } catch(e) {}
    }
    const nativeSetter = Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, 'value').set;
    el.focus();
    nativeSetter.call(el, value);
    el.dispatchEvent(new Event('input', { bubbles: true }));
    el.dispatchEvent(new Event('change', { bubbles: true }));
    el.dispatchEvent(new Event('blur', { bubbles: true }));
};

// Consent checkbox helper (for about-you page)
window._checkConsent = function() {
    const cb = document.querySelector('input[type="checkbox"]');
    if (cb && !cb.checked) {
        cb.click();
        cb.dispatchEvent(new Event('change', { bubbles: true }));
        return true;
    }
    return !cb;
};
"""

# ── OAuth constants (matching the real Codex CLI byte-for-byte) ──────────────
OAUTH_AUTH_URL  = "https://auth.openai.com/oauth/authorize"
OAUTH_TOKEN_URL = "https://auth.openai.com/oauth/token"
OAUTH_CLIENT_ID = "app_EMoamEEZ73f0CkXaXp7hrann"
OAUTH_REDIRECT  = "http://localhost:1455/auth/callback"
OAUTH_SCOPE     = "openid profile email offline_access api.connectors.read api.connectors.invoke"

def log(msg):
    print(f"[v3 {time.strftime('%H:%M:%S')}] {msg}", file=sys.stderr, flush=True)

def react_fill(page, selector, value):
    """Fill a React-controlled input using the native value setter hack.

    ChatGPT uses React; Playwright's locator.fill() only sets the DOM .value
    property, which React ignores. The native setter + dispatchEvent combo
    tells React the input changed, so the form submits with real data instead
    of bouncing back to the login page (looking like a "refresh").
    """
    escaped = json.dumps(str(value))
    page.evaluate(f'_fillReactInput(document.querySelector({json.dumps(selector)}), {escaped})')

def fetch_otp(since_ts):
    """Poll for a NEW OTP code (received after since_ts)."""
    deadline = time.time() + 150
    while time.time() < deadline:
        try:
            req = urllib.request.Request(OTP_URL, headers={"User-Agent": UA})
            with urllib.request.urlopen(req, timeout=10) as resp:
                data = json.loads(resp.read().decode())
        except Exception:
            time.sleep(5); continue
        for mail in data.get("emails", []):
            # Only accept OTPs that arrived AFTER this registration started
            mail_ts = mail.get("时间", "") or mail.get("receivedAt", "") or "2000-01-01"
            if mail_ts < since_ts:
                continue
            combined = f"{mail.get('主题','')} {mail.get('内容预览','')} {mail.get('html','')} {mail.get('subject','')} {mail.get('body','')}"
            m = re.search(r"(?<!\d)(\d{6})(?!\d)", combined)
            if m and m.group(1) != "177010":
                return m.group(1).strip()
        time.sleep(5)
    return None

# ── PKCE OAuth helpers ────────────────────────────────────────────────────────

def b64url(data: bytes) -> str:
    """base64url-no-pad encode (matching the standard PKCE spec)."""
    return base64.urlsafe_b64encode(data).rstrip(b"=").decode()

def generate_pkce():
    """Generate a (code_verifier, code_challenge) pair per RFC 7636.

    Matching GuJumpgate local-cli-proxy-api.js generatePkceCodes():
    - 96 random bytes → base64url = code_verifier
    - SHA-256(verifier) → base64url = code_challenge
    """
    verifier_bytes = secrets.token_bytes(96)
    code_verifier = b64url(verifier_bytes)
    code_challenge = b64url(hashlib.sha256(code_verifier.encode()).digest())
    return code_verifier, code_challenge

def build_oauth_url(code_challenge: str, state: str) -> str:
    """Build the OAuth authorize URL matching the REAL Codex CLI parameters.

    Ground truth: codex-rs login/src/server.rs build_authorize_url().
    Key differences from GuJumpgate:
      - NO prompt=login (the real CLI omits it; sending it causes instant error-redirect)
      - Scope includes api.connectors.read/invoke (granted to this client_id)
      - codex_cli_simplified_flow=true, id_token_add_organizations=true
      - originator=codex_cli_rs
    """
    params = {
        "client_id": OAUTH_CLIENT_ID,
        "response_type": "code",
        "redirect_uri": OAUTH_REDIRECT,
        "scope": OAUTH_SCOPE,
        "state": state,
        "code_challenge": code_challenge,
        "code_challenge_method": "S256",
        "id_token_add_organizations": "true",
        "codex_cli_simplified_flow": "true",
        "originator": "codex_cli_rs",
    }
    qs = urllib.parse.urlencode(params)
    return f"{OAUTH_AUTH_URL}?{qs}"

def exchange_code_for_tokens(code: str, code_verifier: str):
    """POST to auth.openai.com/oauth/token to exchange authorization code for tokens.

    Matching GuJumpgate exchangeCodeForTokens() + pool_server exchangeCodexCode().
    Returns dict with access_token, refresh_token, id_token, expires_in on success.
    Returns None on failure (so caller can fall back to session token).
    """
    form = urllib.parse.urlencode({
        "grant_type": "authorization_code",
        "client_id": OAUTH_CLIENT_ID,
        "code": code,
        "redirect_uri": OAUTH_REDIRECT,
        "code_verifier": code_verifier,
    }).encode()
    req = urllib.request.Request(
        OAUTH_TOKEN_URL,
        data=form,
        headers={
            "Content-Type": "application/x-www-form-urlencoded",
            "Accept": "application/json",
            "User-Agent": UA,
        },
    )
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            raw = resp.read().decode()
            if resp.status != 200:
                log(f"oauth token exchange failed ({resp.status}): {raw[:300]}")
                return None
            return json.loads(raw)
    except Exception as e:
        log(f"oauth token exchange error: {e}")
        return None

def capture_redirect_code(page, timeout_s=30):
    """Wait for the page URL to become a localhost:1455 OAuth callback, extract code.

    After the consent click, the browser redirects to:
        http://localhost:1455/auth/callback?code=...&state=...
    The page fails to load (nothing listening on :1455) but the URL is in the address bar.
    We poll page.url looking for the redirect, same technique GuJumpgate uses.
    """
    deadline = time.time() + timeout_s
    while time.time() < deadline:
        url = page.url
        if "localhost:1455" in url and "code=" in url:
            parsed = urllib.parse.urlparse(url)
            qs = urllib.parse.parse_qs(parsed.query)
            code = qs.get("code", [None])[0]
            state = qs.get("state", [None])[0]
            # Also check fragment (rare but handled by Codex CLI)
            if not code and parsed.fragment:
                frag_qs = urllib.parse.parse_qs(parsed.fragment)
                code = frag_qs.get("code", [None])[0]
                state = state or frag_qs.get("state", [None])[0]
            if code:
                log(f"captured OAuth code (state={state})")
                return code
        # Also check for error redirect
        if "error=" in url and "localhost" in url:
            parsed = urllib.parse.urlparse(url)
            qs = urllib.parse.parse_qs(parsed.query)
            err = qs.get("error", [""])[0]
            err_desc = qs.get("error_description", [""])[0]
            log(f"OAuth error redirect: {err} — {err_desc}")
            return None
        time.sleep(0.5)
    log(f"timeout waiting for OAuth redirect; final URL: {page.url[:200]}")
    return None

def main():
    fn, ln = random.choice(FIRST_NAMES), random.choice(LAST_NAMES)
    age = str(random.randint(19, 25))
    log(f"Profile: {fn} {ln}, age {age} | email={EMAIL} | server={PROXY_SERVER}")

    from playwright.sync_api import sync_playwright

    with sync_playwright() as p:
        ctx = p.chromium.launch_persistent_context(
            user_data_dir=f"/tmp/regv3_{os.urandom(4).hex()}",
            headless=HEADLESS,
            executable_path=CHROME or None,
            args=["--no-sandbox","--disable-gpu","--disable-dev-shm-usage",
                  "--disable-blink-features=AutomationControlled"],
            proxy={"server": PROXY_SERVER, "username": PROXY_USER, "password": PROXY_PASS},
            user_agent=UA, locale="en-US", viewport={"width":1280,"height":720},
            ignore_default_args=["--enable-automation"],
        )
        ctx.add_init_script(STEALTH)
        page = ctx.pages[0]

        try:
            # 1. Login page — snapshot the timestamp so we filter OTPs
            log("1. login page")
            since_ts = time.strftime("%Y-%m-%dT%H:%M", time.gmtime())
            page.goto("https://chatgpt.com/auth/login", wait_until="domcontentloaded", timeout=60000)
            for _ in range(30):
                if "get started" in page.title().lower() or "chatgpt" in page.title().lower():
                    break
                time.sleep(3)
            time.sleep(4)

            # 2. Fill email
            log(f"2. email: {EMAIL}")
            react_fill(page, 'input[name="email"]', EMAIL)
            time.sleep(1)
            page.locator('input[name="email"]').first.press("Enter")
            log("   submitted")
            since_ts = time.strftime("%Y-%m-%dT%H:%M", time.gmtime())  # snapshot NOW
            time.sleep(5)
            for _ in range(20):
                if "just a moment" not in page.title().lower(): break
                time.sleep(3)

            # 3. OTP page
            log(f"3. page: {page.title()} | {page.url[:100]}")
            for _ in range(15):
                if page.locator('input[name="code"]').first.count() > 0: break
                time.sleep(2)

            # 4. Get NEW OTP (only after since_ts)
            log(f"4. fetching OTP (since {since_ts})...")
            code = fetch_otp(since_ts)
            if not code:
                log("OTP TIMEOUT")
                return
            log(f"   code: {code}")

            # 5. Enter OTP (use regular fill — Chrome 150 compatible)
            log("5. entering OTP")
            react_fill(page, 'input[name="code"]', code)
            time.sleep(1)
            page.locator('input[name="code"]').first.press("Enter")
            time.sleep(6)
            log(f"   after OTP: {page.title()} | {page.url[:100]}")

            # Handle "Check your inbox" interstitial (OTP processing)
            if "check your inbox" in page.title().lower():
                log("   waiting for OTP to process...")
                for _ in range(15):
                    time.sleep(2)
                    title = page.title()
                    if "about-you" in page.url.lower() or "how old" in title.lower():
                        break
                    if page.locator('input[name="name"]').first.count() > 0: break
                log(f"   after process: {page.title()}")

            # 6. Profile — human-paced interaction to avoid "Oops" error
            log("6. profile")
            for _ in range(10):
                name_input = page.locator('input[name="name"]').first
                if name_input.count() > 0:
                    try:
                        name_input.wait_for(state="visible", timeout=5000)
                        break
                    except: pass
                time.sleep(2)
            full_name = f"{fn} {ln}"

            # Use react_fill directly — the native setter doesn't need focus first.
            # (Do NOT click() React Aria inputs first; their floating labels
            # intercept pointer events and cause 30s timeouts.)
            try:
                react_fill(page, 'input[name="name"]', full_name)
                log(f"   name: {full_name}")
            except Exception as e:
                log(f"   name fill err: {e}")

            # Small human delay between fields
            time.sleep(random.uniform(0.5, 1.2))

            age_input = page.locator('input[name="age"][type="number"]').first
            if age_input.count() > 0:
                try:
                    react_fill(page, 'input[name="age"][type="number"]', age)
                    log(f"   age: {age}")
                except Exception as e:
                    log(f"   age fill err: {e}")

            # Human-like pause before clicking continue (think time)
            time.sleep(random.uniform(0.5, 1.0))

            # Check consent checkbox (GuJumpgate technique)
            page.evaluate('_checkConsent()')

            # Try multiple strategies to click the submit button
            clicked = False
            for btn_text in ["Continue", "Next", "Submit", "Agree and continue"]:
                try:
                    btn = page.get_by_role("button", name=btn_text)
                    if btn.count() > 0 and btn.first.is_visible():
                        btn.first.click()
                        log(f"   clicked '{btn_text}' button")
                        clicked = True
                        break
                except: pass
            if not clicked:
                # Fallback: click last submit button
                try:
                    page.locator('button[type="submit"]').last.click()
                    log("   clicked submit button (fallback)")
                    clicked = True
                except: pass
            log("   submitted")
            time.sleep(5)
            log(f"   after profile: {page.title()} | {page.url[:100]}")

            # Oops recovery — with _checkConsent + improved retry
            for rec in range(3):
                title = page.title().lower()
                url_low = page.url.lower()
                if "oops" not in title and "error" not in title:
                    break
                if "chatgpt.com" in url_low and "/auth/" not in url_low:
                    log("   despite oops, we appear to be in ChatGPT — proceeding")
                    break
                log(f"   recovery #{rec+1}")
                time.sleep(2)
                # Re-fill profile if inputs are visible (with valueTracker sync)
                if page.locator('input[name="name"]').first.count() > 0:
                    try:
                        react_fill(page, 'input[name="name"]', full_name)
                        time.sleep(random.uniform(0.3, 0.7))
                    except: pass
                    ae = page.locator('input[name="age"][type="number"]').first
                    if ae.count() > 0:
                        try:
                            react_fill(page, 'input[name="age"][type="number"]', age)
                            time.sleep(random.uniform(0.3, 0.7))
                        except: pass
                    page.evaluate('_checkConsent()')
                    time.sleep(random.uniform(0.5, 1.0))
                    # Try named button first
                    for t in ["Continue","Next","Submit","Agree and continue"]:
                        try:
                            b = page.get_by_role("button", name=t)
                            if b.count() > 0 and b.first.is_visible():
                                b.first.click(); log(f"   clicked '{t}'"); break
                        except: pass
                    else:
                        page.locator('button[type="submit"]').last.click()
                    time.sleep(4)

            # 7. Post-profile — click through consent/workspace pages
            log("7. post-profile")
            for _ in range(20):
                url = page.url
                if "chatgpt.com" in url and "/auth/" not in url:
                    log("   ✅ in ChatGPT!")
                    break
                # OpenAI may demand phone verification before letting us in.
                if maybe_handle_phone(page, log):
                    continue
                if "consent" in url.lower() or "workspace" in url.lower():
                    for t in ["Authorize","Allow","Accept","Continue","同意"]:
                        try:
                            btn = page.get_by_role("button", name=t)
                            if btn.count() > 0:
                                btn.first.click(); log(f"   clicked {t}"); time.sleep(3); break
                        except: pass
                time.sleep(2)

            # 8. Extract web session
            log("8. session")
            sess = page.evaluate(
                'async () => { try { const r = await fetch("/api/auth/session",{credentials:"include"}); return await r.json(); } catch(e) { return {error:e.message}; } }'
            )
            acct = (sess or {}).get("account", {}) or {}
            user = (sess or {}).get("user", {}) or {}
            session_at = (sess or {}).get("accessToken", "")

            if not user.get("id"):
                log(f"no_session: title={page.title()} keys={(sess or {}).keys()}")
                return

            log(f"✅ session OK user={user.get('id')} account={acct.get('id')} plan={acct.get('planType','free')}")

            # ── 9. PKCE OAuth token exchange (THE MISSING STEP) ──────────────────
            # The web session accessToken from /api/auth/session expires in ~6h and
            # lacks a refresh_token. We now perform a full PKCE OAuth flow in the
            # same browser context (already authenticated) to get proper OAuth tokens
            # that can be refreshed — exactly what the real Codex CLI stores in
            # auth.json and what GuJumpgate's local-cli-proxy-api module produces.

            oauth_access_token = ""
            oauth_refresh_token = ""
            oauth_id_token = ""

            try:
                log("9. PKCE OAuth flow")

                # 9a. Generate PKCE codes + state
                code_verifier, code_challenge = generate_pkce()
                oauth_state = secrets.token_hex(16)
                log(f"   verifier={code_verifier[:20]}... challenge={code_challenge[:20]}...")

                # 9b. Build and navigate to OAuth authorize URL
                oauth_url = build_oauth_url(code_challenge, oauth_state)
                log(f"   navigating to authorize URL...")
                page.goto(oauth_url, wait_until="domcontentloaded", timeout=30000)
                time.sleep(4)
                log(f"   authorize page: {page.title()} | {page.url[:120]}")

                # 9c. Handle OAuth consent / account selector page
                for _ in range(30):
                    url = page.url
                    if "localhost:1455" in url:
                        break
                    if "chatgpt.com" in url and "/auth/" not in url:
                        log("   landed back at chatgpt.com (OAuth completed inline)")
                        break

                    # THE fix: OpenAI inserts add-phone here for fresh accounts. The old
                    # code only clicked Authorize/Allow (which don't exist on add-phone),
                    # spun out, and fell back to a 6h session token. Now we complete it.
                    if maybe_handle_phone(page, log):
                        continue

                    # Account selector page ("choose-an-account")
                    if "choose-an-account" in url.lower():
                        log("   account selector — clicking first account")
                        # Click the first account option
                        try:
                            page.locator('[role="listitem"], [role="option"], .account-item, li').first.click()
                            log("   clicked account")
                            time.sleep(2)
                        except: pass
                        # Then click Continue
                        for t in ["Continue","Next","Authorize"]:
                            try:
                                b = page.get_by_role("button", name=t)
                                if b.count() > 0 and b.first.is_visible():
                                    b.first.click(); log(f"   '{t}'"); time.sleep(3); break
                            except: pass

                    # Consent/authorize buttons
                    clicked = False
                    for t in ["Authorize","Allow","Accept","Continue","同意","Confirm"]:
                        try:
                            btn = page.get_by_role("button", name=t)
                            if btn.count() > 0 and btn.first.is_visible():
                                btn.first.click()
                                log(f"   clicked '{t}'")
                                clicked = True
                                time.sleep(3)
                                break
                        except: pass
                    if not clicked:
                        try:
                            primary = page.locator('button[type="submit"], button.btn-primary').first
                            if primary.count() > 0 and primary.is_visible():
                                primary.click(); log("   clicked primary"); time.sleep(3)
                        except: pass
                    time.sleep(2)

                # 9d. Capture the redirect code
                # After consent, the browser redirects to localhost:1455/auth/callback?code=...
                # The page fails to load (nothing on :1455) but the URL is in the address bar.
                oauth_code = capture_redirect_code(page, timeout_s=30)
                if oauth_code:
                    log(f"   captured code: {oauth_code[:20]}...")

                    # 9e. Exchange code for tokens
                    log("   exchanging code for tokens...")
                    token_resp = exchange_code_for_tokens(oauth_code, code_verifier)
                    if token_resp:
                        oauth_access_token = token_resp.get("access_token", "")
                        oauth_refresh_token = token_resp.get("refresh_token", "")
                        oauth_id_token = token_resp.get("id_token", "")
                        expires_in = token_resp.get("expires_in", 0)
                        log(f"✅ OAuth tokens obtained: access_token={oauth_access_token[:20]}... refresh_token={'yes' if oauth_refresh_token else 'NO'} id_token={'yes' if oauth_id_token else 'NO'} expires_in={expires_in}s")
                    else:
                        log("⚠️ token exchange returned no tokens — falling back to session accessToken")
                else:
                    log("⚠️ no OAuth code captured — falling back to session accessToken")

            except Exception as e:
                log(f"⚠️ OAuth flow error (falling back to session token): {e}")
                import traceback; traceback.print_exc()

            # ── 10. Output account JSON ──────────────────────────────────────────
            # Prefer OAuth tokens (refreshable) over session tokens (6h TTL).
            # The Go pipeline reads access_token, refresh_token, id_token from this JSON.
            final_access_token = oauth_access_token or session_at
            if not final_access_token:
                log("❌ No access token (neither OAuth nor session) — account unusable")
                return

            result = {
                "type": "codex",
                "email": EMAIL,
                "account_id": acct.get("id", ""),
                "user_id": user.get("id", ""),
                "plan_type": acct.get("planType", "free"),
                "name": user.get("name", full_name),
                "access_token": final_access_token,
                "refresh_token": oauth_refresh_token,
                "id_token": oauth_id_token,
                # Fallback session fields (useful for cookie-based refresh)
                "session_token": session_at,
            }
            log(f"✅ SUCCESS user={user.get('id')} account={acct.get('id')} oauth_refresh={'yes' if oauth_refresh_token else 'no'}")
            print("__CODEX_ACCOUNT__ " + json.dumps(result, ensure_ascii=False), flush=True)

        except Exception as e:
            log(f"ERROR: {e}")
            import traceback; traceback.print_exc()
        finally:
            ctx.close()

if __name__ == "__main__":
    main()
