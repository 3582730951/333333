import React, { useCallback, useMemo } from 'react';
import { Button, Tag } from '@douyinfe/semi-ui';
import { IconRefresh } from '@douyinfe/semi-icons';
import { get } from '../api.js';
import PageHeader from '../components/PageHeader.jsx';
import ResourceTable from '../components/ResourceTable.jsx';
import useAsyncResource from '../hooks/useAsyncResource.js';
import { fmtDateTime } from '../lib/format.js';

const statusColor = (s) => {
  const m = { active: 'green', paid: 'green', pending: 'amber', failed: 'red', expired: 'grey' };
  return m[String(s).toLowerCase()] || 'blue';
};

// Format currency with proper handling
const formatAmount = (v) => {
  if (!v && v !== 0) return '—';
  const num = Number(v);
  if (isNaN(num)) return '—';
  return `¥${num.toFixed(2)}`;
};

export default function Gopay() {
  const fetchRows = useCallback(async ({ signal }) => {
    const d = await get('/admin/gopay', undefined, { signal });
    return Array.isArray(d) ? d : d?.accounts || d?.rows || [];
  }, []);
  const { data: rows = [], loading, error, lastRefresh, reload: load } = useAsyncResource(fetchRows, [fetchRows], { initialData: [] });

  // Calculate summary stats
  const stats = useMemo(() => {
    if (!rows.length) return null;
    const total = rows.reduce((sum, r) => sum + Number(r.amount || 0), 0);
    const active = rows.filter(r => r.status === 'active' || r.status === 'paid').length;
    const pending = rows.filter(r => r.status === 'pending').length;
    return { total, active, pending, count: rows.length };
  }, [rows]);

  // Smart column detection with known fields prioritized
  const base = rows[0] || {};
  const known = [
    { key: 'id', dataIndex: 'id', title: 'ID', width: 110, render: (v) => <span className="pool-mono pool-nowrap pool-text-secondary" title={String(v || '')}>{String(v).slice(0, 12)}...</span> },
    { key: 'email', dataIndex: 'email', title: '邮箱', width: 180, render: (v) => v || '—' },
    { key: 'account_id', dataIndex: 'account_id', title: '账号ID', width: 150, render: (v) => <span className="pool-mono pool-nowrap pool-text-secondary" title={String(v || '')}>{v || '—'}</span> },
    { key: 'status', dataIndex: 'status', title: '状态', width: 90, render: (v) => <Tag color={statusColor(v)}>{String(v || 'unknown')}</Tag> },
    { key: 'plan', dataIndex: 'plan', title: '套餐', width: 100, render: (v) => v || '—' },
    { key: 'amount', dataIndex: 'amount', title: '金额', width: 100, render: (v) => <span className="pool-strong">{formatAmount(v)}</span> },
    { key: 'created_at', dataIndex: 'created_at', title: '创建时间', width: 160, render: (v) => fmtDateTime(v) },
    { key: 'updated_at', dataIndex: 'updated_at', title: '更新时间', width: 160, render: (v) => fmtDateTime(v) },
    { key: 'expires_at', dataIndex: 'expires_at', title: '过期时间', width: 160, render: (v) => fmtDateTime(v) },
  ];
  const knownKeys = new Set(known.map(k => k.key));
  const cols = known.filter(k => k.key in base).concat(
    Object.keys(base).filter(k => !knownKeys.has(k)).map(k => ({
      key: k, dataIndex: k, title: k.replace(/_/g, ' '),
      render: (v) => typeof v === 'object' ? JSON.stringify(v) : String(v ?? '—')
    }))
  );

  return (
    <div>
      <PageHeader title="GoPay 订阅" subtitle="GoPay Plus 自动订阅状态与支付记录"
        actions={<Button icon={<IconRefresh />} onClick={load}>刷新</Button>} />

      {/* Summary stats */}
      {stats && (
        <div className="pool-stat-grid" style={{ marginBottom: 16 }}>
          <div className="pool-stat">
            <div className="accent" style={{ background: 'var(--pool-c1)' }} />
            <div className="stat-top">
              <span className="label">总记录</span>
            </div>
            <div className="value">{stats.count}</div>
          </div>
          <div className="pool-stat">
            <div className="accent" style={{ background: 'var(--pool-c2)' }} />
            <div className="stat-top">
              <span className="label">活跃订阅</span>
            </div>
            <div className="value">{stats.active}</div>
          </div>
          <div className="pool-stat">
            <div className="accent" style={{ background: 'var(--pool-c4)' }} />
            <div className="stat-top">
              <span className="label">待处理</span>
            </div>
            <div className="value">{stats.pending}</div>
          </div>
          <div className="pool-stat">
            <div className="accent" style={{ background: 'var(--pool-c3)' }} />
            <div className="stat-top">
              <span className="label">总收入</span>
            </div>
            <div className="value">{formatAmount(stats.total)}</div>
          </div>
        </div>
      )}

      <ResourceTable
        error={error}
        onRetry={load}
        loading={loading}
        lastRefresh={lastRefresh}
        dataSource={rows}
        columns={cols}
        rowKey={(r, i) => r.id || r.account_id || i}
        pagination={{ pageSize: 20 }}
        layout="fit"
        className="pool-gopay-table"
        emptyTitle="暂无订阅记录"
        emptyDesc="GoPay Plus 订阅记录将显示在这里"
        emptyType="settings"
        skeletonRows={8}
        skeletonCols={Math.max(1, cols.length || 5)}
      />
    </div>
  );
}
