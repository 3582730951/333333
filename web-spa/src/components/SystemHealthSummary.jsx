import React from 'react';
import { Tag, Typography } from './pool/index.jsx';
import { Panel, Meter } from './PageHeader.jsx';
import { COLORS } from '../lib/chartTheme.js';
import { fmtBytes, fmtDuration, fmtInt, fmtKB, fmtRelative } from '../lib/format.js';
import { t } from '../lib/i18n.js';

const C = COLORS;
const moduleColor = { running: 'green', completed: 'green', restarting: 'amber', panic: 'red', failed: 'red', stopped: 'grey' };
const eventColor = { panic: 'red', panic_restart: 'red', failed: 'red', unexpected_exit: 'amber', event: 'blue' };
const meterColor = (p) => (p >= 90 ? C.red : p >= 70 ? C.amber : C.green);
const problemStatuses = new Set(['panic', 'restarting', 'failed']);
const fmtMillis = (ms) => (ms ? fmtDuration(Math.ceil(Number(ms) / 1000)) : '');

function shortEventText(event) {
  const text = String(event?.panic || event?.message || '').trim();
  if (!text) return '—';
  return text.length > 150 ? `${text.slice(0, 147)}...` : text;
}

function moduleHealth(modules = []) {
  const problem = modules.filter((module) => problemStatuses.has(module.status));
  const panicCount = problem.filter((module) => module.status === 'panic').length;
  const failedCount = problem.filter((module) => module.status === 'failed').length;
  const color = panicCount || failedCount ? 'red' : problem.length ? 'amber' : 'green';
  const label = panicCount
    ? t('system.problem_panic').replace('{count}', String(panicCount))
    : failedCount
      ? t('system.problem_failed').replace('{count}', String(failedCount))
      : problem.length
        ? t('system.problem_restarting').replace('{count}', String(problem.length))
        : t('system.normal');
  return { problem, color, label };
}

function moduleTimingText(module) {
  if (!module) return '';
  const parts = [];
  if (module.status === 'running') {
    const uptime = fmtMillis(module.uptime_millis);
    if (uptime) parts.push(t('system.run_for').replace('{duration}', uptime));
  } else {
    const lastUptime = fmtMillis(module.last_uptime_millis || module.uptime_millis);
    if (lastUptime) parts.push(t('system.last_run').replace('{duration}', lastUptime));
  }
  const backoff = fmtMillis(module.restart_backoff_millis);
  if (backoff) parts.push(`${t('system.backoff')} ${backoff}`);
  return parts.join(' · ');
}

function ModuleStatusLine({ modules = [] }) {
  const health = moduleHealth(modules);
  const recent = health.problem[0] || modules[0];
  const recentDetail = recent && problemStatuses.has(recent.status)
    ? shortEventText({ panic: recent.last_panic, message: recent.last_message })
    : '';
  const timingDetail = moduleTimingText(recent);
  return (
    <div>
      <div style={{ display: 'flex', alignItems: 'center', gap: 8, marginBottom: 8 }}>
        <Tag color={health.color}>{health.label}</Tag>
        <Typography.Text type="tertiary" size="small">{t('system.module_count').replace('{count}', fmtInt(modules.length))}</Typography.Text>
      </div>
      {recent ? (
        <div className="pool-muted" style={{ fontSize: 12.5 }}>
          <span className="pool-mono">{recent.name}</span>
          {' · '}
          <Tag size="small" color={moduleColor[recent.status] || 'grey'}>{recent.status ? t(`system.state.${recent.status}`, recent.status) : t('common.unknown')}</Tag>
          {recentDetail && recentDetail !== '—' ? ` · ${recentDetail}` : ''}
          {timingDetail ? ` · ${timingDetail}` : ''}
        </div>
      ) : (
        <Typography.Text type="tertiary" size="small">{t('system.no_module_status')}</Typography.Text>
      )}
    </div>
  );
}

function SupervisorEventLine({ events = [] }) {
  const event = events[0];
  if (!event) {
    return <Typography.Text type="tertiary" size="small">{t('system.no_supervisor_events')}</Typography.Text>;
  }
  const type = event.type || 'event';
  const text = shortEventText(event);
  return (
    <div className="pool-health-event">
      <div className="pool-health-event-head">
        <Tag size="small" color={eventColor[type] || 'grey'}>{type}</Tag>
        <span className="pool-mono">{event.module || 'background'}</span>
        <span className="pool-muted">{fmtRelative(event.time_unix)}</span>
      </div>
      <div className="pool-health-event-message" title={text}>{text}</div>
    </div>
  );
}

export default function SystemHealthSummary({ system, variant = 'detail', action = null }) {
  if (!system?.supported) return null;

  const cpu = system.cpu || {};
  const mem = system.mem || {};
  const disk = system.disk || {};
  const reg = system.registration || {};
  const go = system.go || {};
  const modules = system.supervisor_modules || [];
  const events = system.supervisor_events || [];
  const compact = variant === 'compact';

  return (
    <Panel
      title={compact ? t('system.health') : t('system.resources_modules')}
      extra={action || (!compact ? <Typography.Text type="tertiary" size="small">{t('system.run_for').replace('{duration}', fmtDuration(system.uptime_seconds))}</Typography.Text> : null)}
    >
      <div className={`pool-grid ${compact ? 'cols-3' : 'cols-2'}`}>
        <div>
          <Meter label={`CPU (${cpu.cores || 0} ${t('system.cores')})`} pct={cpu.usage_pct} color={meterColor(cpu.usage_pct || 0)} right={(cpu.usage_pct ?? 0) + '%'} />
          <Meter label={t('system.memory_usage')} pct={mem.used_pct} color={meterColor(mem.used_pct || 0)} right={`${fmtKB(mem.used_kb)} / ${fmtKB(mem.total_kb)}`} />
          <Meter label={`${t('system.disk_usage')} (${disk.path || '/'})`} pct={disk.used_pct} color={meterColor(disk.used_pct || 0)} right={t('system.available').replace('{size}', fmtBytes(disk.free_bytes))} />
        </div>
        <div>
          <div className="pool-muted" style={{ fontSize: 12.5, marginBottom: 6 }}>{t('system.status')}</div>
          <ModuleStatusLine modules={modules} />
          <div className="pool-muted" style={{ fontSize: 12, marginTop: 8 }}>
            goroutine {fmtInt(go.goroutines)} · Go 内存 {fmtBytes(go.sys_bytes)}
          </div>
          <div style={{ marginTop: 12 }}>
            <div className="pool-muted" style={{ fontSize: 12.5, marginBottom: 6 }}>{t('system.recent_events')}</div>
            <SupervisorEventLine events={events} />
          </div>
        </div>
        {compact ? (
          <div>
            <div className="pool-muted" style={{ fontSize: 12.5, marginBottom: 6 }}>{t('system.registration_memory')}</div>
            <div style={{ fontSize: 24, fontWeight: 700 }}>{fmtKB(reg.total_rss_kb)}</div>
            <div className="pool-muted" style={{ fontSize: 12, marginTop: 4 }}>
              node {reg.node || 0} · chrome {reg.chrome || 0} · Xvfb {reg.xvfb || 0}
            </div>
          </div>
        ) : null}
      </div>
    </Panel>
  );
}
