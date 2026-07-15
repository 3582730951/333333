import React from 'react';
import * as PoolUI from '../components/pool/index.jsx';
import { IconRefresh } from '../components/pool/icons.jsx';
import LoadErrorBanner from '../components/LoadErrorBanner.jsx';
import PageHeader, { Panel } from '../components/PageHeader.jsx';
import ResourceTable from '../components/ResourceTable.jsx';
import StatCard from '../components/StatCard.jsx';
import SystemHealthSummary from '../components/SystemHealthSummary.jsx';
import { COLORS } from '../lib/chartTheme.js';
import { fmtKB, fmtBytes, fmtInt, fmtDuration, fmtDateTime } from '../lib/format.js';
import { t } from '../lib/i18n.js';
import { useSystemMetricsData } from '../features/observability/queries/system';
import type { SupervisorEvent, SupervisorModule, SystemProcess } from '../features/observability/model/system';

const { Tag, Button, Typography, Banner } = PoolUI as any;
const ErrorBanner = LoadErrorBanner as any;
const DataTable = ResourceTable as any;
const MetricCard = StatCard as any;
const HealthSummary = SystemHealthSummary as any;
const Section = Panel as any;
const C = COLORS;
const kindColor: Record<string, string> = { node: 'green', chrome: 'blue', xvfb: 'violet', other: 'grey' };
const eventColor: Record<string, string> = { panic: 'red', panic_restart: 'red', failed: 'red', unexpected_exit: 'amber', event: 'blue' };
const moduleColor: Record<string, string> = { running: 'green', restarting: 'amber', panic: 'red', failed: 'red', stopped: 'grey' };
const meterColor = (percent: number) => (percent >= 90 ? C.red : percent >= 70 ? C.amber : C.green);
const fmtMillis = (milliseconds: unknown) => milliseconds ? fmtDuration(Math.ceil(Number(milliseconds) / 1000)) : '—';
const moduleStateLabel = (state: unknown) => {
  const value = String(state || '');
  return value ? t(`system.state.${value}`, value) : t('common.unknown');
};
const fmtModuleRuntime = (_: unknown, row: SupervisorModule) => fmtMillis(row.status === 'running' ? row.uptime_millis : (row.last_uptime_millis || row.uptime_millis));

export default function System() {
  const { data: system, loading, error, lastRefresh, reload } = useSystemMetricsData();

  if (error && !lastRefresh && !loading) {
    return (
      <div>
        <PageHeader title={t('system.title')} subtitle={t('system.subtitle')} actions={<Button icon={<IconRefresh />} onClick={reload}>{t('common.refresh')}</Button>} />
        <ErrorBanner error={error} onRetry={reload} title={t('system.metrics_failed')} />
      </div>
    );
  }

  if (system && !system.supported) {
    return (
      <div>
        <PageHeader title={t('system.title')} subtitle={t('system.subtitle')} actions={<Button icon={<IconRefresh />} onClick={reload} loading={loading}>{t('common.refresh')}</Button>} />
        <ErrorBanner error={error} onRetry={reload} />
        <Banner type="warning" description={t('system.unsupported')} />
      </div>
    );
  }

  const cpu = system?.cpu || {};
  const memory = system?.mem || {};
  const disk = system?.disk || {};
  const network = system?.network || {};
  const diskGuard = system?.disk_guard;
  const registration = system?.registration || {};
  const go = system?.go || {};
  const events = (system?.supervisor_events || []).map((event, index) => ({
    ...event,
    key: `${event.time_unix || 0}-${event.module || 'module'}-${index}`,
  }));
  const modules = (system?.supervisor_modules || []).map((module, index) => ({
    ...module,
    key: module.name || `${module.status || 'module'}-${module.last_event_unix || 0}-${index}`,
  }));
  const processes = registration.procs || [];

  const processColumns: any[] = [
    { title: t('system.pid'), dataIndex: 'pid', width: 90, render: (value: number | string) => <span className="pool-mono">{value}</span> },
    { title: t('system.process'), dataIndex: 'comm' },
    { title: t('system.kind'), dataIndex: 'kind', width: 100, render: (kind: string) => <Tag color={kindColor[kind] || 'grey'}>{kind}</Tag> },
    { title: t('system.rss'), dataIndex: 'rss_kb', width: 140, align: 'right', sorter: (a: SystemProcess, b: SystemProcess) => Number(a.rss_kb || 0) - Number(b.rss_kb || 0), defaultSortOrder: 'descend', render: fmtKB },
  ];
  const eventColumns: any[] = [
    { title: t('system.time'), dataIndex: 'time_unix', width: 140, render: fmtDateTime },
    { title: t('system.module'), dataIndex: 'module', width: 180, render: (value: string) => <span className="pool-mono">{value}</span> },
    { title: t('system.kind'), dataIndex: 'type', width: 130, render: (value: string | undefined) => <Tag color={eventColor[value || 'event'] || 'grey'}>{value || 'event'}</Tag> },
    { title: t('system.description'), dataIndex: 'message', render: (value: string | undefined, row: SupervisorEvent) => row.panic ? `${value || 'panic'}: ${row.panic}` : (value || '—') },
    { title: t('system.runtime'), dataIndex: 'uptime_millis', width: 110, align: 'right', render: fmtMillis },
    { title: t('system.backoff'), dataIndex: 'backoff_millis', width: 110, align: 'right', render: fmtMillis },
  ];
  const moduleColumns: any[] = [
    { title: t('system.module'), dataIndex: 'name', width: 190, render: (value: string) => <span className="pool-mono">{value}</span> },
    { title: t('system.status'), dataIndex: 'status', width: 110, render: (value: string | undefined) => <Tag color={moduleColor[value || ''] || 'grey'}>{moduleStateLabel(value)}</Tag> },
    { title: t('system.restarts'), dataIndex: 'restart_count', width: 90, align: 'right', sorter: (a: SupervisorModule, b: SupervisorModule) => (a.restart_count || 0) - (b.restart_count || 0), render: fmtInt },
    { title: 'panic', dataIndex: 'panic_count', width: 90, align: 'right', sorter: (a: SupervisorModule, b: SupervisorModule) => (a.panic_count || 0) - (b.panic_count || 0), render: fmtInt },
    { title: t('system.unexpected_exits'), dataIndex: 'unexpected_exit_count', width: 110, align: 'right', sorter: (a: SupervisorModule, b: SupervisorModule) => (a.unexpected_exit_count || 0) - (b.unexpected_exit_count || 0), render: fmtInt },
    { title: t('system.current_previous'), dataIndex: 'uptime_millis', width: 110, align: 'right', render: fmtModuleRuntime },
    { title: t('system.backoff'), dataIndex: 'restart_backoff_millis', width: 110, align: 'right', render: fmtMillis },
    { title: t('system.next_retry'), dataIndex: 'next_restart_unix', width: 140, render: fmtDateTime },
    { title: t('system.recent_status'), dataIndex: 'last_message', render: (value: string | undefined, row: SupervisorModule) => row.last_panic ? `${value || 'panic'}: ${row.last_panic}` : (value || '—') },
  ];

  return (
    <div>
      <PageHeader title={t('system.title')} subtitle={t('system.subtitle')}
        actions={<Button icon={<IconRefresh />} onClick={reload} loading={loading}>{t('common.refresh')}</Button>} />

      <ErrorBanner error={error} onRetry={reload} />

      <div className="pool-stat-grid" style={{ marginBottom: 18 }}>
        <MetricCard label={t('system.cpu_usage')} value={`${cpu.usage_pct ?? '—'}%`} color={meterColor(cpu.usage_pct || 0)} sub={`${cpu.cores || 0} ${t('system.cores')} · ${t('system.load')} ${cpu.load1 ?? '—'}`} />
        <MetricCard label={t('system.memory_usage')} value={`${memory.used_pct ?? '—'}%`} color={meterColor(memory.used_pct || 0)} sub={`${fmtKB(memory.used_kb)} / ${fmtKB(memory.total_kb)}`} />
        <MetricCard label={t('system.disk_usage')} value={`${disk.used_pct ?? '—'}%`} color={meterColor(disk.used_pct || 0)} sub={`${fmtBytes(disk.used_bytes)} / ${fmtBytes(disk.total_bytes)}`} />
        <MetricCard label={t('system.network_traffic')} value={`${fmtBytes(network.total_bytes_per_sec)}/s`} color={C.cyan} sub={`${t('system.network_rx')} ${fmtBytes(network.rx_bytes_per_sec)}/s · ${t('system.network_tx')} ${fmtBytes(network.tx_bytes_per_sec)}/s · ${network.interfaces || 0} ${t('system.interfaces')}`} />
        <MetricCard label={t('system.disk_guard')} value={diskGuard ? `${diskGuard.free_percent}%` : '—'} color={diskGuard?.level === 'critical' ? C.red : diskGuard?.level === 'pressure' ? C.amber : C.green} sub={diskGuard ? `${t(`system.disk_guard_${diskGuard.level}`, diskGuard.level)} · TTL ${diskGuard.forced_context_ttl_seconds || 3600}s · ${t('system.contexts_deleted')} ${fmtInt(diskGuard.contexts_deleted)}` : '—'} />
        <MetricCard label={t('system.registration_memory')} value={fmtKB(registration.total_rss_kb)} color={C.violet} sub={`node ${registration.node || 0} · chrome ${registration.chrome || 0} · Xvfb ${registration.xvfb || 0}`} />
        <MetricCard label={t('system.uptime')} value={fmtDuration(system?.uptime_seconds)} color={C.cyan} sub={t('system.system_uptime')} />
        <MetricCard label={t('system.go_process')} value={`${go.goroutines || 0}`} color={C.blue} sub={t('system.goroutine_memory').replace('{memory}', fmtBytes(go.sys_bytes))} />
      </div>

      <div style={{ marginBottom: 18 }}>
        <HealthSummary system={system} variant="detail" />
      </div>

      <Section title={t('system.module_health')} extra={<Typography.Text type="tertiary" size="small">{t('system.module_health_desc')}</Typography.Text>}>
        <DataTable
          loading={loading}
          lastRefresh={lastRefresh}
          dataSource={modules}
          columns={moduleColumns}
          rowKey="key"
          size="small"
          pagination={modules.length > 8 ? { pageSize: 8 } : false}
          emptyTitle={t('system.no_modules')}
          skeletonRows={6}
          skeletonCols={9}
        />
      </Section>

      <div style={{ height: 18 }} />

      <Section title={t('system.module_events')} extra={<Typography.Text type="tertiary" size="small">{t('system.module_events_desc').replace('{count}', String(events.length))}</Typography.Text>}>
        <DataTable
          loading={loading}
          lastRefresh={lastRefresh}
          dataSource={events}
          columns={eventColumns}
          rowKey="key"
          size="small"
          pagination={events.length > 8 ? { pageSize: 8 } : false}
          emptyTitle={t('system.no_events')}
          skeletonRows={6}
          skeletonCols={6}
        />
      </Section>

      <div style={{ height: 18 }} />

      <Section title={t('system.processes')}>
        <DataTable
          loading={loading}
          lastRefresh={lastRefresh}
          dataSource={processes}
          columns={processColumns}
          rowKey="pid"
          size="small"
          pagination={processes.length > 12 ? { pageSize: 12 } : false}
          emptyTitle={t('system.no_processes')}
          skeletonRows={6}
          skeletonCols={4}
        />
      </Section>
    </div>
  );
}
