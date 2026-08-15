import React, { useMemo } from 'react';
import { useNavigate } from 'react-router';
import { Button, LoadingState } from '../../components/pool/index.jsx';
import { IconCheckCircleStroked, IconKey, IconRefresh } from '../../components/pool/icons.jsx';
import LoadErrorBannerBase from '../../components/LoadErrorBanner.jsx';
import PageHeaderBase, { Panel as PanelBase } from '../../components/PageHeader.jsx';
import ResourceTable from '../../components/ResourceTable.jsx';
import MobileResourceCellBase from '../../components/MobileResourceCell.jsx';
import * as MicroCharts from '../../components/MicroCharts.jsx';
import { UsageAreaChart } from '../../components/LazyCharts.jsx';
import { PALETTE, COLORS } from '../../lib/chartTheme.js';
import { fmtTokens, fmtInt } from '../../lib/format.js';
import { t } from '../../lib/i18n.js';
import { usePortalUsageDashboardData } from '../../features/portal/queries/usage';
import { usePortalKeysData } from '../../features/access/queries/keys';
import type { PortalUsageRow } from '../../features/portal/model/usage';

const LoadErrorBanner = LoadErrorBannerBase as any;
const PageHeader = PageHeaderBase as any;
const Panel = PanelBase as any;
const DataTable = ResourceTable as any;
const MobileResourceCell = MobileResourceCellBase as any;
const AreaChart = UsageAreaChart as any;
const { Sparkline, RankedBars, RadialGauge } = MicroCharts as any;
const C = COLORS;

export default function PortalDashboard() {
  const navigate = useNavigate();
  const { data, loading, error, lastRefresh, reload } = usePortalUsageDashboardData();
  const { data: keys = [], loading: keysLoading, error: keysError, reload: reloadKeys } = usePortalKeysData();
  const buckets = data?.buckets;

  const trend = useMemo(() => {
    const series = (buckets || []).map((bucket) => Number(bucket.total_tokens) || 0);
    return { series, peak: series.length ? Math.max(...series) : 0 };
  }, [buckets]);

  if (error && !lastRefresh && !loading) {
    return (
      <div>
        <PageHeader title={t('portal_usage.title')} subtitle={t('portal_usage.subtitle')} actions={<Button icon={<IconRefresh />} onClick={reload}>{t('common.refresh')}</Button>} />
        <LoadErrorBanner error={error} onRetry={reload} title={t('portal_usage.load_failed')} />
      </div>
    );
  }
  if (!data && loading) {
    return (
      <div>
        <PageHeader title={t('portal_usage.title')} subtitle={t('portal_usage.subtitle')} />
        <LoadingState title={t('portal_usage.loading')} />
      </div>
    );
  }

  const rows = data?.rows || [];
  const partialError = data?.error || error;
  const total = rows.reduce((sum, row) => sum + (row.total_tokens || 0), 0);
  const requests = rows.reduce((sum, row) => sum + (row.requests || 0), 0);
  const inputTokens = rows.reduce((sum, row) => sum + (row.prompt_tokens || 0), 0);
  const outputTokens = rows.reduce((sum, row) => sum + (row.completion_tokens || 0), 0);
  const cachedTokens = rows.reduce((sum, row) => sum + (row.cache_read_tokens || row.cached_tokens || 0), 0);
  // "Served from cache" is the one efficiency number a portal user can act on:
  // it is the share of their input that did not have to be reprocessed.
  const cacheShare = inputTokens > 0 ? Math.max(0, Math.min(1, cachedTokens / inputTokens)) : null;
  const avgTokensPerRequest = requests > 0 ? Math.round(total / requests) : 0;
  const hasKey = keys.length > 0;
  const hasFirstRequest = requests > 0;
  const setupSteps = [
    { key: 'key', label: t('portal_onboarding.create_key'), complete: hasKey },
    { key: 'config', label: t('portal_onboarding.copy_config'), complete: hasFirstRequest, active: hasKey && !hasFirstRequest },
    { key: 'request', label: t('portal_onboarding.first_request'), complete: hasFirstRequest },
  ];

  const modelRows = rows
    .slice()
    .sort((a, b) => (b.total_tokens || 0) - (a.total_tokens || 0))
    .slice(0, 6)
    .map((row, index) => ({
      key: row.model_key || row.model,
      name: row.model_label || row.model,
      value: row.total_tokens || 0,
      color: PALETTE[index % PALETTE.length],
      // Keep the meta to the one figure the table below does not lead with, so the
      // two views complement each other instead of restating the same row twice.
      meta: `${fmtInt(row.requests || 0)} ${t('portal_usage.requests')}`,
    }));

  const columns: any[] = [
    { title: t('portal_usage.model'), dataIndex: 'model', render: (value: unknown, row: PortalUsageRow) => <b>{row.model_label || String(value || '')}</b> },
    { title: t('portal_usage.requests'), dataIndex: 'requests', render: fmtInt },
    { title: t('portal_usage.input'), dataIndex: 'prompt_tokens', render: fmtTokens },
    { title: t('portal_usage.output'), dataIndex: 'completion_tokens', render: fmtTokens },
    { title: t('portal_usage.total'), dataIndex: 'total_tokens', render: (value: unknown) => <b>{fmtTokens(value)}</b> },
  ];

  return (
    <div className="pool-portal-page">
      <PageHeader title={t('portal_usage.title')} subtitle={t('portal_usage.subtitle')}
        actions={<Button icon={<IconRefresh />} onClick={() => { reload(); reloadKeys(); }} loading={loading || keysLoading}>{t('common.refresh')}</Button>} />

      <LoadErrorBanner error={partialError} onRetry={reload} title={partialError ? t('portal_usage.partial_failed') : undefined} />
      <LoadErrorBanner error={keysError} onRetry={reloadKeys} title={keysError ? t('portal_onboarding.keys_failed') : undefined} />

      <div className="pool-portal-bento">
      <section className="pool-portal-hero pool-portal-bento__usage">
        <div className="pool-portal-hero__main">
          <span className="pool-portal-hero__eyebrow">{t('portal_usage.last_7_days')}</span>
          <div className="pool-portal-hero__figure">
            <strong>{fmtTokens(total)}</strong>
            <span>{t('portal_usage.total_tokens')}</span>
          </div>
          <dl className="pool-portal-hero__facts">
            <div>
              <dt>{t('portal_usage.requests')}</dt>
              <dd>{fmtInt(requests)}</dd>
            </div>
            <div>
              <dt>{t('portal_usage.models_used')}</dt>
              <dd>{fmtInt(rows.length)}</dd>
            </div>
            <div>
              <dt>{t('portal_usage.avg_per_request')}</dt>
              <dd>{fmtTokens(avgTokensPerRequest)}</dd>
            </div>
          </dl>
          {trend.series.length > 1 ? (
            <div className="pool-portal-hero__spark">
              <Sparkline values={trend.series} color={C.primary} height={54} ariaLabel={t('portal_usage.trend')} />
            </div>
          ) : null}
        </div>
        <div className="pool-portal-hero__aside">
          {cacheShare !== null ? (
            <RadialGauge
              value={cacheShare}
              size={148}
              thickness={13}
              color={C.green}
              label={t('portal_usage.cache_saved_desc')}
              caption={t('portal_usage.cache_saved')}
            />
          ) : (
            <div className="pool-portal-hero__split">
              <div><span>{t('portal_usage.input')}</span><b>{fmtTokens(inputTokens)}</b></div>
              <div><span>{t('portal_usage.output')}</span><b>{fmtTokens(outputTokens)}</b></div>
            </div>
          )}
        </div>
      </section>

      <aside className="pool-portal-onboarding" aria-labelledby="pool-portal-onboarding-title" aria-busy={keysLoading || undefined}>
        <div className="pool-portal-onboarding__head">
          <span className="pool-portal-onboarding__icon" aria-hidden="true">{hasFirstRequest ? <IconCheckCircleStroked /> : <IconKey />}</span>
          <div>
            <h2 id="pool-portal-onboarding-title">{hasFirstRequest ? t('portal_onboarding.complete_title') : t('portal_onboarding.title')}</h2>
            <p>{hasFirstRequest ? t('portal_onboarding.complete_desc') : t('portal_onboarding.desc')}</p>
          </div>
        </div>
        <ol className="pool-portal-onboarding__steps">
          {setupSteps.map((step, index) => (
            <li key={step.key} data-state={step.complete ? 'complete' : step.active ? 'active' : 'pending'}>
              <span aria-hidden="true">{step.complete ? '✓' : index + 1}</span>
              <b>{step.label}</b>
              <small>{step.complete ? t('portal_onboarding.done') : step.active ? t('portal_onboarding.current') : t('portal_onboarding.pending')}</small>
            </li>
          ))}
        </ol>
        {!hasFirstRequest ? <Button theme="solid" block onClick={() => navigate('/portal/keys')}>
          {hasKey ? t('portal_onboarding.view_config') : t('portal_onboarding.create_key')}
        </Button> : null}
      </aside>

      {data?.timeseriesAvailable ? (
        <div className="pool-chart-card pool-portal-trend pool-portal-bento__trend">
          <div className="head"><div><div className="t">{t('portal_usage.trend')}</div><div className="s">{t('portal_usage.trend_desc')}</div></div></div>
          <div style={{ height: 260 }}><AreaChart buckets={data.buckets} height={260} ariaLabel={t('portal_usage.trend')} /></div>
          <table className="pool-sr-only">
            <caption>{t('portal_usage.trend')}</caption>
            <thead><tr><th>{t('portal_usage.period')}</th><th>{t('portal_usage.input')}</th><th>{t('portal_usage.output')}</th><th>{t('portal_usage.cached')}</th></tr></thead>
            <tbody>{(data.buckets || []).map((bucket) => <tr key={bucket.bucket}><th>{String(bucket.bucket || '')}</th><td>{fmtTokens(bucket.prompt_tokens)}</td><td>{fmtTokens(bucket.completion_tokens)}</td><td>{fmtTokens(bucket.cached_tokens)}</td></tr>)}</tbody>
          </table>
        </div>
      ) : <div className="pool-chart-card pool-portal-bento__trend pool-chart-card--empty"><div className="head"><div><div className="t">{t('portal_usage.trend')}</div><div className="s">{t('portal_usage.trend_unavailable')}</div></div></div></div>}

        <div className="pool-chart-card pool-portal-bento__models">
          <div className="head"><div><div className="t">{t('portal_usage.model_share')}</div><div className="s">{t('portal_usage.model_share_desc')}</div></div></div>
          <RankedBars rows={modelRows} valueFormatter={fmtTokens} emptyText={t('portal_usage.empty')} ariaLabel={t('portal_usage.model_share')} />
        </div>
      </div>

      <div className="pool-portal-breakdown">
        <Panel title={t('portal_usage.by_model')}>
          <DataTable
            loading={loading}
            lastRefresh={lastRefresh}
            dataSource={rows}
            columns={columns}
            rowKey={(row: PortalUsageRow) => row.model_key || row.model}
            pagination={false}
            size="small"
            emptyTitle={t('portal_usage.empty')}
            skeletonRows={5}
            skeletonCols={5}
            mobileRenderer={(row: PortalUsageRow) => (
              <MobileResourceCell
                title={row.model_label || row.model}
                subtitle={row.model_key && row.model_key !== row.model ? row.model_key : undefined}
                details={[
                  { label: t('portal_usage.requests'), value: fmtInt(row.requests) },
                  { label: t('portal_usage.input'), value: fmtTokens(row.prompt_tokens) },
                  { label: t('portal_usage.output'), value: fmtTokens(row.completion_tokens) },
                  { label: t('portal_usage.total'), value: fmtTokens(row.total_tokens) },
                ]}
              />
            )}
            mobileListLabel={t('portal_usage.by_model')}
          />
        </Panel>
      </div>
    </div>
  );
}
