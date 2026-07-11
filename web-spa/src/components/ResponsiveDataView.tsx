import { useMemo, useState, type Key, type ReactNode } from 'react';
import * as PoolUI from './pool/index.jsx';
import ResourceTable from './ResourceTable.jsx';
import type { ResponsiveDataView as ResponsiveDataViewModel } from '../model/contracts';

const { Button, Drawer } = PoolUI as any;
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
  const [selected, setSelected] = useState<T | null>(null);
  const mobileActions = useMemo(() => allowedResponsiveActions(definition, true), [definition]);

  const mobileRenderer = (row: T): ReactNode => (
    <div className="pool-responsive-summary">
      <button type="button" className="pool-responsive-summary__open" onClick={() => setSelected(row)}>
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
      <Drawer visible={Boolean(selected)} onCancel={() => setSelected(null)} title="详情" footer={null}>
        {selected ? definition.details(selected) : null}
      </Drawer>
    </>
  );
}
