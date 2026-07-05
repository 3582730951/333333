import React, { useState, useCallback } from 'react';
import { ActionMenu, Button, Toast, Modal, Form, Tag } from '../components/pool/index.jsx';
import { IconPlus, IconRefresh, IconEdit, IconKey, IconDelete } from '../components/pool/icons.jsx';
import { get, post, del } from '../api.js';
import { rowsOf } from '../components/DataPage.jsx';
import PageHeader from '../components/PageHeader.jsx';
import ResourceTable from '../components/ResourceTable.jsx';
import MobileResourceCell from '../components/MobileResourceCell.jsx';
import { TagList, TextClamp } from '../components/DisplayPrimitives.jsx';
import { showErrorToast } from '../components/ErrorToast.jsx';
import VendorLogo from '../components/VendorLogo.jsx';
import useAsyncAction from '../hooks/useAsyncAction.js';
import useKeyedAsyncAction from '../hooks/useKeyedAsyncAction.js';
import useAsyncResource from '../hooks/useAsyncResource.js';

const splitModels = (value) => String(value || '')
  .split(/[\n,]/)
  .map((x) => x.trim())
  .filter(Boolean);

const joinModels = (models) => Array.isArray(models) ? models.join('\n') : '';

const PROTOCOL_OPTIONS = [
  { label: 'Chat Completions（函数工具，best-effort）', value: 'chat_completions' },
  { label: 'Responses 原生（保留 typed tools/未知字段）', value: 'responses' },
];

const providerFormValues = (row) => ({
  id: row?.id || '',
  name: row?.name || '',
  base_url: row?.base_url || '',
  upstream_protocol: row?.upstream_protocol || 'chat_completions',
  enabled: row?.enabled !== false,
  auto_discover_models: row?.auto_discover_models !== false,
  models_text: joinModels(row?.models),
});

export default function Providers() {
  const [editor, setEditor] = useState(null);
  const [importer, setImporter] = useState(null);

  const fetchRows = useCallback(async ({ signal }) => {
    return rowsOf(await get('/admin/providers', undefined, { signal }));
  }, []);
  const { data: rows = [], loading, error, lastRefresh, reload: load } = useAsyncResource(fetchRows, [fetchRows], { initialData: [] });

  const { run: saveProvider, running: savingProvider } = useAsyncAction(async (values) => {
    try {
      await post('/admin/providers', {
        id: values.id,
        name: values.name,
        base_url: values.base_url,
        upstream_protocol: values.upstream_protocol || 'chat_completions',
        enabled: values.enabled !== false,
        auto_discover_models: values.auto_discover_models !== false,
        models: splitModels(values.models_text),
      });
      Toast.success('已保存');
      setEditor(null);
      await load();
    } catch (e) { showErrorToast(e); }
  });

  const { run: remove, running: removingProvider, isRunning: isRemovingProvider } = useKeyedAsyncAction(async (id) => {
    try {
      await del(`/admin/providers/${encodeURIComponent(id)}`);
      Toast.success('已删除');
      await load();
    } catch (e) { showErrorToast(e); }
  });

  const { run: importKey, running: importingKey } = useAsyncAction(async (values) => {
    if (!importer?.id) return;
    try {
      await post('/admin/accounts/import-key', {
        provider_id: importer.id,
        api_key: values.api_key,
        label: values.label,
        group_name: values.group_name,
      });
      Toast.success('账号已导入');
      setImporter(null);
    } catch (e) { showErrorToast(e); }
  });
  const providerOperationRunning = savingProvider || importingKey || removingProvider;

  const renderModels = (models, row) => {
    const list = Array.isArray(models) ? models : [];
    if (!list.length) return row.auto_discover_models ? <Tag color="blue">自动发现</Tag> : '-';
    return <TagList items={list} max={3} />;
  };

  const renderProviderActions = (row) => (
    <ActionMenu
      label="提供商操作"
      items={[
        { label: '编辑', icon: <IconEdit />, disabled: providerOperationRunning, onSelect: () => setEditor({ mode: 'edit', values: providerFormValues(row) }) },
        { label: '导入 Key', icon: <IconKey />, disabled: providerOperationRunning, onSelect: () => setImporter(row) },
        {
          label: isRemovingProvider(row.id) ? '删除中' : '删除',
          icon: <IconDelete />,
          destructive: true,
          disabled: (savingProvider || importingKey) || (removingProvider && !isRemovingProvider(row.id)),
          confirm: {
            title: `删除提供商 ${row.id}？`,
            description: '删除后该提供商配置将不可用于后续账号导入。',
            confirmText: '删除',
          },
          onSelect: () => remove(row.id),
        },
      ]}
    />
  );

  const columns = [
    {
      title: '提供商',
      key: 'summary',
      width: 190,
      render: (_, row) => (
        <div className="pool-resource-summary pool-provider-summary">
          <VendorLogo vendor={row.id || row.name || 'custom'} size={30} />
          <div className="pool-provider-summary__copy">
            <TextClamp strong>{row.name || row.id || '-'}</TextClamp>
            <div className="pool-resource-summary__meta">
              <Tag size="small">{row.id || '-'}</Tag>
              <Tag size="small" color={row.enabled === false ? 'grey' : 'green'}>{row.enabled === false ? '停用' : '启用'}</Tag>
            </div>
          </div>
        </div>
      ),
    },
    { title: 'Base URL', dataIndex: 'base_url', width: 260, render: (v) => <TextClamp>{v || '-'}</TextClamp> },
    {
      title: '协议',
      dataIndex: 'upstream_protocol',
      width: 150,
      render: (v) => v === 'responses' ? <Tag color="green">Responses · Tier 2</Tag> : <Tag color="orange">Chat · Tier 3</Tag>,
    },
    {
      title: '模型',
      dataIndex: 'models',
      width: 220,
      render: renderModels,
    },
    { title: '发现', dataIndex: 'auto_discover_models', width: 72, render: (v) => (v === false ? '关闭' : '开启') },
    { title: '启用', dataIndex: 'enabled', width: 72, render: (v) => (v === false ? '否' : '是') },
    {
      title: '操作',
      key: 'ops',
      width: 264,
      render: (_, row) => renderProviderActions(row),
    },
  ];
  const mobileColumns = [
    {
      title: '提供商',
      dataIndex: 'name',
      render: (_, row) => (
        <MobileResourceCell
          title={row.name || row.id || '-'}
          subtitle={row.base_url || '-'}
          avatar={<VendorLogo vendor={row.id || row.name || 'custom'} size={30} />}
          badges={<><Tag>{row.id || '-'}</Tag><Tag color={row.enabled === false ? 'grey' : 'green'}>{row.enabled === false ? '停用' : '启用'}</Tag></>}
          details={[
            { label: '协议', value: row.upstream_protocol === 'responses' ? 'Responses · Tier 2' : 'Chat Completions · Tier 3' },
            { label: '模型', value: renderModels(row.models, row) },
            { label: '发现', value: row.auto_discover_models === false ? '关闭' : '开启' },
          ]}
          actions={renderProviderActions(row)}
        />
      ),
    },
  ];

  const editorValues = editor?.values || providerFormValues();

  return (
    <div>
      <PageHeader title="模型提供商" subtitle="OpenAI-compatible upstream registry"
        actions={<>
          <Button icon={<IconRefresh />} onClick={load} loading={loading}>刷新</Button>
          <Button icon={<IconPlus />} theme="solid" disabled={providerOperationRunning} onClick={() => setEditor({ mode: 'create', values: providerFormValues() })}>新增</Button>
        </>} />
      <ResourceTable
        error={error}
        onRetry={load}
        loading={loading}
        lastRefresh={lastRefresh}
        dataSource={rows}
        columns={columns}
        rowKey={(r) => r.id}
        pagination={false}
        className="pool-mobile-table pool-providers-table"
        density="compact"
        layout="fit"
        scroll={false}
        rowHeight={72}
        mobileColumns={mobileColumns}
        mobileScroll={false}
        emptyTitle="暂无模型提供商"
        emptyType="settings"
        skeletonRows={6}
      />

      <Modal
        title={editor?.mode === 'edit' ? '编辑模型提供商' : '新增模型提供商'}
        visible={!!editor}
        onCancel={() => { if (!savingProvider) setEditor(null); }}
        footer={null}
        maskClosable={!savingProvider}
      >
        {editor ? (
          <Form
            key={`${editor.mode}:${editorValues.id}`}
            initValues={editorValues}
            onSubmit={saveProvider}
            labelPosition="top"
          >
            <Form.Input field="id" label="Provider ID" disabled={editor.mode === 'edit'} rules={[{ required: true }]} />
            <Form.Input field="name" label="显示名称" />
            <Form.Input field="base_url" label="Base URL" rules={[{ required: true }]} placeholder="https://api.example.com/v1" />
            <Form.Select
              field="upstream_protocol"
              label="上游协议 / Skills 兼容层"
              optionList={PROTOCOL_OPTIONS}
              initValue="chat_completions"
            />
            <div className="pool-help-text" style={{ marginTop: -6, marginBottom: 8 }}>
              官方 Codex 账号为 Tier 1；Responses 原生供应商为 Tier 2；Chat Completions 桥接为 Tier 3，仅承诺基础 function tools，遇到 typed tools 会返回明确兼容性错误。
            </div>
            <Form.Switch field="enabled" label="启用" />
            <Form.Switch field="auto_discover_models" label="自动发现模型" />
            <Form.TextArea field="models_text" label="模型列表" autosize placeholder={"deepseek-chat\ndeepseek-reasoner"} />
            <Button htmlType="submit" theme="solid" loading={savingProvider} style={{ marginTop: 12 }}>保存</Button>
          </Form>
        ) : null}
      </Modal>

      <Modal
        title={importer ? `导入 ${importer.id} Key` : '导入 Key'}
        visible={!!importer}
        onCancel={() => { if (!importingKey) setImporter(null); }}
        footer={null}
        maskClosable={!importingKey}
      >
        {importer ? (
          <Form onSubmit={importKey} labelPosition="top">
            <Form.Input field="api_key" label="API Key" mode="password" rules={[{ required: true }]} />
            <Form.Input field="label" label="账号标签" placeholder={importer.name || importer.id} />
            <Form.Input field="group_name" label="分组" />
            <Button htmlType="submit" theme="solid" loading={importingKey} style={{ marginTop: 12 }}>导入</Button>
          </Form>
        ) : null}
      </Modal>
    </div>
  );
}
