import React, { useEffect, useId, useState } from 'react';
import {
  ResponsiveContainer, AreaChart, Area, BarChart, Bar, PieChart, Pie, Cell,
  XAxis, YAxis, CartesianGrid, Tooltip, Legend, Label,
} from 'recharts';
import { documentElementAttribute, observeDocumentElementAttributes } from '../lib/browserDocument.js';
import { fmtTokens, fmtTime } from '../lib/format.js';
import { PALETTE, seriesColorMap } from '../lib/chartTheme.js';

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

function ChartFrame({ height, ariaLabel = '数据图表', children }) {
  return (
    <div
      className="pool-chart-frame"
      style={{ height }}
      role="img"
      aria-label={ariaLabel}
      tabIndex={0}
    >
      {children}
    </div>
  );
}

function CompactLegend({ payload = [] }) {
  if (!payload.length) return null;
  return (
    <ul className="pool-chart-legend" aria-hidden="true">
      {payload.map((item) => (
        <li key={`${item.dataKey || ''}:${item.value}`}>
          <span style={{ background: item.color }} />
          <b title={String(item.value || '')}>{item.value}</b>
        </li>
      ))}
    </ul>
  );
}

function PolishedChartTooltip({
  active,
  payload,
  label,
  valueFormatter = (value) => value,
  labelFormatter,
  unit = '',
}) {
  if (!active || !payload?.length) return null;
  const resolvedLabel = labelFormatter?.(label, payload) ?? label;
  const rows = payload.filter((item) => item.value !== undefined && item.value !== null);
  if (!rows.length) return null;
  return (
    <div className="pool-chart-tooltip">
      {resolvedLabel !== undefined && resolvedLabel !== null ? (
        <div className="pool-chart-tooltip__label" title={String(resolvedLabel)}>{resolvedLabel}</div>
      ) : null}
      <div className="pool-chart-tooltip__rows">
        {rows.map((item) => (
          <div key={`${item.dataKey || item.name}:${item.value}`} className="pool-chart-tooltip__row">
            <span className="pool-chart-tooltip__swatch" style={{ background: item.color }} />
            <span>{item.name || item.dataKey}</span>
            <b>{valueFormatter(item.value)}{unit ? ` ${unit}` : ''}</b>
          </div>
        ))}
      </div>
    </div>
  );
}

// Stacked token-usage area chart from UsageBucket[] ({bucket, prompt_tokens, completion_tokens, cached_tokens, total_tokens}).
export function UsageAreaChart({ buckets = [], height = 260, ariaLabel = 'Token 用量趋势' }) {
  useIsDark();
  const gradientPrefix = useId().replace(/[^a-zA-Z0-9_-]/g, '');
  const c = axisColors();
  const data = (buckets || []).map((b) => ({
    t: fmtTime(b.bucket),
    输入: b.prompt_tokens || 0,
    输出: b.completion_tokens || 0,
    缓存: b.cached_tokens || 0,
    total: b.total_tokens || 0,
  }));
  return (
    <ChartFrame height={height} ariaLabel={ariaLabel}>
      <ResponsiveContainer width="100%" height="100%">
        <AreaChart data={data} margin={{ top: 8, right: 8, left: 0, bottom: 0 }}>
          <defs>
            {['输入', '输出', '缓存'].map((k, i) => (
              <linearGradient key={k} id={`${gradientPrefix}-usage-${i}`} x1="0" y1="0" x2="0" y2="1">
                <stop offset="0%" stopColor={PALETTE[i]} stopOpacity={0.42} />
                <stop offset="68%" stopColor={PALETTE[i]} stopOpacity={0.12} />
                <stop offset="100%" stopColor={PALETTE[i]} stopOpacity={0.015} />
              </linearGradient>
            ))}
          </defs>
          <CartesianGrid stroke={c.grid} strokeOpacity={0.58} vertical={false} />
          <XAxis dataKey="t" tick={{ fontSize: 11, fill: c.tick }} tickLine={false} axisLine={false} minTickGap={24} padding={{ left: 12, right: 8 }} />
          <YAxis tick={{ fontSize: 11, fill: c.tick }} tickLine={false} axisLine={false} width={44} tickFormatter={fmtTokens} />
          <Tooltip content={<PolishedChartTooltip valueFormatter={fmtTokens} />} cursor={{ stroke: c.grid, strokeWidth: 1 }} />
          <Legend content={<CompactLegend />} />
          {['输入', '输出', '缓存'].map((k, i) => (
            <Area
              key={k}
              type="monotone"
              dataKey={k}
              stackId="1"
              stroke={PALETTE[i]}
              fill={`url(#${gradientPrefix}-usage-${i})`}
              strokeWidth={2.25}
              dot={false}
              activeDot={{ r: 4, strokeWidth: 2, fill: 'var(--pool-bg-surface)' }}
              isAnimationActive={false}
            />
          ))}
        </AreaChart>
      </ResponsiveContainer>
    </ChartFrame>
  );
}

// Generic donut from [{name, value}]. Shows the total in the center with a
// custom SVG label so the chart is both visual and numeric.
export function DonutChart({ data = [], height = 240, unit = '', showCenterTotal = true, valueFormatter, ariaLabel = '占比分布' }) {
  useIsDark();
  const c = axisColors();
  const items = (data || []).filter((d) => (d.value || 0) > 0);
  if (!items.length) return <Empty />;
  const total = items.reduce((s, d) => s + (d.value || 0), 0);
  const formatValue = (v) => valueFormatter?.(v) ?? v;
  const formattedTotal = String(formatValue(total));
  const centerFontSize = formattedTotal.length > 10 ? 14 : formattedTotal.length > 7 ? 17 : 22;
  return (
    <ChartFrame height={height} ariaLabel={ariaLabel}>
      <ResponsiveContainer width="100%" height="100%">
        <PieChart>
          <Pie
            data={items}
            dataKey="value"
            nameKey="name"
            innerRadius="62%"
            outerRadius="82%"
            paddingAngle={3}
            cornerRadius={6}
            stroke="none"
            isAnimationActive={false}
          >
            {items.map((d, i) => <Cell key={i} fill={d.color || PALETTE[i % PALETTE.length]} />)}
            {showCenterTotal ? <Label position="center" value={formattedTotal} fill={c.tick}
              fontSize={centerFontSize} fontWeight={720} dy={unit ? -12 : 0} /> : null}
            {showCenterTotal && unit ? <Label position="center" value={unit} fill={c.tick}
              fontSize={11} fontWeight={520} dy={16} /> : null}
          </Pie>
          <Tooltip content={<PolishedChartTooltip valueFormatter={formatValue} unit={unit} />} />
          <Legend content={<CompactLegend />} />
        </PieChart>
      </ResponsiveContainer>
    </ChartFrame>
  );
}

// Grouped bar from [{x, ...series}] with series=[{key,color,name}].
export function GroupedBar({
  data = [],
  series = [],
  height = 240,
  stacked = false,
  showValues = false,
  ariaLabel = '柱状数据图',
  xTickFormatter,
  tooltipLabelFormatter,
  valueFormatter = fmtTokens,
}) {
  useIsDark();
  const c = axisColors();
  if (!data.length) return <Empty />;
  const labelEl = showValues ? { position: 'top', fill: c.tick, fontSize: 11 } : false;
  return (
    <ChartFrame height={height} ariaLabel={ariaLabel}>
      <ResponsiveContainer width="100%" height="100%">
        <BarChart data={data} margin={{ top: 18, right: 8, left: 0, bottom: 0 }}>
          <CartesianGrid stroke={c.grid} strokeOpacity={0.58} vertical={false} />
          <XAxis
            dataKey="x"
            tick={{ fontSize: 11, fill: c.tick }}
            tickLine={false}
            axisLine={false}
            minTickGap={16}
            tickFormatter={xTickFormatter}
          />
          <YAxis tick={{ fontSize: 11, fill: c.tick }} tickLine={false} axisLine={false} width={42} tickFormatter={fmtTokens} />
          <Tooltip
            content={<PolishedChartTooltip valueFormatter={valueFormatter} labelFormatter={tooltipLabelFormatter} />}
            cursor={{ fill: 'var(--pool-bg-hover)', radius: 8 }}
          />
          <Legend content={<CompactLegend />} />
          {series.map((s, i) => (
            <Bar
              key={s.key}
              dataKey={s.key}
              name={s.name || s.key}
              fill={s.color || PALETTE[i % PALETTE.length]}
              stackId={stacked ? '1' : undefined}
              radius={stacked ? (i === series.length - 1 ? [7, 7, 0, 0] : 0) : [7, 7, 3, 3]}
              maxBarSize={34}
              label={labelEl}
              isAnimationActive={false}
            />
          ))}
        </BarChart>
      </ResponsiveContainer>
    </ChartFrame>
  );
}

function pctOrDash(v) {
  if (v == null || Number.isNaN(Number(v))) return '—';
  const n = Math.max(0, Math.min(1, Number(v)));
  return n > 0 && n < 0.1 ? `${(n * 100).toFixed(1)}%` : `${Math.round(n * 100)}%`;
}

function metricRate(num, den) {
  num = Number(num);
  den = Number(den);
  if (!Number.isFinite(num) || !Number.isFinite(den) || den <= 0) return null;
  return num / den;
}

export function ModelMetricsTooltip({ active, payload, label }) {
  if (!active || !payload?.length) return null;
  const rows = payload
    .map((item) => {
      const metric = item.payload?.__metrics?.[item.dataKey];
      if (!metric) return null;
      return { item, metric };
    })
    .filter(Boolean)
    .sort((a, b) => (b.metric.total_tokens || 0) - (a.metric.total_tokens || 0));
  if (!rows.length) return null;
  return (
    <div className="pool-chart-tooltip pool-chart-tooltip--metrics">
      <div className="pool-chart-tooltip__label">{label}</div>
      {rows.map(({ item, metric }) => {
        const requestHit = metricRate(metric.hit_requests, metric.requests);
        const realTokenHit = metric.real_token_hit_rate ?? metricRate(metric.cache_read_tokens, metric.cache_input_tokens || metric.prompt_tokens);
        const eligibleHit = metric.eligible_cache_hit_rate ?? metricRate(metric.cache_read_tokens, (metric.cache_read_tokens || 0) + (metric.cache_creation_tokens || 0));
        const writeShare = metric.cache_write_share ?? metricRate(metric.cache_creation_tokens, metric.cache_input_tokens || metric.prompt_tokens);
        return (
          <div key={item.dataKey} className="pool-chart-tooltip__metric">
            <div className="pool-chart-tooltip__metric-title">
              <span className="pool-chart-tooltip__swatch" style={{ background: item.color }} />
              <span>{metric.series_label || item.name}</span>
            </div>
            <div className="pool-chart-tooltip__metric-grid">
              <span>Token</span><b>{fmtTokens(metric.total_tokens)}</b>
              <span>请求数</span><b>{metric.requests == null ? '—' : metric.requests}</b>
              <span>请求命中率</span><b>{pctOrDash(requestHit)}</b>
              <span>真实 Token 命中</span><b>{pctOrDash(realTokenHit)}</b>
              <span>可缓存命中</span><b>{pctOrDash(eligibleHit)}</b>
              <span>写缓存占比</span><b>{pctOrDash(writeShare)}</b>
            </div>
          </div>
        );
      })}
    </div>
  );
}

export function UsageModelAreaChart({
  modelSeries = [],
  series = [],
  height = 260,
  metric = 'total_tokens',
  selectedKeys,
  ariaLabel = '模型用量趋势',
}) {
  useIsDark();
  const gradientPrefix = useId().replace(/[^a-zA-Z0-9_-]/g, '');
  const c = axisColors();
  // Colours come from the *unfiltered* series list, deliberately: assigning after the
  // selectedKeys filter would recolour every remaining line whenever one is toggled off.
  const colorOf = seriesColorMap((series || []).map((s) => s.series_key || s.model_key || s.series_label));
  const descriptors = (series || []).map((s, i) => ({
    ...s,
    field: `m${i}`,
    color: colorOf(s.series_key || s.model_key || s.series_label),
  })).filter((s) => !selectedKeys || selectedKeys.has(s.series_key));
  if (!descriptors.length || !modelSeries.length) return <Empty />;
  const fieldByKey = new Map(descriptors.map((s) => [s.series_key, s.field]));
  const labelByKey = new Map(descriptors.map((s) => [s.series_key, s.series_label]));
  const buckets = new Map();
  for (const row of modelSeries || []) {
    const field = fieldByKey.get(row.series_key);
    if (!field) continue;
    if (!buckets.has(row.bucket)) buckets.set(row.bucket, { t: fmtTime(row.bucket), __metrics: {} });
    const item = buckets.get(row.bucket);
    item[field] = row[metric] || 0;
    item.__metrics[field] = { ...row, series_label: row.series_label || labelByKey.get(row.series_key) || row.series_key };
  }
  const data = [...buckets.entries()].sort((a, b) => a[0] - b[0]).map(([, row]) => row);
  if (!data.length) return <Empty />;
  return (
    <ChartFrame height={height} ariaLabel={ariaLabel}>
      <ResponsiveContainer width="100%" height="100%">
        <AreaChart data={data} margin={{ top: 8, right: 8, left: 0, bottom: 0 }}>
          <defs>
            {descriptors.map((s) => (
              <linearGradient key={s.series_key} id={`${gradientPrefix}-${s.field}`} x1="0" y1="0" x2="0" y2="1">
                <stop offset="0%" stopColor={s.color} stopOpacity={0.34} />
                <stop offset="100%" stopColor={s.color} stopOpacity={0.025} />
              </linearGradient>
            ))}
          </defs>
          <CartesianGrid stroke={c.grid} strokeOpacity={0.58} vertical={false} />
          <XAxis dataKey="t" tick={{ fontSize: 11, fill: c.tick }} tickLine={false} axisLine={false} minTickGap={24} padding={{ left: 12, right: 8 }} />
          <YAxis tick={{ fontSize: 11, fill: c.tick }} tickLine={false} axisLine={false} width={44} tickFormatter={fmtTokens} />
          <Tooltip content={<ModelMetricsTooltip />} cursor={{ stroke: c.grid, strokeWidth: 1 }} />
          <Legend content={<CompactLegend />} />
          {descriptors.map((s) => (
            <Area
              key={s.series_key}
              type="monotone"
              dataKey={s.field}
              name={s.series_label || s.series_key}
              stackId="model"
              stroke={s.color}
              fill={`url(#${gradientPrefix}-${s.field})`}
              strokeWidth={2.25}
              dot={false}
              activeDot={{ r: 4, strokeWidth: 2, fill: 'var(--pool-bg-surface)' }}
              isAnimationActive={false}
            />
          ))}
        </AreaChart>
      </ResponsiveContainer>
    </ChartFrame>
  );
}

// Per-model cache-hit-rate bars (rate = cache_read/cache_input), each model its own color.
export function CacheRateBars({ data = [] }) {
  const rows = (data || [])
    .filter((d) => (d.cache_input_tokens || d.prompt_tokens || 0) > 0)
    .map((d) => ({
      model: d.display_label || d.series_label || d.model_label || d.model || '(未知)',
      modelKey: d.dimension_key || d.series_key || d.model_key || d.model || '(未知)',
      rate: Math.max(0, Math.min(100, Math.round((100 * (d.cache_read_tokens || d.cached_tokens || 0)) / (d.cache_input_tokens || d.prompt_tokens || 1)))),
      prompt: d.cache_input_tokens || d.prompt_tokens || 0,
    }));
  if (!rows.length) return <Empty />;
  const colorOf = seriesColorMap(rows.map((r) => r.modelKey));
  return (
    <div>
      {rows.map((r, i) => {
        const c = colorOf(r.modelKey);
        return (
          <div key={i} style={{ marginBottom: 14 }}>
            <div style={{ display: 'flex', justifyContent: 'space-between', fontSize: 12.5, marginBottom: 6 }}>
              <span className="pool-cache-model" title={r.model}>
                <span style={{ width: 9, height: 9, borderRadius: 3, background: c, display: 'inline-block' }} />
                <span>{r.model}</span>
              </span>
              <span style={{ fontWeight: 600, color: 'var(--pool-text)' }}>{r.rate}%</span>
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
    <div className="pool-chart-empty">
      暂无可展示的数据
    </div>
  );
}
