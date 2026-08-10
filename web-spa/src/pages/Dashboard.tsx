import React, { useCallback, useEffect, useMemo, useState } from 'react';
import { useNavigate } from 'react-router';
import { Tag, Button, LoadingState } from '../components/pool/index.jsx';
import { IconRefresh, IconPlus, IconUser, IconKey, IconSetting, IconLineChartStroked } from '../components/pool/icons.jsx';
import LoadErrorBannerBase from '../components/LoadErrorBanner.jsx';
import PageHeaderBase from '../components/PageHeader.jsx';
import SystemHealthSummaryBase from '../components/SystemHealthSummary.jsx';
import * as MicroCharts from '../components/MicroCharts.jsx';
import useVisibleInterval, { usePageVisible } from '../hooks/useVisibleInterval.js';
import { UsageAreaChart, UsageModelAreaChart, DonutChart, GroupedBar, CacheRateBars } from '../components/LazyCharts.jsx';
import { COLORS, seriesColorMap } from '../lib/chartTheme.js';
import { fmtTokens, fmtInt, fmtTime } from '../lib/format.js';
import { t } from '../lib/i18n.js';
import {
  DASHBOARD_REFRESH_MS, invalidateDashboardUsageSnapshot, useDashboardCoreData, useDashboardSecondaryData,
} from '../features/observability/queries/dashboard';

const LoadErrorBanner = LoadErrorBannerBase as any;
const PageHeader = PageHeaderBase as any;
const SystemHealthSummary = SystemHealthSummaryBase as any;
const AreaChart = UsageAreaChart as any;
const ModelAreaChart = UsageModelAreaChart as any;
const Donut = DonutChart as any;
const BarChart = GroupedBar as any;
const CacheBars = CacheRateBars as any;
const { Sparkline, RadialGauge, RankedBars, HeatStrip, StackedMeter, DeltaBadge } = MicroCharts as any;
const C = COLORS;

function newestDate(...values: Array<Date | null>) {
  const timestamps = values.filter(Boolean).map((value) => value!.getTime());
  return timestamps.length ? new Date(Math.max(...timestamps)) : null;
}

function lastRefreshText(lastRefresh: Date | null) {
  if (!lastRefresh) return t('dashboard.loading');
  const seconds = Math.max(0, Math.floor((Date.now() - lastRefresh.getTime()) / 1000));
  if (seconds < 5) return t('dashboard.updated_now');
  if (seconds < 60) return t('dashboard.updated_seconds').replace('{count}', String(seconds));
  return t('dashboard.updated_minutes').replace('{count}', String(Math.floor(seconds / 60)));
}

function healthTag(available: boolean, ok?: boolean) {
  if (!available) return <Tag>{t('dashboard.health_unknown')}</Tag>;
  return ok ? <Tag color="green">{t('dashboard.service_normal')}</Tag> : <Tag color="red">{t('dashboard.service_abnormal')}</Tag>;
}

export function compactIdentity(value: unknown, head = 8, tail = 6) {
  const text = String(value || t('common.unknown'));
  if (text.length <= head + tail + 1) return text;
  return `${text.slice(0, head)}…${text.slice(-tail)}`;
}

// Momentum of a series: the second half of the window against the first half.
// Returns null when there is not enough signal to make the comparison honest.
export function halfWindowDelta(values: number[]): number | null {
  const series = (values || []).filter((value) => Number.isFinite(value));
  if (series.length < 4) return null;
  const middle = Math.floor(series.length / 2);
  const first = series.slice(0, middle).reduce((sum, value) => sum + value, 0);
  const second = series.slice(middle).reduce((sum, value) => sum + value, 0);
  if (first <= 0) return second > 0 ? 1 : null;
  return (second - first) / first;
}

function ratioOrNull(numerator: unknown, denominator: unknown): number | null {
  const top = Number(numerator);
  const bottom = Number(denominator);
  if (!Number.isFinite(top) || !Number.isFinite(bottom) || bottom <= 0) return null;
  return Math.max(0, Math.min(1, top / bottom));
}

function KpiTile({
  label, value, delta, series, color, caption, invertDelta,
}: {
  label: string;
  value: React.ReactNode;
  delta?: number | null;
  series?: number[];
  color: string;
  caption?: string;
  invertDelta?: boolean;
}) {
  return (
    // `color` used to reach only the Sparkline, so it rendered nothing on the three
    // tiles that carry no series. The dot gives every tile the same treatment and
    // doubles as the sparkline's legend where one is present.
    <article className="pool-kpi" style={{ '--pool-kpi-accent': color } as React.CSSProperties}>
      <div className="pool-kpi__head">
        <span className="pool-kpi__ident">
          <span className="pool-kpi__dot" aria-hidden="true" />
          <span className="pool-kpi__label">{label}</span>
        </span>
        {delta !== undefined ? <DeltaBadge value={delta} invert={invertDelta} /> : null}
      </div>
      <div className="pool-kpi__value">{value}</div>
      <div className="pool-kpi__foot">
        {caption ? <span className="pool-kpi__caption">{caption}</span> : <span className="pool-kpi__caption" />}
        {series && series.length > 1 ? (
          <div className="pool-kpi__spark">
            <Sparkline values={series} color={color} height={30} ariaLabel={t('dashboard.trend_sparkline').replace('{label}', label)} />
          </div>
        ) : null}
      </div>
    </article>
  );
}

export default function Dashboard() {
  const navigate = useNavigate();
  const pageVisible = usePageVisible();
  const coreQuery = useDashboardCoreData();
  const secondaryQuery = useDashboardSecondaryData();
  const core = coreQuery.data;
  const secondary = secondaryQuery.data;
  const loading = coreQuery.loading || secondaryQuery.loading;
  const lastRefresh = newestDate(coreQuery.lastRefresh, secondaryQuery.lastRefresh);
  const [countdown, setCountdown] = useState(DASHBOARD_REFRESH_MS / 1000);

  const reload = useCallback(async () => {
    invalidateDashboardUsageSnapshot();
    const [coreResult] = await Promise.all([coreQuery.reload(), secondaryQuery.reload()]);
    return coreResult;
  }, [coreQuery.reload, secondaryQuery.reload]);

  useVisibleInterval(() => {
    setCountdown((value) => value <= 1 ? DASHBOARD_REFRESH_MS / 1000 : value - 1);
  }, 1000, { fireOnVisible: false });

  useEffect(() => {
    if (!loading && lastRefresh) setCountdown(DASHBOARD_REFRESH_MS / 1000);
  }, [loading, lastRefresh]);

  const buckets = core?.buckets || [];
  const derived = useMemo(() => {
    const requestSeries = buckets.map((bucket) => Number(bucket.requests) || 0);
    const tokenSeries = buckets.map((bucket) => Number(bucket.total_tokens) || 0);
    const cachedSeries = buckets.map((bucket) => Number(bucket.cached_tokens) || 0);
    return {
      requestSeries,
      tokenSeries,
      cachedSeries,
      requests: requestSeries.reduce((sum, value) => sum + value, 0),
      tokens: tokenSeries.reduce((sum, value) => sum + value, 0),
      requestDelta: halfWindowDelta(requestSeries),
      tokenDelta: halfWindowDelta(tokenSeries),
    };
  }, [buckets]);

  if (coreQuery.error && !coreQuery.lastRefresh && !coreQuery.loading) {
    return (
      <div>
        <PageHeader title={t('dashboard.title')} subtitle={t('dashboard.subtitle')} actions={<Button icon={<IconRefresh />} onClick={reload}>{t('common.refresh')}</Button>} />
        <LoadErrorBanner error={coreQuery.error} onRetry={reload} title={t('dashboard.load_failed')} />
      </div>
    );
  }
  if (!core && coreQuery.loading) {
    return (
      <div>
        <PageHeader title={t('dashboard.title')} subtitle={t('dashboard.subtitle')} />
        <LoadingState title={t('dashboard.loading')} />
      </div>
    );
  }

  const summary = core?.accountSummary;
  const active = summary?.active || 0;
  const total = summary?.total || 0;
  const quarantined = summary?.quarantined || 0;
  const cooling = summary?.cooling || 0;
  const recheck = summary?.recheck || 0;
  const codex = summary?.codex || 0;
  const claude = summary?.claude || 0;
  const other = summary?.other || 0;
  const providerModelSeries = core?.modelSeries || [];
  const providerModelDescriptors = core?.series || [];
  const { requests, tokens, requestSeries, tokenSeries, requestDelta, tokenDelta } = derived;

  const cacheByModel = secondary?.cache?.by_provider_model || secondary?.cache?.by_model || [];
  const officialCacheByModel = cacheByModel.filter((row) => String(row.provider || '').toLowerCase() !== 'kiro');
  const cacheByProvider = secondary?.cache?.by_provider || [];
  const cacheSummary = secondary?.cache?.summary || {};
  const officialCacheRows = cacheByProvider.filter((row) => String(row.provider || '').toLowerCase() !== 'kiro');
  const cacheInput = officialCacheRows.length
    ? officialCacheRows.reduce((sum, row) => sum + Number(row.cache_input_tokens || row.prompt_tokens || 0), 0)
    : (cacheSummary.cache_input_tokens ?? cacheByModel.reduce((sum, row) => sum + (row.cache_input_tokens || row.prompt_tokens || 0), 0));
  const cacheRead = officialCacheRows.length
    ? officialCacheRows.reduce((sum, row) => sum + Number(row.cache_read_tokens || row.cached_tokens || 0), 0)
    : (cacheSummary.cache_read_tokens ?? cacheByModel.reduce((sum, row) => sum + (row.cache_read_tokens || row.cached_tokens || 0), 0));
  const cacheHitRate = cacheInput > 0 ? cacheRead / cacheInput : 0;
  const kiroCache = cacheByProvider.find((row) => String(row.provider || '').toLowerCase() === 'kiro');
  const cacheCompleteness = kiroCache?.cache_reporting_state === 'unreported'
    ? t('usage.upstream_unreported')
    : kiroCache?.cache_reporting_state === 'partial'
      ? `${Math.round(Number(kiroCache.cache_reporting_rate || 0) * 100)}%`
      : t('dashboard.cache_complete');
  const registrationRate = secondary?.registration?.totals?.success_rate || 0;
  const byModel = secondary?.byModel || [];

  // Cache efficiency reads three different ways; showing them side by side stops a
  // single headline percentage from hiding a bad write ratio.
  const requestHitRate = cacheSummary.request_hit_rate ?? ratioOrNull(cacheSummary.hit_requests, cacheSummary.real_requests ?? cacheSummary.requests);
  const tokenHitRate = cacheSummary.real_token_hit_rate ?? cacheSummary.token_hit_rate ?? ratioOrNull(cacheRead, cacheInput);
  const eligibleHitRate = cacheSummary.eligible_cache_hit_rate
    ?? ratioOrNull(cacheSummary.cache_read_tokens, Number(cacheSummary.cache_read_tokens || 0) + Number(cacheSummary.cache_creation_tokens || 0));
  const cacheSegments = [
    { key: 'read', name: t('dashboard.cache_read'), value: Number(cacheSummary.cache_read_tokens || 0), color: C.green },
    { key: 'write', name: t('dashboard.cache_write'), value: Number(cacheSummary.cache_creation_tokens || 0), color: C.violet },
    { key: 'miss', name: t('dashboard.cache_miss'), value: Number(cacheSummary.cache_miss_tokens || 0), color: C.grey },
  ];
  const hasCacheComposition = cacheSegments.some((segment) => segment.value > 0);
  const hasCacheGauges = [requestHitRate, tokenHitRate, eligibleHitRate].some((rate) => rate !== null && rate !== undefined);

  const statusDonut = [
    { name: t('dashboard.active'), value: active, color: C.green },
    { name: t('dashboard.cooling'), value: cooling, color: C.cyan },
    { name: t('dashboard.quarantined'), value: quarantined, color: C.red },
    { name: t('dashboard.recheck'), value: recheck, color: C.amber },
  ];
  const providerDonut = [
    { name: 'Codex', value: codex, color: C.blue },
    { name: 'Claude', value: claude, color: C.violet },
    { name: t('dashboard.other'), value: other, color: COLORS.grey },
  ];
  const registrationDays = (secondary?.registration?.by_day || []).map((row) => ({
    x: (row.date || '').slice(5), success: row.succeeded || 0, failed: row.failed || 0,
  }));
  const topModels = byModel.slice(0, 6);
  // Built over the sliced rows -- the six that actually render -- so the six colours are
  // guaranteed distinct rather than hash-collided down to four or five.
  const modelTokenColor = seriesColorMap(topModels.map((row) => row.dimension_key || row.model_key || row.model));
  const modelTokenRows = topModels.map((row) => ({
    key: String(row.dimension_key || row.model_key || row.model || ''),
    name: row.display_label || row.series_label || `${row.provider_name || row.provider_id || ''}${row.provider_name || row.provider_id ? ' · ' : ''}${row.model_label || row.model || `(${t('common.unknown')})`}`,
    value: row.total_tokens || 0,
    color: modelTokenColor(row.dimension_key || row.model_key || row.model),
    meta: row.requests ? `${fmtInt(row.requests)} ${t('dashboard.requests_unit')}` : undefined,
  }));
  // Local formatter: the compact B/M/k scale belongs to this view only and must not
  // be achieved by reaching into the shared fmtTokens helper.
  const modelTokenFormatter = (value: unknown) => {
    const number = Number(value) || 0;
    if (number >= 1e9) return `${(number / 1e9).toFixed(2)}B`;
    if (number >= 1e6) return `${(number / 1e6).toFixed(2)}M`;
    if (number >= 1e3) return `${(number / 1e3).toFixed(1)}k`;
    return `${number}`;
  };
  const topAccounts = (secondary?.cache?.by_account || []).slice(0, 6);
  // A separate map from the model chart above: this is its own chart with its own legend, so
  // the two are independent and an account may share a colour with a model. Within one chart
  // no two rows can.
  const accountColor = seriesColorMap(topAccounts.map((row) => row.account_id));
  const topAccountRows = topAccounts.map((row) => {
    const input = Number(row.actual_prompt_tokens || row.prompt_tokens || 0);
    const output = Number(row.actual_completion_tokens || row.completion_tokens || 0);
    return {
      key: String(row.account_id || ''),
      name: compactIdentity(row.account_id),
      value: input + output,
      color: accountColor(row.account_id),
      meta: `${t('usage.input')} ${fmtTokens(input)} · ${t('usage.output')} ${fmtTokens(output)}`,
    };
  });

  // Hourly request intensity — a band that reads at a glance next to the
  // axis-heavy trend chart above it.
  const activityCells = buckets.map((bucket) => ({
    key: String(bucket.bucket),
    value: Number(bucket.requests) || 0,
    label: fmtTime(bucket.bucket),
    valueText: `${fmtInt(bucket.requests || 0)} ${t('dashboard.requests_unit')}`,
  }));
  const peakBucket = buckets.reduce<{ requests: number; bucket: number } | null>((best, bucket) => {
    const value = Number(bucket.requests) || 0;
    return !best || value > best.requests ? { requests: value, bucket: bucket.bucket } : best;
  }, null);

  const system = secondary?.system;
  const hostMeters = system?.supported ? [
    { key: 'cpu', label: t('dashboard.cpu'), pct: Number(system.cpu?.usage_pct ?? NaN), color: C.blue },
    { key: 'mem', label: t('dashboard.memory'), pct: Number(system.mem?.used_pct ?? NaN), color: C.violet },
    { key: 'disk', label: t('dashboard.disk'), pct: Number(system.disk?.used_pct ?? NaN), color: C.amber },
  ].filter((meter) => Number.isFinite(meter.pct)) : [];

  const partialError = core?.error || secondary?.error || secondaryQuery.error;
  const hasStatusDistribution = statusDonut.some((item) => item.value > 0);
  const hasProviderDistribution = providerDonut.some((item) => item.value > 0);
  const hasRegistrationTrend = secondary?.registrationAvailable && registrationDays.some((item) => item.success || item.failed);
  const hasModelTokens = secondary?.modelAvailable && modelTokenRows.some((item) => item.value > 0);

  const attentionItems = [
    {
      key: 'quarantined',
      tone: quarantined ? 'issue' : 'clear',
      label: t('dashboard.quarantined_accounts'),
      value: fmtInt(quarantined),
      description: quarantined ? t('dashboard.quarantined_issue') : t('dashboard.quarantined_clear'),
      action: quarantined ? { label: t('dashboard.view_accounts'), to: '/accounts' } : null,
    },
    {
      key: 'recheck',
      tone: recheck ? 'issue' : 'clear',
      label: t('dashboard.recheck'),
      value: fmtInt(recheck),
      description: recheck ? t('dashboard.recheck_issue') : t('dashboard.recheck_clear'),
      action: null,
    },
    {
      key: 'cooling',
      tone: cooling ? 'warn' : 'clear',
      label: t('dashboard.cooling'),
      value: fmtInt(cooling),
      description: cooling ? t('dashboard.cooling_issue') : t('dashboard.cooling_clear'),
      action: null,
    },
    {
      key: 'coverage',
      tone: 'neutral',
      label: t('dashboard.model_coverage'),
      value: secondary?.modelAvailable ? fmtInt(byModel.length) : '—',
      description: t('dashboard.model_coverage_desc'),
      action: null,
    },
    {
      key: 'completeness',
      tone: 'neutral',
      label: t('dashboard.cache_completeness'),
      value: secondary?.cacheAvailable ? cacheCompleteness : '—',
      description: kiroCache?.cache_reporting_state === 'unreported' ? t('dashboard.kiro_cache_unreported') : t('dashboard.cache_completeness_desc'),
      action: null,
    },
  ];

  return (
    <div className="pool-dashboard">
      <PageHeader title={t('dashboard.title')} subtitle={t('dashboard.subtitle')}
        actions={<>
          <Button icon={<IconRefresh />} onClick={reload} loading={loading}>{t('common.refresh')}</Button>
          {!loading ? <span className="pool-text-tertiary" style={{ fontSize: 12 }}>
            {lastRefreshText(lastRefresh)} · {pageVisible ? t('dashboard.auto_refresh').replace('{count}', String(countdown)) : t('dashboard.refresh_paused')}
          </span> : null}
        </>} />

      <LoadErrorBanner error={partialError} onRetry={reload} title={partialError ? t('dashboard.partial_failed') : undefined} />

      <div className="pool-dashboard-statusbar">
        <div className="pool-dashboard-statusbar__health">
          {healthTag(Boolean(core?.healthAvailable), core?.health?.ok)}
          <span>{lastRefreshText(lastRefresh)}</span>
        </div>
        <div className="pool-dashboard-statusbar__actions">
          <Button size="small" icon={<IconPlus />} type="primary" onClick={() => navigate('/accounts?action=import')}>{t('dashboard.import_account')}</Button>
          <Button size="small" icon={<IconUser />} onClick={() => navigate('/accounts')}>{t('dashboard.account_management')}</Button>
          <Button size="small" icon={<IconKey />} onClick={() => navigate('/keys')}>API Keys</Button>
          <Button size="small" icon={<IconLineChartStroked />} onClick={() => navigate('/usage')}>{t('dashboard.usage')}</Button>
          <Button size="small" icon={<IconSetting />} onClick={() => navigate('/settings-v2')}>{t('dashboard.system_settings')}</Button>
        </div>
      </div>

      <section className="pool-kpi-strip" aria-label={t('dashboard.key_metrics')}>
        <KpiTile
          label={t('dashboard.schedulable_accounts')}
          value={fmtInt(active)}
          color={C.green}
          caption={total ? t('dashboard.of_total').replace('{count}', fmtInt(total)) : undefined}
        />
        <KpiTile
          label={t('dashboard.requests_24h')}
          value={core?.timeseriesAvailable ? fmtInt(requests) : '—'}
          delta={core?.timeseriesAvailable ? requestDelta : undefined}
          series={requestSeries}
          color={C.blue}
          caption={t('dashboard.vs_first_half')}
        />
        <KpiTile
          label={t('dashboard.tokens_24h')}
          value={core?.timeseriesAvailable ? fmtTokens(tokens) : '—'}
          delta={core?.timeseriesAvailable ? tokenDelta : undefined}
          series={tokenSeries}
          color={C.violet}
          caption={t('dashboard.vs_first_half')}
        />
        <KpiTile
          label={t('dashboard.cache_hit')}
          value={secondary?.cacheAvailable ? `${Math.round(cacheHitRate * 100)}%` : '—'}
          color={C.cyan}
          caption={secondary?.cacheAvailable ? t('dashboard.cache_read_of_input') : undefined}
        />
        <KpiTile
          label={t('dashboard.registration_rate')}
          value={secondary?.registrationAvailable ? `${Math.round(registrationRate * 100)}%` : '—'}
          color={C.amber}
          caption={secondary?.registrationAvailable && secondary?.registration?.totals
            ? t('dashboard.registration_counts')
              .replace('{ok}', fmtInt(secondary.registration.totals.succeeded || 0))
              .replace('{fail}', fmtInt(secondary.registration.totals.failed || 0))
            : undefined}
        />
      </section>

      <section className="pool-ops-split">
        {core?.timeseriesAvailable ? (
          <div className="pool-chart-card pool-ops-split__trend">
            <div className="head"><div><div className="t">{t('dashboard.provider_model_trend')}</div><div className="s">{t('dashboard.provider_model_trend_desc')}</div></div></div>
            <div className="pool-ops-split__canvas">
              {providerModelSeries.length && providerModelDescriptors.length
                ? <ModelAreaChart modelSeries={providerModelSeries} series={providerModelDescriptors} height="100%" ariaLabel={t('dashboard.provider_model_trend')} />
                : <AreaChart buckets={buckets} height="100%" ariaLabel={t('dashboard.provider_model_trend')} />}
            </div>
            {activityCells.length > 1 ? (
              <div className="pool-ops-split__activity">
                <div className="pool-ops-split__activity-label">{t('dashboard.activity_24h')}</div>
                <HeatStrip
                  cells={activityCells}
                  color={C.blue}
                  ariaLabel={t('dashboard.activity_desc')}
                  footer={(
                    <>
                      <span>{activityCells[0]?.label}</span>
                      {peakBucket ? <span>{t('dashboard.peak_hour').replace('{value}', `${fmtTime(peakBucket.bucket)} · ${fmtInt(peakBucket.requests)}`)}</span> : null}
                      <span>{activityCells[activityCells.length - 1]?.label}</span>
                    </>
                  )}
                />
              </div>
            ) : null}
          </div>
        ) : null}

        <aside className="pool-card pool-attention-rail" aria-label={t('dashboard.attention')}>
          <div className="pool-section-title">{t('dashboard.attention')}</div>
          <ul className="pool-attention-rail__list">
            {attentionItems.map((item) => (
              <li key={item.key} className={`pool-attention-card pool-attention-card--${item.tone}`}>
                <div className="pool-attention-card__head">
                  <span className="pool-attention-card__label">{item.label}</span>
                  <b className="pool-attention-card__value">{item.value}</b>
                </div>
                <p className="pool-attention-card__desc">{item.description}</p>
                {item.action ? (
                  <Button size="small" theme="borderless" onClick={() => navigate(item.action!.to)}>{item.action.label}</Button>
                ) : null}
              </li>
            ))}
          </ul>
        </aside>
      </section>

      {secondary?.cacheAvailable && (hasCacheGauges || hasCacheComposition || officialCacheByModel.length) ? (
        <section className="pool-chart-card pool-cache-panel">
          <div className="head"><div><div className="t">{t('dashboard.cache_efficiency')}</div><div className="s">{t('dashboard.cache_efficiency_desc')}</div></div></div>
          <div className="pool-cache-panel__body">
            {hasCacheGauges ? (
              <div className="pool-cache-panel__gauges">
                {/* No caption: each one read as a truncation of its own label — 请求 inside
                    请求命中率 — so every gauge said its subject twice, once in 12px inside the
                    ring and once underneath it. The label alone names the metric. */}
                <RadialGauge value={requestHitRate ?? 0} color={C.blue} label={t('dashboard.request_hit_rate')} valueText={requestHitRate == null ? '—' : undefined} />
                <RadialGauge value={tokenHitRate ?? 0} color={C.green} label={t('dashboard.token_hit_rate')} valueText={tokenHitRate == null ? '—' : undefined} />
                <RadialGauge value={eligibleHitRate ?? 0} color={C.violet} label={t('dashboard.eligible_hit_rate')} valueText={eligibleHitRate == null ? '—' : undefined} />
              </div>
            ) : null}
            <div className="pool-cache-panel__detail">
              {hasCacheComposition ? (
                <div className="pool-cache-panel__composition">
                  <div className="pool-subsection-title">{t('dashboard.cache_composition')}</div>
                  <StackedMeter segments={cacheSegments} valueFormatter={fmtTokens} ariaLabel={t('dashboard.cache_composition')} />
                </div>
              ) : null}
              {officialCacheByModel.length ? (
                <div className="pool-cache-panel__models">
                  <div className="pool-subsection-title">{t('dashboard.model_cache_rate')}</div>
                  <CacheBars data={officialCacheByModel} />
                </div>
              ) : null}
            </div>
          </div>
        </section>
      ) : null}

      {(hasStatusDistribution || hasProviderDistribution || hasRegistrationTrend) ? (
        <div className="pool-grid cols-3 pool-dashboard-chart-grid">
          {hasStatusDistribution ? <div className="pool-chart-card"><div className="head"><div className="t">{t('dashboard.account_distribution')}</div></div><Donut data={statusDonut} unit={t('dashboard.account_unit')} ariaLabel={t('dashboard.account_distribution')} /></div> : null}
          {hasProviderDistribution ? <div className="pool-chart-card"><div className="head"><div className="t">{t('dashboard.provider_distribution')}</div></div><Donut data={providerDonut} unit={t('dashboard.account_unit')} ariaLabel={t('dashboard.provider_distribution')} /></div> : null}
          {hasRegistrationTrend ? <div className="pool-chart-card"><div className="head"><div className="t">{t('dashboard.registration_trend')}</div></div>
            <BarChart ariaLabel={t('dashboard.registration_trend')} data={registrationDays} series={[{ key: 'success', name: t('dashboard.success'), color: C.green }, { key: 'failed', name: t('dashboard.failed'), color: C.red }]} stacked />
          </div> : null}
        </div>
      ) : null}

      {(hasModelTokens || topAccountRows.length || hostMeters.length) ? (
        <div className="pool-grid cols-3 pool-dashboard-chart-grid">
          {hasModelTokens ? (
            <div className="pool-chart-card">
              <div className="head"><div><div className="t">{t('dashboard.model_token_share')}</div><div className="s">{t('dashboard.model_token_share_desc')}</div></div></div>
              <RankedBars rows={modelTokenRows} valueFormatter={modelTokenFormatter} ariaLabel={t('dashboard.model_token_share')} />
            </div>
          ) : null}
          {topAccountRows.length ? (
            <div className="pool-chart-card">
              <div className="head"><div><div className="t">{t('dashboard.top_accounts')}</div><div className="s">{t('dashboard.top_accounts_desc')}</div></div></div>
              <RankedBars rows={topAccountRows} valueFormatter={fmtTokens} ariaLabel={t('dashboard.top_accounts')} />
            </div>
          ) : null}
          {hostMeters.length ? (
            <div className="pool-chart-card">
              <div className="head"><div><div className="t">{t('dashboard.host_resources')}</div><div className="s">{t('dashboard.host_resources_desc')}</div></div></div>
              <RankedBars
                rows={hostMeters.map((meter) => ({ key: meter.key, name: meter.label, value: Math.max(meter.pct, 0.01), color: meter.color }))}
                max={100}
                valueFormatter={(value: number) => `${Math.round(value)}%`}
                ariaLabel={t('dashboard.host_resources')}
              />
            </div>
          ) : null}
        </div>
      ) : null}

      {secondary?.systemAvailable && secondary.system?.supported ? (
        <details className="pool-disclosure">
          <summary>{t('dashboard.system_health_details')}</summary>
          <SystemHealthSummary system={secondary.system} variant="compact" action={<Button size="small" onClick={() => navigate('/system')}>{t('dashboard.view_system')}</Button>} />
        </details>
      ) : null}
    </div>
  );
}
