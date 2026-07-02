import React, { useState, useCallback } from 'react';
import { Button, Select, Toast } from '../components/pool/index.jsx';
import { IconRefresh } from '../components/pool/icons.jsx';
import { IconDownload } from '../components/pool/icons.jsx';
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

function fmtPct(v) {
  const n = Number(v) || 0;
  if (n > 0 && n < 0.1) return (n * 100).toFixed(1) + '%';
  return Math.round(n * 100) + '%';
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
      cache: { label: '缓存诊断', load: () => get('/admin/usage/cache', { since: now - r.value }, { signal }) },
    });
    return {
      rows: normalize(values.usage),
      ts: values.timeseries?.buckets || [],
      byModel: values.byModel?.models || [],
      cache: values.cache || {},
      error,
    };
  }, [range]);

  const {
    data = { rows: [], ts: [], byModel: [], cache: {}, error: null },
    loading,
    error,
    lastRefresh,
    reload: load,
  } = useAsyncResource(fetchUsage, [fetchUsage], { initialData: { rows: [], ts: [], byModel: [], cache: {}, error: null } });
  const rows = data.rows || [];
  const ts = data.ts || [];
  const byModel = data.byModel || [];
  const cache = data.cache || {};
  const cacheSummary = cache.summary || {};
  const cacheByKey = cache.by_api_key || [];
  const cacheByAccountModel = cache.by_account_model || [];
  const loadError = error || data.error;

  const totalTokens = ts.reduce((s, b) => s + (b.total_tokens || 0), 0);
  const totalReqs = ts.reduce((s, b) => s + (b.requests || 0), 0);
  const cached = cacheSummary.cached_tokens || ts.reduce((s, b) => s + (b.cached_tokens || 0), 0);
  const promptForCache = cacheSummary.prompt_tokens || ts.reduce((s, b) => s + (b.prompt_tokens || 0), 0);
  const cacheRate = cacheSummary.token_hit_rate ?? (promptForCache ? cached / promptForCache : 0);
  const requestHitRate = cacheSummary.request_hit_rate || 0;
  const missed = Math.max(0, promptForCache - cached);
  const cachedPct = promptForCache > 0 ? Math.max(0, Math.min(100, Math.round((cached / promptForCache) * 100))) : 0;
  const missedPct = promptForCache > 0 ? Math.max(0, 100 - cachedPct) : 0;

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

  const cacheKeyCols = [
    { title: 'API Key', dataIndex: 'api_key_hash_prefix', render: (v) => v || '未归因' },
    { title: '请求', dataIndex: 'requests', sorter: (a, b) => (a.requests || 0) - (b.requests || 0), render: fmtInt },
    { title: '请求命中', dataIndex: 'request_hit_rate', sorter: (a, b) => (a.request_hit_rate || 0) - (b.request_hit_rate || 0), render: fmtPct },
    { title: 'Token 命中', dataIndex: 'token_hit_rate', sorter: (a, b) => (a.token_hit_rate || 0) - (b.token_hit_rate || 0), render: fmtPct },
    { title: '缓存 Token', dataIndex: 'cached_tokens', sorter: (a, b) => (a.cached_tokens || 0) - (b.cached_tokens || 0), render: fmtTokens },
    { title: '估算占比', dataIndex: 'estimated_rate', render: fmtPct },
  ];

  const cacheAccountModelCols = [
    { title: '账号', dataIndex: 'account_id', render: (v) => v || '未归因' },
    { title: '模型', dataIndex: 'model', render: (v) => v || 'unknown' },
    { title: '请求', dataIndex: 'requests', sorter: (a, b) => (a.requests || 0) - (b.requests || 0), render: fmtInt },
    { title: '命中请求', dataIndex: 'hit_requests', sorter: (a, b) => (a.hit_requests || 0) - (b.hit_requests || 0), render: fmtInt },
    { title: '请求命中', dataIndex: 'request_hit_rate', render: fmtPct },
    { title: 'Token 命中', dataIndex: 'token_hit_rate', sorter: (a, b) => (a.token_hit_rate || 0) - (b.token_hit_rate || 0), render: fmtPct },
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
        <StatCard label="请求命中概率" value={fmtPct(requestHitRate)} color={C.green} sub={`${fmtInt(cacheSummary.hit_requests || 0)} / ${fmtInt(cacheSummary.requests || 0)} 请求`} />
        <StatCard label="缓存 Token 占比" value={fmtPct(cacheRate)} color={C.cyan} sub={`${fmtTokens(cached)} / ${fmtTokens(promptForCache)} 输入`} />
      </div>

      <div className="pool-cache-breakdown" style={{ marginBottom: 18 }}>
        <div className="pool-cache-breakdown__head">
          <div>
            <div className="pool-section-title">缓存命中构成</div>
            <div className="pool-text-tertiary">cached tokens / eligible prompt tokens</div>
          </div>
          <b>{fmtPct(cacheRate)}</b>
        </div>
        <div className="pool-cache-breakdown__bar" aria-label="cache hit breakdown">
          <span className="pool-cache-breakdown__cached" style={{ width: `${cachedPct}%` }} />
          <span className="pool-cache-breakdown__missed" style={{ width: `${missedPct}%` }} />
        </div>
        <div className="pool-cache-breakdown__legend">
          <span><i className="pool-cache-breakdown__dot pool-cache-breakdown__dot--cached" />命中 {fmtTokens(cached)}</span>
          <span><i className="pool-cache-breakdown__dot pool-cache-breakdown__dot--missed" />未命中 {fmtTokens(missed)}</span>
          <span>请求命中 {fmtPct(requestHitRate)}</span>
        </div>
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

      <div className="pool-grid cols-2" style={{ marginBottom: 18 }}>
        <Panel title="按 API Key 缓存诊断">
          <ResourceTable
            loading={loading}
            lastRefresh={lastRefresh}
            dataSource={cacheByKey}
            columns={cacheKeyCols}
            rowKey={(r) => r.api_key_hash_prefix || 'none'}
            pagination={{ pageSize: 8 }}
            emptyTitle="暂无缓存诊断"
            skeletonRows={6}
            skeletonCols={6}
            density="compact"
          />
        </Panel>
        <Panel title="按账号 / 模型缓存诊断">
          <ResourceTable
            loading={loading}
            lastRefresh={lastRefresh}
            dataSource={cacheByAccountModel}
            columns={cacheAccountModelCols}
            rowKey={(r) => `${r.account_id || 'none'}:${r.model || 'unknown'}`}
            pagination={{ pageSize: 8 }}
            emptyTitle="暂无缓存诊断"
            skeletonRows={6}
            skeletonCols={6}
            density="compact"
          />
        </Panel>
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
