import React, { useState, useEffect, useCallback } from 'react';
import { Tag, Button } from '@douyinfe/semi-ui';
import { IconRefresh, IconPlus, IconUser, IconKey, IconSetting, IconLineChartStroked } from '@douyinfe/semi-icons';
import { useNavigate } from 'react-router-dom';
import { get } from '../api.js';
import LoadErrorBanner from '../components/LoadErrorBanner.jsx';
import PageHeader from '../components/PageHeader.jsx';
import StatCard from '../components/StatCard.jsx';
import SystemHealthSummary from '../components/SystemHealthSummary.jsx';
import useAsyncResource from '../hooks/useAsyncResource.js';
import useVisibleInterval, { usePageVisible } from '../hooks/useVisibleInterval.js';
import { UsageAreaChart, DonutChart, GroupedBar, CacheRateBars } from '../components/LazyCharts.jsx';
import { COLORS, modelColor } from '../lib/chartTheme.js';
import { fmtTokens, fmtInt } from '../lib/format.js';
import { loadResourceGroup } from '../lib/resource.js';

const C = COLORS;
const EMPTY_ACCOUNT_SUMMARY = {
  total: 0,
  active: 0,
  quarantined: 0,
  cooling: 0,
  recheck: 0,
  codex: 0,
  claude: 0,
  other: 0,
};
const EMPTY_DASHBOARD_CORE = { accountSummary: EMPTY_ACCOUNT_SUMMARY, ts: [], health: null, error: null };
const EMPTY_DASHBOARD_SECONDARY = { reg: null, sys: null, byModel: [], error: null };
const EMPTY_DASHBOARD = { ...EMPTY_DASHBOARD_CORE, ...EMPTY_DASHBOARD_SECONDARY };
const DASHBOARD_REFRESH_MS = 15000;

export default function Dashboard() {
  const navigate = useNavigate();
  const pageVisible = usePageVisible();

  const fetchDashboardCore = useCallback(async ({ signal }) => {
    const now = Math.floor(Date.now() / 1000);
    const { values, error } = await loadResourceGroup({
      health: { label: '健康检查', load: () => get('/healthz', undefined, { signal }) },
      accountSummary: { label: '账号汇总', load: () => get('/admin/accounts/summary', undefined, { signal }) },
      timeseries: { label: '用量趋势', load: () => get('/admin/usage/timeseries', { since: now - 86400, bucket: 3600 }, { signal }) },
    });
    return {
      health: values.health,
      accountSummary: values.accountSummary || EMPTY_ACCOUNT_SUMMARY,
      ts: values.timeseries?.buckets || [],
      error,
    };
  }, []);

  const fetchDashboardSecondary = useCallback(async ({ signal }) => {
    const now = Math.floor(Date.now() / 1000);
    const { values, error } = await loadResourceGroup({
      registration: { label: '注册统计', load: () => get('/admin/register/stats', undefined, { signal }) },
      system: { label: '系统资源', load: () => get('/admin/system', undefined, { signal }) },
      byModel: { label: '模型统计', load: () => get('/admin/usage/by-model', { since: now - 7 * 86400 }, { signal }) },
    });
    return {
      reg: values.registration || null,
      sys: values.system || null,
      byModel: values.byModel?.models || [],
      error,
    };
  }, []);

  const {
    data: core = EMPTY_DASHBOARD_CORE,
    loading: coreLoading,
    error: coreError,
    lastRefresh: coreLastRefresh,
    reload: loadCore,
  } = useAsyncResource(fetchDashboardCore, [fetchDashboardCore], { initialData: EMPTY_DASHBOARD_CORE });
  const {
    data: secondary = EMPTY_DASHBOARD_SECONDARY,
    loading: secondaryLoading,
    error: secondaryError,
    lastRefresh: secondaryLastRefresh,
    reload: loadSecondary,
  } = useAsyncResource(fetchDashboardSecondary, [fetchDashboardSecondary], { initialData: EMPTY_DASHBOARD_SECONDARY });
  const load = useCallback(async () => {
    const [coreResult] = await Promise.all([loadCore(), loadSecondary()]);
    return coreResult;
  }, [loadCore, loadSecondary]);
  const d = { ...EMPTY_DASHBOARD, ...core, ...secondary };
  const loading = coreLoading || secondaryLoading;
  const lastRefresh = coreLastRefresh || secondaryLastRefresh;
  const loadError = coreError || secondaryError || core.error || secondary.error;

  useVisibleInterval(load, DASHBOARD_REFRESH_MS);

  // Format last refresh time
  const formatLastRefresh = () => {
    if (!lastRefresh) return '加载中...';
    const now = new Date();
    const diff = Math.floor((now - lastRefresh) / 1000);
    if (diff < 5) return '刚刚更新';
    if (diff < 60) return `${diff} 秒前更新`;
    return `${Math.floor(diff / 60)} 分钟前更新`;
  };

  // Calculate next refresh countdown
  const [countdown, setCountdown] = useState(DASHBOARD_REFRESH_MS / 1000);
  useVisibleInterval(() => {
    setCountdown((c) => {
      if (c <= 1) return DASHBOARD_REFRESH_MS / 1000;
      return c - 1;
    });
  }, 1000, { fireOnVisible: false });

  useEffect(() => {
    if (!loading && lastRefresh) {
      setCountdown(DASHBOARD_REFRESH_MS / 1000);
    }
  }, [loading, lastRefresh]);

  const accountSummary = { ...EMPTY_ACCOUNT_SUMMARY, ...(d.accountSummary || {}) };
  const active = accountSummary.active || 0;
  const quarantined = accountSummary.quarantined || 0;
  const cooling = accountSummary.cooling || 0;
  const recheck = accountSummary.recheck || 0;
  const codex = accountSummary.codex || 0;
  const claude = accountSummary.claude || 0;
  const other = accountSummary.other || 0;

  const tokens = (d.ts || []).reduce((s, b) => s + (b.total_tokens || 0), 0);
  const reqs = (d.ts || []).reduce((s, b) => s + (b.requests || 0), 0);

  const statusDonut = [
    { name: '活跃', value: active, color: C.green },
    { name: '冷却中', value: cooling, color: C.cyan },
    { name: '隔离', value: quarantined, color: C.red },
    { name: '待复测', value: recheck, color: C.amber },
  ];
  const providerDonut = [
    { name: 'Codex', value: codex, color: C.blue },
    { name: 'Claude', value: claude, color: C.violet },
    { name: '其它', value: other, color: COLORS.grey },
  ];
  const regByDay = (d.reg?.by_day || []).map((x) => ({ x: (x.date || '').slice(5), 成功: x.succeeded || 0, 失败: x.failed || 0 }));
  const modelTokenDonut = (d.byModel || []).slice(0, 6).map((m) => ({ name: m.model || '(未知)', value: m.total_tokens || 0, color: modelColor(m.model) }));
  const sys = d.sys;

  return (
    <div>
      <PageHeader title="总览" subtitle="账号池、用量与系统资源实时概览"
        actions={<>
          {d.health?.ok ? <Tag color="green">服务正常</Tag> : <Tag color="red">服务异常</Tag>}
          <Button icon={<IconRefresh />} onClick={load} loading={loading}>刷新</Button>
          {!loading && (
            <span style={{ fontSize: 12, color: 'var(--semi-color-text-3)' }}>
              {formatLastRefresh()} · {pageVisible ? `${countdown}s 后自动刷新` : '后台暂停自动刷新'}
            </span>
          )}
        </>} />

      <LoadErrorBanner error={loadError} onRetry={load} />

      {/* 快捷操作入口 */}
      <div className="pool-quick-actions" style={{
        display: 'flex',
        gap: 10,
        marginBottom: 18,
        flexWrap: 'wrap',
        alignItems: 'center',
      }}>
        <span style={{ fontSize: 13, fontWeight: 600, color: 'var(--semi-color-text-2)', marginRight: 4 }}>
          快捷操作
        </span>
        <Button icon={<IconPlus />} type="primary" onClick={() => navigate('/accounts?action=import')}>
          导入账号
        </Button>
        <Button icon={<IconUser />} onClick={() => navigate('/accounts')}>
          账号管理
        </Button>
        <Button icon={<IconKey />} onClick={() => navigate('/keys')}>
          API Keys
        </Button>
        <Button icon={<IconLineChartStroked />} onClick={() => navigate('/usage')}>
          用量统计
        </Button>
        <Button icon={<IconSetting />} onClick={() => navigate('/settings-v2')}>
          系统设置
        </Button>
      </div>

      <div className="pool-stat-grid" style={{ marginBottom: 18 }}>
        <StatCard label="账号总数" value={fmtInt(accountSummary.total)} color={C.blue} sub={`Codex ${codex} · Claude ${claude}`} />
        <StatCard label="活跃账号" value={fmtInt(active)} color={C.green} sub="可立即调度" />
        <StatCard label="冷却中" value={fmtInt(cooling)} color={C.cyan} sub="临时退避" />
        <StatCard label="隔离 / 待复测" value={`${quarantined} / ${recheck}`} color={C.amber} sub="quarantine / recheck" />
        <StatCard label="24h Token" value={fmtTokens(tokens)} color={C.violet} sub={`${fmtInt(reqs)} 次请求`} />
        <StatCard label="注册成功率" value={d.reg ? Math.round((d.reg.totals?.success_rate || 0) * 100) + '%' : '—'} color={C.green}
          sub={d.reg ? `${d.reg.totals?.succeeded || 0} 成功 / ${d.reg.totals?.failed || 0} 失败` : ''} />
      </div>

      <div className="pool-chart-card" style={{ marginBottom: 18 }}>
        <div className="head"><div><div className="t">Token 用量（近 24 小时）</div><div className="s">输入 / 输出 / 缓存，按小时聚合</div></div></div>
        <div style={{ height: 280 }}><UsageAreaChart buckets={d.ts} height={280} /></div>
      </div>

      <div className="pool-grid cols-3" style={{ marginBottom: 18 }}>
        <div className="pool-chart-card"><div className="head"><div className="t">账号状态分布</div></div><DonutChart data={statusDonut} unit=" 个" /></div>
        <div className="pool-chart-card"><div className="head"><div className="t">平台分布</div></div><DonutChart data={providerDonut} unit=" 个" /></div>
        <div className="pool-chart-card"><div className="head"><div className="t">注册成功 / 失败（近 14 天）</div></div>
          <GroupedBar data={regByDay} series={[{ key: '成功', color: C.green }, { key: '失败', color: C.red }]} stacked />
        </div>
      </div>

      <div className="pool-grid cols-2" style={{ marginBottom: 18 }}>
        <div className="pool-chart-card">
          <div className="head"><div><div className="t">模型缓存命中率</div><div className="s">cached / prompt，近 7 天 · 颜色区分模型</div></div></div>
          <div style={{ paddingTop: 6 }}><CacheRateBars data={d.byModel} /></div>
        </div>
        <div className="pool-chart-card"><div className="head"><div className="t">模型 Token 占比</div></div><DonutChart data={modelTokenDonut} unit=" tok" /></div>
      </div>

      {sys?.supported && (
        <SystemHealthSummary
          system={sys}
          variant="compact"
          action={<Button size="small" onClick={() => navigate('/system')}>系统监控</Button>}
        />
      )}
    </div>
  );
}
