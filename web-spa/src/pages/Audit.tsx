import React, { useEffect, useMemo, useRef, useState } from 'react';
import * as PoolUI from '../components/pool/index.jsx';
import { IconRefresh, IconDownload } from '../components/pool/icons.jsx';
import PageHeader from '../components/PageHeader.jsx';
import ResourceTable from '../components/ResourceTable.jsx';
import MobileResourceCell from '../components/MobileResourceCell.jsx';
import { TextClamp } from '../components/DisplayPrimitives.jsx';
import { useAuditData } from '../features/observability/queries/events.ts';
import { useAuditArchiveMutation } from '../features/observability/queries/exports';
import type { AuditExportKind } from '../features/observability/api/exports';
import type { AuditRow } from '../features/observability/model/types';
import { toCSV, downloadCSV } from '../lib/csv.js';
import { downloadBlob } from '../lib/browserDownload.js';
import { fmtDateTime, fmtRelative } from '../lib/format.js';
import { t } from '../lib/i18n.js';

const { ActionMenu, Button, Tag, Select, Toast } = PoolUI as any;
const DataTable = ResourceTable as any;
const MobileRow = MobileResourceCell as any;

const stateColor = (state: unknown) => {
  const colors: Record<string, string> = { alive: 'green', banned: 'red', permission_denied: 'red', rate_limited: 'amber', unreachable: 'grey', unknown: 'grey' };
  return colors[String(state || '')] || 'blue';
};
const stateLabel = (state: unknown) => {
  const value = String(state || '');
  return value ? t(`audit.state.${value}`, value) : '—';
};
const actionLabel = (action: unknown) => {
  const value = String(action || '');
  return value ? t(`audit.action.${value}`, value) : '—';
};

export default function Audit() {
  const [action, setAction] = useState('');
  const {
    data: rows = [],
    loading,
    error,
    lastRefresh,
    reload: load,
  } = useAuditData();
  const archiveMutation = useAuditArchiveMutation();
  const archiveAbortRef = useRef<AbortController | null>(null);

  useEffect(() => () => {
    archiveAbortRef.current?.abort();
    archiveAbortRef.current = null;
  }, []);

  const actions = useMemo(() => Array.from(new Set(rows.map((row) => row.action).filter((value): value is string => Boolean(value)))), [rows]);
  const filtered = action ? rows.filter((r) => r.action === action) : rows;

  const exportCSV = () => {
    const ok = downloadCSV('audit.csv', toCSV(filtered, [
      { title: 'time', get: (r: AuditRow) => fmtDateTime(r.created_at) }, { title: 'account', get: (r: AuditRow) => r.account_label || r.account_id },
      { title: 'action', get: (r: AuditRow) => r.action }, { title: 'state', get: (r: AuditRow) => r.state },
      { title: 'reason', get: (r: AuditRow) => r.reason }, { title: 'detail', get: (r: AuditRow) => r.detail },
    ]));
    if (!ok) Toast.error(t('audit.export_failed'));
  };

  const exportArchive = async (kind: AuditExportKind, successMessage: string) => {
    archiveAbortRef.current?.abort();
    const controller = typeof AbortController === 'undefined' ? null : new AbortController();
    archiveAbortRef.current = controller;
    try {
      const archive = await archiveMutation.mutateAsync({
        kind,
        diagnosticOptions: controller ? { signal: controller.signal } : {},
      });
      if (!downloadBlob(archive.filename, archive.blob)) Toast.error(t('audit.export_failed'));
      else Toast.success(successMessage);
    } catch (error) {
      if (controller?.signal.aborted) return;
      const detail = error instanceof Error ? error.message.trim() : '';
      Toast.error(detail || t('audit.archive_failed'));
    } finally {
      if (archiveAbortRef.current === controller) archiveAbortRef.current = null;
    }
  };

  const cols: any[] = [
    { title: t('audit.time'), dataIndex: 'created_at', width: 170, sorter: (a: AuditRow, b: AuditRow) => (a.created_at || 0) - (b.created_at || 0), defaultSortOrder: 'descend',
      render: (v: number | undefined) => (
        <div className="pool-resource-summary">
          <span className="pool-mono">{fmtDateTime(v)}</span>
          <div className="pool-resource-summary__meta">{fmtRelative(v)}</div>
        </div>
      )
    },
    // Clamping by character count puts an ellipsis in strings the column had room for
    // and misjudges CJK, which is twice as wide per character. TextClamp cuts at the
    // real edge of the cell instead, so nothing is abbreviated that need not be.
    { title: t('audit.account'), dataIndex: 'account_label', width: 150, render: (v: string | undefined, r: AuditRow) => <TextClamp title={v || r.account_id || ''}>{v || r.account_id}</TextClamp> },
    { title: t('audit.action'), dataIndex: 'action', width: 118, render: (v: string | undefined) => <Tag title={actionLabel(v)}>{actionLabel(v)}</Tag> },
    { title: t('audit.result'), dataIndex: 'state', width: 108, render: (v: string | undefined) => (v ? <Tag color={stateColor(v)}>{stateLabel(v)}</Tag> : '—') },
    { title: t('audit.reason'), dataIndex: 'reason', width: 116, render: (v: string | undefined) => <TextClamp title={v || ''}>{v}</TextClamp> },
    { title: t('audit.detail'), dataIndex: 'detail', width: 220, priority: 20, render: (v: string | undefined) => <TextClamp lines={2} title={v || ''} className="pool-mono">{v}</TextClamp> },
  ];

  return (
    <div>
      <PageHeader title={t('audit.title')} subtitle={t('audit.subtitle')}
        actions={<>
          <Select value={action} onChange={(value: string) => setAction(value)} placeholder={t('audit.all_actions')} style={{ width: 180 }}
            optionList={[{ label: t('audit.all_actions'), value: '' }, ...actions.map((value) => ({ label: actionLabel(value), value }))]} />
          {/* Three side-by-side export buttons made the header read as the page's main
              event. They are one occasional action with three targets, so they collapse
              into one menu and the header keeps its focus on the log itself. */}
          <ActionMenu
            text={t('audit.export')}
            icon={<IconDownload />}
            label={t('audit.export')}
            loading={archiveMutation.isPending}
            items={[
              { label: t('audit.export_csv'), icon: <IconDownload />, onSelect: exportCSV },
              { label: t('audit.export_cache'), icon: <IconDownload />, disabled: archiveMutation.isPending, onSelect: () => exportArchive('cache-hits', t('audit.cache_done')) },
              { label: t('audit.export_diagnostics'), icon: <IconDownload />, disabled: archiveMutation.isPending, onSelect: () => exportArchive('diagnostics', t('audit.diagnostics_done')) },
            ]}
          />
          <Button icon={<IconRefresh />} onClick={load}>{t('common.refresh')}</Button>
        </>} />
      <DataTable
        error={error}
        onRetry={load}
        loading={loading}
        lastRefresh={lastRefresh}
        dataSource={filtered}
        columns={cols}
        rowKey="id"
        pagination={{ pageSize: 30 }}
        className="pool-audit-table"
        emptyTitle={t('audit.empty')}
        emptyDesc={t('audit.empty_desc')}
        loadingTitle={t('audit.loading')}
        skeletonRows={8}
        skeletonCols={6}
        mobileRenderer={(row: AuditRow) => (
          <MobileRow
            title={actionLabel(row.action)}
            subtitle={row.account_label || row.account_id || t('cf.unlinked_account')}
            badges={row.state ? <Tag color={stateColor(row.state)}>{stateLabel(row.state)}</Tag> : null}
            details={[
              { label: t('audit.time'), value: fmtDateTime(row.created_at) },
              { label: t('audit.reason'), value: row.reason || '—' },
              { label: t('audit.detail'), value: row.detail || '—' },
            ]}
          />
        )}
        mobileListLabel={t('audit.mobile_label')}
      />
    </div>
  );
}
