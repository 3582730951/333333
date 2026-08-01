import React from 'react';
import * as PoolUI from '../components/pool/index.jsx';
import { IconRefresh, IconDownload } from '../components/pool/icons.jsx';
import PageHeader from '../components/PageHeader.jsx';
import ResourceTable from '../components/ResourceTable.jsx';
import MobileResourceCell from '../components/MobileResourceCell.jsx';
import { useQuotaData } from '../features/observability/queries/events.ts';
import { toCSV, downloadCSV } from '../lib/csv.js';
import { fmtTokens, fmtRelative } from '../lib/format.js';
import { t } from '../lib/i18n.js';
import type { QuotaRow } from '../features/observability/model/types';

const { Button, Progress, Tag, Toast } = PoolUI as any;
const DataTable = ResourceTable as any;
const MobileRow = MobileResourceCell as any;
const pctColor = (p: number) => (p >= 90 ? 'var(--pool-danger)' : p >= 70 ? 'var(--pool-warning)' : 'var(--pool-success)');

export default function Quota() {
  const {
    data: rows = [],
    loading,
    error,
    lastRefresh,
    reload: load,
  } = useQuotaData();

  const bar = (p: number | null | undefined) => {
    if (p == null || p < 0) return <span className="pool-muted">{t('common.unknown')}</span>;
    const v = Math.round(p);
    return <div style={{ minWidth: 130 }}><Progress percent={v} stroke={pctColor(v)} showInfo size="small" format={() => v + '%'} /></div>;
  };

  const exportCSV = () => {
    const ok = downloadCSV('quota.csv', toCSV(rows, [
      { title: 'account', get: (r: QuotaRow) => r.label || r.account_id }, { title: 'provider', get: (r: QuotaRow) => r.provider },
      { title: 'plan_type', get: (r: QuotaRow) => r.plan_type }, { title: 'oauth_rate_limit_tier', get: (r: QuotaRow) => r.oauth_rate_limit_tier },
      { title: '5h_used_pct', get: (r: QuotaRow) => r.quota_summary?.primary?.used_percent ?? r.used_percent },
      { title: '7d_used_pct', get: (r: QuotaRow) => r.quota_summary?.secondary?.used_percent ?? r.secondary_7d_used_pct },
      { title: 'remaining_tokens', get: (r: QuotaRow) => r.quota_summary?.primary?.remaining_tokens ?? r.remaining_tokens },
      { title: 'sync_reason', get: (r: QuotaRow) => r.quota_summary?.sync_reason ?? r.status },
    ]));
    if (!ok) Toast.error(t('quota.export_failed'));
  };

  const cols: any[] = [
    { title: t('quota.account'), dataIndex: 'account_id', width: 230, render: (v: unknown, r: QuotaRow) => <b className="pool-break-value">{r.label || String(v || '')}</b> },
    { title: t('quota.provider'), dataIndex: 'provider', width: 96, render: (v: any) => v ? <Tag>{v}</Tag> : '—' },
    { title: t('quota.plan'), dataIndex: 'plan_type', width: 110, render: (v: any) => v ? <Tag>{v}</Tag> : '—' },
    { title: 'OAuth Tier', dataIndex: 'oauth_rate_limit_tier', width: 140, render: (v: any) => v ? <span className="pool-mono">{v}</span> : '—' },
    { title: t('quota.window'), dataIndex: 'limiter_type', width: 150, render: (v: any) => v ? <span className="pool-mono">{v}</span> : '—' },
    { title: t('quota.usage_5h'), dataIndex: 'used_percent', width: 170, sorter: (a: QuotaRow, b: QuotaRow) => ((a.quota_summary?.primary?.used_percent ?? a.used_percent) || 0) - ((b.quota_summary?.primary?.used_percent ?? b.used_percent) || 0), defaultSortOrder: 'descend', render: (v: number, r: QuotaRow) => bar(r.quota_summary?.primary?.used_percent ?? v) },
    { title: t('quota.usage_7d'), dataIndex: 'secondary_7d_used_pct', width: 170, sorter: (a: QuotaRow, b: QuotaRow) => ((a.quota_summary?.secondary?.used_percent ?? a.secondary_7d_used_pct) || 0) - ((b.quota_summary?.secondary?.used_percent ?? b.secondary_7d_used_pct) || 0), render: (v: number, r: QuotaRow) => bar(r.quota_summary?.secondary?.used_percent ?? v) },
    { title: t('quota.remaining_tokens'), dataIndex: 'remaining_tokens', width: 150, sorter: (a: QuotaRow, b: QuotaRow) => ((a.quota_summary?.primary?.remaining_tokens ?? a.remaining_tokens) || 0) - ((b.quota_summary?.primary?.remaining_tokens ?? b.remaining_tokens) || 0), render: (v: number, r: QuotaRow) => {
      const remaining = r.quota_summary?.primary?.remaining_tokens ?? v;
      return remaining == null || remaining < 0 ? '—' : fmtTokens(remaining);
    } },
    { title: t('quota.sync'), dataIndex: 'status', width: 170, render: (v: string, r: QuotaRow) => {
      const reason = r.quota_summary?.sync_reason || v || 'never_polled';
      const color = String(reason).startsWith('error/') || reason === 'token_expired' ? 'red' : reason === 'ok' ? 'green' : 'amber';
      return <Tag color={color}>{reason}</Tag>;
    } },
    { title: t('quota.reset'), dataIndex: 'reset_at', width: 150, render: (v: number, r: QuotaRow) => <span className="pool-nowrap">{(r.quota_summary?.primary?.reset_at ?? v) ? fmtRelative(r.quota_summary?.primary?.reset_at ?? v) : '—'}</span> },
  ];

  return (
    <div>
      <PageHeader title={t('quota.title')} subtitle={t('quota.subtitle')}
        actions={<>
          <Button icon={<IconDownload />} onClick={exportCSV}>{t('common.export')}</Button>
          <Button icon={<IconRefresh />} onClick={load}>{t('common.refresh')}</Button>
        </>} />
      <DataTable
        error={error}
        onRetry={load}
        loading={loading}
        lastRefresh={lastRefresh}
        dataSource={rows}
        columns={cols}
        rowKey={(r: QuotaRow) => `${r.account_id}:${r.provider || ''}:${r.model || ''}:${r.limiter_type || ''}`}
        pagination={{ pageSize: 20 }}
        layout="fit"
        className="pool-quota-table"
        emptyTitle={t('quota.empty')}
        skeletonRows={8}
        skeletonCols={10}
        mobileRenderer={(row: QuotaRow) => {
          const primary = row.quota_summary?.primary || {};
          const secondary = row.quota_summary?.secondary || {};
          const primaryUsed = primary.used_percent ?? row.used_percent;
          const secondaryUsed = secondary.used_percent ?? row.secondary_7d_used_pct;
          const remaining = primary.remaining_tokens ?? row.remaining_tokens;
          return (
            <MobileRow
              title={row.label || row.account_id || t('common.unnamed_account')}
              subtitle={row.account_id}
              badges={<><Tag>{row.provider || t('common.unknown')}</Tag>{row.plan_type ? <Tag>{row.plan_type}</Tag> : null}</>}
              details={[
                { label: t('quota.usage_5h'), value: primaryUsed == null ? t('common.unknown') : `${Math.round(primaryUsed)}%` },
                { label: t('quota.usage_7d'), value: secondaryUsed == null ? t('common.unknown') : `${Math.round(secondaryUsed)}%` },
                { label: t('quota.remaining_tokens'), value: remaining == null || remaining < 0 ? t('common.unknown') : fmtTokens(remaining) },
                { label: t('quota.window'), value: row.limiter_type || '—' },
                { label: t('quota.sync_status'), value: row.quota_summary?.sync_reason || row.status || 'never_polled' },
                { label: t('quota.reset'), value: (primary.reset_at ?? row.reset_at) ? fmtRelative(primary.reset_at ?? row.reset_at) : '—' },
              ]}
            />
          );
        }}
        mobileListLabel={t('quota.mobile_label')}
      />
    </div>
  );
}
