import React from 'react';
import { Table } from '@douyinfe/semi-ui';
import EmptyState from './EmptyState.jsx';
import LoadErrorBanner from './LoadErrorBanner.jsx';
import { TableSkeleton } from './Skeleton.jsx';
import useResponsiveLayout from '../hooks/useResponsiveLayout.js';

function flattenColumns(columns = []) {
  return columns.flatMap((column) => (Array.isArray(column.children) ? flattenColumns(column.children) : column));
}

function numericWidth(width) {
  if (typeof width === 'number' && Number.isFinite(width)) return width;
  if (typeof width === 'string' && /^\d+$/.test(width.trim())) return Number(width.trim());
  return 0;
}

function defaultTableScrollX(columns, { minScrollX = 720, safeActionWidth = 128 } = {}) {
  const leafColumns = flattenColumns(Array.isArray(columns) ? columns : []);
  const declaredWidth = leafColumns.reduce((sum, column) => {
    const declared = numericWidth(column.width);
    const key = String(column.key || column.dataIndex || column.title || '').toLowerCase();
    const isActionColumn = key.includes('op') || key.includes('action') || key.includes('操作');
    return sum + (declared || (isActionColumn ? safeActionWidth : 144));
  }, 0);
  const estimatedWidth = Math.max(minScrollX, leafColumns.length * 144);
  return Math.max(declaredWidth, estimatedWidth);
}

const densityRowHeights = {
  compact: 56,
  default: 64,
  regular: 64,
  account: 72,
};

export default function ResourceTable({
  error,
  errorTitle,
  onRetry,
  loading,
  lastRefresh,
  dataSource,
  columns,
  rowKey,
  pagination,
  empty,
  emptyTitle = '暂无数据',
  emptyDesc,
  emptyType = 'default',
  emptyAction,
  skeletonRows = 6,
  skeletonCols,
  scroll,
  mobileColumns,
  mobileScroll,
  density = 'default',
  layout = 'fluid',
  minScrollX,
  safeActionWidth,
  rowHeight,
  className,
  style,
  ...tableProps
}) {
  const { isMobile } = useResponsiveLayout();
  const activeColumns = isMobile && Array.isArray(mobileColumns) && mobileColumns.length > 0 ? mobileColumns : columns;
  const activeScroll = isMobile && mobileScroll !== undefined ? mobileScroll : scroll;
  const rows = Array.isArray(dataSource) ? dataSource : [];
  const firstLoad = !lastRefresh && loading;
  const contentWidth = defaultTableScrollX(activeColumns, { minScrollX, safeActionWidth });
  const explicitScrollX = activeScroll && activeScroll !== false ? numericWidth(activeScroll.x) : 0;
  const fitWidth = Math.max(contentWidth, explicitScrollX);
  const resolvedScroll = activeScroll === false ? undefined : {
    x: fitWidth,
    ...(activeScroll || {}),
  };
  const resolvedClassName = [
    'pool-table-wrapper',
    density ? `pool-table-density-${density}` : '',
    layout ? `pool-table-layout-${layout}` : '',
    className,
  ].filter(Boolean).join(' ');
  const resolvedRowHeight = rowHeight || densityRowHeights[density] || densityRowHeights.regular;
  const tableStyle = {
    '--pool-table-row-height': `${resolvedRowHeight}px`,
    '--pool-table-fit-width': `${fitWidth}px`,
    ...(style || {}),
  };
  const emptyNode = empty || (
    <EmptyState
      title={emptyTitle}
      desc={emptyDesc}
      type={emptyType}
      action={emptyAction}
    />
  );

  return (
    <>
      <LoadErrorBanner error={error} onRetry={onRetry} title={errorTitle} />
      {firstLoad ? (
        <TableSkeleton rows={skeletonRows} cols={skeletonCols || Math.max(1, activeColumns?.length || 5)} />
      ) : (
        <Table
          loading={loading}
          dataSource={rows}
          columns={activeColumns}
          rowKey={rowKey}
          pagination={pagination}
          empty={emptyNode}
          scroll={resolvedScroll}
          className={resolvedClassName}
          style={tableStyle}
          {...tableProps}
        />
      )}
    </>
  );
}
