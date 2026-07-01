import React, { useCallback } from 'react';
import { Button, Tag } from '@douyinfe/semi-ui';
import { IconRefresh } from '@douyinfe/semi-icons';
import { get } from '../api.js';
import PageHeader from '../components/PageHeader.jsx';
import ResourceTable from '../components/ResourceTable.jsx';
import { MetricRail, TextClamp } from '../components/DisplayPrimitives.jsx';
import useAsyncResource from '../hooks/useAsyncResource.js';
import { fmtDateTime } from '../lib/format.js';

const statusColor = (s) => {
  const code = Number(s);
  if (!Number.isFinite(code)) return 'grey';
  return code >= 500 ? 'red' : code >= 400 ? 'amber' : 'green';
};

export default function CFEvents() {
  const fetchRows = useCallback(async ({ signal }) => {
    const d = await get('/admin/cf-events', { limit: 300 }, { signal });
    return Array.isArray(d) ? d : d?.events || [];
  }, []);
  const { data: rows = [], loading, error, lastRefresh, reload: load } = useAsyncResource(fetchRows, [fetchRows], { initialData: [] });
  const blockedCount = rows.filter((row) => Number(row.status) >= 400).length;
  const serverErrorCount = rows.filter((row) => Number(row.status) >= 500).length;
  const challengeCount = rows.filter((row) => String(row.category || '').toLowerCase().includes('challenge')).length;
  const cfMetrics = [
    { label: '事件数', value: rows.length },
    { label: '通过', value: rows.filter((row) => Number(row.status) < 400).length, tone: 'success' },
    { label: '挑战', value: challengeCount, tone: challengeCount ? 'warning' : undefined },
    { label: '4xx / 5xx', value: `${blockedCount} / ${serverErrorCount}`, tone: blockedCount ? 'danger' : undefined },
  ];

  const cols = [
    {
      title: '事件',
      key: 'event',
      width: 440,
      render: (_, row) => (
        <div className="pool-event-cell">
          <div className="pool-event-meta">
            <Tag size="small" color={statusColor(row.status)}>{row.status || '—'}</Tag>
            {row.category ? <Tag size="small">{row.category}</Tag> : null}
            <span>{fmtDateTime(row.created_at)}</span>
          </div>
          <TextClamp>{row.message || '—'}</TextClamp>
        </div>
      ),
    },
    {
      title: '账号 / 出口',
      key: 'identity',
      width: 280,
      render: (_, row) => (
        <div className="pool-resource-summary">
          <TextClamp>{row.account_id || '—'}</TextClamp>
          <div className="pool-resource-summary__meta">
            {row.egress_id ? <Tag size="small">{row.egress_id}</Tag> : '默认出口'}
          </div>
        </div>
      ),
    },
    { title: 'CF Ray', dataIndex: 'cf_ray', width: 180, render: (v) => <TextClamp className="pool-mono">{v || '—'}</TextClamp> },
  ];

  return (
    <div>
      <PageHeader title="Cloudflare 事件" subtitle="账号 / 出口的 CF 拦截与挑战记录"
        actions={<Button icon={<IconRefresh />} onClick={load}>刷新</Button>} />
      <div className="pool-resource-split">
        <ResourceTable
          error={error}
          onRetry={load}
          loading={loading}
          lastRefresh={lastRefresh}
          dataSource={rows}
          columns={cols}
          rowKey="id"
          pagination={{ pageSize: 25 }}
          className="pool-cf-events-table"
          density="compact"
          layout="fit"
          scroll={false}
          rowHeight={64}
          emptyTitle="暂无 CF 事件"
          skeletonRows={8}
          skeletonCols={3}
        />
        <MetricRail items={cfMetrics} />
      </div>
    </div>
  );
}
