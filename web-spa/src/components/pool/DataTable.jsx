import React, { useEffect, useMemo, useRef, useState } from 'react';

import EmptyState from './EmptyState.jsx';
import { Button } from './Button.jsx';

// A pane that scrolls horizontally must be reachable by keyboard, otherwise the
// columns past the fold are only available to pointer users (axe:
// scrollable-region-focusable). Tables become scrollable purely as a function of
// column count and viewport, so the affordance is applied only while it is needed —
// an always-on tab stop would add a focus target to every table on every page.
//
// `signature` keys the effect to the things that can change the table's intrinsic
// width. Re-running on every render would rebuild the observer and force a synchronous
// layout read on each sort, hover or selection change.
function useScrollableRegion(ref, signature) {
  const [scrollable, setScrollable] = useState(false);
  useEffect(() => {
    const element = ref.current;
    if (!element) return undefined;
    const measure = () => setScrollable(element.scrollWidth - element.clientWidth > 1);
    measure();
    if (typeof ResizeObserver === 'undefined') return undefined;
    const observer = new ResizeObserver(measure);
    observer.observe(element);
    for (const child of element.children) observer.observe(child);
    return () => observer.disconnect();
  }, [ref, signature]);
  return scrollable;
}

function cx(...parts) {
  return parts.filter(Boolean).join(' ');
}

function flattenColumns(columns = []) {
  return columns.flatMap((column) => Array.isArray(column.children) ? flattenColumns(column.children) : column);
}

function valueFor(row, column) {
  if (typeof column.dataIndex === 'string') return row?.[column.dataIndex];
  return undefined;
}

// Numbers read best right-aligned, so a column is aligned that way when its key names a
// quantity. The words have to be matched as whole tokens: as bare substrings, "count" is
// inside "account" and "rate" is inside "assignment_strategy", which right-aligned account
// ids, account labels and strategy tags — text, in columns whose neighbours are all left.
const NUMERIC_KEY_WORDS = new Set([
  'token', 'tokens', 'total', 'count', 'rate', 'pct', 'percent', 'bytes', 'panic', 'pid',
]);
// CJK does not tokenise on separators, so these stay substring tests.
const NUMERIC_KEY_CJK = /(请求|数|内存|磁盘|重启)/;

function inferredAlign(column) {
  if (column.align) return column.align;
  const key = String(column.key || column.dataIndex || column.title || '');
  if (NUMERIC_KEY_CJK.test(key)) return 'right';
  // snake_case, kebab-case, dotted paths and camelCase all split into their own words.
  // The camel boundary has to be found before the key is lowercased, or it is gone.
  const tokens = key.split(/[^A-Za-z0-9]+|(?<=[a-z0-9])(?=[A-Z])/).map((part) => part.toLowerCase());
  if (tokens.some((token) => NUMERIC_KEY_WORDS.has(token))) return 'right';
  return undefined;
}

function rowKeyOf(row, rowKey, index) {
  if (typeof rowKey === 'function') return rowKey(row, index) ?? row?.id ?? row?.key ?? index;
  if (typeof rowKey === 'string') return row?.[rowKey] ?? row?.id ?? row?.key ?? index;
  return row?.id ?? row?.key ?? index;
}

function isInteractive(target) {
  return !!target.closest?.('button,a,input,select,textarea,[role="button"],[role="menuitem"]');
}

export function DataTable({
  dataSource = [],
  columns = [],
  rowKey,
  pagination,
  empty,
  loading,
  rowSelection,
  className,
  style,
  scroll,
  onRow,
  expandedRowRender,
  'aria-label': ariaLabel,
  ...props
}) {
  const wrapperRef = useRef(null);
  const [expandedKeys, setExpandedKeys] = useState(() => new Set());
  const rows = Array.isArray(dataSource) ? dataSource : [];
  const leafColumns = flattenColumns(columns);
  const defaultSort = leafColumns.find((column) => column.defaultSortOrder);
  const [sortState, setSortState] = useState(() => defaultSort ? {
    key: defaultSort.key || defaultSort.dataIndex || defaultSort.title,
    order: defaultSort.defaultSortOrder,
  } : null);
  const [internalPage, setInternalPage] = useState(1);
  const pageSize = pagination && pagination !== false ? Number(pagination.pageSize) || 20 : rows.length || 1;
  const controlledPage = pagination && pagination !== false && pagination.currentPage;
  const currentPage = Number(controlledPage || internalPage || 1);
  const sortedRows = useMemo(() => {
    if (!sortState) return rows;
    const column = leafColumns.find((item) => (item.key || item.dataIndex || item.title) === sortState.key);
    if (!column?.sorter) return rows;
    const next = [...rows].sort(column.sorter);
    return sortState.order === 'descend' ? next.reverse() : next;
  }, [leafColumns, rows, sortState]);
  const total = pagination && pagination !== false ? Number(pagination.total) || sortedRows.length : sortedRows.length;
  const localPage = pagination && pagination !== false && !pagination.total;
  const pageRows = localPage ? sortedRows.slice((currentPage - 1) * pageSize, currentPage * pageSize) : sortedRows;
  const selected = new Set(rowSelection?.selectedRowKeys || []);
  const pageCount = Math.max(1, Math.ceil(total / pageSize));
  const tableStyle = {
    '--pool-table-fit-width': scroll?.x ? `${scroll.x}px` : undefined,
    ...(style || {}),
  };
  // Anything that can change how wide the table wants to be.
  const scrollable = useScrollableRegion(
    wrapperRef,
    `${leafColumns.length}:${pageRows.length}:${loading ? 1 : 0}:${scroll?.x ?? ''}:${className || ''}`,
  );

  const setPage = (next) => {
    const page = Math.min(pageCount, Math.max(1, next));
    setInternalPage(page);
    pagination?.onPageChange?.(page);
  };

  const toggleRow = (key, checked) => {
    if (!rowSelection || rowSelection.getCheckboxProps?.({})?.disabled) return;
    const next = new Set(selected);
    if (checked) next.add(key);
    else next.delete(key);
    rowSelection.onChange?.([...next]);
  };

  const content = pageRows.length ? (
    <table className="pool-table pool-table--cards" {...props}>
      <thead>
        <tr>
          {rowSelection ? <th style={{ width: 44 }} aria-label="选择" /> : null}
          {expandedRowRender ? <th className="pool-table-expander-cell" style={{ width: 40 }} aria-label="展开" /> : null}
          {leafColumns.map((column) => {
            const key = column.key || column.dataIndex || column.title;
            const active = sortState?.key === key ? sortState.order : '';
            return (
              <th key={String(key)} style={{ width: column.width }} data-align={inferredAlign(column)} aria-sort={active === 'ascend' ? 'ascending' : active === 'descend' ? 'descending' : 'none'}>
                {column.sorter ? (
                  <button
                    type="button"
                    className="pool-table-sort"
                    onClick={() => {
                      setSortState((current) => {
                        if (current?.key !== key) return { key, order: 'ascend' };
                        if (current.order === 'ascend') return { key, order: 'descend' };
                        return null;
                      });
                    }}
                  >
                    <span>{column.title}</span>
                    <span aria-hidden="true">{active === 'ascend' ? '↑' : active === 'descend' ? '↓' : '↕'}</span>
                  </button>
                ) : column.title}
              </th>
            );
          })}
        </tr>
      </thead>
      <tbody>
        {pageRows.map((row, index) => {
          const key = rowKeyOf(row, rowKey, index);
          const rowProps = onRow?.(row, index) || {};
          const checked = selected.has(key);
          const checkboxProps = rowSelection?.getCheckboxProps?.(row) || {};
          const expandable = typeof expandedRowRender === 'function' ? expandedRowRender(row, index) : null;
          const isExpanded = expandedKeys.has(String(key));
          const bodyCells = (
            <>
              {rowSelection ? (
                <td>
                  <input
                    type="checkbox"
                    checked={checked}
                    disabled={checkboxProps.disabled}
                    onChange={(event) => toggleRow(key, event.target.checked)}
                    aria-label={checkboxProps['aria-label'] || `选择第 ${index + 1} 行`}
                  />
                </td>
              ) : null}
              {expandedRowRender ? (
                <td className="pool-table-expander-cell">
                  {expandable ? (
                    <button
                      type="button"
                      className="pool-table-expander"
                      aria-expanded={isExpanded}
                      aria-label={isExpanded ? '收起其余字段' : '展开其余字段'}
                      onClick={(event) => {
                        event.stopPropagation();
                        setExpandedKeys((current) => {
                          const next = new Set(current);
                          if (next.has(String(key))) next.delete(String(key));
                          else next.add(String(key));
                          return next;
                        });
                      }}
                    >
                      <span aria-hidden="true">{isExpanded ? '⌄' : '›'}</span>
                    </button>
                  ) : null}
                </td>
              ) : null}
            </>
          );
          return (
            <React.Fragment key={String(key)}>
            <tr
              aria-selected={checked}
              tabIndex={rowProps.onClick ? 0 : undefined}
              onClick={(event) => {
                if (isInteractive(event.target)) return;
                rowProps.onClick?.(event);
              }}
              onKeyDown={(event) => {
                if ((event.key === 'Enter' || event.key === ' ') && rowProps.onClick) {
                  event.preventDefault();
                  rowProps.onClick(event);
                }
              }}
            >
              {bodyCells}
              {leafColumns.map((column) => {
                const keyName = column.key || column.dataIndex || column.title;
                const value = valueFor(row, column);
                const label = typeof column.title === 'string' ? column.title.replace(/[↑↓↕]/g, '').trim() : '';
                return <td key={String(keyName)} data-label={label} data-align={inferredAlign(column)}>{column.render ? column.render(value, row, index) : value}</td>;
              })}
            </tr>
            {expandable && isExpanded ? (
              <tr className="pool-table-expanded-row">
                <td colSpan={leafColumns.length + (rowSelection ? 1 : 0) + 1}>{expandable}</td>
              </tr>
            ) : null}
            </React.Fragment>
          );
        })}
      </tbody>
    </table>
  ) : (
    <div className="pool-table-empty">{empty || <EmptyState title="暂无数据" />}</div>
  );

  return (
    <div
      ref={wrapperRef}
      className={cx('pool-table-wrapper', className)}
      style={tableStyle}
      tabIndex={scrollable ? 0 : undefined}
      role={scrollable ? 'group' : undefined}
      aria-label={scrollable ? (ariaLabel || '可横向滚动的数据表') : undefined}
    >
      {loading && !rows.length ? <div className="pool-table-empty"><span className="pool-spinner" role="status" aria-label="正在加载数据" /></div> : content}
      {pagination && pagination !== false && total > pageSize ? (
        <div className="pool-pagination">
          <span>{currentPage} / {pageCount}</span>
          <Button size="small" onClick={() => setPage(currentPage - 1)} disabled={currentPage <= 1}>上一页</Button>
          <Button size="small" onClick={() => setPage(currentPage + 1)} disabled={currentPage >= pageCount}>下一页</Button>
        </div>
      ) : null}
    </div>
  );
}

export const Table = DataTable;
export default DataTable;
