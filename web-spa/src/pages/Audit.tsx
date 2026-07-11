import React, { useState, useMemo } from 'react';
import * as PoolUI from '../components/pool/index.jsx';
import { IconRefresh, IconDownload } from '../components/pool/icons.jsx';
import PageHeader from '../components/PageHeader.jsx';
import ResourceTable from '../components/ResourceTable.jsx';
import MobileResourceCell from '../components/MobileResourceCell.jsx';
import { useAuditData } from '../features/observability/queries/events.ts';
import { useAuditArchiveMutation } from '../features/observability/queries/exports';
import type { AuditExportKind } from '../features/observability/api/exports';
import type { AuditRow } from '../features/observability/model/types';
import { toCSV, downloadCSV } from '../lib/csv.js';
import { downloadBlob } from '../lib/browserDownload.js';
import { fmtDateTime, fmtRelative } from '../lib/format.js';
import { t } from '../lib/i18n.js';

const { Button, Tag, Select, Typography, Toast } = PoolUI as any;
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
function clipText(value: unknown, max = 28) {
  const text = String(value || '');
  if (text.length <= max) return text || '—';
  return `${text.slice(0, Math.max(8, max - 9))}…${text.slice(-6)}`;
}

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
    try {
      const archive = await archiveMutation.mutateAsync(kind);
      if (!downloadBlob(archive.filename, archive.blob)) Toast.error(t('audit.export_failed'));
      else Toast.success(successMessage);
    } catch {
      Toast.error(t('audit.archive_failed'));
    }
  };

  const cols: any[] = [
    { title: t('audit.time'), dataIndex: 'created_at', width: 170, sorter: (a: AuditRow, b: AuditRow) => (a.created_at || 0) - (b.created_at || 0), defaultSortOrder: 'descend',
      render: (v: number | undefined) => (
        <div>
          <Typography.Text style={{ fontFamily: 'ui-monospace, SFMono-Regular, Menlo, monospace', fontSize: 12.5 }}>{fmtDateTime(v)}</Typography.Text>
          <Typography.Text type="tertiary" size="small" style={{ display: 'block', fontSize: 11, marginTop: 2 }}>{fmtRelative(v)}</Typography.Text>
        </div>
      )
    },
    { title: t('audit.account'), dataIndex: 'account_label', width: 150, render: (v: string | undefined, r: AuditRow) => <span title={v || r.account_id || ''}>{clipText(v || r.account_id, 24)}</span> },
    { title: t('audit.action'), dataIndex: 'action', width: 118, render: (v: string | undefined) => <Tag title={v}>{clipText(actionLabel(v), 16)}</Tag> },
    { title: t('audit.result'), dataIndex: 'state', width: 108, render: (v: string | undefined) => (v ? <Tag color={stateColor(v)}>{stateLabel(v)}</Tag> : '—') },
    { title: t('audit.reason'), dataIndex: 'reason', width: 116, render: (v: string | undefined) => v || '—' },
    { title: t('audit.detail'), dataIndex: 'detail', width: 220, render: (v: string | undefined) => <Typography.Text title={v || ''} className="pool-mono pool-audit-detail">{clipText(v, 22)}</Typography.Text> },
  ];

  return (
    <div>
      <PageHeader title={t('audit.title')} subtitle={t('audit.subtitle')}
        actions={<>
          <Select value={action} onChange={(value: string) => setAction(value)} placeholder={t('audit.all_actions')} style={{ width: 180 }}
            optionList={[{ label: t('audit.all_actions'), value: '' }, ...actions.map((value) => ({ label: actionLabel(value), value }))]} />
          <span className="pool-audit-export-group">
            <Button icon={<IconDownload />} disabled={archiveMutation.isPending} loading={archiveMutation.isPending && archiveMutation.variables === 'cache-hits'} onClick={() => exportArchive('cache-hits', t('audit.cache_done'))}>{t('audit.export_cache')}</Button>
            <Button icon={<IconDownload />} disabled={archiveMutation.isPending} loading={archiveMutation.isPending && archiveMutation.variables === 'diagnostics'} onClick={() => exportArchive('diagnostics', t('audit.diagnostics_done'))}>{t('audit.export_diagnostics')}</Button>
            <Button icon={<IconDownload />} onClick={exportCSV}>{t('audit.export_csv')}</Button>
          </span>
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
        layout="fit"
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
