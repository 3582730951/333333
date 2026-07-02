import React, { useCallback, useMemo } from 'react';
import { Button, Card, Tag, Typography } from '../components/pool/index.jsx';
import { IconRefresh } from '../components/pool/icons.jsx';
import { get } from '../api.js';
import EmptyState from '../components/EmptyState.jsx';
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

const EMPTY_GOPAY = { rows: [], serviceStatus: null, settings: null, logs: [], raw: null };

function normalizeGopayPayload(payload) {
  if (Array.isArray(payload)) return { ...EMPTY_GOPAY, rows: payload, raw: payload };
  const source = payload && typeof payload === 'object' ? payload : {};
  const rows = Array.isArray(source.rows)
    ? source.rows
    : Array.isArray(source.accounts)
      ? source.accounts
      : Array.isArray(source.subscriptions)
        ? source.subscriptions
        : [];
  const serviceStatus = source.service_status
    || source.serviceStatus
    || source.service_health
    || source.serviceHealth
    || source.health
    || (!rows.length ? source.status : null)
    || null;
  const settings = source.settings || source.config || source.configuration || null;
  const logs = Array.isArray(source.logs) ? source.logs : Array.isArray(source.events) ? source.events : [];
  return { rows, serviceStatus, settings, logs, raw: payload ?? null };
}

function renderValue(value) {
  if (value === undefined || value === null || value === '') return '—';
  if (typeof value === 'object') return JSON.stringify(value, null, 2);
  return String(value);
}

export default function Gopay() {
  const fetchPayload = useCallback(async ({ signal }) => {
    const d = await get('/admin/gopay', undefined, { signal });
    return normalizeGopayPayload(d);
  }, []);
  const { data: payload = EMPTY_GOPAY, loading, error, lastRefresh, reload: load } = useAsyncResource(fetchPayload, [fetchPayload], { initialData: EMPTY_GOPAY });
  const rows = payload.rows || [];
  const serviceStatus = payload.serviceStatus;
  const settings = payload.settings;
  const logs = payload.logs || [];

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
    { key: 'subscription_id', dataIndex: 'subscription_id', title: '订阅ID', width: 150, render: (v) => <span className="pool-mono pool-nowrap pool-text-secondary" title={String(v || '')}>{v || '—'}</span> },
    { key: 'email', dataIndex: 'email', title: '邮箱', width: 180, render: (v) => v || '—' },
    { key: 'account_id', dataIndex: 'account_id', title: '账号ID', width: 150, render: (v) => <span className="pool-mono pool-nowrap pool-text-secondary" title={String(v || '')}>{v || '—'}</span> },
    { key: 'customer_id', dataIndex: 'customer_id', title: '客户ID', width: 150, render: (v) => <span className="pool-mono pool-nowrap pool-text-secondary" title={String(v || '')}>{v || '—'}</span> },
    { key: 'status', dataIndex: 'status', title: '状态', width: 90, render: (v) => <Tag color={statusColor(v)}>{String(v || 'unknown')}</Tag> },
    { key: 'plan', dataIndex: 'plan', title: '套餐', width: 100, render: (v) => v || '—' },
    { key: 'amount', dataIndex: 'amount', title: '金额', width: 100, render: (v) => <span className="pool-strong">{formatAmount(v)}</span> },
    { key: 'created_at', dataIndex: 'created_at', title: '创建时间', width: 160, render: (v) => fmtDateTime(v) },
    { key: 'updated_at', dataIndex: 'updated_at', title: '更新时间', width: 160, render: (v) => fmtDateTime(v) },
    { key: 'expires_at', dataIndex: 'expires_at', title: '过期时间', width: 160, render: (v) => fmtDateTime(v) },
    { key: 'current_period_end', dataIndex: 'current_period_end', title: '周期结束', width: 160, render: (v) => fmtDateTime(v) },
  ];
  const knownKeys = new Set(known.map(k => k.key));
  const cols = known.filter(k => k.key in base).concat(
    Object.keys(base).filter(k => !knownKeys.has(k)).map(k => ({
      key: k, dataIndex: k, title: k.replace(/_/g, ' '),
      render: (v) => typeof v === 'object' ? JSON.stringify(v) : String(v ?? '—')
    }))
  );
  const statusPanel = serviceStatus ? (
    <Card title="服务状态" className="pool-gopay-panel">
      {typeof serviceStatus === 'object' ? (
        <dl className="pool-kv-grid">
          {Object.entries(serviceStatus).map(([key, value]) => (
            <div key={key}>
              <dt>{key}</dt>
              <dd>{renderValue(value)}</dd>
            </div>
          ))}
        </dl>
      ) : (
        <Tag color={statusColor(serviceStatus)}>{String(serviceStatus)}</Tag>
      )}
    </Card>
  ) : null;
  const settingsPanel = settings ? (
    <Card title="设置" className="pool-gopay-panel">
      {typeof settings === 'object' ? (
        <dl className="pool-kv-grid">
          {Object.entries(settings).map(([key, value]) => (
            <div key={key}>
              <dt>{key}</dt>
              <dd>{renderValue(value)}</dd>
            </div>
          ))}
        </dl>
      ) : (
        <Typography.Text>{String(settings)}</Typography.Text>
      )}
    </Card>
  ) : null;
  const logsPanel = logs.length ? (
    <Card title="日志" className="pool-gopay-panel">
      <div className="pool-log-stream__lines pool-mono">
        {logs.map((entry, index) => {
          const item = entry && typeof entry === 'object' ? entry : { message: entry };
          return (
            <div key={item.id || item.timestamp || index} className="pool-log-stream__line">
              {item.timestamp ? <span className="pool-muted">[{fmtDateTime(item.timestamp)}]</span> : null}
              {item.level ? <Tag size="small" color={item.level === 'error' ? 'red' : 'grey'}>{item.level}</Tag> : null}
              <span>{renderValue(item.message ?? item)}</span>
            </div>
          );
        })}
      </div>
    </Card>
  ) : null;

  return (
    <div>
      <PageHeader title="GoPay 订阅" subtitle="GoPay Plus 自动订阅状态与支付记录"
        actions={<Button icon={<IconRefresh />} onClick={load}>刷新</Button>} />

      {/* Summary stats */}
      {stats && (
        <div className="pool-stat-grid" style={{ marginBottom: 16 }}>
          <div className="pool-stat">
            <div className="accent" style={{ background: 'var(--chart-blue)' }} />
            <div className="stat-top">
              <span className="label">总记录</span>
            </div>
            <div className="value">{stats.count}</div>
          </div>
          <div className="pool-stat">
            <div className="accent" style={{ background: 'var(--chart-green)' }} />
            <div className="stat-top">
              <span className="label">活跃订阅</span>
            </div>
            <div className="value">{stats.active}</div>
          </div>
          <div className="pool-stat">
            <div className="accent" style={{ background: 'var(--chart-orange)' }} />
            <div className="stat-top">
              <span className="label">待处理</span>
            </div>
            <div className="value">{stats.pending}</div>
          </div>
          <div className="pool-stat">
            <div className="accent" style={{ background: 'var(--chart-purple)' }} />
            <div className="stat-top">
              <span className="label">总收入</span>
            </div>
            <div className="value">{formatAmount(stats.total)}</div>
          </div>
        </div>
      )}

      {rows.length ? (
        <ResourceTable
          error={error}
          onRetry={load}
          loading={loading}
          lastRefresh={lastRefresh}
          dataSource={rows}
          columns={cols}
          rowKey={(r, i) => r.id || r.subscription_id || r.account_id || i}
          pagination={{ pageSize: 20 }}
          layout="fit"
          className="pool-gopay-table"
          emptyTitle="暂无订阅记录"
          emptyDesc="GoPay Plus 订阅记录将显示在这里"
          emptyType="settings"
          skeletonRows={8}
          skeletonCols={Math.max(1, cols.length || 5)}
        />
      ) : (
        <div className="pool-gopay-fallback">
          <ResourceTable
            error={error}
            onRetry={load}
            loading={loading}
            lastRefresh={lastRefresh}
            dataSource={[]}
            columns={cols.length ? cols : [{ title: '记录', key: 'record' }]}
            rowKey={(r, i) => r.id || r.subscription_id || r.account_id || i}
            pagination={false}
            layout="fit"
            className="pool-gopay-table"
            empty={<EmptyState title="暂无订阅记录" desc="当前 /admin/gopay 响应未返回 rows、accounts 或 subscriptions 记录。" type="settings" />}
            skeletonRows={4}
            skeletonCols={Math.max(1, cols.length || 1)}
          />
          <div className="pool-gopay-panels">
            {statusPanel}
            {settingsPanel}
            {logsPanel}
            {!statusPanel && !settingsPanel && !logsPanel ? (
              <Card title="服务状态" className="pool-gopay-panel">
                <Typography.Text type="tertiary">当前响应未包含服务状态、设置或日志。</Typography.Text>
              </Card>
            ) : null}
          </div>
        </div>
      )}
    </div>
  );
}
