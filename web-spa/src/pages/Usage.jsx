import React, { useState, useCallback } from 'react';
import { Button, ConfirmDialog, Select, Toast } from '../components/pool/index.jsx';
import { IconDownload, IconRefresh } from '../components/pool/icons.jsx';
import { get, post } from '../api.js';
import LoadErrorBanner from '../components/LoadErrorBanner.jsx';
import ResourceTable from '../components/ResourceTable.jsx';
import PageHeader, { Panel } from '../components/PageHeader.jsx';
import StatCard from '../components/StatCard.jsx';
import useAsyncResource from '../hooks/useAsyncResource.js';
import { UsageAreaChart, GroupedBar, CacheRateBars, UsageModelAreaChart } from '../components/LazyCharts.jsx';
import { COLORS, modelColor } from '../lib/chartTheme.js';
import { fmtTokens, fmtInt } from '../lib/format.js';
import { toCSV, downloadCSV } from '../lib/csv.js';
import { loadResourceGroup } from '../lib/resource.js';

const C = COLORS;
const RANGES = [
  { label: '今日', value: 'today', bucket: 3600 },
  { label: '近 7 天', value: 604800, bucket: 86400 },
  { label: '近 30 天', value: 2592000, bucket: 86400 },
];
const FULL_CACHE_FIELDS = 'summary,by_account,by_model,by_api_key,by_account_model,by_route,by_route_account_model,by_time_bucket';
const CACHE_METRICS = [
  { label: '总 Token', value: 'total_tokens' },
  { label: '读命中', value: 'cache_read_tokens' },
  { label: '写缓存', value: 'cache_creation_tokens' },
  { label: '输入 Token', value: 'prompt_tokens' },
];

function normalize(data) {
  if (Array.isArray(data)) return data;
  for (const k of ['rows', 'usage', 'data', 'accounts']) if (Array.isArray(data?.[k])) return data[k];
  return [];
}

function fmtPct(v) {
  const n = Math.max(0, Math.min(1, Number(v) || 0));
  if (n > 0 && n < 0.1) return (n * 100).toFixed(1) + '%';
  return Math.round(n * 100) + '%';
}

function textOrDash(v) {
  return v == null || v === '' ? '—' : String(v);
}

function unixDateTime(v) {
  const n = Number(v) || 0;
  return n > 0 ? new Date(n * 1000).toLocaleString() : '—';
}

function fmtOffset(seconds) {
  const n = Number(seconds) || 0;
  const sign = n >= 0 ? '+' : '-';
  const abs = Math.abs(n);
  const hh = String(Math.floor(abs / 3600)).padStart(2, '0');
  const mm = String(Math.floor((abs % 3600) / 60)).padStart(2, '0');
  return `UTC${sign}${hh}:${mm}`;
}

function modelKey(row) {
  return row?.model_key || row?.series_key || row?.model || '__unknown__';
}

function modelLabel(row) {
  return row?.model_label || row?.series_label || row?.model || '(未知)';
}

function mobileDiagnosticRenderer(columns, titleForRow) {
  const visible = (columns || []).slice(0, 5);
  return (row) => (
    <div className="pool-diagnostic-card">
      <div className="pool-diagnostic-card__title">{titleForRow?.(row) || '诊断记录'}</div>
      <div className="pool-diagnostic-card__grid">
        {visible.map((column, index) => {
          const key = column.key || column.dataIndex || column.title || index;
          const raw = typeof column.dataIndex === 'string' ? row?.[column.dataIndex] : undefined;
          const value = column.render ? column.render(raw, row, index) : textOrDash(raw);
          return (
            <div className="pool-diagnostic-card__item" key={String(key)}>
              <span className="pool-diagnostic-card__label">{column.title}</span>
              <span className="pool-diagnostic-card__value">{value}</span>
            </div>
          );
        })}
      </div>
    </div>
  );
}

export default function Usage() {
  const [range, setRange] = useState('today');
  const [resetOpen, setResetOpen] = useState(false);
  const [resetting, setResetting] = useState(false);
  const [trendMode, setTrendMode] = useState('model');
  const [cacheMetric, setCacheMetric] = useState('cache_read_tokens');
  const [selectedCacheModels, setSelectedCacheModels] = useState([]);
  const [hoveredModel, setHoveredModel] = useState(null);
  const [activeDiagnostic, setActiveDiagnostic] = useState('apiKey');

  const fetchUsage = useCallback(async ({ signal }) => {
    const r = RANGES.find((x) => x.value === range) || RANGES[0];
    const now = Math.floor(Date.now() / 1000);
    const windowParams = r.value === 'today' ? undefined : { since: now - Number(r.value) };
    const bucketParams = r.value === 'today'
      ? { bucket: r.bucket, series_dimension: 'model', series_limit: 6 }
      : { since: now - Number(r.value), bucket: r.bucket, series_dimension: 'model', series_limit: 6 };
    const cacheParams = { bucket: r.bucket, fields: FULL_CACHE_FIELDS };
    const { values, error } = await loadResourceGroup({
      usage: { label: '账号用量', load: () => get('/admin/usage', windowParams, { signal }) },
      timeseries: { label: '趋势数据', load: () => get('/admin/usage/timeseries', bucketParams, { signal }) },
      byModel: { label: '模型统计', load: () => get('/admin/usage/by-model', windowParams, { signal }) },
      cache: { label: '缓存诊断', load: () => get('/admin/usage/cache', cacheParams, { signal }) },
    });
    return {
      rows: normalize(values.usage),
      ts: values.timeseries?.buckets || [],
      modelSeries: values.timeseries?.model_series || [],
      series: values.timeseries?.series || [],
      byModel: values.byModel?.models || [],
      cache: values.cache || {},
      usageWindow: values.usage || {},
      error,
    };
  }, [range]);

  const {
    data = { rows: [], ts: [], modelSeries: [], series: [], byModel: [], cache: {}, usageWindow: {}, error: null },
    loading,
    error,
    lastRefresh,
    reload: load,
  } = useAsyncResource(fetchUsage, [fetchUsage], { initialData: { rows: [], ts: [], modelSeries: [], series: [], byModel: [], cache: {}, usageWindow: {}, error: null } });
  const rows = data.rows || [];
  const ts = data.ts || [];
  const modelSeries = data.modelSeries || [];
  const series = data.series || [];
  const byModel = data.byModel || [];
  const cache = data.cache || {};
  const cacheSummary = cache.summary || {};
  const cacheByKey = cache.by_api_key || [];
  const cacheByAccountModel = cache.by_account_model || [];
  const cacheByRoute = cache.by_route || [];
  const cacheByRouteAccountModel = cache.by_route_account_model || [];
  const cacheByTimeBucket = cache.by_time_bucket || [];
  const usageWindow = data.usageWindow || {};
  const windowInfo = usageWindow.window || cache.window || {};
  const loadError = error || data.error;

  const totalTokens = ts.reduce((s, b) => s + (b.total_tokens || 0), 0);
  const totalReqs = ts.reduce((s, b) => s + (b.requests || 0), 0);
  const fallbackCacheRead = ts.reduce((s, b) => s + (b.cache_read_tokens || 0), 0);
  const fallbackCacheCreation = ts.reduce((s, b) => s + (b.cache_creation_tokens || 0), 0);
  const fallbackCacheInput = ts.reduce((s, b) => s + (b.cache_input_tokens ?? b.prompt_tokens ?? 0), 0);
  const cacheRead = cacheSummary.cache_read_tokens ?? fallbackCacheRead;
  const cacheCreation = cacheSummary.cache_creation_tokens ?? fallbackCacheCreation;
  const promptForCache = cacheSummary.cache_input_tokens ?? cacheSummary.prompt_tokens ?? fallbackCacheInput;
  const cacheMiss = cacheSummary.cache_miss_tokens ?? Math.max(0, promptForCache - cacheRead);
  const cacheRate = cacheSummary.token_hit_rate ?? (promptForCache ? cacheRead / promptForCache : 0);
  const realTokenHitRate = cacheSummary.real_token_hit_rate ?? cacheRate;
  const eligibleHitRate = cacheSummary.eligible_cache_hit_rate ?? (cacheRead + cacheCreation > 0 ? cacheRead / (cacheRead + cacheCreation) : 0);
  const cacheWriteShare = cacheSummary.cache_write_share ?? (promptForCache ? cacheCreation / promptForCache : 0);
  const requestHitRate = cacheSummary.request_hit_rate ?? 0;
  const cachedPct = promptForCache > 0 ? Math.max(0, Math.min(100, Math.round((cacheRead / promptForCache) * 100))) : 0;
  const cacheWritePct = promptForCache > 0 ? Math.max(0, Math.min(100, Math.round((cacheCreation / promptForCache) * 100))) : 0;
  const missedPct = promptForCache > 0 ? Math.max(0, 100 - cachedPct - cacheWritePct) : 0;
  const cacheCompositionSegments = (cache.by_model || []).slice(0, 8).map((m) => ({
    key: modelKey(m),
    label: modelLabel(m),
    color: modelColor(modelKey(m)),
    read: m.cache_read_tokens || 0,
    requests: m.requests,
    request_hit_rate: m.request_hit_rate,
    real_token_hit_rate: m.real_token_hit_rate,
    eligible_cache_hit_rate: m.eligible_cache_hit_rate,
    cache_write_share: m.cache_write_share,
    total_tokens: m.total_tokens,
  }));
  const cacheSegmentTotal = cacheCompositionSegments.reduce((s, m) => s + m.read, 0);
  const selectedKeySet = new Set(selectedCacheModels.length ? selectedCacheModels : series.map((s) => s.series_key));
  const hasModelTrend = modelSeries.length > 0 && series.length > 0;

  const toggleCacheModel = (key) => {
    const all = series.map((s) => s.series_key);
    const base = selectedCacheModels.length ? selectedCacheModels : all;
    const next = base.includes(key) ? base.filter((x) => x !== key) : [...base, key];
    setSelectedCacheModels(next.length ? next : all);
  };

  const topAccts = [...rows].sort((a, b) => (b.total_tokens || 0) - (a.total_tokens || 0)).slice(0, 10)
    .map((a) => ({ x: (a.label || a.account_id || '').slice(0, 10), 输入: a.prompt_tokens || 0, 输出: a.completion_tokens || 0 }));
  const hasTopAccts = topAccts.some((item) => item.输入 || item.输出);
  const hasModelCacheBars = byModel.some((item) => (item.cache_input_tokens || item.prompt_tokens || 0) > 0);

  const exportCSV = () => {
    const ok = downloadCSV('usage-by-account.csv', toCSV(rows, [
      { title: 'account', get: (r) => r.label || r.account_id }, { title: 'requests', get: (r) => r.requests },
      { title: 'prompt_tokens', get: (r) => r.prompt_tokens }, { title: 'completion_tokens', get: (r) => r.completion_tokens },
      { title: 'cached_tokens', get: (r) => r.cached_tokens }, { title: 'cache_input_tokens', get: (r) => r.cache_input_tokens }, { title: 'cache_read_tokens', get: (r) => r.cache_read_tokens },
      { title: 'cache_creation_tokens', get: (r) => r.cache_creation_tokens }, { title: 'total_tokens', get: (r) => r.total_tokens },
    ]));
    if (!ok) Toast.error('导出失败，请检查浏览器下载权限');
  };

  const resetCacheStats = async () => {
    setResetting(true);
    try {
      await post('/admin/usage/cache/reset', {});
      Toast.success('缓存统计视图已重置');
      setResetOpen(false);
      await load();
    } catch (e) {
      Toast.error('重置失败，请稍后重试。');
    } finally {
      setResetting(false);
    }
  };

  const cols = [
    { title: '账号', dataIndex: 'account_id', render: (v, r) => <span>{r.label || v}</span> },
    { title: '请求数', dataIndex: 'requests', sorter: (a, b) => (a.requests || 0) - (b.requests || 0), render: fmtInt },
    { title: '输入', dataIndex: 'prompt_tokens', sorter: (a, b) => (a.prompt_tokens || 0) - (b.prompt_tokens || 0), render: fmtTokens },
    { title: '输出', dataIndex: 'completion_tokens', sorter: (a, b) => (a.completion_tokens || 0) - (b.completion_tokens || 0), render: fmtTokens },
    { title: '读命中', dataIndex: 'cache_read_tokens', render: (v) => fmtTokens(v || 0) },
    { title: '写缓存', dataIndex: 'cache_creation_tokens', render: fmtTokens },
    { title: '总计', dataIndex: 'total_tokens', sorter: (a, b) => (a.total_tokens || 0) - (b.total_tokens || 0), defaultSortOrder: 'descend', render: (v) => <b>{fmtTokens(v)}</b> },
  ];

  const cacheKeyCols = [
    { title: 'API Key', dataIndex: 'api_key_hash_prefix', render: (v) => v || '未归因' },
    { title: '请求', dataIndex: 'requests', sorter: (a, b) => (a.requests || 0) - (b.requests || 0), render: fmtInt },
    { title: '请求命中', dataIndex: 'request_hit_rate', sorter: (a, b) => (a.request_hit_rate || 0) - (b.request_hit_rate || 0), render: fmtPct },
    { title: 'Token 命中', dataIndex: 'token_hit_rate', sorter: (a, b) => (a.token_hit_rate || 0) - (b.token_hit_rate || 0), render: fmtPct },
    { title: '真实 Token 命中', dataIndex: 'real_token_hit_rate', sorter: (a, b) => (a.real_token_hit_rate || 0) - (b.real_token_hit_rate || 0), render: fmtPct },
    { title: '读命中 Token', dataIndex: 'cache_read_tokens', sorter: (a, b) => (a.cache_read_tokens || 0) - (b.cache_read_tokens || 0), render: (v) => fmtTokens(v || 0) },
    { title: '写缓存 Token', dataIndex: 'cache_creation_tokens', sorter: (a, b) => (a.cache_creation_tokens || 0) - (b.cache_creation_tokens || 0), render: fmtTokens },
    { title: '未命中 Token', dataIndex: 'cache_miss_tokens', sorter: (a, b) => (a.cache_miss_tokens || 0) - (b.cache_miss_tokens || 0), render: fmtTokens },
    { title: '估算占比', dataIndex: 'estimated_rate', render: fmtPct },
  ];

  const cacheAccountModelCols = [
    { title: '账号', dataIndex: 'account_id', render: (v) => v || '未归因' },
    { title: '模型', dataIndex: 'model', render: (v) => v || 'unknown' },
    { title: '请求', dataIndex: 'requests', sorter: (a, b) => (a.requests || 0) - (b.requests || 0), render: fmtInt },
    { title: '命中请求', dataIndex: 'hit_requests', sorter: (a, b) => (a.hit_requests || 0) - (b.hit_requests || 0), render: fmtInt },
    { title: '请求命中', dataIndex: 'request_hit_rate', render: fmtPct },
    { title: 'Token 命中', dataIndex: 'token_hit_rate', sorter: (a, b) => (a.token_hit_rate || 0) - (b.token_hit_rate || 0), render: fmtPct },
    { title: '真实 Token 命中', dataIndex: 'real_token_hit_rate', sorter: (a, b) => (a.real_token_hit_rate || 0) - (b.real_token_hit_rate || 0), render: fmtPct },
    { title: '读命中', dataIndex: 'cache_read_tokens', sorter: (a, b) => (a.cache_read_tokens || 0) - (b.cache_read_tokens || 0), render: fmtTokens },
    { title: '写缓存', dataIndex: 'cache_creation_tokens', sorter: (a, b) => (a.cache_creation_tokens || 0) - (b.cache_creation_tokens || 0), render: fmtTokens },
    { title: '未命中', dataIndex: 'cache_miss_tokens', sorter: (a, b) => (a.cache_miss_tokens || 0) - (b.cache_miss_tokens || 0), render: fmtTokens },
  ];

  const cacheRouteCols = [
    { title: '路由', dataIndex: 'route_key_hash_prefix', width: 130, render: (v) => v || '未归因' },
    { title: '路由类型', dataIndex: 'route_class', width: 130, render: textOrDash },
    { title: '亲和来源', dataIndex: 'affinity_source', width: 150, render: textOrDash },
    { title: 'Key 来源', dataIndex: 'prompt_cache_key_source', width: 150, render: textOrDash },
    { title: '稳定前缀', dataIndex: 'stable_prefix_source', width: 150, render: (v, r) => `${textOrDash(v)} / ${textOrDash(r.stable_prefix_reason)}` },
    { title: '前缀字节', dataIndex: 'stable_prefix_bytes', width: 110, sorter: (a, b) => (a.stable_prefix_bytes || 0) - (b.stable_prefix_bytes || 0), render: fmtInt },
    { title: 'Retention', dataIndex: 'retention_effective', width: 140, render: (v, r) => `${textOrDash(v)} / ${textOrDash(r.retention_source)}` },
    { title: 'Claude TTL', dataIndex: 'claude_cache_ttl', width: 110, render: textOrDash },
    { title: '请求', dataIndex: 'requests', width: 90, sorter: (a, b) => (a.requests || 0) - (b.requests || 0), render: fmtInt },
    { title: '请求命中', dataIndex: 'request_hit_rate', width: 110, render: fmtPct },
    { title: 'Token 命中', dataIndex: 'real_token_hit_rate', width: 120, sorter: (a, b) => (a.real_token_hit_rate || 0) - (b.real_token_hit_rate || 0), render: fmtPct },
    { title: '写占比', dataIndex: 'cache_write_share', width: 110, sorter: (a, b) => (a.cache_write_share || 0) - (b.cache_write_share || 0), render: fmtPct },
    { title: '读命中', dataIndex: 'cache_read_tokens', width: 120, sorter: (a, b) => (a.cache_read_tokens || 0) - (b.cache_read_tokens || 0), render: fmtTokens },
    { title: '写缓存', dataIndex: 'cache_creation_tokens', width: 120, sorter: (a, b) => (a.cache_creation_tokens || 0) - (b.cache_creation_tokens || 0), render: fmtTokens },
    { title: '未命中', dataIndex: 'cache_miss_tokens', width: 120, sorter: (a, b) => (a.cache_miss_tokens || 0) - (b.cache_miss_tokens || 0), render: fmtTokens },
    { title: '断点', dataIndex: 'cache_breakpoint_count', width: 90, sorter: (a, b) => (a.cache_breakpoint_count || 0) - (b.cache_breakpoint_count || 0), render: fmtInt },
    { title: '最新 User 标记', dataIndex: 'latest_user_cache_control', width: 130, render: (v) => (v ? '是' : '否') },
    { title: '风险', dataIndex: 'risk_flags', width: 180, render: (v) => (Array.isArray(v) && v.length ? v.join(' / ') : '—') },
  ];

  const cacheRouteAccountModelCols = [
    { title: '账号', dataIndex: 'account_id', width: 150, render: (v) => v || '未归因' },
    { title: '模型', dataIndex: 'model', width: 150, render: (v) => v || 'unknown' },
    ...cacheRouteCols,
  ];

  const cacheTimeCols = [
    { title: '时间桶', dataIndex: 'bucket', width: 150, render: (v) => (v ? new Date(v * 1000).toLocaleString() : '—') },
    { title: '请求', dataIndex: 'requests', sorter: (a, b) => (a.requests || 0) - (b.requests || 0), render: fmtInt },
    { title: '真实请求', dataIndex: 'real_requests', sorter: (a, b) => (a.real_requests || 0) - (b.real_requests || 0), render: fmtInt },
    { title: '读占比', dataIndex: 'cache_read_share', sorter: (a, b) => (a.cache_read_share || 0) - (b.cache_read_share || 0), render: fmtPct },
    { title: '写占比', dataIndex: 'cache_write_share', sorter: (a, b) => (a.cache_write_share || 0) - (b.cache_write_share || 0), render: fmtPct },
    { title: '读命中', dataIndex: 'cache_read_tokens', sorter: (a, b) => (a.cache_read_tokens || 0) - (b.cache_read_tokens || 0), render: fmtTokens },
    { title: '写缓存', dataIndex: 'cache_creation_tokens', sorter: (a, b) => (a.cache_creation_tokens || 0) - (b.cache_creation_tokens || 0), render: fmtTokens },
    { title: '未命中', dataIndex: 'cache_miss_tokens', sorter: (a, b) => (a.cache_miss_tokens || 0) - (b.cache_miss_tokens || 0), render: fmtTokens },
    { title: '估算占比', dataIndex: 'estimated_rate', render: fmtPct },
  ];

  const routeDiagKey = (r) => [
    r.route_key_hash_prefix || 'none',
    r.account_id || '',
    r.model || '',
    r.route_class || '',
    r.affinity_source || '',
    r.prompt_cache_key_source || '',
    r.stable_prefix_source || '',
    r.stable_prefix_reason || '',
    r.retention_effective || '',
    r.retention_source || '',
    r.claude_cache_ttl || '',
  ].join(':');

  const diagnosticTabs = [
    { key: 'apiKey', label: 'API Key', title: '按 API Key 缓存诊断', data: cacheByKey, columns: cacheKeyCols, rowKey: (r) => r.api_key_hash_prefix || 'none', minScrollX: 1080, mobileTitle: (r) => r.api_key_hash_prefix || '未归因' },
    { key: 'accountModel', label: '账号/模型', title: '按账号 / 模型缓存诊断', data: cacheByAccountModel, columns: cacheAccountModelCols, rowKey: (r) => `${r.account_id || 'none'}:${r.model || 'unknown'}`, minScrollX: 1180, mobileTitle: (r) => `${r.account_id || '未归因'} · ${r.model || 'unknown'}` },
    { key: 'route', label: '路由', title: '按路由缓存诊断', data: cacheByRoute, columns: cacheRouteCols, rowKey: routeDiagKey, minScrollX: 1800, mobileTitle: (r) => r.route_key_hash_prefix || '未归因路由' },
    { key: 'time', label: '时间桶', title: '时间桶缓存趋势', data: cacheByTimeBucket, columns: cacheTimeCols, rowKey: (r) => r.bucket, minScrollX: 1080, mobileTitle: (r) => (r.bucket ? new Date(r.bucket * 1000).toLocaleString() : '时间桶') },
    { key: 'accountUsage', label: '账号用量', title: '按账号用量', data: rows, columns: cols, rowKey: (r) => r.account_id, minScrollX: 860, mobileTitle: (r) => r.label || r.account_id || '账号' },
  ];
  const activeDiagnosticTab = diagnosticTabs.find((tab) => tab.key === activeDiagnostic) || diagnosticTabs[0];

  return (
    <div>
      <PageHeader title="用量" subtitle="用量分析视图：Token 消耗、缓存命中与诊断分层。"
        actions={<>
          <Select value={range} onChange={setRange} optionList={RANGES.map((r) => ({ label: r.label, value: r.value }))} style={{ width: 130 }} />
          <Button icon={<IconDownload />} onClick={exportCSV}>导出 CSV</Button>
          <Button icon={<IconRefresh />} onClick={load} loading={loading}>刷新</Button>
        </>} />

      <LoadErrorBanner error={loadError} onRetry={load} />

      <div className="pool-window-strip">
        <div className="pool-window-strip__items">
          <span>VPS 时区：{textOrDash(windowInfo.timezone)} · {fmtOffset(windowInfo.utc_offset_seconds)}</span>
          <span>窗口 {unixDateTime(usageWindow.effective_start_at)} 至 {unixDateTime(usageWindow.effective_until_at)}</span>
          <span>自上次重置缓存统计以来 {unixDateTime(cache.effective_start_at)}</span>
          <span>下次日切 {unixDateTime(windowInfo.next_day_start_at)}</span>
        </div>
        <div className="pool-window-strip__actions">
          <span className="pool-text-tertiary">不删除历史记录</span>
          <Button onClick={() => setResetOpen(true)} loading={resetting}>
            <span className="pool-sr-only">重置当前缓存统计视图</span>
            <span>重置用量统计视图</span>
          </Button>
        </div>
      </div>

      <div className="pool-stat-grid" style={{ marginBottom: 18 }}>
        <StatCard label="总 Token" value={fmtTokens(totalTokens)} color={C.violet} />
        <StatCard label="请求数" value={fmtInt(totalReqs)} color={C.blue} />
        <StatCard label="请求命中概率" value={fmtPct(requestHitRate)} color={C.green} sub={`${fmtInt(cacheSummary.hit_requests || 0)} / ${fmtInt(cacheSummary.requests || 0)} 请求`} />
        <StatCard label="真实 Token 命中" value={fmtPct(realTokenHitRate)} color={C.cyan} sub={`${fmtTokens(cacheRead)} / ${fmtTokens(promptForCache)} 输入`} />
        <StatCard label="可缓存命中" value={fmtPct(eligibleHitRate)} color={C.teal} sub="read / (read + write)" />
        <StatCard label="写缓存占比" value={fmtPct(cacheWriteShare)} color={C.amber} sub={fmtTokens(cacheCreation)} />
      </div>

      <div className="pool-cache-breakdown" style={{ marginBottom: 18 }}>
        <div className="pool-cache-breakdown__head">
          <div>
            <div className="pool-section-title">缓存命中构成</div>
            <div className="pool-text-tertiary">读命中 Token / 可缓存输入 Token</div>
          </div>
          <b>{fmtPct(cacheRate)}</b>
        </div>
        <div className="pool-cache-breakdown__bar" aria-label="cache hit breakdown">
          <span className="pool-cache-breakdown__cached" style={{ width: `${cachedPct}%` }} />
          <span className="pool-cache-breakdown__write" style={{ width: `${cacheWritePct}%` }} />
          <span className="pool-cache-breakdown__missed" style={{ width: `${missedPct}%` }} />
        </div>
        {cacheCompositionSegments.length ? (
          <div className="pool-cache-model-strip" aria-label="cache read by model">
            {cacheCompositionSegments.map((m) => (
              <span
                key={m.key}
                onMouseEnter={() => setHoveredModel(m)}
                onMouseLeave={() => setHoveredModel(null)}
                style={{
                  width: `${cacheSegmentTotal > 0 ? Math.max(2, (m.read / cacheSegmentTotal) * 100) : 0}%`,
                  background: m.color,
                }}
              />
            ))}
            {hoveredModel ? (
              <div className="pool-cache-model-tooltip">
                <b>{hoveredModel.label}</b>
                <span>Token {fmtTokens(hoveredModel.total_tokens)}</span>
                <span>请求数 {fmtInt(hoveredModel.requests)}</span>
                <span>请求命中率 {fmtPct(hoveredModel.request_hit_rate)}</span>
                <span>真实 Token 命中 {fmtPct(hoveredModel.real_token_hit_rate)}</span>
                <span>可缓存命中 {fmtPct(hoveredModel.eligible_cache_hit_rate)}</span>
                <span>写缓存占比 {fmtPct(hoveredModel.cache_write_share)}</span>
              </div>
            ) : null}
          </div>
        ) : null}
        <div className="pool-cache-breakdown__legend">
          <span><i className="pool-cache-breakdown__dot pool-cache-breakdown__dot--cached" />读命中 {fmtTokens(cacheRead)}</span>
          <span><i className="pool-cache-breakdown__dot pool-cache-breakdown__dot--write" />写入缓存 {fmtTokens(cacheCreation)}</span>
          <span><i className="pool-cache-breakdown__dot pool-cache-breakdown__dot--missed" />未命中 {fmtTokens(cacheMiss)}</span>
          <span>请求命中 {fmtPct(requestHitRate)}</span>
        </div>
      </div>

      <div className="pool-chart-card" style={{ marginBottom: 18 }}>
        <div className="head">
          <div><div className="t">Token 用量趋势</div><div className="s">默认按模型，保留输入 / 输出 / 缓存视图</div></div>
          <div style={{ display: 'flex', gap: 8 }}>
            <Button size="small" theme={trendMode === 'model' ? 'solid' : 'outline'} onClick={() => setTrendMode('model')}>按模型</Button>
            <Button size="small" theme={trendMode === 'type' ? 'solid' : 'outline'} onClick={() => setTrendMode('type')}>按类型</Button>
          </div>
        </div>
        <div style={{ height: 280 }}>
          {trendMode === 'model' && hasModelTrend
            ? <UsageModelAreaChart modelSeries={modelSeries} series={series} height={280} selectedKeys={selectedKeySet} />
            : <UsageAreaChart buckets={ts} height={280} />}
        </div>
      </div>

      {hasModelTrend ? (
        <div className="pool-chart-card" style={{ marginBottom: 18 }}>
          <div className="head">
            <div><div className="t">模型缓存趋势</div><div className="s">Top 6 动态模型 · 多选与指标切换</div></div>
            <Select value={cacheMetric} onChange={setCacheMetric} optionList={CACHE_METRICS} style={{ width: 130 }} />
          </div>
          <div className="pool-model-toggle-row">
            {series.map((s) => {
              const active = selectedKeySet.has(s.series_key);
              const color = modelColor(s.series_key);
              return (
                <button key={s.series_key} type="button" className={`pool-model-toggle ${active ? 'is-active' : ''}`} onClick={() => toggleCacheModel(s.series_key)}>
                  <i style={{ background: color }} />
                  <span>{s.series_label || s.series_key}</span>
                </button>
              );
            })}
          </div>
          <div style={{ height: 260 }}>
            <UsageModelAreaChart modelSeries={modelSeries} series={series} height={260} metric={cacheMetric} selectedKeys={selectedKeySet} />
          </div>
        </div>
      ) : null}

      {(hasTopAccts || hasModelCacheBars) ? (
        <div className="pool-grid cols-2" style={{ marginBottom: 18 }}>
          {hasTopAccts ? (
            <div className="pool-chart-card"><div className="head"><div className="t">Top 账号（输入 / 输出）</div></div>
              <GroupedBar data={topAccts} series={[{ key: '输入', color: C.blue }, { key: '输出', color: C.green }]} stacked />
            </div>
          ) : null}
          {hasModelCacheBars ? (
            <div className="pool-chart-card">
              <div className="head"><div><div className="t">模型缓存命中率</div><div className="s">读命中 Token / 可缓存输入 Token · 颜色区分模型。</div></div></div>
              <div style={{ paddingTop: 6 }}><CacheRateBars data={byModel} /></div>
            </div>
          ) : null}
        </div>
      ) : null}

      <section className="pool-usage-diagnostics" style={{ marginBottom: 18 }}>
        <div className="pool-usage-diagnostics__head">
          <div>
            <div className="pool-section-title">诊断</div>
            <div className="pool-text-tertiary">默认只显示一个诊断层级，切换标签查看更细维度。</div>
          </div>
          <div className="pool-segmented" role="tablist" aria-label="用量诊断维度">
            {diagnosticTabs.map((tab) => (
              <Button
                key={tab.key}
                size="small"
                className={activeDiagnostic === tab.key ? 'is-active' : ''}
                aria-pressed={activeDiagnostic === tab.key}
                onClick={() => setActiveDiagnostic(tab.key)}
              >
                {tab.label}
              </Button>
            ))}
          </div>
        </div>
        <Panel title={activeDiagnosticTab.title}>
          <ResourceTable
            loading={loading}
            lastRefresh={lastRefresh}
            dataSource={activeDiagnosticTab.data}
            columns={activeDiagnosticTab.columns}
            rowKey={activeDiagnosticTab.rowKey}
            pagination={{ pageSize: activeDiagnostic === 'accountUsage' ? 15 : 8 }}
            emptyTitle="暂无诊断数据"
            emptyDesc="当前时间范围内还没有足够的数据生成该诊断。"
            skeletonRows={6}
            skeletonCols={Math.min(8, activeDiagnosticTab.columns.length)}
            density="compact"
            minScrollX={activeDiagnosticTab.minScrollX}
            mobileRenderer={mobileDiagnosticRenderer(activeDiagnosticTab.columns, activeDiagnosticTab.mobileTitle)}
            mobileListLabel={activeDiagnosticTab.title}
          />
        </Panel>
      </section>

      <ConfirmDialog
        open={resetOpen}
        title="重置用量统计视图？"
        description={(
          <div className="pool-confirm-copy">
            <p>这只会重建诊断统计视图，不会删除用量记录、审计日志或历史分析数据。</p>
            <span className="pool-sr-only">不会删除 usage_records</span>
          </div>
        )}
        confirmText="重置视图"
        cancelText="取消"
        onCancel={() => setResetOpen(false)}
        onConfirm={resetCacheStats}
      />
    </div>
  );
}
