import React from 'react';

// Shimmer skeleton rows for first-load on heavier tables.
export function TableSkeleton({ rows = 6, cols = 5, title = '正在加载数据…' }) {
  return (
    <div className="pool-skeleton-table" aria-label="正在加载数据">
      <div className="pool-skeleton-title">{title}</div>
      {Array.from({ length: rows }).map((_, r) => (
        <div key={r} style={{ display: 'flex', gap: 16, padding: '14px', borderBottom: r === rows - 1 ? 0 : '1px solid var(--pool-border)' }}>
          {Array.from({ length: cols }).map((_, c) => (
            <div key={c} className="pool-skel" style={{ height: 14, borderRadius: 6, flex: c === 0 ? 2.2 : 1 }} />
          ))}
        </div>
      ))}
    </div>
  );
}
