import React, { useState, useRef } from 'react';
import * as PoolUI from '../components/pool/index.jsx';
import { IconDelete, IconEdit, IconRefresh, IconPlus } from '../components/pool/icons.jsx';
import PageHeader from '../components/PageHeader.jsx';
import ResourceTable from '../components/ResourceTable.jsx';
import MobileResourceCell from '../components/MobileResourceCell.jsx';
import { MetricRail } from '../components/DisplayPrimitives.jsx';
import { showErrorToast } from '../components/ErrorToast.jsx';
import { fmtDateTime } from '../lib/format.js';
import { t } from '../lib/i18n.js';
import type { UserRow, UserUpdateInput } from '../features/access/model/users';
import {
  useCreateUserMutation, useDeleteUserMutation, useUpdateUserMutation, useUsersData,
} from '../features/access/queries/users.ts';

const { ActionMenu, Button, Modal, Form, Toast, Tag } = PoolUI as any;
const DataTable = ResourceTable as any;
const MobileRow = MobileResourceCell as any;
const SummaryRail = MetricRail as any;

type EditState = { mode: 'create'; user?: undefined } | { mode: 'edit'; user: UserRow };
interface UserFormValues {
  email: string;
  name?: string;
  role?: string;
  status?: string;
  password?: string;
}
interface LegacyFormApi {
  validate: () => Promise<UserFormValues>;
}

function hasFormErrors(error: unknown): boolean {
  return Boolean(error && typeof error === 'object' && 'errorFields' in error);
}

export default function Users() {
  const [edit, setEdit] = useState<EditState | null>(null);
  const formApi = useRef<LegacyFormApi | null>(null);

  const {
    data: rows = [],
    loading,
    error,
    lastRefresh,
    reload: load,
  } = useUsersData();
  const userMetrics = [
    { label: t('users.total'), value: rows.length },
    { label: t('users.admins'), value: rows.filter((row) => row.role === 'admin').length },
    { label: t('users.active'), value: rows.filter((row) => row.status === 'active').length, tone: 'success' },
    { label: t('users.disabled'), value: rows.filter((row) => row.status && row.status !== 'active').length, tone: rows.some((row) => row.status && row.status !== 'active') ? 'warning' : undefined },
  ];

  const createMutation = useCreateUserMutation();
  const updateMutation = useUpdateUserMutation();
  const deleteMutation = useDeleteUserMutation();
  const saving = createMutation.isPending || updateMutation.isPending;
  const removing = deleteMutation.isPending;
  const isRemoving = (id: string) => removing && deleteMutation.variables === id;

  const save = async () => {
    try {
      if (!formApi.current || !edit) return;
      const v = await formApi.current.validate();
      if (edit.mode === 'create') {
        await createMutation.mutateAsync({ email: v.email, name: v.name || '', role: v.role || 'user', status: v.status || 'active', password: v.password || '' });
        Toast.success(t('users.created'));
      } else {
        const body: UserUpdateInput['values'] = { role: v.role, status: v.status, name: v.name };
        if (v.password) body.password = v.password;
        await updateMutation.mutateAsync({ id: edit.user.id, values: body });
        Toast.success(t('users.updated'));
      }
      setEdit(null);
    } catch (e) {
      if (hasFormErrors(e)) return;
      showErrorToast(e);
    }
  };

  const remove = async (id: string) => {
    try { await deleteMutation.mutateAsync(id); Toast.success(t('users.deleted')); }
    catch (e) { showErrorToast(e); }
  };

  const renderUserActions = (r: UserRow) => (
    <ActionMenu
      label={t('users.actions')}
      items={[
        {
          label: t('users.edit'),
          icon: <IconEdit />,
          disabled: saving || removing,
          onSelect: () => setEdit({ mode: 'edit', user: r }),
        },
        {
          label: isRemoving(r.id) ? t('users.deleting') : t('users.delete'),
          icon: <IconDelete />,
          destructive: true,
          disabled: saving || (removing && !isRemoving(r.id)),
          confirm: {
            title: t('users.delete_title').replace('{email}', r.email),
            description: t('users.delete_desc'),
            confirmText: t('common.delete'),
          },
          onSelect: () => remove(r.id),
        },
      ]}
    />
  );

  const cols: any[] = [
    { title: t('users.email'), dataIndex: 'email', width: 240, render: (v: string) => <b>{v}</b> },
    { title: t('users.name'), dataIndex: 'name', width: 150, render: (v: string) => v || '—' },
    { title: t('users.role'), dataIndex: 'role', width: 96, sorter: (a: UserRow, b: UserRow) => String(a.role || '').localeCompare(String(b.role || '')), render: (v: string) => <Tag color={v === 'admin' ? 'violet' : 'blue'}>{v}</Tag> },
    { title: t('users.status'), dataIndex: 'status', width: 96, sorter: (a: UserRow, b: UserRow) => String(a.status || '').localeCompare(String(b.status || '')), render: (v: string) => <Tag color={v === 'active' ? 'green' : 'grey'}>{v}</Tag> },
    { title: t('users.created_at'), dataIndex: 'created_at', width: 180, sorter: (a: UserRow, b: UserRow) => (a.created_at || 0) - (b.created_at || 0), defaultSortOrder: 'descend', render: fmtDateTime },
    {
      title: t('users.operations'), width: 90, render: (_: unknown, r: UserRow) => (
        renderUserActions(r)
      ),
    },
  ];
  const mobileColumns: any[] = [
    {
      title: t('users.user'),
      dataIndex: 'email',
      render: (_: unknown, r: UserRow) => (
        <MobileRow
          title={r.email}
          subtitle={r.name || t('users.name_unset')}
          badges={<><Tag color={r.role === 'admin' ? 'violet' : 'blue'}>{r.role}</Tag><Tag color={r.status === 'active' ? 'green' : 'grey'}>{r.status}</Tag></>}
          details={[
            { label: t('users.created_short'), value: fmtDateTime(r.created_at) },
          ]}
          actions={renderUserActions(r)}
        />
      ),
    },
  ];

  const u = edit?.mode === 'edit' ? edit.user : undefined;
  return (
    <div>
      <PageHeader title={t('users.title')} subtitle={t('users.subtitle')}
        actions={<>
          <Button icon={<IconPlus />} theme="solid" disabled={removing} onClick={() => setEdit({ mode: 'create' })}>{t('users.new')}</Button>
          <Button icon={<IconRefresh />} onClick={load}>{t('common.refresh')}</Button>
        </>} />

      <div className="pool-resource-split">
        <DataTable
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
          emptyTitle={t('users.empty')}
          emptyType="users"
          skeletonRows={6}
        />
        {!error || lastRefresh ? <SummaryRail items={userMetrics} /> : null}
      </div>

      <Modal title={edit?.mode === 'create' ? t('users.new') : t('users.edit_title')} visible={!!edit} onCancel={() => { if (!saving) setEdit(null); }} onOk={save} confirmLoading={saving} okText={t('common.save')} maskClosable={!saving}>
        {edit && (
          <Form getFormApi={(a: LegacyFormApi) => { formApi.current = a; }} labelPosition="left" labelWidth={90}
            initValues={{ email: u?.email, name: u?.name, role: u?.role || 'user', status: u?.status || 'active' }}>
            <Form.Input field="email" label={t('users.email')} disabled={edit.mode === 'edit'} rules={edit.mode === 'create' ? [{ required: true, message: t('users.email_required') }, { type: 'email', message: t('users.email_invalid') }] : []} />
            <Form.Input field="name" label={t('users.name')} placeholder={t('users.optional')} />
            <Form.Select field="role" label={t('users.role')} optionList={[{ label: t('users.user'), value: 'user' }, { label: t('users.admins'), value: 'admin' }]} />
            <Form.Select field="status" label={t('users.status')} optionList={[{ label: t('users.active'), value: 'active' }, { label: t('users.disabled'), value: 'disabled' }]} />
            <Form.Input field="password" label={t('users.password')} mode="password" placeholder={edit.mode === 'edit' ? t('users.password_keep') : t('users.password_hint')} rules={[{ min: 8, message: t('users.password_min') }]} />
          </Form>
        )}
      </Modal>
    </div>
  );
}
