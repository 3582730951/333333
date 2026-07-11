import React, { useState, type ReactNode } from 'react';
import * as PoolUI from '../components/pool/index.jsx';
import { IconDownload, IconRefresh } from '../components/pool/icons.jsx';
import LoadErrorBanner from '../components/LoadErrorBanner.jsx';
import ResourceTable from '../components/ResourceTable.jsx';
import PageHeader, { Panel } from '../components/PageHeader.jsx';
import StatCard from '../components/StatCard.jsx';
import { UsageAreaChart, GroupedBar, CacheRateBars, UsageModelAreaChart } from '../components/LazyCharts.jsx';
import { COLORS, modelColor } from '../lib/chartTheme.js';
import { fmtTokens, fmtInt } from '../lib/format.js';
import { t } from '../lib/i18n.js';
import { toCSV, downloadCSV } from '../lib/csv.js';
import { useResetUsageCacheMutation, useUsageDashboardData } from '../features/observability/queries/usage';
import type { UsageMetricRow, UsageRange } from '../features/observability/model/usage';

const { Button, ConfirmDialog, Select, Toast } = PoolUI as any;
const DataTable = ResourceTable as any;
const Section = Panel as any;
const MetricCard = StatCard as any;
const AreaChart = UsageAreaChart as any;
const ModelAreaChart = UsageModelAreaChart as any;
const BarChart = GroupedBar as any;
const CacheBars = CacheRateBars as any;
const C = COLORS;
const RANGES: Array<{ labelKey: string; value: UsageRange; bucket: number }> = [
  { labelKey: 'usage.today', value: 'today', bucket: 3600 },
  { labelKey: 'usage.last_7_days', value: 604800, bucket: 86400 },
  { labelKey: 'usage.last_30_days', value: 2592000, bucket: 86400 },
];
const CACHE_METRICS = [
  { labelKey: 'usage.total_tokens', value: 'total_tokens' },
  { labelKey: 'usage.cache_read', value: 'cache_read_tokens' },
  { labelKey: 'usage.cache_write', value: 'cache_creation_tokens' },
  { labelKey: 'usage.input_tokens', value: 'prompt_tokens' },
];

interface UsageColumn {
  title: ReactNode;
  dataIndex?: string;
  key?: string;
  width?: number;
  render?: (value: any, row: UsageMetricRow, index: number) => ReactNode;
  sorter?: (left: UsageMetricRow, right: UsageMetricRow) => number;
  [key: string]: unknown;
}

interface DiagnosticTab {
  key: string;
  label: string;
  title: string;
  data: UsageMetricRow[];
  columns: UsageColumn[];
  rowKey: (row: UsageMetricRow) => unknown;
  minScrollX: number;
  mobileTitle: (row: UsageMetricRow) => ReactNode;
}

function fmtPct(v: unknown) {
  const n = Math.max(0, Math.min(1, Number(v) || 0));
  if (n > 0 && n < 0.1) return (n * 100).toFixed(1) + '%';
  return Math.round(n * 100) + '%';
}

function textOrDash(v: unknown) {
  return v == null || v === '' ? '—' : String(v);
}

function unixDateTime(v: unknown) {
  const n = Number(v) || 0;
  return n > 0 ? new Date(n * 1000).toLocaleString() : '—';
}

function fmtOffset(seconds: unknown) {
  const n = Number(seconds) || 0;
  const sign = n >= 0 ? '+' : '-';
  const abs = Math.abs(n);
  const hh = String(Math.floor(abs / 3600)).padStart(2, '0');
  const mm = String(Math.floor((abs % 3600) / 60)).padStart(2, '0');
  return `UTC${sign}${hh}:${mm}`;
}

function modelKey(row: UsageMetricRow) {
  return row?.model_key || row?.series_key || row?.model || '__unknown__';
}

function modelLabel(row: UsageMetricRow) {
  return row?.model_label || row?.series_label || row?.model || `(${t('usage.unknown_model')})`;
}

function mobileDiagnosticRenderer(columns: any[], titleForRow?: (row: UsageMetricRow) => ReactNode) {
  const visible = (columns || []).slice(0, 5);
  return (row: UsageMetricRow) => (
    <div className="pool-diagnostic-card">
      <div className="pool-diagnostic-card__title">{titleForRow?.(row) || t('usage.diagnostic_record')}</div>
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
  const [range, setRange] = useState<UsageRange>('today');
  const [resetOpen, setResetOpen] = useState(false);
  const [trendMode, setTrendMode] = useState('model');
  const [cacheMetric, setCacheMetric] = useState('cache_read_tokens');
  const [selectedCacheModels, setSelectedCacheModels] = useState<string[]>([]);
  const [hoveredModel, setHoveredModel] = useState<any>(null);
  const [activeDiagnostic, setActiveDiagnostic] = useState('apiKey');

  const { data, loading, error, lastRefresh, reload: load } = useUsageDashboardData(range);
  const resetMutation = useResetUsageCacheMutation();
  const resetting = resetMutation.isPending;
  if (error && !lastRefresh && !loading) {
    return (
      <div>
        <PageHeader title={t('usage.title')} subtitle={t('usage.short_subtitle')} actions={<Button icon={<IconRefresh />} onClick={load}>{t('common.refresh')}</Button>} />
        <LoadErrorBanner error={error} onRetry={load} title={t('usage.load_failed')} />
      </div>
    );
  }
  const rows = data?.rows || [];
  const ts = data?.buckets || [];
  const modelSeries = data?.modelSeries || [];
  const series = data?.series || [];
  const byModel = data?.byModel || [];
  const cache = data?.cache || {};
  const cacheSummary = cache.summary || {};
  const cacheByKey = cache.by_api_key || [];
  const cacheByAccountModel = cache.by_account_model || [];
  const cacheByRoute = cache.by_route || [];
  const cacheByRouteAccountModel = cache.by_route_account_model || [];
  const cacheByTimeBucket = cache.by_time_bucket || [];
  const usageWindow = data?.usageWindow || { rows: [] };
  const windowInfo = usageWindow.window || cache.window || {};

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

  const toggleCacheModel = (key: string) => {
    const all = series.map((s) => s.series_key);
    const base = selectedCacheModels.length ? selectedCacheModels : all;
    const next = base.includes(key) ? base.filter((x) => x !== key) : [...base, key];
    setSelectedCacheModels(next.length ? next : all);
  };

  const topAccts = [...rows].sort((a, b) => (b.total_tokens || 0) - (a.total_tokens || 0)).slice(0, 10)
    .map((a) => ({ x: (a.label || a.account_id || '').slice(0, 10), input: a.prompt_tokens || 0, output: a.completion_tokens || 0 }));
  const hasTopAccts = topAccts.some((item) => item.input || item.output);
  const hasModelCacheBars = byModel.some((item) => (item.cache_input_tokens || item.prompt_tokens || 0) > 0);

  const exportCSV = () => {
    const ok = downloadCSV('usage-by-account.csv', toCSV(rows, [
      { title: 'account', get: (r: UsageMetricRow) => r.label || r.account_id }, { title: 'requests', get: (r: UsageMetricRow) => r.requests },
      { title: 'prompt_tokens', get: (r: UsageMetricRow) => r.prompt_tokens }, { title: 'completion_tokens', get: (r: UsageMetricRow) => r.completion_tokens },
      { title: 'cached_tokens', get: (r: UsageMetricRow) => r.cached_tokens }, { title: 'cache_input_tokens', get: (r: UsageMetricRow) => r.cache_input_tokens }, { title: 'cache_read_tokens', get: (r: UsageMetricRow) => r.cache_read_tokens },
      { title: 'cache_creation_tokens', get: (r: UsageMetricRow) => r.cache_creation_tokens }, { title: 'total_tokens', get: (r: UsageMetricRow) => r.total_tokens },
    ]));
    if (!ok) Toast.error(t('usage.export_failed'));
  };

  const resetCacheStats = async () => {
    try {
      await resetMutation.mutateAsync(undefined);
      Toast.success(t('usage.reset_done'));
      setResetOpen(false);
    } catch {
      Toast.error(t('usage.reset_failed'));
    }
  };

  const cols: UsageColumn[] = [
    { title: t('usage.account'), dataIndex: 'account_id', render: (v, r) => <span>{r.label || v}</span> },
    { title: t('usage.requests'), dataIndex: 'requests', sorter: (a, b) => (a.requests || 0) - (b.requests || 0), render: fmtInt },
    { title: t('usage.input'), dataIndex: 'prompt_tokens', sorter: (a, b) => (a.prompt_tokens || 0) - (b.prompt_tokens || 0), render: fmtTokens },
    { title: t('usage.output'), dataIndex: 'completion_tokens', sorter: (a, b) => (a.completion_tokens || 0) - (b.completion_tokens || 0), render: fmtTokens },
    { title: t('usage.cache_read'), dataIndex: 'cache_read_tokens', render: (v) => fmtTokens(v || 0) },
    { title: t('usage.cache_write'), dataIndex: 'cache_creation_tokens', render: fmtTokens },
    { title: t('usage.total'), dataIndex: 'total_tokens', sorter: (a, b) => (a.total_tokens || 0) - (b.total_tokens || 0), defaultSortOrder: 'descend', render: (v) => <b>{fmtTokens(v)}</b> },
  ];

  const cacheKeyCols: UsageColumn[] = [
    { title: 'API Key', dataIndex: 'api_key_hash_prefix', render: (v) => v || t('usage.unattributed') },
    { title: t('usage.request_unit'), dataIndex: 'requests', sorter: (a, b) => (a.requests || 0) - (b.requests || 0), render: fmtInt },
    { title: t('usage.request_hit'), dataIndex: 'request_hit_rate', sorter: (a, b) => (a.request_hit_rate || 0) - (b.request_hit_rate || 0), render: fmtPct },
    { title: t('usage.token_hit'), dataIndex: 'token_hit_rate', sorter: (a, b) => (a.token_hit_rate || 0) - (b.token_hit_rate || 0), render: fmtPct },
    { title: t('usage.real_token_hit'), dataIndex: 'real_token_hit_rate', sorter: (a, b) => (a.real_token_hit_rate || 0) - (b.real_token_hit_rate || 0), render: fmtPct },
    { title: t('usage.cache_read_tokens'), dataIndex: 'cache_read_tokens', sorter: (a, b) => (a.cache_read_tokens || 0) - (b.cache_read_tokens || 0), render: (v) => fmtTokens(v || 0) },
    { title: t('usage.cache_write_tokens'), dataIndex: 'cache_creation_tokens', sorter: (a, b) => (a.cache_creation_tokens || 0) - (b.cache_creation_tokens || 0), render: fmtTokens },
    { title: t('usage.cache_miss_tokens'), dataIndex: 'cache_miss_tokens', sorter: (a, b) => (a.cache_miss_tokens || 0) - (b.cache_miss_tokens || 0), render: fmtTokens },
    { title: t('usage.estimated_share'), dataIndex: 'estimated_rate', render: fmtPct },
  ];

  const cacheAccountModelCols: UsageColumn[] = [
    { title: t('usage.account'), dataIndex: 'account_id', render: (v) => v || t('usage.unattributed') },
    { title: t('usage.model'), dataIndex: 'model', render: (v) => v || t('usage.unknown_model') },
    { title: t('usage.request_unit'), dataIndex: 'requests', sorter: (a, b) => (a.requests || 0) - (b.requests || 0), render: fmtInt },
    { title: t('usage.hit_requests'), dataIndex: 'hit_requests', sorter: (a, b) => (a.hit_requests || 0) - (b.hit_requests || 0), render: fmtInt },
    { title: t('usage.request_hit'), dataIndex: 'request_hit_rate', render: fmtPct },
    { title: t('usage.token_hit'), dataIndex: 'token_hit_rate', sorter: (a, b) => (a.token_hit_rate || 0) - (b.token_hit_rate || 0), render: fmtPct },
    { title: t('usage.real_token_hit'), dataIndex: 'real_token_hit_rate', sorter: (a, b) => (a.real_token_hit_rate || 0) - (b.real_token_hit_rate || 0), render: fmtPct },
    { title: t('usage.cache_read'), dataIndex: 'cache_read_tokens', sorter: (a, b) => (a.cache_read_tokens || 0) - (b.cache_read_tokens || 0), render: fmtTokens },
    { title: t('usage.cache_write'), dataIndex: 'cache_creation_tokens', sorter: (a, b) => (a.cache_creation_tokens || 0) - (b.cache_creation_tokens || 0), render: fmtTokens },
    { title: t('usage.cache_miss'), dataIndex: 'cache_miss_tokens', sorter: (a, b) => (a.cache_miss_tokens || 0) - (b.cache_miss_tokens || 0), render: fmtTokens },
  ];

  const cacheRouteCols: UsageColumn[] = [
    { title: t('usage.route'), dataIndex: 'route_key_hash_prefix', width: 130, render: (v) => v || t('usage.unattributed') },
    { title: t('usage.route_type'), dataIndex: 'route_class', width: 130, render: textOrDash },
    { title: t('usage.affinity_source'), dataIndex: 'affinity_source', width: 150, render: textOrDash },
    { title: t('usage.key_source'), dataIndex: 'prompt_cache_key_source', width: 150, render: textOrDash },
    { title: t('usage.stable_prefix'), dataIndex: 'stable_prefix_source', width: 150, render: (v, r) => `${textOrDash(v)} / ${textOrDash(r.stable_prefix_reason)}` },
    { title: t('usage.prefix_bytes'), dataIndex: 'stable_prefix_bytes', width: 110, sorter: (a, b) => (a.stable_prefix_bytes || 0) - (b.stable_prefix_bytes || 0), render: fmtInt },
    { title: t('usage.retention'), dataIndex: 'retention_effective', width: 140, render: (v, r) => `${textOrDash(v)} / ${textOrDash(r.retention_source)}` },
    { title: t('usage.claude_ttl'), dataIndex: 'claude_cache_ttl', width: 110, render: textOrDash },
    { title: t('usage.write_5m_share'), dataIndex: 'cache_creation_5m_share', width: 110, render: fmtPct },
    { title: t('usage.request_unit'), dataIndex: 'requests', width: 90, sorter: (a, b) => (a.requests || 0) - (b.requests || 0), render: fmtInt },
    { title: t('usage.request_hit'), dataIndex: 'request_hit_rate', width: 110, render: fmtPct },
    { title: t('usage.token_hit'), dataIndex: 'real_token_hit_rate', width: 120, sorter: (a, b) => (a.real_token_hit_rate || 0) - (b.real_token_hit_rate || 0), render: fmtPct },
    { title: t('usage.write_share'), dataIndex: 'cache_write_share', width: 110, sorter: (a, b) => (a.cache_write_share || 0) - (b.cache_write_share || 0), render: fmtPct },
    { title: t('usage.cache_read'), dataIndex: 'cache_read_tokens', width: 120, sorter: (a, b) => (a.cache_read_tokens || 0) - (b.cache_read_tokens || 0), render: fmtTokens },
    { title: t('usage.cache_write'), dataIndex: 'cache_creation_tokens', width: 120, sorter: (a, b) => (a.cache_creation_tokens || 0) - (b.cache_creation_tokens || 0), render: fmtTokens },
    { title: t('usage.cache_miss'), dataIndex: 'cache_miss_tokens', width: 120, sorter: (a, b) => (a.cache_miss_tokens || 0) - (b.cache_miss_tokens || 0), render: fmtTokens },
    { title: t('usage.breakpoint'), dataIndex: 'cache_breakpoint_count', width: 90, sorter: (a, b) => (a.cache_breakpoint_count || 0) - (b.cache_breakpoint_count || 0), render: fmtInt },
    { title: t('usage.latest_user_risk'), dataIndex: 'latest_user_cache_control', width: 130, render: (v) => (v ? t('usage.yes') : t('usage.no')) },
    { title: t('usage.auto_context'), dataIndex: 'latest_user_auto_context_cache_control', width: 110, render: fmtInt },
    { title: t('usage.user_tail'), dataIndex: 'latest_user_tail_cache_control', width: 100, render: fmtInt },
    { title: t('usage.tool_tail'), dataIndex: 'latest_user_tool_result_cache_control', width: 100, render: fmtInt },
    { title: t('usage.risk'), dataIndex: 'risk_flags', width: 180, render: (v) => (Array.isArray(v) && v.length ? v.join(' / ') : '—') },
  ];

  const cacheRouteAccountModelCols: UsageColumn[] = [
    { title: t('usage.account'), dataIndex: 'account_id', width: 150, render: (v) => v || t('usage.unattributed') },
    { title: t('usage.model'), dataIndex: 'model', width: 150, render: (v) => v || t('usage.unknown_model') },
    ...cacheRouteCols,
  ];

  const cacheTimeCols: UsageColumn[] = [
    { title: t('usage.time_bucket'), dataIndex: 'bucket', width: 150, render: (v) => (v ? new Date(v * 1000).toLocaleString() : '—') },
    { title: t('usage.request_unit'), dataIndex: 'requests', sorter: (a, b) => (a.requests || 0) - (b.requests || 0), render: fmtInt },
    { title: t('usage.real_requests'), dataIndex: 'real_requests', sorter: (a, b) => (a.real_requests || 0) - (b.real_requests || 0), render: fmtInt },
    { title: t('usage.read_share'), dataIndex: 'cache_read_share', sorter: (a, b) => (a.cache_read_share || 0) - (b.cache_read_share || 0), render: fmtPct },
    { title: t('usage.write_share'), dataIndex: 'cache_write_share', sorter: (a, b) => (a.cache_write_share || 0) - (b.cache_write_share || 0), render: fmtPct },
    { title: t('usage.cache_read'), dataIndex: 'cache_read_tokens', sorter: (a, b) => (a.cache_read_tokens || 0) - (b.cache_read_tokens || 0), render: fmtTokens },
    { title: t('usage.cache_write'), dataIndex: 'cache_creation_tokens', sorter: (a, b) => (a.cache_creation_tokens || 0) - (b.cache_creation_tokens || 0), render: fmtTokens },
    { title: t('usage.cache_miss'), dataIndex: 'cache_miss_tokens', sorter: (a, b) => (a.cache_miss_tokens || 0) - (b.cache_miss_tokens || 0), render: fmtTokens },
    { title: t('usage.estimated_share'), dataIndex: 'estimated_rate', render: fmtPct },
  ];

  const routeDiagKey = (r: UsageMetricRow) => [
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

  const diagnosticTabs: DiagnosticTab[] = [
    { key: 'apiKey', label: 'API Key', title: t('usage.api_key_diagnostic'), data: cacheByKey, columns: cacheKeyCols, rowKey: (r) => r.api_key_hash_prefix || 'none', minScrollX: 1080, mobileTitle: (r) => r.api_key_hash_prefix || t('usage.unattributed') },
    { key: 'accountModel', label: t('usage.account_model'), title: t('usage.account_model_diagnostic'), data: cacheByAccountModel, columns: cacheAccountModelCols, rowKey: (r) => `${r.account_id || 'none'}:${r.model || 'unknown'}`, minScrollX: 1180, mobileTitle: (r) => `${r.account_id || t('usage.unattributed')} · ${r.model || 'unknown'}` },
    { key: 'route', label: t('usage.route'), title: t('usage.route_diagnostic'), data: cacheByRoute, columns: cacheRouteCols, rowKey: routeDiagKey, minScrollX: 1800, mobileTitle: (r) => r.route_key_hash_prefix || t('usage.unattributed_route') },
    { key: 'time', label: t('usage.time_bucket'), title: t('usage.time_diagnostic'), data: cacheByTimeBucket, columns: cacheTimeCols, rowKey: (r) => r.bucket, minScrollX: 1080, mobileTitle: (r) => (r.bucket ? new Date(r.bucket * 1000).toLocaleString() : t('usage.time_bucket')) },
    { key: 'accountUsage', label: t('usage.account_usage'), title: t('usage.account_usage_title'), data: rows, columns: cols, rowKey: (r) => r.account_id, minScrollX: 860, mobileTitle: (r) => r.label || r.account_id || t('usage.account') },
  ];
  const activeDiagnosticTab = diagnosticTabs.find((tab) => tab.key === activeDiagnostic) || diagnosticTabs[0];

  return (
    <div>
      <PageHeader title={t('usage.title')} subtitle={t('usage.subtitle')}
        actions={<>
          <Select aria-label={t('usage.window')} value={range} onChange={(value: UsageRange) => setRange(value)} optionList={RANGES.map((item) => ({ label: t(item.labelKey), value: item.value }))} style={{ width: 130 }} />
          <Button icon={<IconDownload />} onClick={exportCSV}>{t('common.export')}</Button>
          <Button icon={<IconRefresh />} onClick={load} loading={loading}>{t('common.refresh')}</Button>
        </>} />

      <LoadErrorBanner error={error} onRetry={load} />

      <div className="pool-window-strip">
        <div className="pool-window-strip__items">
          <span>{t('usage.vps_timezone')}：{textOrDash(windowInfo.timezone)} · {fmtOffset(windowInfo.utc_offset_seconds)}</span>
          <span>{t('usage.window')} {unixDateTime(usageWindow.effective_start_at)} {t('usage.to')} {unixDateTime(usageWindow.effective_until_at)}</span>
          <span>{t('usage.since_reset')} {unixDateTime(cache.effective_start_at)}</span>
          <span>{t('usage.next_day')} {unixDateTime(windowInfo.next_day_start_at)}</span>
        </div>
        <div className="pool-window-strip__actions">
          <span className="pool-text-tertiary">{t('usage.no_history_delete')}</span>
          <Button onClick={() => setResetOpen(true)} loading={resetting}>
            <span className="pool-sr-only">{t('usage.reset_current_sr')}</span>
            <span>{t('usage.reset_view')}</span>
          </Button>
        </div>
      </div>

      <div className="pool-stat-grid" style={{ marginBottom: 18 }}>
        <MetricCard label={t('usage.total_tokens')} value={fmtTokens(totalTokens)} color={C.violet} />
        <MetricCard label={t('usage.requests')} value={fmtInt(totalReqs)} color={C.blue} />
        <MetricCard label={t('usage.request_hit_probability')} value={fmtPct(requestHitRate)} color={C.green} sub={`${fmtInt(cacheSummary.hit_requests || 0)} / ${fmtInt(cacheSummary.requests || 0)} ${t('usage.request_unit')}`} />
        <MetricCard label={t('usage.real_token_hit')} value={fmtPct(realTokenHitRate)} color={C.cyan} sub={`${fmtTokens(cacheRead)} / ${fmtTokens(promptForCache)} ${t('usage.input_short')}`} />
        <MetricCard label={t('usage.eligible_hit')} value={fmtPct(eligibleHitRate)} color={C.teal} sub="read / (read + write)" />
        <MetricCard label={t('usage.write_share')} value={fmtPct(cacheWriteShare)} color={C.amber} sub={fmtTokens(cacheCreation)} />
      </div>

      <div className="pool-cache-breakdown" style={{ marginBottom: 18 }}>
        <div className="pool-cache-breakdown__head">
          <div>
            <div className="pool-section-title">{t('usage.cache_composition')}</div>
            <div className="pool-text-tertiary">{t('usage.cache_composition_desc')}</div>
          </div>
          <b>{fmtPct(cacheRate)}</b>
        </div>
        <div className="pool-cache-breakdown__bar" role="img" aria-label={t('usage.cache_breakdown_label')}>
          <span className="pool-cache-breakdown__cached" style={{ width: `${cachedPct}%` }} />
          <span className="pool-cache-breakdown__write" style={{ width: `${cacheWritePct}%` }} />
          <span className="pool-cache-breakdown__missed" style={{ width: `${missedPct}%` }} />
        </div>
        {cacheCompositionSegments.length ? (
          <div className="pool-cache-model-strip" role="img" aria-label={t('usage.cache_by_model_label')}>
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
                <span>{t('usage.requests')} {fmtInt(hoveredModel.requests)}</span>
                <span>{t('usage.request_hit')} {fmtPct(hoveredModel.request_hit_rate)}</span>
                <span>{t('usage.real_token_hit')} {fmtPct(hoveredModel.real_token_hit_rate)}</span>
                <span>{t('usage.eligible_hit')} {fmtPct(hoveredModel.eligible_cache_hit_rate)}</span>
                <span>{t('usage.write_share')} {fmtPct(hoveredModel.cache_write_share)}</span>
              </div>
            ) : null}
          </div>
        ) : null}
        <div className="pool-cache-breakdown__legend">
          <span><i className="pool-cache-breakdown__dot pool-cache-breakdown__dot--cached" />{t('usage.cache_read')} {fmtTokens(cacheRead)}</span>
          <span><i className="pool-cache-breakdown__dot pool-cache-breakdown__dot--write" />{t('usage.cache_written')} {fmtTokens(cacheCreation)}</span>
          <span><i className="pool-cache-breakdown__dot pool-cache-breakdown__dot--missed" />{t('usage.cache_miss')} {fmtTokens(cacheMiss)}</span>
          <span>{t('usage.request_hit')} {fmtPct(requestHitRate)}</span>
        </div>
      </div>

      <div className="pool-chart-card" style={{ marginBottom: 18 }}>
        <div className="head">
          <div><div className="t">{t('usage.trend')}</div><div className="s">{t('usage.trend_desc')}</div></div>
          <div style={{ display: 'flex', gap: 8 }}>
            <Button size="small" theme={trendMode === 'model' ? 'solid' : 'outline'} onClick={() => setTrendMode('model')}>{t('usage.by_model')}</Button>
            <Button size="small" theme={trendMode === 'type' ? 'solid' : 'outline'} onClick={() => setTrendMode('type')}>{t('usage.by_type')}</Button>
          </div>
        </div>
        <div style={{ height: 280 }}>
          {trendMode === 'model' && hasModelTrend
            ? <ModelAreaChart modelSeries={modelSeries} series={series} height={280} selectedKeys={selectedKeySet} />
            : <AreaChart buckets={ts} height={280} />}
        </div>
      </div>

      {hasModelTrend ? (
        <div className="pool-chart-card" style={{ marginBottom: 18 }}>
          <div className="head">
            <div><div className="t">{t('usage.model_cache_trend')}</div><div className="s">{t('usage.model_cache_trend_desc')}</div></div>
            <Select aria-label={t('usage.cache_metric')} value={cacheMetric} onChange={(value: string) => setCacheMetric(value)} optionList={CACHE_METRICS.map((item) => ({ label: t(item.labelKey), value: item.value }))} style={{ width: 130 }} />
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
            <ModelAreaChart modelSeries={modelSeries} series={series} height={260} metric={cacheMetric} selectedKeys={selectedKeySet} />
          </div>
        </div>
      ) : null}

      {(hasTopAccts || hasModelCacheBars) ? (
        <div className="pool-grid cols-2" style={{ marginBottom: 18 }}>
          {hasTopAccts ? (
            <div className="pool-chart-card"><div className="head"><div className="t">{t('usage.top_accounts')}</div></div>
              <BarChart data={topAccts} series={[{ key: 'input', name: t('usage.input'), color: C.blue }, { key: 'output', name: t('usage.output'), color: C.green }]} stacked />
            </div>
          ) : null}
          {hasModelCacheBars ? (
            <div className="pool-chart-card">
              <div className="head"><div><div className="t">{t('usage.model_cache_rate')}</div><div className="s">{t('usage.model_cache_rate_desc')}</div></div></div>
              <div style={{ paddingTop: 6 }}><CacheBars data={byModel} /></div>
            </div>
          ) : null}
        </div>
      ) : null}

      <section className="pool-usage-diagnostics" style={{ marginBottom: 18 }}>
        <div className="pool-usage-diagnostics__head">
          <div>
            <div className="pool-section-title">{t('usage.diagnostics')}</div>
            <div className="pool-text-tertiary">{t('usage.diagnostics_desc')}</div>
          </div>
          <div className="pool-segmented" role="group" aria-label={t('usage.diagnostic_dimensions')}>
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
        <Section title={activeDiagnosticTab.title}>
          <DataTable
            loading={loading}
            lastRefresh={lastRefresh}
            dataSource={activeDiagnosticTab.data}
            columns={activeDiagnosticTab.columns}
            rowKey={activeDiagnosticTab.rowKey}
            pagination={{ pageSize: activeDiagnostic === 'accountUsage' ? 15 : 8 }}
            emptyTitle={t('usage.no_diagnostics')}
            emptyDesc={t('usage.no_diagnostics_desc')}
            skeletonRows={6}
            skeletonCols={Math.min(8, activeDiagnosticTab.columns.length)}
            density="compact"
            minScrollX={activeDiagnosticTab.minScrollX}
            mobileRenderer={mobileDiagnosticRenderer(activeDiagnosticTab.columns, activeDiagnosticTab.mobileTitle)}
            mobileListLabel={activeDiagnosticTab.title}
          />
        </Section>
      </section>

      <ConfirmDialog
        open={resetOpen}
        title={t('usage.reset_title')}
        description={(
          <div className="pool-confirm-copy">
            <p>{t('usage.reset_desc')}</p>
            <span className="pool-sr-only">{t('usage.reset_record_note')}</span>
          </div>
        )}
        confirmText={t('usage.reset_confirm')}
        cancelText={t('common.cancel')}
        onCancel={() => setResetOpen(false)}
        onConfirm={resetCacheStats}
      />
    </div>
  );
}
