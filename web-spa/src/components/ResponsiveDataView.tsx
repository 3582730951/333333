import { useEffect, useMemo, useState, type Key, type ReactNode } from 'react';
import { Button, Drawer } from './pool/index.jsx';
import ResourceTable from './ResourceTable.jsx';
import type { ResponsiveDataView as ResponsiveDataViewModel } from '../model/contracts';

const DataViewTable = ResourceTable as any;

interface ResponsiveDataViewProps<T> {
  rows: T[];
  definition: ResponsiveDataViewModel<T>;
  rowKey: (row: T, index: number) => Key;
  loading?: boolean;
  error?: unknown;
  lastRefresh?: Date | null;
  onRetry?: () => void;
  emptyTitle?: string;
}

export function allowedResponsiveActions<T>(definition: ResponsiveDataViewModel<T>, mobile: boolean) {
  return definition.actions.filter((action) => !mobile || action.mobile === 'allow');
}

export default function ResponsiveDataView<T>({
  rows,
  definition,
  rowKey,
  loading = false,
  error,
  lastRefresh,
  onRetry,
  emptyTitle = '暂无数据',
}: ResponsiveDataViewProps<T>) {
  const [selectedKey, setSelectedKey] = useState<Key | null>(null);
  const mobileActions = useMemo(() => allowedResponsiveActions(definition, true), [definition]);
  const selectedIndex = useMemo(() => {
    if (selectedKey === null) return -1;
    return rows.findIndex((row, index) => rowKey(row, index) === selectedKey);
  }, [rowKey, rows, selectedKey]);
  const hasSelection = selectedIndex >= 0;
  const selected = hasSelection ? rows[selectedIndex] : undefined;

  useEffect(() => {
    if (selectedKey !== null && !hasSelection) setSelectedKey(null);
  }, [hasSelection, selectedKey]);

  const mobileRenderer = (row: T, context: { key: Key }): ReactNode => (
    <div className="pool-responsive-summary">
      <button type="button" className="pool-responsive-summary__open" onClick={() => setSelectedKey(context.key)}>
        {definition.mobileSummary(row)}
      </button>
      {mobileActions.length ? (
        <div className="pool-responsive-summary__actions">
          {mobileActions.map((action) => (
            <Button
              key={action.key}
              size="small"
              type={action.destructive ? 'danger' : undefined}
              disabled={action.disabled?.(row)}
              onClick={() => action.run(row)}
            >{action.label}</Button>
          ))}
        </div>
      ) : null}
    </div>
  );

  return (
    <>
      <DataViewTable
        error={error}
        onRetry={onRetry}
        loading={loading}
        lastRefresh={lastRefresh}
        dataSource={rows}
        columns={definition.desktopColumns}
        rowKey={rowKey}
        mobileRenderer={mobileRenderer}
        emptyTitle={emptyTitle}
      />
      <Drawer visible={hasSelection} onCancel={() => setSelectedKey(null)} title="详情" footer={null}>
        {hasSelection ? definition.details(selected as T) : null}
      </Drawer>
    </>
  );
}
