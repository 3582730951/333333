import React, { useState, useCallback } from 'react';
import { Button, ConfirmDialog, Select, Toast } from '../components/pool/index.jsx';
import { IconDownload, IconRefresh } from '../components/pool/icons.jsx';
import { errMsg, get, post } from '../api.js';
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
  { label: '今日', value: 'today', bucket: 3600 },
  { label: '近 7 天', value: 604800, bucket: 86400 },
  { label: '近 30 天', value: 2592000, bucket: 86400 },
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

export default function Usage() {
  const [range, setRange] = useState('today');
  const [resetOpen, setResetOpen] = useState(false);
  const [resetting, setResetting] = useState(false);

  const fetchUsage = useCallback(async ({ signal }) => {
    const r = RANGES.find((x) => x.value === range) || RANGES[0];
    const now = Math.floor(Date.now() / 1000);
    const windowParams = r.value === 'today' ? undefined : { since: now - Number(r.value) };
    const bucketParams = r.value === 'today' ? { bucket: r.bucket } : { since: now - Number(r.value), bucket: r.bucket };
    const cacheParams = { bucket: r.bucket };
    const { values, error } = await loadResourceGroup({
      usage: { label: '账号用量', load: () => get('/admin/usage', windowParams, { signal }) },
      timeseries: { label: '趋势数据', load: () => get('/admin/usage/timeseries', bucketParams, { signal }) },
      byModel: { label: '模型统计', load: () => get('/admin/usage/by-model', windowParams, { signal }) },
      cache: { label: '缓存诊断', load: () => get('/admin/usage/cache', cacheParams, { signal }) },
    });
    return {
      rows: normalize(values.usage),
      ts: values.timeseries?.buckets || [],
      byModel: values.byModel?.models || [],
      cache: values.cache || {},
      usageWindow: values.usage || {},
      error,
    };
  }, [range]);

  const {
    data = { rows: [], ts: [], byModel: [], cache: {}, usageWindow: {}, error: null },
    loading,
    error,
    lastRefresh,
    reload: load,
  } = useAsyncResource(fetchUsage, [fetchUsage], { initialData: { rows: [], ts: [], byModel: [], cache: {}, usageWindow: {}, error: null } });
  const rows = data.rows || [];
  const ts = data.ts || [];
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
  const missedPct = promptForCache > 0 ? Math.max(0, 100 - cachedPct) : 0;

  const topAccts = [...rows].sort((a, b) => (b.total_tokens || 0) - (a.total_tokens || 0)).slice(0, 10)
    .map((a) => ({ x: (a.label || a.account_id || '').slice(0, 10), 输入: a.prompt_tokens || 0, 输出: a.completion_tokens || 0 }));

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
      Toast.error(`重置失败：${errMsg(e)}`);
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
    { title: '亲和来源', dataIndex: 'affinity_source', width: 150, render: textOrDash },
    { title: 'Key 来源', dataIndex: 'prompt_cache_key_source', width: 150, render: textOrDash },
    { title: '稳定前缀', dataIndex: 'stable_prefix_source', width: 150, render: (v, r) => `${textOrDash(v)} / ${textOrDash(r.stable_prefix_reason)}` },
    { title: '前缀字节', dataIndex: 'stable_prefix_bytes', width: 110, sorter: (a, b) => (a.stable_prefix_bytes || 0) - (b.stable_prefix_bytes || 0), render: fmtInt },
    { title: 'Retention', dataIndex: 'retention_effective', width: 140, render: (v, r) => `${textOrDash(v)} / ${textOrDash(r.retention_source)}` },
    { title: 'Claude TTL', dataIndex: 'claude_cache_ttl', width: 110, render: textOrDash },
    { title: '请求', dataIndex: 'requests', width: 90, sorter: (a, b) => (a.requests || 0) - (b.requests || 0), render: fmtInt },
    { title: '请求命中', dataIndex: 'request_hit_rate', width: 110, render: fmtPct },
    { title: 'Token 命中', dataIndex: 'real_token_hit_rate', width: 120, sorter: (a, b) => (a.real_token_hit_rate || 0) - (b.real_token_hit_rate || 0), render: fmtPct },
    { title: '读命中', dataIndex: 'cache_read_tokens', width: 120, sorter: (a, b) => (a.cache_read_tokens || 0) - (b.cache_read_tokens || 0), render: fmtTokens },
    { title: '写缓存', dataIndex: 'cache_creation_tokens', width: 120, sorter: (a, b) => (a.cache_creation_tokens || 0) - (b.cache_creation_tokens || 0), render: fmtTokens },
    { title: '未命中', dataIndex: 'cache_miss_tokens', width: 120, sorter: (a, b) => (a.cache_miss_tokens || 0) - (b.cache_miss_tokens || 0), render: fmtTokens },
    { title: '断点', dataIndex: 'cache_breakpoint_count', width: 90, sorter: (a, b) => (a.cache_breakpoint_count || 0) - (b.cache_breakpoint_count || 0), render: fmtInt },
    { title: '最新 User 标记', dataIndex: 'latest_user_cache_control', width: 130, render: (v) => (v ? '是' : '否') },
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
    r.affinity_source || '',
    r.prompt_cache_key_source || '',
    r.stable_prefix_source || '',
    r.stable_prefix_reason || '',
    r.retention_effective || '',
    r.retention_source || '',
    r.claude_cache_ttl || '',
  ].join(':');

  return (
    <div>
      <PageHeader title="用量分析" subtitle="Token 消耗与按账号明细"
        actions={<>
          <Select value={range} onChange={setRange} optionList={RANGES.map((r) => ({ label: r.label, value: r.value }))} style={{ width: 130 }} />
          <Button icon={<IconDownload />} onClick={exportCSV}>导出 CSV</Button>
          <Button icon={<IconRefresh />} onClick={load} loading={loading}>刷新</Button>
        </>} />

      <LoadErrorBanner error={loadError} onRetry={load} />

      <div className="pool-window-strip">
        <div className="pool-window-strip__items">
          <span>VPS 时区 {textOrDash(windowInfo.timezone)} · {fmtOffset(windowInfo.utc_offset_seconds)}</span>
          <span>窗口 {unixDateTime(usageWindow.effective_start_at)} 至 {unixDateTime(usageWindow.effective_until_at)}</span>
          <span>自上次重置缓存统计以来 {unixDateTime(cache.effective_start_at)}</span>
          <span>下次日切 {unixDateTime(windowInfo.next_day_start_at)}</span>
        </div>
        <div className="pool-window-strip__actions">
          <span className="pool-text-tertiary">不删除历史记录</span>
          <Button onClick={() => setResetOpen(true)} loading={resetting}>重置当前缓存统计视图</Button>
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
            <div className="pool-text-tertiary">cache read tokens / cache input tokens</div>
          </div>
          <b>{fmtPct(cacheRate)}</b>
        </div>
        <div className="pool-cache-breakdown__bar" aria-label="cache hit breakdown">
          <span className="pool-cache-breakdown__cached" style={{ width: `${cachedPct}%` }} />
          <span className="pool-cache-breakdown__missed" style={{ width: `${missedPct}%` }} />
        </div>
        <div className="pool-cache-breakdown__legend">
          <span><i className="pool-cache-breakdown__dot pool-cache-breakdown__dot--cached" />读命中 {fmtTokens(cacheRead)}</span>
          <span>写入缓存 {fmtTokens(cacheCreation)}</span>
          <span><i className="pool-cache-breakdown__dot pool-cache-breakdown__dot--missed" />未命中 {fmtTokens(cacheMiss)}</span>
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
          <div className="head"><div><div className="t">模型缓存命中率</div><div className="s">cache read / cache input · 颜色区分模型（命中率越高成本越低）</div></div></div>
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

      <Panel title="按路由缓存诊断">
        <ResourceTable
          loading={loading}
          lastRefresh={lastRefresh}
          dataSource={cacheByRoute}
          columns={cacheRouteCols}
          rowKey={routeDiagKey}
          pagination={{ pageSize: 10 }}
          emptyTitle="暂无路由缓存诊断"
          skeletonRows={6}
          skeletonCols={8}
          density="compact"
          minScrollX={1800}
        />
      </Panel>

      <div className="pool-grid cols-2" style={{ margin: '18px 0' }}>
        <Panel title="按路由 / 账号 / 模型">
          <ResourceTable
            loading={loading}
            lastRefresh={lastRefresh}
            dataSource={cacheByRouteAccountModel}
            columns={cacheRouteAccountModelCols}
            rowKey={routeDiagKey}
            pagination={{ pageSize: 8 }}
            emptyTitle="暂无路由账号诊断"
            skeletonRows={6}
            skeletonCols={8}
            density="compact"
            minScrollX={1800}
          />
        </Panel>
        <Panel title="时间桶缓存趋势">
          <ResourceTable
            loading={loading}
            lastRefresh={lastRefresh}
            dataSource={cacheByTimeBucket}
            columns={cacheTimeCols}
            rowKey={(r) => r.bucket}
            pagination={{ pageSize: 8 }}
            emptyTitle="暂无时间桶缓存数据"
            skeletonRows={6}
            skeletonCols={7}
            density="compact"
            minScrollX={1080}
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

      <ConfirmDialog
        open={resetOpen}
        title="重置当前缓存统计视图"
        description={(
          <div className="pool-confirm-copy">
            <p>这只会推进管理员缓存诊断的统计起点。</p>
            <p>不会删除 usage_records、审计记录、7/30 天历史分析或用户端用量。</p>
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
