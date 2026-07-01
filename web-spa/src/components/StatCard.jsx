import React from 'react';

// KPI stat card with optional icon, accent color, and sub-label.
export default function StatCard({ label, value, sub, icon, color = 'var(--semi-color-primary)', tone }) {
  const bg = tone || 'var(--semi-color-primary-light-default)';
  return (
    <div className="pool-stat">
      <span className="accent" style={{ background: color }} />
      <div className="stat-top">
        <span className="label">{label}</span>
        {icon ? <span className="icon" style={{ background: bg, color }}>{icon}</span> : null}
      </div>
      <div className="value">{value}</div>
      {sub != null ? <div className="sub">{sub}</div> : null}
    </div>
  );
}
