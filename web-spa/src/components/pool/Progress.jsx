import React from 'react';

function toneClass(tone) {
  return tone ? `pool-status-pill--${tone}` : '';
}

export function ProgressBar({ value, percent, label, className, showInfo, format }) {
  const pct = Math.max(0, Math.min(100, Number(percent ?? value) || 0));
  const text = format ? format(pct) : `${pct}%`;
  return (
    <div className={className}>
      <div className="pool-progress" role="progressbar" aria-valuenow={pct} aria-valuemin={0} aria-valuemax={100} aria-label={label}>
        <div className="pool-progress__bar" style={{ '--pool-progress-value': `${pct}%` }} />
      </div>
      {showInfo ? <div className="pool-field__help">{text}</div> : null}
    </div>
  );
}

export function StatusDot({ tone = 'info', className }) {
  return <span className={`pool-status-dot ${toneClass(tone)} ${className || ''}`} aria-hidden="true" />;
}

export function StatusPill({ tone = 'info', children, className }) {
  return <span className={`pool-status-pill ${toneClass(tone)} ${className || ''}`}><StatusDot tone={tone} />{children}</span>;
}

export const Progress = ProgressBar;
