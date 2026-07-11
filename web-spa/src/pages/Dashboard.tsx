import React, { useCallback, useEffect, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import * as PoolUI from '../components/pool/index.jsx';
import { IconRefresh, IconPlus, IconUser, IconKey, IconSetting, IconLineChartStroked } from '../components/pool/icons.jsx';
import LoadErrorBannerBase from '../components/LoadErrorBanner.jsx';
import PageHeaderBase from '../components/PageHeader.jsx';
import SystemHealthSummaryBase from '../components/SystemHealthSummary.jsx';
import useVisibleInterval, { usePageVisible } from '../hooks/useVisibleInterval.js';
import { UsageAreaChart, DonutChart, GroupedBar, CacheRateBars } from '../components/LazyCharts.jsx';
import { COLORS, modelColor } from '../lib/chartTheme.js';
import { fmtTokens, fmtInt } from '../lib/format.js';
import { t } from '../lib/i18n.js';
import {
  DASHBOARD_REFRESH_MS, useDashboardCoreData, useDashboardSecondaryData,
} from '../features/observability/queries/dashboard';

const { Tag, Button, LoadingState } = PoolUI as any;
const LoadErrorBanner = LoadErrorBannerBase as any;
const PageHeader = PageHeaderBase as any;
const SystemHealthSummary = SystemHealthSummaryBase as any;
const AreaChart = UsageAreaChart as any;
const Donut = DonutChart as any;
const BarChart = GroupedBar as any;
const CacheBars = CacheRateBars as any;
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
    const [coreResult] = await Promise.all([coreQuery.reload(), secondaryQuery.reload()]);
    return coreResult;
  }, [coreQuery.reload, secondaryQuery.reload]);

  useVisibleInterval(() => {
    setCountdown((value) => value <= 1 ? DASHBOARD_REFRESH_MS / 1000 : value - 1);
  }, 1000, { fireOnVisible: false });

  useEffect(() => {
    if (!loading && lastRefresh) setCountdown(DASHBOARD_REFRESH_MS / 1000);
  }, [loading, lastRefresh]);

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
  const quarantined = summary?.quarantined || 0;
  const cooling = summary?.cooling || 0;
  const recheck = summary?.recheck || 0;
  const codex = summary?.codex || 0;
  const claude = summary?.claude || 0;
  const other = summary?.other || 0;
  const buckets = core?.buckets || [];
  const tokens = buckets.reduce((sum, bucket) => sum + (bucket.total_tokens || 0), 0);
  const requests = buckets.reduce((sum, bucket) => sum + (bucket.requests || 0), 0);
  const cacheByModel = secondary?.cache?.by_model || [];
  const cacheSummary = secondary?.cache?.summary || {};
  const cacheInput = cacheSummary.cache_input_tokens ?? cacheByModel.reduce((sum, row) => sum + (row.cache_input_tokens || row.prompt_tokens || 0), 0);
  const cacheRead = cacheSummary.cache_read_tokens ?? cacheByModel.reduce((sum, row) => sum + (row.cache_read_tokens || row.cached_tokens || 0), 0);
  const cacheHitRate = cacheInput > 0 ? cacheRead / cacheInput : 0;
  const registrationRate = secondary?.registration?.totals?.success_rate || 0;
  const byModel = secondary?.byModel || [];

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
  const modelTokenDonut = byModel.slice(0, 6).map((row) => ({
    name: row.model_label || row.model || `(${t('common.unknown')})`,
    value: row.total_tokens || 0,
    color: modelColor(row.model_key || row.model),
  }));
  const modelTokenFormatter = (value: unknown) => {
    const number = Number(value) || 0;
    if (number >= 1e9) return `${(number / 1e9).toFixed(2)}B`;
    if (number >= 1e6) return `${(number / 1e6).toFixed(2)}M`;
    if (number >= 1e3) return `${(number / 1e3).toFixed(1)}k`;
    return `${number}`;
  };
  const partialError = core?.error || secondary?.error || secondaryQuery.error;
  const hasStatusDistribution = statusDonut.some((item) => item.value > 0);
  const hasProviderDistribution = providerDonut.some((item) => item.value > 0);
  const hasRegistrationTrend = secondary?.registrationAvailable && registrationDays.some((item) => item.success || item.failed);
  const hasModelTokens = secondary?.modelAvailable && modelTokenDonut.some((item) => item.value > 0);

  return (
    <div>
      <PageHeader title={t('dashboard.title')} subtitle={t('dashboard.subtitle')}
        actions={<>
          <Button icon={<IconRefresh />} onClick={reload} loading={loading}>{t('common.refresh')}</Button>
          {!loading ? <span className="pool-text-tertiary" style={{ fontSize: 12 }}>
            {lastRefreshText(lastRefresh)} · {pageVisible ? t('dashboard.auto_refresh').replace('{count}', String(countdown)) : t('dashboard.refresh_paused')}
          </span> : null}
        </>} />

      <LoadErrorBanner error={partialError} onRetry={reload} title={partialError ? t('dashboard.partial_failed') : undefined} />

      <section className="pool-dashboard-command" style={{ marginBottom: 18 }}>
        <div className="pool-dashboard-command__main">
          <div className="pool-dashboard-command__eyebrow">
            {healthTag(Boolean(core?.healthAvailable), core?.health?.ok)}
            <span>{lastRefreshText(lastRefresh)}</span>
          </div>
          <div className="pool-dashboard-command__metric">
            <strong>{fmtInt(active)}</strong>
            <span>{t('dashboard.schedulable_accounts')}</span>
          </div>
          <div className="pool-dashboard-command__subgrid">
            <div><span>{t('dashboard.requests_24h')}</span><b>{core?.timeseriesAvailable ? fmtInt(requests) : '—'}</b></div>
            <div><span>{t('dashboard.tokens_24h')}</span><b>{core?.timeseriesAvailable ? fmtTokens(tokens) : '—'}</b></div>
            <div><span>{t('dashboard.cache_hit')}</span><b>{secondary?.cacheAvailable ? `${Math.round(cacheHitRate * 100)}%` : '—'}</b></div>
            <div><span>{t('dashboard.registration_rate')}</span><b>{secondary?.registrationAvailable ? `${Math.round(registrationRate * 100)}%` : '—'}</b></div>
          </div>
        </div>
        <div className="pool-dashboard-command__side">
          <div className="pool-section-title">{t('dashboard.attention')}</div>
          <div className="pool-attention-list">
            <div>
              <span>{t('dashboard.quarantined_accounts')}</span><b className={quarantined ? 'pool-danger-text' : ''}>{fmtInt(quarantined)}</b>
              <p>{quarantined ? t('dashboard.quarantined_issue') : t('dashboard.quarantined_clear')}</p>
              {quarantined ? <Button size="small" theme="borderless" onClick={() => navigate('/accounts')}>{t('dashboard.view_accounts')}</Button> : null}
            </div>
            <div>
              <span>{t('dashboard.recheck')}</span><b className={recheck ? 'pool-danger-text' : ''}>{fmtInt(recheck)}</b>
              <p>{recheck ? t('dashboard.recheck_issue') : t('dashboard.recheck_clear')}</p>
            </div>
            <div>
              <span>{t('dashboard.cooling')}</span><b>{fmtInt(cooling)}</b>
              <p>{cooling ? t('dashboard.cooling_issue') : t('dashboard.cooling_clear')}</p>
            </div>
            <div>
              <span>{t('dashboard.model_coverage')}</span><b>{secondary?.modelAvailable ? fmtInt(byModel.length) : '—'}</b>
              <p>{t('dashboard.model_coverage_desc')}</p>
            </div>
          </div>
        </div>
      </section>

      <div className="pool-quick-actions" style={{ display: 'flex', gap: 10, marginBottom: 18, flexWrap: 'wrap', alignItems: 'center' }}>
        <span style={{ fontSize: 13, fontWeight: 600, color: 'var(--pool-text-2)', marginRight: 4 }}>{t('dashboard.quick_actions')}</span>
        <Button icon={<IconPlus />} type="primary" onClick={() => navigate('/accounts?action=import')}>{t('dashboard.import_account')}</Button>
        <Button icon={<IconUser />} onClick={() => navigate('/accounts')}>{t('dashboard.account_management')}</Button>
        <Button icon={<IconKey />} onClick={() => navigate('/keys')}>API Keys</Button>
        <Button icon={<IconLineChartStroked />} onClick={() => navigate('/usage')}>{t('dashboard.usage')}</Button>
        <Button icon={<IconSetting />} onClick={() => navigate('/settings-v2')}>{t('dashboard.system_settings')}</Button>
      </div>

      {core?.timeseriesAvailable ? (
        <div className="pool-chart-card" style={{ marginBottom: 18 }}>
          <div className="head"><div><div className="t">{t('dashboard.token_trend')}</div><div className="s">{t('dashboard.token_trend_desc')}</div></div></div>
          <div style={{ height: 280 }}><AreaChart buckets={buckets} height={280} /></div>
        </div>
      ) : null}

      {(hasStatusDistribution || hasProviderDistribution || hasRegistrationTrend) ? (
        <div className="pool-grid cols-3" style={{ marginBottom: 18 }}>
          {hasStatusDistribution ? <div className="pool-chart-card"><div className="head"><div className="t">{t('dashboard.account_distribution')}</div></div><Donut data={statusDonut} unit={t('dashboard.account_unit')} /></div> : null}
          {hasProviderDistribution ? <div className="pool-chart-card"><div className="head"><div className="t">{t('dashboard.provider_distribution')}</div></div><Donut data={providerDonut} unit={t('dashboard.account_unit')} /></div> : null}
          {hasRegistrationTrend ? <div className="pool-chart-card"><div className="head"><div className="t">{t('dashboard.registration_trend')}</div></div>
            <BarChart data={registrationDays} series={[{ key: 'success', name: t('dashboard.success'), color: C.green }, { key: 'failed', name: t('dashboard.failed'), color: C.red }]} stacked />
          </div> : null}
        </div>
      ) : null}

      {(cacheByModel.length || hasModelTokens) ? (
        <div className="pool-grid cols-2" style={{ marginBottom: 18 }}>
          {secondary?.cacheAvailable && cacheByModel.length ? <div className="pool-chart-card">
            <div className="head"><div><div className="t">{t('dashboard.model_cache_rate')}</div><div className="s">{t('dashboard.model_cache_desc')}</div></div></div>
            <div style={{ paddingTop: 6 }}><CacheBars data={cacheByModel} /></div>
          </div> : null}
          {hasModelTokens ? <div className="pool-chart-card"><div className="head"><div className="t">{t('dashboard.model_token_share')}</div></div><Donut data={modelTokenDonut} unit="Token" valueFormatter={modelTokenFormatter} /></div> : null}
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
