import React, { useState, useCallback } from 'react';
import { Button, Toast, Modal, Form, Popconfirm, Tag } from '@douyinfe/semi-ui';
import { IconPlus, IconRefresh } from '@douyinfe/semi-icons';
import { get, post, del } from '../api.js';
import PageHeader from '../components/PageHeader.jsx';
import ResourceTable from '../components/ResourceTable.jsx';
import { ActionGroup, MetricRail, TagList, TextClamp } from '../components/DisplayPrimitives.jsx';
import { showErrorToast } from '../components/ErrorToast.jsx';
import useAsyncAction from '../hooks/useAsyncAction.js';
import useKeyedAsyncAction from '../hooks/useKeyedAsyncAction.js';
import useAsyncResource from '../hooks/useAsyncResource.js';

function groupPolicyTags(row) {
  const tags = [];
  if (row.force_model) tags.push({ label: row.force_model, color: 'blue' });
  if (row.force_effort) tags.push({ label: `effort ${row.force_effort}`, color: 'violet' });
  tags.push({ label: row.virtual_2m_enabled ? 'Virtual2M 开' : 'Virtual2M 关', color: row.virtual_2m_enabled ? 'green' : 'grey' });
  if (!row.force_model && !row.force_effort) tags.unshift({ label: '继承默认', color: 'grey' });
  return tags;
}

export default function Groups() {
  const [open, setOpen] = useState(false);

  const fetchRows = useCallback(async ({ signal }) => {
    const g = await get('/admin/groups', undefined, { signal });
    return Array.isArray(g) ? g : g?.groups || [];
  }, []);
  const { data: rows = [], loading, error, lastRefresh, reload: load } = useAsyncResource(fetchRows, [fetchRows], { initialData: [] });
  const groupMetrics = [
    { label: '分组数', value: rows.length },
    { label: '强制模型', value: rows.filter((row) => row.force_model).length },
    { label: '推理强度', value: rows.filter((row) => row.force_effort).length },
    { label: 'Virtual2M', value: rows.filter((row) => row.virtual_2m_enabled).length, tone: 'success' },
  ];

  const { run: create, running: creating } = useAsyncAction(async (values) => {
    try { await post('/admin/groups', values); Toast.success('已创建'); setOpen(false); await load(); }
    catch (e) { showErrorToast(e); }
  });

  const { run: remove, running: removing, isRunning: isRemoving } = useKeyedAsyncAction(async (name) => {
    try { await del(`/admin/groups/${encodeURIComponent(name)}`); Toast.success('已删除'); await load(); }
    catch (e) { showErrorToast(e); }
  });

  const columns = [
    {
      title: '分组',
      dataIndex: 'name',
      width: 260,
      render: (_, r) => (
        <div className="pool-resource-summary">
          <TextClamp strong>{r.name || '默认分组'}</TextClamp>
          <div className="pool-resource-summary__meta">账号策略按分组继承，可被账号或 Key 覆盖</div>
        </div>
      ),
    },
    {
      title: '策略',
      key: 'policy',
      width: 300,
      render: (_, r) => (
        <TagList
          items={groupPolicyTags(r)}
          max={4}
          renderItem={(item) => <Tag key={item.label} size="small" color={item.color}>{item.label}</Tag>}
        />
      ),
    },
    {
      title: '模型',
      dataIndex: 'force_model',
      width: 180,
      render: (v) => <TextClamp muted={!v}>{v || '继承默认'}</TextClamp>,
    },
    {
      title: '操作',
      key: 'ops',
      width: 116,
      render: (_, r) => (
        <ActionGroup minWidth={80}>
          <Popconfirm title={`删除分组 ${r.name}?`} onConfirm={() => remove(r.name)}>
            <Button size="small" type="danger" loading={isRemoving(r.name)} disabled={creating || (removing && !isRemoving(r.name))}>删除</Button>
          </Popconfirm>
        </ActionGroup>
    ) },
  ];

  return (
    <div>
      <PageHeader title="分组" subtitle="按分组下发强制模型 / 推理强度 / 虚拟上下文"
        actions={<>
          <Button icon={<IconRefresh />} onClick={load}>刷新</Button>
          <Button icon={<IconPlus />} theme="solid" disabled={removing} onClick={() => setOpen(true)}>新建分组</Button>
        </>} />
      <div className="pool-resource-split">
        <ResourceTable
          error={error}
          onRetry={load}
          loading={loading}
          lastRefresh={lastRefresh}
          dataSource={rows}
          columns={columns}
          rowKey="name"
          pagination={false}
          density="compact"
          layout="fit"
          className="pool-groups-table"
          scroll={false}
          rowHeight={68}
          emptyTitle="暂无分组"
          emptyType="groups"
          skeletonRows={5}
        />
        <MetricRail items={groupMetrics} />
      </div>
      <Modal title="新建分组" visible={open} onCancel={() => { if (!creating) setOpen(false); }} footer={null} maskClosable={!creating}>
        <Form onSubmit={create}>
          <Form.Input field="name" label="分组名" rules={[{ required: true }]} />
          <Form.Input field="force_model" label="强制模型 (可选)" />
          <Form.Select field="force_effort" label="强制 effort (可选)" optionList={['', 'minimal', 'low', 'medium', 'high', 'xhigh'].map((x) => ({ label: x || '不强制', value: x }))} />
          <Button htmlType="submit" theme="solid" loading={creating} style={{ marginTop: 12 }}>创建</Button>
        </Form>
      </Modal>
    </div>
  );
}
