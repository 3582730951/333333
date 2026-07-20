/* core.js — shared foundation for BOTH front-ends:
     • user portal  : index.html  + portal.js   (served at /)
     • admin console : admin.html  + adminapp.js (served at /admin.html)
   Provides DOM helpers, the API client (cookie session + CSRF + legacy admin_token),
   theme, identity (/auth/me), the login/register gate, and small shared widgets.
   Loaded FIRST in both shells, before charts.js / page renderers / the shell script.
   The two shells each define their own renderShell()/setView()/loadView()/rerender(). */

const $ = (s, r = document) => r.querySelector(s);
const $$ = (s, r = document) => [...r.querySelectorAll(s)];

let TOK = localStorage.getItem("cp_tok") || "";
let ME = null;      // identity from /auth/me: {role, via, authed, email, name, allow_registration}
let VIEW = "";      // active view key (owned by the shell router)
let MODELS = [];    // /v1/models ids, shared by both consoles

function esc(s) { return (s == null ? "" : String(s)).replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c])); }
function fmt(n) { n = +n || 0; if (n >= 1e9) return (n / 1e9).toFixed(2) + "B"; if (n >= 1e6) return (n / 1e6).toFixed(2) + "M"; if (n >= 1e3) return (n / 1e3).toFixed(1) + "k"; return n; }
function toast(msg, kind = "") { const host = $("#toasts"); if (!host) { return; } const el = document.createElement("div"); el.className = "toast " + kind; el.textContent = msg; host.append(el); setTimeout(() => el.remove(), 4200); }
function getCookie(name) { const m = document.cookie.match("(^|;)\\s*" + name + "\\s*=\\s*([^;]+)"); return m ? decodeURIComponent(m.pop()) : ""; }

/* ===== API client ===== */
function headers(extra = {}) {
  const h = { "Content-Type": "application/json", ...extra };
  if (TOK) h["Authorization"] = "Bearer " + TOK;
  const csrf = getCookie("cp_csrf");
  if (csrf) h["X-CP-CSRF"] = csrf;
  return h;
}
async function api(path, opts = {}) {
  const r = await fetch(path, { ...opts, headers: headers(opts.headers || {}), credentials: "same-origin" });
  const txt = await r.text();
  let data; try { data = txt ? JSON.parse(txt) : null; } catch { data = txt; }
  if (!r.ok) { const m = (data && data.error && data.error.message) || (typeof data === "string" && data ? data : r.status); const e = new Error(m); e.status = r.status; e.data = data; throw e; }
  return data;
}

/* ===== theme ===== */
let THEME = localStorage.getItem("cp_theme") || (window.matchMedia && matchMedia("(prefers-color-scheme: light)").matches ? "light" : "dark");
// Persist the resolved default so embedded iframe pages (which read cp_theme on their own
// load) inherit the SAME theme — otherwise they fall back to their hardcoded default and
// render dark inside a light console (the theme-mismatch the embedded tools showed).
try { localStorage.setItem("cp_theme", THEME); } catch {}
// applyFrameChrome pushes the full chrome (theme + preset/density/radius/font axes) onto an
// embedded page's <html>, so iframes match the console exactly, not just light/dark.
function applyFrameChrome(docEl) {
  if (!docEl) return;
  try {
    docEl.setAttribute("data-theme", THEME);
    const g = (k, d) => localStorage.getItem(k) || d;
    const preset = g("cp_theme_preset", "");
    if (preset) docEl.setAttribute("data-theme-preset", preset); else docEl.removeAttribute("data-theme-preset");
    docEl.setAttribute("data-density", g("cp_density", "default"));
    docEl.setAttribute("data-radius", g("cp_radius", "default"));
    docEl.setAttribute("data-font", g("cp_font", "sans"));
  } catch {}
}
function applyTheme() {
  document.documentElement.setAttribute("data-theme", THEME);
  const b = $("#themeBtn"); if (b && typeof icon === "function") b.innerHTML = icon(THEME === "light" ? "moon" : "sun");
  // keep embedded tool pages (admin iframes) on the same theme + design axes
  $$("iframe.embedframe").forEach((f) => { try { applyFrameChrome(f.contentDocument.documentElement); } catch {} });
}
function toggleTheme() { THEME = THEME === "light" ? "dark" : "light"; localStorage.setItem("cp_theme", THEME); applyTheme(); }

/* ===== identity ===== */
function isAdmin() { return !!(ME && ME.role === "admin"); }
async function fetchMe() { try { ME = await api("/auth/me"); } catch { ME = { authed: false }; } return ME; }
async function doLogout() {
  try { await api("/auth/logout", { method: "POST" }); } catch {}
  TOK = ""; localStorage.removeItem("cp_tok"); ME = null;
  location.reload();
}

/* ===== login / register gate =====
   Renders the auth card into `container` and invokes onAuthed() once a session (or
   admin_token / open-admin identity) is established. Generic so both shells reuse it. */
function renderAuth(container, mode, allowReg, onAuthed) {
  const av = typeof container === "string" ? $(container) : container;
  if (!av) return;
  const isLogin = mode !== "register";
  const sub = (typeof t === "function") ? t("brand.sub") : "Codex · Claude";
  const T = (k, fb) => (typeof t === "function" ? t(k) : fb);
  av.innerHTML = `<div class="authwrap"><div class="authcard">
    <div class="brand"><div class="logo">CP</div><div><b>Pool</b><small>${esc(sub)}</small></div></div>
    <h2>${esc(isLogin ? T("auth.welcome", "欢迎回来") : T("auth.create", "创建账户"))}</h2>
    <p class="sub">${esc(isLogin ? T("auth.sub_login", "登录以管理你的密钥与用量") : T("auth.sub_register", "注册一个新账户开始使用"))}</p>
    <label class="f">${esc(T("auth.email", "邮箱"))}</label><input class="t" id="auEmail" type="email" autocomplete="username">
    ${isLogin ? "" : `<label class="f">${esc(T("auth.name", "昵称（可选）"))}</label><input class="t" id="auName">`}
    <label class="f">${esc(T("auth.password", "密码"))}</label><input class="t" id="auPass" type="password" autocomplete="${isLogin ? "current-password" : "new-password"}">
    <div class="err" id="auErr"></div>
    <div style="height:6px"></div>
    <button class="btn pri" style="width:100%;justify-content:center" id="auSubmit">${esc(isLogin ? T("auth.login_btn", "登录") : T("auth.register_btn", "注册"))}</button>
    <div class="switch">${allowReg === false && !isLogin ? esc(T("auth.reg_disabled", "管理员已关闭注册")) :
      `<a id="auSwitch">${esc(isLogin ? T("auth.to_register", "没有账户？去注册") : T("auth.to_login", "已有账户？去登录"))}</a>`}</div>
    ${isLogin ? `<div class="sect" style="margin-top:16px">${(typeof LANG !== "undefined" && LANG === "en") ? "or sign in with an Admin Token" : "或使用管理员令牌登录"}</div>
      <div class="row" style="flex-wrap:nowrap;gap:6px"><input class="t mono" id="auTok" type="password" placeholder="Admin Token"><button class="btn" id="auTokBtn">${esc(T("auth.use_token", "进入"))}</button></div>` : ""}
    ${ME && ME.via === "open" ? `<div class="note" style="margin-top:14px">${esc(T("auth.open_note", "当前为开放部署（未设置 admin_token）。"))}<div style="height:8px"></div><button class="btn" id="auOpen">${esc(T("auth.enter_admin", "以管理员身份进入"))}</button></div>` : ""}
  </div></div>`;
  const submit = async () => {
    $("#auErr").textContent = "";
    const email = ($("#auEmail").value || "").trim(), pass = $("#auPass").value;
    try {
      if (isLogin) ME = await api("/auth/login", { method: "POST", body: JSON.stringify({ email, password: pass }) });
      else ME = await api("/auth/register", { method: "POST", body: JSON.stringify({ email, password: pass, name: ($("#auName") || {}).value || "" }) });
      onAuthed();
    } catch (e) { $("#auErr").textContent = e.message; }
  };
  $("#auSubmit").onclick = submit;
  $("#auPass").onkeydown = (e) => { if (e.key === "Enter") submit(); };
  if ($("#auSwitch")) $("#auSwitch").onclick = () => renderAuth(av, isLogin ? "register" : "login", allowReg, onAuthed);
  if ($("#auTokBtn")) $("#auTokBtn").onclick = async () => { TOK = ($("#auTok").value || "").trim(); if (!TOK) return; localStorage.setItem("cp_tok", TOK); await fetchMe(); onAuthed(); };
  if ($("#auOpen")) $("#auOpen").onclick = () => { ME = { role: "admin", via: "open", authed: true }; onAuthed(); };
  if (typeof applyI18n === "function") applyI18n(av);
}

/* ===== shared widgets / probes ===== */
function provChip(p) { if (p === "claude") return '<span class="chip claude">Claude</span>'; if (!p || p === "codex") return '<span class="chip codex">Codex</span>'; return `<span class="chip custom">${esc(p)}</span>`; }
function statusChip(s) { const m = { active: "ok", disabled: "warn", quarantined: "bad" }; return `<span class="chip ${m[s] || ""}">${esc(s)}</span>`; }

async function ping() { const d = $("#hdot"); if (!d) return; try { await api("/healthz"); d.className = "dot ok"; } catch { d.className = "dot bad"; } }
async function refreshModels() {
  try { const m = await api("/v1/models"); MODELS = (m.data || []).map((x) => x.id); const dl = $("#modelOpts"); if (dl) dl.innerHTML = MODELS.map((id) => `<option value="${esc(id)}">`).join(""); } catch {}
  return MODELS;
}

/* ===== clipboard + secret/select modals (shared by both consoles) ===== */
/* copyText: copy an arbitrary string to the clipboard with a graceful
   execCommand fallback for non-secure (http://IP) contexts, where the async
   Clipboard API is unavailable — the common case for a self-hosted pool. */
function copyText(text) {
  text = text == null ? "" : String(text);
  const done = () => toast(typeof t === "function" ? t("ok.copied") : "Copied", "ok");
  const fail = () => toast("Ctrl/⌘ + C", "");
  if (navigator.clipboard && window.isSecureContext) {
    navigator.clipboard.writeText(text).then(done, () => fallbackCopyText(text, done, fail));
  } else {
    fallbackCopyText(text, done, fail);
  }
}
function fallbackCopyText(text, done, fail) {
  const ta = document.createElement("textarea");
  ta.value = text; ta.setAttribute("readonly", "");
  ta.style.cssText = "position:fixed;left:-9999px;top:0;opacity:0";
  document.body.appendChild(ta); ta.focus(); ta.select();
  try { (document.execCommand("copy") ? done : fail)(); } catch { fail(); }
  document.body.removeChild(ta);
}
function fileInstallCommand(key) {
  return `curl -fsSL '${location.origin}/file/${encodeURIComponent(key || "")}' | bash`;
}
function copyEncodedText(encoded) {
  try { copyText(decodeURIComponent(encoded || "")); } catch { copyText(encoded || ""); }
}
// keyCopyCell renders "Copy key" + "Copy install" buttons for a downstream key whose
// plaintext secret was stored (created after the secret column existed). The install
// command is the one-shot setup script (Codex + Claude Code + rtk). Legacy hash-only
// keys can't be recovered → prompt to rotate. Shared by the admin Keys page and the
// user portal's My Keys (both load core.js); previously duplicated byte-for-byte in
// admin.js and user.js.
function keyCopyCell(k) {
  const en = LANG === "en";
  if (!k.secret) return `<span class="muted" style="font-size:12px">${en ? "rotate to copy" : "需轮换才可复制"}</span>`;
  const cmd = fileInstallCommand(k.secret);
  return `<div class="row" style="gap:4px;flex-wrap:wrap"><button class="btn sm" title="${esc(k.secret)}" onclick="copyEncodedText('${encodeURIComponent(k.secret)}')">${en ? "Copy key" : "复制 Key"}</button><button class="btn sm" onclick="copyEncodedText('${encodeURIComponent(cmd)}')">${en ? "Copy install" : "复制安装命令"}</button></div>`;
}
/* openModal: append a centered overlay+card to <body>; click-away / Esc / close()
   all remove it. Returns { ov, close }. Self-contained styling (CSS vars) so it
   needs no markup in the shell HTML. */
function openModal(innerHTML) {
  const ov = document.createElement("div");
  ov.className = "cp-modal-ov";
  ov.style.cssText = "position:fixed;inset:0;z-index:9999;display:flex;align-items:center;justify-content:center;padding:18px;background:rgba(4,6,12,.55);backdrop-filter:blur(3px)";
  ov.innerHTML = `<div class="cp-modal" style="background:var(--solid);border:1px solid var(--line2);border-radius:var(--radius);box-shadow:var(--shadow);max-width:560px;width:100%;padding:20px">${innerHTML}</div>`;
  const close = () => { ov.remove(); document.removeEventListener("keydown", onKey); };
  const onKey = (e) => { if (e.key === "Escape") close(); };
  ov.addEventListener("click", (e) => { if (e.target === ov) close(); });
  document.addEventListener("keydown", onKey);
  document.body.appendChild(ov);
  return { ov, close };
}
/* showSecretModal: reveal a one-time secret (a freshly created API key) in a
   read-only field with a Copy button — replaces the old alert(), which could not
   be copied conveniently. #1: "apikey 必须可复制". */
function showSecretModal(value, opts = {}) {
  const en = (typeof LANG !== "undefined" && LANG === "en");
  const title = opts.title || (en ? "API Key (shown once)" : "API Key（仅显示一次）");
  const note = opts.note || (en ? "Copy and store it now — it cannot be shown again." : "请立即复制保存，关闭后无法再次查看。");
  const extra = opts.extra || "";
  const { ov, close } = openModal(`
    <div style="display:flex;align-items:center;gap:8px;margin-bottom:12px"><b style="font-size:15px">${esc(title)}</b></div>
    <div class="row" style="gap:6px;flex-wrap:nowrap;align-items:stretch">
      <input class="t mono" id="cpSecretVal" readonly value="${esc(value)}" style="flex:1" onclick="this.select()" />
      <button class="btn pri" id="cpSecretCopy">${en ? "Copy" : "复制"}</button>
    </div>
    <p class="muted" style="margin:10px 0 0;font-size:12px">${esc(note)}</p>${extra}
    <div class="row" style="justify-content:flex-end;margin-top:16px"><button class="btn" id="cpSecretClose">${en ? "Done" : "完成"}</button></div>`);
  const inp = ov.querySelector("#cpSecretVal");
  ov.querySelector("#cpSecretCopy").onclick = () => { copyText(value); if (inp) { inp.focus(); inp.select(); } };
  ov.querySelector("#cpSecretClose").onclick = close;
  if (inp) { inp.focus(); inp.select(); }
  return { ov, close };
}
/* modelSelectHTML: build a <select> populated from the live /v1/models list
   (MODELS). #2: the model must be CHOSEN from what the pool offers, never free-
   typed. A previously-saved value not in the live list is preserved as the first
   option so editing never silently drops it. Caller should `await refreshModels()`
   first so MODELS is warm. */
function modelSelectHTML(id, current, opts = {}) {
  const en = (typeof LANG !== "undefined" && LANG === "en");
  const list = (MODELS || []).slice();
  current = (current == null ? "" : String(current)).trim();
  if (current && list.indexOf(current) === -1) list.unshift(current);
  const head = opts.allowEmpty === false ? "" :
    `<option value="">${esc(opts.emptyLabel || (en ? "(auto · follow request)" : "（自动 · 跟随请求）"))}</option>`;
  const body = list.map((m) => `<option value="${esc(m)}" ${m === current ? "selected" : ""}>${esc(m)}</option>`).join("");
  const cls = opts.cls || "t";
  return `<select class="${cls}" id="${esc(id)}">${head}${body}</select>`;
}
/* selectModal: a small modal wrapping a single <select> (used to replace prompt()
   for editing a key's force_model). Resolves to the chosen value, or null if
   cancelled. */
function selectModal(title, selectHTML, current) {
  const en = (typeof LANG !== "undefined" && LANG === "en");
  return new Promise((resolve) => {
    const { ov, close } = openModal(`
      <div style="margin-bottom:12px"><b style="font-size:15px">${esc(title)}</b></div>
      <div id="cpSelWrap">${selectHTML}</div>
      <div class="row" style="justify-content:flex-end;gap:8px;margin-top:16px">
        <button class="btn" id="cpSelCancel">${en ? "Cancel" : "取消"}</button>
        <button class="btn pri" id="cpSelOk">${en ? "Save" : "保存"}</button></div>`);
    const sel = ov.querySelector("#cpSelWrap select");
    if (sel && current != null) sel.value = current;
    ov.querySelector("#cpSelCancel").onclick = () => { close(); resolve(null); };
    ov.querySelector("#cpSelOk").onclick = () => { const v = sel ? sel.value : ""; close(); resolve(v); };
  });
}
/* keyUsageHint: shown under a freshly created key (admin + user) — the one-liner
   that auto-configures the codex CLI with this key via the /file/{key} download,
   so the operator can copy it immediately while the plaintext key is still visible. */
function keyUsageHint(key) {
  const en = (typeof LANG !== "undefined" && LANG === "en");
  const cmd = fileInstallCommand(key);
  return `<div class="sect" style="margin-top:14px">${en ? "One-shot Codex + Claude Code setup" : "一键配置 Codex + Claude Code"}</div>
    <div class="row" style="gap:6px;flex-wrap:nowrap;align-items:stretch">
      <input class="t mono" readonly value="${esc(cmd)}" style="flex:1" onclick="this.select()" />
      <button class="btn" onclick="copyEncodedText('${encodeURIComponent(cmd)}')">${en ? "Copy" : "复制"}</button></div>
    <p class="muted" style="margin:8px 0 0;font-size:12px">${en ? "Run once on the client to configure Codex, Claude Code, and rtk for this pool." : "在客户端执行一次即可配置 Codex、Claude Code 和 rtk 指向本池。"}</p>`;
}

/* shared usage time-series definition (admin Usage/Overview + user My Usage) */
const USAGE_SERIES = [
  { key: "prompt_tokens", label: "in", color: "#5b8cff", fill: "url(#gradAcc)" },
  { key: "completion_tokens", label: "out", color: "#7c5cff", fill: "url(#gradVio)" },
  { key: "cached_tokens", label: "cache", color: "#34d399", fill: "url(#gradOk)" },
];
function usageLegend() {
  const en = (typeof LANG !== "undefined" && LANG === "en");
  return `<span class="legend"><span><i style="background:#5b8cff"></i>${en ? "input" : "输入"}</span><span><i style="background:#7c5cff"></i>${en ? "output" : "输出"}</span><span><i style="background:#34d399"></i>${en ? "cached" : "缓存"}</span></span>`;
}

/* SVG gradient defs shared by the inline charts — injected once into either shell. */
function injectChartDefs() {
  if ($("#cpChartDefs")) return;
  const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
  svg.id = "cpChartDefs"; svg.setAttribute("width", "0"); svg.setAttribute("height", "0");
  svg.setAttribute("style", "position:absolute"); svg.setAttribute("aria-hidden", "true");
  svg.innerHTML = `<defs>
    <linearGradient id="gradAcc" x1="0" y1="0" x2="0" y2="1"><stop offset="0%" stop-color="#5b8cff" stop-opacity=".55"/><stop offset="100%" stop-color="#5b8cff" stop-opacity="0"/></linearGradient>
    <linearGradient id="gradVio" x1="0" y1="0" x2="0" y2="1"><stop offset="0%" stop-color="#7c5cff" stop-opacity=".5"/><stop offset="100%" stop-color="#7c5cff" stop-opacity="0"/></linearGradient>
    <linearGradient id="gradOk" x1="0" y1="0" x2="0" y2="1"><stop offset="0%" stop-color="#34d399" stop-opacity=".5"/><stop offset="100%" stop-color="#34d399" stop-opacity="0"/></linearGradient>
    <linearGradient id="gradClaude" x1="0" y1="0" x2="0" y2="1"><stop offset="0%" stop-color="#d39a6a" stop-opacity=".5"/><stop offset="100%" stop-color="#d39a6a" stop-opacity="0"/></linearGradient>
    <linearGradient id="strokeAcc" x1="0" y1="0" x2="1" y2="0"><stop offset="0%" stop-color="#5b8cff"/><stop offset="100%" stop-color="#7c5cff"/></linearGradient>
  </defs>`;
  document.body.prepend(svg);
}


/* ===== Sortable table =====
   Call makeTableSortable(tableEl) to enable click-to-sort on column headers.
   Columns with data-sort="number" sort numerically; others sort alphabetically.
   Adds ▲/▼ indicators. */
function makeTableSortable(table) {
  if (!table || table.dataset.sortable === "1") return;
  table.dataset.sortable = "1";
  const headers = table.querySelectorAll("th");
  const en = (typeof LANG !== "undefined" && LANG === "en");

  // Multi-column sort stack: [{colIdx, dir}] — first entry = primary sort
  let sortStack = [];

  const doSort = () => {
    const tbody = table.querySelector("tbody");
    if (!tbody) return;
    const rows = [...tbody.querySelectorAll("tr")];
    if (!rows.length) return;

    rows.sort((a, b) => {
      for (const entry of sortStack) {
        const {colIdx, dir} = entry;
        const ca = (a.children[colIdx] || {}).textContent || "";
        const cb = (b.children[colIdx] || {}).textContent || "";
        let va = ca.trim(), vb = cb.trim();
        const numeric = headers[colIdx] && headers[colIdx].dataset.sort === "number";
        if (numeric) { va = parseFloat(va) || 0; vb = parseFloat(vb) || 0; }
        if (va < vb) return dir === "asc" ? -1 : 1;
        if (va > vb) return dir === "asc" ? 1 : -1;
        // equal → fall through to next sort column
      }
      return 0;
    });
    rows.forEach((r) => tbody.appendChild(r));
  };

  const updateIndicators = () => {
    // Clear all
    headers.forEach((h) => {
      delete h.dataset.sortDir;
      delete h.dataset.sortPriority;
      h.classList.remove("sort-asc", "sort-desc");
      h.style.setProperty("--sort-prio", '""');
      if (!h.classList.contains("no-sort")) h.setAttribute("aria-sort", "none");
    });
    // Set from stack with priority numbers
    sortStack.forEach((entry, i) => {
      const th = headers[entry.colIdx];
      if (!th) return;
      th.dataset.sortDir = entry.dir;
      th.dataset.sortPriority = String(i + 1);
      th.classList.add(entry.dir === "asc" ? "sort-asc" : "sort-desc");
      th.setAttribute("aria-sort", entry.dir === "asc" ? "ascending" : "descending");
    });
  };

  headers.forEach((th, colIdx) => {
    if (th.classList.contains("no-sort")) return;
    th.style.cursor = "pointer";
    th.setAttribute("role", "columnheader");
    th.setAttribute("tabindex", "0");
    th.setAttribute("aria-sort", "none");
    th.title = en
      ? "Click to sort · Shift+Click for multi-column sort"
      : "点击排序 · Shift+点击多列排序";
    // Keyboard parity: the header is focusable, so Enter/Space must trigger the same
    // sort a click does (Space also scrolls the page by default — suppress that).
    th.addEventListener("keydown", (e) => {
      if (e.key === "Enter" || e.key === " " || e.key === "Spacebar") {
        e.preventDefault();
        th.click();
      }
    });

    th.addEventListener("click", (e) => {
      const multi = e.shiftKey;

      if (!multi) {
        // Single-column sort: clear stack, start fresh
        const existing = sortStack.find((s) => s.colIdx === colIdx);
        if (existing && sortStack.length === 1) {
          // Toggle direction on same single column
          existing.dir = existing.dir === "asc" ? "desc" : "asc";
        } else {
          sortStack = [{colIdx, dir: "asc"}];
        }
      } else {
        // Multi-column sort
        const existingIdx = sortStack.findIndex((s) => s.colIdx === colIdx);
        if (existingIdx >= 0) {
          // Column already in stack: asc → desc → remove
          if (sortStack[existingIdx].dir === "asc") {
            sortStack[existingIdx].dir = "desc";
          } else {
            sortStack.splice(existingIdx, 1);
          }
        } else {
          // Add to end of stack (lowest priority)
          sortStack.push({colIdx, dir: "asc"});
        }
      }

      if (!sortStack.length) {
        // All columns removed — clear indicators, restore original order
        updateIndicators();
        // Restore original order by re-appending rows in DOM order
        const tbody2 = table.querySelector("tbody");
        if (tbody2) {
          const rows2 = [...tbody2.querySelectorAll("tr")];
          rows2.forEach((r) => tbody2.appendChild(r)); // no-op, keeps current order
        }
        return;
      }

      updateIndicators();
      doSort();
    });
  });

  // Inject multi-column sort indicator styles
  const styleId = "cpSortStyle";
  if (!document.getElementById(styleId)) {
    const s = document.createElement("style");
    s.id = styleId;
    s.textContent =
      "th.sort-asc::after{content:' ▲';font-size:9px}" +
      "th.sort-desc::after{content:' ▼';font-size:9px}" +
      "th[data-sort-priority]::before{content:attr(data-sort-priority);font-size:8px;" +
      "  color:var(--acc);font-weight:700;margin-right:2px;vertical-align:super;line-height:1}";
    document.head.appendChild(s);
  }
}

/* makeAllTablesSortable: auto-enhance all tables inside a container */
function makeAllTablesSortable(container) {
  (container || document).querySelectorAll("table").forEach(makeTableSortable);
}


/* ===== Table pagination =====
   Wraps a table with pagination controls. Call makeTablePageable(table, 15) to show
   15 rows per page with prev/next + page buttons. */
function makeTablePageable(table, pageSize) {
  pageSize = pageSize || 15;
  if (!table || table.dataset.pageable === "1") return;
  table.dataset.pageable = "1";
  const tbody = table.querySelector("tbody");
  if (!tbody) return;
  const allRows = [...tbody.querySelectorAll("tr")];
  if (allRows.length <= pageSize) return; // no pagination needed
  let currentPage = 1;
  const totalPages = Math.ceil(allRows.length / pageSize);

  // Create pagination container
  const pagDiv = document.createElement("div");
  pagDiv.className = "pag";
  const en = (typeof LANG !== "undefined" && LANG === "en");

  const render = () => {
    const start = (currentPage - 1) * pageSize;
    const end = Math.min(start + pageSize, allRows.length);
    allRows.forEach((r, i) => { r.style.display = (i >= start && i < end) ? "" : "none"; });

    let pagesHTML = "";
    const maxShow = 5;
    let pStart = Math.max(1, currentPage - Math.floor(maxShow / 2));
    let pEnd = Math.min(totalPages, pStart + maxShow - 1);
    if (pEnd - pStart < maxShow - 1) pStart = Math.max(1, pEnd - maxShow + 1);
    for (let p = pStart; p <= pEnd; p++) {
      pagesHTML += `<button class="${p === currentPage ? "on" : ""}" data-pg="${p}">${p}</button>`;
    }
    if (pStart > 1) pagesHTML = `<button data-pg="1">1</button>${pStart > 2 ? "<span class=\"muted\">…</span>" : ""}` + pagesHTML;
    if (pEnd < totalPages) pagesHTML += `${pEnd < totalPages - 1 ? "<span class=\"muted\">…</span>" : ""}<button data-pg="${totalPages}">${totalPages}</button>`;

    pagDiv.innerHTML = `
      <span class="info">${en ? "Showing" : "第"} ${start+1}-${end} ${en ? "of" : "条，共"} ${allRows.length}</span>
      <div class="ctls">
        <select class="pg-size"><option value="10" ${pageSize===10?"selected":""}>10</option><option value="15" ${pageSize===15?"selected":""}>15</option><option value="25" ${pageSize===25?"selected":""}>25</option><option value="50" ${pageSize===50?"selected":""}>50</option></select>
        <button data-pg="${currentPage-1}" ${currentPage===1?"disabled":""}>‹</button>
        <span class="pages">${pagesHTML}</span>
        <button data-pg="${currentPage+1}" ${currentPage===totalPages?"disabled":""}>›</button>
      </div>`;

    // Wire events
    pagDiv.querySelectorAll("button[data-pg]").forEach((b) => {
      b.onclick = () => { const p = parseInt(b.dataset.pg); if (p >= 1 && p <= totalPages) { currentPage = p; render(); } };
    });
    const sizeSel = pagDiv.querySelector(".pg-size");
    if (sizeSel) {
      sizeSel.onchange = () => {
        pageSize = parseInt(sizeSel.value);
        currentPage = 1;
        // Remove old pag, re-paginate
        const oldPag = table.parentNode.querySelector(".pag");
        if (oldPag) oldPag.remove();
        makeTablePageable(table, pageSize);
      };
    }
  };

  render();
  table.parentNode.insertBefore(pagDiv, table.nextSibling);
}

/* ===== Table filter row =====
   Adds a filter input row above a table. columns: array of {index, placeholder, type:"text"|"select", options:[...]} */
function addTableFilter(table, columns) {
  if (!table || table.dataset.filtered === "1") return;
  table.dataset.filtered = "1";
  const tbody = table.querySelector("tbody");
  if (!tbody) return;
  const allRows = [...tbody.querySelectorAll("tr")];
  const en = (typeof LANG !== "undefined" && LANG === "en");

  const filterRow = document.createElement("div");
  filterRow.className = "tf-row";

  columns.forEach((col) => {
    if (col.type === "select") {
      const sel = document.createElement("select");
      sel.innerHTML = `<option value="">${col.placeholder || ""}</option>` + (col.options || []).map((o) => `<option value="${esc(o)}">${esc(o)}</option>`).join("");
      sel.addEventListener("input", applyFilters);
      filterRow.appendChild(sel);
    } else {
      const inp = document.createElement("input");
      inp.type = "text";
      inp.placeholder = col.placeholder || (en ? "Filter…" : "筛选…");
      inp.dataset.colIdx = col.index;
      inp.addEventListener("input", applyFilters);
      filterRow.appendChild(inp);
    }
  });

  function applyFilters() {
    const inputs = [...filterRow.querySelectorAll("input")];
    const selects = [...filterRow.querySelectorAll("select")];
    allRows.forEach((row) => {
      let match = true;
      inputs.forEach((inp) => {
        const idx = parseInt(inp.dataset.colIdx);
        if (isNaN(idx)) return;
        const cell = row.children[idx];
        const cellText = (cell ? cell.textContent : "").toLowerCase();
        const q = inp.value.toLowerCase();
        if (q && !cellText.includes(q)) match = false;
      });
      selects.forEach((sel) => {
        if (!sel.value) return;
        // Find the column index from the select's position
        const selIdx = selects.indexOf(sel);
        const selectCol = columns.filter((c) => c.type === "select")[selIdx];
        if (!selectCol) return;
        const cell = row.children[selectCol.index];
        const cellText = (cell ? cell.textContent : "").trim().toLowerCase();
        if (sel.value && !cellText.includes(sel.value.toLowerCase())) match = false;
      });
      row.style.display = match ? "" : "none";
    });
    // Re-trigger pagination if present
    const pagDiv = table.parentNode.querySelector(".pag");
    if (pagDiv && typeof makeTablePageable === "function") {
      // Reset pagination
      const tbody2 = table.querySelector("tbody");
      if (tbody2) {
        const visibleRows = [...tbody2.querySelectorAll("tr")].filter((r) => r.style.display !== "none");
        // Simple: just let pagination re-count
        table.dataset.pageable = "0";
        pagDiv.remove();
        if (visibleRows.length > 15) makeTablePageable(table, 15);
      }
    }
  }

  const container = table.parentNode;
  container.insertBefore(filterRow, table);
}

/* initialize density / radius / font from localStorage */
function initAppearance() {
  const preset = localStorage.getItem("cp_theme_preset") || "";
  const density = localStorage.getItem("cp_density") || "default";
  const radius = localStorage.getItem("cp_radius") || "default";
  const font = localStorage.getItem("cp_font") || "sans";
  if (preset) document.documentElement.setAttribute("data-theme-preset", preset);
  if (density !== "default") document.documentElement.setAttribute("data-density", density);
  if (radius !== "default") document.documentElement.setAttribute("data-radius", radius);
  if (font !== "sans") document.documentElement.setAttribute("data-font", font);
}


/* ===== Column resize =====
   Call makeTableResizable(table) to let users drag column borders to resize.
   Adds a drag handle to the right edge of every <th>. The table gets table-layout:fixed
   and explicit column widths so resizing works predictably. Resized widths are persisted
   to a single localStorage key (per page/view). */
function makeTableResizable(table) {
  if (!table || table.dataset.resizable === "1") return;
  table.dataset.resizable = "1";
  // Switch to fixed layout so width settings are honored
  table.style.tableLayout = "fixed";
  table.style.width = "100%";

  const headers = table.querySelectorAll("th");
  if (!headers.length) return;

  // Restore persisted widths if any
  const viewKey = (typeof VIEW !== "undefined" && VIEW) || (table.closest("[data-view]") ? table.closest("[data-view]").dataset.view : "");
  const storageKey = "cp_colw_" + (viewKey || "default");
  let savedWidths = {};
  try { savedWidths = JSON.parse(localStorage.getItem(storageKey) || "{}"); } catch {}

  // Initialize column widths
  headers.forEach((th, i) => {
    const saved = savedWidths[i];
    if (saved) { th.style.width = saved + "px"; }
    else if (!th.style.width) {
      // Set a reasonable default min-width
      const textLen = (th.textContent || "").length;
      th.style.width = Math.max(60, Math.min(200, textLen * 10 + 20)) + "px";
    }
    // Add resize handle
    if (th.querySelector(".col-resize")) return; // already has one
    const handle = document.createElement("div");
    handle.className = "col-resize";
    handle.title = (typeof LANG !== "undefined" && LANG === "en") ? "Drag to resize" : "拖拽调整列宽";
    th.appendChild(handle);

    // Drag logic
    let startX, startW;
    handle.addEventListener("mousedown", (e) => {
      e.preventDefault();
      e.stopPropagation();
      startX = e.clientX;
      startW = th.offsetWidth;
      handle.classList.add("active");
      th.classList.add("resizing");
      document.body.style.cursor = "col-resize";
      document.body.style.userSelect = "none";

      const onMove = (ev) => {
        const dx = ev.clientX - startX;
        const newW = Math.max(40, startW + dx);
        th.style.width = newW + "px";
        // Auto-save on drag end
      };
      const onUp = () => {
        handle.classList.remove("active");
        th.classList.remove("resizing");
        document.body.style.cursor = "";
        document.body.style.userSelect = "";
        document.removeEventListener("mousemove", onMove);
        document.removeEventListener("mouseup", onUp);
        // Persist
        const widths = {};
        headers.forEach((h, j) => { widths[j] = h.offsetWidth; });
        try { localStorage.setItem(storageKey, JSON.stringify(widths)); } catch {}
      };
      document.addEventListener("mousemove", onMove);
      document.addEventListener("mouseup", onUp);
    });

    // Prevent sort from triggering when clicking the resize handle
    handle.addEventListener("click", (e) => { e.stopPropagation(); });
  });
}

/* ===== Sticky columns =====
   Call makeTableSticky(table) to pin the first column (or first N columns) so
   they stay visible during horizontal scroll. Adds the .tbl-sticky class and
   sets data-sticky-cols attribute. */
function makeTableSticky(table, colCount) {
  colCount = colCount || 1;
  if (!table || table.dataset.sticky === "1") return;
  table.dataset.sticky = "1";
  table.classList.add("tbl-sticky");
  if (colCount > 1) table.setAttribute("data-sticky-cols", String(colCount));
}

/* ===== Auto-enhance all tables in container ===== */
function enhanceAllTables(container) {
  (container || document).querySelectorAll("table").forEach((tbl) => {
    if (tbl.closest(".no-enhance")) return; // opt-out marker
    const cols = tbl.querySelectorAll("th").length;
    if (cols < 2) return;
    makeTableResizable(tbl);
    if (cols >= 4) makeTableSticky(tbl, 1);
    addColumnToggle(tbl);
  });
}

/* ===== Column visibility toggle =====
   Adds a "Columns" dropdown button before the table. Users can check/uncheck
   columns to show/hide them. Visibility is persisted per view in localStorage. */
function addColumnToggle(table) {
  if (!table || table.dataset.colToggle === "1") return;
  table.dataset.colToggle = "1";

  const headers = [...table.querySelectorAll("th")];
  if (headers.length < 2) return;

  // Generate column keys from header text
  const viewKey = (typeof VIEW !== "undefined" && VIEW) ||
    (table.closest("[data-view]") ? table.closest("[data-view]").dataset.view : "") ||
    "default";
  const storageKey = "cp_colvis_" + viewKey;

  // Assign data-col attributes to all th and corresponding td
  headers.forEach((th, i) => {
    const key = th.textContent.trim().slice(0, 20).replace(/[^a-zA-Z0-9\u4e00-\u9fff]/g, "_") || "col" + i;
    th.setAttribute("data-col", key);
    // Tag corresponding td cells
    const rows = table.querySelectorAll("tbody tr");
    rows.forEach((row) => {
      const cell = row.children[i];
      if (cell) cell.setAttribute("data-col", key);
    });
  });

  // Load saved visibility
  let hiddenCols = {};
  try { hiddenCols = JSON.parse(localStorage.getItem(storageKey) || "{}"); } catch {}

  // Apply saved state
  headers.forEach((th) => {
    const key = th.getAttribute("data-col");
    if (hiddenCols[key]) {
      th.setAttribute("data-col-hidden", "");
      table.querySelectorAll(`[data-col="${key}"]`).forEach((el) => el.setAttribute("data-col-hidden", ""));
      // Also adjust sticky indices if first column is hidden
    }
  });

  // Create toggle UI
  const en = (typeof LANG !== "undefined" && LANG === "en");
  const wrap = document.createElement("span");
  wrap.className = "col-toggle-wrap";

  const btn = document.createElement("button");
  btn.className = "col-toggle-btn";
  btn.innerHTML = (typeof icon === "function" ? icon("gear", 13) : "⚙") + " " + (en ? "Cols" : "列");
  btn.title = en ? "Show/hide columns" : "显示/隐藏列";

  const drop = document.createElement("div");
  drop.className = "col-toggle-drop";

  const renderChecks = () => {
    drop.innerHTML = headers.map((th) => {
      const key = th.getAttribute("data-col");
      const label = th.textContent.trim();
      const checked = !hiddenCols[key] ? "checked" : "";
      return `<label><input type="checkbox" data-col="${key}" ${checked}> ${label}</label>`;
    }).join("");

    drop.querySelectorAll("input").forEach((cb) => {
      cb.addEventListener("change", () => {
        const key = cb.dataset.col;
        hiddenCols[key] = !cb.checked;
        // Toggle all elements with this data-col
        table.querySelectorAll(`[data-col="${key}"]`).forEach((el) => {
          if (cb.checked) el.removeAttribute("data-col-hidden");
          else el.setAttribute("data-col-hidden", "");
        });
        // Persist
        try { localStorage.setItem(storageKey, JSON.stringify(hiddenCols)); } catch {}
        // Re-check sticky columns — if first col was hidden, update sticky
        updateStickyAfterToggle(table);
      });
    });
  };

  btn.addEventListener("click", (e) => {
    e.stopPropagation();
    e.preventDefault();
    // Close other open toggles
    document.querySelectorAll(".col-toggle-drop.show").forEach((d) => {
      if (d !== drop) d.classList.remove("show");
    });
    drop.classList.toggle("show");
    if (drop.classList.contains("show")) renderChecks();
  });

  // Click-away to close
  document.addEventListener("click", (e) => {
    if (!wrap.contains(e.target)) drop.classList.remove("show");
  });

  wrap.appendChild(btn);
  wrap.appendChild(drop);

  // Insert before the table
  table.parentNode.insertBefore(wrap, table);

  // Ensure table wrapper handles the toggle button nicely
  const panel = table.closest(".bd");
  if (panel) {
    wrap.style.marginBottom = "6px";
    wrap.style.display = "block";
  }
}

/* Re-check sticky positioning after column toggle */
function updateStickyAfterToggle(table) {
  if (!table.classList.contains("tbl-sticky")) return;
  // If first column is hidden, the sticky offset needs to shift
  const firstTh = table.querySelector("th:not([data-col-hidden])");
  if (firstTh) {
    firstTh.style.left = "0";
  }
}

/* global UX: click-away closes the user menu, Escape closes drawers/modals (if present) */
document.addEventListener("click", () => { const m = $("#userMenu"); if (m) m.classList.remove("open"); });
document.addEventListener("keydown", (e) => { if (e.key === "Escape") { if (typeof closeDrawer === "function") closeDrawer(); if (typeof closeImport === "function") closeImport(); } });
