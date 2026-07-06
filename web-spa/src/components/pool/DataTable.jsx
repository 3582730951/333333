import React, { useMemo, useState } from 'react';

import EmptyState from './EmptyState.jsx';
import { Button } from './Button.jsx';

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

function inferredAlign(column) {
  if (column.align) return column.align;
  const key = String(column.key || column.dataIndex || column.title || '').toLowerCase();
  if (/(token|tokens|请求|数|total|count|rate|pct|percent|bytes|内存|磁盘|重启|panic|pid)/.test(key)) return 'right';
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
  ...props
}) {
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
          return (
            <tr
              key={String(key)}
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
              {rowSelection ? (
                <td>
                  <input
                    type="checkbox"
                    checked={checked}
                    disabled={checkboxProps.disabled}
                    onChange={(event) => toggleRow(key, event.target.checked)}
                    aria-label={`选择第 ${index + 1} 行`}
                  />
                </td>
              ) : null}
              {leafColumns.map((column) => {
                const keyName = column.key || column.dataIndex || column.title;
                const value = valueFor(row, column);
                const label = typeof column.title === 'string' ? column.title.replace(/[↑↓↕]/g, '').trim() : '';
                return <td key={String(keyName)} data-label={label} data-align={inferredAlign(column)}>{column.render ? column.render(value, row, index) : value}</td>;
              })}
            </tr>
          );
        })}
      </tbody>
    </table>
  ) : (
    <div className="pool-table-empty">{empty || <EmptyState title="暂无数据" />}</div>
  );

  return (
    <div className={cx('pool-table-wrapper', className)} style={tableStyle}>
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
