import React from 'react';

import LoadErrorBanner from './LoadErrorBanner.jsx';
import { Banner, Drawer, Tag, Typography } from './pool/index.jsx';
import { ProgressBar } from './pool/Progress.jsx';
import { TextClamp } from './DisplayPrimitives.jsx';
import { fmtDateTime } from '../lib/format.js';

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
        <span className="pool-success-text">成功 {success}</span>
        <span className="pool-danger-text">失败 {failed}</span>
        <span>完成 {done}/{denominator}</span>
      </div>
      <ProgressBar percent={pct} label={`完成 ${done}/${denominator}`} />
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
        title="依赖检查失败"
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
        title="正在读取依赖状态"
        description="请稍候，注册依赖状态读取完成后即可启动任务。"
        closeIcon={null}
      />
    );
  }

  const blocked = blockers.length > 0;
  return (
    <Banner
      className="pool-readiness-panel"
      type={blocked ? 'warning' : 'success'}
      title={blocked ? '启动前检查未通过' : '启动前检查通过'}
      description={(
        <div className="pool-readiness-panel__body">
          <div>
            {blocked
              ? blockers.join('; ')
              : readiness.policy_error
                ? '当前身份模式所需依赖可用；自动化策略需要处理。'
                : '当前身份模式所需依赖可用。'}
          </div>
          <div className="pool-readiness-panel__tags">
            {providerSummary.map(([key, value]) => (
              <Tag key={key} color={Number(value) > 0 ? 'green' : 'grey'}>{key}: {Number(value) || 0}</Tag>
            ))}
            <Tag color={(numberOf(pool.deficit) || 0) > 0 ? 'amber' : 'green'}>缺口: {numberOf(pool.deficit) || 0}</Tag>
            {readiness.policy_error ? <Tag color="orange">automation: 异常</Tag> : null}
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
    <section className="pool-service-health-strip" aria-label="外部服务状态">
      <Typography.Text strong>外部服务</Typography.Text>
      <div className="pool-service-health-strip__items">
        {list.map((service) => (
          <div key={service.name || service.service || service.id} className="pool-service-health-strip__item">
            <span>{service.name || service.service || service.id || 'service'}</span>
            {renderStatus ? renderStatus(service.status) : <Tag>{service.status || 'unknown'}</Tag>}
            {service.last_error ? <Typography.Text type="danger" size="small">{service.last_error}</Typography.Text> : null}
          </div>
        ))}
      </div>
    </section>
  );
}

export function LogStream({ logs = [], streaming = false, error, onRetry }) {
  const list = Array.isArray(logs) ? logs : [];
  return (
    <div className="pool-log-stream">
      <LoadErrorBanner error={error} onRetry={onRetry} title="任务日志读取失败" />
      <div className="pool-log-stream__state">
        <Tag color={streaming ? 'green' : 'grey'}>{streaming ? '实时连接中' : '实时连接断开'}</Tag>
      </div>
      {!list.length ? <Typography.Text type="tertiary">暂无日志</Typography.Text> : null}
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
  const details = task && typeof task === 'object' ? [
    ['ID', task.id],
    ['类型', task.task_type || task.method],
    ['平台', task.platform],
    ['分组', task.group_name],
    ['出口', task.egress_id || task.registration_egress_pool_id],
    ['目标', task.total ?? task.target_count],
    ['成功', task.succeeded ?? task.success_count],
    ['失败', task.failed ?? task.failed_count],
    ['创建时间', fmtDateTime(task.created_at)],
    ['更新时间', fmtDateTime(task.updated_at)],
  ].filter(([, value]) => value !== undefined && value !== null && value !== '') : [];

  return (
    <Drawer
      visible={active}
      onCancel={onClose}
      title={title || (task ? `任务详情 · ${task.id || task.task_type || 'task'}` : '任务详情')}
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
          {children}
          {logs !== undefined || logError ? (
            <LogStream logs={logs} streaming={logStreaming} error={logError} onRetry={onRetryLogs} />
          ) : null}
        </div>
      ) : null}
    </Drawer>
  );
}
