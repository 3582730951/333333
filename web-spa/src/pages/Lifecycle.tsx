import React, { useRef, useState } from 'react';
import * as PoolUI from '../components/pool/index.jsx';
import { IconDelete, IconFile, IconRefresh, IconPlus } from '../components/pool/icons.jsx';
import LoadErrorBanner from '../components/LoadErrorBanner.jsx';
import PageHeader from '../components/PageHeader.jsx';
import ResourceTable from '../components/ResourceTable.jsx';
import { LogStream, ServiceHealthStrip, TaskDetailDrawer, TaskProgress } from '../components/WorkflowPrimitives.jsx';
import { showErrorToast } from '../components/ErrorToast.jsx';
import useLifecycleTaskLogs from '../hooks/useLifecycleTaskLogs.js';
import { fmtDateTime } from '../lib/format.js';
import { t } from '../lib/i18n.js';
import {
  useCancelLifecycleTaskMutation, useCreateLifecycleTaskMutation,
  useLifecycleDashboardData, useLifecycleOptionsData,
} from '../features/automation/queries/lifecycle';
import type { LifecycleTask, LifecycleTaskCreateInput } from '../features/automation/model/lifecycle';

const { ActionMenu, Button, Modal, Form, Toast, Tag } = PoolUI as any;
const ErrorBanner = LoadErrorBanner as any;
const DataTable = ResourceTable as any;
const HealthStrip = ServiceHealthStrip as any;
const Progress = TaskProgress as any;
const DetailDrawer = TaskDetailDrawer as any;
const Logs = LogStream as any;

interface LifecycleFormValues {
  task_type?: string;
  platform?: string;
  target_count?: number | string;
  group_name?: string;
  concurrency?: number | string;
  egress_id?: string;
  sms_provider?: string;
  mailbox_provider?: string;
  payment_method?: string;
  password?: string;
}

interface LegacyFormApi {
  validate: () => Promise<LifecycleFormValues>;
}

function hasFormErrors(error: unknown): boolean {
  return Boolean(error && typeof error === 'object' && 'errorFields' in error);
}

function statusTag(status: unknown) {
  const value = String(status || 'unknown');
  const colors: Record<string, string> = {
    completed: 'green', running: 'blue', pending: 'grey', cancelled: 'amber', failed: 'red',
    stopped: 'grey', starting: 'blue', stopping: 'amber',
  };
  return <Tag color={colors[value] || 'grey'}>{value}</Tag>;
}

function taskInput(values: LifecycleFormValues): LifecycleTaskCreateInput {
  return {
    task_type: values.task_type || 'register',
    platform: values.platform || 'chatgpt',
    target_count: Number(values.target_count) || 1,
    group_name: values.group_name || '',
    concurrency: Number(values.concurrency) || 1,
    egress_id: values.egress_id || '',
    sms_provider: values.sms_provider || '',
    mailbox_provider: values.mailbox_provider || '',
    payment_method: values.payment_method || '',
    password: values.password || '',
  };
}

export default function Lifecycle() {
  const [createOpen, setCreateOpen] = useState(false);
  const [logTask, setLogTask] = useState<LifecycleTask | null>(null);
  const formApi = useRef<LegacyFormApi | null>(null);
  const {
    logs,
    error: logError,
    streaming: logStreaming,
    reload: reloadLogs,
  } = useLifecycleTaskLogs(logTask?.id || '');

  const lifecycleQuery = useLifecycleDashboardData();
  const optionsQuery = useLifecycleOptionsData();
  const createMutation = useCreateLifecycleTaskMutation();
  const cancelMutation = useCancelLifecycleTaskMutation();

  const tasks = lifecycleQuery.data?.tasks ?? [];
  const services = lifecycleQuery.data?.services ?? [];
  const groups = optionsQuery.data?.groups ?? [];
  const egresses = optionsQuery.data?.egresses ?? [];
  const providerOptions = optionsQuery.data?.providers ?? { sms: [], mailbox: [], captcha: [] };
  const creating = createMutation.isPending;
  const cancelling = cancelMutation.isPending;
  const isCancelling = (id: string) => cancelling && cancelMutation.variables === id;

  const create = async () => {
    try {
      if (!formApi.current) return;
      const values = await formApi.current.validate();
      await createMutation.mutateAsync(taskInput(values));
      Toast.success(t('lifecycle.created'));
      setCreateOpen(false);
    } catch (error) {
      if (hasFormErrors(error)) return;
      showErrorToast(error);
    }
  };

  const cancel = async (id: string) => {
    try {
      await cancelMutation.mutateAsync(id);
      Toast.success(t('lifecycle.cancelled'));
    } catch (error) {
      showErrorToast(error);
    }
  };

  const openLogs = (task: LifecycleTask) => {
    if (logTask?.id === task.id) reloadLogs();
    setLogTask(task);
  };

  const columns: any[] = [
    { title: t('lifecycle.task'), dataIndex: 'id', render: (value: string) => <span className="pool-mono">{value}</span> },
    { title: t('lifecycle.type'), dataIndex: 'task_type', render: (value: string) => <Tag>{value}</Tag> },
    { title: t('lifecycle.platform'), dataIndex: 'platform' },
    { title: t('lifecycle.group'), dataIndex: 'group_name', render: (value: string | undefined) => value || '—' },
    { title: t('lifecycle.egress'), dataIndex: 'egress_id', render: (value: string | undefined) => value ? <Tag size="small">{value}</Tag> : '—' },
    { title: t('lifecycle.status'), dataIndex: 'status', render: statusTag },
    {
      title: t('lifecycle.progress'),
      render: (_: unknown, row: LifecycleTask) => (
        <Progress
          task={row}
          totalKey="target_count"
          completedKey="completed_count"
          successKey="success_count"
          failedKey="failed_count"
        />
      ),
    },
    { title: t('lifecycle.success_failed'), render: (_: unknown, row: LifecycleTask) => <span><span className="pool-success-text">{row.success_count || 0}</span> / <span className="pool-danger-text">{row.failed_count || 0}</span></span> },
    { title: t('lifecycle.created_at'), dataIndex: 'created_at', render: fmtDateTime },
    {
      title: t('lifecycle.operations'), render: (_: unknown, row: LifecycleTask) => (
        <ActionMenu
          label={t('lifecycle.task_actions')}
          items={[
            { label: t('lifecycle.details_logs'), icon: <IconFile />, onSelect: () => openLogs(row) },
            {
              label: isCancelling(row.id) ? t('lifecycle.cancelling') : t('lifecycle.cancel_task'),
              icon: <IconDelete />,
              destructive: true,
              disabled: !['pending', 'running'].includes(row.status || '') || creating || (cancelling && !isCancelling(row.id)),
              confirm: {
                title: t('lifecycle.cancel_title'),
                description: t('lifecycle.cancel_desc').replace('{id}', row.id),
                confirmText: t('lifecycle.cancel_task'),
              },
              onSelect: () => cancel(row.id),
            },
          ]}
        />
      ),
    },
  ];

  return (
    <div>
      <PageHeader title={t('lifecycle.title')} subtitle={t('lifecycle.subtitle')}
        actions={<>
          <Button icon={<IconPlus />} theme="solid" disabled={cancelling} onClick={() => setCreateOpen(true)}>{t('lifecycle.new')}</Button>
          <Button icon={<IconRefresh />} onClick={lifecycleQuery.reload}>{t('common.refresh')}</Button>
        </>} />

      <ErrorBanner error={lifecycleQuery.data?.serviceError} onRetry={lifecycleQuery.reload} title={t('lifecycle.services_failed')} />
      <ErrorBanner error={optionsQuery.error || optionsQuery.data?.error} onRetry={optionsQuery.reload} title={t('lifecycle.options_failed')} />

      <HealthStrip services={services} renderStatus={statusTag} />

      <DataTable
        error={lifecycleQuery.error}
        onRetry={lifecycleQuery.reload}
        loading={lifecycleQuery.loading}
        lastRefresh={lifecycleQuery.lastRefresh}
        dataSource={tasks}
        columns={columns}
        rowKey="id"
        pagination={{ pageSize: 15 }}
        emptyTitle={t('lifecycle.empty')}
        skeletonRows={6}
        skeletonCols={8}
        onRow={(row: LifecycleTask) => ({ onClick: () => openLogs(row) })}
      />

      <Modal title={t('lifecycle.modal_title')} visible={createOpen} onCancel={() => { if (!creating) setCreateOpen(false); }} onOk={create} confirmLoading={creating} okText={t('common.create')} width={560} maskClosable={!creating}>
        <Form getFormApi={(api: LegacyFormApi) => { formApi.current = api; }} labelPosition="left" labelWidth={110}>
          <Form.Select field="task_type" label={t('lifecycle.task_type')} initValue="register" rules={[{ required: true }]}
            optionList={[{ label: t('lifecycle.register'), value: 'register' }, { label: t('lifecycle.upgrade_plus'), value: 'upgrade_plus' }, { label: t('lifecycle.register_and_plus'), value: 'register_and_plus' }]} />
          <Form.Select field="platform" label={t('lifecycle.platform')} initValue="chatgpt" optionList={[{ label: 'ChatGPT', value: 'chatgpt' }, { label: 'Claude', value: 'claude' }]} />
          <Form.InputNumber field="target_count" label={t('lifecycle.target_count')} initValue={1} min={1} max={500} />
          <Form.InputNumber field="concurrency" label={t('lifecycle.concurrency')} initValue={1} min={1} max={16} />
          <Form.Select field="group_name" label={t('lifecycle.group')} placeholder={t('lifecycle.default')}
            optionList={[{ label: t('lifecycle.default'), value: '' }, ...groups.map((group) => ({ label: group.name, value: group.name }))]} />
          <Form.Select field="egress_id" label={t('lifecycle.egress')} placeholder={t('lifecycle.default')}
            optionList={[
              { label: `(${t('lifecycle.default')})`, value: '' },
              ...egresses.map((egress) => ({
                label: `${egress.name || egress.id} (${egress.type || 'direct'})`,
                value: egress.id,
              })),
            ]} />
          <Form.Select field="sms_provider" label={t('lifecycle.sms_provider')} placeholder={t('lifecycle.optional_default')}
            optionList={[{ label: `(${t('lifecycle.default')})`, value: '' }, ...providerOptions.sms]} />
          <Form.Select field="mailbox_provider" label={t('lifecycle.mailbox_provider')} placeholder={t('users.optional')}
            optionList={[{ label: `(${t('lifecycle.default')})`, value: '' }, ...providerOptions.mailbox]} />
          <Form.Select field="payment_method" label={t('lifecycle.payment_method')} placeholder={t('lifecycle.plus_only')} optionList={[{ label: t('lifecycle.none'), value: '' }, { label: 'GoPay', value: 'gopay' }, { label: 'PayPal', value: 'paypal' }]} />
          <Form.Input field="password" label={t('lifecycle.password')} mode="password" placeholder={t('lifecycle.password_hint')} />
        </Form>
      </Modal>

      <DetailDrawer
        task={logTask}
        visible={!!logTask}
        onClose={() => setLogTask(null)}
        title={logTask ? `${t('lifecycle.drawer_title')} · ${logTask.id}` : t('lifecycle.drawer_title')}
        status={logTask ? statusTag(logTask.status) : null}
      >
        <Logs logs={logs} streaming={logStreaming} error={logError} onRetry={logTask ? reloadLogs : undefined} />
      </DetailDrawer>
    </div>
  );
}
