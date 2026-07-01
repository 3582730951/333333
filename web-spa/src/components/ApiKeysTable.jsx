import React from 'react';
import { Button, Popconfirm, Switch, Tag, Typography } from '@douyinfe/semi-ui';
import { KeyCopyActions } from './KeySecretTools.jsx';
import ResourceTable from './ResourceTable.jsx';
import { fmtDateTime } from '../lib/format.js';

function keyHash(row) {
  return row.key_hash || row.hash || '';
}

function keyRowID(row) {
  return keyHash(row) || row.label || String(row.created_at || row.secret || '');
}

function labelCell(value, row, mode) {
  if (mode !== 'portal') return value || '—';
  return (
    <div>
      <div>{value || <span className="pool-muted">未命名</span>}</div>
      <Typography.Text type="tertiary" size="small" className="pool-mono">…{String(keyHash(row)).slice(-10)}</Typography.Text>
    </div>
  );
}

function mobileInfoCell(row, mode) {
  const portal = mode === 'portal';
  const label = row.label || '未命名';
  const group = row.group_name || (portal ? '—' : '默认');
  const model = row.force_model || '未限定模型';
  const effort = row.force_effort || '';
  return (
    <div className="pool-key-mobile-info">
      <div className="pool-key-mobile-title">{label}</div>
      <div className="pool-key-mobile-meta">
        <Tag size="small">{group}</Tag>
        {row.enabled === false ? <Tag size="small" color="orange">停用</Tag> : <Tag size="small" color="green">启用</Tag>}
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

function deleteAction(row, onDelete, deleteRunning, isDeleteRunning, disabled = false) {
  const hash = keyHash(row);
  return (
    <Popconfirm title="删除该 Key？" onConfirm={() => onDelete?.(hash)}>
      <Button size="small" type="danger" loading={isDeleteRunning(hash)} disabled={disabled || (deleteRunning && !isDeleteRunning(hash))}>删除</Button>
    </Popconfirm>
  );
}

function mobileKeyCell(row, mode, onDelete, deleteRunning, isDeleteRunning, disabled = false) {
  return (
    <div className="pool-key-mobile-cell">
      {mobileInfoCell(row, mode)}
      <div className="pool-key-mobile-actions">
        <KeyCopyActions secret={row.secret} compact />
        {deleteAction(row, onDelete, deleteRunning, isDeleteRunning, disabled)}
      </div>
    </div>
  );
}

export default function ApiKeysTable({
  rows,
  loading,
  mode = 'admin',
  onDelete,
  onToggle,
  deleteRunning = false,
  isDeleteRunning = () => false,
  toggleRunning = false,
  isToggleRunning = () => false,
}) {
  const portal = mode === 'portal';
  const mobileColumns = [
    {
      title: 'API Key',
      key: 'mobile_key',
      render: (_, row) => mobileKeyCell(row, mode, onDelete, deleteRunning, isDeleteRunning, portal && toggleRunning),
    },
  ];
  const columns = portal ? [
    { title: '名称', dataIndex: 'label', width: 180, render: (value, row) => labelCell(value, row, mode) },
    { title: 'Key / 一键安装', dataIndex: 'secret', width: 320, render: (value) => <KeyCopyActions secret={value} /> },
    { title: '强制模型', dataIndex: 'force_model', width: 160, render: (value) => (value ? <Tag>{value}</Tag> : '—') },
    { title: '分组', dataIndex: 'group_name', width: 120, render: (value) => value || '—' },
    { title: '启用', dataIndex: 'enabled', width: 90, render: (value, row) => {
      const hash = keyHash(row);
      return <Switch checked={value} size="small" loading={isToggleRunning(hash)} disabled={toggleRunning} onChange={(checked) => onToggle?.(row, checked)} />;
    } },
    { title: '创建', dataIndex: 'created_at', width: 140, render: fmtDateTime },
    {
      title: '操作',
      width: 90,
      fixed: 'right',
      render: (_, row) => {
        return deleteAction(row, onDelete, deleteRunning, isDeleteRunning, toggleRunning);
      },
    },
  ] : [
    { title: '标签', dataIndex: 'label', width: 160, render: (value, row) => labelCell(value, row, mode) },
    { title: '分组', dataIndex: 'group_name', width: 120, render: (value) => value || '默认' },
    { title: '强制模型', dataIndex: 'force_model', width: 180, render: (value) => value || '—' },
    { title: '推理强度', dataIndex: 'force_effort', width: 120, render: (value) => (value ? <Tag color="blue">{value}</Tag> : '—') },
    { title: 'Key / 一键安装', dataIndex: 'secret', width: 320, render: (value) => <KeyCopyActions secret={value} /> },
    { title: '启用', dataIndex: 'enabled', width: 90, render: (value) => (value === false ? <Tag color="orange">否</Tag> : <Tag color="green">是</Tag>) },
    {
      title: '操作',
      key: 'ops',
      width: 100,
      fixed: 'right',
      render: (_, row) => {
        return deleteAction(row, onDelete, deleteRunning, isDeleteRunning);
      },
    },
  ];

  return (
    <div className="pool-table-wrapper pool-key-table">
      <ResourceTable
        dataSource={rows}
        columns={columns}
        mobileColumns={mobileColumns}
        rowKey={keyRowID}
        loading={loading}
        pagination={portal ? { pageSize: 15 } : false}
        emptyTitle={portal ? '还没有 API Key' : '暂无 API Key'}
        emptyType="keys"
        skeletonRows={6}
        skeletonCols={columns.length}
        scroll={{ x: 1080 }}
        mobileScroll={false}
      />
    </div>
  );
}
