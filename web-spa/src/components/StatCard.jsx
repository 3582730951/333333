import React from 'react';

// KPI stat card with optional icon, accent color, and sub-label.
export default function StatCard({ label, value, sub, icon, color = 'var(--pool-accent)', tone }) {
  const bg = tone || 'var(--pool-accent-soft)';
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
