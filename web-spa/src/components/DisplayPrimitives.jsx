import React from 'react';
import { Space, Tag, Tooltip } from './pool/index.jsx';

function textTitle(value) {
  if (typeof value === 'string' || typeof value === 'number') return String(value);
  return undefined;
}

/**
 * Truncates to the real edge of its box rather than at a character count, so a value is
 * only ever abbreviated when it genuinely does not fit.
 *
 * The annotations are what let .tsx callers use this without an `as any` alias: inferred
 * from the destructuring alone, every prop without a default would come out required.
 *
 * @param {object} props
 * @param {React.ReactNode} [props.children]
 * @param {number} [props.lines]
 * @param {string} [props.className]
 * @param {number|string} [props.maxWidth]
 * @param {string} [props.title]
 * @param {string} [props.ariaLabel]
 * @param {boolean} [props.strong]
 * @param {boolean} [props.muted]
 * @param {(event: React.MouseEvent) => void} [props.onClick]
 */
export function TextClamp({
  children,
  lines = 1,
  className = '',
  maxWidth,
  title,
  ariaLabel,
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
      aria-label={ariaLabel}
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

// A rail of counts is the weakest thing a console can show: four numbers with no
// sense of scale. `share` (0..1) turns an entry into a proportional track, so a rail
// whose entries are parts of a whole reads as a small chart. Entries without a share
// stay plain numbers — a total has nothing to be a fraction of.
export function MetricRail({ items = [], className = '', label = '指标摘要' }) {
  const list = Array.isArray(items) ? items.filter(Boolean) : [];
  if (!list.length) return null;
  return (
    <div className={['pool-metric-rail', className].filter(Boolean).join(' ')} role="complementary" aria-label={label}>
      {list.map((item) => {
        const share = Number(item.share);
        const hasShare = Number.isFinite(share);
        const pct = hasShare ? Math.max(0, Math.min(100, Math.round(share * 100))) : 0;
        return (
          <dl
            key={item.key || item.label}
            className={[
              'pool-metric-card',
              item.tone ? `pool-metric-card--${item.tone}` : '',
              hasShare ? 'pool-metric-card--tracked' : '',
            ].filter(Boolean).join(' ')}
          >
            <dt className="pool-metric-card__label">{item.label}</dt>
            <dd className="pool-metric-card__value"><strong>{item.value}</strong></dd>
            {hasShare ? (
              <dd className="pool-metric-card__track">
                <span style={{ width: `${pct}%` }} />
              </dd>
            ) : null}
            {item.detail ? <dd className="pool-metric-card__detail">{item.detail}</dd> : null}
          </dl>
        );
      })}
    </div>
  );
}

export function TinyMeter({ value, max = 100, label, tone = 'accent' }) {
  const n = Number(value);
  const m = Number(max) || 100;
  const pct = Number.isFinite(n) && m > 0 ? Math.max(0, Math.min(100, Math.round((n / m) * 100))) : 0;
  return (
    <div
      className={`pool-tiny-meter pool-tiny-meter--${tone}`}
      title={label || `${pct}%`}
      role="progressbar"
      aria-label={label || `${pct}%`}
      aria-valuemin={0}
      aria-valuemax={100}
      aria-valuenow={pct}
    >
      <span className="pool-tiny-meter__bar" style={{ width: `${pct}%` }} />
    </div>
  );
}
