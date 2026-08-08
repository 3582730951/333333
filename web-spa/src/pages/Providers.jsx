import React, { useState, useCallback, useRef } from 'react';
import { ActionMenu, Banner, Button, Toast, Modal, Form, Tag } from '../components/pool/index.jsx';
import { IconPlus, IconRefresh, IconEdit, IconKey, IconDelete, IconPlay } from '../components/pool/icons.jsx';
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

const splitModelMappings = (value) => {
  const out = {};
  for (const line of String(value || '').split('\n')) {
    const match = line.match(/^\s*(.+?)\s*(?:=>|=)\s*(.+?)\s*$/);
    if (match?.[1] && match?.[2]) out[match[1].trim()] = match[2].trim();
  }
  return out;
};

const joinModelMappings = (mappings) => Object.entries(mappings || {})
  .sort(([left], [right]) => left.localeCompare(right))
  .map(([source, target]) => `${source} => ${target}`)
  .join('\n');

const CLAUDE_MODEL_TABLE = [
  'claude-fable-5',
  'claude-opus-5',
  'claude-sonnet-5',
  'claude-opus-4-8',
  'claude-opus-4-7',
  'claude-opus-4-6',
  'claude-sonnet-4-6',
  'claude-sonnet-4-5-20250929',
  'claude-opus-4-5-20251101',
  'claude-haiku-4-5-20251001',
  'claude-sonnet-4-5',
  'claude-opus-4-5',
  'claude-haiku-4-5',
];

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

const DOWNSTREAM_PATH_OPTIONS = [
  { label: 'OpenAI · /v1/chat/completions', value: '/v1/chat/completions' },
  { label: 'Codex · /v1/responses', value: '/v1/responses' },
  { label: 'Claude · /v1/messages', value: '/v1/messages' },
  { label: '其他资源路径 · *', value: '*' },
];

const newProviderRoute = (routes = []) => {
  const used = new Set((routes || []).map((route) => route.downstream_path));
  const downstreamPath = DOWNSTREAM_PATH_OPTIONS.find((option) => !used.has(option.value))?.value || '/v1/chat/completions';
  const profile = downstreamPath === '/v1/responses'
    ? TRANSPORT_PROFILES[1]
    : downstreamPath === '/v1/messages'
      ? TRANSPORT_PROFILES[2]
      : TRANSPORT_PROFILES[0];
  return {
    id: '', downstream_path: downstreamPath, base_url: '',
    upstream_protocol: profile.protocol, transport_profile: profile.value,
  };
};

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

export const providerFormValues = (row) => ({
  id: row?.id || '',
  name: row?.name || '',
  base_url: row?.base_url || '',
  upstream_protocol: row?.upstream_protocol || 'chat_completions',
  transport_profile: row?.transport_profile || 'generic',
  routes: Array.isArray(row?.routes) ? row.routes.map((route) => ({ ...route })) : [],
  egress_ids: Array.isArray(row?.egress_ids) ? row.egress_ids : [],
  enabled: row?.enabled !== false,
  auto_discover_models: row?.auto_discover_models !== false,
  models_text: joinModels(row?.models),
  model_mappings_text: joinModelMappings(row?.model_mappings),
});

export const providerRoutesPayload = (routes) => (Array.isArray(routes) ? routes : [])
  .map((route) => ({
    id: String(route?.id || '').trim(),
    downstream_path: String(route?.downstream_path || '').trim(),
    base_url: String(route?.base_url || '').trim(),
    upstream_protocol: String(route?.upstream_protocol || '').trim(),
    transport_profile: String(route?.transport_profile || '').trim(),
  }))
  .filter((route) => route.downstream_path);

export function ProviderEditor({ editor, egressOptions, saving, onCancel, onSave }) {
  const [values, setValues] = useState(() => editor?.values || providerFormValues());
  const transportProfileRefs = useRef([]);
  const setValue = (key, value) => setValues((current) => ({ ...current, [key]: value }));
  const activeTransportProfileIndex = Math.max(0, TRANSPORT_PROFILES.findIndex((profile) => profile.value === values.transport_profile));
  const selectTransportProfile = (profile) => setValues((current) => ({
    ...current,
    transport_profile: profile.value,
    upstream_protocol: profile.protocol,
  }));
  const moveTransportProfileFocus = (event, index) => {
    let nextIndex = index;
    if (event.key === 'ArrowLeft' || event.key === 'ArrowUp') nextIndex = (index + TRANSPORT_PROFILES.length - 1) % TRANSPORT_PROFILES.length;
    else if (event.key === 'ArrowRight' || event.key === 'ArrowDown') nextIndex = (index + 1) % TRANSPORT_PROFILES.length;
    else if (event.key === 'Home') nextIndex = 0;
    else if (event.key === 'End') nextIndex = TRANSPORT_PROFILES.length - 1;
    else return;
    event.preventDefault();
    selectTransportProfile(TRANSPORT_PROFILES[nextIndex]);
    transportProfileRefs.current[nextIndex]?.focus();
  };
  const updateRoute = (index, patch) => setValues((current) => ({
    ...current,
    routes: (current.routes || []).map((route, routeIndex) => routeIndex === index ? { ...route, ...patch } : route),
  }));
  const removeRoute = (index) => setValues((current) => ({
    ...current,
    routes: (current.routes || []).filter((_, routeIndex) => routeIndex !== index),
  }));
  const addRoute = () => setValues((current) => ({
    ...current,
    routes: [...(current.routes || []), newProviderRoute(current.routes)],
  }));
  return (
    <div className="pool-provider-editor">
      <div className="pool-user-group-grid">
        <Form.Input label="Provider ID" value={values.id} onChange={(value) => setValue('id', value)} disabled={editor.mode === 'edit'} placeholder="例如：codex-edge" />
        <Form.Input label="显示名称" value={values.name} onChange={(value) => setValue('name', value)} placeholder="例如：Codex Edge" />
      </div>
      <Form.Input label="默认 Base URL（旧配置兼容）" value={values.base_url} onChange={(value) => setValue('base_url', value)} placeholder="https://api.example.com/v1" help="没有命中下方调用路径时使用；已有单路径配置保持原行为。" />
      <div className="pool-field pool-field--top">
        <span className="pool-field__label">客户端传输画像</span>
        <div className="pool-provider-profile-cards" role="radiogroup" aria-label="客户端传输画像">
          {TRANSPORT_PROFILES.map((profile) => {
            const active = values.transport_profile === profile.value;
            const profileIndex = TRANSPORT_PROFILES.indexOf(profile);
            return (
              <button
                type="button"
                role="radio"
                aria-checked={active}
                tabIndex={profileIndex === activeTransportProfileIndex ? 0 : -1}
                className={active ? 'is-active' : ''}
                key={profile.value}
                ref={(node) => { transportProfileRefs.current[profileIndex] = node; }}
                onClick={() => selectTransportProfile(profile)}
                onKeyDown={(event) => moveTransportProfileFocus(event, profileIndex)}
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
      <div className="pool-provider-routes" aria-label="调用路径">
        <div className="pool-provider-routes__heading">
          <div>
            <strong>调用路径</strong>
            <div className="pool-field__help">同一 Provider 可按下游入口使用不同 Base URL、协议与客户端画像；精确路径优先，* 处理文件等辅助接口。</div>
          </div>
          <Button icon={<IconPlus />} onClick={addRoute} disabled={saving}>添加路径</Button>
        </div>
        {(values.routes || []).length === 0 ? (
          <div className="pool-provider-routes__empty">未添加覆盖路径，全部请求继续使用上方默认配置。</div>
        ) : (values.routes || []).map((route, index) => (
          <div className="pool-provider-route-card" key={index}>
            <div className="pool-provider-route-card__heading">
              <strong>路径 {index + 1}</strong>
              <Button icon={<IconDelete />} theme="borderless" aria-label={`删除调用路径 ${index + 1}`} onClick={() => removeRoute(index)}>删除</Button>
            </div>
            <div className="pool-user-group-grid">
              <Form.Select
                label="下游入口"
                value={route.downstream_path}
                onChange={(value) => updateRoute(index, { downstream_path: value })}
                optionList={DOWNSTREAM_PATH_OPTIONS}
              />
              <Form.Input label="路径 ID（可选）" value={route.id || ''} onChange={(value) => updateRoute(index, { id: value })} placeholder="自动生成" />
            </div>
            <Form.Input label="该路径 Base URL" value={route.base_url || ''} onChange={(value) => updateRoute(index, { base_url: value })} placeholder={values.base_url || 'https://relay.example/v1'} help="留空继承默认 Base URL。" />
            <div className="pool-user-group-grid">
              <Form.Select
                label="客户端画像"
                value={route.transport_profile || 'generic'}
                onChange={(value) => {
                  const selected = TRANSPORT_PROFILES.find((profile) => profile.value === value);
                  updateRoute(index, { transport_profile: value, upstream_protocol: selected?.protocol || route.upstream_protocol });
                }}
                optionList={TRANSPORT_PROFILES.map((profile) => ({ label: profile.label, value: profile.value }))}
              />
              <Form.Select label="上游协议" value={route.upstream_protocol || 'chat_completions'} onChange={(value) => updateRoute(index, { upstream_protocol: value })} optionList={PROTOCOL_OPTIONS} />
            </div>
          </div>
        ))}
      </div>
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
      {values.upstream_protocol === 'anthropic_messages' && values.auto_discover_models ? (
        <Banner
          type="info"
          title="Claude 候选模型表探测"
          description={`若中转站没有 /models，系统会逐个发送最小 Messages 请求验证：${CLAUDE_MODEL_TABLE.join('、')}`}
        />
      ) : null}
      <Form.TextArea label="上游模型表" value={values.models_text} onChange={(value) => setValue('models_text', value)} autosize placeholder={'claude-sonnet-5\nrelay-model-pro'} help="每行一个目标中转站实际接受的模型；自动发现结果会回写到这里。" />
      <Form.TextArea label="下游 → 上游模型映射" value={values.model_mappings_text} onChange={(value) => setValue('model_mappings_text', value)} autosize placeholder={'claude-sonnet-5 => relay-model-pro\n* => relay-default'} help="每行 source => target；支持 * 作为该提供商的默认目标模型。" />
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
  const [tester, setTester] = useState(null);
  const [testModel, setTestModel] = useState('');
  const [testResult, setTestResult] = useState(null);
  const [testDownstreamPath, setTestDownstreamPath] = useState('');

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
        model_mappings: splitModelMappings(values.model_mappings_text),
        routes: providerRoutesPayload(values.routes),
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
      });
      Toast.success('账号已导入');
      setImporter(null);
    } catch (e) { showErrorToast(e); }
  });
  const { run: testProvider, running: testingProvider } = useAsyncAction(async () => {
    if (!tester?.id || !String(testModel || '').trim()) {
      Toast.warning('请输入要测试的模型');
      return;
    }
    try {
      const result = await post(`/admin/providers/${encodeURIComponent(tester.id)}/test`, {
        model: String(testModel).trim(),
        downstream_path: testDownstreamPath || undefined,
      });
      setTestResult(result);
      if (result?.ok) {
        Toast.success(`已到达目标中转站：${result.target_model || testModel}`);
        void load();
      } else {
        Toast.error(`测试未通过：${result?.error_code || 'unknown'}`);
      }
    } catch (e) {
      const result = e?.response?.data;
      if (result?.provider_id && result?.requested_model) {
        setTestResult(result);
      }
      showErrorToast(e);
    }
  });
  const providerOperationRunning = savingProvider || importingKey || testingProvider || removingProvider;

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
        { label: '测试模型', icon: <IconPlay />, disabled: providerOperationRunning, onSelect: () => { setTester(row); setTestModel(row.models?.[0] || Object.keys(row.model_mappings || {})[0] || ''); setTestDownstreamPath(row.routes?.[0]?.downstream_path || ''); setTestResult(null); } },
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
    {
      title: '调用路径', key: 'routes', width: 280,
      render: (_, row) => (
        <div className="pool-resource-summary">
          <TextClamp>{row.base_url || '-'}</TextClamp>
          <div className="pool-resource-summary__meta">{row.routes?.length ? `${row.routes.length} 条路径覆盖` : '默认单路径'}</div>
        </div>
      ),
    },
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
            { label: '调用路径', value: row.routes?.length ? `${row.routes.length} 条覆盖` : '默认单路径' },
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
            <div className="pool-field__help">无需配置分组；账号会自动加入系统默认账号池分组并继承其有序出口。</div>
            <Button htmlType="submit" theme="solid" loading={importingKey} style={{ marginTop: 12 }}>导入</Button>
          </Form>
        ) : null}
      </Modal>
      <Modal
        title={tester ? `测试 ${tester.name || tester.id}` : '测试模型提供商'}
        visible={!!tester}
        onCancel={() => { if (!testingProvider) { setTester(null); setTestResult(null); } }}
        onOk={testProvider}
        confirmLoading={testingProvider}
        okText="发送最小测试"
        maskClosable={!testingProvider}
      >
        <Form.Input label="测试模型" value={testModel} onChange={setTestModel} placeholder="例如 claude-sonnet-5" />
        {tester?.routes?.length ? (
          <Form.Select
            label="测试调用路径"
            value={testDownstreamPath}
            onChange={setTestDownstreamPath}
            optionList={[
              { label: '默认配置', value: '' },
              ...tester.routes.map((route) => ({
                label: `${route.downstream_path} · ${route.id || '自动 ID'}`,
                value: route.downstream_path,
              })),
            ]}
          />
        ) : null}
        <div className="pool-field__help">请求会经过该提供商的 Base URL、协议画像、API Key、出口及模型映射；结果同时显示下游模型与目标模型。</div>
        {testResult ? (
          <div className="pool-resource-summary" style={{ marginTop: 12 }}>
            <strong>{testResult.ok ? '到达成功' : '测试失败'}</strong>
            <div className="pool-resource-summary__meta">
              {testResult.requested_model || '-'} → {testResult.target_model || '-'} · HTTP {testResult.http_status || 0} · {testResult.latency_ms || 0} ms
            </div>
            {testResult.route_id ? <div className="pool-resource-summary__meta">调用路径：{testResult.downstream_path || '-'} · {testResult.route_id}</div> : null}
            {testResult.error_code ? <div className="pool-resource-summary__meta">错误：{testResult.error_code}</div> : null}
            {testResult.response_sample ? <TextClamp>{testResult.response_sample}</TextClamp> : null}
          </div>
        ) : null}
      </Modal>
    </div>
  );
}
