/* portal.js — the USER portal shell (index.html, served at /). Owns the user-facing
   nav, header, hash router and auth gate. Page renderers live in user.js. Anything
   shared with the admin console (API client, theme, auth card, helpers) is in core.js.
   An admin who signs in here still lands in the user portal, but gets an "Admin console"
   entry in the header menu that links to /admin.html. */

const NAV_USER = [
  { v: "dashboard", ic: "layout", key: "nav.dashboard" },
  { v: "mykeys", ic: "key", key: "nav.mykeys" },
  { v: "myusage", ic: "chart", key: "nav.myusage" },
  { v: "models", ic: "box", key: "nav.models" },
  { v: "mysettings", ic: "gear", key: "nav.mysettings" },
];
function navKey(v) { const f = NAV_USER.find((x) => x.v === v); return f ? f.key : ""; }

/* ===== auth gate ===== */
async function boot() {
  applyTheme();
  if (typeof initAppearance === "function") initAppearance();
  injectChartDefs();
  if (typeof initComponents === "function") initComponents();
  await fetchMe();
  if (ME && ME.authed) showApp();
  else showAuth(ME && ME.allow_registration !== false);
}
function showAuth(allowReg) {
  $("#appShell").classList.add("hide");
  $("#authView").classList.remove("hide");
  renderAuth($("#authView"), "login", allowReg, showApp);
}
function showApp() {
  $("#authView").classList.add("hide");
  $("#appShell").classList.remove("hide");
  renderShell();
  ping(); refreshModels();
  setView(viewFromHash() || "dashboard");
  setInterval(ping, 15000);
  setInterval(tickUntil, 1000);
}

/* ===== shell (sidebar + header) ===== */
function rerender() { renderShell(); if (VIEW) loadView(VIEW); }
function navBtn(item) {
  return `<button data-v="${item.v}" class="${item.v === VIEW ? "on" : ""}"><span class="ic">${icon(item.ic)}</span><span class="lbl">${esc(t(item.key))}</span></button>`;
}
function renderShell() {
  $("#nav").innerHTML = `<div class="navgroup-t">${esc(t("grp.user"))}</div>` + NAV_USER.map(navBtn).join("");
  $$("#nav button").forEach((b) => (b.onclick = () => setView(b.dataset.v)));
  $$("#langSeg button").forEach((b) => b.classList.toggle("on", b.textContent.trim() === (LANG === "en" ? "EN" : "中")));
  const name = (ME && (ME.name || ME.email)) || (LANG === "en" ? "Account" : "账户");
  const initial = (String(name)[0] || "?").toUpperCase();
  const adminLink = isAdmin()
    ? `<a href="/admin.html"><span class="ic">${icon("shield")}</span>${esc(t("nav.admin_console"))}</a><div class="sep"></div>`
    : "";
  $("#userMenu").innerHTML = `<button class="ubtn" id="userBtn"><span class="avatar">${esc(initial)}</span><span class="mono" style="max-width:120px;overflow:hidden;text-overflow:ellipsis">${esc(name)}</span><span>▾</span></button>
    <div class="menu" id="userMenuDrop">
      <button onclick="setView('mysettings')"><span class="ic">${icon("gear")}</span>${esc(t("nav.mysettings"))}</button>
      ${adminLink}
      <button onclick="doLogout()"><span class="ic">${icon("logout")}</span>${esc(t("act.logout"))}</button>
    </div>`;
  $("#userBtn").onclick = (e) => { e.stopPropagation(); $("#userMenu").classList.toggle("open"); };
  const rb = $("#refreshBtn"); if (rb) rb.innerHTML = icon("refresh");
  applyTheme();
  applyI18n(document);
}

/* ===== router ===== */
// Views are reflected in location.hash (#/mykeys) so browser back/forward work and
// a view is a shareable link. The hashchange listener only re-routes on a DIFFERENT
// view, so the hash setView writes never recurses back into setView.
function isValidView(v) { return !!v && NAV_USER.some((x) => x.v === v); }
function viewFromHash() {
  const h = (location.hash || "").replace(/^#\/?/, "").trim();
  return isValidView(h) ? h : "";
}
function setView(v) {
  VIEW = v;
  $$("#nav button").forEach((b) => b.classList.toggle("on", b.dataset.v === v));
  $$("section[data-view]").forEach((s) => s.classList.toggle("hide", s.dataset.view !== v));
  const k = navKey(v); $("#title").textContent = k ? t(k) : "";
  if (window.innerWidth <= 760) $("#sidebar").classList.remove("show");
  if (typeof recordRecentView === "function") recordRecentView(v);
  if (typeof scrollNavToActive === "function") setTimeout(scrollNavToActive, 50);
  const target = "#/" + v;
  if (location.hash !== target) location.hash = target;
  loadView(v);
}
window.addEventListener("hashchange", () => {
  const v = viewFromHash();
  if (v && v !== VIEW) setView(v);
});
function loadView(v) {
  const map = {
    dashboard: typeof loadMyDashboard === "function" ? loadMyDashboard : null,
    mykeys: typeof loadMyKeys === "function" ? loadMyKeys : null,
    myusage: typeof loadMyUsage === "function" ? loadMyUsage : null,
    models: typeof loadModelsPage === "function" ? loadModelsPage : null,
    mysettings: typeof loadMySettings === "function" ? loadMySettings : null,
  };
  const fn = map[v];
  try { if (fn) fn(); } catch (e) { toast(e.message, "bad"); }
}
function refreshAll() { ping(); refreshModels(); if (VIEW) loadView(VIEW); }

window.addEventListener("DOMContentLoaded", boot);
