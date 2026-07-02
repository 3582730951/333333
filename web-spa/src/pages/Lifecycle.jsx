import React, { useState, useCallback, useRef } from 'react';
import { ActionMenu, Button, Modal, Form, Toast, Tag } from '../components/pool/index.jsx';
import { IconDelete, IconFile, IconRefresh, IconPlus } from '../components/pool/icons.jsx';
import { get, post, del } from '../api.js';
import LoadErrorBanner from '../components/LoadErrorBanner.jsx';
import PageHeader from '../components/PageHeader.jsx';
import ResourceTable from '../components/ResourceTable.jsx';
import { LogStream, ServiceHealthStrip, TaskDetailDrawer, TaskProgress } from '../components/WorkflowPrimitives.jsx';
import { showErrorToast } from '../components/ErrorToast.jsx';
import useAsyncAction from '../hooks/useAsyncAction.js';
import useKeyedAsyncAction from '../hooks/useKeyedAsyncAction.js';
import useAsyncResource from '../hooks/useAsyncResource.js';
import useLifecycleTaskLogs from '../hooks/useLifecycleTaskLogs.js';
import useVisibleInterval from '../hooks/useVisibleInterval.js';
import { fmtDateTime } from '../lib/format.js';
import { loadResourceGroup } from '../lib/resource.js';

const statusTag = (s) => {
  const map = {
    completed: 'green', running: 'blue', pending: 'grey', cancelled: 'amber', failed: 'red',
    stopped: 'grey', starting: 'blue', stopping: 'amber',
  };
  return <Tag color={map[s] || 'grey'}>{s}</Tag>;
};

const EMPTY_LIFECYCLE = { tasks: [], services: [], error: null };
const EMPTY_OPTIONS = { groups: [], egresses: [], providerOpts: { sms: [], mailbox: [], captcha: [] }, error: null };

export default function Lifecycle() {
  const [createOpen, setCreateOpen] = useState(false);
  const [logTask, setLogTask] = useState(null);
  const formApi = useRef(null);
  const {
    logs,
    error: logError,
    streaming: logStreaming,
    reload: reloadLogs,
  } = useLifecycleTaskLogs(logTask?.id || '');

  const fetchLifecycle = useCallback(async ({ signal }) => {
    const { values, error } = await loadResourceGroup({
      tasks: { label: '任务列表', load: () => get('/admin/lifecycle/tasks', undefined, { signal }) },
      services: { label: '外部服务状态', load: () => get('/admin/lifecycle/services', undefined, { signal }) },
    });
    if (error?.failures?.some((failure) => failure.key === 'tasks')) throw error;
    const serviceRows = values.services && typeof values.services === 'object' ? Object.values(values.services) : [];
    return {
      tasks: Array.isArray(values.tasks) ? values.tasks : values.tasks?.tasks || [],
      services: serviceRows.filter(Boolean),
      error,
    };
  }, []);

  const {
    data: lifecycle = EMPTY_LIFECYCLE,
    loading,
    error,
    lastRefresh,
    reload: load,
  } = useAsyncResource(fetchLifecycle, [fetchLifecycle], { initialData: EMPTY_LIFECYCLE });

  useVisibleInterval(load, 5000);

  const fetchOptions = useCallback(async ({ signal }) => {
    const { values, error } = await loadResourceGroup({
      groups: { label: '分组选项', load: () => get('/admin/groups', undefined, { signal }) },
      egresses: { label: '出口选项', load: () => get('/admin/egress-profiles', undefined, { signal }) },
      providers: { label: '服务商选项', load: () => get('/admin/register/providers/options', undefined, { signal }) },
    });
    return {
      groups: Array.isArray(values.groups) ? values.groups : values.groups?.groups || [],
      egresses: Array.isArray(values.egresses) ? values.egresses : values.egresses?.profiles || [],
      providerOpts: values.providers || EMPTY_OPTIONS.providerOpts,
      error,
    };
  }, []);

  const {
    data: options = EMPTY_OPTIONS,
    error: optionsError,
    reload: reloadOptions,
  } = useAsyncResource(fetchOptions, [fetchOptions], { initialData: EMPTY_OPTIONS });

  const tasks = lifecycle.tasks || [];
  const services = lifecycle.services || [];
  const groups = options.groups || [];
  const egresses = options.egresses || [];
  const providerOpts = options.providerOpts || EMPTY_OPTIONS.providerOpts;

  const { run: create, running: creating } = useAsyncAction(async () => {
    try {
      const v = await formApi.current.validate();
      await post('/admin/lifecycle/tasks', {
        task_type: v.task_type, platform: v.platform || 'chatgpt',
        target_count: Number(v.target_count) || 1, group_name: v.group_name || '',
        concurrency: Number(v.concurrency) || 1,
        egress_id: v.egress_id || '',
        sms_provider: v.sms_provider || '', mailbox_provider: v.mailbox_provider || '',
        payment_method: v.payment_method || '', password: v.password || '',
      });
      Toast.success('任务已创建');
      setCreateOpen(false);
      await load();
    } catch (e) {
      if (e?.errorFields) return;
      showErrorToast(e);
    }
  });

  const { run: cancel, running: cancelling, isRunning: isCancelling } = useKeyedAsyncAction(async (id) => {
    try { await del(`/admin/lifecycle/tasks/${id}`); Toast.success('已取消'); await load(); }
    catch (e) { showErrorToast(e); }
  });

  const openLogs = (task) => {
    if (logTask?.id === task.id) reloadLogs();
    setLogTask(task);
  };

  const cols = [
    { title: '任务', dataIndex: 'id', render: (v) => <span className="pool-mono">{v}</span> },
    { title: '类型', dataIndex: 'task_type', render: (v) => <Tag>{v}</Tag> },
    { title: '平台', dataIndex: 'platform' },
    { title: '分组', dataIndex: 'group_name', render: (v) => v || '—' },
    { title: '出口', dataIndex: 'egress_id', render: (v) => v ? <Tag size="small">{v}</Tag> : '—' },
    { title: '状态', dataIndex: 'status', render: statusTag },
    {
      title: '进度',
      render: (_, r) => (
        <TaskProgress
          task={r}
          totalKey="target_count"
          completedKey="completed_count"
          successKey="success_count"
          failedKey="failed_count"
        />
      ),
    },
    { title: '成功 / 失败', render: (_, r) => <span><span className="pool-success-text">{r.success_count || 0}</span> / <span className="pool-danger-text">{r.failed_count || 0}</span></span> },
    { title: '创建时间', dataIndex: 'created_at', render: fmtDateTime },
    {
      title: '操作', render: (_, r) => (
        <ActionMenu
          label="任务操作"
          items={[
            { label: '详情 / 日志', icon: <IconFile />, onSelect: () => openLogs(r) },
            {
              label: isCancelling(r.id) ? '取消中' : '取消任务',
              icon: <IconDelete />,
              destructive: true,
              disabled: !['pending', 'running'].includes(r.status) || creating || (cancelling && !isCancelling(r.id)),
              confirm: {
                title: '取消该任务？',
                description: `任务 ${r.id} 将被取消，已完成的结果不会回滚。`,
                confirmText: '取消任务',
              },
              onSelect: () => cancel(r.id),
            },
          ]}
        />
      ),
    },
  ];

  return (
    <div>
      <PageHeader title="生命周期任务" subtitle="批量注册 / 升级 Plus 的编排任务（每 5 秒刷新）"
        actions={<>
          <Button icon={<IconPlus />} theme="solid" onClick={() => setCreateOpen(true)}>新建任务</Button>
          <Button icon={<IconRefresh />} onClick={load}>刷新</Button>
        </>} />

      <LoadErrorBanner error={error || lifecycle.error} onRetry={load} />
      <LoadErrorBanner error={optionsError || options.error} onRetry={reloadOptions} title="表单选项读取失败" />

      <ServiceHealthStrip services={services} renderStatus={statusTag} />

      <ResourceTable
        loading={loading}
        lastRefresh={lastRefresh}
        dataSource={tasks}
        columns={cols}
        rowKey="id"
        pagination={{ pageSize: 15 }}
        emptyTitle="暂无任务"
        skeletonRows={6}
        skeletonCols={8}
        onRow={(row) => ({ onClick: () => openLogs(row) })}
      />

      <Modal title="新建生命周期任务" visible={createOpen} onCancel={() => setCreateOpen(false)} onOk={create} confirmLoading={creating} okText="创建" width={560}>
        <Form getFormApi={(a) => { formApi.current = a; }} labelPosition="left" labelWidth={110}>
          <Form.Select field="task_type" label="任务类型" initValue="register" rules={[{ required: true }]}
            optionList={[{ label: '注册账号', value: 'register' }, { label: '升级 Plus', value: 'upgrade_plus' }, { label: '注册并升级', value: 'register_and_plus' }]} />
          <Form.Select field="platform" label="平台" initValue="chatgpt" optionList={[{ label: 'ChatGPT', value: 'chatgpt' }, { label: 'Claude', value: 'claude' }]} />
          <Form.InputNumber field="target_count" label="目标数量" initValue={1} min={1} max={500} />
          <Form.InputNumber field="concurrency" label="并发" initValue={1} min={1} max={16} />
          <Form.Select field="group_name" label="分组" placeholder="默认"
            optionList={[{ label: '默认', value: '' }, ...groups.map((g) => ({ label: g.name, value: g.name }))]} />
          <Form.Select field="egress_id" label="出口" placeholder="默认"
            optionList={[
              { label: '(默认)', value: '' },
              ...(egresses || []).filter((e) => e && e.id).map((e) => ({
                label: `${e.name || e.id} (${e.type || 'direct'})`,
                value: e.id,
              })),
            ]} />
          <Form.Select field="sms_provider" label="短信提供商" placeholder="可选，留空用默认"
            optionList={[{ label: '(默认)', value: '' }, ...(providerOpts.sms || [])]} />
          <Form.Select field="mailbox_provider" label="邮箱提供商" placeholder="可选"
            optionList={[{ label: '(默认)', value: '' }, ...(providerOpts.mailbox || [])]} />
          <Form.Select field="payment_method" label="支付方式" placeholder="升级 Plus 时" optionList={[{ label: '无', value: '' }, { label: 'GoPay', value: 'gopay' }, { label: 'PayPal', value: 'paypal' }]} />
          <Form.Input field="password" label="账号密码" mode="password" placeholder="可选，注册账号统一密码" />
        </Form>
      </Modal>

      <TaskDetailDrawer
        task={logTask}
        visible={!!logTask}
        onClose={() => setLogTask(null)}
        title={logTask ? `生命周期任务 · ${logTask.id}` : '生命周期任务'}
        status={logTask ? statusTag(logTask.status) : null}
      >
        <LogStream logs={logs} streaming={logStreaming} error={logError} onRetry={logTask ? reloadLogs : undefined} />
      </TaskDetailDrawer>
    </div>
  );
}
