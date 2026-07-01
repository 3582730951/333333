#!/usr/bin/env python3
"""Correct OpenAI add-phone / phone-verification handler for the Codex OAuth flow.

This is a faithful port of the *working* GuJumpgate logic in
  other_GuJumpgate/content/phone-auth.js  (submitPhoneNumber / submitPhoneVerificationCode)
which the older pool_server scripts (oauth_login.py / reg_v3.py) implemented INCORRECTLY,
causing the endless "stuck at add_phone" loop.

The three bugs this module fixes (vs. the old oauth_login.py inline code):

  1. NUMBER FORMAT (the killer). The old code selected the country in the dropdown
     AND filled the FULL international number into the tel input. OpenAI's widget then
     prepends the dial code again -> "+62" + "6285..." -> invalid -> bounced back to
     add-phone forever. GuJumpgate fills the NATIONAL number (dial code stripped) into
     the visible tel input and the full E.164 into the hidden input[name=phoneNumber].

  2. WHATSAPP DELIVERY. OpenAI defaults many countries (ID/PH/...) to WhatsApp OTP,
     which a hero-sms SMS number can never receive -> hero_wait_sms times out -> stuck.
     We force the SMS channel and, if the page only offers WhatsApp, we release the
     number and try a fresh one (different country if needed).

  3. NO LOOP DETECTION. After submitting the code we distinguish consent-ready (success),
     invalid-code, and returned-to-add-phone (rejected) and replace the number up to
     `max_replacements` times instead of silently spinning.

Usage (from a Playwright page already sitting on / about to show the add-phone page):

    from phone_verify import verify_phone
    res = verify_phone(page, hero_key=KEY, country="PH", log=log)
    if res["ok"]:
        ...  # continue OAuth: capture localhost:1455?code=...

Returns: {"ok": bool, "phone": str, "country": str, "channel": str, "error": str}
"""
import json
import re
import time
import urllib.parse
import urllib.request

HEROSMS_BASE = "https://hero-sms.com/stubs/handler_api.php"
HEROSMS_SERVICE = "dr"  # "dr" = OpenAI/ChatGPT on hero-sms (NOT "ot" = other)
_UA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"

# country ISO-2 -> (hero-sms country id, dial code). Cheapest-first ordering for FALLBACK_ORDER.
# hero ids from hero-sms getPrices (service "dr"); dial codes are the E.164 country codes.
COUNTRY_CFG = {
    "PH": {"hero": "4",   "dial": "63"},   # Philippines  ~$0.025  (cheapest, good for OpenAI)
    "ID": {"hero": "6",   "dial": "62"},   # Indonesia    ~$0.045
    "BR": {"hero": "73",  "dial": "55"},   # Brazil       ~$0.045
    "VN": {"hero": "10",  "dial": "84"},   # Vietnam
    "MY": {"hero": "7",   "dial": "60"},   # Malaysia
    "ZA": {"hero": "27",  "dial": "27"},   # South Africa
    "CL": {"hero": "56",  "dial": "56"},   # Chile
    "TH": {"hero": "52",  "dial": "66"},   # Thailand
    "GB": {"hero": "16",  "dial": "44"},   # United Kingdom
    "IN": {"hero": "22",  "dial": "91"},   # India
    "US": {"hero": "187", "dial": "1"},    # United States
}
# Used when a country keeps failing (banned numbers / whatsapp-only) and we want to rotate.
FALLBACK_ORDER = ["PH", "ID", "BR", "VN", "MY", "ZA", "CL", "TH", "GB"]


# ── hero-sms client ───────────────────────────────────────────────────────────
def _http(url, timeout=15):
    req = urllib.request.Request(url, headers={"User-Agent": _UA})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return r.read().decode().strip()


def hero_get_number(hero_key, country_iso, log=print):
    """getNumber for the OpenAI service. Returns (order_id, full_digits) or (None, None)."""
    cfg = COUNTRY_CFG.get(country_iso.upper())
    if not cfg:
        log(f"   [phone] unknown country {country_iso}")
        return None, None
    q = urllib.parse.urlencode({
        "api_key": hero_key, "action": "getNumber",
        "service": HEROSMS_SERVICE, "country": cfg["hero"],
    })
    try:
        txt = _http(f"{HEROSMS_BASE}?{q}", timeout=20)
    except Exception as e:
        log(f"   [phone] getNumber err: {e}")
        return None, None
    log(f"   [phone] hero getNumber({country_iso}): {txt}")
    if txt.startswith("ACCESS_NUMBER"):
        parts = txt.split(":", 2)
        if len(parts) == 3:
            return parts[1], re.sub(r"\D", "", parts[2])
    return None, None


def hero_wait_code(hero_key, order_id, timeout_s=150, log=print):
    """Poll getStatus until STATUS_OK:<code>. Returns the code or None."""
    deadline = time.time() + timeout_s
    while time.time() < deadline:
        q = urllib.parse.urlencode({"api_key": hero_key, "action": "getStatus", "id": order_id})
        try:
            txt = _http(f"{HEROSMS_BASE}?{q}", timeout=12)
        except Exception:
            time.sleep(4)
            continue
        if txt.startswith("STATUS_OK"):
            parts = txt.split(":", 1)
            code = parts[1] if len(parts) == 2 else ""
            m = re.search(r"\b(\d{4,8})\b", code)
            if m:
                return m.group(1)
        elif txt == "STATUS_CANCEL":
            return None
        time.sleep(4)
    return None


def hero_set_status(hero_key, order_id, status, log=print):
    """status 6 = finish (code used OK), 8 = cancel/release."""
    q = urllib.parse.urlencode({"api_key": hero_key, "action": "setStatus", "id": order_id, "status": str(status)})
    try:
        _http(f"{HEROSMS_BASE}?{q}", timeout=10)
    except Exception:
        pass


# ── phone number formatting (matches GuJumpgate to/from national/E164) ─────────
def to_national(full_digits, dial):
    d = re.sub(r"\D", "", str(full_digits))
    dial = re.sub(r"\D", "", str(dial))
    if dial and d.startswith(dial) and len(d) > len(dial):
        return d[len(dial):]
    return d


def to_e164(full_digits, dial):
    d = re.sub(r"\D", "", str(full_digits))
    dial = re.sub(r"\D", "", str(dial))
    if not d:
        return ""
    if dial and d.startswith(dial):
        return "+" + d
    if d.startswith("0"):
        return "+" + dial + d[1:]
    return "+" + dial + d


# ── injected DOM helpers (faithful port of phone-auth.js) ──────────────────────
# One self-contained script defining window.__pv.* helpers. Re-injected each call
# (cheap, idempotent) so it survives navigations within the OAuth flow.
_PV_JS = r"""
(() => {
  if (window.__pv) return;
  const vis = (el) => {
    if (!el) return false;
    const s = window.getComputedStyle(el);
    if (s.display === 'none' || s.visibility === 'hidden' || Number(s.opacity) === 0) return false;
    const r = el.getBoundingClientRect();
    return r.width > 0 && r.height > 0;
  };
  const norm = (v) => String(v || '').replace(/\s+/g, ' ').trim();
  const digits = (v) => String(v || '').replace(/\D+/g, '');
  const addForm = () => document.querySelector('form[action*="/add-phone" i]');
  const verForm = () => document.querySelector('form[action*="/phone-verification" i]');

  const setNative = (el, value) => {
    if (!el) return;
    const proto = el instanceof HTMLTextAreaElement ? HTMLTextAreaElement.prototype : HTMLInputElement.prototype;
    const setter = Object.getOwnPropertyDescriptor(proto, 'value').set;
    const prev = String(el.value ?? '');
    const tracker = el._valueTracker;
    if (tracker && typeof tracker.setValue === 'function') { try { tracker.setValue.call(tracker, prev); } catch (e) {} }
    el.focus();
    setter.call(el, value);
    el.dispatchEvent(new Event('input', { bubbles: true }));
    el.dispatchEvent(new Event('change', { bubbles: true }));
    el.dispatchEvent(new Event('blur', { bubbles: true }));
  };

  const extractDial = (v) => {
    const m = String(v || '').match(/\(\+\s*(\d{1,4})\s*\)|\+\s*\(\s*(\d{1,4})\s*\)|\+\s*(\d{1,4})\b/);
    return String(m?.[1] || m?.[2] || m?.[3] || '').trim();
  };
  const countryButtonText = () => {
    const f = addForm();
    const b = f?.querySelector('button[aria-haspopup="listbox"]');
    if (!b) return '';
    const v = b.querySelector('.react-aria-SelectValue');
    return norm(v?.textContent || b.textContent || '');
  };
  const displayedDial = () => {
    const d = extractDial(countryButtonText());
    if (d) return d;
    const f = addForm();
    if (!f) return '';
    const span = Array.from(f.querySelectorAll('span')).find((e) => vis(e) && /^\d{1,4}$/.test(norm(e.textContent)));
    return norm(span?.textContent || '');
  };

  window.__pv = {
    state() {
      const path = (location.pathname || '') + ' ' + (location.href || '');
      const af = addForm(), vf = verForm();
      let page = 'other';
      if (/\/phone-verification(?:[\/?#]|$)/i.test(path) || (vf && vis(vf))) page = 'phone_verification';
      else if (/\/add-phone(?:[\/?#]|$)/i.test(path) || (af && vis(af))) page = 'add_phone';
      else if (/\/(consent|authorize|oauth)/i.test(path)) page = 'consent';
      return { page, url: location.href, displayedDial: displayedDial() };
    },

    // Select country <select> by ISO-2 value (e.g. "PH"); fall back to matching the dial
    // code inside the option label. Then force the SMS channel. Returns a status object.
    prepare(iso, dial) {
      const f = addForm();
      if (!f) return { ok: false, error: 'no add-phone form' };
      const sel = f.querySelector('select');
      let selectedLabel = '';
      if (sel) {
        const opts = Array.from(sel.options);
        let opt = opts.find((o) => String(o.value || '').trim().toUpperCase() === String(iso).toUpperCase());
        if (!opt && dial) {
          let best = null, bestLen = 0;
          for (const o of opts) {
            const dc = digits(extractDial(o.textContent || o.label || ''));
            if (dc && dc === String(dial) && dc.length >= bestLen) { best = o; bestLen = dc.length; }
          }
          opt = best;
        }
        if (opt) {
          setNative(sel, String(opt.value || ''));
          selectedLabel = norm(opt.textContent);
        }
      }
      // Channel: force SMS / Text Message, never WhatsApp.
      const radios = Array.from(f.querySelectorAll('input[type="radio"]'));
      const channelInfo = radios.map((input) => {
        const label = input.closest('label');
        const text = norm([input.getAttribute('aria-label'), input.value, label?.textContent, input.closest('[role="radio"],[class*="option"]')?.textContent].filter(Boolean).join(' '));
        const v = String(input.value || '').trim().toLowerCase();
        const ch = (v === 'sms' || /(?:^|\b)(?:sms|text\s*message)(?:\b|$)|短信/i.test(text)) ? 'sms'
                 : (v === 'whatsapp' || /whats\s*app/i.test(text)) ? 'whatsapp' : '';
        return { input, label, ch };
      });
      const hidden = f.querySelector('input[name="channel"]');
      const smsOpt = channelInfo.find((c) => c.ch === 'sms');
      const waOnly = channelInfo.length > 0 && !smsOpt && channelInfo.some((c) => c.ch === 'whatsapp');
      if (smsOpt) {
        channelInfo.forEach((c) => {
          if (!c.input) return;
          c.input.checked = c.input === smsOpt.input;
          c.label?.setAttribute?.('data-state', c.input.checked ? 'on' : 'off');
        });
        try { smsOpt.label && smsOpt.input.dispatchEvent(new Event('click', { bubbles: true })); } catch (e) {}
        smsOpt.input.dispatchEvent(new Event('input', { bubbles: true }));
        smsOpt.input.dispatchEvent(new Event('change', { bubbles: true }));
        if (hidden) { hidden.value = 'sms'; hidden.dispatchEvent(new Event('input', { bubbles: true })); hidden.dispatchEvent(new Event('change', { bubbles: true })); }
      }
      // Detect page-level WhatsApp-only delivery copy (no SMS option AND text says WhatsApp).
      const bodyTxt = norm((f.parentElement?.parentElement || f).innerText || '');
      const waCopy = /whats\s*app/i.test(bodyTxt) && /(verification\s+code|one[-\s]*time\s+code|验证码)/i.test(bodyTxt) && !/(?:sms|text\s*message|短信)/i.test(bodyTxt);
      return {
        ok: true,
        selectedLabel,
        displayedDial: displayedDial(),
        smsChannel: Boolean(smsOpt),
        whatsappOnly: Boolean(waOnly || waCopy),
        hiddenChannel: String(hidden?.value || ''),
      };
    },

    fillPhone(national, e164) {
      const f = addForm();
      if (!f) return { ok: false, error: 'no add-phone form' };
      const tel = f.querySelector('input[type="tel"], input[name="__reservedForPhoneNumberInput_tel"], input[autocomplete="tel"]');
      if (!tel) return { ok: false, error: 'no tel input' };
      setNative(tel, national);
      const hidden = f.querySelector('input[name="phoneNumber"]');
      if (hidden) { hidden.value = e164; hidden.dispatchEvent(new Event('input', { bubbles: true })); hidden.dispatchEvent(new Event('change', { bubbles: true })); }
      return { ok: true, telValue: tel.value };
    },

    submitAddPhone() {
      const f = addForm();
      if (!f) return { ok: false };
      const btns = Array.from(f.querySelectorAll('button[type="submit"], input[type="submit"]'));
      const b = btns.find((x) => vis(x) && !x.disabled) || btns.find(vis);
      if (!b) return { ok: false };
      b.click();
      return { ok: true };
    },

    addPhoneError() {
      const f = addForm();
      const roots = [f, document.querySelector('main'), document.body].filter(Boolean);
      const sels = ['.react-aria-FieldError', '[slot="errorMessage"]', '[id$="-error"]', '[class*="error"]', '[role="alert"]', '[aria-live]'];
      const msgs = [];
      for (const root of roots) for (const s of sels) root.querySelectorAll(s).forEach((e) => { const t = norm(e.textContent); if (t && !msgs.includes(t)) msgs.push(t); });
      const pref = msgs.find((t) => /already|used|linked|eligible|invalid|phone|号码|手机号|错误|失败|try\s+again|whats\s*app|unable|cannot|can't/i.test(t));
      return pref || msgs[0] || '';
    },

    fillCode(code) {
      const f = verForm() || document;
      const input = f.querySelector('input[name="code"], input[autocomplete="one-time-code"], input[inputmode="numeric"]');
      if (!input || !vis(input)) return { ok: false };
      setNative(input, code);
      const btns = Array.from((verForm() || document).querySelectorAll('button[type="submit"], input[type="submit"]'));
      const b = btns.find((x) => vis(x) && !x.disabled && String(x.getAttribute('value') || '').toLowerCase() !== 'resend') || btns.find(vis);
      if (b) b.click(); else input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
      return { ok: true };
    },

    verifyError() {
      const f = verForm();
      const roots = [f, document.querySelector('main'), document.body].filter(Boolean);
      const sels = ['.react-aria-FieldError', '[slot="errorMessage"]', '[id$="-error"]', '[class*="error"]', '[role="alert"]'];
      const msgs = [];
      for (const root of roots) for (const s of sels) root.querySelectorAll(s).forEach((e) => { const t = norm(e.textContent); if (t && !msgs.includes(t)) msgs.push(t); });
      return msgs.find((t) => /invalid|incorrect|wrong|expired|错误|无效|过期|try\s+again/i.test(t)) || '';
    },
  };
})();
"""


def _ensure_js(page):
    try:
        page.evaluate(_PV_JS)
    except Exception:
        pass


def _state(page):
    _ensure_js(page)
    try:
        return page.evaluate("() => window.__pv.state()") or {}
    except Exception:
        return {}


def _wait_state(page, wanted, timeout_s=20):
    """Wait until state.page is one of `wanted`. Returns the final state dict."""
    deadline = time.time() + timeout_s
    last = {}
    while time.time() < deadline:
        last = _state(page)
        if last.get("page") in wanted:
            return last
        time.sleep(0.5)
    return last


# ── main entry point ───────────────────────────────────────────────────────────
def verify_phone(page, *, hero_key, country="PH", log=print,
                 max_replacements=3, sms_timeout=150, allow_country_rotation=True):
    """Complete the OpenAI add-phone + phone-verification steps in `page`.

    `page` is a Playwright sync Page already on (or about to reach) the add-phone page.
    `country` is the PRIMARY ISO-2 country whose proxy region you are exiting from — we
    request the hero-sms number from the same country so the IP and phone agree.

    Returns {"ok", "phone", "country", "channel", "error"}.
    """
    iso = (country or "PH").upper()
    # Build the rotation list: primary first, then cheap fallbacks (deduped).
    order = [iso] + [c for c in FALLBACK_ORDER if c != iso] if allow_country_rotation else [iso]
    order = order[: max_replacements + 1]

    last_err = ""
    for attempt, cur_iso in enumerate(order):
        cfg = COUNTRY_CFG.get(cur_iso)
        if not cfg:
            continue
        dial = cfg["dial"]

        st = _wait_state(page, {"add_phone", "phone_verification", "consent"}, timeout_s=25)
        if st.get("page") == "consent":
            log("   [phone] already past phone step (consent ready)")
            return {"ok": True, "phone": "", "country": cur_iso, "channel": "", "error": ""}
        if st.get("page") != "add_phone":
            log(f"   [phone] not on add-phone (page={st.get('page')}, url={st.get('url','')[:90]})")
            # If we are already on the verification page from a previous try, fall through.
            if st.get("page") != "phone_verification":
                last_err = f"unexpected page {st.get('page')}"
                continue

        order_id = None
        try:
            if st.get("page") == "add_phone":
                order_id, full = hero_get_number(hero_key, cur_iso, log)
                if not order_id:
                    last_err = "hero getNumber failed"
                    continue
                national = to_national(full, dial)
                e164 = to_e164(full, dial)
                log(f"   [phone] {cur_iso} num={full} dial={dial} national={national} e164={e164}")

                prep = page.evaluate("(a) => window.__pv.prepare(a.iso, a.dial)",
                                     {"iso": cur_iso, "dial": dial}) or {}
                log(f"   [phone] prepare: {prep}")
                if prep.get("whatsappOnly"):
                    log("   [phone] page is WhatsApp-only -> release number, rotate")
                    hero_set_status(hero_key, order_id, 8, log)
                    last_err = "whatsapp_only"
                    continue
                # Verify the dropdown actually applied our dial code.
                disp = re.sub(r"\D", "", str(prep.get("displayedDial", "")))
                if disp and disp != dial:
                    log(f"   [phone] dial mismatch displayed={disp} expected={dial} -> rotate")
                    hero_set_status(hero_key, order_id, 8, log)
                    last_err = "dial_mismatch"
                    continue

                fill = page.evaluate("(a) => window.__pv.fillPhone(a.n, a.e)",
                                     {"n": national, "e": e164}) or {}
                if not fill.get("ok"):
                    log(f"   [phone] fillPhone failed: {fill}")
                    hero_set_status(hero_key, order_id, 8, log)
                    last_err = "fill_failed"
                    continue
                # Re-assert SMS channel just before submit (some widgets reset on fill).
                page.evaluate("(a) => window.__pv.prepare(a.iso, a.dial)", {"iso": cur_iso, "dial": dial})
                time.sleep(0.4)
                page.evaluate("() => window.__pv.submitAddPhone()")
                log("   [phone] submitted phone number")

                # Wait for either the verification page or a rejection back on add-phone.
                res_st = _wait_state(page, {"phone_verification", "consent"}, timeout_s=22)
                if res_st.get("page") == "consent":
                    log("   [phone] consent ready right after phone (no SMS needed)")
                    hero_set_status(hero_key, order_id, 6, log)
                    return {"ok": True, "phone": e164, "country": cur_iso, "channel": "sms", "error": ""}
                if res_st.get("page") != "phone_verification":
                    err = page.evaluate("() => window.__pv.addPhoneError()") or ""
                    log(f"   [phone] phone rejected on add-phone: {err!r} -> rotate")
                    hero_set_status(hero_key, order_id, 8, log)
                    last_err = "add_phone_rejected: " + err
                    continue

            # ── phone-verification: wait for the SMS code and submit it ──
            log(f"   [phone] waiting hero SMS (order={order_id}, timeout={sms_timeout}s)")
            code = hero_wait_code(hero_key, order_id, sms_timeout, log) if order_id else None
            if not code:
                log("   [phone] SMS timeout -> rotate")
                if order_id:
                    hero_set_status(hero_key, order_id, 8, log)
                last_err = "sms_timeout"
                continue
            log(f"   [phone] SMS code = {code}")
            page.evaluate("(c) => window.__pv.fillCode(c)", code)

            # Success = we left the phone steps (to consent / localhost callback / chatgpt)
            # with no inline "invalid code" error. Don't require an exact "consent" URL —
            # after verification OpenAI may jump straight to the OAuth redirect.
            deadline = time.time() + 20
            outcome = None
            while time.time() < deadline:
                now = _state(page)
                pg = now.get("page")
                url_now = (now.get("url") or "").lower()
                left_phone = (
                    "localhost:1455" in url_now
                    or pg == "consent"
                    or (pg == "other"
                        and "add-phone" not in url_now
                        and "phone-verification" not in url_now)
                )
                if left_phone:
                    outcome = "ok"
                    break
                verr = page.evaluate("() => window.__pv.verifyError()") or ""
                if verr:
                    outcome = "invalid:" + verr
                    break
                if pg == "add_phone":
                    outcome = "returned_add_phone"
                    break
                time.sleep(0.5)

            if outcome == "ok":
                log("   [phone] ✅ verified -> left phone step")
                hero_set_status(hero_key, order_id, 6, log)
                return {"ok": True, "phone": to_e164(full, dial) if order_id else "",
                        "country": cur_iso, "channel": "sms", "error": ""}

            log(f"   [phone] post-code outcome={outcome or 'timeout'} -> rotate")
            hero_set_status(hero_key, order_id, 8, log)
            last_err = "verify_failed: " + (outcome or "timeout")
            # If it bounced back to add-phone, the next loop iteration will re-acquire.
            continue

        except Exception as e:
            log(f"   [phone] exception: {e}")
            if order_id:
                hero_set_status(hero_key, order_id, 8, log)
            last_err = f"exception: {e}"
            continue

    return {"ok": False, "phone": "", "country": iso, "channel": "", "error": last_err or "exhausted"}
