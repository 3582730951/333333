import React from 'react';

import LoadErrorBanner from './LoadErrorBanner.jsx';
import { Banner, Drawer, Tag, Typography } from './pool/index.jsx';
import { ProgressBar } from './pool/Progress.jsx';
import { TextClamp } from './DisplayPrimitives.jsx';
import { fmtDateTime } from '../lib/format.js';
import { t } from '../lib/i18n.js';

function numberOf(value) {
  const next = Number(value);
  return Number.isFinite(next) ? next : 0;
}

function clampPercent(value) {
  return Math.max(0, Math.min(100, Math.round(value)));
}

function taskValue(task, keys, fallback = 0) {
  const list = Array.isArray(keys) ? keys : [keys];
  for (const key of list) {
    if (task?.[key] !== undefined && task?.[key] !== null) return numberOf(task[key]);
  }
  return fallback;
}

export function TaskProgress({
  task,
  totalKey = ['total', 'target_count'],
  completedKey = ['completed', 'completed_count'],
  successKey = ['succeeded', 'success_count'],
  failedKey = ['failed', 'failed_count'],
  className = '',
}) {
  const success = Math.max(0, taskValue(task, successKey));
  const failed = Math.max(0, taskValue(task, failedKey));
  const completed = Math.max(0, taskValue(task, completedKey, success + failed));
  const total = Math.max(0, taskValue(task, totalKey));
  const done = total > 0 ? Math.min(total, Math.max(completed, success + failed)) : Math.max(completed, success + failed);
  const pct = total > 0 ? clampPercent((done / total) * 100) : 0;
  const denominator = total || Math.max(done, 1);

  return (
    <div className={['pool-task-progress', className].filter(Boolean).join(' ')}>
      <div className="pool-task-progress__stats">
        <b>{pct}%</b>
        <span className="pool-success-text">{t('workflow.success')} {success}</span>
        <span className="pool-danger-text">{t('workflow.failed')} {failed}</span>
        <span>{t('workflow.completed')} {done}/{denominator}</span>
      </div>
      <ProgressBar percent={pct} label={`${t('workflow.completed')} ${done}/${denominator}`} />
    </div>
  );
}

export function ReadinessPanel({
  readiness,
  readinessError,
  blockers = [],
  providerSummary = [],
  pool = {},
}) {
  if (readinessError) {
    return (
      <Banner
        className="pool-readiness-panel"
        type="danger"
        title={t('workflow.readiness_failed')}
        description={readinessError}
        closeIcon={null}
      />
    );
  }

  if (!readiness) {
    return (
      <Banner
        className="pool-readiness-panel"
        type="info"
        title={t('workflow.readiness_loading')}
        description={t('workflow.readiness_wait')}
        closeIcon={null}
      />
    );
  }

  const blocked = blockers.length > 0;
  return (
    <Banner
      className="pool-readiness-panel"
      type={blocked ? 'warning' : 'success'}
      title={blocked ? t('workflow.readiness_blocked') : t('workflow.readiness_ready')}
      description={(
        <div className="pool-readiness-panel__body">
          <div>
            {blocked
              ? blockers.join('; ')
              : readiness.policy_error
                ? t('workflow.policy_warning')
                : t('workflow.dependencies_ready')}
          </div>
          <div className="pool-readiness-panel__tags">
            {providerSummary.map(([key, value]) => (
              <Tag key={key} color={Number(value) > 0 ? 'green' : 'grey'}>{key}: {Number(value) || 0}</Tag>
            ))}
            <Tag color={(numberOf(pool.deficit) || 0) > 0 ? 'amber' : 'green'}>{t('workflow.deficit')}: {numberOf(pool.deficit) || 0}</Tag>
            {readiness.policy_error ? <Tag color="orange">{t('workflow.automation_abnormal')}</Tag> : null}
          </div>
        </div>
      )}
      closeIcon={null}
    />
  );
}

export function ServiceHealthStrip({ services = [], renderStatus }) {
  const list = Array.isArray(services) ? services.filter(Boolean) : [];
  if (!list.length) return null;
  return (
    <section className="pool-service-health-strip" aria-label={t('workflow.services')}>
      <Typography.Text strong>{t('workflow.services')}</Typography.Text>
      <div className="pool-service-health-strip__items">
        {list.map((rawService, index) => {
          const service = rawService && typeof rawService === 'object'
            ? rawService
            : { name: String(rawService || `service-${index + 1}`), status: rawService };
          const identity = service.name || service.service || service.id || `service-${index + 1}`;
          return (
            <div key={`${identity}:${index}`} className="pool-service-health-strip__item">
              <span>{identity}</span>
              {renderStatus ? renderStatus(service.status) : <Tag>{service.status || 'unknown'}</Tag>}
              {service.last_error ? <Typography.Text type="danger" size="small">{service.last_error}</Typography.Text> : null}
            </div>
          );
        })}
      </div>
    </section>
  );
}

export function LogStream({ logs = [], streaming = false, error, onRetry }) {
  const list = Array.isArray(logs) ? logs : [];
  return (
    <div className="pool-log-stream">
      <LoadErrorBanner error={error} onRetry={onRetry} title={t('workflow.log_failed')} />
      <div className="pool-log-stream__state">
        <Tag color={streaming ? 'green' : 'grey'}>{streaming ? t('workflow.streaming') : t('workflow.disconnected')}</Tag>
      </div>
      {!list.length ? <Typography.Text type="tertiary">{t('workflow.no_logs')}</Typography.Text> : null}
      <div className="pool-log-stream__lines pool-mono">
        {list.map((entry, index) => {
          const key = entry.id || `${entry.timestamp || 0}:${entry.level || ''}:${index}`;
          return (
            <div key={key} className="pool-log-stream__line">
              <span className="pool-muted">[{fmtDateTime(entry.timestamp)}]</span>
              <Tag size="small" color={entry.level === 'error' ? 'red' : 'grey'}>{entry.level || 'info'}</Tag>
              <span>{entry.message}</span>
            </div>
          );
        })}
      </div>
    </div>
  );
}

export function TaskDetailDrawer({
  task,
  visible,
  open,
  onClose,
  title,
  status,
  children,
  logs,
  logError,
  logStreaming,
  onRetryLogs,
  width = 620,
}) {
  const active = open ?? visible ?? !!task;
  // started_at / completed_at are 0 until the task reaches each state, so they are read through
  // a guard rather than handed to fmtDateTime -- otherwise an unstarted task reports 1970.
  const stamp = (value) => (Number(value) > 0 ? fmtDateTime(value) : undefined);
  const details = task && typeof task === 'object' ? [
    [t('workflow.task_id'), task.id],
    [t('workflow.type'), task.task_type || task.method],
    [t('workflow.platform'), task.platform],
    [t('workflow.group'), task.group_name],
    [t('workflow.egress'), task.egress_id || task.registration_egress_pool_id],
    [t('workflow.target'), task.total ?? task.target_count],
    [t('workflow.success'), task.succeeded ?? task.success_count],
    [t('workflow.failed'), task.failed ?? task.failed_count],
    [t('workflow.created_at'), fmtDateTime(task.created_at)],
    [t('workflow.started_at'), stamp(task.started_at)],
    [t('workflow.completed_at'), stamp(task.completed_at)],
    [t('common.updated_at'), fmtDateTime(task.updated_at)],
  ].filter(([, value]) => value !== undefined && value !== null && value !== '') : [];
  // The reason a task failed was the one field the payload carried that this drawer dropped, so a
  // failed job offered a red tag and nothing else, in the table or here. It sits outside the
  // definition grid because it is prose, not a value, and wraps rather than being clamped.
  const failureReason = task && typeof task === 'object' ? String(task.error || task.last_error || '') : '';

  return (
    <Drawer
      visible={active}
      onCancel={onClose}
      title={title || (task ? `${t('workflow.task_title')} · ${task.id || task.task_type || 'task'}` : t('workflow.task_title'))}
      width={width}
    >
      {task ? (
        <div className="pool-task-detail">
          <div className="pool-task-detail__head">
            <TextClamp strong>{task.id || task.task_type || task.method || 'task'}</TextClamp>
            {status}
          </div>
          <TaskProgress task={task} />
          <dl className="pool-task-detail__grid">
            {details.map(([label, value]) => (
              <div key={label}>
                <dt>{label}</dt>
                <dd>{value}</dd>
              </div>
            ))}
          </dl>
          {failureReason ? (
            <div className="pool-task-detail__failure">
              <span className="pool-task-detail__failure-label">{t('workflow.failure_reason')}</span>
              <p>{failureReason}</p>
            </div>
          ) : null}
          {children}
          {logs !== undefined || logError ? (
            <LogStream logs={logs} streaming={logStreaming} error={logError} onRetry={onRetryLogs} />
          ) : null}
        </div>
      ) : null}
    </Drawer>
  );
}
