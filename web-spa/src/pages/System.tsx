import React from 'react';
import { Tag, Button, Typography, Banner, Drawer } from '../components/pool/index.jsx';
import { IconRefresh } from '../components/pool/icons.jsx';
import LoadErrorBanner from '../components/LoadErrorBanner.jsx';
import PageHeader, { Panel } from '../components/PageHeader.jsx';
import ResourceTable from '../components/ResourceTable.jsx';
import * as MicroCharts from '../components/MicroCharts.jsx';
import { COLORS } from '../lib/chartTheme.js';
import { fmtKB, fmtBytes, fmtInt, fmtDuration, fmtDateTime } from '../lib/format.js';
import { heatCells, hourlyBuckets } from '../lib/timeSeries.js';
import { t } from '../lib/i18n.js';
import { useSystemMetricsData } from '../features/observability/queries/system';
import type { PassiveHealthSeries, SupervisorEvent, SupervisorModule, SystemProcess } from '../features/observability/model/system';

const { HeatStrip, RadialGauge, RankedBars, StackedMeter } = MicroCharts as any;
const ErrorBanner = LoadErrorBanner as any;
const DataTable = ResourceTable as any;
const Section = Panel as any;
const C = COLORS;
const kindColor: Record<string, string> = { node: 'green', chrome: 'blue', xvfb: 'violet', other: 'grey' };
const eventColor: Record<string, string> = { panic: 'red', panic_restart: 'red', failed: 'red', unexpected_exit: 'amber', event: 'blue' };
const moduleColor: Record<string, string> = { running: 'green', restarting: 'amber', panic: 'red', failed: 'red', stopped: 'grey' };
const passiveHealthColor: Record<string, string> = { healthy: 'green', degraded: 'amber', unhealthy: 'red', unknown: 'grey' };
const compatibilityColor: Record<string, string> = {
  current: 'green', unchanged: 'green', last_known_good: 'green', degraded_last_known_good: 'amber',
  refreshing: 'blue', waiting: 'amber', disabled: 'grey', unavailable: 'red', error: 'red',
};
const meterColor = (percent: number) => (percent >= 90 ? C.red : percent >= 70 ? C.amber : C.green);

// Chart palette for the same maps above. Tag colours are names, charts need values.
const MODULE_CHART_COLOR: Record<string, string> = { running: C.green, restarting: C.amber, panic: C.red, failed: C.red, stopped: C.grey };
const KIND_CHART_COLOR: Record<string, string> = { node: C.green, chrome: C.blue, xvfb: C.violet, other: C.grey };
// Composition order for the module meter, healthiest first. `panic` and `failed` share a colour
// because both mean the module is down; they stay separate segments so the counts differ.
const MODULE_STATE_ORDER = ['running', 'restarting', 'panic', 'failed', 'stopped'];
// Guard levels, best first. The backend derives the snapshot level from the worst filesystem,
// so these are also the only values a filesystem entry can carry.
const GUARD_COLOR: Record<string, string> = { normal: C.green, pressure: C.amber, critical: C.red };
const guardColor = (level: unknown) => GUARD_COLOR[String(level || '')] || C.grey;
// Event types that mean something went wrong, as opposed to a routine lifecycle notice.
const problemEventTypes = new Set(['panic', 'panic_restart', 'failed', 'unexpected_exit']);
const fmtMillis = (milliseconds: unknown) => milliseconds ? fmtDuration(Math.ceil(Number(milliseconds) / 1000)) : '—';
const fmtHealthPct = (value: unknown) => `${Math.round(Math.max(0, Math.min(1, Number(value) || 0)) * 100)}%`;
const moduleStateLabel = (state: unknown) => {
  const value = String(state || '');
  return value ? t(`system.state.${value}`, value) : t('common.unknown');
};
// Supervisor event types are Go identifiers. Rendered raw they were the only untranslated strings
// in the table, and `unexpected_exit` is long enough to wrap mid-word inside its own tag border
// at the column's width. The fallback keeps any type the backend adds later readable.
const eventTypeLabel = (type: unknown) => {
  const value = String(type || 'event');
  return t(`system.event_type.${value}`, value);
};
const fmtModuleRuntime = (_: unknown, row: SupervisorModule) => fmtMillis(row.status === 'running' ? row.uptime_millis : (row.last_uptime_millis || row.uptime_millis));

type CompactStat = [label: string, value: React.ReactNode];
type SystemDetail = {
  title: string;
  badge?: React.ReactNode;
  rows: CompactStat[];
};

export function CompactSystemRecord({
  title,
  titleLabel,
  badge,
  subtitle,
  stats,
  note,
  onOpen,
}: {
  title: React.ReactNode;
  titleLabel: string;
  badge?: React.ReactNode;
  subtitle?: React.ReactNode;
  stats: CompactStat[];
  note?: React.ReactNode;
  onOpen: () => void;
}) {
  return (
    <button type="button" className="pool-compact-record pool-system-record" onClick={onOpen} aria-label={titleLabel}>
      <span className="pool-compact-record__head">
        <span className="pool-system-record__title" title={titleLabel}>{title}</span>
        {badge}
      </span>
      {subtitle ? <span className="pool-system-record__subtitle">{subtitle}</span> : null}
      <span className="pool-system-record__stats">
        {stats.map(([label, value]) => (
          <span key={label}>
            <small>{label}</small>
            <strong>{value ?? '—'}</strong>
          </span>
        ))}
      </span>
      {note ? <span className="pool-system-record__note">{note}</span> : null}
      <span className="pool-compact-record__disclosure pool-system-record__disclosure" aria-hidden="true">›</span>
    </button>
  );
}

export default function System() {
  const { data: system, loading, error, lastRefresh, reload } = useSystemMetricsData();
  const [detail, setDetail] = React.useState<SystemDetail | null>(null);
  const [modulePage, setModulePage] = React.useState(1);
  const [eventPage, setEventPage] = React.useState(1);
  const [processPage, setProcessPage] = React.useState(1);
  const [passiveHealthPage, setPassiveHealthPage] = React.useState(1);

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
  const compatibility = system?.compatibility_manifest;
  const passiveHealth = system?.passive_provider_health;
  const healthRank: Record<string, number> = { unhealthy: 0, degraded: 1, unknown: 2, healthy: 3 };
  const passiveSeries = [...(passiveHealth?.series || [])].sort((left, right) =>
    (healthRank[left.health] ?? 2) - (healthRank[right.health] ?? 2)
    || right.failures - left.failures
    || right.observations - left.observations);
  const passiveCounts = passiveSeries.reduce<Record<string, number>>((counts, row) => {
    counts[row.health] = (counts[row.health] || 0) + 1;
    return counts;
  }, {});
  const compatibilityState = compatibility?.state || (compatibility?.enabled ? 'waiting' : 'disabled');
  const modulePagination = modules.length > 8 ? { pageSize: 8, currentPage: modulePage, onPageChange: setModulePage } : false;
  const eventPagination = events.length > 8 ? { pageSize: 8, currentPage: eventPage, onPageChange: setEventPage } : false;
  const processPagination = processes.length > 12 ? { pageSize: 12, currentPage: processPage, onPageChange: setProcessPage } : false;
  const passiveHealthPagination = passiveSeries.length > 8 ? { pageSize: 8, currentPage: passiveHealthPage, onPageChange: setPassiveHealthPage } : false;

  // Everything below is derived from the payload the endpoint already returns. These are plain
  // consts rather than memos because the early returns above are hooks boundaries -- a useMemo
  // here would be a conditional hook call -- and the inputs are a handful of short arrays.
  const moduleStateCounts: Record<string, number> = {};
  for (const module of modules) {
    const state = String(module.status || '');
    if (state) moduleStateCounts[state] = (moduleStateCounts[state] || 0) + 1;
  }
  const moduleSegments = MODULE_STATE_ORDER
    .filter((state) => moduleStateCounts[state])
    .map((state) => ({
      key: state,
      value: moduleStateCounts[state],
      label: moduleStateLabel(state),
      color: MODULE_CHART_COLOR[state],
    }));
  const runningModules = moduleStateCounts.running || 0;

  // Ranked by restarts because that is the number that separates "up right now" from "stable":
  // a module on its eleventh restart is reported as running for the seconds between crashes, and
  // the status tag alone cannot show that. Panics and unexpected exits ride along as the subline
  // so one row explains what kind of instability it is.
  const restartRows = modules
    .filter((module) => Number(module.restart_count) > 0 || Number(module.panic_count) > 0)
    .sort((a, b) => (Number(b.restart_count) || 0) - (Number(a.restart_count) || 0))
    .slice(0, 6)
    .map((module) => ({
      key: module.key,
      name: module.name || t('common.unknown'),
      value: Number(module.restart_count) || 0,
      color: MODULE_CHART_COLOR[String(module.status || '')] || C.grey,
      meta: `${moduleStateLabel(module.status)} · ${t('system.panics')} ${fmtInt(module.panic_count)} · ${t('system.unexpected_exits')} ${fmtInt(module.unexpected_exit_count)}`,
    }));

  // Problem events are counted separately from all events so a wall of heartbeats cannot hide a
  // cluster of panics inside the same hour.
  const eventBuckets = hourlyBuckets(events, {
    timeOf: (event: SupervisorEvent) => event.time_unix,
    series: {
      all: () => true,
      problems: (event: SupervisorEvent) => problemEventTypes.has(String(event.type || '')),
    },
  });
  const eventTimeline = eventBuckets && {
    all: heatCells(eventBuckets.counts.all, eventBuckets.slots, t('system.events_unit')),
    problems: heatCells(eventBuckets.counts.problems, eventBuckets.slots, t('system.problems_unit')),
    from: fmtDateTime(eventBuckets.from),
    to: fmtDateTime(eventBuckets.to),
    problemTotal: eventBuckets.totals.problems,
  };

  // Resident memory per registration helper process. The table below lists them all; this ranks
  // the heaviest, which is the only part anyone reads when the box is under memory pressure.
  const processRows = processes
    .filter((proc: SystemProcess) => Number(proc.rss_kb) > 0)
    .sort((a: SystemProcess, b: SystemProcess) => (Number(b.rss_kb) || 0) - (Number(a.rss_kb) || 0))
    .slice(0, 6)
    .map((proc: SystemProcess) => ({
      key: `${proc.pid}`,
      name: proc.comm || t('common.unknown'),
      value: Number(proc.rss_kb) || 0,
      color: KIND_CHART_COLOR[String(proc.kind || 'other')] || C.grey,
      meta: `${proc.kind || 'other'} · pid ${proc.pid}`,
    }));

  // Per-filesystem usage, fullest first. A single "31% used" figure describes whichever path the
  // metrics collector happened to stat; the guard reports every filesystem it manages, and on a
  // multi-volume host those diverge -- the volume about to fill is rarely the root one.
  //
  // The bar plots used space on a fixed 0-100 axis rather than free space normalised to the
  // widest row. Both details matter: free space would make the longest bar the healthiest disk
  // while every other ranking on this page reads longer-as-worse, and the default axis would
  // stretch the roomiest filesystem to full width no matter how much room that actually is.
  const guardFilesystems = (diskGuard?.filesystems || [])
    .map((entry, index) => {
      const freePercent = Math.min(100, Math.max(0, Number(entry.free_percent) || 0));
      return {
        key: `${(entry.roles || []).join('-') || 'fs'}-${index}`,
        name: (entry.roles || []).map((role) => t(`system.disk_role_${role}`, role)).join(' / ') || t('common.unknown'),
        value: Math.round(100 - freePercent),
        color: guardColor(entry.level),
        meta: `${t(`system.disk_guard_${entry.level}`, String(entry.level || ''))} · ${t('system.free_percent').replace('{percent}', String(Math.round(freePercent)))} · ${t('system.available').replace('{size}', fmtBytes(entry.free_bytes))}`,
      };
    })
    .sort((a, b) => b.value - a.value);

  // Only degraded flags are rendered. Listing three writable filesystems and two unpaused
  // subsystems every time trains the eye to skip the row that matters, so the nominal case
  // collapses to one chip instead of five.
  const guardFlags = diskGuard
    ? [
      diskGuard.admission_blocked ? { key: 'admission', color: 'red', label: t('system.guard_admission_blocked') } : null,
      diskGuard.database_writable === false ? { key: 'database', color: 'red', label: t('system.guard_database_readonly') } : null,
      diskGuard.journal_writable === false ? { key: 'journal', color: 'red', label: t('system.guard_journal_readonly') } : null,
      diskGuard.spool_writable === false ? { key: 'spool', color: 'red', label: t('system.guard_spool_readonly') } : null,
      diskGuard.large_requests_paused ? { key: 'large', color: 'amber', label: t('system.guard_large_paused') } : null,
      diskGuard.background_paused ? { key: 'background', color: 'amber', label: t('system.guard_background_paused') } : null,
    ].filter(Boolean) as Array<{ key: string; color: string; label: string }>
    : [];

  const openModule = (row: SupervisorModule) => {
    const name = row.name || t('common.unknown');
    setDetail({
      title: `${t('system.module')} · ${name}`,
      badge: <Tag color={moduleColor[row.status || ''] || 'grey'}>{moduleStateLabel(row.status)}</Tag>,
      rows: [
        [t('system.module'), name],
        [t('system.status'), moduleStateLabel(row.status)],
        [t('system.restarts'), fmtInt(row.restart_count)],
        [t('system.panics'), fmtInt(row.panic_count)],
        [t('system.unexpected_exits'), fmtInt(row.unexpected_exit_count)],
        [t('system.current_previous'), fmtModuleRuntime(null, row)],
        [t('system.backoff'), fmtMillis(row.restart_backoff_millis)],
        [t('system.next_retry'), fmtDateTime(row.next_restart_unix)],
        [t('system.recent_status'), row.last_panic ? `${row.last_message || 'panic'}: ${row.last_panic}` : (row.last_message || '—')],
      ],
    });
  };

  const openEvent = (row: SupervisorEvent) => {
    const type = row.type || 'event';
    const message = row.panic ? `${row.message || 'panic'}: ${row.panic}` : (row.message || '—');
    setDetail({
      title: `${t('system.module_events')} · ${row.module || type}`,
      badge: <Tag color={eventColor[type] || 'grey'}>{eventTypeLabel(type)}</Tag>,
      rows: [
        [t('system.time'), fmtDateTime(row.time_unix)],
        [t('system.module'), row.module || '—'],
        [t('system.kind'), type],
        [t('system.description'), message],
        [t('system.runtime'), fmtMillis(row.uptime_millis)],
        [t('system.backoff'), fmtMillis(row.backoff_millis)],
      ],
    });
  };

  const openProcess = (row: SystemProcess) => {
    const processName = row.comm || t('common.unknown');
    setDetail({
      title: `${t('system.process')} · ${processName}`,
      badge: <Tag color={kindColor[row.kind || ''] || 'grey'}>{row.kind || 'other'}</Tag>,
      rows: [
        [t('system.pid'), row.pid],
        [t('system.process'), processName],
        [t('system.kind'), row.kind || 'other'],
        [t('system.rss'), fmtKB(row.rss_kb)],
      ],
    });
  };

  const openPassiveHealth = (row: PassiveHealthSeries) => {
    const title = `${row.provider} · ${row.model}`;
    setDetail({
      title: `${t('system.passive_health')} · ${title}`,
      badge: <Tag color={passiveHealthColor[row.health] || 'grey'}>{t(`system.passive_${row.health}`, row.health)}</Tag>,
      rows: [
        [t('system.provider'), row.provider],
        [t('system.model'), row.model],
        [t('system.egress'), row.egress_id],
        [t('system.success_ewma'), fmtHealthPct(row.success_ewma)],
        [t('system.latency_ewma'), `${fmtInt(Math.round(row.latency_ewma_ms))} ms`],
        [t('system.observations'), fmtInt(row.observations)],
        [t('system.failures'), fmtInt(row.failures)],
        [t('system.rate_limited'), fmtInt(row.rate_limited)],
        [t('system.canceled'), fmtInt(row.canceled)],
        [t('system.last_status'), row.last_status_code || '—'],
        [t('system.last_error_class'), row.last_error_class || '—'],
        [t('system.last_seen'), fmtDateTime(row.last_observed_at)],
      ],
    });
  };

  const processColumns: any[] = [
    { title: t('system.pid'), dataIndex: 'pid', width: 90, render: (value: number | string) => <span className="pool-mono">{value}</span> },
    { title: t('system.process'), dataIndex: 'comm' },
    { title: t('system.kind'), dataIndex: 'kind', width: 100, render: (kind: string) => <Tag color={kindColor[kind] || 'grey'}>{kind}</Tag> },
    { title: t('system.rss'), dataIndex: 'rss_kb', width: 140, align: 'right', sorter: (a: SystemProcess, b: SystemProcess) => Number(a.rss_kb || 0) - Number(b.rss_kb || 0), defaultSortOrder: 'descend', render: fmtKB },
  ];
  const eventColumns: any[] = [
    { title: t('system.time'), dataIndex: 'time_unix', width: 140, render: fmtDateTime },
    { title: t('system.module'), dataIndex: 'module', width: 180, render: (value: string) => <span className="pool-mono">{value}</span> },
    { title: t('system.kind'), dataIndex: 'type', width: 130, render: (value: string | undefined) => <Tag color={eventColor[value || 'event'] || 'grey'}>{eventTypeLabel(value)}</Tag> },
    { title: t('system.description'), dataIndex: 'message', render: (value: string | undefined, row: SupervisorEvent) => row.panic ? `${value || 'panic'}: ${row.panic}` : (value || '—') },
    { title: t('system.runtime'), dataIndex: 'uptime_millis', width: 110, align: 'right', render: fmtMillis },
    { title: t('system.backoff'), dataIndex: 'backoff_millis', width: 110, align: 'right', render: fmtMillis },
  ];
  const moduleColumns: any[] = [
    { title: t('system.module'), dataIndex: 'name', width: 190, render: (value: string) => <span className="pool-mono">{value}</span> },
    { title: t('system.status'), dataIndex: 'status', width: 110, render: (value: string | undefined) => <Tag color={moduleColor[value || ''] || 'grey'}>{moduleStateLabel(value)}</Tag> },
    { title: t('system.restarts'), dataIndex: 'restart_count', width: 90, align: 'right', sorter: (a: SupervisorModule, b: SupervisorModule) => (a.restart_count || 0) - (b.restart_count || 0), render: fmtInt },
    { title: t('system.panics'), dataIndex: 'panic_count', width: 90, align: 'right', sorter: (a: SupervisorModule, b: SupervisorModule) => (a.panic_count || 0) - (b.panic_count || 0), render: fmtInt },
    { title: t('system.unexpected_exits'), dataIndex: 'unexpected_exit_count', width: 110, align: 'right', sorter: (a: SupervisorModule, b: SupervisorModule) => (a.unexpected_exit_count || 0) - (b.unexpected_exit_count || 0), render: fmtInt },
    { title: t('system.current_previous'), dataIndex: 'uptime_millis', width: 110, align: 'right', render: fmtModuleRuntime },
    { title: t('system.backoff'), dataIndex: 'restart_backoff_millis', width: 110, align: 'right', render: fmtMillis },
    { title: t('system.next_retry'), dataIndex: 'next_restart_unix', width: 140, render: fmtDateTime },
    { title: t('system.recent_status'), dataIndex: 'last_message', render: (value: string | undefined, row: SupervisorModule) => row.last_panic ? `${value || 'panic'}: ${row.last_panic}` : (value || '—') },
  ];
  const passiveHealthColumns: any[] = [
    { title: t('system.provider'), dataIndex: 'provider', width: 130, render: (value: string) => <span className="pool-mono">{value}</span> },
    { title: t('system.model'), dataIndex: 'model', width: 190, render: (value: string) => <span className="pool-mono">{value}</span> },
    { title: t('system.egress'), dataIndex: 'egress_id', width: 130, render: (value: string) => <span className="pool-mono">{value}</span> },
    { title: t('system.health_state'), dataIndex: 'health', width: 110, render: (value: string) => <Tag color={passiveHealthColor[value] || 'grey'}>{t(`system.passive_${value}`, value)}</Tag> },
    { title: t('system.success_ewma'), dataIndex: 'success_ewma', width: 110, align: 'right', sorter: (a: PassiveHealthSeries, b: PassiveHealthSeries) => a.success_ewma - b.success_ewma, render: fmtHealthPct },
    { title: t('system.latency_ewma'), dataIndex: 'latency_ewma_ms', width: 120, align: 'right', sorter: (a: PassiveHealthSeries, b: PassiveHealthSeries) => a.latency_ewma_ms - b.latency_ewma_ms, render: (value: number) => `${fmtInt(Math.round(value))} ms` },
    { title: t('system.observations'), dataIndex: 'observations', width: 100, align: 'right', render: fmtInt },
    { title: t('system.failures'), dataIndex: 'failures', width: 90, align: 'right', render: fmtInt },
    { title: t('system.rate_limited'), dataIndex: 'rate_limited', width: 100, align: 'right', render: fmtInt },
    { title: t('system.last_error_class'), dataIndex: 'last_error_class', width: 150, render: (value: string) => value || '—' },
    { title: t('system.last_seen'), dataIndex: 'last_observed_at', width: 170, render: fmtDateTime },
  ];

  return (
    <div>
      <PageHeader title={t('system.title')} subtitle={t('system.subtitle')}
        actions={<Button icon={<IconRefresh />} onClick={reload} loading={loading}>{t('common.refresh')}</Button>} />

      <ErrorBanner error={error} onRetry={reload} />

      {/* This replaced eight stat cards followed by a panel that re-rendered CPU, memory, disk,
          uptime and the Go runtime as progress bars -- five of the eight cards were restated
          within the same viewport, and nothing on the page was a chart. */}
      <section className="pool-sys-overview">
        <div className="pool-chart-card pool-sys-load">
          <div className="head">
            <div>
              <div className="t">{t('system.runtime_load')}</div>
              <div className="s">
                {`${cpu.cores || 0} ${t('system.cores')} · ${t('system.load')} ${cpu.load1 ?? '—'} · ${t('system.uptime')} ${fmtDuration(system?.uptime_seconds)}`}
              </div>
            </div>
          </div>
          <div className="pool-sys-load__gauges">
            <RadialGauge
              value={(cpu.usage_pct || 0) / 100}
              size={104}
              color={meterColor(cpu.usage_pct || 0)}
              caption={t('system.resource_cpu')}
              label={`${t('system.load')} ${cpu.load1 ?? '—'}`}
            />
            <RadialGauge
              value={(memory.used_pct || 0) / 100}
              size={104}
              color={meterColor(memory.used_pct || 0)}
              caption={t('system.resource_memory')}
              label={`${fmtKB(memory.used_kb)} / ${fmtKB(memory.total_kb)}`}
            />
            <RadialGauge
              value={(disk.used_pct || 0) / 100}
              size={104}
              color={meterColor(disk.used_pct || 0)}
              caption={t('system.resource_disk')}
              label={`${fmtBytes(disk.used_bytes)} / ${fmtBytes(disk.total_bytes)}`}
            />
          </div>
          <div className="pool-sys-load__foot">
            <div className="pool-sys-load__net">
              <span className="pool-sys-load__net-label">{t('system.network_throughput')}</span>
              <strong>{`${fmtBytes(network.total_bytes_per_sec)}/s`}</strong>
              <span className="pool-muted">
                {`${t('system.network_rx')} ${fmtBytes(network.rx_bytes_per_sec)}/s · ${t('system.network_tx')} ${fmtBytes(network.tx_bytes_per_sec)}/s · ${network.interfaces || 0} ${t('system.interfaces')}`}
              </span>
            </div>
            <div className="pool-sys-load__net">
              <span className="pool-sys-load__net-label">{t('system.go_process')}</span>
              <strong>{fmtInt(go.goroutines)}</strong>
              <span className="pool-muted">{t('system.goroutine_memory').replace('{memory}', fmtBytes(go.sys_bytes))}</span>
            </div>
          </div>
        </div>

        {diskGuard ? (
          <div className="pool-chart-card pool-sys-guard">
            <div className="head">
              <div>
                <div className="t">{t('system.disk_guard')}</div>
                <div className="s">{t('system.guard_used_space')}</div>
              </div>
              <Tag color={diskGuard.level === 'critical' ? 'red' : diskGuard.level === 'pressure' ? 'amber' : 'green'}>
                {t(`system.disk_guard_${diskGuard.level}`, diskGuard.level)}
              </Tag>
            </div>
            <RankedBars
              rows={guardFilesystems}
              max={100}
              keepZero
              valueFormatter={(value: number) => `${value}%`}
              emptyText={t('system.available').replace('{size}', fmtBytes(diskGuard.free_bytes))}
              ariaLabel={t('system.guard_used_space')}
            />
            <div className="pool-sys-guard__flags">
              {guardFlags.length
                ? guardFlags.map((flag) => <Tag key={flag.key} size="small" color={flag.color}>{flag.label}</Tag>)
                : <Tag size="small" color="green">{t('system.guard_all_writable')}</Tag>}
            </div>
            <div className="pool-sys-guard__note">
              {[
                t('system.guard_ttl').replace('{duration}', fmtDuration(diskGuard.forced_context_ttl_seconds || 3600)),
                `${t('system.contexts_deleted')} ${fmtInt(diskGuard.contexts_deleted)}`,
                diskGuard.goal_bytes_reclaimed ? t('system.guard_reclaimed').replace('{size}', fmtBytes(diskGuard.goal_bytes_reclaimed)) : '',
                diskGuard.last_run_at ? t('system.guard_last_run').replace('{time}', fmtDateTime(diskGuard.last_run_at)) : '',
              ].filter(Boolean).join(' · ')}
            </div>
            {diskGuard.last_error ? <div className="pool-sys-guard__error">{diskGuard.last_error}</div> : null}
          </div>
        ) : null}
      </section>

      <section className="pool-sys-modules">
        <div className="pool-chart-card">
          <div className="head">
            <div>
              <div className="t">{t('system.compatibility_manifest')}</div>
              <div className="s">{t('system.compatibility_manifest_desc')}</div>
            </div>
            <Tag color={compatibilityColor[compatibilityState] || 'grey'}>
              {t(`system.compatibility_${compatibilityState}`, compatibilityState)}
            </Tag>
          </div>
          <dl className="pool-system-detail__grid">
            <div><dt>{t('system.source')}</dt><dd>{compatibility?.source || '—'}</dd></div>
            <div><dt>{t('system.generation')}</dt><dd>{fmtInt(compatibility?.generation)}</dd></div>
            <div><dt>{t('system.models')}</dt><dd>{fmtInt(compatibility?.model_count)}</dd></div>
            <div><dt>{t('system.snapshot')}</dt><dd>{compatibility?.snapshot_slot || '—'}</dd></div>
            <div><dt>{t('system.canary')}</dt><dd>{compatibility?.canary || '—'}</dd></div>
            <div><dt>{t('system.last_success')}</dt><dd>{fmtDateTime(compatibility?.last_success_at)}</dd></div>
          </dl>
          {compatibility?.last_error ? <div className="pool-sys-guard__error">{compatibility.last_error}</div> : null}
        </div>
        <div className="pool-chart-card">
          <div className="head">
            <div>
              <div className="t">{t('system.passive_health')}</div>
              <div className="s">{t('system.passive_health_desc')}</div>
            </div>
            <Tag color={(passiveCounts.unhealthy || 0) > 0 ? 'red' : (passiveCounts.degraded || 0) > 0 ? 'amber' : 'green'}>
              {fmtInt(passiveHealth?.series_count || 0)} {t('system.series')}
            </Tag>
          </div>
          <dl className="pool-system-detail__grid">
            <div><dt>{t('system.passive_healthy')}</dt><dd>{fmtInt(passiveCounts.healthy || 0)}</dd></div>
            <div><dt>{t('system.passive_degraded')}</dt><dd>{fmtInt(passiveCounts.degraded || 0)}</dd></div>
            <div><dt>{t('system.passive_unhealthy')}</dt><dd>{fmtInt(passiveCounts.unhealthy || 0)}</dd></div>
            <div><dt>{t('system.passive_unknown')}</dt><dd>{fmtInt(passiveCounts.unknown || 0)}</dd></div>
            <div><dt>{t('system.evictions')}</dt><dd>{fmtInt(passiveHealth?.evictions || 0)}</dd></div>
            <div><dt>{t('system.retention')}</dt><dd>{fmtDuration(passiveHealth?.retention_seconds)}</dd></div>
          </dl>
        </div>
      </section>

      <section className="pool-sys-modules">
        <div className="pool-chart-card">
          <div className="head">
            <div>
              <div className="t">{t('system.module_composition')}</div>
              <div className="s">{t('system.modules_running').replace('{running}', fmtInt(runningModules)).replace('{total}', fmtInt(modules.length))}</div>
            </div>
          </div>
          <StackedMeter segments={moduleSegments} ariaLabel={t('system.module_composition')} />
        </div>
        <div className="pool-chart-card">
          <div className="head">
            <div>
              <div className="t">{t('system.module_restarts')}</div>
              <div className="s">{t('system.registration_memory')}{' · '}{fmtKB(registration.total_rss_kb)}{` · node ${registration.node || 0} · chrome ${registration.chrome || 0} · Xvfb ${registration.xvfb || 0}`}</div>
            </div>
          </div>
          <RankedBars
            rows={restartRows}
            valueFormatter={(value: number) => fmtInt(value)}
            emptyText={t('system.module_all_stable')}
            ariaLabel={t('system.module_restarts')}
          />
        </div>
      </section>

      {eventTimeline ? (
        <section className="pool-chart-card pool-sys-timeline">
          <div className="head">
            <div>
              <div className="t">{t('system.event_cadence')}</div>
              <div className="s">
                {eventTimeline.problemTotal
                  ? t('system.event_cadence_alert').replace('{count}', fmtInt(eventTimeline.problemTotal))
                  : t('system.event_cadence_calm')}
              </div>
            </div>
          </div>
          <div className="pool-sys-timeline__rows">
            <div className="pool-sys-timeline__row">
              <span className="pool-sys-timeline__label">{t('system.events_row')}</span>
              <HeatStrip cells={eventTimeline.all} color={C.blue} ariaLabel={t('system.events_hourly')} />
            </div>
            <div className="pool-sys-timeline__row">
              <span className="pool-sys-timeline__label">{t('system.problems_row')}</span>
              <HeatStrip cells={eventTimeline.problems} color={C.red} ariaLabel={t('system.problems_hourly')} />
            </div>
          </div>
          <div className="pool-sys-timeline__axis">
            <span>{eventTimeline.from}</span>
            <span>{eventTimeline.to}</span>
          </div>
        </section>
      ) : null}

      {processRows.length ? (
        <section className="pool-chart-card pool-sys-procs">
          <div className="head">
            <div>
              <div className="t">{t('system.process_memory')}</div>
              <div className="s">{`${fmtKB(registration.total_rss_kb)} · ${fmtInt(processes.length)} × ${t('system.process')}`}</div>
            </div>
          </div>
          <RankedBars
            rows={processRows}
            valueFormatter={(value: number) => fmtKB(value)}
            emptyText={t('system.process_memory_empty')}
            ariaLabel={t('system.process_memory')}
          />
        </section>
      ) : null}

      <Section title={t('system.passive_health')} extra={<Typography.Text type="tertiary" size="small">{t('system.passive_health_table_desc')}</Typography.Text>}>
        <DataTable
          loading={loading}
          lastRefresh={lastRefresh}
          dataSource={passiveSeries}
          columns={passiveHealthColumns}
          rowKey={(row: PassiveHealthSeries) => `${row.provider}:${row.model}:${row.egress_id}`}
          size="small"
          pagination={passiveHealthPagination}
          emptyTitle={t('system.no_passive_health')}
          emptyDesc={t('system.no_passive_health_desc')}
          skeletonRows={5}
          skeletonCols={11}
          density="compact"
          minScrollX={1390}
          mobileListLabel={t('system.passive_health')}
          mobileRenderer={(row: PassiveHealthSeries) => (
            <CompactSystemRecord
              title={<span className="pool-mono">{row.provider} · {row.model}</span>}
              titleLabel={`${row.provider} ${row.model} ${row.egress_id}`}
              badge={<Tag color={passiveHealthColor[row.health] || 'grey'}>{t(`system.passive_${row.health}`, row.health)}</Tag>}
              subtitle={row.egress_id}
              stats={[
                [t('system.success_ewma'), fmtHealthPct(row.success_ewma)],
                [t('system.latency_ewma'), `${fmtInt(Math.round(row.latency_ewma_ms))} ms`],
                [t('system.failures'), fmtInt(row.failures)],
              ]}
              note={`${t('system.last_seen')} ${fmtDateTime(row.last_observed_at)}`}
              onOpen={() => openPassiveHealth(row)}
            />
          )}
        />
      </Section>

      <div style={{ height: 18 }} />

      <Section title={t('system.module_health')} extra={<Typography.Text type="tertiary" size="small">{t('system.module_health_desc')}</Typography.Text>}>
        <DataTable
          loading={loading}
          lastRefresh={lastRefresh}
          dataSource={modules}
          columns={moduleColumns}
          rowKey="key"
          size="small"
          pagination={modulePagination}
          emptyTitle={t('system.no_modules')}
          skeletonRows={6}
          skeletonCols={9}
          mobileListLabel={t('system.module_health')}
          mobileRenderer={(row: SupervisorModule) => {
            const name = row.name || t('common.unknown');
            const message = row.last_panic ? `${row.last_message || 'panic'}: ${row.last_panic}` : (row.last_message || '—');
            return (
              <CompactSystemRecord
                title={<span className="pool-mono">{name}</span>}
                titleLabel={`${t('system.module')} ${name}`}
                badge={<Tag color={moduleColor[row.status || ''] || 'grey'}>{moduleStateLabel(row.status)}</Tag>}
                stats={[
                  [t('system.restarts'), fmtInt(row.restart_count)],
                  [t('system.panics'), fmtInt(row.panic_count)],
                  [t('system.unexpected_exits'), fmtInt(row.unexpected_exit_count)],
                ]}
                note={`${fmtModuleRuntime(null, row)} · ${message}`}
                onOpen={() => openModule(row)}
              />
            );
          }}
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
          pagination={eventPagination}
          emptyTitle={t('system.no_events')}
          skeletonRows={6}
          skeletonCols={6}
          mobileListLabel={t('system.module_events')}
          mobileRenderer={(row: SupervisorEvent) => {
            const type = row.type || 'event';
            const message = row.panic ? `${row.message || 'panic'}: ${row.panic}` : (row.message || '—');
            return (
              <CompactSystemRecord
                title={<span className="pool-mono">{row.module || t('common.unknown')}</span>}
                titleLabel={`${t('system.module_events')} ${row.module || type}`}
                badge={<Tag color={eventColor[type] || 'grey'}>{eventTypeLabel(type)}</Tag>}
                subtitle={fmtDateTime(row.time_unix)}
                stats={[
                  [t('system.runtime'), fmtMillis(row.uptime_millis)],
                  [t('system.backoff'), fmtMillis(row.backoff_millis)],
                ]}
                note={message}
                onOpen={() => openEvent(row)}
              />
            );
          }}
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
          pagination={processPagination}
          emptyTitle={t('system.no_processes')}
          skeletonRows={6}
          skeletonCols={4}
          mobileListLabel={t('system.processes')}
          mobileRenderer={(row: SystemProcess) => (
            <CompactSystemRecord
              title={<span className="pool-mono">{row.comm || t('common.unknown')}</span>}
              titleLabel={`${t('system.process')} ${row.comm || row.pid}`}
              badge={<Tag color={kindColor[row.kind || ''] || 'grey'}>{row.kind || 'other'}</Tag>}
              subtitle={`${t('system.pid')} ${row.pid}`}
              stats={[[t('system.rss'), fmtKB(row.rss_kb)]]}
              onOpen={() => openProcess(row)}
            />
          )}
        />
      </Section>

      <Drawer
        visible={Boolean(detail)}
        onCancel={() => setDetail(null)}
        title={detail?.title || t('system.recent_status')}
        width={560}
      >
        {detail ? (
          <div className="pool-system-detail">
            {detail.badge ? <div className="pool-system-detail__badge">{detail.badge}</div> : null}
            <dl className="pool-system-detail__grid">
              {detail.rows.map(([label, value]) => (
                <div key={label}>
                  <dt>{label}</dt>
                  <dd>{value ?? '—'}</dd>
                </div>
              ))}
            </dl>
          </div>
        ) : null}
      </Drawer>
    </div>
  );
}
