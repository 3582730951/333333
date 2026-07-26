import React from 'react';
import * as PoolUI from './pool/index.jsx';
import { IconDelete, IconEdit } from './pool/icons.jsx';
import { KeyCopyActions } from './KeySecretTools.jsx';
import ResourceTable from './ResourceTable.jsx';
import { fmtDateTime } from '../lib/format.js';
import { t } from '../lib/i18n.js';
import type { ApiKeyRow } from '../features/access/model/keys';

const { ActionMenu, Switch, Tag, Typography } = PoolUI as any;
const DataTable = ResourceTable as any;
const CopyActions = KeyCopyActions as any;
type KeyTableMode = 'admin' | 'portal';
interface ApiKeysTableProps {
  rows: ApiKeyRow[];
  loading: boolean;
  mode?: KeyTableMode;
  onDelete?: (hash: string) => void | Promise<void>;
  onEdit?: (row: ApiKeyRow) => void;
  onToggle?: (row: ApiKeyRow, enabled: boolean) => void | Promise<void>;
  userGroupNames?: Record<string, string>;
  deleteRunning?: boolean;
  isDeleteRunning?: (hash: string) => boolean;
  toggleRunning?: boolean;
  isToggleRunning?: (hash: string) => boolean;
  isEditRunning?: (hash: string) => boolean;
}

function keyHash(row: ApiKeyRow) {
  return row.key_hash || row.hash || '';
}

function keyRowID(row: ApiKeyRow) {
  return keyHash(row) || row.label || String(row.created_at || row.secret || '');
}

function labelCell(value: unknown, row: ApiKeyRow, mode: KeyTableMode) {
  if (mode !== 'portal') return value || '—';
  return (
    <div>
      <div>{String(value || '') || <span className="pool-muted">{t('keys.unnamed')}</span>}</div>
      <Typography.Text type="tertiary" size="small" className="pool-mono">…{String(keyHash(row)).slice(-10)}</Typography.Text>
    </div>
  );
}

function mobileInfoCell(row: ApiKeyRow, mode: KeyTableMode, userGroupNames: Record<string, string>) {
  const portal = mode === 'portal';
  const label = row.label || t('keys.unnamed');
  const group = row.user_group_id
    ? userGroupNames[row.user_group_id] || row.user_group_id
    : row.group_name || (portal ? '—' : t('keys.default_group'));
  const model = row.force_model || t('keys.no_model');
  const effort = row.force_effort || '';
  return (
    <div className="pool-key-mobile-info">
      <div className="pool-key-mobile-title">{label}</div>
      <div className="pool-key-mobile-meta">
        <Tag size="small">{group}</Tag>
        {row.key_type === 'pool_import' ? <Tag size="small" color="violet">poolimp</Tag> : null}
        {row.enabled === false ? <Tag size="small" color="orange">{t('keys.disabled')}</Tag> : <Tag size="small" color="green">{t('keys.enabled')}</Tag>}
      </div>
      <Typography.Text type="tertiary" size="small" className="pool-key-mobile-detail">
        {model}{effort ? ` · ${effort}` : ''}
      </Typography.Text>
      <Typography.Text type="tertiary" size="small" className="pool-mono pool-key-mobile-detail">
        …{String(keyHash(row)).slice(-10)}
      </Typography.Text>
    </div>
  );
}

function keyActions(
  row: ApiKeyRow,
  mode: KeyTableMode,
  onEdit: ApiKeysTableProps['onEdit'],
  onDelete: ApiKeysTableProps['onDelete'],
  deleteRunning: boolean,
  isDeleteRunning: (hash: string) => boolean,
  isEditRunning: (hash: string) => boolean,
  disabled = false,
) {
  const hash = keyHash(row);
  const editRunning = isEditRunning(hash);
  const items = [];
  if (mode === 'admin') {
    items.push({
      label: editRunning ? '保存中' : '编辑策略',
      icon: <IconEdit />,
      disabled: disabled || editRunning || isDeleteRunning(hash),
      onSelect: () => onEdit?.(row),
    });
  }
  items.push(
    {
      label: isDeleteRunning(hash) ? t('keys.deleting') : t('keys.delete'),
      icon: <IconDelete />,
      destructive: true,
      disabled: disabled || editRunning || isDeleteRunning(hash),
      confirm: {
        title: t('keys.delete_title'),
        description: t('keys.delete_desc'),
        confirmText: t('common.delete'),
      },
      onSelect: () => onDelete?.(hash),
    },
  );
  return (
    <ActionMenu
      label={t('keys.actions')}
      items={items}
    />
  );
}

function mobileKeyCell(
  row: ApiKeyRow,
  mode: KeyTableMode,
  onEdit: ApiKeysTableProps['onEdit'],
  onDelete: ApiKeysTableProps['onDelete'],
  userGroupNames: Record<string, string>,
  deleteRunning: boolean,
  isDeleteRunning: (hash: string) => boolean,
  isEditRunning: (hash: string) => boolean,
  disabled = false,
) {
  return (
    <div className="pool-key-mobile-cell">
      {mobileInfoCell(row, mode, userGroupNames)}
      <div className="pool-key-mobile-actions">
        <CopyActions secret={row.secret} compact />
        {keyActions(row, mode, onEdit, onDelete, deleteRunning, isDeleteRunning, isEditRunning, disabled)}
      </div>
    </div>
  );
}

export default function ApiKeysTable({
  rows,
  loading,
  mode = 'admin',
  onDelete,
  onEdit,
  onToggle,
  userGroupNames = {},
  deleteRunning = false,
  isDeleteRunning = () => false,
  toggleRunning = false,
  isToggleRunning = () => false,
  isEditRunning = () => false,
}: ApiKeysTableProps) {
  const portal = mode === 'portal';
  const mobileColumns = [
    {
      title: 'API Key',
      key: 'mobile_key',
      render: (_: unknown, row: ApiKeyRow) => mobileKeyCell(row, mode, onEdit, onDelete, userGroupNames, deleteRunning, isDeleteRunning, isEditRunning, portal && toggleRunning),
    },
  ];
  const columns = portal ? [
    { title: t('keys.name'), dataIndex: 'label', width: 180, render: (value: unknown, row: ApiKeyRow) => labelCell(value, row, mode) },
    { title: t('keys.copy_install'), dataIndex: 'secret', width: 320, render: (value: string | undefined) => <CopyActions secret={value} /> },
    { title: t('keys.force_model'), dataIndex: 'force_model', width: 160, render: (value: string | undefined) => (value ? <Tag>{value}</Tag> : '—') },
    { title: t('keys.group'), dataIndex: 'group_name', width: 120, render: (value: string | undefined) => value || '—' },
    { title: t('keys.enabled'), dataIndex: 'enabled', width: 90, render: (value: boolean | undefined, row: ApiKeyRow) => {
      const hash = keyHash(row);
      return <Switch checked={value} size="small" loading={isToggleRunning(hash)} disabled={toggleRunning} onChange={(checked: boolean) => onToggle?.(row, checked)} />;
    } },
    { title: t('keys.created_at'), dataIndex: 'created_at', width: 140, render: fmtDateTime },
    {
      title: t('keys.operations'),
      width: 90,
      fixed: 'right',
      render: (_: unknown, row: ApiKeyRow) => {
        return keyActions(row, mode, onEdit, onDelete, deleteRunning, isDeleteRunning, isEditRunning, toggleRunning);
      },
    },
  ] : [
    { title: t('keys.label'), dataIndex: 'label', width: 160, render: (value: unknown, row: ApiKeyRow) => labelCell(value, row, mode) },
    { title: t('keys.type'), dataIndex: 'key_type', width: 120, render: (value: string | undefined) => (value === 'pool_import' ? <Tag color="violet">poolimp</Tag> : <Tag>{t('keys.inference')}</Tag>) },
    { title: '用户分组', dataIndex: 'user_group_id', width: 150, render: (value: string | undefined) => value ? userGroupNames[value] || value : '—' },
    { title: t('keys.group'), dataIndex: 'group_name', width: 120, render: (value: string | undefined) => value || t('keys.default_group') },
    { title: t('keys.force_model'), dataIndex: 'force_model', width: 180, render: (value: string | undefined) => value || '—' },
    { title: t('keys.effort'), dataIndex: 'force_effort', width: 120, render: (value: string | undefined) => (value ? <Tag color="blue">{value}</Tag> : '—') },
    { title: t('keys.copy_install'), dataIndex: 'secret', width: 320, render: (value: string | undefined) => <CopyActions secret={value} /> },
    { title: t('keys.enabled'), dataIndex: 'enabled', width: 90, render: (value: boolean | undefined) => (value === false ? <Tag color="orange">{t('keys.no')}</Tag> : <Tag color="green">{t('keys.yes')}</Tag>) },
    { title: t('keys.expires_at'), dataIndex: 'expires_at', width: 150, render: (value: number | undefined) => (value ? fmtDateTime(value) : t('keys.never_expires')) },
    { title: t('keys.last_used'), dataIndex: 'last_used_at', width: 150, render: (value: number | undefined) => (value ? fmtDateTime(value) : '—') },
    {
      title: t('keys.operations'),
      key: 'ops',
      width: 100,
      fixed: 'right',
      render: (_: unknown, row: ApiKeyRow) => {
        return keyActions(row, mode, onEdit, onDelete, deleteRunning, isDeleteRunning, isEditRunning);
      },
    },
  ];

  return (
    <DataTable
      className="pool-key-table"
      dataSource={rows}
      columns={columns}
      mobileColumns={mobileColumns}
      rowKey={keyRowID}
      loading={loading}
      pagination={portal ? { pageSize: 15 } : false}
      emptyTitle={portal ? t('keys.empty_portal') : t('keys.empty_admin')}
      emptyType="keys"
      skeletonRows={6}
      skeletonCols={columns.length}
      scroll={{ x: 1080 }}
      mobileScroll={false}
    />
  );
}
