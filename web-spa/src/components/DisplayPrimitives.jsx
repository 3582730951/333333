import React from 'react';
import { Space, Tag, Tooltip } from './pool/index.jsx';

function textTitle(value) {
  if (typeof value === 'string' || typeof value === 'number') return String(value);
  return undefined;
}

export function TextClamp({
  children,
  lines = 1,
  className = '',
  maxWidth,
  title,
  strong = false,
  muted = false,
  onClick,
}) {
  const content = children ?? '—';
  const node = (
    <span
      className={[
        'pool-text-clamp',
        lines > 1 ? 'pool-text-clamp--multi' : '',
        strong ? 'pool-text-clamp--strong' : '',
        muted ? 'pool-text-clamp--muted' : '',
        onClick ? 'pool-text-clamp--link' : '',
        className,
      ].filter(Boolean).join(' ')}
      style={{ '--pool-clamp-lines': lines, maxWidth }}
      onClick={onClick}
    >
      {content}
    </span>
  );
  const tooltipTitle = title ?? textTitle(content);
  return tooltipTitle ? <Tooltip content={tooltipTitle}>{node}</Tooltip> : node;
}

export function TagList({
  items = [],
  max = 3,
  empty = '—',
  color,
  size = 'small',
  renderItem,
}) {
  const list = Array.isArray(items) ? items.filter((item) => item !== null && item !== undefined && item !== '') : [];
  if (!list.length) return empty;
  const visible = list.slice(0, max);
  return (
    <Space className="pool-tag-list" spacing={4} wrap>
      {visible.map((item, index) => (
        renderItem
          ? renderItem(item, index)
          : <Tag key={String(item)} size={size} color={color}>{String(item)}</Tag>
      ))}
      {list.length > max ? <Tag size={size}>+{list.length - max}</Tag> : null}
    </Space>
  );
}

export function ActionGroup({ children, className = '', minWidth, compact = false }) {
  return (
    <div
      className={['pool-row-actions', compact ? 'pool-row-actions--compact' : '', className].filter(Boolean).join(' ')}
      style={minWidth ? { minWidth } : undefined}
    >
      {children}
    </div>
  );
}

export function MetricRail({ items = [], className = '' }) {
  const list = Array.isArray(items) ? items.filter(Boolean) : [];
  if (!list.length) return null;
  return (
    <aside className={['pool-metric-rail', className].filter(Boolean).join(' ')}>
      {list.map((item) => (
        <div key={item.key || item.label} className={['pool-metric-card', item.tone ? `pool-metric-card--${item.tone}` : ''].filter(Boolean).join(' ')}>
          <span className="pool-metric-card__label">{item.label}</span>
          <strong>{item.value}</strong>
          {item.detail ? <span className="pool-metric-card__detail">{item.detail}</span> : null}
        </div>
      ))}
    </aside>
  );
}

export function TinyMeter({ value, max = 100, label }) {
  const n = Number(value);
  const m = Number(max) || 100;
  const pct = Number.isFinite(n) && m > 0 ? Math.max(0, Math.min(100, Math.round((n / m) * 100))) : 0;
  return (
    <div className="pool-tiny-meter" title={label || `${pct}%`}>
      <span style={{ width: `${pct}%` }} />
    </div>
  );
}
