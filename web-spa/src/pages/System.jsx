import React, { useCallback } from 'react';
import { Tag, Button, Typography, Banner } from '../components/pool/index.jsx';
import { IconRefresh } from '../components/pool/icons.jsx';
import { get } from '../api.js';
import LoadErrorBanner from '../components/LoadErrorBanner.jsx';
import PageHeader, { Panel } from '../components/PageHeader.jsx';
import ResourceTable from '../components/ResourceTable.jsx';
import StatCard from '../components/StatCard.jsx';
import SystemHealthSummary from '../components/SystemHealthSummary.jsx';
import useAsyncResource from '../hooks/useAsyncResource.js';
import useVisibleInterval from '../hooks/useVisibleInterval.js';
import { COLORS } from '../lib/chartTheme.js';
import { fmtKB, fmtBytes, fmtInt, fmtDuration, fmtDateTime } from '../lib/format.js';

const C = COLORS;
const kindColor = { node: 'green', chrome: 'blue', xvfb: 'violet', other: 'grey' };
const eventColor = { panic: 'red', panic_restart: 'red', failed: 'red', unexpected_exit: 'amber', event: 'blue' };
const moduleColor = { running: 'green', restarting: 'amber', panic: 'red', failed: 'red', stopped: 'grey' };
const meterColor = (p) => (p >= 90 ? C.red : p >= 70 ? C.amber : C.green);
const fmtMillis = (ms) => (ms ? fmtDuration(Math.ceil(Number(ms) / 1000)) : '—');
const fmtModuleRuntime = (_, row) => fmtMillis(row.status === 'running' ? row.uptime_millis : (row.last_uptime_millis || row.uptime_millis));

export default function System() {
  const fetchSystem = useCallback(async ({ signal }) => {
    return get('/admin/system', undefined, { signal });
  }, []);
  const { data: s, loading, error, lastRefresh, reload: load } = useAsyncResource(fetchSystem, [fetchSystem], { initialData: null });

  useVisibleInterval(load, 3000);

  if (s && !s.supported) {
    return (
      <div>
        <PageHeader title="系统监控" actions={<Button icon={<IconRefresh />} onClick={load} loading={loading}>刷新</Button>} />
        <LoadErrorBanner error={error} onRetry={load} />
        <Banner type="warning" description="当前主机不是 Linux（无 /proc），系统指标不可用。部署到 Linux VPS 后此页将显示 CPU/内存/磁盘与注册任务内存。" />
      </div>
    );
  }

  const cpu = s?.cpu || {}, mem = s?.mem || {}, disk = s?.disk || {}, reg = s?.registration || {}, go = s?.go || {};
  const events = (s?.supervisor_events || []).map((event, index) => ({
    ...event,
    key: `${event.time_unix || 0}-${event.module || 'module'}-${index}`,
  }));
  const modules = (s?.supervisor_modules || []).map((module) => ({
    ...module,
    key: module.name || `${module.status || 'module'}-${module.last_event_unix || 0}`,
  }));
  const procCols = [
    { title: 'PID', dataIndex: 'pid', width: 90, render: (v) => <span className="pool-mono">{v}</span> },
    { title: '进程', dataIndex: 'comm' },
    { title: '类型', dataIndex: 'kind', width: 100, render: (k) => <Tag color={kindColor[k] || 'grey'}>{k}</Tag> },
    { title: '内存 (RSS)', dataIndex: 'rss_kb', width: 140, sorter: (a, b) => a.rss_kb - b.rss_kb, defaultSortOrder: 'descend', render: (v) => fmtKB(v) },
  ];
  const eventCols = [
    { title: '时间', dataIndex: 'time_unix', width: 140, render: fmtDateTime },
    { title: '模块', dataIndex: 'module', width: 180, render: (v) => <span className="pool-mono">{v}</span> },
    { title: '类型', dataIndex: 'type', width: 130, render: (v) => <Tag color={eventColor[v] || 'grey'}>{v || 'event'}</Tag> },
    { title: '说明', dataIndex: 'message', render: (v, row) => row.panic ? `${v || 'panic'}: ${row.panic}` : (v || '—') },
    { title: '运行时长', dataIndex: 'uptime_millis', width: 110, render: fmtMillis },
    { title: '重试等待', dataIndex: 'backoff_millis', width: 110, render: fmtMillis },
  ];
  const moduleCols = [
    { title: '模块', dataIndex: 'name', width: 190, render: (v) => <span className="pool-mono">{v}</span> },
    { title: '状态', dataIndex: 'status', width: 110, render: (v) => <Tag color={moduleColor[v] || 'grey'}>{v || 'unknown'}</Tag> },
    { title: '重启', dataIndex: 'restart_count', width: 90, sorter: (a, b) => (a.restart_count || 0) - (b.restart_count || 0), render: fmtInt },
    { title: 'panic', dataIndex: 'panic_count', width: 90, sorter: (a, b) => (a.panic_count || 0) - (b.panic_count || 0), render: fmtInt },
    { title: '异常退出', dataIndex: 'unexpected_exit_count', width: 110, sorter: (a, b) => (a.unexpected_exit_count || 0) - (b.unexpected_exit_count || 0), render: fmtInt },
    { title: '运行/上次', dataIndex: 'uptime_millis', width: 110, render: fmtModuleRuntime },
    { title: '重试等待', dataIndex: 'restart_backoff_millis', width: 110, render: fmtMillis },
    { title: '下次重试', dataIndex: 'next_restart_unix', width: 140, render: fmtDateTime },
    { title: '最近状态', dataIndex: 'last_message', render: (v, row) => row.last_panic ? `${v || 'panic'}: ${row.last_panic}` : (v || '—') },
  ];

  return (
    <div>
      <PageHeader title="系统监控" subtitle="VPS 资源与注册任务实时占用（页面可见时每 3 秒刷新）"
        actions={<Button icon={<IconRefresh />} onClick={load} loading={loading}>刷新</Button>} />

      <LoadErrorBanner error={error} onRetry={load} />

      <div className="pool-stat-grid" style={{ marginBottom: 18 }}>
        <StatCard label="CPU 使用率" value={(cpu.usage_pct ?? '—') + '%'} color={meterColor(cpu.usage_pct || 0)} sub={`${cpu.cores} 核 · 负载 ${cpu.load1 ?? '—'}`} />
        <StatCard label="内存使用" value={(mem.used_pct ?? '—') + '%'} color={meterColor(mem.used_pct || 0)} sub={`${fmtKB(mem.used_kb)} / ${fmtKB(mem.total_kb)}`} />
        <StatCard label="磁盘使用" value={(disk.used_pct ?? '—') + '%'} color={meterColor(disk.used_pct || 0)} sub={`${fmtBytes(disk.used_bytes)} / ${fmtBytes(disk.total_bytes)}`} />
        <StatCard label="注册任务内存" value={fmtKB(reg.total_rss_kb)} color={C.violet} sub={`node ${reg.node || 0} · chrome ${reg.chrome || 0} · Xvfb ${reg.xvfb || 0}`} />
        <StatCard label="运行时长" value={fmtDuration(s?.uptime_seconds)} color={C.cyan} sub="系统 uptime" />
        <StatCard label="Go 进程" value={`${go.goroutines || 0}`} color={C.blue} sub={`goroutine · 内存 ${fmtBytes(go.sys_bytes)}`} />
      </div>

      <div style={{ marginBottom: 18 }}>
        <SystemHealthSummary system={s} variant="detail" />
      </div>

      <Panel title="模块健康" extra={<Typography.Text type="tertiary" size="small">长驻模块与最近异常</Typography.Text>}>
        <ResourceTable
          loading={loading}
          lastRefresh={lastRefresh}
          dataSource={modules}
          columns={moduleCols}
          rowKey="key"
          size="small"
          pagination={modules.length > 8 ? { pageSize: 8 } : false}
          emptyTitle="当前没有 supervisor 模块状态"
          skeletonRows={6}
          skeletonCols={9}
        />
      </Panel>

      <div style={{ height: 18 }} />

      <Panel title="模块事件" extra={<Typography.Text type="tertiary" size="small">最近 {events.length} 条 supervisor 记录</Typography.Text>}>
        <ResourceTable
          loading={loading}
          lastRefresh={lastRefresh}
          dataSource={events}
          columns={eventCols}
          rowKey="key"
          size="small"
          pagination={events.length > 8 ? { pageSize: 8 } : false}
          emptyTitle="当前没有模块 panic、异常退出或重启记录"
          skeletonRows={6}
          skeletonCols={6}
        />
      </Panel>

      <div style={{ height: 18 }} />

      <Panel title="注册子进程明细">
        <ResourceTable
          loading={loading}
          lastRefresh={lastRefresh}
          dataSource={reg.procs || []}
          columns={procCols}
          rowKey="pid"
          size="small"
          pagination={(reg.procs || []).length > 12 ? { pageSize: 12 } : false}
          emptyTitle="当前没有 node/Chrome/Xvfb 注册进程"
          skeletonRows={6}
          skeletonCols={4}
        />
      </Panel>
    </div>
  );
}
