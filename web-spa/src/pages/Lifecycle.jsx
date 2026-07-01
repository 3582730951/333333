import React, { useState, useCallback, useRef } from 'react';
import { Button, Modal, Form, Toast, Tag, Progress, SideSheet, Typography, Popconfirm } from '@douyinfe/semi-ui';
import { IconRefresh, IconPlus } from '@douyinfe/semi-icons';
import { get, post, del } from '../api.js';
import LoadErrorBanner from '../components/LoadErrorBanner.jsx';
import PageHeader from '../components/PageHeader.jsx';
import ResourceTable from '../components/ResourceTable.jsx';
import { showErrorToast } from '../components/ErrorToast.jsx';
import useAsyncAction from '../hooks/useAsyncAction.js';
import useKeyedAsyncAction from '../hooks/useKeyedAsyncAction.js';
import useAsyncResource from '../hooks/useAsyncResource.js';
import useLifecycleTaskLogs, { lifecycleLogKey } from '../hooks/useLifecycleTaskLogs.js';
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
      title: '进度', render: (_, r) => {
        const pct = r.target_count ? Math.round(((r.completed_count || 0) / r.target_count) * 100) : 0;
        return <div style={{ minWidth: 120 }}><Progress percent={pct} showInfo size="small" /></div>;
      },
    },
    { title: '成功 / 失败', render: (_, r) => <span><span style={{ color: 'var(--semi-color-success)' }}>{r.success_count || 0}</span> / <span style={{ color: 'var(--semi-color-danger)' }}>{r.failed_count || 0}</span></span> },
    { title: '创建时间', dataIndex: 'created_at', render: fmtDateTime },
    {
      title: '操作', render: (_, r) => (
        <div style={{ display: 'flex', gap: 6 }}>
          <Button size="small" onClick={() => openLogs(r)}>日志</Button>
          {(r.status === 'pending' || r.status === 'running') && (
            <Popconfirm title="取消该任务？" onConfirm={() => cancel(r.id)}>
              <Button size="small" type="danger" loading={isCancelling(r.id)} disabled={creating || (cancelling && !isCancelling(r.id))}>取消</Button>
            </Popconfirm>
          )}
        </div>
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

      {services.length > 0 && (
        <div style={{ marginBottom: 12, padding: '10px 12px', border: '1px solid var(--semi-color-border)', borderRadius: 6, background: 'var(--semi-color-fill-0)' }}>
          <Typography.Text strong style={{ marginRight: 12 }}>外部服务</Typography.Text>
          {services.map((svc) => (
            <span key={svc.name} style={{ display: 'inline-flex', alignItems: 'center', gap: 6, marginRight: 14, marginBottom: 4 }}>
              <span>{svc.name}</span>
              {statusTag(svc.status)}
              {svc.last_error ? <Typography.Text type="danger" size="small">{svc.last_error}</Typography.Text> : null}
            </span>
          ))}
        </div>
      )}

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

      <SideSheet
        title={logTask ? `任务日志 · ${logTask.id}` : '任务日志'}
        visible={!!logTask}
        onCancel={() => setLogTask(null)}
        width={560}
      >
        <LoadErrorBanner error={logError} onRetry={logTask ? reloadLogs : undefined} title="任务日志读取失败" />
        {logTask ? (
          <div style={{ marginBottom: 10 }}>
            <Tag color={logStreaming ? 'green' : 'grey'}>{logStreaming ? '实时连接中' : '实时连接断开'}</Tag>
          </div>
        ) : null}
        {!logs.length && <Typography.Text type="tertiary">暂无日志</Typography.Text>}
        <div className="pool-mono" style={{ whiteSpace: 'pre-wrap', lineHeight: 1.7 }}>
          {logs.map((l, i) => (
            <div key={lifecycleLogKey(l, i)}>
              <span className="pool-muted">[{fmtDateTime(l.timestamp)}]</span> <Tag size="small" color={l.level === 'error' ? 'red' : 'grey'}>{l.level || 'info'}</Tag> {l.message}
            </div>
          ))}
        </div>
      </SideSheet>
    </div>
  );
}
