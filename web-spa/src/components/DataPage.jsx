import React, { useCallback } from 'react';
import { Button, Typography } from './pool/index.jsx';
import { IconRefresh } from './pool/icons.jsx';
import { get } from '../api.js';
import ResourceTable from './ResourceTable.jsx';
import useAsyncResource from '../hooks/useAsyncResource.js';

// Normalize any common admin response into an array of rows.
export function rowsOf(data) {
  if (Array.isArray(data)) return data;
  for (const k of ['rows', 'items', 'data', 'accounts', 'jobs', 'events', 'logs', 'policies', 'tasks', 'users', 'providers', 'groups']) {
    if (Array.isArray(data?.[k])) return data[k];
  }
  if (data && typeof data === 'object') {
    // settings-style {key: value} map → key/value rows
    return Object.entries(data).map(([key, value]) => ({ key, value: typeof value === 'object' ? JSON.stringify(value) : String(value) }));
  }
  return [];
}

function autoColumns(rows, max = 8) {
  if (!rows.length) return [];
  return Object.keys(rows[0]).slice(0, max).map((k) => ({
    title: k,
    dataIndex: k,
    render: (v) => (v && typeof v === 'object' ? JSON.stringify(v) : String(v ?? '—')),
  }));
}

// DataPage renders a titled, refreshable table for a GET endpoint, deriving columns
// from the data when none are supplied. Covers the read-heavy admin pages with one
// component. `extraToolbar` lets a page add buttons (e.g. create) above the table.
// `emptyType` can be used to provide context-specific empty state: 'accounts', 'keys', etc.
export default function DataPage({ title, url, columns, transform, extraToolbar, hint, pageSize = 20, emptyType }) {
  const fetchRows = useCallback(async ({ signal }) => {
    const d = await get(url, undefined, { signal });
    let r = rowsOf(d);
    if (transform) r = transform(r, d);
    return r;
  }, [url, transform]);
  const {
    data: rows = [],
    loading,
    error,
    lastRefresh,
    reload: load,
  } = useAsyncResource(fetchRows, [fetchRows], { initialData: [] });

  const cols = columns || autoColumns(rows);

  // Format last refresh time
  const formatLastRefresh = () => {
    if (!lastRefresh) return '';
    const now = new Date();
    const diff = Math.floor((now - lastRefresh) / 1000);
    if (diff < 60) return '刚刚';
    if (diff < 3600) return `${Math.floor(diff / 60)} 分钟前`;
    return `${Math.floor(diff / 3600)} 小时前`;
  };

  return (
    <div>
      <Typography.Title heading={4} className="pool-page-title">{title}</Typography.Title>
      <div className="pool-toolbar">
        <Button icon={<IconRefresh />} onClick={load}>刷新</Button>
        {extraToolbar?.(load)}
        {hint && <Typography.Text type="tertiary">{hint}</Typography.Text>}
        <Typography.Text type="tertiary">共 {rows.length} 条</Typography.Text>
        {lastRefresh && (
          <Typography.Text type="quaternary" size="small" style={{ marginLeft: 'auto' }}>
            {formatLastRefresh()}
          </Typography.Text>
        )}
      </div>
      <ResourceTable
        error={error}
        onRetry={load}
        loading={loading}
        lastRefresh={lastRefresh}
        dataSource={rows}
        columns={cols}
        rowKey={(r, i) => r.id || r.key || r.hash || i}
        pagination={{ pageSize }}
        size="middle"
        emptyTitle={emptyType === 'accounts' ? '账号池为空' : '暂无数据'}
        emptyType={emptyType || 'default'}
      />
    </div>
  );
}
