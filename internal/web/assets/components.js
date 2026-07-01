/* components.js — UI component factory for both consoles.
   Provides reusable building blocks: skeleton, empty/error states, badges,
   progress bars, confirm dialogs, and panel wrappers. All functions return
   HTML strings so they work with the existing string-concatenation pattern.
   Depends on: esc(), fmt(), icon(), t() from core.js / i18n.js / icons.js. */

const UI = {
  /* ---- Skeleton / Loading ---- */
  skeleton(w, h, inline) {
    const style = `width:${w};height:${h};` + (inline ? "display:inline-block;" : "");
    return `<span class="skel" style="${style}" aria-hidden="true"></span>`;
  },
  /* Pre-built skeleton templates for common patterns */
  kpiSkeleton() {
    return `<div class="kpi"><div class="k">${UI.skeleton("60px","12px",true)}</div><div class="v">${UI.skeleton("80px","27px",true)}</div></div>`;
  },
  tableSkeleton(cols, rows) {
    rows = rows || 5; cols = cols || 4;
    const hd = "<tr>" + Array.from({length: cols}, () => `<th>${UI.skeleton("60px","11px",true)}</th>`).join("") + "</tr>";
    const bd = Array.from({length: rows}, () =>
      "<tr>" + Array.from({length: cols}, () => `<td>${UI.skeleton("80px","13px",true)}</td>`).join("") + "</tr>"
    ).join("");
    return `<table><thead>${hd}</thead><tbody>${bd}</tbody></table>`;
  },
  chartSkeleton(w, h) {
    return `<div class="skel" style="width:${w || "100%"};height:${h || "220px"};border-radius:var(--radius)" aria-hidden="true"></div>`;
  },
  gaugeSkeleton(count) {
    count = count || 4;
    return `<div class="gauges">` + Array.from({length: count}, () =>
      `<div class="gcard">${UI.skeleton("96px","96px")}<div>${UI.skeleton("60px","12px",true)}</div></div>`
    ).join("") + `</div>`;
  },

  /* ---- Empty State ---- */
  empty(iconName, title, desc, actionHTML) {
    const ic = iconName ? icon(iconName, 32) : "";
    const act = actionHTML || "";
    return `<div class="empty-state">
      ${ic ? `<div class="empty-icon">${ic}</div>` : ""}
      <div class="empty-title">${esc(title || t("common.none"))}</div>
      ${desc ? `<div class="empty-desc">${esc(desc)}</div>` : ""}
      ${act ? `<div class="empty-action">${act}</div>` : ""}
    </div>`;
  },

  /* ---- Error State ---- */
  errorState(msg, retryLabel, retryFn) {
    const label = retryLabel || (typeof t === "function" ? t("act.refresh") : "Retry");
    return `<div class="error-state">
      <div class="error-icon">${icon("alert", 28)}</div>
      <div class="error-title">${esc(msg || "Error")}</div>
      ${retryFn ? `<button class="btn sm error-retry" onclick="${retryFn}">${icon("refresh",14)} ${esc(label)}</button>` : ""}
    </div>`;
  },

  /* ---- Panel wrapper ---- */
  panel(title, bodyHTML, actionsHTML, opts) {
    opts = opts || {};
    const cls = opts.cls || "";
    const hd = title ? `<div class="hd"><h2>${esc(title)}</h2>${actionsHTML ? `<span class="sp"></span>${actionsHTML}` : ""}</div>` : "";
    return `<div class="panel ${cls}">${hd}<div class="bd"${opts.pad0 ? ' style="padding:0"' : ""}>${bodyHTML}</div></div>`;
  },

  /* ---- Badge ---- */
  badge(text, variant) {
    return `<span class="chip ${variant || ""}">${esc(text)}</span>`;
  },

  /* ---- Progress bar ---- */
  progress(value, max, label) {
    const pct = max > 0 ? Math.round(Math.min(100, Math.max(0, (value / max) * 100))) : 0;
    return `<div class="ratio"><div class="bar"><i style="width:${pct}%;background:linear-gradient(90deg,var(--acc),var(--acc2))"></i></div>${label != null ? `<span class="mono">${esc(String(label))}</span>` : ""}</div>`;
  },

  /* ---- Confirm dialog (returns Promise) ---- */
  confirm(title, msg, okLabel, cancelLabel, danger) {
    const en = (typeof LANG !== "undefined" && LANG === "en");
    const ok = okLabel || (en ? "Confirm" : "确认");
    const cancel = cancelLabel || (en ? "Cancel" : "取消");
    const dng = danger ? "danger" : "";
    return new Promise((resolve) => {
      const { ov, close } = openModal(`
        <div style="margin-bottom:8px"><b style="font-size:15px">${esc(title)}</b></div>
        ${msg ? `<p class="muted" style="margin:0 0 16px;font-size:13px">${esc(msg)}</p>` : ""}
        <div class="row" style="justify-content:flex-end;gap:8px">
          <button class="btn" id="cpCfCancel">${esc(cancel)}</button>
          <button class="btn pri ${dng}" id="cpCfOk">${esc(ok)}</button></div>`);
      ov.querySelector("#cpCfCancel").onclick = () => { close(); resolve(false); };
      ov.querySelector("#cpCfOk").onclick = () => { close(); resolve(true); };
    });
  },

  /* ---- Loading spinner ---- */
  spinner(size) {
    size = size || 20;
    return `<svg class="cp-spinner" width="${size}" height="${size}" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round"><path d="M12 2a10 10 0 1 0 10 10"/></svg>`;
  },

  /* ---- Button with loading state ---- */
  btnSpinner() {
    return `<svg class="cp-spinner" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="3" stroke-linecap="round" style="vertical-align:-2px"><path d="M12 2a10 10 0 1 0 10 10"/></svg>`;
  },

  /* ---- Network offline banner ---- */
  offlineBanner() {
    const en = (typeof LANG !== "undefined" && LANG === "en");
    return `<div class="offline-banner" id="offlineBanner" style="display:none">${icon("alert",14)} ${en ? "Network disconnected — reconnecting..." : "网络已断开 — 重连中…"}</div>`;
  },

  /* ---- Staggered entrance: wrap a list of HTML strings so each child gets a
          sequentially delayed fade-up animation ---- */
  stagger(items, baseDelay) {
    baseDelay = baseDelay || 0.05;
    // Insert a style tag once (idempotent via a marker class on the first item's
    // wrapper). The items are wrapped in .stagger-item divs.
    const styleId = "cpStaggerStyle";
    let style = "";
    if (!document.getElementById(styleId)) {
      const s = document.createElement("style");
      s.id = styleId;
      const delays = Array.from({length: 20}, (_, i) =>
        `.stagger-item:nth-child(${i+1}){animation-delay:${(baseDelay*i).toFixed(2)}s}`
      ).join("");
      s.textContent = `.stagger-item{animation:fadeUp .4s ease-out both}${delays}`;
      document.head.appendChild(s);
    }
    return items.map((html) => `<div class="stagger-item">${html}</div>`).join("");
  },
};

/* ---- Command Palette (global) ---- */
let _cmdPalette = null;
function openCmdPalette() {
  if (_cmdPalette) { _cmdPalette.close(); _cmdPalette = null; return; }
  // Collect all navigable views
  const allNav = (typeof NAV_ADMIN !== "undefined" ? NAV_ADMIN : [])
    .concat(typeof NAV_USER !== "undefined" ? NAV_USER : []);
  const groups = typeof NAV_ADMIN_GROUPS !== "undefined" ? NAV_ADMIN_GROUPS : [];
  const en = (typeof LANG !== "undefined" && LANG === "en");

  let itemsHTML = "";
  if (groups.length) {
    groups.forEach((g) => {
      itemsHTML += `<div class="cp-cmd-group">${esc(typeof t === "function" ? t(g.key) : g.key)}</div>`;
      g.items.forEach((item) => {
        itemsHTML += `<button class="cp-cmd-item" data-v="${esc(item.v)}">${icon(item.ic, 14)} ${esc(typeof t === "function" ? t(item.key) : item.key)}</button>`;
      });
    });
  } else {
    allNav.forEach((item) => {
      itemsHTML += `<button class="cp-cmd-item" data-v="${esc(item.v)}">${icon(item.ic,14)} ${esc(typeof t === "function" ? t(item.key) : item.key)}</button>`;
    });
  }

  const html = `<div class="cp-cmd-mask"></div><div class="cp-cmd-palette">
    <div class="cp-cmd-input-wrap"><span class="cp-cmd-icon">${icon("search",16) || "⌘"}</span>
    <input class="cp-cmd-input" id="cpCmdInput" placeholder="${en ? "Search pages…" : "搜索页面…"}" autofocus></div>
    <div class="cp-cmd-list" id="cpCmdList">${itemsHTML}</div>
    <div class="cp-cmd-footer"><span><kbd>↑↓</kbd> ${en ? "navigate" : "导航"}</span><span><kbd>Enter</kbd> ${en ? "open" : "打开"}</span><span><kbd>Esc</kbd> ${en ? "close" : "关闭"}</span></div>
  </div>`;

  const ov = document.createElement("div");
  ov.className = "cp-cmd-overlay";
  ov.innerHTML = html;
  document.body.appendChild(ov);

  const input = ov.querySelector("#cpCmdInput");
  const list = ov.querySelector("#cpCmdList");
  const mask = ov.querySelector(".cp-cmd-mask");

  const filter = () => {
    const q = (input.value || "").toLowerCase();
    ov.querySelectorAll(".cp-cmd-item").forEach((el) => {
      el.classList.toggle("hide", q ? !el.textContent.toLowerCase().includes(q) : false);
    });
    ov.querySelectorAll(".cp-cmd-group").forEach((el) => {
      const hasVisible = [...el.nextElementSibling ? [el.nextElementSibling] : []].some(() => true);
      let sibling = el.nextElementSibling;
      let anyVisible = false;
      while (sibling && !sibling.classList.contains("cp-cmd-group")) {
        if (!sibling.classList.contains("hide")) { anyVisible = true; break; }
        sibling = sibling.nextElementSibling;
      }
      el.classList.toggle("hide", !anyVisible && q);
    });
  };

  input.oninput = filter;
  input.onkeydown = (e) => {
    if (e.key === "Escape") { close(); return; }
    if (e.key === "Enter") {
      const first = ov.querySelector(".cp-cmd-item:not(.hide)");
      if (first) { first.click(); }
    }
    if (e.key === "ArrowDown" || e.key === "ArrowUp") {
      e.preventDefault();
      const items = [...ov.querySelectorAll(".cp-cmd-item:not(.hide)")];
      const cur = ov.querySelector(".cp-cmd-item.focus");
      let idx = cur ? items.indexOf(cur) : -1;
      if (cur) cur.classList.remove("focus");
      idx = e.key === "ArrowDown" ? (idx + 1) % items.length : (idx - 1 + items.length) % items.length;
      if (items[idx]) items[idx].classList.add("focus");
    }
  };

  ov.querySelectorAll(".cp-cmd-item").forEach((btn) => {
    btn.onclick = () => {
      const v = btn.dataset.v;
      close();
      if (typeof setView === "function" && v) setView(v);
    };
  });

  const close = () => { ov.remove(); document.removeEventListener("keydown", onKey); _cmdPalette = null; };
  const onKey = (e) => { if (e.key === "Escape") close(); };
  // Override the global Escape handler while palette is open
  document.addEventListener("keydown", onKey);
  mask.onclick = close;
  _cmdPalette = { close };

  setTimeout(() => input.focus(), 50);
}

/* ---- Global keyboard shortcuts ---- */
function initKeyboardShortcuts() {
  document.addEventListener("keydown", (e) => {
    // Cmd/Ctrl+K → command palette
    if ((e.metaKey || e.ctrlKey) && e.key === "k") {
      e.preventDefault();
      openCmdPalette();
    }
    // Cmd/Ctrl+/ → shortcut help
    if ((e.metaKey || e.ctrlKey) && e.key === "/") {
      e.preventDefault();
      showShortcutHelp();
    }
  });
}

function showShortcutHelp() {
  const en = (typeof LANG !== "undefined" && LANG === "en");
  const shortcuts = [
    ["⌘/Ctrl + K", en ? "Command palette" : "命令面板"],
    ["⌘/Ctrl + /", en ? "Shortcut help" : "快捷键帮助"],
    ["Esc", en ? "Close modal / drawer" : "关闭弹窗 / 抽屉"],
  ];
  const { ov, close } = openModal(`
    <div style="margin-bottom:12px"><b style="font-size:15px">${en ? "Keyboard shortcuts" : "键盘快捷键"}</b></div>
    <table><thead><tr><th>${en ? "Key" : "按键"}</th><th>${en ? "Action" : "操作"}</th></tr></thead><tbody>
      ${shortcuts.map((s) => `<tr><td><kbd class="kbd-key">${esc(s[0])}</kbd></td><td>${esc(s[1])}</td></tr>`).join("")}
    </tbody></table>
    <div class="row" style="justify-content:flex-end;margin-top:12px"><button class="btn" id="cpShClose">${en ? "Close" : "关闭"}</button></div>`);
  ov.querySelector("#cpShClose").onclick = close;
}

/* ---- Sub-navigation: recent pages ---- */
const RECENT_KEY = "cp_recent_views";
function recordRecentView(v) {
  if (!v) return;
  let recent = [];
  try { recent = JSON.parse(localStorage.getItem(RECENT_KEY) || "[]"); } catch { recent = []; }
  recent = recent.filter((x) => x !== v);
  recent.unshift(v);
  if (recent.length > 4) recent.pop();
  localStorage.setItem(RECENT_KEY, JSON.stringify(recent));
}

/* ---- Auto-scroll nav to active item ---- */
function scrollNavToActive() {
  const active = document.querySelector("#nav button.on");
  if (active) active.scrollIntoView({ block: "nearest", behavior: "smooth" });
}

/* ---- Initialize on DOM ready ---- */
function initComponents() {
  initKeyboardShortcuts();
  // Inject offline banner
  const banner = UI.offlineBanner();
  const shell = document.getElementById("appShell");
  if (shell) shell.insertAdjacentHTML("beforebegin", banner);
  // Network status listeners
  window.addEventListener("offline", () => {
    const b = document.getElementById("offlineBanner");
    if (b) b.style.display = "block";
  });
  window.addEventListener("online", () => {
    const b = document.getElementById("offlineBanner");
    if (b) b.style.display = "none";
    if (typeof toast === "function") {
      const en = (typeof LANG !== "undefined" && LANG === "en");
      toast(en ? "Back online" : "网络已恢复", "ok");
    }
  });
}
