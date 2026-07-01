/* charts.js — zero-dependency inline-SVG charts + KPI/quota helpers, reused as-is by
   both consoles. Depends on esc()/fmt()/$$() from core.js + icon() from icons.js (loaded first). */
const Charts = {
  spark(vals, o = {}) {
    const w = o.w || 260, h = o.h || 34, grad = o.grad || "gradAcc", stroke = o.stroke || "var(--acc)";
    vals = (vals || []).map((v) => +v || 0);
    if (!vals.length) return "";
    if (vals.length === 1) vals = [vals[0], vals[0]];
    const max = Math.max(...vals), min = Math.min(...vals, 0), span = (max - min) || 1, n = vals.length;
    const X = (i) => (i / (n - 1)) * w, Y = (v) => h - 2 - ((v - min) / span) * (h - 4);
    let d = "M" + X(0).toFixed(1) + " " + Y(vals[0]).toFixed(1);
    for (let i = 1; i < n; i++) d += " L" + X(i).toFixed(1) + " " + Y(vals[i]).toFixed(1);
    return `<svg class="spark" viewBox="0 0 ${w} ${h}" preserveAspectRatio="none">
      <path d="${d} L${w} ${h} L0 ${h} Z" fill="url(#${grad})"/>
      <path d="${d}" fill="none" stroke="${stroke}" stroke-width="1.6" stroke-linejoin="round"/></svg>`;
  },
  stackArea(buckets, series, o = {}) {
    const w = o.w || 760, h = o.h || 220, padL = 42, padB = 22, padT = 12, padR = 10;
    o.id = o.id || "cpChart" + Math.random().toString(36).slice(2,8);
    if (!buckets || !buckets.length) return '<div class="empty">' + (o.empty || "暂无数据") + "</div>";
    const n = buckets.length, iw = w - padL - padR, ih = h - padT - padB;
    let max = 0; const stacks = buckets.map((b) => { let s = 0; series.forEach((se) => (s += +b[se.key] || 0)); if (s > max) max = s; return s; });
    max = max || 1;
    const X = (i) => padL + (n === 1 ? iw / 2 : (i / (n - 1)) * iw), Y = (v) => padT + ih - (v / max) * ih;
    let grid = "";
    for (let g = 0; g <= 4; g++) { const yv = (max * g) / 4, y = Y(yv);
      grid += `<line class="gridln" x1="${padL}" y1="${y.toFixed(1)}" x2="${w - padR}" y2="${y.toFixed(1)}"/>`;
      grid += `<text class="axis" x="${padL - 5}" y="${(y + 3).toFixed(1)}" text-anchor="end">${fmt(yv)}</text>`; }
    let layers = "", cum = new Array(n).fill(0);
    series.forEach((se) => {
      const top = buckets.map((b, i) => cum[i] + (+b[se.key] || 0)), bot = cum.slice();
      let d = "M" + X(0).toFixed(1) + " " + Y(top[0]).toFixed(1);
      for (let i = 1; i < n; i++) d += " L" + X(i).toFixed(1) + " " + Y(top[i]).toFixed(1);
      for (let i = n - 1; i >= 0; i--) d += " L" + X(i).toFixed(1) + " " + Y(bot[i]).toFixed(1);
      layers += `<path d="${d} Z" fill="${se.fill || se.color}" opacity="${se.opacity || 0.9}" stroke="${se.color}" stroke-width="1.1"/>`;
      cum = top;
    });
    let dots = ""; buckets.forEach((b, i) => { 
      const cx = X(i).toFixed(1), cy = Y(stacks[i]).toFixed(1);
      dots += `<circle cx="${cx}" cy="${cy}" r="5" fill="transparent" class="cp-chart-dot" data-idx="${i}" data-val="${fmt(stacks[i])}"><title>${esc(o.xfmt ? o.xfmt(b) : "")} · ${fmt(stacks[i])}</title></circle>`;
    });
    // Hover tooltip column indicator
    let tooltipCol = `<rect id="${o.id}_tip" x="0" y="${padT}" width="${w}" height="${ih}" fill="rgba(125,140,170,.04)" style="display:none;pointer-events:none"/>
      <line id="${o.id}_line" x1="0" y1="${padT}" x2="0" y2="${h-padB}" stroke="var(--muted)" stroke-width="1" stroke-dasharray="3,3" style="display:none;pointer-events:none"/>`;
    let xl = ""; const idxs = n > 2 ? [0, Math.floor((n - 1) / 2), n - 1] : buckets.map((_, i) => i);
    idxs.forEach((i) => { const anc = i === 0 ? "start" : i === n - 1 ? "end" : "middle"; xl += `<text class="axis" x="${X(i).toFixed(1)}" y="${h - 6}" text-anchor="${anc}">${esc(o.xfmt ? o.xfmt(buckets[i]) : "")}</text>`; });
    return `<svg viewBox="0 0 ${w} ${h}" width="100%" height="${h}" class="cp-interactive-chart" data-chart-id="${o.id}">${grid}${layers}${dots}${tooltipCol}${xl}</svg>`;
  },
  donut(segs, o = {}) {
    const size = o.size || 130, thick = o.thick || 18, r = (size - thick) / 2, c = 2 * Math.PI * r, cx = size / 2;
    segs = (segs || []).filter((s) => (+s.value || 0) > 0);
    const total = segs.reduce((a, s) => a + (+s.value || 0), 0) || 1;
    let off = 0, arcs = "";
    segs.forEach((s) => { const len = ((+s.value || 0) / total) * c;
      arcs += `<circle cx="${cx}" cy="${cx}" r="${r}" fill="none" stroke="${s.color}" stroke-width="${thick}"
        stroke-dasharray="${len.toFixed(2)} ${(c - len).toFixed(2)}" stroke-dashoffset="${(-off).toFixed(2)}"
        transform="rotate(-90 ${cx} ${cx})"><title>${esc(s.label)}: ${fmt(s.value)}</title></circle>`; off += len; });
    if (!segs.length) arcs = `<circle cx="${cx}" cy="${cx}" r="${r}" fill="none" stroke="var(--line)" stroke-width="${thick}"/>`;
    const center = o.center != null ? `<text x="${cx}" y="${cx - 2}" text-anchor="middle" font-size="22" font-weight="750" fill="var(--txt)">${esc(o.center)}</text>
       <text x="${cx}" y="${cx + 15}" text-anchor="middle" font-size="11" fill="var(--muted)">${esc(o.sub || "")}</text>` : "";
    return `<svg viewBox="0 0 ${size} ${size}" width="${size}" height="${size}">
      <circle cx="${cx}" cy="${cx}" r="${r}" fill="none" stroke="var(--bg)" stroke-width="${thick}"/>${arcs}${center}</svg>`;
  },
  ring(pct, o = {}) {
    const size = o.size || 96, thick = o.thick || 9, r = (size - thick) / 2, c = 2 * Math.PI * r, cx = size / 2;
    const known = pct >= 0, p = known ? Math.max(0, Math.min(100, pct)) : 0, col = o.color || "var(--acc)";
    const id = o.id || "ring" + Math.random().toString(36).slice(2, 6);
    const anim = o.animate !== false ? `<animate attributeName="stroke-dashoffset" from="${c.toFixed(2)}" to="${(c - (p / 100) * c).toFixed(2)}" dur="0.8s" fill="freeze" calcMode="spline" keySplines="0.25 0.1 0.25 1"/>` : "";
    return `<div class="ring" style="width:${size}px;height:${size}px">
      <svg viewBox="0 0 ${size} ${size}" width="${size}" height="${size}">
        <circle cx="${cx}" cy="${cx}" r="${r}" fill="none" stroke="var(--bg)" stroke-width="${thick}"/>
        <circle cx="${cx}" cy="${cx}" r="${r}" fill="none" stroke="${col}" stroke-width="${thick}" stroke-linecap="round"
          stroke-dasharray="${c.toFixed(2)}" stroke-dashoffset="${(c - (p / 100) * c).toFixed(2)}" transform="rotate(-90 ${cx} ${cx})">
          ${anim}
        </circle>
      </svg><div class="lbl"><b>${known ? Math.round(p) + "%" : "—"}</b><small>${esc(o.sub || "")}</small></div></div>`;
  },
  rank(items) {
    items = items || []; if (!items.length) return '<div class="empty">' + t("common.none") + "</div>";
    const max = Math.max(...items.map((i) => +i.value || 0), 1);
    return '<div class="rank">' + items.map((i) => `<div class="rankrow">
      <span class="lbl" title="${esc(i.label)}">${esc(i.label)}</span>
      <span class="bar"><i style="width:${Math.max(2, Math.round((100 * (+i.value || 0)) / max))}%;background:${i.color || "linear-gradient(90deg,var(--acc),var(--acc2))"}"></i></span>
      <span class="val">${esc(i.text != null ? i.text : fmt(i.value))}</span></div>`).join("") + "</div>";
  },
};

/* ---- Interactive chart hover tooltip ---- */
(function() {
  document.addEventListener('mouseover', function(e) {
    var dot = e.target.closest('.cp-chart-dot');
    if (!dot) return;
    var chart = dot.closest('.cp-interactive-chart');
    if (!chart) return;
    var id = chart.dataset.chartId;
    var cx = dot.getAttribute('cx');
    var tip = chart.querySelector('#' + id + '_tip');
    var line = chart.querySelector('#' + id + '_line');
    if (tip) { tip.setAttribute('x', cx); tip.style.display = ''; }
    if (line) { line.setAttribute('x1', cx); line.setAttribute('x2', cx); line.style.display = ''; }
    dot.setAttribute('r', '7');
    dot.style.fill = 'var(--txt)';
  });
  document.addEventListener('mouseout', function(e) {
    var dot = e.target.closest('.cp-chart-dot');
    if (!dot) {
      // Also handle multiLine dots
      var ldot = e.target.closest('.cp-line-dot');
      if (ldot) { ldot.setAttribute('r', '4'); ldot.style.opacity = '0'; }
      return;
    }
    var chart = dot.closest('.cp-interactive-chart');
    if (!chart) return;
    var id = chart.dataset.chartId;
    var tip = chart.querySelector('#' + id + '_tip');
    var line = chart.querySelector('#' + id + '_line');
    if (tip) tip.style.display = 'none';
    if (line) line.style.display = 'none';
    dot.setAttribute('r', '5');
    dot.style.fill = 'transparent';
  });
  // multiLine dot hover
  document.addEventListener('mouseover', function(e) {
    var dot = e.target.closest('.cp-line-dot');
    if (dot) { dot.setAttribute('r', '6'); dot.style.opacity = '1'; }
  });
  document.addEventListener('mouseout', function(e) {
    var dot = e.target.closest('.cp-line-dot');
    if (dot) { dot.setAttribute('r', '4'); dot.style.opacity = '0'; }
  });
})();

function kpiCard(o) {
  return `<div class="kpi ${o.accent ? "accent" : ""}">
    <div class="k"><span class="ic">${o.ic ? icon(o.ic) : ""}</span>${esc(o.k)}</div>
    <div class="v">${o.v} <small>${esc(o.sub || "")}</small></div>
    ${o.delta ? `<div class="delta ${o.deltaCls || ""}">${o.delta}</div>` : ""}
    ${o.spark ? Charts.spark(o.spark, { grad: o.grad, stroke: o.stroke }) : ""}</div>`;
}
function pct(r) { return Math.round(100 * r) + "%"; }
function cacheRatio(x) { const c = +(x && x.cached_tokens) || 0, p = +(x && x.prompt_tokens) || 0, d = p + c; return d > 0 ? c / d : 0; }
function quotaColor(p) { p = +p; if (!(p >= 0)) return "var(--muted)"; if (p >= 90) return "var(--bad)"; if (p >= 70) return "var(--warn)"; return "var(--ok)"; }
function statusToneChip(st) { if (!st) return ""; const l = String(st).toLowerCase();
  const cls = l.indexOf("reject") >= 0 ? "bad" : l.indexOf("warn") >= 0 ? "warn" : "ok"; return `<span class="chip ${cls}">${esc(st)}</span>`; }
function fmtUntil(epoch) { const now = Math.floor(Date.now() / 1000); let s = Math.max(0, (+epoch || 0) - now);
  const h = Math.floor(s / 3600), m = Math.floor((s % 3600) / 60), ss = s % 60;
  if (h) return h + "h" + String(m).padStart(2, "0") + "m"; if (m) return m + "m" + String(ss).padStart(2, "0") + "s"; return ss + "s"; }
function quotaMiniBar(snap) {
  if (!snap || !(snap.used_percent >= 0)) {
    if (snap && snap.remaining_tokens >= 0) return `<span class="mono muted">余 ${fmt(snap.remaining_tokens)}</span>`;
    return '<span class="muted">—</span>';
  }
  const p = snap.used_percent;
  const d7 = snap.secondary_7d_used_pct;
  const has7d = d7 >= 0;
  return `<div class="qcell"><div class="qbar"><i style="width:${Math.round(p)}%;background:${quotaColor(p)}"></i></div>
    <div class="qt"><span title="5h">${Math.round(p)}%</span>${has7d ? `<span class="muted" title="7d" style="margin-left:4px">· ${Math.round(d7)}%</span>` : ""}${snap.reset_at ? `<span data-until="${snap.reset_at}" data-pre=""></span>` : ""}</div></div>`;
}
function tickUntil() { $$("[data-until]").forEach((el) => { const u = +el.dataset.until || 0; if (!u) return; el.textContent = (el.dataset.pre || "") + fmtUntil(u); }); }
