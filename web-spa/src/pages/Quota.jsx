import React, { useCallback } from 'react';
import { Button, Progress, Tag, Toast } from '../components/pool/index.jsx';
import { IconRefresh, IconDownload } from '../components/pool/icons.jsx';
import { get } from '../api.js';
import PageHeader from '../components/PageHeader.jsx';
import ResourceTable from '../components/ResourceTable.jsx';
import useAsyncResource from '../hooks/useAsyncResource.js';
import { toCSV, downloadCSV } from '../lib/csv.js';
import { fmtTokens, fmtRelative } from '../lib/format.js';

const pctColor = (p) => (p >= 90 ? 'var(--pool-danger)' : p >= 70 ? 'var(--pool-warning)' : 'var(--pool-success)');

export default function Quota() {
  const fetchRows = useCallback(async ({ signal }) => {
    const d = await get('/admin/quota', { include_missing: 1, page: 1, pageSize: 500 }, { signal });
    return Array.isArray(d) ? d : d?.quota || d?.rows || [];
  }, []);
  const {
    data: rows = [],
    loading,
    error,
    lastRefresh,
    reload: load,
  } = useAsyncResource(fetchRows, [fetchRows], { initialData: [] });

  const bar = (p) => {
    if (p == null || p < 0) return <span className="pool-muted">未知</span>;
    const v = Math.round(p);
    return <div style={{ minWidth: 130 }}><Progress percent={v} stroke={pctColor(v)} showInfo size="small" format={() => v + '%'} /></div>;
  };

  const exportCSV = () => {
    const ok = downloadCSV('quota.csv', toCSV(rows, [
      { title: 'account', get: (r) => r.label || r.account_id }, { title: 'provider', get: (r) => r.provider },
      { title: 'plan_type', get: (r) => r.plan_type }, { title: 'oauth_rate_limit_tier', get: (r) => r.oauth_rate_limit_tier },
      { title: '5h_used_pct', get: (r) => r.quota_summary?.primary?.used_percent ?? r.used_percent },
      { title: '7d_used_pct', get: (r) => r.quota_summary?.secondary?.used_percent ?? r.secondary_7d_used_pct },
      { title: 'remaining_tokens', get: (r) => r.quota_summary?.primary?.remaining_tokens ?? r.remaining_tokens },
      { title: 'sync_reason', get: (r) => r.quota_summary?.sync_reason ?? r.status },
    ]));
    if (!ok) Toast.error('导出失败，请检查浏览器下载权限');
  };

  const cols = [
    { title: '账号', dataIndex: 'account_id', width: 230, render: (v, r) => <b>{r.label || v}</b> },
    { title: '平台', dataIndex: 'provider', width: 96, render: (v) => v ? <Tag>{v}</Tag> : '—' },
    { title: '套餐', dataIndex: 'plan_type', width: 110, render: (v) => v ? <Tag>{v}</Tag> : '—' },
    { title: 'OAuth Tier', dataIndex: 'oauth_rate_limit_tier', width: 140, render: (v) => v ? <span className="pool-mono">{v}</span> : '—' },
    { title: '窗口', dataIndex: 'limiter_type', width: 150, render: (v) => v ? <span className="pool-mono">{v}</span> : '—' },
    { title: '5h 用量', dataIndex: 'used_percent', width: 170, sorter: (a, b) => ((a.quota_summary?.primary?.used_percent ?? a.used_percent) || 0) - ((b.quota_summary?.primary?.used_percent ?? b.used_percent) || 0), defaultSortOrder: 'descend', render: (v, r) => bar(r.quota_summary?.primary?.used_percent ?? v) },
    { title: '7d 用量', dataIndex: 'secondary_7d_used_pct', width: 170, sorter: (a, b) => ((a.quota_summary?.secondary?.used_percent ?? a.secondary_7d_used_pct) || 0) - ((b.quota_summary?.secondary?.used_percent ?? b.secondary_7d_used_pct) || 0), render: (v, r) => bar(r.quota_summary?.secondary?.used_percent ?? v) },
    { title: '剩余 token', dataIndex: 'remaining_tokens', width: 150, sorter: (a, b) => ((a.quota_summary?.primary?.remaining_tokens ?? a.remaining_tokens) || 0) - ((b.quota_summary?.primary?.remaining_tokens ?? b.remaining_tokens) || 0), render: (v, r) => {
      const remaining = r.quota_summary?.primary?.remaining_tokens ?? v;
      return remaining == null || remaining < 0 ? '—' : fmtTokens(remaining);
    } },
    { title: '同步', dataIndex: 'status', width: 170, render: (v, r) => {
      const reason = r.quota_summary?.sync_reason || v || 'never_polled';
      const color = String(reason).startsWith('error/') || reason === 'token_expired' ? 'red' : reason === 'ok' ? 'green' : 'amber';
      return <Tag color={color}>{reason}</Tag>;
    } },
    { title: '重置', dataIndex: 'reset_at', width: 150, render: (v, r) => <span className="pool-nowrap">{(r.quota_summary?.primary?.reset_at ?? v) ? fmtRelative(r.quota_summary?.primary?.reset_at ?? v) : '—'}</span> },
  ];

  return (
    <div>
      <PageHeader title="配额 / 限额" subtitle="各账号上游 5 小时 / 7 天配额使用情况"
        actions={<>
          <Button icon={<IconDownload />} onClick={exportCSV}>导出 CSV</Button>
          <Button icon={<IconRefresh />} onClick={load}>刷新</Button>
        </>} />
      <ResourceTable
        error={error}
        onRetry={load}
        loading={loading}
        lastRefresh={lastRefresh}
        dataSource={rows}
        columns={cols}
        rowKey={(r) => `${r.account_id}:${r.provider || ''}:${r.model || ''}:${r.limiter_type || ''}`}
        pagination={{ pageSize: 20 }}
        layout="fit"
        className="pool-quota-table"
        emptyTitle="暂无配额数据"
        skeletonRows={8}
        skeletonCols={10}
      />
    </div>
  );
}
