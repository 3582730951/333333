import React from 'react';
import { Tag, Typography } from './pool/index.jsx';
import { Panel, Meter } from './PageHeader.jsx';
import { COLORS } from '../lib/chartTheme.js';
import { fmtBytes, fmtDuration, fmtInt, fmtKB, fmtRelative } from '../lib/format.js';

const C = COLORS;
const moduleColor = { running: 'green', restarting: 'amber', panic: 'red', failed: 'red', stopped: 'grey' };
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
  const label = panicCount ? `${panicCount} 个 panic` : failedCount ? `${failedCount} 个失败` : problem.length ? `${problem.length} 个重启中` : '正常';
  return { problem, color, label };
}

function moduleTimingText(module) {
  if (!module) return '';
  const parts = [];
  if (module.status === 'running') {
    const uptime = fmtMillis(module.uptime_millis);
    if (uptime) parts.push(`运行 ${uptime}`);
  } else {
    const lastUptime = fmtMillis(module.last_uptime_millis || module.uptime_millis);
    if (lastUptime) parts.push(`上次运行 ${lastUptime}`);
  }
  const backoff = fmtMillis(module.restart_backoff_millis);
  if (backoff) parts.push(`重试等待 ${backoff}`);
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
        <Typography.Text type="tertiary" size="small">{fmtInt(modules.length)} 个模块</Typography.Text>
      </div>
      {recent ? (
        <div className="pool-muted" style={{ fontSize: 12.5 }}>
          <span className="pool-mono">{recent.name}</span>
          {' · '}
          <Tag size="small" color={moduleColor[recent.status] || 'grey'}>{recent.status || 'unknown'}</Tag>
          {recentDetail && recentDetail !== '—' ? ` · ${recentDetail}` : ''}
          {timingDetail ? ` · ${timingDetail}` : ''}
        </div>
      ) : (
        <Typography.Text type="tertiary" size="small">暂无 supervisor 模块状态。</Typography.Text>
      )}
    </div>
  );
}

function SupervisorEventLine({ events = [] }) {
  const event = events[0];
  if (!event) {
    return <Typography.Text type="tertiary" size="small">暂无 supervisor 事件。</Typography.Text>;
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
      title={compact ? '系统健康' : '资源与模块状态'}
      extra={action || (!compact ? <Typography.Text type="tertiary" size="small">运行 {fmtDuration(system.uptime_seconds)}</Typography.Text> : null)}
    >
      <div className={`pool-grid ${compact ? 'cols-3' : 'cols-2'}`}>
        <div>
          <Meter label={`CPU（${cpu.cores || 0} 核）`} pct={cpu.usage_pct} color={meterColor(cpu.usage_pct || 0)} right={(cpu.usage_pct ?? 0) + '%'} />
          <Meter label="内存" pct={mem.used_pct} color={meterColor(mem.used_pct || 0)} right={`${fmtKB(mem.used_kb)} / ${fmtKB(mem.total_kb)}`} />
          <Meter label={`磁盘（${disk.path || '/'}）`} pct={disk.used_pct} color={meterColor(disk.used_pct || 0)} right={`${fmtBytes(disk.free_bytes)} 可用`} />
        </div>
        <div>
          <div className="pool-muted" style={{ fontSize: 12.5, marginBottom: 6 }}>模块状态</div>
          <ModuleStatusLine modules={modules} />
          <div className="pool-muted" style={{ fontSize: 12, marginTop: 8 }}>
            goroutine {fmtInt(go.goroutines)} · Go 内存 {fmtBytes(go.sys_bytes)}
          </div>
          <div style={{ marginTop: 12 }}>
            <div className="pool-muted" style={{ fontSize: 12.5, marginBottom: 6 }}>最近事件</div>
            <SupervisorEventLine events={events} />
          </div>
        </div>
        {compact ? (
          <div>
            <div className="pool-muted" style={{ fontSize: 12.5, marginBottom: 6 }}>注册任务内存</div>
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
