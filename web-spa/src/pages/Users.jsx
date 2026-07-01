import React, { useState, useCallback, useRef } from 'react';
import { Button, Modal, Form, Toast, Tag, Popconfirm } from '@douyinfe/semi-ui';
import { IconRefresh, IconPlus } from '@douyinfe/semi-icons';
import { get, post, patch, del } from '../api.js';
import PageHeader from '../components/PageHeader.jsx';
import ResourceTable from '../components/ResourceTable.jsx';
import MobileResourceCell from '../components/MobileResourceCell.jsx';
import { MetricRail } from '../components/DisplayPrimitives.jsx';
import { showErrorToast } from '../components/ErrorToast.jsx';
import useAsyncAction from '../hooks/useAsyncAction.js';
import useKeyedAsyncAction from '../hooks/useKeyedAsyncAction.js';
import useAsyncResource from '../hooks/useAsyncResource.js';
import { fmtDateTime } from '../lib/format.js';

export default function Users() {
  const [edit, setEdit] = useState(null); // {mode:'create'|'edit', user}
  const formApi = useRef(null);

  const fetchRows = useCallback(async ({ signal }) => {
    const d = await get('/admin/users', undefined, { signal });
    return Array.isArray(d) ? d : d?.users || [];
  }, []);
  const {
    data: rows = [],
    loading,
    error,
    lastRefresh,
    reload: load,
  } = useAsyncResource(fetchRows, [fetchRows], { initialData: [] });
  const userMetrics = [
    { label: '用户数', value: rows.length },
    { label: '管理员', value: rows.filter((row) => row.role === 'admin').length },
    { label: '启用', value: rows.filter((row) => row.status === 'active').length, tone: 'success' },
    { label: '禁用', value: rows.filter((row) => row.status && row.status !== 'active').length, tone: rows.some((row) => row.status && row.status !== 'active') ? 'warning' : undefined },
  ];

  const { run: save, running: saving } = useAsyncAction(async () => {
    try {
      const v = await formApi.current.validate();
      if (edit.mode === 'create') {
        await post('/admin/users', { email: v.email, name: v.name || '', role: v.role || 'user', status: v.status || 'active', password: v.password || '' });
        Toast.success('用户已创建');
      } else {
        const body = { role: v.role, status: v.status, name: v.name };
        if (v.password) body.password = v.password;
        await patch(`/admin/users/${edit.user.id}`, body);
        Toast.success('已更新');
      }
      setEdit(null);
      await load();
    } catch (e) {
      if (e?.errorFields) return;
      showErrorToast(e);
    }
  });

  const { run: remove, running: removing, isRunning: isRemoving } = useKeyedAsyncAction(async (id) => {
    try { await del(`/admin/users/${id}`); Toast.success('已删除'); await load(); }
    catch (e) { showErrorToast(e); }
  });

  const renderUserActions = (r) => (
    <div className="pool-row-actions">
      <Button size="small" disabled={saving || removing} onClick={() => setEdit({ mode: 'edit', user: r })}>编辑</Button>
      <Popconfirm title={`删除用户 ${r.email}？`} onConfirm={() => remove(r.id)}>
        <Button size="small" type="danger" loading={isRemoving(r.id)} disabled={saving || (removing && !isRemoving(r.id))}>删除</Button>
      </Popconfirm>
    </div>
  );

  const cols = [
    { title: '邮箱', dataIndex: 'email', width: 240, render: (v) => <b>{v}</b> },
    { title: '名称', dataIndex: 'name', width: 150, render: (v) => v || '—' },
    { title: '角色', dataIndex: 'role', width: 96, sorter: (a, b) => String(a.role || '').localeCompare(String(b.role || '')), render: (v) => <Tag color={v === 'admin' ? 'violet' : 'blue'}>{v}</Tag> },
    { title: '状态', dataIndex: 'status', width: 96, sorter: (a, b) => String(a.status || '').localeCompare(String(b.status || '')), render: (v) => <Tag color={v === 'active' ? 'green' : 'grey'}>{v}</Tag> },
    { title: '创建时间', dataIndex: 'created_at', width: 180, sorter: (a, b) => (a.created_at || 0) - (b.created_at || 0), defaultSortOrder: 'descend', render: fmtDateTime },
    {
      title: '操作', width: 132, render: (_, r) => (
        renderUserActions(r)
      ),
    },
  ];
  const mobileColumns = [
    {
      title: '用户',
      dataIndex: 'email',
      render: (_, r) => (
        <MobileResourceCell
          title={r.email}
          subtitle={r.name || '未设置名称'}
          badges={<><Tag color={r.role === 'admin' ? 'violet' : 'blue'}>{r.role}</Tag><Tag color={r.status === 'active' ? 'green' : 'grey'}>{r.status}</Tag></>}
          details={[
            { label: '创建', value: fmtDateTime(r.created_at) },
          ]}
          actions={renderUserActions(r)}
        />
      ),
    },
  ];

  const u = edit?.user;
  return (
    <div>
      <PageHeader title="用户管理" subtitle="门户终端用户与角色"
        actions={<>
          <Button icon={<IconPlus />} theme="solid" disabled={removing} onClick={() => setEdit({ mode: 'create' })}>新建用户</Button>
          <Button icon={<IconRefresh />} onClick={load}>刷新</Button>
        </>} />

      <div className="pool-resource-split">
        <ResourceTable
          error={error}
          onRetry={load}
          loading={loading}
          lastRefresh={lastRefresh}
          dataSource={rows}
          columns={cols}
          rowKey="id"
          pagination={{ pageSize: 15 }}
          className="pool-mobile-table pool-users-table"
          layout="fit"
          mobileColumns={mobileColumns}
          mobileScroll={false}
          emptyTitle="暂无用户"
          emptyType="users"
          skeletonRows={6}
        />
        <MetricRail items={userMetrics} />
      </div>

      <Modal title={edit?.mode === 'create' ? '新建用户' : '编辑用户'} visible={!!edit} onCancel={() => { if (!saving) setEdit(null); }} onOk={save} confirmLoading={saving} okText="保存" maskClosable={!saving}>
        {edit && (
          <Form getFormApi={(a) => { formApi.current = a; }} labelPosition="left" labelWidth={90}
            initValues={{ email: u?.email, name: u?.name, role: u?.role || 'user', status: u?.status || 'active' }}>
            <Form.Input field="email" label="邮箱" disabled={edit.mode === 'edit'} rules={edit.mode === 'create' ? [{ required: true }] : []} />
            <Form.Input field="name" label="名称" placeholder="可选" />
            <Form.Select field="role" label="角色" optionList={[{ label: '用户', value: 'user' }, { label: '管理员', value: 'admin' }]} />
            <Form.Select field="status" label="状态" optionList={[{ label: '启用', value: 'active' }, { label: '禁用', value: 'disabled' }]} />
            <Form.Input field="password" label="密码" mode="password" placeholder={edit.mode === 'edit' ? '留空不修改' : '可选，≥8 位'} />
          </Form>
        )}
      </Modal>
    </div>
  );
}
