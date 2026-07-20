/* admin-integrations.js — admin console pages for custom providers, account import
   (OAuth / token / cookie / API-key), and moderation/sensitive-words. Split out of
   admin.js (no behavior change); all functions stay global and are invoked by name
   from adminapp.js's loadView. Loaded after admin.js, before adminapp.js. */
/* providers page */
let PROV_EDIT = {};
async function loadProvidersPage() {
  $("#providersView").innerHTML = `<div class="panel" style="max-width:920px"><div class="hd"><h2>${t("settings.providers_title")}</h2><span class="sp"></span><button class="btn" onclick="addProvider()">${icon("plus")} ${LANG === "en" ? "Add provider" : "新增供应商"}</button></div><div class="bd">
    <div class="note">${LANG === "en" ? "OpenAI Chat-Completions compatible upstreams (DeepSeek, SiliconFlow, Kimi, OpenRouter, local vLLM). Models can be auto-discovered from {base_url}/models. Import a key on the Accounts page (自定义 · API Key)." : "为 OpenAI Chat-Completions 兼容上游配置 base_url 与模型（可自动发现 {base_url}/models）。在「账号」页用 自定义 · API Key 导入其 Key。"}</div>
    <div id="presets" class="row" style="margin:4px 0 10px"></div><div id="providersBox" class="note">${t("common.loading")}</div></div></div>`;
  renderPresets(); renderProviders();
}
const PROVIDER_PRESETS = [
  { id: "deepseek", name: "DeepSeek", base_url: "https://api.deepseek.com/v1", models: ["deepseek-chat", "deepseek-reasoner"] },
  { id: "siliconflow", name: "SiliconFlow 硅基流动", base_url: "https://api.siliconflow.cn/v1", models: [] },
  { id: "kimi", name: "Kimi (Moonshot)", base_url: "https://api.moonshot.cn/v1", models: ["moonshot-v1-8k", "moonshot-v1-32k"] },
  { id: "openrouter", name: "OpenRouter", base_url: "https://openrouter.ai/api/v1", models: [] },
];
function renderPresets() {
  $("#presets").innerHTML = `<span class="muted" style="margin-right:4px">${LANG === "en" ? "Presets:" : "快速添加："}</span>` +
    PROVIDER_PRESETS.map((p, i) => `<button class="btn sm" onclick="applyPreset(${i})">${esc(p.name)}</button>`).join("");
}
function applyPreset(i) { const p = PROVIDER_PRESETS[i]; PROV_EDIT[""] = (p.models || []).slice(); renderProviders(); setTimeout(() => { const idEl = $("#pv_id_"); if (idEl) idEl.value = p.id; const nm = $(`#pv_name_`); if (nm) nm.value = p.name; const bu = $(`#pv_base_`); if (bu) bu.value = p.base_url; }, 0); }
async function renderProviders() {
  let ps = []; try { ps = (await api("/admin/providers")) || []; } catch (e) { $("#providersBox").textContent = e.message; return; }
  PROVIDERS = ps; const box = $("#providersBox");
  if (!ps.length && PROV_EDIT[""] === undefined) { box.innerHTML = `<div class="muted">${LANG === "en" ? "No providers." : "暂无供应商。"}</div>`; return; }
  const cards = ps.map((p) => providerCard(p));
  if (PROV_EDIT[""] !== undefined) cards.unshift(providerCard({ id: "", name: "", base_url: "", enabled: true, auto_discover_models: true, models: PROV_EDIT[""] || [] }, true));
  box.innerHTML = cards.join("");
}
function providerModels(p, isNew) { const key = isNew ? "" : p.id; let list = PROV_EDIT[key]; if (list === undefined) list = (p.models || []).slice(); return list; }
function providerCard(p, isNew) {
  const key = isNew ? "" : p.id; const models = providerModels(p, isNew);
  const rows = models.map((m, i) => `<div class="row" style="gap:6px;margin:3px 0"><input class="t mono" style="flex:1" value="${esc(m)}" oninput="provModelEdit('${esc(key)}',${i},this.value)"><button class="btn sm" onclick="provModelDel('${esc(key)}',${i})">${icon("x")}</button></div>`).join("");
  const idField = isNew ? `<input class="t mono" id="pv_id_" placeholder="deepseek" style="flex:1">` : `<span class="mono">${esc(p.id)}</span>`;
  return `<div class="panel" style="margin:8px 0" data-pv="${esc(key)}"><div class="bd">
    <div class="row" style="gap:10px;flex-wrap:wrap;align-items:flex-end"><div><label class="f">ID</label><div class="row" style="gap:6px">${idField}</div></div><div style="flex:1"><label class="f">${LANG === "en" ? "Name" : "名称"}</label><input class="t" id="pv_name_${esc(key)}" value="${esc(p.name || "")}" placeholder="DeepSeek"></div></div>
    <label class="f">Base URL (incl /v1)</label><input class="t mono" id="pv_base_${esc(key)}" value="${esc(p.base_url || "")}" placeholder="https://api.deepseek.com/v1">
    <div class="row" style="gap:18px;margin:8px 0"><label class="f" style="margin:0"><input type="checkbox" id="pv_en_${esc(key)}" ${p.enabled !== false ? "checked" : ""}> ${LANG === "en" ? "Enabled" : "启用"}</label><label class="f" style="margin:0"><input type="checkbox" id="pv_auto_${esc(key)}" ${p.auto_discover_models !== false ? "checked" : ""}> ${LANG === "en" ? "Auto-discover models" : "自动发现模型"}</label></div>
    <label class="f">${LANG === "en" ? "Models (auto-filled on probe; editable)" : "模型列表（探测后自动填充，可增删）"}</label><div id="pv_models_${esc(key)}">${rows || `<div class="muted" style="margin:2px 0">${LANG === "en" ? "(none yet)" : "（暂无）"}</div>`}</div>
    <div class="row" style="gap:6px;margin-top:6px"><button class="btn sm" onclick="provModelAdd('${esc(key)}')">${icon("plus")} ${LANG === "en" ? "Model" : "模型"}</button></div>
    <div class="row" style="gap:6px;margin-top:12px"><button class="btn pri" onclick="saveProvider('${esc(key)}',${isNew ? true : false})">${t("act.save")}</button>
      ${isNew ? `<button class="btn" onclick="cancelNewProvider()">${t("act.cancel")}</button>` : `<button class="btn" onclick="probeProvider('${esc(p.id)}')">${LANG === "en" ? "Probe" : "探测模型"}</button><button class="btn bad" onclick="delProvider('${esc(p.id)}')">${t("act.delete")}</button>`}</div></div></div>`;
}
function cssEsc(s) { return (s == null ? "" : String(s)).replace(/[^a-zA-Z0-9_-]/g, "\\$&"); }
function syncProvEdit(key) { const cont = $(`#pv_models_${cssEsc(key)}`); if (!cont) return; PROV_EDIT[key] = [...cont.querySelectorAll("input")].map((i) => i.value); }
function provModelEdit(key, i, v) { if (PROV_EDIT[key] === undefined) PROV_EDIT[key] = []; PROV_EDIT[key][i] = v; }
function provModelAdd(key) { syncProvEdit(key); if (PROV_EDIT[key] === undefined) PROV_EDIT[key] = []; PROV_EDIT[key].push(""); renderProviders(); }
function provModelDel(key, i) { syncProvEdit(key); if (PROV_EDIT[key]) PROV_EDIT[key].splice(i, 1); renderProviders(); }
function addProvider() { PROV_EDIT[""] = []; renderProviders(); }
function cancelNewProvider() { delete PROV_EDIT[""]; renderProviders(); }
async function saveProvider(key, isNew) {
  syncProvEdit(key);
  const id = isNew ? $("#pv_id_").value.trim() : key;
  const models = (PROV_EDIT[key] || []).map((s) => s.trim()).filter(Boolean);
  const body = { id, name: $(`#pv_name_${cssEsc(key)}`).value.trim(), base_url: $(`#pv_base_${cssEsc(key)}`).value.trim(), enabled: $(`#pv_en_${cssEsc(key)}`).checked, auto_discover_models: $(`#pv_auto_${cssEsc(key)}`).checked, models };
  if (!body.id && !body.name) { toast(LANG === "en" ? "Provider id or name required" : "请填写供应商 ID 或名称", "bad"); return; }
  try { await api("/admin/providers", { method: "POST", body: JSON.stringify(body) }); delete PROV_EDIT[key]; if (isNew) delete PROV_EDIT[""]; toast(t("ok.saved"), "ok"); renderProviders(); refreshModels(); } catch (e) { toast(e.message, "bad"); }
}
async function delProvider(id) { if (!confirm((LANG === "en" ? "Delete provider " : "删除供应商 ") + id + "?")) return; try { await api("/admin/providers/" + encodeURIComponent(id), { method: "DELETE" }); delete PROV_EDIT[id]; toast(t("ok.deleted"), "ok"); renderProviders(); refreshModels(); } catch (e) { toast(e.message, "bad"); } }
async function probeProvider(id) { try { const accs = (await api("/admin/accounts")) || []; const a = accs.find((x) => x.provider === id); if (!a) { toast(LANG === "en" ? "Import a key for this provider first" : "请先用「自定义 · API Key」导入该供应商账号", "bad"); return; } toast(LANG === "en" ? "Probing…" : "探测中…"); await api(`/admin/accounts/${a.id}/probe-models`, { method: "POST" }); delete PROV_EDIT[id]; toast(t("ok.saved"), "ok"); renderProviders(); refreshModels(); } catch (e) { toast(e.message, "bad"); } }

/* import modal + oauth */
let impMode = "oauth_codex", oauthSession = null;
function openImport() { $("#importMask").classList.remove("hide"); setImp("oauth_codex"); }
function closeImport() { $("#importMask").classList.add("hide"); }
function setImp(m) {
  impMode = m; $$("#impTabs button").forEach((x) => x.classList.toggle("on", x.dataset.m === m));
  $$("#importMask [data-m]").forEach((x) => { if (x.tagName === "DIV") x.classList.toggle("hide", x.dataset.m !== m); });
  const isOauth = m === "oauth_codex" || m === "oauth_claude";
  const isProviderKey = m === "codex_key" || m === "claude_key";
  $("#oauthPanel").classList.toggle("hide", !isOauth);
  $("#impSubmitBtn").classList.toggle("hide", isOauth);
  $("#impSubmitBtn").textContent = isProviderKey ? (LANG === "en" ? "Import and run two-stage probe" : "导入并执行双层测活") : (LANG === "en" ? "Import" : "导入 / Import");
  $("#providerKeyResult").classList.add("hide"); $("#providerKeyResult").innerHTML = "";
  if (isOauth) resetOauth();
  if (m === "customkey") fillProviderSelect();
}
function resetOauth() { oauthSession = null; $("#oauthStep2").classList.add("hide"); $("#oauthStatus").textContent = ""; $("#oauthUrl").value = ""; $("#oauthPaste").value = ""; $("#oauthGenBtn").disabled = false; }
async function oauthStart() {
  const provider = impMode === "oauth_claude" ? "claude" : "codex"; $("#oauthGenBtn").disabled = true; $("#oauthStatus").textContent = "…";
  try { const r = await api("/admin/oauth/start", { method: "POST", body: JSON.stringify({ provider }) }); oauthSession = r.session_id; $("#oauthUrl").value = r.auth_url; $("#oauthOpen").href = r.auth_url; $("#oauthStep2").classList.remove("hide"); $("#oauthStatus").textContent = "~" + Math.round((r.expires_in || 900) / 60) + "min"; } catch (e) { toast(e.message, "bad"); $("#oauthStatus").textContent = ""; }
  $("#oauthGenBtn").disabled = false;
}
function oauthCopy() { const el = $("#oauthUrl"); if (!el.value) return; if (navigator.clipboard && window.isSecureContext) navigator.clipboard.writeText(el.value).then(() => toast(t("ok.copied"), "ok"), () => fallbackCopy(el)); else fallbackCopy(el); }
function fallbackCopy(el) { el.focus(); el.select(); try { document.execCommand("copy") ? toast(t("ok.copied"), "ok") : toast("Ctrl/⌘+C", ""); } catch { toast("Ctrl/⌘+C", ""); } }
async function oauthComplete() {
  if (!oauthSession) { toast(LANG === "en" ? "Generate the link first" : "请先生成登录链接", "bad"); return; }
  const redirected = $("#oauthPaste").value.trim(); if (!redirected) { toast(LANG === "en" ? "Paste the URL/code" : "请粘贴网址或授权码", "bad"); return; }
  try { const acc = await api("/admin/oauth/complete", { method: "POST", body: JSON.stringify({ session_id: oauthSession, redirected, label: $("#imp_label").value.trim(), group_name: $("#imp_group").value.trim() }) }); toast("✓ " + (acc.label || acc.email || acc.id), "ok"); closeImport(); loadAccounts(); } catch (e) { toast(e.message, "bad"); }
}
async function fillProviderSelect() { try { PROVIDERS = (await api("/admin/providers")) || []; } catch { PROVIDERS = []; } const sel = $("#ck_provider"); if (!sel) return; const en = PROVIDERS.filter((p) => p.enabled !== false); sel.innerHTML = en.length ? en.map((p) => `<option value="${esc(p.id)}">${esc(p.name || p.id)}</option>`).join("") : `<option value="">${LANG === "en" ? "(add a provider first)" : "（请先新增供应商）"}</option>`; }
function renderProviderKeyResult(acc) {
  const auth = acc.auth_probe || {}, inference = acc.inference_probe || {};
  const result = $("#providerKeyResult"); if (!result) return;
  const state = acc.ready ? (LANG === "en" ? "Ready" : "已就绪")
    : acc.quarantined ? (LANG === "en" ? "Saved and quarantined" : "已保存并隔离")
      : (LANG === "en" ? "Authentication failed; not saved" : "认证失败，未保存");
  result.classList.remove("hide");
  result.innerHTML = `<b>${state}</b><br>`
    + `${LANG === "en" ? "Free auth probe" : "免费认证探针"}: ${auth.alive ? "✓" : "✕"} ${esc(auth.state || "unknown")} · HTTP ${esc(auth.http_status || "—")}<br>`
    + `${LANG === "en" ? "Minimal inference probe" : "最小推理探针"}: ${inference.alive ? "✓" : inference.checked ? "✕" : "—"} ${esc(inference.state || "unknown")}${inference.model ? " · " + esc(inference.model) : ""}`
    + (acc.quarantine_reason ? `<br>${LANG === "en" ? "Quarantine reason" : "隔离原因"}: ${esc(acc.quarantine_reason)}` : "");
}
async function doImport() {
  const label = $("#imp_label").value.trim(), group = $("#imp_group").value.trim();
  try {
    let ep, body;
    if (impMode === "token") { ep = "/admin/accounts/import-token"; body = { access_token: $("#tk_at").value.trim(), account_id: $("#tk_acc").value.trim(), label, group_name: group }; }
    else if (impMode === "claude") { ep = "/admin/accounts/import-token"; body = { access_token: $("#cl_at").value.trim(), label, group_name: group }; }
    else if (impMode === "codex_key" || impMode === "claude_key") {
      const codex = impMode === "codex_key";
      const key = $(codex ? "#codex_api_key" : "#claude_api_key").value.trim();
      const confirmed = $(codex ? "#codex_confirm_cost" : "#claude_confirm_cost").checked;
      if (!key) { toast(LANG === "en" ? "Enter the upstream API key" : "请填写上游 API Key", "bad"); return; }
      if (!confirmed) { toast(LANG === "en" ? "Confirm the possible inference cost" : "请确认最小推理可能产生费用", "bad"); return; }
      ep = "/admin/accounts/import-key"; body = { provider_id: codex ? "codex" : "claude", api_key: key, label, group_name: group, confirm_cost: true };
    }
    else if (impMode === "customkey") { ep = "/admin/accounts/import-key"; body = { provider_id: $("#ck_provider").value, api_key: $("#ck_apikey").value.trim(), label, group_name: group }; if (!body.provider_id) { toast(LANG === "en" ? "Pick a provider" : "请选择供应商", "bad"); return; } if (!body.api_key) { toast(LANG === "en" ? "Enter API key" : "请填写 API Key", "bad"); return; } }
    else if (impMode === "cookie") { ep = "/admin/accounts/import-cookie"; body = { cookie_header: $("#ck_hdr").value.trim(), label, group_name: group }; }
    else { ep = "/admin/accounts/import-auth-json"; body = { auth_json_text: $("#aj_txt").value, label, group_name: group }; }
    const acc = await api(ep, { method: "POST", body: JSON.stringify(body) });
    if (impMode === "codex_key" || impMode === "claude_key") {
      renderProviderKeyResult(acc);
      toast(acc.ready ? "✓ " + (acc.label || acc.id) : (LANG === "en" ? "Authentication passed; inference failed and the account is quarantined" : "认证通过，但推理失败；账号已隔离"), acc.ready ? "ok" : "bad");
      loadAccounts(); return;
    }
    toast("✓ " + (acc.label || acc.id), "ok"); closeImport(); loadAccounts();
  } catch (e) { const result = $("#providerKeyResult"); if (result && (impMode === "codex_key" || impMode === "claude_key")) { if (e.data && e.data.auth_probe) renderProviderKeyResult(e.data); else { result.classList.remove("hide"); result.textContent = e.message; } } toast(e.message, "bad"); }
}

/* ===== Moderation ===== */
let MOD_WORDS = [], MOD_CFG = {};
function hasCJK(s) { return /[぀-ヿ㐀-䶿一-鿿豈-﫿]/.test(s || ""); }
async function loadModeration() {
  const box = $("#modView");
  box.innerHTML = `<div class="empty">${t("common.loading")}</div>`;
  try { MOD_CFG = await api("/admin/moderation"); }
  catch (e) { box.innerHTML = `<div class="panel"><div class="bd" style="color:var(--bad)">${esc(e.message)}</div></div>`; return; }
  MOD_WORDS = MOD_CFG.words || [];
  if (!MODELS.length) { try { const m = await api("/v1/models"); MODELS = (m.data || []).map((x) => x.id); } catch {} }
  renderModeration();
}
// capture unsaved control state so word add/delete re-renders don't lose it
function syncModState() {
  if ($("#mod_enabled")) MOD_CFG.enabled = $("#mod_enabled").checked;
  if ($("#mod_auto_translate")) MOD_CFG.auto_translate = $("#mod_auto_translate").checked;
  if ($("#mod_model")) MOD_CFG.model = $("#mod_model").value;
}
function renderModeration() {
  const cfg = MOD_CFG;
  const modelOpts = MODELS.map((m) => `<option value="${esc(m)}">${esc(m)}</option>`).join("");
  const wordRows = MOD_WORDS.map((w, i) => `<div class="row" style="gap:6px;flex-wrap:nowrap"><input class="t" value="${esc(w)}" onchange="updateModWord(${i}, this.value)"><button class="btn sm bad" onclick="delModWord(${i})">${icon("x")}</button></div>`).join("");
  const sw = (id, on, title, desc) => `<div class="swrow"><div class="lbl"><b>${title}</b><small>${desc}</small></div><label class="sw"><input type="checkbox" id="${id}" ${on ? "checked" : ""}><i></i></label></div>`;
  $("#modView").innerHTML = `<div class="grid splitr rev">
    <div class="panel"><div class="hd"><h2>${LANG === "en" ? "Content moderation" : "内容审查"}</h2><span class="sp"></span>${cfg.enabled ? '<span class="chip ok">on</span>' : '<span class="chip warn">off</span>'}</div><div class="bd">
      ${sw("mod_enabled", !!cfg.enabled, LANG === "en" ? "Enable moderation" : "启用内容审查", LANG === "en" ? "Rewrites prior assistant turns containing configured words before forwarding upstream. Live streamed replies are never touched." : "检测到配置的关键词时，在转发前改写历史对话（助手回复）。流式输出的实时回复永不处理。")}
      ${sw("mod_auto_translate", !!cfg.auto_translate, LANG === "en" ? "Auto-translate (CN→EN)" : "自动翻译（中→英）", LANG === "en" ? "When adding a Chinese word, auto-append its English translation." : "添加中文词时自动追加英文翻译。")}
      <label class="f">${LANG === "en" ? "Rewrite model" : "执行改写的池内模型"}</label>
      <select class="t" id="mod_model"><option value="">—</option>${modelOpts}</select>
      <div style="height:14px"></div>
      <button class="btn pri" onclick="saveModeration()">${icon("check")} ${t("act.save")}</button>
    </div></div>
    <div class="panel"><div class="hd"><h2>${LANG === "en" ? "Detection words" : "检测词列表"}</h2><span class="sp"></span><span class="muted">${MOD_WORDS.length}</span><button class="btn sm" onclick="addModWord()">${icon("plus")} ${t("act.add")}</button></div><div class="bd" style="display:flex;flex-direction:column;gap:8px">
      ${wordRows || `<div class="empty">${LANG === "en" ? "No words configured" : "暂无配置词"}</div>`}
    </div></div>
  </div>`;
  const mm = $("#mod_model"); if (mm) mm.value = cfg.model || "";
}
function updateModWord(i, v) { MOD_WORDS[i] = v.trim(); }
function delModWord(i) { syncModState(); MOD_WORDS.splice(i, 1); renderModeration(); }
async function addModWord() {
  const word = prompt(LANG === "en" ? "Enter detection word/phrase:" : "输入检测词或短语:");
  if (!word || !word.trim()) return;
  syncModState();
  const w = word.trim();
  MOD_WORDS.push(w);
  if (MOD_CFG.auto_translate && hasCJK(w)) {
    try {
      const model = MOD_CFG.model || "";
      if (model) {
        const r = await api("/admin/moderation/translate", { method: "POST", body: JSON.stringify({ word: w, model }) });
        if (r.translations && r.translations.length > 0) {
          r.translations.forEach((en) => { if (en && !MOD_WORDS.includes(en)) MOD_WORDS.push(en); });
        }
      }
    } catch {}
  }
  renderModeration();
}
async function saveModeration() {
  syncModState();
  const body = {
    enabled: !!MOD_CFG.enabled,
    model: MOD_CFG.model || "",
    auto_translate: !!MOD_CFG.auto_translate,
    words: MOD_WORDS.filter((w) => w.trim() !== ""),
  };
  if (body.enabled && !body.model) {
    toast(LANG === "en" ? "Pick a model" : "请选择模型", "bad");
    return;
  }
  try {
    await api("/admin/moderation", { method: "POST", body: JSON.stringify(body) });
    toast(t("ok.saved"), "ok");
    loadModeration();
  } catch (e) {
    toast(e.message, "bad");
  }
}
