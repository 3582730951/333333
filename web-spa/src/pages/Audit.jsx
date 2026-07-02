import React, { useState, useCallback, useMemo } from 'react';
import { Button, Tag, Select, Typography, Toast } from '../components/pool/index.jsx';
import { IconRefresh, IconDownload } from '../components/pool/icons.jsx';
import { get } from '../api.js';
import PageHeader from '../components/PageHeader.jsx';
import ResourceTable from '../components/ResourceTable.jsx';
import useAsyncResource from '../hooks/useAsyncResource.js';
import { toCSV, downloadCSV } from '../lib/csv.js';
import { fmtDateTime, fmtRelative } from '../lib/format.js';

const stateColor = (s) => {
  const m = { alive: 'green', banned: 'red', permission_denied: 'red', rate_limited: 'amber', unreachable: 'grey', unknown: 'grey' };
  return m[s] || 'blue';
};

export default function Audit() {
  const [action, setAction] = useState('');
  const fetchRows = useCallback(async ({ signal }) => {
    const d = await get('/admin/audit', { limit: 500 }, { signal });
    return Array.isArray(d) ? d : d?.rows || [];
  }, []);
  const {
    data: rows = [],
    loading,
    error,
    lastRefresh,
    reload: load,
  } = useAsyncResource(fetchRows, [fetchRows], { initialData: [] });

  const actions = useMemo(() => Array.from(new Set(rows.map((r) => r.action).filter(Boolean))), [rows]);
  const filtered = action ? rows.filter((r) => r.action === action) : rows;

  const exportCSV = () => {
    const ok = downloadCSV('audit.csv', toCSV(filtered, [
      { title: 'time', get: (r) => fmtDateTime(r.created_at) }, { title: 'account', get: (r) => r.account_label || r.account_id },
      { title: 'action', get: (r) => r.action }, { title: 'state', get: (r) => r.state },
      { title: 'reason', get: (r) => r.reason }, { title: 'detail', get: (r) => r.detail },
    ]));
    if (!ok) Toast.error('导出失败，请检查浏览器下载权限');
  };

  const cols = [
    { title: '时间', dataIndex: 'created_at', width: 170, sorter: (a, b) => (a.created_at || 0) - (b.created_at || 0), defaultSortOrder: 'descend',
      render: (v) => (
        <div>
          <Typography.Text style={{ fontFamily: 'ui-monospace, SFMono-Regular, Menlo, monospace', fontSize: 12.5 }}>{fmtDateTime(v)}</Typography.Text>
          <Typography.Text type="tertiary" size="small" style={{ display: 'block', fontSize: 11, marginTop: 2 }}>{fmtRelative(v)}</Typography.Text>
        </div>
      )
    },
    { title: '账号', dataIndex: 'account_label', width: 150, render: (v, r) => v || r.account_id || '—' },
    { title: '动作', dataIndex: 'action', width: 118, render: (v) => <Tag>{v}</Tag> },
    { title: '结果', dataIndex: 'state', width: 108, render: (v) => (v ? <Tag color={stateColor(v)}>{v}</Tag> : '—') },
    { title: '原因', dataIndex: 'reason', width: 116, render: (v) => v || '—' },
    { title: '详情', dataIndex: 'detail', width: 220, render: (v) => <Typography.Text ellipsis={{ showTooltip: true }} className="pool-mono pool-audit-detail">{v || '—'}</Typography.Text> },
  ];

  return (
    <div>
      <PageHeader title="审计日志" subtitle="封禁 / 隔离 / 健康测试等事件"
        actions={<>
          <Select value={action} onChange={setAction} placeholder="全部动作" style={{ width: 180 }}
            optionList={[{ label: '全部动作', value: '' }, ...actions.map((a) => ({ label: a, value: a }))]} />
          <Button icon={<IconDownload />} onClick={exportCSV}>导出 CSV</Button>
          <Button icon={<IconRefresh />} onClick={load}>刷新</Button>
        </>} />
      <ResourceTable
        error={error}
        onRetry={load}
        loading={loading}
        lastRefresh={lastRefresh}
        dataSource={filtered}
        columns={cols}
        rowKey="id"
        pagination={{ pageSize: 30 }}
        layout="fit"
        className="pool-audit-table"
        emptyTitle="暂无审计记录"
        skeletonRows={8}
        skeletonCols={6}
      />
    </div>
  );
}
