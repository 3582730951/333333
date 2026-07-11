import React from 'react';
import * as PoolUI from '../../components/pool/index.jsx';
import { IconRefresh } from '../../components/pool/icons.jsx';
import LoadErrorBannerBase from '../../components/LoadErrorBanner.jsx';
import PageHeaderBase, { Panel as PanelBase } from '../../components/PageHeader.jsx';
import ResourceTable from '../../components/ResourceTable.jsx';
import MobileResourceCellBase from '../../components/MobileResourceCell.jsx';
import StatCardBase from '../../components/StatCard.jsx';
import { UsageAreaChart, DonutChart } from '../../components/LazyCharts.jsx';
import { PALETTE, COLORS } from '../../lib/chartTheme.js';
import { fmtTokens, fmtInt } from '../../lib/format.js';
import { t } from '../../lib/i18n.js';
import { usePortalUsageDashboardData } from '../../features/portal/queries/usage';
import type { PortalUsageRow } from '../../features/portal/model/usage';

const { Button, LoadingState } = PoolUI as any;
const LoadErrorBanner = LoadErrorBannerBase as any;
const PageHeader = PageHeaderBase as any;
const Panel = PanelBase as any;
const DataTable = ResourceTable as any;
const MobileResourceCell = MobileResourceCellBase as any;
const StatCard = StatCardBase as any;
const AreaChart = UsageAreaChart as any;
const ModelDonut = DonutChart as any;
const C = COLORS;

export default function PortalDashboard() {
  const { data, loading, error, lastRefresh, reload } = usePortalUsageDashboardData();
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
  const buckets = data?.buckets || [];
  const partialError = data?.error || error;
  const total = rows.reduce((sum, row) => sum + (row.total_tokens || 0), 0);
  const requests = rows.reduce((sum, row) => sum + (row.requests || 0), 0);
  const modelDonut = rows.slice(0, 6).map((row, index) => ({
    name: row.model_label || row.model,
    value: row.total_tokens || 0,
    color: PALETTE[index % PALETTE.length],
  }));

  const columns: any[] = [
    { title: t('portal_usage.model'), dataIndex: 'model', render: (value: unknown, row: PortalUsageRow) => <b>{row.model_label || String(value || '')}</b> },
    { title: t('portal_usage.requests'), dataIndex: 'requests', render: fmtInt },
    { title: t('portal_usage.input'), dataIndex: 'prompt_tokens', render: fmtTokens },
    { title: t('portal_usage.output'), dataIndex: 'completion_tokens', render: fmtTokens },
    { title: t('portal_usage.total'), dataIndex: 'total_tokens', render: (value: unknown) => <b>{fmtTokens(value)}</b> },
  ];

  return (
    <div>
      <PageHeader title={t('portal_usage.title')} subtitle={t('portal_usage.subtitle')}
        actions={<Button icon={<IconRefresh />} onClick={reload} loading={loading}>{t('common.refresh')}</Button>} />

      <LoadErrorBanner error={partialError} onRetry={reload} title={partialError ? t('portal_usage.partial_failed') : undefined} />

      <div className="pool-stat-grid" style={{ marginBottom: 18 }}>
        <StatCard label={t('portal_usage.period')} value={t('portal_usage.last_7_days')} color={C.cyan} />
        <StatCard label={t('portal_usage.total_tokens')} value={fmtTokens(total)} color={C.violet} />
        <StatCard label={t('portal_usage.requests')} value={fmtInt(requests)} color={C.blue} />
        <StatCard label={t('portal_usage.models_used')} value={fmtInt(rows.length)} color={C.green} />
      </div>

      {data?.timeseriesAvailable ? (
        <div className="pool-chart-card" style={{ marginBottom: 18 }}>
          <div className="head"><div className="t">{t('portal_usage.trend')}</div></div>
          <div style={{ height: 260 }}><AreaChart buckets={buckets} height={260} /></div>
        </div>
      ) : null}

      <div className="pool-grid cols-2">
        <div className="pool-chart-card"><div className="head"><div className="t">{t('portal_usage.model_share')}</div></div><ModelDonut data={modelDonut} unit="tokens" valueFormatter={fmtTokens} /></div>
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
