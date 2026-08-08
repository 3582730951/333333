import React, { useCallback, useEffect, useRef, useState } from 'react';
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

function columnKeyOf(column, index) {
  return String(column.key || column.dataIndex || column.title || index);
}

function isActionColumn(column) {
  const key = String(column.key || column.dataIndex || '').toLowerCase();
  const title = typeof column.title === 'string' ? column.title : '';
  return key.includes('action') || key === 'op' || key.includes('ops') || title.includes('操作');
}

// Widths of the chrome columns the table adds around the caller's own columns.
const EXPANDER_COLUMN_WIDTH = 40;
const SELECTION_COLUMN_WIDTH = 44;
const WRAPPER_BORDER_WIDTH = 2;

// Columns are dropped from the widest table until the rest fit the pane. Later
// columns go first, because table layouts put identity and status up front and
// diagnostics at the end. `priority` overrides this: lower keeps a column longer,
// and the identity column plus any action column are always pinned.
//
// The fit test uses the very formula that later sets --pool-table-fit-width, so
// folding cannot disagree with the width the table actually renders at. That
// formula has a per-column floor, which means dropping is sometimes driven by
// column count rather than by declared widths.
function foldColumnsToWidth(columns, available, fitOptions, reserved = 0) {
  const leaf = flattenColumns(Array.isArray(columns) ? columns : []);
  if (!available || leaf.length <= 1) return { visible: leaf, hidden: [] };
  if (defaultTableScrollX(leaf, fitOptions) <= available - reserved) return { visible: leaf, hidden: [] };
  // A page-supplied minScrollX is a floor for the column set that page declared.
  // Once columns fold away it describes a table that is no longer on screen, so the
  // per-column floor takes over — otherwise a stale number blocks folding outright.
  const foldedFit = { ...fitOptions, minScrollX: 0 };
  const fits = (candidates, budget) => budget > 0 && defaultTableScrollX(candidates, foldedFit) <= budget;

  // Folding pays for itself only if the row expander it introduces still leaves
  // room for the survivors.
  const budget = available - reserved - EXPANDER_COLUMN_WIDTH;
  const pinned = new Set();
  leaf.forEach((column, index) => {
    if (index === 0 || isActionColumn(column)) pinned.add(columnKeyOf(column, index));
  });
  const isPinned = (column, index) => pinned.has(columnKeyOf(column, index));
  // A pane too narrow even for the pinned columns keeps its scrollbar whatever we
  // drop, so dropping would only cost information.
  if (!fits(leaf.filter(isPinned), budget)) return { visible: leaf, hidden: [] };

  const order = leaf
    .map((column, index) => ({ column, index, key: columnKeyOf(column, index) }))
    // Drop the lowest-priority (highest number), right-most column first.
    .filter((item) => !pinned.has(item.key))
    .sort((a, b) => (b.column.priority ?? 50) - (a.column.priority ?? 50) || b.index - a.index);

  const dropped = new Set();
  const survivors = () => leaf.filter((column, index) => !dropped.has(columnKeyOf(column, index)));
  for (const item of order) {
    if (fits(survivors(), budget)) break;
    dropped.add(item.key);
  }
  if (!dropped.size) return { visible: leaf, hidden: [] };
  const visible = [];
  const hidden = [];
  leaf.forEach((column, index) => {
    (dropped.has(columnKeyOf(column, index)) ? hidden : visible).push(column);
  });
  return { visible, hidden };
}

// Measures the pane the table renders into so folding reacts to sidebar collapse
// and browser zoom, not just to media-query breakpoints. A callback ref is used so
// the observer still attaches when the node appears after a mobile→desktop switch.
function useAvailableWidth() {
  const [width, setWidth] = useState(0);
  const observerRef = useRef(null);
  const setNode = useCallback((node) => {
    observerRef.current?.disconnect();
    observerRef.current = null;
    if (!node || typeof ResizeObserver === 'undefined') return;
    setWidth(node.clientWidth);
    const observer = new ResizeObserver(() => setWidth(node.clientWidth));
    observer.observe(node);
    observerRef.current = observer;
  }, []);
  useEffect(() => () => observerRef.current?.disconnect(), []);
  return [setNode, width];
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
  minScrollX,
  safeActionWidth,
  rowHeight,
  className,
  style,
  ...tableProps
}) {
  const { isMobile } = useResponsiveLayout();
  const [paneRef, paneWidth] = useAvailableWidth();
  const requestedColumns = isMobile && Array.isArray(mobileColumns) && mobileColumns.length > 0 ? mobileColumns : columns;
  const fitOptions = { minScrollX, safeActionWidth };
  // The selection checkbox column and the wrapper border eat into the pane before
  // any of the caller's columns get a share of it.
  const reservedWidth = WRAPPER_BORDER_WIDTH + (tableProps.rowSelection ? SELECTION_COLUMN_WIDTH : 0);
  // Mobile has its own card renderer; folding only applies to the desktop grid.
  const folded = isMobile
    ? { visible: flattenColumns(requestedColumns || []), hidden: [] }
    : foldColumnsToWidth(requestedColumns, paneWidth, fitOptions, reservedWidth);
  const activeColumns = folded.visible;
  const hiddenColumns = folded.hidden;
  // Same reasoning as inside the fold: the page's floor no longer describes this table.
  const widthOptions = hiddenColumns.length ? { ...fitOptions, minScrollX: 0 } : fitOptions;
  const activeScroll = isMobile && mobileScroll !== undefined ? mobileScroll : scroll;
  const rows = Array.isArray(dataSource) ? dataSource : [];
  const firstLoad = !lastRefresh && loading;
  const contentWidth = defaultTableScrollX(activeColumns, widthOptions);
  // An explicit scroll.x is likewise a floor for the caller's full column set.
  const explicitScrollX = activeScroll && activeScroll !== false && !hiddenColumns.length ? numericWidth(activeScroll.x) : 0;
  const fitWidth = Math.max(contentWidth, explicitScrollX);
  const resolvedScroll = activeScroll === false ? undefined : {
    ...(activeScroll || {}),
    // The measured fit wins over any declared x: it accounts for folding.
    x: fitWidth,
  };
  // Folded-away fields are not lost: each row can reveal them inline, which keeps
  // every value reachable without a horizontal scrollbar hiding columns off-pane.
  const expandedRowRender = useCallback((row, index) => {
    if (!hiddenColumns.length) return null;
    return (
      <dl className="pool-table-more">
        {hiddenColumns.map((column, columnIndex) => {
          const label = typeof column.title === 'string' ? column.title.replace(/[↑↓↕]/g, '').trim() : '';
          const value = typeof column.dataIndex === 'string' ? row?.[column.dataIndex] : undefined;
          return (
            <div key={columnKeyOf(column, columnIndex)}>
              <dt>{label}</dt>
              <dd>{column.render ? column.render(value, row, index) : (value ?? '—')}</dd>
            </div>
          );
        })}
      </dl>
    );
  }, [hiddenColumns]);
  const resolvedClassName = [
    'pool-table-wrapper',
    density ? `pool-table-density-${density}` : '',
    // There was a pool-table-layout-${layout} class here that no stylesheet ever matched,
    // and twelve pages passed layout="fit" expecting it to do something. Fitting the pane
    // is not optional and not per-caller: foldColumnsToWidth measures every table against
    // its pane and --pool-table-fit-width carries the result into the min-width below.
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
      <div ref={paneRef} className="pool-table-pane">
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
      </div>
    );
  }

  return (
    <div ref={paneRef} className="pool-table-pane">
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
          expandedRowRender={hiddenColumns.length ? expandedRowRender : undefined}
          {...tableProps}
        />
      )}
    </div>
  );
}
