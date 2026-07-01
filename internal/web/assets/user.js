/* user.js — end-user console pages. The Models page uses /v1/models (always available);
   the dashboard/keys/usage pages call /user/* (added in P3) and degrade to an empty
   state until then; My Settings drives theme/language locally and profile/password via
   /user/profile. */

async function loadMyDashboard() {
  $("#dashView").innerHTML = '<div class="viz kpis">' + Array.from({length:3}, () => UI.kpiSkeleton()).join('') + '</div>' +
    '<div class="panel"><div class="hd"><h2>' + t("me.usage_tokens") + '</h2></div><div class="bd chartwrap">' + UI.chartSkeleton() + '</div></div>' +
    '<div class="panel"><div class="hd"><h2>' + t("me.keys_title") + '</h2></div><div class="bd">' + UI.tableSkeleton(4, 2) + '</div></div>';
  const now = Math.floor(Date.now() / 1000);
  let usage = [], ts = { buckets: [] };
  try { usage = (await api("/user/usage")) || []; } catch {}
  try { ts = (await api(`/user/usage/timeseries?since=${now - 24 * 3600}&bucket=3600`)) || { buckets: [] }; } catch {}
  const buckets = ts.buckets || [];
  const total = usage.reduce((s, x) => s + (+x.total_tokens || 0), 0);
  const reqs = usage.reduce((s, x) => s + (+x.requests || 0), 0);
  const promptT = usage.reduce((s, x) => s + (+x.prompt_tokens || 0), 0);
  const cached = usage.reduce((s, x) => s + (+x.cached_tokens || 0), 0);
  const ratio = cached + promptT > 0 ? cached / (cached + promptT) : 0;
  const spark = buckets.map((b) => +b.total_tokens || 0);
  $("#dashView").innerHTML =
    `<div class="viz kpis">${kpiCard({ k: t("me.usage_tokens"), ic: "chart", v: fmt(total), sub: "", accent: true, spark, grad: "gradAcc", stroke: "var(--acc)" }) + kpiCard({ k: t("me.requests"), ic: "activity", v: fmt(reqs) }) + kpiCard({ k: LANG === "en" ? "Cache hit" : "缓存命中", ic: "zap", v: pct(ratio) })}</div>
    <div class="panel"><div class="hd"><h2>${t("me.usage_tokens")}</h2></div><div class="bd chartwrap">${Charts.stackArea(buckets, USAGE_SERIES, { xfmt: (b) => { const d = new Date(b.bucket * 1000); return String(d.getHours()).padStart(2, "0") + ":00"; }, empty: LANG === "en" ? "No usage yet — create a key and start calling the API." : "暂无用量 — 创建 Key 并调用 API 后显示。" })}</div></div>
    <div class="panel"><div class="hd"><h2>${t("me.keys_title")}</h2><span class="sp"></span><button class="btn pri sm" onclick="setView('mykeys')">${t("me.new_key")} ›</button></div><div class="bd" id="dashKeys"><div class="empty">${t("common.loading")}</div></div></div>`;
  try { const keys = (await api("/user/api-keys")) || []; $("#dashKeys").innerHTML = keys.length ? keysTableHTML(keys) : `<div class="empty">${t("me.no_keys")}</div>`; }
  catch { $("#dashKeys").innerHTML = `<div class="empty">${t("me.no_keys")}</div>`; }
  tickUntil();
}

function keysTableHTML(keys) {
  return `<table><thead><tr><th>${LANG === "en" ? "Label" : "标签"}</th><th>${LANG === "en" ? "Group" : "分组"}</th><th>force_model</th><th>${t("common.status")}</th><th>${LANG === "en" ? "Key / Install" : "Key / 安装"}</th><th></th></tr></thead><tbody>` +
    keys.map((k) => `<tr><td>${esc(k.label || "-")}</td><td class="muted">${esc(k.group_name || "cyber")}</td><td class="mono">${esc(k.force_model || "—")}</td><td>${k.enabled ? '<span class="chip ok">on</span>' : '<span class="chip warn">off</span>'}</td><td>${keyCopyCell(k)}</td>
      <td><div class="row"><button class="btn sm" onclick="myToggleKey('${k.key_hash}',${k.enabled ? "false" : "true"})">${k.enabled ? t("act.disable") : t("act.enable")}</button><button class="btn sm danger" onclick="myDeleteKey('${k.key_hash}')">${t("act.delete")}</button></div></td></tr>`).join("") + `</tbody></table>`;
}
// keyCopyCell now lives in core.js (shared with the admin Keys page).
async function loadMyKeys() {
  $("#mykeysView").innerHTML = '<div class="grid splitr"><div class="panel"><div class="hd"><h2>' + t("me.keys_title") + '</h2></div><div class="bd" style="padding:0">' + UI.tableSkeleton(4, 3) + '</div></div><div class="panel"><div class="hd"><h2>' + t("me.new_key") + '</h2></div><div class="bd">' + UI.skeleton("100%","120px") + '</div></div></div>';
  await refreshModels();
  $("#mykeysView").innerHTML = `<div class="grid splitr">
    <div class="panel"><div class="hd"><h2>${t("me.keys_title")}</h2></div><div class="bd" style="padding:0" id="myKeysTable"><div class="empty">${t("common.loading")}</div></div></div>
    <div class="panel"><div class="hd"><h2>${t("me.new_key")}</h2></div><div class="bd">
      <label class="f">${LANG === "en" ? "Label" : "标签"}</label><input class="t" id="mkLabel" placeholder="my-app">
      <label class="f">force_model <span class="muted">(${LANG === "en" ? "optional" : "可选"})</span></label>${modelSelectHTML("mkModel", "", {})}
      <div style="height:12px"></div><button class="btn pri" onclick="myCreateKey()">${t("act.create")}</button>
      <p class="muted" style="margin-top:10px">${t("me.key_once")}</p></div></div></div>`;
  try { const keys = (await api("/user/api-keys")) || []; $("#myKeysTable").innerHTML = keys.length ? keysTableHTML(keys) : `<div class="empty">${t("me.no_keys")}</div>`; }
  catch (e) { $("#myKeysTable").innerHTML = `<div class="empty">${esc(e.message)}</div>`; }
}
async function myCreateKey() { try { const r = await api("/user/api-keys", { method: "POST", body: JSON.stringify({ label: $("#mkLabel").value.trim(), force_model: $("#mkModel").value.trim() }) }); showSecretModal(r.key, { extra: keyUsageHint(r.key) }); $("#mkLabel").value = ""; const ms = $("#mkModel"); if (ms) ms.value = ""; toast(t("ok.saved"), "ok"); loadMyKeys(); } catch (e) { toast(e.message, "bad"); } }
async function myToggleKey(hash, on) { try { await api("/user/api-keys/" + hash, { method: "PATCH", body: JSON.stringify({ enabled: on }) }); loadMyKeys(); } catch (e) { toast(e.message, "bad"); } }
async function myDeleteKey(hash) { if (!confirm(LANG === "en" ? "Delete key?" : "删除该 Key？")) return; try { await api("/user/api-keys/" + hash, { method: "DELETE" }); toast(t("ok.deleted"), "ok"); loadMyKeys(); } catch (e) { toast(e.message, "bad"); } }

async function loadMyUsage() {
  const now = Math.floor(Date.now() / 1000);
  let usage = [], ts = { buckets: [] };
  try { usage = (await api("/user/usage")) || []; } catch {}
  try { ts = (await api(`/user/usage/timeseries?since=${now - 7 * 86400}&bucket=86400`)) || { buckets: [] }; } catch {}
  const buckets = ts.buckets || [];
  const byModel = {}; usage.forEach((u) => { const m = u.model || "—"; if (!byModel[m]) byModel[m] = { model: m, requests: 0, total: 0 }; byModel[m].requests += +u.requests || 0; byModel[m].total += +u.total_tokens || 0; });
  const rows = Object.values(byModel).sort((a, b) => b.total - a.total);
  $("#myusageView").innerHTML = `<div class="panel"><div class="hd"><h2>${t("nav.myusage")}</h2></div><div class="bd chartwrap">${Charts.stackArea(buckets, USAGE_SERIES, { xfmt: (b) => { const d = new Date(b.bucket * 1000); return d.getMonth() + 1 + "/" + d.getDate(); }, empty: LANG === "en" ? "No usage yet" : "暂无用量" })}</div></div>
    <div class="panel"><div class="hd"><h2>${LANG === "en" ? "By model" : "按模型"}</h2></div><div class="bd" style="padding:0">` +
    (rows.length ? `<table><thead><tr><th>${t("common.model")}</th><th>${LANG === "en" ? "Requests" : "请求"}</th><th>Token</th></tr></thead><tbody>` + rows.map((r) => `<tr><td class="mono">${esc(r.model)}</td><td>${r.requests}</td><td class="mono">${fmt(r.total)}</td></tr>`).join("") + `</tbody></table>` : `<div class="empty">${LANG === "en" ? "No usage yet" : "暂无用量"}</div>`) + `</div></div>`;
}

async function loadModelsPage() {
  $("#modelsView").innerHTML = '<div class="panel"><div class="hd"><h2>' + t("me.models_title") + '</h2></div><div class="bd"><div class="modelcards">' + Array.from({length:6}, () => '<div class="modelcard">' + UI.skeleton("120px","14px") + '<div style="height:4px"></div>' + UI.skeleton("60px","11px",true) + '</div>').join('') + '</div></div></div>';
  let models = [];
  try { const m = await api("/v1/models"); models = m.data || []; } catch (e) { $("#modelsView").innerHTML = `<div class="empty">${esc(e.message)}</div>`; return; }
  $("#modelsView").innerHTML = `<div class="panel"><div class="hd"><h2>${t("me.models_title")}</h2><span class="sp"></span><span class="muted">${models.length}</span></div><div class="bd">` +
    (models.length ? `<div class="modelcards">` + models.map((m) => `<div class="modelcard"><div class="mname">${esc(m.id)}</div><div class="mtag">${m.window_mode ? `<span class="chip">${esc(m.window_mode)}</span>` : ""}${m.owned_by ? `<span class="chip muted">${esc(m.owned_by)}</span>` : ""}</div></div>`).join("") + `</div>` : `<div class="empty">${LANG === "en" ? "No models" : "暂无模型"}</div>`) + `</div></div>`;
}

async function loadMySettings() {
  const origin = location.origin;
  $("#mysettingsView").innerHTML = `<div class="panel" style="max-width:640px"><div class="hd"><h2>${t("me.profile")}</h2></div><div class="bd">
    <div class="kv"><div class="key">${t("auth.email")}</div><div class="mono">${esc((ME && ME.email) || "—")}</div><div class="key">${LANG === "en" ? "Role" : "角色"}</div><div><span class="chip ${isAdmin() ? "acc" : ""}">${esc((ME && ME.role) || "user")}</span></div></div>
    <label class="f">${t("me.display_name")}</label><input class="t" id="profName" value="${esc((ME && ME.name) || "")}">
    <div style="height:8px"></div><button class="btn" onclick="saveProfile()">${t("act.save")}</button>
    <div class="sect">${t("me.change_pw")}</div>
    <label class="f">${t("me.old_pw")}</label><input class="t" id="pwOld" type="password">
    <label class="f">${t("me.new_pw")}</label><input class="t" id="pwNew" type="password">
    <div style="height:8px"></div><button class="btn" onclick="changePassword()">${t("me.change_pw")}</button>
    <div class="sect">${LANG === "en" ? "Appearance" : "外观"}</div>
    <div class="row" style="align-items:center;gap:10px"><span class="muted">${t("common.theme")}</span><div class="seg"><button class="${THEME === "light" ? "on" : ""}" onclick="setThemeExplicit('light')">${LANG === "en" ? "Light" : "浅色"}</button><button class="${THEME === "dark" ? "on" : ""}" onclick="setThemeExplicit('dark')">${LANG === "en" ? "Dark" : "深色"}</button></div>
      <span class="muted" style="margin-left:14px">${t("common.lang")}</span><div class="seg"><button class="${LANG === "zh" ? "on" : ""}" onclick="setLang('zh')">中文</button><button class="${LANG === "en" ? "on" : ""}" onclick="setLang('en')">EN</button></div></div>
    <div class="row" style="align-items:center;gap:10px;margin-top:8px;flex-wrap:wrap">
      <span class="muted" style="font-size:12px">${LANG === "en" ? "Preset" : "预设主题"}</span>
      <div class="seg" id="presetSeg">${["default","anthropic","ocean","forest","rose","sunset","midnight","lavender"].map((p) => `<button data-p="${p}" class="${(localStorage.getItem("cp_theme_preset")||"")===p?"on":""}" onclick="setThemePreset('${p}')">${p[0].toUpperCase()+p.slice(1)}</button>`).join("")}</div>
    </div>
    <div class="row" style="align-items:center;gap:10px;margin-top:8px;flex-wrap:wrap">
      <span class="muted" style="font-size:12px">${LANG === "en" ? "Density" : "密度"}</span>
      <div class="seg" id="densitySeg">${["compact","default","comfortable","spacious"].map((d) => `<button data-d="${d}" class="${(localStorage.getItem("cp_density")||"default")===d?"on":""}" onclick="setDensity('${d}')">${d[0].toUpperCase()+d.slice(1)}</button>`).join("")}</div>
    </div>
    <div class="row" style="align-items:center;gap:10px;margin-top:8px;flex-wrap:wrap">
      <span class="muted" style="font-size:12px">${LANG === "en" ? "Radius" : "圆角"}</span>
      <div class="seg" id="radiusSeg">${["sharp","default","round","pill"].map((r) => `<button data-r="${r}" class="${(localStorage.getItem("cp_radius")||"default")===r?"on":""}" onclick="setRadius('${r}')">${r[0].toUpperCase()+r.slice(1)}</button>`).join("")}</div>
    </div>
    <div class="row" style="align-items:center;gap:10px;margin-top:8px;flex-wrap:wrap">
      <span class="muted" style="font-size:12px">${LANG === "en" ? "Font" : "字体"}</span>
      <div class="seg" id="fontSeg">${[{v:"sans",l:"Sans"},{v:"serif",l:"Serif"}].map((f) => `<button data-f="${f.v}" class="${(localStorage.getItem("cp_font")||"sans")===f.v?"on":""}" onclick="setAppFont('${f.v}')">${f.l}</button>`).join("")}</div>
    </div>
    <div class="sect">${t("me.endpoints")}</div>
    <div class="note">OpenAI: <code class="k">${origin}/v1</code><br>Anthropic: <code class="k">${origin}</code><br>${LANG === "en" ? "Use one of your keys above as the API key." : "用上面任意一个 Key 作为 API Key。"}</div>
    <div class="sect">${LANG === "en" ? "One-shot Codex setup" : "一键配置 Codex"}</div>
    <div class="note">${LANG === "en" ? "Run once on the client (replace with your key):" : "在客户端执行一次（把 KEY 换成你的 Key）："}<br><code class="k">curl -fsSL ${origin}/file/&lt;YOUR_KEY&gt; | bash</code></div></div></div>`;
}

function setThemePreset(p) {
  if (p === "default") { localStorage.removeItem("cp_theme_preset"); document.documentElement.removeAttribute("data-theme-preset"); }
  else { localStorage.setItem("cp_theme_preset", p); document.documentElement.setAttribute("data-theme-preset", p); }
  loadMySettings();
}
function setDensity(d) {
  if (d === "default") { localStorage.removeItem("cp_density"); document.documentElement.removeAttribute("data-density"); }
  else { localStorage.setItem("cp_density", d); document.documentElement.setAttribute("data-density", d); }
  loadMySettings();
}
function setRadius(r) {
  if (r === "default") { localStorage.removeItem("cp_radius"); document.documentElement.removeAttribute("data-radius"); }
  else { localStorage.setItem("cp_radius", r); document.documentElement.setAttribute("data-radius", r); }
  loadMySettings();
}
function setAppFont(f) {
  if (f === "sans") { localStorage.removeItem("cp_font"); document.documentElement.removeAttribute("data-font"); }
  else { localStorage.setItem("cp_font", f); document.documentElement.setAttribute("data-font", f); }
  loadMySettings();
}

function setThemeExplicit(m) { if (THEME !== m) toggleTheme(); loadMySettings(); }
async function saveProfile() { try { const r = await api("/user/profile", { method: "PATCH", body: JSON.stringify({ name: $("#profName").value.trim() }) }); if (ME) ME.name = (r && r.name) || $("#profName").value.trim(); renderShell(); toast(t("ok.saved"), "ok"); } catch (e) { toast(e.message, "bad"); } }
async function changePassword() { const oldp = $("#pwOld").value, np = $("#pwNew").value; if (!np) { toast(LANG === "en" ? "Enter a new password" : "请输入新密码", "bad"); return; } try { await api("/user/profile", { method: "PATCH", body: JSON.stringify({ old_password: oldp, new_password: np }) }); $("#pwOld").value = ""; $("#pwNew").value = ""; toast(t("ok.saved"), "ok"); } catch (e) { toast(e.message, "bad"); } }
