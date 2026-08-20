import React, { useState, type ReactNode } from 'react';
import { Button, ConfirmDialog, Select, Tag, Toast } from '../components/pool/index.jsx';
import { IconDownload, IconRefresh } from '../components/pool/icons.jsx';
import LoadErrorBanner from '../components/LoadErrorBanner.jsx';
import ResourceTable from '../components/ResourceTable.jsx';
import PageHeader, { Panel } from '../components/PageHeader.jsx';
import StatCard from '../components/StatCard.jsx';
import { UsageAreaChart, GroupedBar, CacheRateBars, UsageModelAreaChart } from '../components/LazyCharts.jsx';
import { COLORS, seriesColorMap } from '../lib/chartTheme.js';
import { fmtTokens, fmtInt } from '../lib/format.js';
import { t } from '../lib/i18n.js';
import { toCSV, downloadCSV } from '../lib/csv.js';
import {
  useModelAuditData, useResetUsageCacheMutation, useUsageCacheDiagnosticData, useUsageDashboardData,
} from '../features/observability/queries/usage';
import type { UsageCacheDiagnosticField } from '../features/observability/api/usage';
import {
  reportedCacheMetric, usageDimensionKey, usageDisplayLabel,
  type ModelAuditRow, type UsageMetricRow, type UsageRange,
} from '../features/observability/model/usage';

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
const DIAGNOSTIC_FIELDS: Record<string, UsageCacheDiagnosticField> = {
  apiKey: 'by_api_key',
  accountModel: 'by_account_model',
  route: 'by_route',
  time: 'by_time_bucket',
};

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

function fmtOptionalPct(v: unknown) {
  return v == null || v === '' ? '—' : fmtPct(v);
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
  return usageDimensionKey(row);
}

function modelLabel(row: UsageMetricRow) {
  return usageDisplayLabel(row, `(${t('usage.unknown_model')})`);
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
  const [trendMode, setTrendMode] = useState('provider_model');
  const [cacheMetric, setCacheMetric] = useState('cache_read_tokens');
  const [selectedCacheModels, setSelectedCacheModels] = useState<string[]>([]);
  const [hoveredModel, setHoveredModel] = useState<any>(null);
  const [activeDiagnostic, setActiveDiagnostic] = useState('providerModel');

  const { data, loading, error, lastRefresh, reload: load } = useUsageDashboardData(range);
  const {
    data: modelAudit,
    loading: modelAuditLoading,
    error: modelAuditError,
    lastRefresh: modelAuditLastRefresh,
    reload: reloadModelAudit,
  } = useModelAuditData(range, Boolean(data));
  const diagnosticField = DIAGNOSTIC_FIELDS[activeDiagnostic] || null;
  const {
    data: diagnosticCache = {},
    loading: diagnosticLoading,
    error: diagnosticError,
    lastRefresh: diagnosticLastRefresh,
    reload: reloadDiagnostic,
  } = useUsageCacheDiagnosticData(range, diagnosticField);
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
  const stableCacheSummary = cache.stable_summary || cacheSummary;
  const cacheByKey = diagnosticCache.by_api_key || [];
  const cacheByAccountModel = diagnosticCache.by_account_model || [];
  const cacheByProvider = cache.by_provider || [];
  const cacheByProviderModel = cache.by_provider_model || [];
  const officialProviderModelCache = cacheByProviderModel.filter((row) => String(row.provider || '').toLowerCase() !== 'kiro');
  const cacheByRoute = diagnosticCache.by_route || [];
  const cacheByTimeBucket = diagnosticCache.by_time_bucket || [];
  const usageWindow = data?.usageWindow || { rows: [] };
  const windowInfo = usageWindow.window || cache.window || {};

  const actualTokens = rows.reduce((s, b) => s + Number(b.actual_total_tokens ?? b.total_tokens ?? 0), 0);
  const estimatedTokens = rows.reduce((s, b) => s + Number(b.estimated_total_tokens ?? b.estimated_tokens ?? 0), 0);
  const totalTokens = actualTokens + estimatedTokens;
  const actualReqs = rows.reduce((s, b) => s + Number(b.actual_requests ?? b.requests ?? 0), 0);
  const estimatedReqs = rows.reduce((s, b) => s + Number(b.estimated_requests || 0), 0);
  const fallbackCacheRead = ts.reduce((s, b) => s + (b.cache_read_tokens || 0), 0);
  const fallbackCacheCreation = ts.reduce((s, b) => s + (b.cache_creation_tokens || 0), 0);
  const fallbackCacheInput = ts.reduce((s, b) => s + (b.cache_input_tokens ?? b.prompt_tokens ?? 0), 0);
  const officialCacheRows = cacheByProvider.filter((row) => String(row.provider || '').toLowerCase() !== 'kiro');
  const officialCacheAvailable = officialCacheRows.length > 0;
  const sumOfficial = (key: keyof UsageMetricRow) => officialCacheRows.reduce((sum, row) => sum + Number(row[key] || 0), 0);
  const cacheRead = officialCacheAvailable ? sumOfficial('cache_read_tokens') : (cacheSummary.cache_read_tokens ?? fallbackCacheRead);
  const cacheCreation = officialCacheAvailable ? sumOfficial('cache_creation_tokens') : (cacheSummary.cache_creation_tokens ?? fallbackCacheCreation);
  const promptForCache = officialCacheAvailable ? sumOfficial('cache_input_tokens') : (cacheSummary.cache_input_tokens ?? cacheSummary.prompt_tokens ?? fallbackCacheInput);
  const cacheMiss = officialCacheAvailable ? Math.max(0, promptForCache - cacheRead) : (cacheSummary.cache_miss_tokens ?? Math.max(0, promptForCache - cacheRead));
  const cacheRate = promptForCache ? cacheRead / promptForCache : 0;
  const officialRealInput = officialCacheAvailable ? sumOfficial('cache_input_tokens') : 0;
  const officialRealRead = officialCacheAvailable ? sumOfficial('cache_read_tokens') : 0;
  const realTokenHitRate = officialCacheAvailable && officialRealInput > 0
    ? officialRealRead / officialRealInput
    : (stableCacheSummary.real_token_hit_rate ?? stableCacheSummary.token_hit_rate ?? cacheRate);
  const cacheCreationReported = officialCacheAvailable
    ? sumOfficial('cache_creation_reported_requests') > 0
    : Number(cacheSummary.cache_creation_reported_requests || 0) > 0;
  const eligibleHitRate = cacheCreationReported ? (cacheSummary.eligible_cache_hit_rate ?? (cacheRead + cacheCreation > 0 ? cacheRead / (cacheRead + cacheCreation) : 0)) : null;
  const cacheWriteShare = cacheCreationReported ? (cacheSummary.cache_write_share ?? (promptForCache ? cacheCreation / promptForCache : 0)) : null;
  const officialRealRequests = officialCacheAvailable ? sumOfficial('real_requests') : 0;
  const officialHitRequests = officialCacheAvailable ? sumOfficial('hit_requests') : 0;
  const requestHitRate = officialRealRequests > 0 ? officialHitRequests / officialRealRequests : (stableCacheSummary.request_hit_rate ?? 0);
  const kiroCache = cacheByProvider.find((row) => String(row.provider || '').toLowerCase() === 'kiro');
  const kiroUnreported = kiroCache?.cache_reporting_state === 'unreported';
  const kiroCreditsReported = Number(kiroCache?.kiro_credits_reported_requests || 0) > 0;
  const kiroCredits = Number(kiroCache?.kiro_credits || 0);
  const cachedPct = promptForCache > 0 ? Math.max(0, Math.min(100, Math.round((cacheRead / promptForCache) * 100))) : 0;
  const cacheWritePct = promptForCache > 0 ? Math.max(0, Math.min(100, Math.round((cacheCreation / promptForCache) * 100))) : 0;
  const missedPct = promptForCache > 0 ? Math.max(0, 100 - cachedPct - cacheWritePct) : 0;
  const cacheCompositionRows = (officialProviderModelCache.length ? officialProviderModelCache : cache.by_model || []).slice(0, 8);
  const cacheCompositionColor = seriesColorMap(cacheCompositionRows.map((m) => modelKey(m)));
  const cacheCompositionSegments = cacheCompositionRows.map((m) => ({
    key: modelKey(m),
    label: modelLabel(m),
    color: cacheCompositionColor(modelKey(m)),
    read: m.cache_read_tokens || 0,
    requests: m.requests,
    request_hit_rate: m.request_hit_rate,
    real_token_hit_rate: m.real_token_hit_rate,
    eligible_cache_hit_rate: Number(m.cache_creation_reported_requests || 0) > 0 ? m.eligible_cache_hit_rate : null,
    cache_write_share: Number(m.cache_creation_reported_requests || 0) > 0 ? m.cache_write_share : null,
    total_tokens: m.total_tokens,
  }));
  const cacheSegmentTotal = cacheCompositionSegments.reduce((s, m) => s + m.read, 0);
  const cacheSeries = series.filter((descriptor) => descriptor.provider_type !== 'kiro' && descriptor.provider_id !== 'kiro');
  // Must use the same key expression ModelAreaChart uses internally, and be built from the
  // same unfiltered cacheSeries: the toggle swatches below are the legend for the lines that
  // chart draws, so any divergence in key or order would colour the legend differently from
  // the thing it labels.
  const cacheSeriesColor = seriesColorMap(cacheSeries.map((s: any) => s.series_key || s.model_key || s.series_label));
  const cacheSeriesKeys = new Set(cacheSeries.map((descriptor) => descriptor.series_key));
  const cacheModelSeries = modelSeries.filter((row) => cacheSeriesKeys.has(String(row.series_key || '')));
  const selectedKeySet = new Set(selectedCacheModels.length ? selectedCacheModels : cacheSeries.map((descriptor) => descriptor.series_key));
  const hasModelTrend = modelSeries.length > 0 && series.length > 0;
  const hasCacheModelTrend = cacheModelSeries.length > 0 && cacheSeries.length > 0;

  const toggleCacheModel = (key: string) => {
    const all = cacheSeries.map((s) => s.series_key);
    const base = selectedCacheModels.length ? selectedCacheModels : all;
    const next = base.includes(key) ? base.filter((x) => x !== key) : [...base, key];
    setSelectedCacheModels(next.length ? next : all);
  };

  const topAccts = [...rows].sort((a, b) => Number(b.combined_total_tokens ?? b.total_tokens ?? 0) - Number(a.combined_total_tokens ?? a.total_tokens ?? 0)).slice(0, 10)
    .map((a) => ({ x: (a.label || a.account_id || '').slice(0, 10), input: Number(a.actual_prompt_tokens ?? a.prompt_tokens ?? 0), output: Number(a.actual_completion_tokens ?? a.completion_tokens ?? 0) }));
  const hasTopAccts = topAccts.some((item) => item.input || item.output);
  const officialByModel = byModel.filter((item) => item.provider_type !== 'kiro' && item.provider_id !== 'kiro');
  const hasModelCacheBars = officialByModel.some((item) => (item.cache_input_tokens || item.prompt_tokens || 0) > 0);

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
    { title: '实际请求', dataIndex: 'actual_requests', render: fmtInt },
    { title: '估算请求', dataIndex: 'estimated_requests', render: fmtInt },
    { title: '实际输入', dataIndex: 'actual_prompt_tokens', render: fmtTokens },
    { title: '实际输出', dataIndex: 'actual_completion_tokens', render: fmtTokens },
    { title: '实际总量', dataIndex: 'actual_total_tokens', render: fmtTokens },
    { title: '估算输入', dataIndex: 'estimated_prompt_tokens', render: fmtTokens },
    { title: '估算输出', dataIndex: 'estimated_completion_tokens', render: fmtTokens },
    { title: '估算总量', dataIndex: 'estimated_total_tokens', render: fmtTokens },
    { title: '合计', dataIndex: 'combined_total_tokens', render: (v) => <b>{fmtTokens(v)}</b> },
    { title: '估算占比', dataIndex: 'estimated_rate', render: fmtPct },
    { title: t('usage.cache_read'), dataIndex: 'cache_read_tokens', render: (v) => fmtTokens(v || 0) },
    { title: t('usage.cache_write'), dataIndex: 'cache_creation_tokens', render: fmtTokens },
    { title: t('usage.total'), dataIndex: 'total_tokens', sorter: (a, b) => (a.total_tokens || 0) - (b.total_tokens || 0), defaultSortOrder: 'descend', render: (v) => <b>{fmtTokens(v)}</b> },
  ];

  const modelAuditColumns: any[] = [
    { title: t('usage.requested_model'), dataIndex: 'requested_model', width: 180, render: textOrDash },
    { title: t('usage.resolved_model'), dataIndex: 'resolved_model', width: 180, render: textOrDash },
    { title: t('usage.actual_model'), dataIndex: 'actual_model', width: 180, render: (value: unknown) => value === 'unknown' ? <Tag color="grey">{t('usage.actual_unavailable')}</Tag> : textOrDash(value) },
    { title: t('usage.override_source'), dataIndex: 'model_override_source', width: 150, render: textOrDash },
    {
      title: t('usage.model_match'), dataIndex: 'mismatch', width: 130,
      render: (value: boolean, row: ModelAuditRow) => value
        ? <Tag color="red">{row.mismatch_reason || t('usage.model_mismatch')}</Tag>
        : <Tag color="green">{t('usage.model_match_ok')}</Tag>,
    },
    { title: t('usage.request_unit'), dataIndex: 'requests', width: 90, align: 'right', render: fmtInt },
    { title: t('usage.last_seen'), dataIndex: 'last_seen_at', width: 180, render: unixDateTime },
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

  const providerModelCols: UsageColumn[] = [
    { title: 'Provider', dataIndex: 'provider', render: (value) => textOrDash(value) },
    { title: t('usage.model'), dataIndex: 'model', render: (_, row) => row.model_label || row.model || t('usage.unknown_model') },
    { title: t('usage.request_unit'), dataIndex: 'requests', render: fmtInt },
    { title: t('usage.total_tokens'), dataIndex: 'combined_total_tokens', render: fmtTokens },
    { title: '缓存单位', key: 'cache_unit', render: (_, row) => String(row.provider || '').toLowerCase() === 'kiro' ? 'cache point / credit' : 'Token' },
    {
      title: t('usage.cache_read'), dataIndex: 'cache_read_tokens',
      render: (value, row) => reportedCacheMetric(row, value, t('usage.upstream_unreported'), fmtTokens),
    },
    {
      title: t('usage.cache_write'), dataIndex: 'cache_creation_tokens',
      render: (value, row) => reportedCacheMetric(row, value, t('usage.upstream_unreported'), fmtTokens),
    },
    {
      title: t('usage.real_token_hit'), dataIndex: 'real_token_hit_rate',
      render: (value, row) => String(row.provider || '').toLowerCase() === 'kiro' || row.cache_reporting_state === 'unreported' ? '—' : fmtOptionalPct(value),
    },
    { title: 'Credits', dataIndex: 'kiro_credits', render: (value, row) => Number(row.kiro_credits_reported_requests || 0) > 0 ? Number(value || 0).toFixed(2) : (String(row.provider || '').toLowerCase() === 'kiro' ? t('usage.upstream_unreported') : '—') },
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
    { title: t('usage.write_share'), dataIndex: 'cache_write_share', width: 110, sorter: (a, b) => (a.cache_write_share || 0) - (b.cache_write_share || 0), render: (v, row) => Number(row.cache_creation_reported_requests || 0) > 0 ? fmtPct(v) : '—' },
    { title: t('usage.cache_read'), dataIndex: 'cache_read_tokens', width: 120, sorter: (a, b) => (a.cache_read_tokens || 0) - (b.cache_read_tokens || 0), render: fmtTokens },
    { title: t('usage.cache_write'), dataIndex: 'cache_creation_tokens', width: 120, sorter: (a, b) => (a.cache_creation_tokens || 0) - (b.cache_creation_tokens || 0), render: fmtTokens },
    { title: t('usage.cache_miss'), dataIndex: 'cache_miss_tokens', width: 120, sorter: (a, b) => (a.cache_miss_tokens || 0) - (b.cache_miss_tokens || 0), render: fmtTokens },
    { title: t('usage.breakpoint'), dataIndex: 'cache_breakpoint_count', width: 90, sorter: (a, b) => (a.cache_breakpoint_count || 0) - (b.cache_breakpoint_count || 0), render: fmtInt },
    { title: t('usage.latest_user_risk'), dataIndex: 'latest_user_cache_control', width: 130, render: (v) => (Number(v) > 0 ? t('usage.yes') : t('usage.no')) },
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
    { title: t('usage.time_bucket'), dataIndex: 'bucket', width: 190, render: (v, row) => <span style={row.partial ? { opacity: .65, borderBottom: '1px dashed currentColor' } : undefined}>{v ? new Date(v * 1000).toLocaleString() : '—'}{row.partial ? ' · 数据仍在写入' : ''}</span> },
    { title: t('usage.request_unit'), dataIndex: 'requests', sorter: (a, b) => (a.requests || 0) - (b.requests || 0), render: fmtInt },
    { title: t('usage.real_requests'), dataIndex: 'real_requests', sorter: (a, b) => (a.real_requests || 0) - (b.real_requests || 0), render: fmtInt },
    { title: t('usage.read_share'), dataIndex: 'cache_read_share', sorter: (a, b) => (a.cache_read_share || 0) - (b.cache_read_share || 0), render: fmtPct },
    { title: t('usage.write_share'), dataIndex: 'cache_write_share', sorter: (a, b) => (a.cache_write_share || 0) - (b.cache_write_share || 0), render: (v, row) => Number(row.cache_creation_reported_requests || 0) > 0 ? fmtPct(v) : '—' },
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
    { key: 'providerModel', label: t('usage.by_provider_model'), title: t('usage.provider_model_diagnostic'), data: cacheByProviderModel, columns: providerModelCols, rowKey: (row) => `${row.provider || 'unknown'}:${row.model_key || row.model || 'unknown'}`, minScrollX: 1000, mobileTitle: (row) => modelLabel(row) },
    { key: 'apiKey', label: 'API Key', title: t('usage.api_key_diagnostic'), data: cacheByKey, columns: cacheKeyCols, rowKey: (r) => r.api_key_hash_prefix || 'none', minScrollX: 1080, mobileTitle: (r) => r.api_key_hash_prefix || t('usage.unattributed') },
    { key: 'accountModel', label: t('usage.account_model'), title: t('usage.account_model_diagnostic'), data: cacheByAccountModel, columns: cacheAccountModelCols, rowKey: (r) => `${r.account_id || 'none'}:${r.model || 'unknown'}`, minScrollX: 1180, mobileTitle: (r) => `${r.account_id || t('usage.unattributed')} · ${r.model || 'unknown'}` },
    { key: 'provider', label: 'Provider', title: 'Provider / Kiro 用量诊断', data: cacheByProvider, columns: [
      { title: 'Provider', dataIndex: 'provider', render: textOrDash }, { title: '实际 Token', dataIndex: 'actual_total_tokens', render: fmtTokens },
      { title: '估算 Token', dataIndex: 'estimated_total_tokens', render: fmtTokens }, { title: 'Credits', dataIndex: 'kiro_credits' },
      { title: 'Credits 报告请求', dataIndex: 'kiro_credits_reported_requests', render: fmtInt }, { title: '缓存报告状态', dataIndex: 'cache_reporting_state', render: textOrDash },
      { title: '缓存读取', dataIndex: 'cache_read_tokens', render: (v, r) => r.cache_reporting_state === 'unreported' ? '—（上游未报告缓存 token）' : fmtTokens(v) },
      { title: '缓存写入', dataIndex: 'cache_creation_tokens', render: (v, r) => r.cache_reporting_state === 'unreported' ? '—' : fmtTokens(v) },
      { title: '命中率', dataIndex: 'real_token_hit_rate', render: (v, r) => r.cache_reporting_state === 'unreported' ? '—' : fmtOptionalPct(v) },
      { title: 'cachePoint 注入/接受', dataIndex: 'cache_control_injected', render: (v) => fmtInt(v || 0) },
      { title: '已验证 reuse', dataIndex: 'cache_hit_after_prewarm', render: (v) => fmtInt(v || 0) },
    ], rowKey: (r) => r.provider || 'unknown', minScrollX: 900, mobileTitle: (r) => r.provider || 'unknown' },
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
          <span>完整水位 {unixDateTime(cache.usage_complete_through_at)} · pending {fmtInt(cache.pending_usage_requests || 0)} · 延迟 {fmtInt(cache.usage_lag_seconds || 0)}s</span>
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
        <MetricCard label="实际 Token" value={fmtTokens(actualTokens)} color={C.blue} />
        <MetricCard label="估算 Token" value={fmtTokens(estimatedTokens)} color={C.amber} />
        <MetricCard label="合计 Token" value={fmtTokens(totalTokens)} color={C.violet} />
        <MetricCard label="实际请求" value={fmtInt(actualReqs)} color={C.blue} />
        <MetricCard label="估算请求" value={fmtInt(estimatedReqs)} color={C.amber} />
        <MetricCard label="稳定请求命中率" value={fmtPct(requestHitRate)} color={C.green} />
        <MetricCard label="稳定 Token 命中率" value={fmtPct(realTokenHitRate)} color={C.cyan} />
        <MetricCard label="缓存统计口径" value="官方 Token" sub="不包含 Kiro cache point / credit" color={C.teal} />
      </div>

      {kiroCache ? (
        <div className="pool-stat-grid pool-kiro-stat-grid" style={{ marginBottom: 18 }}>
          <MetricCard label="Kiro credits" value={kiroCreditsReported ? kiroCredits.toFixed(2) : t('usage.upstream_unreported')} sub="单位：credit" color={C.teal} />
          <MetricCard label="Kiro cache points" value={kiroUnreported ? t('usage.upstream_unreported') : fmtInt(kiroCache.cache_control_injected || 0)} sub="单位：cache point" color={C.violet} />
          <MetricCard label="Kiro verified reuse" value={kiroUnreported ? t('usage.upstream_unreported') : fmtInt(kiroCache.cache_hit_after_prewarm || 0)} sub="单位：request" color={C.green} />
        </div>
      ) : null}

      <div className="pool-cache-breakdown" style={{ marginBottom: 18 }}>
        <div className="pool-cache-breakdown__head">
          <div>
            <div className="pool-section-title">{t('usage.cache_composition')} · Token</div>
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
                <span>{t('usage.eligible_hit')} {fmtOptionalPct(hoveredModel.eligible_cache_hit_rate)}</span>
                <span>{t('usage.write_share')} {fmtOptionalPct(hoveredModel.cache_write_share)}</span>
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
            <Button size="small" theme={trendMode === 'provider_model' ? 'solid' : 'outline'} onClick={() => setTrendMode('provider_model')}>{t('usage.by_provider_model')}</Button>
            <Button size="small" theme={trendMode === 'type' ? 'solid' : 'outline'} onClick={() => setTrendMode('type')}>{t('usage.by_type')}</Button>
          </div>
        </div>
        <div style={{ height: 280 }}>
          {trendMode === 'provider_model' && hasModelTrend
            ? <ModelAreaChart modelSeries={modelSeries} series={series} height={280} ariaLabel={t('usage.trend')} />
            : <AreaChart buckets={ts} height={280} ariaLabel={t('usage.trend')} />}
        </div>
      </div>

      {hasCacheModelTrend ? (
        <div className="pool-chart-card" style={{ marginBottom: 18 }}>
          <div className="head">
            <div><div className="t">{t('usage.model_cache_trend')}</div><div className="s">{t('usage.model_cache_trend_desc')}</div></div>
            <Select aria-label={t('usage.cache_metric')} value={cacheMetric} onChange={(value: string) => setCacheMetric(value)} optionList={CACHE_METRICS.map((item) => ({ label: t(item.labelKey), value: item.value }))} style={{ width: 130 }} />
          </div>
          <div className="pool-model-toggle-row">
            {cacheSeries.map((s) => {
              const active = selectedKeySet.has(s.series_key);
              const color = cacheSeriesColor(s.series_key || s.model_key || s.series_label);
              return (
                <button key={s.series_key} type="button" className={`pool-model-toggle ${active ? 'is-active' : ''}`} onClick={() => toggleCacheModel(s.series_key)}>
                  <i style={{ background: color }} />
                  <span>{s.series_label || s.series_key}</span>
                </button>
              );
            })}
          </div>
          <div style={{ height: 260 }}>
            <ModelAreaChart modelSeries={cacheModelSeries} series={cacheSeries} height={260} metric={cacheMetric} selectedKeys={selectedKeySet} ariaLabel={t('usage.model_cache_trend')} />
          </div>
        </div>
      ) : null}

      {(hasTopAccts || hasModelCacheBars) ? (
        <div className="pool-grid cols-2" style={{ marginBottom: 18 }}>
          {hasTopAccts ? (
            <div className="pool-chart-card"><div className="head"><div className="t">{t('usage.top_accounts')}</div></div>
              <BarChart ariaLabel={t('usage.top_accounts')} data={topAccts} series={[{ key: 'input', name: t('usage.input'), color: C.blue }, { key: 'output', name: t('usage.output'), color: C.green }]} stacked />
            </div>
          ) : null}
          {hasModelCacheBars ? (
            <div className="pool-chart-card">
              <div className="head"><div><div className="t">{t('usage.model_cache_rate')}</div><div className="s">{t('usage.model_cache_rate_desc')}</div></div></div>
              <div style={{ paddingTop: 6 }}><CacheBars data={officialByModel} /></div>
            </div>
          ) : null}
        </div>
      ) : null}

      <Section
        title={t('usage.model_audit')}
        extra={(
          <span className="pool-text-tertiary">
            {t('usage.audit_summary')
              .replace('{requests}', fmtInt(modelAudit?.requests || 0))
              .replace('{mismatches}', fmtInt(modelAudit?.mismatches || 0))
              .replace('{unknown}', fmtInt(modelAudit?.actual_model_unavailable || 0))}
          </span>
        )}
      >
        <DataTable
          error={modelAuditError}
          onRetry={reloadModelAudit}
          loading={modelAuditLoading}
          lastRefresh={modelAuditLastRefresh}
          dataSource={modelAudit?.rows || []}
          columns={modelAuditColumns}
          rowKey={(row: ModelAuditRow) => [row.requested_model, row.resolved_model, row.actual_model, row.model_override_source, row.mismatch_reason].join(':')}
          pagination={{ pageSize: 8 }}
          emptyTitle={t('usage.no_model_audit')}
          emptyDesc={t('usage.no_model_audit_desc')}
          skeletonRows={5}
          skeletonCols={7}
          density="compact"
          minScrollX={1090}
          mobileListLabel={t('usage.model_audit')}
          mobileRenderer={(row: ModelAuditRow) => (
            <div className="pool-diagnostic-card">
              <div className="pool-diagnostic-card__title">{row.requested_model || t('usage.unknown_model')}</div>
              <div className="pool-diagnostic-card__grid">
                <div className="pool-diagnostic-card__item"><span className="pool-diagnostic-card__label">{t('usage.resolved_model')}</span><span className="pool-diagnostic-card__value">{textOrDash(row.resolved_model)}</span></div>
                <div className="pool-diagnostic-card__item"><span className="pool-diagnostic-card__label">{t('usage.actual_model')}</span><span className="pool-diagnostic-card__value">{textOrDash(row.actual_model)}</span></div>
                <div className="pool-diagnostic-card__item"><span className="pool-diagnostic-card__label">{t('usage.model_match')}</span><span className="pool-diagnostic-card__value">{row.mismatch ? (row.mismatch_reason || t('usage.model_mismatch')) : t('usage.model_match_ok')}</span></div>
                <div className="pool-diagnostic-card__item"><span className="pool-diagnostic-card__label">{t('usage.request_unit')}</span><span className="pool-diagnostic-card__value">{fmtInt(row.requests)}</span></div>
              </div>
            </div>
          )}
        />
      </Section>

      <div style={{ height: 18 }} />

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
            error={diagnosticField ? diagnosticError : error}
            onRetry={diagnosticField ? reloadDiagnostic : load}
            loading={loading || diagnosticLoading}
            lastRefresh={diagnosticField ? diagnosticLastRefresh : lastRefresh}
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
