import React, { useState, useCallback } from 'react';
import { Button, Select, Toast } from '@douyinfe/semi-ui';
import { IconRefresh } from '@douyinfe/semi-icons';
import { IconDownload } from '@douyinfe/semi-icons';
import { get } from '../api.js';
import LoadErrorBanner from '../components/LoadErrorBanner.jsx';
import ResourceTable from '../components/ResourceTable.jsx';
import PageHeader, { Panel } from '../components/PageHeader.jsx';
import StatCard from '../components/StatCard.jsx';
import useAsyncResource from '../hooks/useAsyncResource.js';
import { UsageAreaChart, GroupedBar, CacheRateBars } from '../components/LazyCharts.jsx';
import { COLORS } from '../lib/chartTheme.js';
import { fmtTokens, fmtInt } from '../lib/format.js';
import { toCSV, downloadCSV } from '../lib/csv.js';
import { loadResourceGroup } from '../lib/resource.js';

const C = COLORS;
const RANGES = [
  { label: '近 24 小时', value: 86400, bucket: 3600 },
  { label: '近 7 天', value: 604800, bucket: 86400 },
  { label: '近 30 天', value: 2592000, bucket: 86400 },
];

function normalize(data) {
  if (Array.isArray(data)) return data;
  for (const k of ['rows', 'usage', 'data', 'accounts']) if (Array.isArray(data?.[k])) return data[k];
  return [];
}

export default function Usage() {
  const [range, setRange] = useState(86400);

  const fetchUsage = useCallback(async ({ signal }) => {
    const r = RANGES.find((x) => x.value === range) || RANGES[0];
    const now = Math.floor(Date.now() / 1000);
    const { values, error } = await loadResourceGroup({
      usage: { label: '账号用量', load: () => get('/admin/usage', undefined, { signal }) },
      timeseries: { label: '趋势数据', load: () => get('/admin/usage/timeseries', { since: now - r.value, bucket: r.bucket }, { signal }) },
      byModel: { label: '模型统计', load: () => get('/admin/usage/by-model', { since: now - r.value }, { signal }) },
    });
    return {
      rows: normalize(values.usage),
      ts: values.timeseries?.buckets || [],
      byModel: values.byModel?.models || [],
      error,
    };
  }, [range]);

  const {
    data = { rows: [], ts: [], byModel: [], error: null },
    loading,
    error,
    lastRefresh,
    reload: load,
  } = useAsyncResource(fetchUsage, [fetchUsage], { initialData: { rows: [], ts: [], byModel: [], error: null } });
  const rows = data.rows || [];
  const ts = data.ts || [];
  const byModel = data.byModel || [];
  const loadError = error || data.error;

  const totalTokens = ts.reduce((s, b) => s + (b.total_tokens || 0), 0);
  const totalReqs = ts.reduce((s, b) => s + (b.requests || 0), 0);
  const cached = ts.reduce((s, b) => s + (b.cached_tokens || 0), 0);
  const cacheRate = totalTokens ? Math.round((cached / totalTokens) * 100) : 0;

  const topAccts = [...rows].sort((a, b) => (b.total_tokens || 0) - (a.total_tokens || 0)).slice(0, 10)
    .map((a) => ({ x: (a.label || a.account_id || '').slice(0, 10), 输入: a.prompt_tokens || 0, 输出: a.completion_tokens || 0 }));

  const exportCSV = () => {
    const ok = downloadCSV('usage-by-account.csv', toCSV(rows, [
      { title: 'account', get: (r) => r.label || r.account_id }, { title: 'requests', get: (r) => r.requests },
      { title: 'prompt_tokens', get: (r) => r.prompt_tokens }, { title: 'completion_tokens', get: (r) => r.completion_tokens },
      { title: 'cached_tokens', get: (r) => r.cached_tokens }, { title: 'total_tokens', get: (r) => r.total_tokens },
    ]));
    if (!ok) Toast.error('导出失败，请检查浏览器下载权限');
  };

  const cols = [
    { title: '账号', dataIndex: 'account_id', render: (v, r) => <span>{r.label || v}</span> },
    { title: '请求数', dataIndex: 'requests', sorter: (a, b) => (a.requests || 0) - (b.requests || 0), render: fmtInt },
    { title: '输入', dataIndex: 'prompt_tokens', sorter: (a, b) => (a.prompt_tokens || 0) - (b.prompt_tokens || 0), render: fmtTokens },
    { title: '输出', dataIndex: 'completion_tokens', sorter: (a, b) => (a.completion_tokens || 0) - (b.completion_tokens || 0), render: fmtTokens },
    { title: '缓存', dataIndex: 'cached_tokens', render: fmtTokens },
    { title: '总计', dataIndex: 'total_tokens', sorter: (a, b) => (a.total_tokens || 0) - (b.total_tokens || 0), defaultSortOrder: 'descend', render: (v) => <b>{fmtTokens(v)}</b> },
  ];

  return (
    <div>
      <PageHeader title="用量分析" subtitle="Token 消耗与按账号明细"
        actions={<>
          <Select value={range} onChange={setRange} optionList={RANGES.map((r) => ({ label: r.label, value: r.value }))} style={{ width: 130 }} />
          <Button icon={<IconDownload />} onClick={exportCSV}>导出 CSV</Button>
          <Button icon={<IconRefresh />} onClick={load} loading={loading}>刷新</Button>
        </>} />

      <LoadErrorBanner error={loadError} onRetry={load} />

      <div className="pool-stat-grid" style={{ marginBottom: 18 }}>
        <StatCard label="总 Token" value={fmtTokens(totalTokens)} color={C.violet} />
        <StatCard label="请求数" value={fmtInt(totalReqs)} color={C.blue} />
        <StatCard label="缓存命中率" value={cacheRate + '%'} color={C.green} sub={`${fmtTokens(cached)} 缓存 token`} />
        <StatCard label="活跃账号" value={fmtInt(rows.length)} color={C.cyan} sub="有用量记录" />
      </div>

      <div className="pool-chart-card" style={{ marginBottom: 18 }}>
        <div className="head"><div className="t">Token 用量趋势</div></div>
        <div style={{ height: 280 }}><UsageAreaChart buckets={ts} height={280} /></div>
      </div>

      <div className="pool-grid cols-2" style={{ marginBottom: 18 }}>
        <div className="pool-chart-card"><div className="head"><div className="t">Top 账号（输入 / 输出）</div></div>
          <GroupedBar data={topAccts} series={[{ key: '输入', color: C.blue }, { key: '输出', color: C.green }]} stacked />
        </div>
        <div className="pool-chart-card">
          <div className="head"><div><div className="t">模型缓存命中率</div><div className="s">cached / prompt · 颜色区分模型（命中率越高成本越低）</div></div></div>
          <div style={{ paddingTop: 6 }}><CacheRateBars data={byModel} /></div>
        </div>
      </div>

      <Panel title="按账号用量">
        <ResourceTable
          loading={loading}
          lastRefresh={lastRefresh}
          dataSource={rows}
          columns={cols}
          rowKey={(r) => r.account_id}
          pagination={{ pageSize: 15 }}
          emptyTitle="暂无用量记录"
          skeletonRows={8}
          skeletonCols={6}
        />
      </Panel>
    </div>
  );
}
