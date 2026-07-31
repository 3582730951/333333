import React from 'react';
import { Button, Table } from './pool/index.jsx';
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

function rowKeyOf(row, rowKey, index) {
  if (typeof rowKey === 'function') return rowKey(row, index) ?? row?.id ?? row?.key ?? index;
  if (typeof rowKey === 'string') return row?.[rowKey] ?? row?.id ?? row?.key ?? index;
  return row?.id ?? row?.key ?? index;
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
  loadingTitle = '正在加载数据…',
  scroll,
  mobileColumns,
  mobileRenderer,
  mobileListLabel = '列表',
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
    isMobile && activeScroll === false ? 'pool-table-wrapper--mobile-cards' : '',
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

  if (error && !lastRefresh && !loading) {
    return <LoadErrorBanner error={error} onRetry={onRetry} title={errorTitle} />;
  }

  if (isMobile && typeof mobileRenderer === 'function') {
    const pageSize = pagination && pagination !== false ? Number(pagination.pageSize) || rows.length || 1 : rows.length || 1;
    const total = pagination && pagination !== false ? Number(pagination.total) || rows.length : rows.length;
    const currentPage = Number(pagination?.currentPage || 1);
    const localPage = pagination && pagination !== false && !pagination.total;
    const visibleRows = localPage ? rows.slice((currentPage - 1) * pageSize, currentPage * pageSize) : rows;
    const pageCount = Math.max(1, Math.ceil(total / pageSize));
    const selected = new Set(tableProps.rowSelection?.selectedRowKeys || []);
    const toggleSelected = (key, checked) => {
      const next = new Set(selected);
      if (checked) next.add(key);
      else next.delete(key);
      tableProps.rowSelection?.onChange?.([...next]);
    };
    return (
      <>
        <LoadErrorBanner error={error} onRetry={onRetry} title={lastRefresh ? (errorTitle || '刷新失败，正在显示上次数据') : errorTitle} />
        {firstLoad ? (
          <TableSkeleton rows={skeletonRows} cols={1} title={loadingTitle} />
        ) : visibleRows.length ? (
          <div
            className={['pool-mobile-list', className].filter(Boolean).join(' ')}
            role="list"
            aria-label={mobileListLabel}
          >
            {visibleRows.map((row, index) => {
              const key = rowKeyOf(row, rowKey, index);
              const isSelected = selected.has(key);
              return (
                <div key={String(key)} className="pool-mobile-list__item" role="listitem">
                  {mobileRenderer(row, {
                    index,
                    key,
                    selected: isSelected,
                    toggleSelected: (checked = !isSelected) => toggleSelected(key, checked),
                  })}
                </div>
              );
            })}
            {pagination && pagination !== false && total > pageSize ? (
              <div className="pool-mobile-list__pager">
                <span>{currentPage} / {pageCount}</span>
                <span className="pool-inline">
                  <Button size="small" onClick={() => pagination?.onPageChange?.(Math.max(1, currentPage - 1))} disabled={currentPage <= 1}>上一页</Button>
                  <Button size="small" onClick={() => pagination?.onPageChange?.(Math.min(pageCount, currentPage + 1))} disabled={currentPage >= pageCount}>下一页</Button>
                </span>
              </div>
            ) : null}
          </div>
        ) : (
          <div className="pool-table-empty">{emptyNode}</div>
        )}
      </>
    );
  }

  return (
    <>
      <LoadErrorBanner error={error} onRetry={onRetry} title={lastRefresh ? (errorTitle || '刷新失败，正在显示上次数据') : errorTitle} />
      {firstLoad ? (
        <TableSkeleton rows={skeletonRows} cols={skeletonCols || Math.max(1, activeColumns?.length || 5)} title={loadingTitle} />
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
