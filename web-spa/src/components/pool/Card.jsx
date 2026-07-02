import React from 'react';

function cx(...parts) {
  return parts.filter(Boolean).join(' ');
}

export function Card({ children, className, title, headerExtraContent, bodyStyle, ...props }) {
  return (
    <section className={cx('pool-card', className)} {...props}>
      {title || headerExtraContent ? (
        <div className="pool-card-head">
          <div className="pool-text-strong">{title}</div>
          {headerExtraContent}
        </div>
      ) : null}
      <div className="pool-card-body" style={bodyStyle}>{children}</div>
    </section>
  );
}

export function DataCard({ children, className, ...props }) {
  return <section className={cx('pool-data-card', className)} {...props}>{children}</section>;
}

export function MetricCard({ label, value, sub, tone, className, ...props }) {
  return (
    <section className={cx('pool-metric-card', tone ? `pool-metric-card--${tone}` : '', className)} {...props}>
      <div className="pool-resource-summary__meta">{label}</div>
      <div className="pool-page-title">{value}</div>
      {sub ? <div className="pool-resource-summary__meta">{sub}</div> : null}
    </section>
  );
}

export default Card;
