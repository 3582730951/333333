import React, { useState, useCallback } from 'react';
import { ActionMenu, Banner, Button, Toast, Modal, Form, Tag } from '../components/pool/index.jsx';
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
import OrderedEgressSelect from '../components/OrderedEgressSelect.jsx';

const splitModels = (value) => String(value || '')
  .split(/[\n,]/)
  .map((x) => x.trim())
  .filter(Boolean);

const joinModels = (models) => Array.isArray(models) ? models.join('\n') : '';

const PROTOCOL_OPTIONS = [
  { label: 'Chat Completions 格式（OpenAI 兼容）', value: 'chat_completions' },
  { label: 'Codex 格式（Responses API）', value: 'responses' },
  { label: 'Claude Code 格式（Anthropic Messages API）', value: 'anthropic_messages' },
];

const TRANSPORT_PROFILES = [
  {
    value: 'generic',
    protocol: 'chat_completions',
    label: 'OpenAI Chat',
    description: '通用 Bearer 鉴权与 OpenAI Chat Completions 请求画像。',
  },
  {
    value: 'codex_cli',
    protocol: 'responses',
    label: 'Codex CLI',
    description: '模拟 Codex CLI/cc-switch 的 Responses 请求头、会话和流格式。',
  },
  {
    value: 'claude_code',
    protocol: 'anthropic_messages',
    label: 'Claude Code',
    description: '模拟 Claude Code Messages、beta 标记与会话请求画像。',
  },
];

const protocolLabel = (protocol) => {
  if (protocol === 'responses') return 'Codex · Responses';
  if (protocol === 'anthropic_messages') return 'Claude · Messages';
  return 'OpenAI · Chat';
};

const egressOptionList = (profiles = []) => {
  const out = [];
  const seen = new Set();
  const add = (profile) => {
    const id = String(profile?.id || '').trim();
    if (!id || seen.has(id)) return;
    seen.add(id);
    out.push({ label: `${profile.name || id} (${profile.type || 'direct'})`, value: id });
  };
  add({ id: 'egress_direct', name: 'egress_direct', type: 'direct' });
  for (const profile of profiles || []) add(profile);
  return out;
};

const providerFormValues = (row) => ({
  id: row?.id || '',
  name: row?.name || '',
  base_url: row?.base_url || '',
  upstream_protocol: row?.upstream_protocol || 'chat_completions',
  transport_profile: row?.transport_profile || 'generic',
  egress_ids: Array.isArray(row?.egress_ids) ? row.egress_ids : [],
  enabled: row?.enabled !== false,
  auto_discover_models: row?.auto_discover_models !== false,
  models_text: joinModels(row?.models),
});

function ProviderEditor({ editor, egressOptions, saving, onCancel, onSave }) {
  const [values, setValues] = useState(() => editor?.values || providerFormValues());
  const setValue = (key, value) => setValues((current) => ({ ...current, [key]: value }));
  return (
    <div className="pool-provider-editor">
      <div className="pool-user-group-grid">
        <Form.Input label="Provider ID" value={values.id} onChange={(value) => setValue('id', value)} disabled={editor.mode === 'edit'} placeholder="例如：codex-edge" />
        <Form.Input label="显示名称" value={values.name} onChange={(value) => setValue('name', value)} placeholder="例如：Codex Edge" />
      </div>
      <Form.Input label="Base URL" value={values.base_url} onChange={(value) => setValue('base_url', value)} placeholder="https://api.example.com/v1" />
      <div className="pool-field pool-field--top">
        <span className="pool-field__label">客户端传输画像</span>
        <div className="pool-provider-profile-cards" role="radiogroup" aria-label="客户端传输画像">
          {TRANSPORT_PROFILES.map((profile) => {
            const active = values.transport_profile === profile.value;
            return (
              <button
                type="button"
                role="radio"
                aria-checked={active}
                className={active ? 'is-active' : ''}
                key={profile.value}
                onClick={() => setValues((current) => ({ ...current, transport_profile: profile.value, upstream_protocol: profile.protocol }))}
              >
                <strong>{profile.label}</strong>
                <span>{profile.description}</span>
              </button>
            );
          })}
        </div>
      </div>
      <Form.Select label="上游协议" value={values.upstream_protocol} onChange={(value) => setValue('upstream_protocol', value)} optionList={PROTOCOL_OPTIONS} />
      <Banner
        type="info"
        title="请求画像会影响协议头与会话语义"
        description="Codex CLI 和 Claude Code 画像会模拟对应客户端的已测试请求形态；generic 保持现有中转行为。协议转换仍由统一 Adapter 完成。"
      />
      <div className="pool-field pool-field--top">
        <span className="pool-field__label">有序出口</span>
        <OrderedEgressSelect
          value={values.egress_ids}
          onChange={(egress_ids) => setValue('egress_ids', egress_ids)}
          options={egressOptions}
          disabled={saving}
          help="首项为主出口，先穷尽该提供商的备用出口，再切换用户分组中的其他目标。"
        />
      </div>
      <div className="pool-user-group-grid">
        <label className="pool-inline-switch"><Form.Switch value={values.enabled} onChange={(value) => setValue('enabled', value)} /><span>启用提供商</span></label>
        <label className="pool-inline-switch"><Form.Switch value={values.auto_discover_models} onChange={(value) => setValue('auto_discover_models', value)} /><span>自动发现模型</span></label>
      </div>
      <Form.TextArea label="模型能力 / 映射" value={values.models_text} onChange={(value) => setValue('models_text', value)} autosize placeholder={'gpt-5.3-codex\nclaude-opus-4-1'} help="每行一个模型；留空时暂按支持全部模型处理，并建议尽快补齐能力。" />
      <div className="pool-modal-actions">
        <Button onClick={onCancel} disabled={saving}>取消</Button>
        <Button theme="solid" loading={saving} disabled={!String(values.id || values.name).trim() || !String(values.base_url).trim()} onClick={() => onSave(values)}>保存</Button>
      </div>
    </div>
  );
}

export default function Providers() {
  const [editor, setEditor] = useState(null);
  const [importer, setImporter] = useState(null);

  const fetchRows = useCallback(async ({ signal }) => {
    return rowsOf(await get('/admin/providers', undefined, { signal }));
  }, []);
  const { data: rows = [], loading, error, lastRefresh, reload: load } = useAsyncResource(fetchRows, [fetchRows], { initialData: [] });
  const fetchEgressRows = useCallback(async ({ signal }) => rowsOf(await get('/admin/egress-profiles', undefined, { signal })), []);
  const { data: egressRows = [] } = useAsyncResource(fetchEgressRows, [fetchEgressRows], { initialData: [] });
  const egressOptions = egressOptionList(egressRows);

  const { run: saveProvider, running: savingProvider } = useAsyncAction(async (values) => {
    try {
      const identity = `${values.id || ''} ${values.name || ''}`.toLowerCase();
      let transportProfile = values.transport_profile || 'generic';
      let upstreamProtocol = values.upstream_protocol || 'chat_completions';
      if (editor?.mode === 'create' && transportProfile === 'generic' && upstreamProtocol === 'chat_completions') {
        if (identity.includes('claude-code') || identity.includes('claude_code')) {
          transportProfile = 'claude_code';
          upstreamProtocol = 'anthropic_messages';
        } else if (identity.includes('codex')) {
          transportProfile = 'codex_cli';
          upstreamProtocol = 'responses';
        }
      }
      await post('/admin/providers', {
        id: values.id,
        name: values.name,
        base_url: values.base_url,
        upstream_protocol: upstreamProtocol,
        transport_profile: transportProfile,
        egress_ids: Array.isArray(values.egress_ids) ? values.egress_ids : [],
        enabled: values.enabled !== false,
        auto_discover_models: values.auto_discover_models !== false,
        models: splitModels(values.models_text),
      });
      Toast.success('已保存');
      setEditor(null);
      void load();
    } catch (e) { showErrorToast(e); }
  });

  const { run: remove, running: removingProvider, isRunning: isRemovingProvider } = useKeyedAsyncAction(async (id) => {
    try {
      await del(`/admin/providers/${encodeURIComponent(id)}`);
      Toast.success('已删除');
      void load();
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
      title: '协议 / 画像',
      key: 'protocol',
      width: 190,
      render: (_, row) => (
        <div className="pool-resource-summary">
          <Tag color={row.upstream_protocol === 'responses' ? 'green' : row.upstream_protocol === 'anthropic_messages' ? 'violet' : 'orange'}>{protocolLabel(row.upstream_protocol)}</Tag>
          <div className="pool-resource-summary__meta">{row.transport_profile || 'generic'}</div>
        </div>
      ),
    },
    {
      title: '模型',
      dataIndex: 'models',
      width: 220,
      render: renderModels,
    },
    {
      title: '出口',
      dataIndex: 'egress_ids',
      width: 180,
      render: (ids) => Array.isArray(ids) && ids.length
        ? <TagList items={ids.map((id, index) => `${index === 0 ? '主' : `备${index}`} · ${id}`)} max={3} />
        : <span className="pool-muted">系统默认</span>,
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
            { label: '协议', value: protocolLabel(row.upstream_protocol) },
            { label: '画像', value: row.transport_profile || 'generic' },
            { label: '出口', value: row.egress_ids?.length ? row.egress_ids.join(' → ') : '系统默认' },
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
      <PageHeader title="模型提供商" subtitle="支持 OpenAI Chat、Codex Responses 与 Claude Code Messages 格式的中转站"
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
        {editor ? <ProviderEditor key={`${editor.mode}:${editorValues.id}`} editor={{ ...editor, values: editorValues }} egressOptions={egressOptions} saving={savingProvider} onCancel={() => setEditor(null)} onSave={saveProvider} /> : null}
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
            <div className="pool-field__help">账号请求时动态继承所选账号池分组的有序出口，不在账号记录中复制出口配置。</div>
            <Button htmlType="submit" theme="solid" loading={importingKey} style={{ marginTop: 12 }}>导入</Button>
          </Form>
        ) : null}
      </Modal>
    </div>
  );
}
