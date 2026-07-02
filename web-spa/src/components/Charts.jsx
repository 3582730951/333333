import React, { useEffect, useState } from 'react';
import {
  ResponsiveContainer, AreaChart, Area, BarChart, Bar, PieChart, Pie, Cell,
  XAxis, YAxis, CartesianGrid, Tooltip, Legend,
} from 'recharts';
import { documentElementAttribute, observeDocumentElementAttributes } from '../lib/browserDocument.js';
import { fmtTokens, fmtTime } from '../lib/format.js';
import { PALETTE, modelColor } from '../lib/chartTheme.js';

// Track the console theme so chart axes/grid stay legible on theme toggle.
export function useIsDark() {
  const read = () => documentElementAttribute('data-theme') !== 'light';
  const [dark, setDark] = useState(read());
  useEffect(() => {
    return observeDocumentElementAttributes(() => setDark(read()), ['data-theme']);
  }, []);
  return dark;
}

function axisColors() {
  return {
    grid: 'var(--pool-chart-grid)',
    tick: 'var(--pool-chart-tick)',
    tooltipBg: 'var(--pool-chart-tooltip-bg)',
    tooltipBorder: 'var(--pool-chart-tooltip-border)',
  };
}

export function ChartCard({ title, sub, extra, children, height = 260 }) {
  return (
    <div className="pool-chart-card">
      <div className="head">
        <div>
          <div className="t">{title}</div>
          {sub ? <div className="s">{sub}</div> : null}
        </div>
        {extra}
      </div>
      <div style={{ width: '100%', height }}>{children}</div>
    </div>
  );
}

function tooltipStyle(c) {
  return {
    contentStyle: { background: c.tooltipBg, border: `1px solid ${c.tooltipBorder}`, borderRadius: 10, fontSize: 12 },
    labelStyle: { color: c.tick }, itemStyle: { fontSize: 12 },
  };
}

// Stacked token-usage area chart from UsageBucket[] ({bucket, prompt_tokens, completion_tokens, cached_tokens, total_tokens}).
export function UsageAreaChart({ buckets = [], height = 260 }) {
  useIsDark();
  const c = axisColors();
  const data = (buckets || []).map((b) => ({
    t: fmtTime(b.bucket),
    输入: b.prompt_tokens || 0,
    输出: b.completion_tokens || 0,
    缓存: b.cached_tokens || 0,
    total: b.total_tokens || 0,
  }));
  return (
    <ResponsiveContainer width="100%" height={height}>
      <AreaChart data={data} margin={{ top: 6, right: 8, left: 0, bottom: 0 }}>
        <defs>
          {['输入', '输出', '缓存'].map((k, i) => (
            <linearGradient key={k} id={`g-${i}`} x1="0" y1="0" x2="0" y2="1">
              <stop offset="0%" stopColor={PALETTE[i]} stopOpacity={0.5} />
              <stop offset="100%" stopColor={PALETTE[i]} stopOpacity={0.04} />
            </linearGradient>
          ))}
        </defs>
        <CartesianGrid strokeDasharray="3 3" stroke={c.grid} vertical={false} />
        <XAxis dataKey="t" tick={{ fontSize: 11, fill: c.tick }} tickLine={false} axisLine={false} minTickGap={24} />
        <YAxis tick={{ fontSize: 11, fill: c.tick }} tickLine={false} axisLine={false} width={44} tickFormatter={fmtTokens} />
        <Tooltip {...tooltipStyle(c)} formatter={(v) => fmtTokens(v)} />
        <Legend wrapperStyle={{ fontSize: 12 }} />
        {['输入', '输出', '缓存'].map((k, i) => (
          <Area key={k} type="monotone" dataKey={k} stackId="1" stroke={PALETTE[i]} fill={`url(#g-${i})`} strokeWidth={2} />
        ))}
      </AreaChart>
    </ResponsiveContainer>
  );
}

// Generic donut from [{name, value}]. Shows the total in the center with a
// custom SVG label so the chart is both visual and numeric.
export function DonutChart({ data = [], height = 240, unit = '', showCenterTotal = true }) {
  useIsDark();
  const c = axisColors();
  const items = (data || []).filter((d) => (d.value || 0) > 0);
  if (!items.length) return <Empty />;
  const total = items.reduce((s, d) => s + (d.value || 0), 0);
  const centerLabel = (props) => {
    const { cx, cy } = props;
    return showCenterTotal ? (
      <>
        <text x={cx} y={cy - 10} textAnchor="middle" dominantBaseline="middle"
          fontSize={22} fontWeight={700} fill={c.tick}>
          {total}
        </text>
        <text x={cx} y={cy + 14} textAnchor="middle" dominantBaseline="middle"
          fontSize={11} fill={c.tick}>
          {unit || ''}
        </text>
      </>
    ) : null;
  };
  return (
    <ResponsiveContainer width="100%" height={height}>
      <PieChart>
        <Pie data={items} dataKey="value" nameKey="name" innerRadius="58%" outerRadius="82%"
          paddingAngle={2} stroke="none" label={centerLabel} labelLine={false}>
          {items.map((d, i) => <Cell key={i} fill={d.color || PALETTE[i % PALETTE.length]} />)}
        </Pie>
        <Tooltip {...tooltipStyle(c)} formatter={(v, n) => [`${v}${unit}`, n]} />
        <Legend wrapperStyle={{ fontSize: 12 }} />
      </PieChart>
    </ResponsiveContainer>
  );
}

// Grouped bar from [{x, ...series}] with series=[{key,color,name}].
export function GroupedBar({ data = [], series = [], height = 240, stacked = false, showValues = true }) {
  useIsDark();
  const c = axisColors();
  if (!data.length) return <Empty />;
  const labelEl = showValues ? { position: 'top', fill: c.tick, fontSize: 11 } : false;
  return (
    <ResponsiveContainer width="100%" height={height}>
      <BarChart data={data} margin={{ top: 18, right: 8, left: 0, bottom: 0 }}>
        <CartesianGrid strokeDasharray="3 3" stroke={c.grid} vertical={false} />
        <XAxis dataKey="x" tick={{ fontSize: 11, fill: c.tick }} tickLine={false} axisLine={false} minTickGap={16} />
        <YAxis tick={{ fontSize: 11, fill: c.tick }} tickLine={false} axisLine={false} width={40} tickFormatter={fmtTokens} />
        <Tooltip {...tooltipStyle(c)} />
        <Legend wrapperStyle={{ fontSize: 12 }} />
        {series.map((s, i) => (
          <Bar key={s.key} dataKey={s.key} name={s.name || s.key} fill={s.color || PALETTE[i % PALETTE.length]}
            stackId={stacked ? '1' : undefined} radius={stacked ? 0 : [4, 4, 0, 0]} maxBarSize={36}
            label={labelEl} />
        ))}
      </BarChart>
    </ResponsiveContainer>
  );
}

// Per-model cache-hit-rate bars (rate = cached/prompt), each model its own color.
export function CacheRateBars({ data = [] }) {
  const rows = (data || [])
    .filter((d) => (d.prompt_tokens || 0) > 0)
    .map((d) => ({
      model: d.model || '(未知)',
      rate: Math.round((100 * (d.cached_tokens || 0)) / (d.prompt_tokens || 1)),
      prompt: d.prompt_tokens || 0,
    }));
  if (!rows.length) return <Empty />;
  return (
    <div>
      {rows.map((r, i) => {
        const c = modelColor(r.model);
        return (
          <div key={i} style={{ marginBottom: 14 }}>
            <div style={{ display: 'flex', justifyContent: 'space-between', fontSize: 12.5, marginBottom: 6 }}>
              <span style={{ display: 'flex', alignItems: 'center', gap: 7 }}>
                <span style={{ width: 9, height: 9, borderRadius: 3, background: c, display: 'inline-block' }} />
                {r.model}
              </span>
              <span style={{ fontWeight: 600, color: c }}>{r.rate}%</span>
            </div>
            <div className="pool-meter"><span style={{ width: r.rate + '%', background: c }} /></div>
          </div>
        );
      })}
    </div>
  );
}

function Empty() {
  return (
    <div style={{ height: '100%', display: 'flex', alignItems: 'center', justifyContent: 'center', color: 'var(--pool-text-2)', fontSize: 13 }}>
      暂无数据
    </div>
  );
}
