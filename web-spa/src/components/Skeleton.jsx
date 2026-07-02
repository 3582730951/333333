import React from 'react';

// Shimmer skeleton rows for first-load on heavier tables.
export function TableSkeleton({ rows = 6, cols = 5 }) {
  return (
    <div style={{ padding: '6px 2px' }}>
      {Array.from({ length: rows }).map((_, r) => (
        <div key={r} style={{ display: 'flex', gap: 16, padding: '11px 4px', borderBottom: '1px solid var(--pool-border)' }}>
          {Array.from({ length: cols }).map((_, c) => (
            <div key={c} className="pool-skel" style={{ height: 14, borderRadius: 6, flex: c === 0 ? 2.2 : 1 }} />
          ))}
        </div>
      ))}
    </div>
  );
}
