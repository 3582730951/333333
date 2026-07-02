import React, { useCallback } from 'react';
import { Button } from '../../components/pool/index.jsx';
import { IconRefresh } from '../../components/pool/icons.jsx';
import { get } from '../../api.js';
import LoadErrorBanner from '../../components/LoadErrorBanner.jsx';
import PageHeader, { Panel } from '../../components/PageHeader.jsx';
import ResourceTable from '../../components/ResourceTable.jsx';
import StatCard from '../../components/StatCard.jsx';
import useAsyncResource from '../../hooks/useAsyncResource.js';
import { UsageAreaChart, DonutChart } from '../../components/LazyCharts.jsx';
import { PALETTE, COLORS } from '../../lib/chartTheme.js';
import { fmtTokens, fmtInt } from '../../lib/format.js';
import { loadResourceGroup } from '../../lib/resource.js';

const C = COLORS;

export default function PortalDashboard() {
  const fetchUsage = useCallback(async ({ signal }) => {
    const now = Math.floor(Date.now() / 1000);
    const { values, error } = await loadResourceGroup({
      usage: { label: '模型用量', load: () => get('/user/usage', undefined, { signal }) },
      timeseries: { label: '趋势数据', load: () => get('/user/usage/timeseries', { since: now - 7 * 86400, bucket: 86400 }, { signal }) },
    });
    return {
      usage: Array.isArray(values.usage) ? values.usage : [],
      ts: values.timeseries?.buckets || [],
      error,
    };
  }, []);

  const {
    data = { usage: [], ts: [], error: null },
    loading,
    error,
    lastRefresh,
    reload: load,
  } = useAsyncResource(fetchUsage, [fetchUsage], { initialData: { usage: [], ts: [], error: null } });
  const usage = data.usage || [];
  const ts = data.ts || [];
  const loadError = error || data.error;

  const total = usage.reduce((s, r) => s + (r.total_tokens || 0), 0);
  const reqs = usage.reduce((s, r) => s + (r.requests || 0), 0);
  const modelDonut = usage.slice(0, 6).map((r, i) => ({ name: r.model, value: r.total_tokens || 0, color: PALETTE[i % PALETTE.length] }));

  const cols = [
    { title: '模型', dataIndex: 'model', render: (v) => <b>{v}</b> },
    { title: '请求', dataIndex: 'requests', render: fmtInt },
    { title: '输入', dataIndex: 'prompt_tokens', render: fmtTokens },
    { title: '输出', dataIndex: 'completion_tokens', render: fmtTokens },
    { title: '总计', dataIndex: 'total_tokens', render: (v) => <b>{fmtTokens(v)}</b> },
  ];

  return (
    <div>
      <PageHeader title="我的用量" subtitle="你的 API 调用与 token 消耗"
        actions={<Button icon={<IconRefresh />} onClick={load} loading={loading}>刷新</Button>} />

      <LoadErrorBanner error={loadError} onRetry={load} />

      <div className="pool-stat-grid" style={{ marginBottom: 18 }}>
        <StatCard label="总 Token" value={fmtTokens(total)} color={C.violet} />
        <StatCard label="总请求" value={fmtInt(reqs)} color={C.blue} />
        <StatCard label="使用模型" value={fmtInt(usage.length)} color={C.green} />
      </div>

      <div className="pool-chart-card" style={{ marginBottom: 18 }}>
        <div className="head"><div className="t">用量趋势（近 7 天）</div></div>
        <div style={{ height: 260 }}><UsageAreaChart buckets={ts} height={260} /></div>
      </div>

      <div className="pool-grid cols-2">
        <div className="pool-chart-card"><div className="head"><div className="t">模型占比</div></div><DonutChart data={modelDonut} /></div>
        <Panel title="按模型用量">
          <ResourceTable
            loading={loading}
            lastRefresh={lastRefresh}
            dataSource={usage}
            columns={cols}
            rowKey="model"
            pagination={false}
            size="small"
            emptyTitle="暂无用量"
            skeletonRows={5}
            skeletonCols={5}
          />
        </Panel>
      </div>
    </div>
  );
}
