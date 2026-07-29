/* adminapp.js — the ADMIN console shell (admin.html, served at /admin.html). Owns the
   admin nav groups, header, router and an admin-only auth gate (a signed-in non-admin is
   bounced to an access-denied card with a link back to the user portal). Page renderers
   live in admin.js; shared primitives (API client, theme, auth card, charts) in core.js. */

const NAV_ADMIN_GROUPS = [
  { key: "grp.admin_monitor", items: [
    { v: "overview", ic: "layout", key: "nav.overview" },
    { v: "usage", ic: "chart", key: "nav.usage" },
    { v: "reg_dashboard", ic: "trending-up", key: "nav.reg_dashboard" },
  ] },
  { key: "grp.admin_registration", items: [
    { v: "registration", ic: "plus", key: "nav.registration" },
    { v: "reg_providers", ic: "package", key: "nav.reg_providers" },
    { v: "automation", ic: "clock", key: "nav.automation" },
  ] },
  { key: "grp.admin_upstream", items: [
    { v: "accounts", ic: "server", key: "nav.accounts" },
    { v: "providers", ic: "layers", key: "nav.providers" },
    { v: "egress", ic: "shuffle", key: "nav.egress" },
    { v: "lifecycle", ic: "refresh", key: "nav.lifecycle" },
  ] },
  { key: "grp.admin_security", items: [
    { v: "isolation", ic: "shield", key: "nav.isolation" },
    { v: "moderation", ic: "filter", key: "nav.moderation" },
    { v: "thinking", ic: "zap", key: "nav.thinking" },
    { v: "cf", ic: "alert", key: "nav.cf" },
    { v: "audit", ic: "clipboard", key: "nav.audit" },
  ] },
  { key: "grp.admin_business", items: [
    { v: "users", ic: "users", key: "nav.users" },
    { v: "keys", ic: "key", key: "nav.keys" },
    { v: "groups", ic: "folder", key: "nav.groups" },
    { v: "org", ic: "briefcase", key: "nav.org" },
  ] },
  { key: "grp.admin_system", items: [
    { v: "settings", ic: "gear", key: "nav.settings" },
  ] },
];
const NAV_ADMIN = NAV_ADMIN_GROUPS.flatMap((g) => g.items);
function navKey(v) { const f = NAV_ADMIN.find((x) => x.v === v); return f ? f.key : ""; }

/* ===== admin-only auth gate ===== */
async function boot() {
  applyTheme();
  if (typeof initAppearance === "function") initAppearance();
  injectChartDefs();
  if (typeof initComponents === "function") initComponents();
  await fetchMe();
  if (ME && ME.authed) { if (isAdmin()) showApp(); else showDeny(); }
  else showAuth(false); // admin console never offers self-registration
}
function showAuth(allowReg) {
  $("#appShell").classList.add("hide"); $("#denyView").classList.add("hide");
  $("#authView").classList.remove("hide");
  renderAuth($("#authView"), "login", allowReg, () => { if (isAdmin()) showApp(); else showDeny(); });
}
function showDeny() {
  $("#appShell").classList.add("hide"); $("#authView").classList.add("hide");
  $("#denyView").classList.remove("hide");
  applyI18n(document);
}
function showApp() {
  $("#authView").classList.add("hide"); $("#denyView").classList.add("hide");
  $("#appShell").classList.remove("hide");
  renderShell();
  ping(); refreshModels();
  setView(viewFromHash() || "overview");
  setInterval(ping, 15000);
  setInterval(tickUntil, 1000);
}

/* ===== shell (sidebar groups + header) ===== */
function rerender() { renderShell(); if (VIEW) loadView(VIEW); }
var _navCollapsed = JSON.parse(localStorage.getItem("cp_nav_collapsed") || "{}");
function toggleNavGroup(key) { _navCollapsed[key] = !_navCollapsed[key]; localStorage.setItem("cp_nav_collapsed", JSON.stringify(_navCollapsed)); renderShell(); }
function navBtn(item) {
  return `<button data-v="${item.v}" class="${item.v === VIEW ? "on" : ""}"><span class="ic">${icon(item.ic)}</span><span class="lbl">${esc(t(item.key))}</span></button>`;
}
function renderShell() {
  var html = "";
  for (var g of NAV_ADMIN_GROUPS) {
    var collapsed = !!_navCollapsed[g.key];
    var active = g.items.some(function(x) { return x.v === VIEW; });
    var cnt = g.items.length;
    html += '<div class="navgroup-hd' + (active ? ' active' : '') + '" onclick="toggleNavGroup(\'' + g.key + '\')">' +
      '<span class="ngh-t">' + esc(t(g.key)) + '</span>' +
      '<span class="ngh-meta">' + cnt + '</span>' +
      '<span class="ngh-arr">' + (collapsed ? '▸' : '▾') + '</span>' +
      '</div>';
    if (!collapsed) html += '<div class="navgroup-bd">' + g.items.map(navBtn).join("") + '</div>';
  }
  $("#nav").innerHTML = html;
  $$("#nav button").forEach(function(b) { b.onclick = function() { setView(b.dataset.v); }; });
  $$("#langSeg button").forEach(function(b) { b.classList.toggle("on", b.textContent.trim() === (LANG === "en" ? "EN" : "中")); });
  var name = (ME && (ME.name || ME.email)) || "Admin";
  var initial = (String(name)[0] || "?").toUpperCase();
  $("#userMenu").innerHTML = '<button class="ubtn" id="userBtn"><span class="avatar">' + esc(initial) + '</span><span class="mono" style="max-width:120px;overflow:hidden;text-overflow:ellipsis">' + esc(name) + '</span><span>▾</span></button>' +
    '<div class="menu" id="userMenuDrop">' +
      '<a href="/"><span class="ic">' + icon("layout") + '</span>' + esc(t("nav.portal")) + '</a>' +
      '<div class="sep"></div>' +
      '<button onclick="doLogout()"><span class="ic">' + icon("logout") + '</span>' + esc(t("act.logout")) + '</button>' +
    '</div>';
  $("#userBtn").onclick = function(e) { e.stopPropagation(); $("#userMenu").classList.toggle("open"); };
  var rb = $("#refreshBtn"); if (rb) rb.innerHTML = icon("refresh");
  applyTheme();
  applyI18n(document);
}

/* ===== router ===== */
// Views are reflected in location.hash (#/accounts) so the browser back/forward
// buttons work and a specific view is a shareable link. The hashchange listener
// only re-routes when the hash names a DIFFERENT view than the current one, so the
// hash we write inside setView never recurses back into setView.
function isValidView(v) { return !!v && NAV_ADMIN.some((x) => x.v === v); }
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
    overview: typeof loadDash === "function" ? loadDash : null,
    accounts: typeof loadAccounts === "function" ? loadAccounts : null,
    providers: typeof loadProvidersPage === "function" ? loadProvidersPage : null,
    egress: typeof loadEgress === "function" ? loadEgress : null,
    usage: typeof loadUsage === "function" ? loadUsage : null,
    isolation: typeof loadIsolation === "function" ? loadIsolation : null,
    groups: typeof loadGroups === "function" ? loadGroups : null,
    keys: typeof loadKeys === "function" ? loadKeys : null,
    users: typeof loadUsers === "function" ? loadUsers : null,
    moderation: typeof loadModeration === "function" ? loadModeration : null,
    cf: typeof loadCF === "function" ? loadCF : null,
    audit: typeof loadAudit === "function" ? loadAudit : null,
    org: typeof loadOrg === "function" ? loadOrg : null,
    settings: typeof loadSettings === "function" ? loadSettings : null,
    sysconfig: typeof loadSystemConfig === "function" ? loadSystemConfig : null,
    thinking: () => { _settingTab = "thinking"; loadSettings(); },
    sysconfig: () => { _settingTab = "system"; loadSettings(); },
    registration: () => loadEmbedded("registrationFrame", "/registration.html"),
    reg_providers: () => loadEmbedded("regProvidersFrame", "/providers-config.html"),
    reg_dashboard: () => loadEmbedded("regDashboardFrame", "/dashboard.html"),
    automation: () => loadEmbedded("automationFrame", "/automation.html"),
    lifecycle: () => loadEmbedded("lifecycleFrame", "/lifecycle-tasks.html"),
    proxies: () => loadEmbedded("proxiesFrame", "/proxy-configs.html"),
  };
  const fn = map[v];
  try { if (fn) fn(); } catch (e) { toast(e.message, "bad"); }
}
// Standalone tool pages render inside the shell via a lazy iframe — src is set on first
// visit only. On load we push the console's theme + design axes onto the iframe so an
// embedded page never renders in a different theme than the console around it.
function loadEmbedded(frameId, src) {
  const f = document.getElementById(frameId);
  if (f && !f.getAttribute("src")) {
    f.addEventListener("load", () => {
      try { applyFrameChrome(f.contentDocument.documentElement); } catch {}
    });
    f.setAttribute("src", src);
  }
}
function refreshAll() { ping(); refreshModels(); if (VIEW) loadView(VIEW); }

window.addEventListener("DOMContentLoaded", boot);
