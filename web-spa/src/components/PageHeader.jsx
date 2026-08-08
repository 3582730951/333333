import React from 'react';
import { Typography } from './pool/index.jsx';

export default function PageHeader({ title, subtitle, actions }) {
  return (
    <header className="pool-pagehead">
      <div className="pool-pagehead-copy">
        <Typography.Title heading={1} className="pool-page-title" tabIndex={-1}>{title}</Typography.Title>
        {subtitle ? <div className="sub">{subtitle}</div> : null}
      </div>
      {actions ? <div className="actions pool-page-actions">{actions}</div> : null}
    </header>
  );
}

export function Panel({ title, extra, children, style }) {
  return (
    <div className="pool-panel" style={style}>
      {(title || extra) && (
        <div className="pool-panel-head">
          <div className="pool-section-title">{title}</div>
          {extra}
        </div>
      )}
      {children}
    </div>
  );
}

export function Meter({ label, pct, color = 'var(--pool-accent)', right }) {
  const p = Math.max(0, Math.min(100, Number(pct) || 0));
  return (
    <div style={{ marginBottom: 12 }}>
      <div style={{ display: 'flex', justifyContent: 'space-between', fontSize: 12.5, marginBottom: 6 }}>
        <span className="pool-muted">{label}</span>
        <span style={{ fontWeight: 600 }}>{right != null ? right : p + '%'}</span>
      </div>
      <div className="pool-meter"><span style={{ width: p + '%', background: color }} /></div>
    </div>
  );
}
