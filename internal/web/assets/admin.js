/* admin.js — admin console pages, ported onto the new shell. Each load* renders into
   its section (data-view) and reuses the shared helpers/charts. The page set matches
   the original console plus a dedicated Providers page; 用户管理 (loadUsers) lists the
   portal users. All talk to the existing /admin/* REST API. */

/* dashboard / usage time range */
let DASH_RANGE = "24h";
function rangeParams() {
  const now = Math.floor(Date.now() / 1000);
  return DASH_RANGE === "7d"
    ? { since: now - 7 * 86400, bucket: 86400, xfmt: (b) => { const d = new Date(b.bucket * 1000); return d.getMonth() + 1 + "/" + d.getDate(); } }
    : { since: now - 24 * 3600, bucket: 3600, xfmt: (b) => { const d = new Date(b.bucket * 1000); return String(d.getHours()).padStart(2, "0") + ":00"; } };
}
function setRange(r) { DASH_RANGE = r; loadView(VIEW); }
function rangeSeg() { return `<div class="seg"><button class="${DASH_RANGE === "24h" ? "on" : ""}" onclick="setRange('24h')">24h</button><button class="${DASH_RANGE === "7d" ? "on" : ""}" onclick="setRange('7d')">7d</button></div>`; }
/* USAGE_SERIES + usageLegend() now live in core.js (shared with the user portal's My Usage). */

function cooldownInfo(a) {
  const now = Math.floor(Date.now() / 1000);
  const cd = a && a.egress_binding ? +a.egress_binding.cooldown_until || 0 : 0;
  const q = +(a && a.quarantine_until) || 0;
  const until = Math.max(cd, q);
  return until > now ? { active: true, secs: until - now, kind: q > now ? "quarantine" : "cooldown" } : { active: false, secs: 0, kind: "" };
}

function isKiroAccount(a) { return !!a && String(a.provider || "").toLowerCase() === "kiro"; }
function isKiroSuspended(a) { return isKiroAccount(a) && String(a.quarantine_reason || "").toLowerCase() === "aws_user_suspended"; }
function kiroHealthConfirmation(accounts) {
  if (!(accounts || []).some(isKiroAccount)) return true;
  return confirm(LANG === "en"
    ? "Kiro health testing performs a real minimal model request after the free authentication check and may consume credits. Continue?"
    : "Kiro 测活会先执行免费认证检查，再发送一次最小真实推理请求，可能消耗 credits。是否继续？");
}

function filterAccTable(el) {
  const container = el.closest(".panel");
  if (!container) return;
  const tbody = container.querySelector("tbody");
  if (!tbody) return;
  const rows = [...tbody.querySelectorAll("tr")];
  const filters = [...container.querySelectorAll(".tf-row input, .tf-row select")];
  rows.forEach((row) => {
    let match = true;
    filters.forEach((f) => {
      const colIdx = parseInt(f.dataset.col);
      if (isNaN(colIdx)) return;
      const cell = row.children[colIdx];
      const cellText = (cell ? cell.textContent : "").toLowerCase();
      const q = (f.value || "").toLowerCase();
      if (q && !cellText.includes(q)) match = false;
    });
    row.style.display = match ? "" : "none";
  });
  // Reset pagination
  const pag = container.querySelector(".pag");
  if (pag) pag.remove();
  const tbl = container.querySelector("table");
  if (tbl) { tbl.dataset.pageable = "0"; makeTablePageable(tbl, 15); }
}

function cooldownChip(a) {
  if (isKiroSuspended(a)) return `<span class="chip bad" title="${LANG === "en" ? "Contact AWS Support, then run the confirmed health test to recover." : "请先联系 AWS 支持，恢复后由管理员确认并执行双层测活。"}">${LANG === "en" ? "AWS User ID suspended" : "AWS User ID 已暂停"}</span>`;
  const c = cooldownInfo(a);
  if (!c.active) return `<span class="chip ok">${LANG === "en" ? "ready" : "就绪"}</span>`;
  const m = Math.floor(c.secs / 60), s = c.secs % 60;
  return `<span class="chip ${c.kind === "quarantine" ? "bad" : "warn"}">${c.kind === "quarantine" ? (LANG === "en" ? "quarantined" : "隔离") : (LANG === "en" ? "cooling" : "限额冷却")} <span class="countdown">${m}:${String(s).padStart(2, "0")}</span></span>`;
}

async function loadDash() {
  // Show skeleton while loading
  $("#overviewView").innerHTML = '<div class="viz kpis">' + Array.from({length:6}, () => UI.kpiSkeleton()).join('') + '</div>' +
    '<div class="viz cols2"><div class="panel"><div class="hd"><h2>' + (LANG === "en" ? "Usage trend" : "用量趋势") + '</h2></div><div class="bd chartwrap">' + UI.chartSkeleton() + '</div></div><div class="panel"><div class="hd"><h2>' + (LANG === "en" ? "Accounts" : "账号构成") + '</h2></div><div class="bd">' + UI.chartSkeleton("100%","200") + '</div></div></div>' +
    '<div class="panel"><div class="hd"><h2>' + (LANG === "en" ? "Account quota" : "账号额度 · 实时配额") + '</h2></div><div class="bd">' + UI.gaugeSkeleton(4) + '</div></div>' +
    '<div class="panel"><div class="hd"><h2>' + (LANG === "en" ? "Account health" : "账号健康") + '</h2></div><div class="bd" style="padding:0">' + UI.tableSkeleton(6, 4) + '</div></div>';
  const rp = rangeParams();
  const [accs, egs, cf, usage, settings, quota, ts] = await Promise.all([
    api("/admin/accounts").catch(() => []), api("/admin/egress-profiles").catch(() => []),
    api("/admin/cf-events?limit=100").catch(() => []), api("/admin/usage").catch(() => []),
    api("/admin/settings").catch(() => ({})), api("/admin/quota").catch(() => []),
    api(`/admin/usage/timeseries?since=${rp.since}&bucket=${rp.bucket}`).catch(() => ({ buckets: [] })),
  ]);
  ACCTS = accs || []; EGRESS = egs || []; SETTINGS = settings || {};
  const cfList = cf || [];
  QUOTA = {}; (quota || []).forEach((q) => (QUOTA[q.account_id] = q));
  const buckets = (ts && ts.buckets) || [];
  const claude = ACCTS.filter((a) => a.provider === "claude").length;
  const custom = ACCTS.filter((a) => a.provider && a.provider !== "claude" && a.provider !== "codex").length;
  const codex = ACCTS.length - claude - custom;
  const active = ACCTS.filter((a) => a.status === "active").length;
  const abnormal = ACCTS.filter((a) => a.status !== "active").length;
  const cooling = ACCTS.filter((a) => cooldownInfo(a).active).length;
  const activeReady = ACCTS.filter((a) => a.status === "active" && !cooldownInfo(a).active).length;
  const activeCooling = ACCTS.filter((a) => a.status === "active" && cooldownInfo(a).active).length;
  const totalTokens = (usage || []).reduce((s, u) => s + (+u.total_tokens || 0), 0);
  const cached = (usage || []).reduce((s, u) => s + (+u.cached_tokens || 0), 0);
  const promptT = (usage || []).reduce((s, u) => s + (+u.prompt_tokens || 0), 0);
  const totalReq = (usage || []).reduce((s, u) => s + (+u.requests || 0), 0);
  const ratio = cached + promptT > 0 ? cached / (cached + promptT) : 0;
  const periodTok = buckets.reduce((s, b) => s + (+b.total_tokens || 0), 0);
  const sparkTot = buckets.map((b) => +b.total_tokens || 0), sparkReq = buckets.map((b) => +b.requests || 0);
  const rangeLbl = DASH_RANGE === "7d" ? (LANG === "en" ? "7d" : "近7天") : (LANG === "en" ? "24h" : "近24h");
  const kpis =
    kpiCard({ k: LANG === "en" ? "Accounts" : "账号总数", ic: "users", v: ACCTS.length, sub: (LANG === "en" ? "active " : "活跃 ") + active, accent: true }) +
    kpiCard({ k: "Token", ic: "chart", v: fmt(totalTokens), sub: LANG === "en" ? "total" : "累计", spark: sparkTot, grad: "gradAcc", stroke: "var(--acc)", delta: `${rangeLbl} ${fmt(periodTok)}` }) +
    kpiCard({ k: LANG === "en" ? "Requests" : "请求数", ic: "activity", v: fmt(totalReq), sub: LANG === "en" ? "total" : "累计", spark: sparkReq, grad: "gradVio", stroke: "var(--acc2)" }) +
    kpiCard({ k: LANG === "en" ? "Cache hit" : "缓存命中", ic: "zap", v: pct(ratio), sub: LANG === "en" ? "saves quota" : "省额度", deltaCls: ratio >= 0.5 ? "up" : "" }) +
    kpiCard({ k: LANG === "en" ? "Cooling" : "限额冷却", ic: "clock", v: cooling, sub: cooling ? (LANG === "en" ? "accounts" : "账号在冷却") : "—", deltaCls: cooling ? "down" : "up" }) +
    kpiCard({ k: "CF", ic: "alert", v: cfList.length, sub: LANG === "en" ? "last 100" : "近100", deltaCls: cfList.length ? "down" : "up" });
  const trend = `<div class="panel"><div class="hd"><h2>${LANG === "en" ? "Usage trend" : "用量趋势"}</h2><span class="sp"></span>${usageLegend()}<span style="margin-left:10px">${rangeSeg()}</span></div>
    <div class="bd chartwrap">${Charts.stackArea(buckets, USAGE_SERIES, { xfmt: rp.xfmt, empty: LANG === "en" ? "No usage yet" : "暂无用量数据" })}</div></div>`;
  const donuts = `<div class="panel"><div class="hd"><h2>${LANG === "en" ? "Accounts" : "账号构成"}</h2></div>
    <div class="bd" style="display:flex;gap:16px;flex-wrap:wrap;justify-content:space-around;align-items:center">
      <div style="text-align:center">${Charts.donut([{ label: "Codex", value: codex, color: "#5b8cff" }, { label: "Claude", value: claude, color: "#d39a6a" }, { label: "Custom", value: custom, color: "#34d399" }], { center: ACCTS.length, sub: LANG === "en" ? "accounts" : "账号" })}
        <div class="legend" style="justify-content:center;margin-top:6px"><span><i style="background:#5b8cff"></i>Codex ${codex}</span><span><i style="background:#d39a6a"></i>Claude ${claude}</span><span><i style="background:#34d399"></i>Custom ${custom}</span></div></div>
      <div style="text-align:center">${Charts.donut([{ label: "ready", value: activeReady, color: "#34d399" }, { label: "cool", value: activeCooling, color: "#fbbf24" }, { label: "bad", value: abnormal, color: "#f87171" }], { center: active, sub: LANG === "en" ? "active" : "活跃" })}
        <div class="legend" style="justify-content:center;margin-top:6px"><span><i style="background:#34d399"></i>${activeReady}</span><span><i style="background:#fbbf24"></i>${activeCooling}</span><span><i style="background:#f87171"></i>${abnormal}</span></div></div>
    </div></div>`;
  const quotaPanel = `<div class="panel"><div class="hd"><h2>${LANG === "en" ? "Account quota" : "账号额度 · 实时配额"}</h2></div><div class="bd">${quotaGaugesHTML()}</div></div>`;
  const labels = {}; ACCTS.forEach((a) => (labels[a.id] = a.label || a.email || a.id));
  const topAcc = (usage || []).slice().sort((a, b) => (+b.total_tokens || 0) - (+a.total_tokens || 0)).slice(0, 8)
    .map((u) => ({ label: labels[u.account_id] || u.account_id, value: +u.total_tokens || 0, text: fmt(u.total_tokens) }));
  const topPanel = `<div class="panel"><div class="hd"><h2>${LANG === "en" ? "Top accounts" : "账号用量 Top"}</h2></div><div class="bd">${Charts.rank(topAcc)}</div></div>`;
  const egItems = (egs || []).map((e) => ({ label: (e.name || e.id) + (e.region ? " · " + e.region : ""), value: +e.latency_millis || 0, text: e.latency_millis ? e.latency_millis + "ms" : e.health || "-", color: e.health === "healthy" ? "linear-gradient(90deg,#34d399,#5b8cff)" : e.health === "tripped" ? "#f87171" : "#fbbf24" })).sort((a, b) => a.value - b.value).slice(0, 8);
  const egPanel = `<div class="panel"><div class="hd"><h2>${LANG === "en" ? "Egress latency" : "出口延迟 / 健康"}</h2></div><div class="bd">${(egs || []).length ? Charts.rank(egItems) : '<div class="empty">—</div>'}</div></div>`;
  $("#overviewView").innerHTML =
    `<div class="viz kpis">${kpis}</div>` +
    `<div class="viz cols2">${trend}${donuts}</div>` + quotaPanel +
    `<div class="viz cols2">${topPanel}${egPanel}</div>` +
    `<div class="panel"><div class="hd"><h2>${LANG === "en" ? "Account health" : "账号健康"}</h2></div><div class="bd" style="padding:0">${accTableHTML(ACCTS, true)}</div></div>`;
  tickUntil();
  setTimeout(() => {
    enhanceAllTables($("#overviewView"));
    const tbl = $("#overviewView table");
    if (tbl) makeTablePageable(tbl, 12);
  }, 10);
}
function quotaGaugesHTML() {
  const labels = {}, provOf = {}; ACCTS.forEach((a) => { labels[a.id] = a.label || a.email || a.id; provOf[a.id] = a.provider; });
  const items = Object.values(QUOTA).filter((q) => q.used_percent >= 0 || q.remaining_tokens >= 0);
  if (!items.length) return `<div class="empty">${LANG === "en" ? "No quota data yet — captured from upstream rate-limit headers." : "暂无额度数据 — 账号产生上游响应后自动采集。"}</div>`;
  items.sort((a, b) => (b.used_percent || 0) - (a.used_percent || 0));
  return '<div class="gauges">' + items.map((q) => {
    const nm = labels[q.account_id] || q.label || q.account_id;
    const remain = q.remaining_tokens >= 0 ? "余 " + fmt(q.remaining_tokens) + (q.limit_tokens > 0 ? " / " + fmt(q.limit_tokens) : "") : "";
    const d7 = q.secondary_7d_used_pct;
    const has7d = d7 >= 0;
    const sub = (q.source || "") + (has7d ? " · 7d " + Math.round(d7) + "%" : "");
    return `<div class="gcard">${Charts.ring(q.used_percent, { color: quotaColor(q.used_percent), sub: sub })}
      <div class="nm" title="${esc(nm)}">${esc(nm)} ${provChip(provOf[q.account_id] || q.provider)}</div>
      <div class="meta">${remain ? esc(remain) : '<span class="muted">—</span>'}${q.reset_at ? `<br><span data-until="${q.reset_at}" data-pre=""></span>` : ""}</div>${statusToneChip(q.status)}</div>`;
  }).join("") + "</div>";
}

function accTableHTML(accs, compact) {
  if (!accs.length) return `<div class="empty">${LANG === "en" ? "No accounts — click Import." : "暂无账号，点击「导入账号」添加。"}</div>`;
  // Filter row
  const filterRow = `<div class="tf-row">
    <input type="text" placeholder="${LANG === "en" ? "Filter label/email/id…" : "筛选标签/邮箱/ID…"}" data-col="0" oninput="filterAccTable(this)">
    <select data-col="2" onchange="filterAccTable(this)"><option value="">${LANG === "en" ? "All status" : "全部状态"}</option><option value="active">active</option><option value="disabled">disabled</option><option value="quarantined">quarantined</option></select>
    <input type="text" placeholder="${LANG === "en" ? "Filter model…" : "筛选模型…"}" data-col="4" oninput="filterAccTable(this)">
  </div>`;
  const rows = accs.map((a) => {
    const caps = (a.capabilities || []).map((c) => c.model_slug).slice(0, 3).join(", ");
    const eg = a.egress_binding ? a.egress_binding.primary_egress_id : "-";
    const actions = compact ? "" : `
      <button class="btn sm" onclick="event.stopPropagation();act('${a.id}','probe-models','POST')">${LANG === "en" ? "Probe" : "探测"}</button>
      <button class="btn sm" onclick="event.stopPropagation();act('${a.id}','refresh','POST')">${LANG === "en" ? "Refresh" : "刷新"}</button>
      ${a.status === "active" ? `<button class="btn sm" onclick="event.stopPropagation();act('${a.id}','disable','POST')">${LANG === "en" ? "Disable" : "禁用"}</button>` : `<button class="btn sm" onclick="event.stopPropagation();act('${a.id}','enable','POST')">${LANG === "en" ? "Enable" : "启用"}</button>`}
      <button class="btn sm danger" onclick="event.stopPropagation();delAcc('${a.id}')">${LANG === "en" ? "Delete" : "删除"}</button>`;
    return `<tr class="clk" onclick="openAcc('${a.id}')">
      <td><div>${esc(a.label || "-")} ${provChip(a.provider)}</div><div class="mono muted">${esc(a.id)}</div></td>
      <td>${esc(a.email || "-")}<div class="muted">${esc(a.plan_type || "")}</div></td>
      <td>${isKiroSuspended(a) ? `<span class="chip bad">${LANG === "en" ? "AWS User ID suspended" : "AWS User ID 已暂停"}</span>` : statusChip(a.status)}${a.is_fedramp ? ' <span class="chip">fedramp</span>' : ""}</td>
      <td>${quotaMiniBar(QUOTA[a.id])}</td>
      <td class="mono">${esc(caps || "—")}</td>
      <td class="mono">${esc(eg)}</td>
      ${compact ? "" : `<td><div class="row">${actions}</div></td>`}
    </tr>`;
  }).join("");
  const H = LANG === "en"
    ? ["Account", "Email / Plan", "Status", "Quota", "Models", "Egress", "Actions"]
    : ["账号", "邮箱 / 套餐", "状态", "额度", "模型", "出口", "操作"];
  return `<table><thead><tr><th>${H[0]}</th><th>${H[1]}</th><th>${H[2]}</th><th class="no-sort">${H[3]}</th><th>${H[4]}</th><th>${H[5]}</th>${compact ? "" : `<th class="no-sort">${H[6]}</th>`}</tr></thead><tbody>${rows}</tbody></table>`;
}
async function loadAccounts() {
  $("#accView").innerHTML = '<div class="panel"><div class="hd"><h2>' + t("nav.accounts") + '</h2></div><div class="bd" style="padding:0">' + UI.tableSkeleton(6, 5) + '</div></div>';
  const accs = (await api("/admin/accounts")) || []; ACCTS = accs;
  EGRESS = (await api("/admin/egress-profiles").catch(() => [])) || [];
  const quota = (await api("/admin/quota").catch(() => [])) || []; QUOTA = {}; quota.forEach((q) => (QUOTA[q.account_id] = q));
  $("#accView").innerHTML = `<div class="panel"><div class="hd"><h2>${t("nav.accounts")}</h2><span class="sp"></span><button class="btn pri" onclick="openImport()">${icon("plus")} ${LANG === "en" ? "Import account" : "导入账号"}</button></div><div class="bd" style="padding:0" id="accTable"></div></div>`;
  $("#accTable").innerHTML = accTableHTML(accs, false);
  tickUntil();
  setTimeout(() => {
    // Enhance THIS page's table (#accView), not the overview's — the previous code
    // copy-pasted #overviewView here, so the accounts table got no sort/resize/sticky
    // /pagination at all while the overview table was re-enhanced behind the scenes.
    enhanceAllTables($("#accView"));
    const tbl = $("#accView table");
    if (tbl) makeTablePageable(tbl, 15);
  }, 10);
}
async function act(id, action, method) { try { await api(`/admin/accounts/${id}/${action}`, { method }); toast(action + " ✓", "ok"); loadAccounts(); } catch (e) { toast(e.message, "bad"); } }
async function delAcc(id) { if (!confirm((LANG === "en" ? "Delete account " : "删除账号 ") + id + " ?")) return; try { await api(`/admin/accounts/${id}/delete`, { method: "DELETE" }); toast(t("ok.deleted"), "ok"); closeDrawer(); loadAccounts(); } catch (e) { toast(e.message, "bad"); } }
async function clearQuarantine(id) {
  const account = (ACCTS || []).find((a) => a.id === id);
  if (isKiroSuspended(account)) {
    toast(LANG === "en" ? "Run a confirmed Kiro health test after AWS restores the account." : "AWS 恢复账号后，请执行带 credits 确认的 Kiro 双层测活。", "bad");
    return;
  }
  if (!confirm((LANG === "en" ? "Clear quarantine for account " : "清除账号隔离 ") + id + " ?")) return;
  try {
    await api(`/admin/accounts/${id}/clear-quarantine`, { method: "POST" });
    toast((LANG === "en" ? "Quarantine cleared" : "隔离已清除"), "ok");
    await loadAccounts();
    openAcc(id);
  } catch (e) { toast(e.message, "bad"); }
}

function closeDrawer() { $("#drawer").classList.remove("open"); }
async function openAcc(id) {
  const a = ACCTS.find((x) => x.id === id); if (!a) return;
  $("#drTitle").innerHTML = `${esc(a.label || a.id)} ${provChip(a.provider)}`;
  $("#drawer").classList.add("open");
  $("#drBody").innerHTML = '<div class="empty">' + t("common.loading") + "</div>";
  let idn = {}; try { idn = await api(`/admin/accounts/${id}/identity`); } catch (e) {}
  const curEg = a.egress_binding ? a.egress_binding.primary_egress_id : "";
  const egOpts = EGRESS.map((e) => `<option value="${esc(e.id)}" ${e.id === curEg ? "selected" : ""}>${esc(e.id)} (${esc(e.type)})</option>`).join("");
  const caps = (a.capabilities || []).map((c) => `<span class="chip">${esc(c.model_slug)} · ${fmt(c.native_max_context_window)}</span>`).join(" ") || `<span class="muted">${LANG === "en" ? "not probed" : "未探测"}</span>`;
  const kv = (k, v) => `<div class="key">${k}</div><div class="mono">${esc(v || "—")}</div>`;
  const q = QUOTA[id]; const hasQ = q && (q.used_percent >= 0 || q.remaining_tokens >= 0);
  const sd = q && q.secondary_7d;
  const hasSd = sd && sd.limit_window_seconds > 0;
  const quotaSect = hasQ ? `<div class="sect">${LANG === "en" ? "Quota" : "账号额度"}</div>
    <div class="row" style="align-items:center;gap:14px;flex-wrap:wrap">
      <div style="text-align:center">${Charts.ring(q.used_percent, { size: 90, thick: 10, color: quotaColor(q.used_percent), sub: "5h" })}
        <div class="muted" style="font-size:11px;margin-top:4px">${q.used_percent >= 0 ? Math.round(q.used_percent) + "%" : "—"}</div></div>
      ${hasSd ? `<div style="text-align:center">${Charts.ring(sd.used_percent, { size: 90, thick: 10, color: quotaColor(sd.used_percent), sub: "7d" })}
        <div class="muted" style="font-size:11px;margin-top:4px">${sd.used_percent >= 0 ? Math.round(sd.used_percent) + "%" : "—"}</div></div>` : `<div style="text-align:center">${Charts.ring(0, { size: 90, thick: 10, color: "var(--muted)", sub: "7d" })}<div class="muted" style="font-size:11px;margin-top:4px">—</div></div>`}
      <div class="kv" style="grid-template-columns:88px 1fr;flex:1;margin:0">${kv("5h " + (LANG === "en" ? "used" : "已用"), q.used_percent >= 0 ? Math.round(q.used_percent) + "%" : "—")}${kv("5h " + (LANG === "en" ? "remain" : "剩余"), q.remaining_tokens >= 0 ? fmt(q.remaining_tokens) : "—")}${hasSd ? kv("7d " + (LANG === "en" ? "used" : "已用"), Math.round(sd.used_percent) + "%") : ""}${hasSd ? kv("7d " + (LANG === "en" ? "remain" : "剩余"), sd.remaining_tokens >= 0 ? fmt(sd.remaining_tokens) : "—") : ""}</div></div>` : "";
  const suspensionNote = isKiroSuspended(a) ? `<div class="note" style="border-color:var(--bad);color:var(--bad)"><b>${LANG === "en" ? "AWS User ID suspended" : "AWS User ID 已暂停"}</b><br>${LANG === "en" ? "Contact AWS Support for identity verification. After restoration, an administrator must run the confirmed two-stage health test before scheduling resumes." : "请联系 AWS 支持完成身份验证。AWS 恢复后，管理员必须确认 credits 消耗并执行双层测活，成功后才会恢复调度。"}</div>` : "";
  $("#drBody").innerHTML = `
    ${suspensionNote}<div class="kv">${kv("ID", a.id)}${kv("Provider", a.provider)}${kv("Email", a.email)}${kv("Plan", a.plan_type)}${kv("Status", isKiroSuspended(a) ? (LANG === "en" ? "AWS User ID suspended / quarantined" : "AWS User ID 已暂停 / 隔离") : a.status)}${kv(LANG === "en" ? "Quarantine reason" : "隔离原因", a.quarantine_reason)}</div>
    <div class="sect">${LANG === "en" ? "Models" : "模型能力"}</div><div class="row">${caps}</div>${quotaSect}
    <div class="sect">${LANG === "en" ? "Egress" : "出口绑定"}</div>
    <div class="row"><select class="t" id="drEg" style="flex:1">${egOpts || "<option>—</option>"}</select><button class="btn" onclick="setEg('${a.id}')">${LANG === "en" ? "Set primary" : "设为主出口"}</button></div>
    <div class="sect">${LANG === "en" ? "Virtual identity" : "账号绑定的虚拟身份"}</div>
    <div class="kv">${kv("OS", (idn.os_name || "") + " " + (idn.os_version || ""))}${kv("Arch", idn.arch)}${kv("Terminal", idn.terminal)}${kv("Codex UA", idn.codex_user_agent)}${kv("Claude UA", idn.claude_user_agent)}${kv("Session", idn.session_id)}${kv("Machine ID", idn.machine_id)}${kv("Username", idn.username)}${kv("Hostname", idn.hostname)}</div>
    <div class="sect">${t("common.actions")}</div>
    <div class="row">
      <button class="btn sm" onclick="act('${a.id}','probe-models','POST')">${LANG === "en" ? "Probe models" : "探测模型"}</button>
      <button class="btn sm" onclick="act('${a.id}','refresh','POST')">${LANG === "en" ? "Refresh" : "刷新"}</button>
      <button class="btn sm" onclick="healthTest('${a.id}')">${LANG === "en" ? "Health test" : "测试存活"}</button>
      ${a.status === "active" ? `<button class="btn sm" onclick="act('${a.id}','disable','POST')">${LANG === "en" ? "Disable" : "禁用"}</button>` : `<button class="btn sm" onclick="act('${a.id}','enable','POST')">${LANG === "en" ? "Enable" : "启用"}</button>`}
      ${!isKiroSuspended(a) && a.quarantine_until && a.quarantine_until > Math.floor(Date.now() / 1000) ? `<button class="btn sm warn" onclick="clearQuarantine('${a.id}')">${LANG === "en" ? "Clear quarantine" : "清除隔离"}</button>` : ""}
      <button class="btn sm danger" onclick="delAcc('${a.id}')">${LANG === "en" ? "Delete" : "删除"}</button>
    </div>`;
  tickUntil();
  // The detail drawer (#drBody) has no tables; the previous code re-enhanced
  // #overviewView here as a copy-paste leftover. Scope to the drawer so opening an
  // account never reaches across views.
  setTimeout(() => { enhanceAllTables($("#drBody")); }, 10);
}
async function setEg(id) { try { const eg = $("#drEg").value; await api(`/admin/accounts/${id}/egress-binding`, { method: "POST", body: JSON.stringify({ primary_egress_id: eg }) }); toast(t("ok.saved"), "ok"); await loadAccounts(); openAcc(id); } catch (e) { toast(e.message, "bad"); } }

/* egress */
let egMode = "fields";
function setEgMode(m) { egMode = m; $$('#egView [data-m]').forEach((x) => { if (x.tagName === "BUTTON") x.classList.toggle("on", x.dataset.m === m); else if (x.tagName === "DIV") x.classList.toggle("hide", x.dataset.m !== m); }); }
function maskEndpoint(ep) { if (!ep) return "-"; return ep.replace(/\/\/([^:@/]+):([^@/]+)@/, "//$1:***@"); }
async function loadEgress() {
  $("#egView").innerHTML = egressPageHTML();
  $("#egTabs").addEventListener("click", (e) => { const b = e.target.closest("button"); if (b) setEgMode(b.dataset.m); });
  const egs = (await api("/admin/egress-profiles")) || []; EGRESS = egs;
  $("#egTable").innerHTML = !egs.length ? `<div class="empty">${LANG === "en" ? "No egress" : "暂无出口"}</div>` :
    `<table><thead><tr><th>${LANG === "en" ? "Name / Type" : "名称 / 类型"}</th><th>Endpoint</th><th>${LANG === "en" ? "Region / IP" : "地区 / 出口IP"}</th><th>${LANG === "en" ? "Health" : "健康"}</th><th>${LANG === "en" ? "Latency" : "延迟"}</th><th></th></tr></thead><tbody>` +
    egs.map((e) => `<tr><td>${esc(e.name || e.id)}<div class="mono muted">${esc(e.type)}</div></td>
      <td class="mono" style="max-width:200px;word-break:break-all">${esc(maskEndpoint(e.endpoint))}</td>
      <td>${e.region ? `<span class="chip ${/^(JP|TW)/i.test(e.region) ? "ok" : "warn"}">${esc(e.region)}</span>` : `<span class="muted">—</span>`}<div class="mono muted">${esc(e.exit_ip || "")}</div></td>
      <td><span class="chip ${e.health === "healthy" ? "ok" : e.health === "tripped" ? "bad" : "warn"}">${esc(e.health)}</span></td>
      <td class="mono">${e.latency_millis ? e.latency_millis + "ms" : "-"}</td>
      <td><div class="row"><button class="btn sm" onclick="detectRegion('${e.id}')">${LANG === "en" ? "Region" : "识别地区"}</button><button class="btn sm" onclick="egHealth('${e.id}')">${LANG === "en" ? "Check" : "健康检查"}</button></div></td></tr>`).join("") + `</tbody></table>`;
  setTimeout(() => enhanceAllTables($("#egTable")), 10);
}
function egressPageHTML() {
  return `<div class="grid splitr">
    <div class="panel"><div class="hd"><h2>${LANG === "en" ? "Egress / Proxy" : "出口 Egress / 代理"}</h2></div><div class="bd" style="padding:0"><div id="egTable"></div></div></div>
    <div class="panel"><div class="hd"><h2>${LANG === "en" ? "Add egress" : "新增出口 / 代理"}</h2></div><div class="bd">
      <div class="tabs" id="egTabs"><button data-m="fields" class="on">${LANG === "en" ? "Fields" : "4 字段"}</button><button data-m="endpoint">Endpoint</button><button data-m="batch">${LANG === "en" ? "Batch" : "批量导入"}</button></div>
      <label class="f">${LANG === "en" ? "Type" : "类型"}</label>
      <select class="t" id="egType"><option value="socks5h_proxy">socks5h_proxy</option><option value="socks5_proxy">socks5_proxy</option><option value="http_proxy">http_proxy</option><option value="https_proxy">https_proxy</option><option value="warp_proxy">warp_proxy</option><option value="direct">direct</option><option value="curl_cffi_sidecar">curl_cffi_sidecar</option></select>
      <div data-m="fields"><div class="row"><div style="flex:2"><label class="f">${LANG === "en" ? "Proxy host" : "代理 IP / 主机"}</label><input class="t" id="egHost" placeholder="1.2.3.4"></div><div style="flex:1"><label class="f">${LANG === "en" ? "Port" : "端口"}</label><input class="t" id="egPort" placeholder="1080"></div></div>
        <div class="row"><div style="flex:1"><label class="f">${LANG === "en" ? "User" : "用户名"}</label><input class="t" id="egUser"></div><div style="flex:1"><label class="f">${LANG === "en" ? "Pass" : "密码"}</label><input class="t" id="egPass" type="password"></div></div></div>
      <div data-m="endpoint" class="hide"><label class="f">${LANG === "en" ? "Name" : "名称"}</label><input class="t" id="egName" placeholder="jp-1"><label class="f">Endpoint URL</label><input class="t" id="egEndpoint" placeholder="socks5h://user:pass@host:1080"></div>
      <div data-m="batch" class="hide"><label class="f">${LANG === "en" ? "Batch (host:port:user:pass per line)" : "批量（每行 host:port:user:pass）"}</label><textarea class="t" id="egBatch"></textarea></div>
      <label class="row" style="gap:6px;margin-top:10px"><input type="checkbox" id="egDetect" checked> ${LANG === "en" ? "Auto-detect region" : "保存时自动识别地区"}</label>
      <label class="f" style="margin-top:8px">${LANG === "en" ? "Max concurrency" : "最大并发"}</label><input class="t" id="egConc" type="number" value="16">
      <div style="height:12px"></div><button class="btn pri" onclick="addEgress()">${t("act.save")}</button>
    </div></div></div>`;
}
async function addEgress() {
  try {
    const type = $("#egType").value, detect = $("#egDetect").checked, conc = parseInt($("#egConc").value || "16", 10);
    if (egMode === "batch") {
      const lines = $("#egBatch").value.trim(); if (!lines) { toast(LANG === "en" ? "Enter proxies" : "请输入批量代理", "bad"); return; }
      const r = await api("/admin/egress-profiles/import", { method: "POST", body: JSON.stringify({ type, lines, detect_region: detect }) });
      toast(`+${r.count}` + (r.errors && r.errors.length ? ` · ${r.errors.length} failed` : ""), r.errors && r.errors.length ? "bad" : "ok"); $("#egBatch").value = ""; loadEgress(); return;
    }
    const body = { type, stream_capable: true, health: "healthy", max_concurrency: conc, detect_region: detect };
    if (egMode === "fields") { body.host = $("#egHost").value.trim(); body.port = $("#egPort").value.trim(); body.username = $("#egUser").value.trim(); body.password = $("#egPass").value; if (!body.host) { toast(LANG === "en" ? "Enter host" : "请输入代理 IP", "bad"); return; } }
    else { body.name = $("#egName").value.trim(); body.endpoint = $("#egEndpoint").value.trim(); if (!body.endpoint) { toast(LANG === "en" ? "Enter endpoint" : "请输入 Endpoint", "bad"); return; } }
    await api("/admin/egress-profiles", { method: "POST", body: JSON.stringify(body) }); toast(t("ok.saved"), "ok"); loadEgress();
  } catch (e) { toast(e.message, "bad"); }
}
async function detectRegion(id) { toast(LANG === "en" ? "Detecting…" : "识别中…"); try { const r = await api(`/admin/egress-profiles/${id}/detect-region`, { method: "POST" }); toast(`${r.region || "?"} ${r.city || ""} · ${r.exit_ip || ""} · ${r.latency_ms || "?"}ms`, "ok"); loadEgress(); } catch (e) { toast(e.message, "bad"); } }
async function egHealth(id) { try { await api(`/admin/egress-profiles/${id}/health-check`, { method: "POST" }); toast("healthy", "ok"); loadEgress(); } catch (e) { toast(e.message, "bad"); } }

async function loadUsage() {
  $("#usageView").innerHTML = '<div class="viz kpis">' + Array.from({length:5}, () => UI.kpiSkeleton()).join('') + '</div>' +
    '<div class="viz cols2"><div class="panel"><div class="hd"><h2>' + (LANG === "en" ? "Usage trend" : "用量趋势") + '</h2></div><div class="bd chartwrap">' + UI.chartSkeleton() + '</div></div><div class="panel"><div class="hd"><h2>' + (LANG === "en" ? "Token mix" : "Token 构成") + '</h2></div><div class="bd">' + UI.chartSkeleton("200","200") + '</div></div></div>' +
    '<div class="panel"><div class="hd"><h2>' + (LANG === "en" ? "Usage by account" : "用量 / 计费") + '</h2></div><div class="bd" style="padding:0">' + UI.tableSkeleton(4, 5) + '</div></div>';
  const rp = rangeParams();
  const [usage, accs, ts] = await Promise.all([api("/admin/usage").catch(() => []), api("/admin/accounts").catch(() => []), api(`/admin/usage/timeseries?since=${rp.since}&bucket=${rp.bucket}`).catch(() => ({ buckets: [] }))]);
  const u = usage || []; const labels = {}; (accs || []).forEach((a) => (labels[a.id] = a.label || a.id));
  const buckets = (ts && ts.buckets) || [];
  const totalTok = u.reduce((s, x) => s + (+x.total_tokens || 0), 0), cached = u.reduce((s, x) => s + (+x.cached_tokens || 0), 0);
  const promptT = u.reduce((s, x) => s + (+x.prompt_tokens || 0), 0), completion = u.reduce((s, x) => s + (+x.completion_tokens || 0), 0);
  const ratio = cached + promptT > 0 ? cached / (cached + promptT) : 0;
  $("#usageView").innerHTML =
    `<div class="viz kpis">${kpiCard({ k: "Token", v: fmt(totalTok), sub: "in+out", accent: true }) + kpiCard({ k: LANG === "en" ? "Input" : "输入", v: fmt(promptT) }) + kpiCard({ k: LANG === "en" ? "Output" : "输出", v: fmt(completion) }) + kpiCard({ k: LANG === "en" ? "Cached" : "缓存读取", v: fmt(cached) }) + kpiCard({ k: LANG === "en" ? "Hit rate" : "缓存命中率", v: pct(ratio) })}</div>
    <div class="viz cols2"><div class="panel"><div class="hd"><h2>${LANG === "en" ? "Usage trend" : "用量趋势"}</h2><span class="sp"></span>${usageLegend()}<span style="margin-left:10px">${rangeSeg()}</span></div><div class="bd chartwrap">${Charts.stackArea(buckets, USAGE_SERIES, { xfmt: rp.xfmt })}</div></div>
      <div class="panel"><div class="hd"><h2>${LANG === "en" ? "Token mix" : "Token 构成"}</h2></div><div class="bd" style="display:flex;justify-content:center">${Charts.donut([{ label: "in", value: Math.max(0, promptT - cached), color: "#5b8cff" }, { label: "cache", value: cached, color: "#34d399" }, { label: "out", value: completion, color: "#7c5cff" }], { center: pct(ratio), sub: LANG === "en" ? "cache" : "缓存命中" })}</div></div></div>
    <div class="panel"><div class="hd"><h2>${LANG === "en" ? "Usage by account" : "用量 / 计费"}</h2></div><div class="bd" style="padding:0" id="usageTable"></div></div>`;
  if (!u.length) { $("#usageTable").innerHTML = `<div class="empty">${LANG === "en" ? "No usage records" : "暂无用量记录"}</div>`; return; }
  const max = Math.max(...u.map((x) => +x.total_tokens || 0), 1);
  $("#usageTable").innerHTML = `<table><thead><tr><th>${t("common.account")}</th><th data-sort="number">${LANG === "en" ? "Req" : "请求"}</th><th data-sort="number">${LANG === "en" ? "In" : "输入"}</th><th data-sort="number">${LANG === "en" ? "Out" : "输出"}</th><th data-sort="number">${LANG === "en" ? "Cached" : "缓存"}</th><th data-sort="number">${LANG === "en" ? "Total" : "合计"}</th></tr></thead><tbody>` +
    u.map((x) => `<tr><td>${esc(labels[x.account_id] || x.account_id)}</td><td>${x.requests}</td><td class="mono">${fmt(x.prompt_tokens)}</td><td class="mono">${fmt(x.completion_tokens)}</td><td class="mono">${fmt(x.cached_tokens)}</td><td class="mono">${fmt(x.total_tokens)}</td></tr>`).join("") + `</tbody></table>`;
  setTimeout(() => {
    const tbl = $("#usageTable table, #usageTable > table");
    if (tbl) { enhanceAllTables(tbl); makeTablePageable(tbl, 15); }
  }, 10);
}

/* isolation */
async function loadIsolation() {
  $("#isoView").innerHTML = `<div class="grid splitr"><div class="panel"><div class="hd"><h2>${LANG === "en" ? "Session map · isolation" : "账号会话映射 · 串号隔离"}</h2></div><div class="bd" style="padding:0" id="isoTable"></div></div><div class="panel"><div class="hd"><h2>${LANG === "en" ? "Isolation / cache" : "隔离 / 缓存开关"}</h2></div><div class="bd" id="isoControls"><div class="empty">${t("common.loading")}</div></div></div></div>`;
  const [settings, accs, usage] = await Promise.all([api("/admin/settings").catch(() => ({})), api("/admin/accounts").catch(() => []), api("/admin/usage").catch(() => [])]);
  SETTINGS = settings || {}; ACCTS = accs || [];
  const um = {}; (usage || []).forEach((x) => (um[x.account_id] = x));
  renderIsoControls(); renderIsoTable(ACCTS, um);
}
function renderIsoControls() {
  const s = SETTINGS;
  const sw = (key, on, title, desc) => `<div class="swrow"><div class="lbl"><b>${title}</b><small>${desc}</small></div><label class="sw"><input type="checkbox" ${on ? "checked" : ""} onchange="toggleSetting('${key}',this.checked)"><i></i></label></div>`;
  $("#isoControls").innerHTML =
    sw("conversation_isolation", !!s.conversation_isolation, LANG === "en" ? "Conversation isolation" : "串号隔离", LANG === "en" ? "Namespace conversation ids per account." : "按账号命名空间化会话标识。") +
    sw("leak_scrub", !!s.leak_scrub, LANG === "en" ? "Leak scrub" : "防泄露", LANG === "en" ? "Hide pool-internal upstream signals." : "隐藏上游账号额度/限流等信息。") +
    sw("claude_cache_control_inject", !!s.claude_cache_control_inject, LANG === "en" ? "Claude cache inject" : "Claude 缓存注入", LANG === "en" ? "Inject cache_control on the OpenAI→Claude path." : "为兼容通道注入 cache_control 断点。");
}
async function toggleSetting(key, val) { try { const s = await api("/admin/settings", { method: "PATCH", body: JSON.stringify({ [key]: val }) }); SETTINGS = s || {}; toast(t("ok.saved"), "ok"); if ($("#isoControls")) renderIsoControls(); } catch (e) { toast(e.message, "bad"); if ($("#isoControls")) loadIsolation(); } }
function renderIsoTable(accs, um) {
  if (!accs.length) { $("#isoTable").innerHTML = `<div class="empty">${LANG === "en" ? "No accounts" : "暂无账号"}</div>`; return; }
  const iso = !!SETTINGS.conversation_isolation;
  $("#isoTable").innerHTML = `<table><thead><tr><th>${t("common.account")}</th><th>${LANG === "en" ? "Isolation" : "隔离"}</th><th>${LANG === "en" ? "Cooling" : "限额冷却"}</th><th data-sort="number">${LANG === "en" ? "Cache" : "缓存命中"}</th></tr></thead><tbody>` +
    accs.map((a) => { const r = cacheRatio(um[a.id]); return `<tr class="clk" onclick="openSessions('${a.id}')"><td><div>${esc(a.label || a.id)} ${provChip(a.provider)}</div><div class="mono muted">${esc(a.id)}</div></td><td>${iso ? `<span class="chip ok">${LANG === "en" ? "isolated" : "隔离中"}</span>` : `<span class="chip warn">${LANG === "en" ? "passthrough" : "透传"}</span>`}</td><td>${cooldownChip(a)}</td><td><div class="ratio"><div class="bar"><i style="width:${Math.round(100 * r)}%;background:linear-gradient(90deg,var(--ok),var(--acc))"></i></div><span class="mono">${pct(r)}</span></div></td></tr>`; }).join("") + `</tbody></table>`;
  setTimeout(() => enhanceAllTables($("#isoTable")), 10);
}
async function openSessions(id) {
  const a = (ACCTS || []).find((x) => x.id === id) || { id };
  $("#drTitle").innerHTML = `${esc(a.label || id)} · ${LANG === "en" ? "sessions" : "会话映射"}`;
  $("#drawer").classList.add("open"); $("#drBody").innerHTML = '<div class="empty">' + t("common.loading") + "</div>";
  let d = {}; try { d = await api(`/admin/accounts/${id}/sessions`); } catch (e) { $("#drBody").innerHTML = '<div class="empty">' + esc(e.message) + "</div>"; return; }
  const kv = (k, v) => `<div class="key">${k}</div><div class="mono">${esc(v == null ? "—" : v)}</div>`;
  const sess = d.sessions || [];
  const srows = sess.length ? sess.map((s) => `<tr><td><span class="chip">${esc(s.source)}</span></td><td class="mono">${esc(s.original || "—")}</td><td class="mono" style="color:var(--acc)">${esc(s.namespaced || "—")}</td></tr>`).join("") : `<tr><td colspan="3" class="muted" style="padding:14px">${LANG === "en" ? "No pinned sessions yet" : "暂无固定会话"}</td></tr>`;
  $("#drBody").innerHTML = `<div class="kv">${kv(LANG === "en" ? "Isolation" : "隔离状态", d.isolation_enabled ? "on" : "off")}${kv("Machine ID", d.machine_id)}${kv("Codex Session", d.session_id)}${kv("Claude Session", d.claude_session_id)}</div>
    <div class="sect">${LANG === "en" ? "Namespaced session map" : "命名空间化会话映射"}</div><table><thead><tr><th>${LANG === "en" ? "Source" : "来源"}</th><th>${LANG === "en" ? "Original" : "原始"}</th><th>${LANG === "en" ? "Namespaced" : "命名空间化"}</th></tr></thead><tbody>${srows}</tbody></table>`;
}

/* groups */
let GROUPS_CACHE = [];
const EFFORTS = ["", "minimal", "low", "medium", "high", "xhigh"];
function effortSelectHTML(id, val) {
  return `<select class="t" id="${id}">${EFFORTS.map((e) => `<option value="${e}" ${e === (val || "") ? "selected" : ""}>${e || "—"}</option>`).join("")}</select>`;
}
async function loadGroups() {
  await refreshModels();
  const [groups, accounts] = await Promise.all([api("/admin/groups").catch(() => []), api("/admin/accounts").catch(() => [])]);
  GROUPS_CACHE = groups || [];
  const accs = accounts || [];
  const counts = {};
  accs.forEach((a) => { const g = a.group_name || "cyber"; counts[g] = (counts[g] || 0) + 1; });
  const en = LANG === "en";
  const cards = GROUPS_CACHE.map((g) => groupCard(g, counts[g.name] || 0)).join("");
  $("#groupsView").innerHTML = `
    <div class="panel" style="max-width:860px"><div class="hd"><h2>${en ? "Groups" : "分组管理"}</h2><span class="sp"></span><button class="btn" onclick="newGroupForm()">${icon("plus")} ${en ? "New group" : "新建分组"}</button></div>
      <div class="bd"><div class="note">${en ? "Each group pins a model + reasoning effort for its accounts; a downstream API key (or the default group) routes requests here." : "每个分组可为其账号固定模型 + 推理强度；下游 API Key（或默认分组）据此路由。"}</div>
      <div id="newGroupBox"></div>${cards || `<div class="muted">${en ? "No groups." : "暂无分组。"}</div>`}</div></div>
    <div class="panel" style="max-width:860px;margin-top:12px"><div class="hd"><h2>${en ? "Account membership" : "账号归属"}</h2></div><div class="bd" style="padding:0" id="membersTable"></div></div>`;
  renderMembers(accs);
}
function groupCard(g, count) {
  const en = LANG === "en";
  const k = cssEsc(g.name);
  return `<div class="panel" style="margin:8px 0" data-grp="${esc(g.name)}"><div class="bd">
    <div class="row" style="align-items:center;gap:8px"><b style="font-size:15px">${esc(g.name)}</b><span class="chip">${count} ${en ? "accts" : "账号"}</span></div>
    <div class="row" style="gap:12px;margin-top:8px"><div style="flex:1"><label class="f">force_model</label>${modelSelectHTML("gm_" + k, g.force_model || "", {})}</div>
      <div style="flex:1"><label class="f">force_effort</label>${effortSelectHTML("ge_" + k, g.force_effort || "")}</div></div>
    <label class="f" style="margin-top:8px">default_egress_id</label><input class="t mono" id="gx_${k}" value="${esc(g.default_egress_id || "")}" placeholder="(${en ? "default / shared VPS" : "默认 / 共享VPS"})">
    <label class="f" style="margin-top:8px">system_prompt</label><textarea class="t" id="gp_${k}" placeholder="${en ? "(empty = passthrough)" : "（留空 = 不改写）"}">${esc(g.system_prompt || "")}</textarea>
    <div class="row" style="gap:18px;margin:8px 0"><label class="row" style="gap:6px"><input type="checkbox" id="gc_${k}" ${g.system_prompt_apply_to_compaction ? "checked" : ""}> compaction</label></div>
    <div class="row" style="gap:6px"><button class="btn pri" onclick="saveGroup('${esc(g.name)}')">${t("act.save")}</button><button class="btn bad" onclick="deleteGroup('${esc(g.name)}')">${t("act.delete")}</button></div></div></div>`;
}
function newGroupForm() {
  const en = LANG === "en";
  $("#newGroupBox").innerHTML = `<div class="panel" style="margin:8px 0;border:1px dashed var(--bd)"><div class="bd">
    <label class="f">${en ? "New group name" : "新分组名"}</label><input class="t mono" id="ngName" placeholder="team-a">
    <div class="row" style="gap:12px;margin-top:8px"><div style="flex:1"><label class="f">force_model</label>${modelSelectHTML("ngModel", "", {})}</div><div style="flex:1"><label class="f">force_effort</label>${effortSelectHTML("ngEffort", "")}</div></div>
    <div class="row" style="gap:6px;margin-top:10px"><button class="btn pri" onclick="createGroup()">${t("act.create")}</button><button class="btn" onclick="document.getElementById('newGroupBox').innerHTML=''">${t("act.cancel")}</button></div></div></div>`;
}
async function createGroup() {
  const name = ($("#ngName").value || "").trim();
  if (!name) { toast(LANG === "en" ? "Name required" : "请填写分组名", "bad"); return; }
  try { await api("/admin/groups", { method: "POST", body: JSON.stringify({ name, force_model: ($("#ngModel").value || "").trim(), force_effort: $("#ngEffort").value }) }); toast(t("ok.saved"), "ok"); loadGroups(); }
  catch (e) { toast(e.message, "bad"); }
}
async function saveGroup(name) {
  const k = cssEsc(name);
  try {
    await api("/admin/groups/" + encodeURIComponent(name), { method: "PATCH", body: JSON.stringify({
      system_prompt: $("#gp_" + k).value, system_prompt_apply_to_compaction: $("#gc_" + k).checked,
      force_model: ($("#gm_" + k).value || "").trim(), force_effort: $("#ge_" + k).value, default_egress_id: ($("#gx_" + k).value || "").trim(),
    }) });
    toast(t("ok.saved"), "ok");
  } catch (e) { toast(e.message, "bad"); }
}
async function deleteGroup(name) {
  if (!confirm((LANG === "en" ? "Delete group " : "删除分组 ") + name + "?")) return;
  try { await api("/admin/groups/" + encodeURIComponent(name), { method: "DELETE" }); toast(t("ok.deleted"), "ok"); loadGroups(); }
  catch (e) { toast(e.message, "bad"); }
}
function renderMembers(accs) {
  const en = LANG === "en";
  if (!accs.length) { $("#membersTable").innerHTML = `<div class="empty">${en ? "No accounts" : "暂无账号"}</div>`; return; }
  const opts = (cur) => GROUPS_CACHE.map((g) => `<option value="${esc(g.name)}" ${g.name === (cur || "cyber") ? "selected" : ""}>${esc(g.name)}</option>`).join("");
  $("#membersTable").innerHTML = `<table><thead><tr><th>${t("common.account")}</th><th>${en ? "Provider" : "供应商"}</th><th>${en ? "Group" : "分组"}</th></tr></thead><tbody>` +
    accs.map((a) => `<tr><td>${esc(a.label || a.email || a.id)}</td><td>${esc(a.provider || "codex")}</td><td><select class="t" onchange="reassignAccount('${esc(a.id)}', this.value)">${opts(a.group_name)}</select></td></tr>`).join("") + `</tbody></table>`;
}
async function reassignAccount(id, group) {
  try { await api("/admin/accounts/" + encodeURIComponent(id) + "/group", { method: "POST", body: JSON.stringify({ group }) }); toast(t("ok.saved"), "ok"); }
  catch (e) { toast(e.message, "bad"); loadGroups(); }
}

/* api keys (admin: all keys) */
async function loadKeys() {
  await refreshModels();
  $("#keysView").innerHTML = `<div class="grid splitr"><div class="panel"><div class="hd"><h2>${LANG === "en" ? "Downstream API keys" : "下游 API Key"}</h2></div><div class="bd" style="padding:0" id="keysTable"></div></div>
    <div class="panel"><div class="hd"><h2>${LANG === "en" ? "New key" : "新建 Key"}</h2></div><div class="bd">
      <label class="f">${LANG === "en" ? "Label" : "标签"}</label><input class="t" id="kLabel"><label class="f">${LANG === "en" ? "Group" : "分组"}</label><input class="t" id="kGroup" placeholder="cyber">
      <label class="f">force_model</label>${modelSelectHTML("kForceModel", "", {})}<label class="f">force_effort</label><select class="t" id="kForceEffort"><option value="">—</option><option>minimal</option><option>low</option><option>medium</option><option>high</option><option>xhigh</option></select>
      <div style="height:12px"></div><button class="btn pri" onclick="createKey()">${t("act.create")}</button></div></div></div>`;
  const keys = (await api("/admin/api-keys").catch(() => [])) || [];
  if (!keys.length) { $("#keysTable").innerHTML = `<div class="empty">${LANG === "en" ? "No keys" : "暂无下游 Key"}</div>`; window.__KEYS = []; return; }
  $("#keysTable").innerHTML = `<table><thead><tr><th>${LANG === "en" ? "Label / Group" : "标签 / 分组"}</th><th>force_model</th><th>effort</th><th>${t("common.status")}</th><th>${LANG === "en" ? "Key / Install" : "Key / 安装"}</th><th></th></tr></thead><tbody>` +
    keys.map((k) => `<tr><td>${esc(k.label || "-")}<div class="muted">${esc(k.group_name || "cyber")}</div></td><td class="mono">${esc(k.force_model || "—")}</td><td>${k.force_effort ? `<span class="chip acc">${esc(k.force_effort)}</span>` : "—"}</td><td>${k.enabled ? `<span class="chip ok">on</span>` : `<span class="chip warn">off</span>`}</td><td>${keyCopyCell(k)}</td>
      <td><div class="row"><button class="btn sm" onclick="editKey('${k.key_hash}')">${t("act.edit")}</button><button class="btn sm" onclick="toggleKey('${k.key_hash}',${k.enabled ? "false" : "true"})">${k.enabled ? t("act.disable") : t("act.enable")}</button><button class="btn sm danger" onclick="deleteKey('${k.key_hash}')">${t("act.delete")}</button></div></td></tr>`).join("") + `</tbody></table>`;
  window.__KEYS = keys;
  setTimeout(() => enhanceAllTables($("#keysTable")), 10);
}
// keyCopyCell now lives in core.js (shared with the user portal's My Keys page).
async function createKey() { try { const r = await api("/admin/api-keys", { method: "POST", body: JSON.stringify({ label: $("#kLabel").value.trim(), group_name: $("#kGroup").value.trim() || "cyber", force_model: $("#kForceModel").value.trim(), force_effort: $("#kForceEffort").value, enabled: true }) }); showSecretModal(r.key, { extra: keyUsageHint(r.key) }); toast(t("ok.saved"), "ok"); loadKeys(); } catch (e) { toast(e.message, "bad"); } }
async function editKey(hash) {
  const k = (window.__KEYS || []).find((x) => x.key_hash === hash) || {};
  await refreshModels();
  const en = LANG === "en";
  const effortOpts = ["", "minimal", "low", "medium", "high", "xhigh"].map((e) => `<option value="${e}" ${e === (k.force_effort || "") ? "selected" : ""}>${e || "—"}</option>`).join("");
  const { ov, close } = openModal(`
    <div style="margin-bottom:12px"><b style="font-size:15px">${en ? "Edit key" : "编辑 Key"}${k.label ? " · " + esc(k.label) : ""}</b></div>
    <label class="f">force_model</label>${modelSelectHTML("ekModel", k.force_model || "", {})}
    <label class="f" style="margin-top:8px">force_effort</label><select class="t" id="ekEffort">${effortOpts}</select>
    <div class="row" style="justify-content:flex-end;gap:8px;margin-top:16px"><button class="btn" id="ekCancel">${t("act.cancel")}</button><button class="btn pri" id="ekSave">${t("act.save")}</button></div>`);
  ov.querySelector("#ekCancel").onclick = close;
  ov.querySelector("#ekSave").onclick = async () => {
    const force_model = (ov.querySelector("#ekModel") || {}).value || "";
    const force_effort = (ov.querySelector("#ekEffort") || {}).value || "";
    try { await api("/admin/api-keys/" + hash, { method: "PATCH", body: JSON.stringify({ force_model, force_effort }) }); close(); toast(t("ok.saved"), "ok"); loadKeys(); } catch (e) { toast(e.message, "bad"); }
  };
}
async function toggleKey(hash, on) { try { await api("/admin/api-keys/" + hash, { method: "PATCH", body: JSON.stringify({ enabled: on }) }); loadKeys(); } catch (e) { toast(e.message, "bad"); } }
async function deleteKey(hash) { if (!confirm(LANG === "en" ? "Delete key?" : "删除该 Key？")) return; try { await api("/admin/api-keys/" + hash, { method: "DELETE" }); toast(t("ok.deleted"), "ok"); loadKeys(); } catch (e) { toast(e.message, "bad"); } }

/* users (admin): list + create + role/status/reset-password/delete */
async function loadUsers() {
  const users = (await api("/admin/users").catch(() => [])) || [];
  window.__USERS = users;
  const rows = users.length ? users.map((u) => `<tr>
    <td>${esc(u.email)}<div class="mono muted">${esc(u.id)}</div></td>
    <td>${esc(u.name || "—")}</td>
    <td><span class="chip ${u.role === "admin" ? "acc" : ""}">${esc(u.role || "user")}</span></td>
    <td>${statusChip(u.status || "active")}</td>
    <td><div class="row">
      <button class="btn sm" onclick="userSetRole('${u.id}','${u.role === "admin" ? "user" : "admin"}')">${u.role === "admin" ? (LANG === "en" ? "→ user" : "降为用户") : (LANG === "en" ? "→ admin" : "设为管理员")}</button>
      <button class="btn sm" onclick="userSetStatus('${u.id}','${u.status === "disabled" ? "active" : "disabled"}')">${u.status === "disabled" ? t("act.enable") : t("act.disable")}</button>
      <button class="btn sm" onclick="userResetPw('${u.id}')">${LANG === "en" ? "Reset pw" : "重置密码"}</button>
      <button class="btn sm danger" onclick="userDelete('${u.id}')">${t("act.delete")}</button>
    </div></td></tr>`).join("") : `<tr><td colspan="5"><div class="empty">${LANG === "en" ? "No users" : "暂无用户"}</div></td></tr>`;
  $("#usersView").innerHTML = `<div class="grid splitr">
    <div class="panel"><div class="hd"><h2>${t("nav.users")}</h2><span class="sp"></span><span class="muted">${users.length}</span></div><div class="bd" style="padding:0">
      <table><thead><tr><th>${t("auth.email")}</th><th>${LANG === "en" ? "Name" : "昵称"}</th><th>${LANG === "en" ? "Role" : "角色"}</th><th>${t("common.status")}</th><th>${t("common.actions")}</th></tr></thead><tbody>${rows}</tbody></table></div></div>
    <div class="panel"><div class="hd"><h2>${LANG === "en" ? "New user" : "新建用户"}</h2></div><div class="bd">
      <label class="f">${t("auth.email")}</label><input class="t" id="nuEmail" type="email">
      <label class="f">${LANG === "en" ? "Name" : "昵称"}</label><input class="t" id="nuName">
      <label class="f">${t("auth.password")}</label><input class="t" id="nuPass" type="password">
      <label class="f">${LANG === "en" ? "Role" : "角色"}</label><select class="t" id="nuRole"><option value="user">user</option><option value="admin">admin</option></select>
      <div style="height:12px"></div><button class="btn pri" onclick="adminCreateUser()">${t("act.create")}</button></div></div></div>`;
  setTimeout(() => enhanceAllTables($("#usersView")), 10);
}
async function adminCreateUser() {
  try {
    await api("/admin/users", { method: "POST", body: JSON.stringify({ email: $("#nuEmail").value.trim(), name: $("#nuName").value.trim(), password: $("#nuPass").value, role: $("#nuRole").value }) });
    toast(t("ok.saved"), "ok"); loadUsers();
  } catch (e) { toast(e.message, "bad"); }
}
async function userPatch(id, body) { try { await api("/admin/users/" + id, { method: "PATCH", body: JSON.stringify(body) }); toast(t("ok.saved"), "ok"); loadUsers(); } catch (e) { toast(e.message, "bad"); } }
function userSetRole(id, role) { userPatch(id, { role }); }
function userSetStatus(id, status) { userPatch(id, { status }); }
function userResetPw(id) { const pw = prompt(LANG === "en" ? "New password (min 8 chars):" : "新密码（至少 8 位）："); if (!pw) return; userPatch(id, { password: pw }); }
async function userDelete(id) {
  const u = (window.__USERS || []).find((x) => x.id === id) || {};
  if (!confirm((LANG === "en" ? "Delete user " : "删除用户 ") + (u.email || id) + "?")) return;
  try { await api("/admin/users/" + id, { method: "DELETE" }); toast(t("ok.deleted"), "ok"); loadUsers(); } catch (e) { toast(e.message, "bad"); }
}

/* CF */
async function loadCF() {
  const ev = (await api("/admin/cf-events?limit=100")) || [];
  $("#cfView").innerHTML = `<div class="panel"><div class="hd"><h2>Cloudflare / ${LANG === "en" ? "region blocks" : "区域拦截事件"}</h2></div><div class="bd" style="padding:0">` +
    (!ev.length ? `<div class="empty">${LANG === "en" ? "No CF events 🎉" : "无 CF 事件 🎉"}</div>` :
      `<table><thead><tr><th>${LANG === "en" ? "Time" : "时间"}</th><th>${t("common.account")}</th><th>Egress</th><th>${t("common.status")}</th><th>cf-ray</th></tr></thead><tbody>` +
      ev.map((e) => `<tr><td class="mono">${new Date(e.created_at * 1000).toLocaleString()}</td><td class="mono">${esc(e.account_id)}</td><td class="mono">${esc(e.egress_id)}</td><td><span class="chip bad">${e.status}</span></td><td class="mono">${esc(e.cf_ray || "-")}</td></tr>`).join("") + `</tbody></table>`) + `</div></div>`;
}

/* audit */
function auditChip(a) { const m = { ban_delete: "bad", ban_quarantine: "bad", auth_quarantine: "warn", kiro_user_suspended: "bad", kiro_inference_probe: a.state === "alive" ? "ok" : "bad", kiro_suspension_recovered: "ok", health_test: a.state === "alive" ? "ok" : a.state === "banned" ? "bad" : "warn" }; return `<span class="chip ${m[a.action] || ""}">${esc(a.action)}</span>`; }
async function loadAudit() {
  $("#auditView").innerHTML = `<div class="panel"><div class="hd"><h2>${LANG === "en" ? "Audit · ban detection" : "审计日志 · 封禁检测"}</h2><span class="sp"></span><button class="btn sm" onclick="healthTestAll()">${LANG === "en" ? "Test abnormal" : "一键测试异常账号"}</button></div><div class="bd" style="padding:0" id="auditTable"><div class="empty">${t("common.loading")}</div></div></div>`;
  const rows = (await api("/admin/audit?limit=200").catch(() => [])) || [];
  if (!rows.length) { $("#auditTable").innerHTML = `<div class="empty">${LANG === "en" ? "No audit records" : "暂无审计记录"}</div>`; return; }
  $("#auditTable").innerHTML = `<table><thead><tr><th>${LANG === "en" ? "Time" : "时间"}</th><th>${LANG === "en" ? "Action" : "动作"}</th><th>${t("common.account")}</th><th>${LANG === "en" ? "Verdict" : "判定"}</th><th>${LANG === "en" ? "Reason" : "原因"}</th></tr></thead><tbody>` +
    rows.map((a) => `<tr><td class="mono">${new Date(a.created_at * 1000).toLocaleString()}</td><td>${auditChip(a)}</td><td>${esc(a.account_label || "-")}</td><td><span class="chip ${a.state === "alive" ? "ok" : a.state === "banned" ? "bad" : "warn"}">${esc(a.state || "-")}</span></td><td class="mono">${esc(a.reason || "")}</td></tr>`).join("") + `</tbody></table>`;
}
function healthTestMessage(r) {
  if (r && r.auth_probe) {
    const auth = r.auth_probe.alive ? (LANG === "en" ? "Authentication OK" : "认证正常") : (LANG === "en" ? "Authentication failed" : "认证失败");
    let inference = LANG === "en" ? "Inference not checked" : "推理未验证";
    if (r.inference_probe && r.inference_probe.checked) {
      inference = r.inference_probe.alive ? (LANG === "en" ? "Inference available" : "推理可用")
        : r.inference_probe.error_code === "kiro_account_suspended" || r.inference_probe.state === "banned"
          ? (LANG === "en" ? "Inference suspended (contact AWS Support)" : "推理暂停（需联系 AWS 支持）")
          : (LANG === "en" ? "Inference failed" : "推理失败");
    }
    return `${r.ready ? "✓" : "✕"} ${auth} · ${inference}`;
  }
  const detail = (r.state !== "alive" || (r.http_status && r.http_status >= 400)) && r.snippet ? " · " + String(r.snippet).slice(0, 160) : "";
  return `${r.alive ? "✓" : "✕"} ${r.state}${r.reason ? " (" + r.reason + ")" : ""} · HTTP ${r.http_status || "-"}${detail}`;
}
async function healthTest(id) {
  const account = (ACCTS || []).find((a) => a.id === id);
  if (!kiroHealthConfirmation(account ? [account] : [])) return null;
  toast(LANG === "en" ? "Testing…" : "测试中…");
  try {
    const opts = { method: "POST" };
    if (isKiroAccount(account)) opts.body = JSON.stringify({ confirm_cost: true });
    const r = await api(`/admin/accounts/${id}/health-test`, opts);
    toast(healthTestMessage(r), r.ready === true || (!r.auth_probe && r.alive) ? "ok" : "bad");
    if (r.deleted) { closeDrawer(); loadAccounts(); }
    return r;
  } catch (e) { toast(e.message, "bad"); return null; }
}
async function healthTestQuiet(account, kiroConfirmed) {
  try {
    const opts = { method: "POST" };
    if (isKiroAccount(account)) {
      if (!kiroConfirmed) return null;
      opts.body = JSON.stringify({ confirm_cost: true });
    }
    return await api(`/admin/accounts/${account.id}/health-test`, opts);
  } catch { return null; }
}
async function healthTestAll() {
  const accs = ACCTS && ACCTS.length ? ACCTS : (await api("/admin/accounts").catch(() => [])) || [];
  let targets = accs.filter((a) => a.status !== "active" || cooldownInfo(a).active); if (!targets.length) targets = accs;
  if (!targets.length) { toast(LANG === "en" ? "No accounts" : "无账号"); return; }
  if (!kiroHealthConfirmation(targets)) return;
  toast(`${LANG === "en" ? "Testing" : "测试"} ${targets.length}…`);
  let authAlive = 0, authFailed = 0, inferenceAlive = 0, inferenceSuspended = 0, inferenceUnchecked = 0, otherAlive = 0, otherFailed = 0, deleted = 0;
  for (const a of targets) {
    const r = await healthTestQuiet(a, true); if (!r) continue;
    if (r.auth_probe) {
      r.auth_probe.alive ? authAlive++ : authFailed++;
      if (!r.inference_probe || !r.inference_probe.checked) inferenceUnchecked++;
      else if (r.inference_probe.alive) inferenceAlive++;
      else if (r.inference_probe.error_code === "kiro_account_suspended" || r.inference_probe.state === "banned") inferenceSuspended++;
      else inferenceUnchecked++;
    } else r.alive ? otherAlive++ : otherFailed++;
    if (r.deleted) deleted++;
  }
  const summary = LANG === "en"
    ? `Auth OK ${authAlive} / failed ${authFailed} · inference available ${inferenceAlive} / suspended ${inferenceSuspended} / unverified ${inferenceUnchecked} · other OK ${otherAlive} / failed ${otherFailed} · deleted ${deleted}`
    : `认证正常 ${authAlive} / 失败 ${authFailed} · 推理可用 ${inferenceAlive} / 暂停 ${inferenceSuspended} / 未验证 ${inferenceUnchecked} · 其他通过 ${otherAlive} / 失败 ${otherFailed} · 删除 ${deleted}`;
  toast(summary, authFailed || inferenceSuspended || inferenceUnchecked || otherFailed ? "bad" : "ok"); loadAudit();
}

/* gopay */
async function loadGopay() {
  const egs = (await api("/admin/egress-profiles").catch(() => [])) || []; EGRESS = egs;
  let st = {}; try { st = await api("/admin/gopay"); } catch (e) { $("#gopayView").innerHTML = '<div class="empty">' + esc(e.message) + "</div>"; return; }
  const s = st.settings || {};
  $("#gopayView").innerHTML = `<div class="panel"><div class="hd"><h2>GoPay ${LANG === "en" ? "auto-subscribe" : "自动订阅"}</h2><span class="sp"></span>${st.enabled ? '<span class="chip ok">on</span>' : '<span class="chip warn">off</span>'}</div><div class="bd">
    <div class="swrow"><div class="lbl"><b>${LANG === "en" ? "Enable GoPay" : "启用 GoPay 自动订阅"}</b><small>${LANG === "en" ? "Default off. Needs python deps." : "默认关闭，需服务器 python 依赖。"}</small></div><label class="sw"><input type="checkbox" ${st.enabled ? "checked" : ""} onchange="toggleGopay(this.checked)"><i></i></label></div>
    ${st.warning ? `<div class="note" style="border-color:var(--bad)">${esc(st.warning)}</div>` : ""}
    <div class="sect">${LANG === "en" ? "Run subscription" : "运行订阅"}</div><div class="row"><input class="t" id="gpAccId" placeholder="account id" style="flex:1"><button class="btn pri" onclick="gopaySubscribeInput()">${LANG === "en" ? "Subscribe" : "订阅"}</button></div>
    <div class="sect">${LANG === "en" ? "Orchestrator log" : "编排器日志"}</div><pre class="mono" style="height:160px;overflow:auto;white-space:pre-wrap;background:var(--bg);border:1px solid var(--line);border-radius:9px;padding:10px">${esc((st.logs || []).join("\n") || "—")}</pre></div></div>`;
}
async function toggleGopay(on) { try { const st = await api("/admin/gopay", { method: "PATCH", body: JSON.stringify({ enabled: on }) }); toast(t("ok.saved") + (st.warning ? " (" + st.warning + ")" : ""), st.warning ? "bad" : "ok"); loadGopay(); } catch (e) { toast(e.message, "bad"); loadGopay(); } }
async function gopaySubscribe(accountId, phone, pin) { toast(LANG === "en" ? "Subscribing…" : "订阅中…"); try { const r = await api("/admin/gopay/subscribe", { method: "POST", body: JSON.stringify({ account_id: accountId, phone_number: phone || "", pin: pin || "" }) }); toast(r.ok ? "✓ " + (r.charge_ref || "") : "✕ " + (r.error || ""), r.ok ? "ok" : "bad"); return r; } catch (e) { toast(e.message, "bad"); return null; } }
function gopaySubscribeInput() { const id = $("#gpAccId").value.trim(); if (!id) { toast(LANG === "en" ? "Enter account id" : "请输入账号 ID", "bad"); return; } gopaySubscribe(id).then(() => loadGopay()); }

/* org */
async function loadOrg() {
  $("#orgView").innerHTML = `<div class="grid three">
    <div class="panel"><div class="hd"><h2>${LANG === "en" ? "Tenants" : "租户"}</h2></div><div class="bd"><div id="tenList"></div><label class="f">${LANG === "en" ? "Name" : "名称"}</label><input class="t" id="tenName"><div style="height:8px"></div><button class="btn" onclick="addTenant()">${icon("plus")}</button></div></div>
    <div class="panel"><div class="hd"><h2>${LANG === "en" ? "Users" : "用户"}</h2></div><div class="bd"><div id="usrList"></div><label class="f">tenant id</label><input class="t" id="usrTen"><label class="f">email</label><input class="t" id="usrEmail"><div style="height:8px"></div><button class="btn" onclick="addUser()">${icon("plus")}</button></div></div>
    <div class="panel"><div class="hd"><h2>${LANG === "en" ? "Projects" : "项目"}</h2></div><div class="bd"><div id="prjList"></div><label class="f">tenant id</label><input class="t" id="prjTen"><label class="f">${LANG === "en" ? "Name" : "名称"}</label><input class="t" id="prjName"><div style="height:8px"></div><button class="btn" onclick="addProject()">${icon("plus")}</button></div></div></div>`;
  const [tens, usrs, prjs] = await Promise.all([api("/admin/tenants").catch(() => []), api("/admin/users").catch(() => []), api("/admin/projects").catch(() => [])]);
  $("#tenList").innerHTML = (tens || []).map((x) => `<div class="row" style="justify-content:space-between"><span>${esc(x.name)}</span><span class="mono muted">${esc(x.id)}</span></div>`).join("") || `<div class="muted">—</div>`;
  $("#usrList").innerHTML = (usrs || []).map((u) => `<div>${esc(u.email)} <span class="chip ${u.role === "admin" ? "acc" : ""}">${esc(u.role || "user")}</span></div>`).join("") || `<div class="muted">—</div>`;
  $("#prjList").innerHTML = (prjs || []).map((p) => `<div>${esc(p.name)} <span class="chip">${esc(p.group_name)}</span></div>`).join("") || `<div class="muted">—</div>`;
}
async function addTenant() { try { await api("/admin/tenants", { method: "POST", body: JSON.stringify({ name: $("#tenName").value.trim() }) }); toast(t("ok.saved"), "ok"); loadOrg(); } catch (e) { toast(e.message, "bad"); } }
async function addUser() { try { await api("/admin/users", { method: "POST", body: JSON.stringify({ tenant_id: $("#usrTen").value.trim(), email: $("#usrEmail").value.trim() }) }); toast(t("ok.saved"), "ok"); loadOrg(); } catch (e) { toast(e.message, "bad"); } }
async function addProject() { try { await api("/admin/projects", { method: "POST", body: JSON.stringify({ tenant_id: $("#prjTen").value.trim(), name: $("#prjName").value.trim(), group_name: "cyber" }) }); toast(t("ok.saved"), "ok"); loadOrg(); } catch (e) { toast(e.message, "bad"); } }

/* system config — registry-backed, runtime-editable knobs (Phase ①). Reads /admin/config
   (each field carries value + type + category + effect + overridden), renders grouped
   per-type controls, and PATCHes a single key on change. The effect badge tells the
   operator whether a change is live (hot / upstream) or needs a restart (bootstrap
   fields, shown disabled). */
async function loadSystemConfig() {
  let fields = [];
  try { fields = await api("/admin/config"); }
  catch (e) { $("#sysconfigView").innerHTML = `<div class="panel" style="max-width:900px"><div class="bd"><div class="muted">${esc(e.message)}</div></div></div>`; return; }
  $("#sysconfigView").innerHTML = renderConfigPanels(fields || []);
}
function renderConfigPanels(fields) {
  if (!fields.length) return `<div class="panel" style="max-width:900px"><div class="bd"><div class="muted">${LANG === "en" ? "Config unavailable." : "配置不可用。"}</div></div></div>`;
  const cats = []; const byCat = {};
  fields.forEach((f) => { if (!byCat[f.category]) { byCat[f.category] = []; cats.push(f.category); } byCat[f.category].push(f); });
  const intro = LANG === "en"
    ? "Runtime-editable configuration — saved immediately. Most changes take effect on the next request (live); a few bootstrap fields need a restart."
    : "运行时可改配置——改动即时保存。多数下一次请求即生效（即时），少数引导项需重启。";
  return `<div class="panel" style="max-width:900px"><div class="bd"><div class="note">${intro}</div></div></div>` +
    cats.map((c) => `<div class="panel" style="max-width:900px;margin-top:10px"><div class="hd"><h2>${esc(c)}</h2></div><div class="bd">${byCat[c].map(configFieldRow).join("")}</div></div>`).join("");
}
function configFieldRow(f) {
  const dis = f.effect === "restart" ? "disabled" : "";
  const eff = f.effect === "restart" ? (LANG === "en" ? "restart" : "需重启")
    : f.effect === "upstream" ? (LANG === "en" ? "live · upstream" : "即时 · 上游")
    : (LANG === "en" ? "live" : "即时");
  const badge = `<span class="chip">${eff}</span>`;
  const over = f.overridden ? ` <span class="chip">${LANG === "en" ? "overridden" : "已覆盖"}</span>` : "";
  const key = esc(f.key);
  let input;
  if (f.type === "bool") {
    input = `<label class="sw"><input type="checkbox" ${f.value ? "checked" : ""} ${dis} onchange="saveConfigField('${key}', this.checked)"><i></i></label>`;
  } else if (f.type === "select") {
    input = `<select class="t" style="max-width:220px" ${dis} onchange="saveConfigField('${key}', this.value)">${(f.options || []).map((o) => `<option value="${esc(o)}" ${o === f.value ? "selected" : ""}>${esc(o === "" ? (LANG === "en" ? "(default)" : "（默认）") : o)}</option>`).join("")}</select>`;
  } else if (f.type === "int") {
    input = `<input class="t" type="number" style="max-width:160px" value="${esc(String(f.value))}" ${dis} onchange="saveConfigField('${key}', Number(this.value))">`;
  } else {
    const v = Array.isArray(f.value) ? f.value.join(", ") : (f.value == null ? "" : f.value);
    input = `<input class="t" style="max-width:340px" value="${esc(String(v))}" ${dis} placeholder="${f.type === "csv" ? "a, b, c" : ""}" onchange="saveConfigField('${key}', this.value)">`;
  }
  return `<div class="swrow"><div class="lbl"><b>${esc(f.label)}</b> <code class="k" style="font-size:11px">${key}</code> ${badge}${over}<small>${esc(f.help || "")}</small></div><div>${input}</div></div>`;
}
async function saveConfigField(key, val) {
  try { await api("/admin/config", { method: "PATCH", body: JSON.stringify({ [key]: val }) }); toast(t("ok.saved"), "ok"); setTimeout(loadSystemConfig, 120); }
  catch (e) { toast(e.message, "bad"); loadSystemConfig(); }
}

/* Unified settings (3 tabs: connect / system / thinking) */
var _settingTab = "connect";
async function loadSettings() {
  const tab = _settingTab || "connect";
  const tabs = [
    { v: "connect", ic: "link", zh: "接入信息", en: "Connect" },
    { v: "system", ic: "box", zh: "系统配置", en: "System" },
    { v: "thinking", ic: "zap", zh: "Thinking", en: "Thinking" },
  ];
  const t = (k) => (LANG === "en" ? k.en : k.zh);
  const tabbar = `<div class="tabs" style="margin:0 0 10px">${tabs.map((x) => `<button class="${tab === x.v ? 'on' : ''}" onclick="switchSettingTab('${x.v}')"><span class="ic">${icon(x.ic)}</span>${esc(t(x))}</button>`).join("")}</div>`;

  if (tab === "system") {
    await loadSystemConfig();
    // prepend tab bar
    $("#settingsView").querySelector(".panel")?.insertAdjacentHTML("beforebegin", tabbar);
    return;
  }
  if (tab === "thinking") {
    loadEmbedded("thinkingFrame", "/thinking.html");
    const f = document.getElementById("thinkingFrame");
    if (f) {
      const before = f.previousElementSibling;
      const wrapper = document.getElementById("settingsView");
      if (!before || !before.classList.contains("tabs")) {
        const tdiv = document.createElement("div"); tdiv.className = "tabs"; tdiv.style.margin = "0 0 10px";
        tdiv.innerHTML = tabs.map((x) => `<button class="${tab === x.v ? 'on' : ''}" onclick="switchSettingTab('${x.v}')"><span class="ic">${icon(x.ic)}</span>${esc(t(x))}</button>`).join("");
        wrapper.insertBefore(tdiv, wrapper.firstChild);
      }
    }
    return;
  }
  // connect tab
  let models = "—"; try { const m = await api("/v1/models"); models = (m.data || []).map((x) => `${esc(x.id)} <span class="chip">${esc(x.window_mode || "")}</span>`).join(" "); } catch {}
  let st = {}; try { st = await api("/admin/settings"); } catch {}
  const origin = location.origin;
  $("#settingsView").innerHTML = tabbar + `<div class="panel" style="max-width:880px"><div class="hd"><h2>${LANG === "en" ? "Connect" : "下游接入"}</h2></div><div class="bd">
    <div class="sect">${LANG === "en" ? "Operations" : "运营"}</div>
    <div class="swrow"><div class="lbl"><b>${LANG === "en" ? "Allow registration" : "允许注册"}</b><small>${LANG === "en" ? "Let new users self-register an account on the login page." : "允许新用户在登录页自助注册账户（关闭后仅管理员可建号）。"}</small></div><label class="sw"><input type="checkbox" ${st.allow_registration !== false ? "checked" : ""} onchange="toggleSetting('allow_registration',this.checked)"><i></i></label></div>
    <div class="sect">Codex CLI</div><div class="note">${LANG === "en" ? "Point Codex base_url at" : "将 Codex 的 base_url 指向"} <code class="k">${origin}/v1</code>.<br>${LANG === "en" ? "Or one-shot auto-config:" : "或一键自动配置（KEY 换成下游 Key）："} <code class="k">curl -fsSL ${origin}/file/&lt;KEY&gt; | bash</code></div>
    <div class="sect">Claude Code</div><div class="note"><code class="k">ANTHROPIC_BASE_URL=${origin}</code><br><code class="k">ANTHROPIC_AUTH_TOKEN=&lt;any non-empty&gt;</code><br>${LANG === "en" ? "Native /v1/messages passthrough; also OpenAI /v1/chat/completions with a claude* model auto-converts." : "原生 /v1/messages 透传；也支持 /v1/chat/completions 且 model 以 claude 开头时自动转 Anthropic。"}</div>
    <div class="sect">${LANG === "en" ? "Custom providers (DeepSeek / SiliconFlow)" : "自定义供应商（DeepSeek / 硅基流动）"}</div><div class="note">${LANG === "en" ? "Manage providers + import keys on the Providers page. Set the downstream model to a provider model to route through it." : "在「供应商」页管理 base_url 与模型并导入 Key；下游把模型设为该供应商的模型即走此通道。"}</div>
    <div class="sect">${LANG === "en" ? "Telemetry off (downstream)" : "下游降噪 / 防遥测"}</div><div class="note"><code class="k">CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1</code> · <code class="k">DO_NOT_TRACK=1</code></div>
    <div class="sect">/v1/models</div><div>${models}</div></div></div>`;
}
function switchSettingTab(v) { _settingTab = v; loadSettings(); }
