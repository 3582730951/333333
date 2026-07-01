import React, { useCallback } from 'react';
import { Button, Progress, Tag, Toast } from '@douyinfe/semi-ui';
import { IconRefresh, IconDownload } from '@douyinfe/semi-icons';
import { get } from '../api.js';
import PageHeader from '../components/PageHeader.jsx';
import ResourceTable from '../components/ResourceTable.jsx';
import useAsyncResource from '../hooks/useAsyncResource.js';
import { toCSV, downloadCSV } from '../lib/csv.js';
import { fmtTokens, fmtRelative } from '../lib/format.js';

const pctColor = (p) => (p >= 90 ? 'var(--semi-color-danger)' : p >= 70 ? 'var(--semi-color-warning)' : 'var(--semi-color-success)');

export default function Quota() {
  const fetchRows = useCallback(async ({ signal }) => {
    const d = await get('/admin/quota', undefined, { signal });
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
      { title: '5h_used_pct', get: (r) => r.used_percent }, { title: '7d_used_pct', get: (r) => r.secondary_7d_used_pct },
      { title: 'remaining_tokens', get: (r) => r.remaining_tokens }, { title: 'status', get: (r) => r.status },
    ]));
    if (!ok) Toast.error('导出失败，请检查浏览器下载权限');
  };

  const cols = [
    { title: '账号', dataIndex: 'account_id', width: 230, render: (v, r) => <b>{r.label || v}</b> },
    { title: '平台', dataIndex: 'provider', width: 96, render: (v) => v ? <Tag>{v}</Tag> : '—' },
    { title: '5h 用量', dataIndex: 'used_percent', width: 170, sorter: (a, b) => (a.used_percent || 0) - (b.used_percent || 0), defaultSortOrder: 'descend', render: bar },
    { title: '7d 用量', dataIndex: 'secondary_7d_used_pct', width: 170, sorter: (a, b) => (a.secondary_7d_used_pct || 0) - (b.secondary_7d_used_pct || 0), render: bar },
    { title: '剩余 token', dataIndex: 'remaining_tokens', width: 150, sorter: (a, b) => (a.remaining_tokens || 0) - (b.remaining_tokens || 0), render: (v) => (v == null || v < 0 ? '—' : fmtTokens(v)) },
    { title: '状态', dataIndex: 'status', width: 150, render: (v) => v ? <Tag color={String(v).includes('reject') ? 'red' : String(v).includes('warning') ? 'amber' : 'green'}>{v}</Tag> : '—' },
    { title: '重置', dataIndex: 'reset_at', width: 150, render: (v) => <span className="pool-nowrap">{v ? fmtRelative(v) : '—'}</span> },
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
        rowKey={(r) => r.account_id}
        pagination={{ pageSize: 20 }}
        layout="fit"
        className="pool-quota-table"
        emptyTitle="暂无配额数据"
        skeletonRows={8}
        skeletonCols={7}
      />
    </div>
  );
}
