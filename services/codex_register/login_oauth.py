#!/usr/bin/env python3
"""Login to existing ChatGPT account + PKCE OAuth token exchange.

Logs into an already-registered ChatGPT account (email + OTP or password),
then navigates to the Codex OAuth authorize URL in the same browser context.
Since the browser is already authenticated, it lands directly on the consent
page. Click "Authorize" → capture the code from the localhost redirect →
exchange for OAuth tokens (access_token + refresh_token + id_token).

This is the GuJumpgate approach: reuse the authenticated browser session
to perform a headless PKCE OAuth flow matching the real Codex CLI.

Env vars:
  REG_PROXY_SERVER   proxy server URL
  REG_PROXY_USER     proxy username
  REG_PROXY_PASS     proxy password
  REG_EMAIL          ChatGPT account email
  REG_PASSWORD       (optional) ChatGPT account password (if set)
  REG_OTP_URL        OTP API URL (for email verification codes)
  REG_CHROME         chrome binary path
  REG_HEADLESS       0=headed, 1=headless

Output: __CODEX_ACCOUNT__ {json} with OAuth tokens on stdout.
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
PROXY_PASS   = os.environ.get("REG_PROXY_PASS", "")
EMAIL        = os.environ.get("REG_EMAIL", "")
PASSWORD     = os.environ.get("REG_PASSWORD", "")
OTP_URL      = os.environ.get("REG_OTP_URL", "")
TARGET_WORKSPACE_ID = os.environ.get("REG_TARGET_WORKSPACE_ID", "")
CHROME       = os.environ.get("REG_CHROME", os.environ.get("CHROME_PATH", ""))
HEADLESS     = os.environ.get("REG_HEADLESS", "1") != "0"

UA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"

STEALTH = """
Object.defineProperty(navigator, 'webdriver', {get: () => undefined});
delete Object.getOwnPropertyDescriptor(navigator.__proto__, 'webdriver');
if (!window.chrome) { window.chrome = {runtime: {}, app: {}}; }
Object.defineProperty(navigator, 'hardwareConcurrency', {get: () => 8});
Object.defineProperty(navigator, 'deviceMemory', {get: () => 8});
Object.defineProperty(navigator, 'languages', {get: () => ['en-US', 'en']});

// React controlled-input hack (GuJumpgate technique)
window._fillReactInput = function(el, value) {
    const nativeSetter = Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, 'value').set;
    nativeSetter.call(el, value);
    el.dispatchEvent(new Event('input', { bubbles: true }));
    el.dispatchEvent(new Event('change', { bubbles: true }));
};
"""

# ── OAuth constants (matching the real Codex CLI) ──────────────────────────
OAUTH_AUTH_URL  = "https://auth.openai.com/oauth/authorize"
OAUTH_TOKEN_URL = "https://auth.openai.com/oauth/token"
OAUTH_CLIENT_ID = "app_EMoamEEZ73f0CkXaXp7hrann"
OAUTH_REDIRECT  = "http://localhost:1455/auth/callback"
OAUTH_SCOPE     = "openid profile email offline_access api.connectors.read api.connectors.invoke"

def log(msg):
    print(f"[oauth {time.strftime('%H:%M:%S')}] {msg}", file=sys.stderr, flush=True)

def react_fill(page, selector, value):
    """Fill React-controlled input via native value setter."""
    page.evaluate(f'_fillReactInput(document.querySelector({json.dumps(selector)}), {json.dumps(str(value))})')

def fetch_otp(since_ts):
    """Poll for NEW OTP code received after since_ts."""
    deadline = time.time() + 150
    while time.time() < deadline:
        try:
            req = urllib.request.Request(OTP_URL, headers={"User-Agent": UA})
            with urllib.request.urlopen(req, timeout=10) as resp:
                data = json.loads(resp.read().decode())
        except Exception:
            time.sleep(5); continue
        for mail in data.get("emails", []):
            mail_ts = mail.get("时间", "") or mail.get("receivedAt", "") or "2000-01-01"
            if mail_ts < since_ts:
                continue
            combined = f"{mail.get('主题','')} {mail.get('内容预览','')} {mail.get('html','')} {mail.get('subject','')} {mail.get('body','')}"
            m = re.search(r"(?<!\d)(\d{6})(?!\d)", combined)
            if m and m.group(1) != "177010":
                return m.group(1).strip()
        time.sleep(5)
    return None

# ── PKCE + OAuth helpers ──────────────────────────────────────────────────

def b64url(data: bytes) -> str:
    return base64.urlsafe_b64encode(data).rstrip(b"=").decode()

def generate_pkce():
    verifier_bytes = secrets.token_bytes(96)
    code_verifier = b64url(verifier_bytes)
    code_challenge = b64url(hashlib.sha256(code_verifier.encode()).digest())
    return code_verifier, code_challenge

def build_oauth_url(code_challenge: str, state: str) -> str:
    """Build OAuth authorize URL matching the REAL Codex CLI."""
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
    if TARGET_WORKSPACE_ID:
        params["allowed_workspace_id"] = TARGET_WORKSPACE_ID
    return f"{OAUTH_AUTH_URL}?{urllib.parse.urlencode(params)}"

def exchange_code_for_tokens(code: str, code_verifier: str):
    """POST to auth.openai.com/oauth/token to exchange code for tokens."""
    form = urllib.parse.urlencode({
        "grant_type": "authorization_code",
        "client_id": OAUTH_CLIENT_ID,
        "code": code,
        "redirect_uri": OAUTH_REDIRECT,
        "code_verifier": code_verifier,
    }).encode()
    req = urllib.request.Request(
        OAUTH_TOKEN_URL, data=form,
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
                log(f"token exchange failed ({resp.status}): {raw[:300]}")
                return None
            return json.loads(raw)
    except Exception as e:
        log(f"token exchange error: {e}")
        return None

def capture_redirect_code(page, timeout_s=30):
    """Wait for page URL to become localhost:1455 OAuth callback, extract code."""
    deadline = time.time() + timeout_s
    while time.time() < deadline:
        url = page.url
        if "localhost:1455" in url and "code=" in url:
            parsed = urllib.parse.urlparse(url)
            qs = urllib.parse.parse_qs(parsed.query)
            code = qs.get("code", [None])[0]
            if not code and parsed.fragment:
                frag_qs = urllib.parse.parse_qs(parsed.fragment)
                code = frag_qs.get("code", [None])[0]
            if code:
                log(f"captured OAuth code")
                return code
        if "error=" in url and "localhost" in url:
            parsed = urllib.parse.urlparse(url)
            qs = urllib.parse.parse_qs(parsed.query)
            log(f"OAuth error: {qs.get('error',[''])[0]} — {qs.get('error_description',[''])[0]}")
            return None
        time.sleep(0.5)
    log(f"timeout waiting for OAuth redirect; final URL: {page.url[:200]}")
    return None

def click_consent_buttons(page, timeout_s=20):
    """Click through OAuth consent/authorize buttons.

    Since we're already logged in, navigating to the OAuth URL should land
    directly on the consent page. Click "Authorize" / "Continue" to proceed.
    """
    deadline = time.time() + timeout_s
    while time.time() < deadline:
        url = page.url

        # Already redirected → done
        if "localhost:1455" in url:
            log("   redirect detected, stopping consent loop")
            return
        # Landed back at ChatGPT → consent completed without explicit button
        if "chatgpt.com" in url and "/auth/" not in url and "consent" not in url.lower():
            log("   landed at chatgpt.com (consent passed)")
            return
        # Error page
        if "error" in url.lower() and "localhost" not in url:
            log(f"   error page: {page.title()} | {url[:120]}")
            # Try to recover by going back to OAuth URL
            return

        # Try clicking consent buttons
        for btn_text in ["Authorize", "Allow", "Accept", "Continue", "Confirm", "同意"]:
            try:
                btn = page.get_by_role("button", name=btn_text)
                if btn.count() > 0 and btn.first.is_visible():
                    btn.first.click()
                    log(f"   clicked '{btn_text}'")
                    time.sleep(3)
                    break
            except:
                pass

        # Also try generic submit buttons
        for sel in ['button[type="submit"]', 'button.btn-primary', 'button.continue-button',
                     'button[data-action="continue"]', 'button[data-testid="continue-button"]']:
            try:
                el = page.locator(sel).first
                if el.count() > 0 and el.is_visible():
                    el.click()
                    log(f"   clicked {sel}")
                    time.sleep(3)
                    break
            except:
                pass

        time.sleep(1.5)

def main():
    log(f"Login+OAuth for: {EMAIL} | server={PROXY_SERVER}")

    from playwright.sync_api import sync_playwright

    with sync_playwright() as p:
        ctx = p.chromium.launch_persistent_context(
            user_data_dir=f"/tmp/oauth_{os.urandom(4).hex()}",
            headless=HEADLESS,
            executable_path=CHROME or None,
            args=["--no-sandbox", "--disable-gpu", "--disable-dev-shm-usage",
                  "--disable-blink-features=AutomationControlled"],
            proxy={"server": PROXY_SERVER, "username": PROXY_USER, "password": PROXY_PASS},
            user_agent=UA, locale="en-US", viewport={"width": 1280, "height": 720},
            ignore_default_args=["--enable-automation"],
        )
        ctx.add_init_script(STEALTH)
        page = ctx.pages[0]

        try:
            # ── Step 1: Login ──────────────────────────────────────────
            log("1. navigating to login page")
            page.goto("https://chatgpt.com/auth/login", wait_until="domcontentloaded", timeout=60000)
            for _ in range(30):
                if "chatgpt" in page.title().lower():
                    break
                time.sleep(2)
            time.sleep(3)
            log(f"   title: {page.title()}")

            # 2. Enter email and click Continue
            log(f"2. email: {EMAIL}")
            react_fill(page, 'input[name="email"]', EMAIL)
            time.sleep(random.uniform(0.5, 1.0))

            # Try multiple ways to submit email: click Continue button OR press Enter
            clicked = False
            for btn_text in ["Continue", "Next", "Get started", "Log in", "Sign in"]:
                try:
                    btn = page.get_by_role("button", name=btn_text)
                    if btn.count() > 0 and btn.first.is_visible():
                        btn.first.click()
                        log(f"   clicked '{btn_text}'")
                        clicked = True
                        break
                except: pass
            if not clicked:
                page.locator('input[name="email"]').first.press("Enter")
                log("   pressed Enter")
            log("   submitted")
            since_ts = time.strftime("%Y-%m-%dT%H:%M", time.gmtime())
            time.sleep(5)

            # Check for Cloudflare "just a moment"
            for _ in range(20):
                if "just a moment" not in page.title().lower():
                    break
                log("   waiting for CF challenge...")
                time.sleep(3)

            log(f"3. after email: title={page.title()} | url={page.url[:120]}")

            # 3. Handle: password page OR OTP page
            password_needed = False
            otp_needed = False

            for _ in range(15):
                if page.locator('input[type="password"]').first.count() > 0:
                    password_needed = True
                    break
                if page.locator('input[name="code"]').first.count() > 0:
                    otp_needed = True
                    break
                time.sleep(1)

            if password_needed:
                log("3a. password page detected")
                if not PASSWORD:
                    log("   no REG_PASSWORD set — trying OTP fallback")
                    # Click "use verification code" or similar link
                    for link_text in ["Use a code", "Verify with a code", "Try another way",
                                       "Email me a code", "Send code"]:
                        try:
                            link = page.get_by_text(link_text)
                            if link.count() > 0:
                                link.first.click()
                                log(f"   clicked '{link_text}'")
                                time.sleep(3)
                                otp_needed = True
                                break
                        except:
                            pass
                else:
                    react_fill(page, 'input[type="password"]', PASSWORD)
                    time.sleep(random.uniform(0.5, 1.0))
                    page.locator('input[type="password"]').first.press("Enter")
                    log("   password submitted")
                    time.sleep(5)

            if otp_needed:
                log("3b. OTP page — fetching code...")
                code = fetch_otp(since_ts)
                if not code:
                    log("OTP TIMEOUT — cannot login")
                    return
                log(f"   code: {code}")
                react_fill(page, 'input[name="code"]', code)
                time.sleep(random.uniform(0.5, 1.0))
                page.locator('input[name="code"]').first.press("Enter")
                time.sleep(6)
                log(f"   after OTP: title={page.title()} | url={page.url[:120]}")

            # 4. Wait for login to complete
            log("4. waiting for login completion...")
            for _ in range(30):
                url = page.url
                if "chatgpt.com" in url and "/auth/" not in url and "login" not in url:
                    log(f"   ✅ logged in: {page.title()}")
                    break
                # Handle post-login redirects, consent, workspace selection
                if "consent" in url.lower() or "workspace" in url.lower():
                    for t in ["Authorize", "Allow", "Accept", "Continue", "同意"]:
                        try:
                            btn = page.get_by_role("button", name=t)
                            if btn.count() > 0:
                                btn.first.click()
                                log(f"   clicked '{t}'")
                                time.sleep(3)
                                break
                        except:
                            pass
                # Handle profile page (if account was in pending state)
                if "about-you" in url.lower():
                    log("   profile page detected (account was pending)")
                    # Try to skip / fill minimal profile
                    try:
                        page.locator('button[type="submit"]').last.click()
                        log("   clicked continue on profile")
                        time.sleep(3)
                    except:
                        pass
                time.sleep(2)

            # 5. Extract web session
            log("5. extracting session...")
            sess = page.evaluate(
                'async () => { try { const r = await fetch("/api/auth/session",{credentials:"include"}); return await r.json(); } catch(e) { return {error:e.message}; } }'
            )
            acct = (sess or {}).get("account", {}) or {}
            user = (sess or {}).get("user", {}) or {}
            session_at = (sess or {}).get("accessToken", "")

            if not user.get("id"):
                log(f"❌ login failed: title={page.title()} keys={(sess or {}).keys()}")
                return

            log(f"✅ logged in: user={user.get('id')} account={acct.get('id')} plan={acct.get('planType','free')}")

            # ── Step 6: PKCE OAuth token exchange ───────────────────────
            log("6. PKCE OAuth flow...")

            oauth_access_token = ""
            oauth_refresh_token = ""
            oauth_id_token = ""

            try:
                # 6a. Generate PKCE
                code_verifier, code_challenge = generate_pkce()
                oauth_state = secrets.token_hex(16)
                log(f"   verifier={code_verifier[:16]}... challenge={code_challenge[:16]}...")

                # 6b. Navigate to OAuth authorize URL
                oauth_url = build_oauth_url(code_challenge, oauth_state)
                log(f"   navigating: {oauth_url[:120]}...")
                page.goto(oauth_url, wait_until="domcontentloaded", timeout=30000)
                time.sleep(4)
                log(f"   authorize page: title={page.title()} | url={page.url[:120]}")

                # 6c. Click consent
                click_consent_buttons(page)

                # 6d. Capture redirect code
                oauth_code = capture_redirect_code(page, timeout_s=30)
                if oauth_code:
                    log(f"   code captured: {oauth_code[:20]}...")

                    # 6e. Exchange for tokens
                    log("   exchanging for tokens...")
                    token_resp = exchange_code_for_tokens(oauth_code, code_verifier)
                    if token_resp:
                        oauth_access_token = token_resp.get("access_token", "")
                        oauth_refresh_token = token_resp.get("refresh_token", "")
                        oauth_id_token = token_resp.get("id_token", "")
                        expires_in = token_resp.get("expires_in", 0)
                        log(f"✅ OAuth tokens: access={'✓' if oauth_access_token else '✗'} refresh={'✓' if oauth_refresh_token else '✗'} id={'✓' if oauth_id_token else '✗'} expires_in={expires_in}s")
                    else:
                        log("⚠️ token exchange returned nothing — using session token")
                else:
                    log("⚠️ no OAuth code captured — using session token")

            except Exception as e:
                log(f"⚠️ OAuth error: {e}")
                import traceback
                traceback.print_exc()

            # ── Step 7: Output ─────────────────────────────────────────
            final_access_token = oauth_access_token or session_at
            if not final_access_token:
                log("❌ no access token at all")
                return

            result = {
                "type": "codex",
                "email": EMAIL,
                "account_id": acct.get("id", ""),
                "user_id": user.get("id", ""),
                "plan_type": acct.get("planType", "free"),
                "name": user.get("name", ""),
                "access_token": final_access_token,
                "refresh_token": oauth_refresh_token,
                "id_token": oauth_id_token,
                "session_token": session_at,
            }
            log(f"✅ SUCCESS oauth_refresh={'yes' if oauth_refresh_token else 'no'}")
            print("__CODEX_ACCOUNT__ " + json.dumps(result, ensure_ascii=False), flush=True)

        except Exception as e:
            log(f"ERROR: {e}")
            import traceback
            traceback.print_exc()
        finally:
            ctx.close()

if __name__ == "__main__":
    main()
