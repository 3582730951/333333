import React, { useCallback, useMemo, useState } from 'react';
import { Banner, Button, Card, Input, InputNumber, Modal, Select, Switch, Tag, Toast, Typography } from './pool/index.jsx';
import { IconCopy, IconEdit, IconPlus, IconRefresh, IconShield, IconTrash } from './pool/icons.jsx';
import { get } from '../api.js';
import { showErrorToast } from './ErrorToast.jsx';
import useAsyncAction from '../hooks/useAsyncAction.js';
import useAsyncResource from '../hooks/useAsyncResource.js';
import CopyCodeBlock from './CopyCodeBlock.jsx';
import { copyText } from './KeySecretTools.jsx';
import { browserOrigin } from '../lib/browserNavigation.js';
import {
  createSub2APIHubConnection,
  fetchSub2APIHubConnections,
  revokeSub2APIHubConnection,
  rotateSub2APIHubKey,
  setSub2APIHubEnabled,
  testSub2APIHubConnection,
  updateSub2APIHubConnection,
} from '../features/accounts/api/sub2apiHub.ts';

const EMPTY = { connections: [], global_enabled: false };
const DEFAULT_DRAFT = {
  name: '', target_group_id: '', inventory_scope: 'connection_only', provider_allowlist: ['codex'],
  allowed_proxy_ids: [], allowed_cidrs: [], default_concurrency: 3, default_priority: 50,
  max_accounts: 1000, max_import_batch: 100, requests_per_minute: 120,
  max_concurrent_requests: 4, duplicate_policy: 'reject_cross_connection',
  activation_policy: 'verify_then_activate', enabled: true,
};

function asArray(value) {
  return Array.isArray(value) ? value : [];
}

function connectionDraft(row) {
  return {
    ...DEFAULT_DRAFT,
    ...(row || {}),
    provider_allowlist: asArray(row?.provider_allowlist).length ? asArray(row.provider_allowlist) : ['codex'],
    allowed_proxy_ids: asArray(row?.allowed_proxy_ids),
    allowed_cidrs: asArray(row?.allowed_cidrs),
  };
}

function formatDate(value) {
  const n = Number(value);
  if (!n) return '—';
  try { return new Intl.DateTimeFormat(undefined, { dateStyle: 'medium', timeStyle: 'short' }).format(new Date(n * 1000)); }
  catch { return '—'; }
}

function keyCommand(baseURL, key) {
  return `curl -fsS '${baseURL}/admin/accounts?page=1&page_size=20' \\\n+  -H 'x-api-key: ${key}' \\\n+  -H 'Accept: application/json'`;
}

function defaultHubBaseURL() {
  const origin = browserOrigin();
  return origin ? `${origin}/api/v1` : '/api/v1';
}

function ConnectionStatus({ row }) {
  const expired = Number(row?.expires_at) > 0 && Number(row.expires_at) <= Math.floor(Date.now() / 1000);
  if (!row?.enabled || expired) return <Tag color="grey">已停用</Tag>;
  return <Tag color="green">已启用</Tag>;
}

function HubConnectionCard({ row, onEdit, onRotate, onTest, onRevoke, busy }) {
  const [showConfig, setShowConfig] = useState(false);
  const baseURL = row.base_url || defaultHubBaseURL();
  return (
    <Card className="pool-hub-connection-card">
      <div className="pool-hub-connection-card__head">
        <div>
          <div className="pool-hub-connection-card__title">{row.connection?.name || row.name || row.id}</div>
          <div className="pool-muted pool-hub-connection-card__id">{row.connection?.id || row.id}</div>
        </div>
        <ConnectionStatus row={row.connection || row} />
      </div>
      <div className="pool-hub-connection-card__metrics">
        <span><b>{row.account_count ?? 0}</b> 个账号</span>
        <span>目标组：<b>{row.target_group?.name || row.connection?.target_group_id || row.target_group_id || '—'}</b></span>
        <span>最近连接：<b>{formatDate((row.connection || row).last_seen_at)}</b></span>
      </div>
      <div className="pool-hub-connection-card__meta">
        <Tag size="small" color="blue">connection_only</Tag>
        <Tag size="small">Key {String((row.connection || row).key_prefix || 'hub_sk_')}</Tag>
        <span className="pool-muted">限流 {Number((row.connection || row).requests_per_minute || 120)} RPM · 并发 {Number((row.connection || row).max_concurrent_requests || 4)}</span>
      </div>
      <div className="pool-hub-connection-card__actions">
        <Button size="small" icon={<IconShield />} onClick={() => onTest(row)} loading={busy === 'test'}>测试兼容</Button>
        <Button size="small" icon={<IconEdit />} onClick={() => onEdit(row)} disabled={Boolean(busy)}>编辑</Button>
        <Button size="small" icon={<IconRefresh />} onClick={() => onRotate(row)} loading={busy === 'rotate'}>轮换 Key</Button>
        <Button size="small" theme="borderless" onClick={() => setShowConfig((v) => !v)}>{showConfig ? '收起连接配置' : '查看连接配置'}</Button>
        <Button size="small" type="danger" icon={<IconTrash />} onClick={() => onRevoke(row)} loading={busy === 'revoke'}>撤销</Button>
      </div>
      {showConfig ? (
        <div className="pool-hub-connection-card__config">
          <div className="pool-hub-connection-card__config-row"><span>Base URL</span><code>{baseURL}</code></div>
          <Typography.Text type="tertiary" size="small">Admin API Key 只在创建/轮换时显示一次；数据库仅保存 HMAC，不会在这里恢复旧 Key。</Typography.Text>
        </div>
      ) : null}
    </Card>
  );
}

function ConnectionEditor({ open, initial, groups, proxies, saving, onCancel, onSubmit }) {
  const [draft, setDraft] = useState(() => connectionDraft(initial));
  const editing = Boolean(initial?.connection?.id || initial?.id);
  React.useEffect(() => setDraft(connectionDraft(initial)), [initial]);
  const set = (key, value) => setDraft((current) => ({ ...current, [key]: value }));
  const submit = () => {
    if (!String(draft.name || '').trim() || !String(draft.target_group_id || '').trim()) {
      Toast.error('名称和目标分组不能为空');
      return;
    }
    onSubmit({ ...draft, name: String(draft.name).trim(), target_group_id: String(draft.target_group_id).trim() });
  };
  return (
    <Modal visible={open} title={editing ? '编辑 Sub2API 号池链接' : '创建 Sub2API 号池链接'} onCancel={onCancel} onOk={submit} okText={editing ? '保存并热更新' : '创建连接'} confirmLoading={saving} width={680}>
      <div className="pool-hub-editor">
        <div className="pool-callout pool-callout--info"><b>兼容接收面</b>：上游只需替换 Base URL 与 Hub Key；凭据由本地隔离验证后才进入可调度库存。</div>
        <div className="pool-grid pool-grid-2">
          <Input label="连接名称" value={draft.name} onChange={(v) => set('name', v)} placeholder="例如：生产自动补号器" />
          <Select label="目标分组" value={draft.target_group_id} onChange={(v) => set('target_group_id', v)} placeholder="选择本地账号分组" optionList={groups.map((g) => ({ label: g.name, value: g.name }))} />
        </div>
        <div className="pool-grid pool-grid-2">
          <Select label="库存范围" value={draft.inventory_scope} onChange={(v) => set('inventory_scope', v)} optionList={[{ label: '仅本连接导入账号（推荐）', value: 'connection_only' }, { label: '目标分组全部库存', value: 'target_group' }]} />
          <Select label="重复凭据策略" value={draft.duplicate_policy} onChange={(v) => set('duplicate_policy', v)} optionList={[{ label: '跨连接拒绝（推荐）', value: 'reject_cross_connection' }, { label: '复用未归属本地账号', value: 'reuse_unowned_local' }]} />
        </div>
        <div className="pool-grid pool-grid-3">
          <InputNumber label="默认并发" min={1} max={1000} value={draft.default_concurrency} onChange={(v) => set('default_concurrency', Number(v) || 1)} />
          <InputNumber label="最大账号数" min={1} max={1000000} value={draft.max_accounts} onChange={(v) => set('max_accounts', Number(v) || 1)} />
          <InputNumber label="单批上限" min={1} max={500} value={draft.max_import_batch} onChange={(v) => set('max_import_batch', Number(v) || 1)} />
        </div>
        <div className="pool-grid pool-grid-3">
          <InputNumber label="每分钟请求" min={1} max={10000} value={draft.requests_per_minute} onChange={(v) => set('requests_per_minute', Number(v) || 1)} />
          <InputNumber label="最大并发请求" min={1} max={100} value={draft.max_concurrent_requests} onChange={(v) => set('max_concurrent_requests', Number(v) || 1)} />
          <InputNumber label="默认优先级" min={1} max={10000} value={draft.default_priority} onChange={(v) => set('default_priority', Number(v) || 1)} />
        </div>
        <Select label="允许代理（可选）" value={draft.allowed_proxy_ids} onChange={(v) => set('allowed_proxy_ids', v)} multiple filter optionList={proxies.map((p) => ({ label: `${p.name || p.id} (${p.type || 'direct'})`, value: p.id }))} placeholder="仅允许管理员预先批准的代理" />
        <Input label="允许来源 CIDR（可选，逗号分隔）" value={draft.allowed_cidrs.join(', ')} onChange={(v) => set('allowed_cidrs', String(v || '').split(',').map((x) => x.trim()).filter(Boolean))} placeholder="例如：10.0.0.0/8, 203.0.113.12" />
        <div className="pool-hub-editor__switch"><Switch checked={Boolean(draft.enabled)} onChange={(v) => set('enabled', v)} /><span><b>连接启用</b><small>撤销只拒绝新请求，不删除已导入账号。</small></span></div>
      </div>
    </Modal>
  );
}

function OneTimeKey({ credential, onClose }) {
  if (!credential) return null;
  const baseURL = credential.base_url || defaultHubBaseURL();
  const key = credential.api_key || '';
  return (
    <Modal visible title="请立即保存 Hub 连接凭据" onCancel={onClose} footer={null} width={680}>
      <div className="pool-hub-secret">
        <Banner type="warning" title="Admin API Key 只显示一次" description="关闭此窗口后无法恢复旧 Key；如遗失，请在连接卡片中轮换 Key。" />
        <div className="pool-hub-secret__row"><span>Sub2API Base URL</span><code>{baseURL}</code><Button size="small" icon={<IconCopy />} onClick={() => void copyText(baseURL, 'Base URL 已复制')}>复制</Button></div>
        <div className="pool-hub-secret__row"><span>Admin API Key</span><code className="pool-secret-value">{key}</code><Button size="small" icon={<IconCopy />} onClick={() => void copyText(key, 'Admin API Key 已复制')}>复制</Button></div>
        <CopyCodeBlock code={keyCommand(baseURL, key)} label="复制 curl 示例" />
        <Typography.Text type="tertiary" size="small">不要把 Key 放进 URL、日志或前端代码；建议通过上游的 Secret 环境变量注入。</Typography.Text>
        <div className="pool-hub-secret__footer"><Button theme="solid" onClick={onClose}>我已安全保存</Button></div>
      </div>
    </Modal>
  );
}

export default function Sub2APIHubPanel() {
  const [editor, setEditor] = useState(null);
  const [credential, setCredential] = useState(null);
  const [busyKey, setBusyKey] = useState('');
  const load = useCallback(async ({ signal }) => {
    const [hub, groups, profiles] = await Promise.all([
      fetchSub2APIHubConnections(signal),
      get('/admin/groups', undefined, { signal }),
      get('/admin/egress-profiles', undefined, { signal }),
    ]);
    return {
      ...hub,
      groups: Array.isArray(groups) ? groups : groups?.groups || [],
      proxies: Array.isArray(profiles) ? profiles : profiles?.profiles || profiles?.egress_profiles || [],
    };
  }, []);
  const { data = { ...EMPTY, groups: [], proxies: [] }, loading, error, reload } = useAsyncResource(load, [load], { initialData: { ...EMPTY, groups: [], proxies: [] } });
  const connections = data.connections || [];
  const { run: save, running: saving } = useAsyncAction(async (input) => {
    try {
      const row = editor?.connection || editor;
      const response = row?.id ? await updateSub2APIHubConnection(row.id, input) : await createSub2APIHubConnection(input);
      setEditor(null);
      if (response?.api_key) setCredential(response);
      Toast.success(row?.id ? '连接已热更新' : '连接已创建');
      await reload();
    } catch (err) { showErrorToast(err); }
  });
  const action = async (kind, row, fn) => {
    const id = row?.connection?.id || row?.id;
    if (!id) return;
    setBusyKey(`${id}:${kind}`);
    try { const response = await fn(id); if (response?.api_key) setCredential({ ...response, base_url: row.base_url }); Toast.success(kind === 'test' ? '兼容性检查完成' : kind === 'rotate' ? 'Key 已轮换，请立即保存' : '连接已撤销'); await reload(); }
    catch (err) { showErrorToast(err); }
    finally { setBusyKey(''); }
  };
  const enable = async (next) => {
    setBusyKey('global');
    try { await setSub2APIHubEnabled(next); Toast.success(next ? '兼容接收面已开启' : '兼容接收面已关闭'); await reload(); }
    catch (err) { showErrorToast(err); }
    finally { setBusyKey(''); }
  };
  const visibleConnections = useMemo(() => connections.map((row) => ({ ...row, connection: row.connection || row })), [connections]);
  return (
    <section className="pool-hub-panel" aria-label="Sub2API 号池链接">
      <div className="pool-hub-panel__intro">
        <div><Typography.Title heading={3}>Sub2API 号池链接</Typography.Title><Typography.Text type="tertiary">兼容上游自动补号器的 Admin API 接收面。导入账号先隔离验证，库存状态通过同一协议即时回执。</Typography.Text></div>
        <div className="pool-hub-panel__toolbar"><Tag color={data.global_enabled ? 'green' : 'grey'}>{data.global_enabled ? '兼容面已开启' : '兼容面默认关闭'}</Tag><Switch checked={Boolean(data.global_enabled)} disabled={busyKey === 'global'} onChange={enable} /><Button icon={<IconRefresh />} loading={loading} onClick={() => reload()}>刷新</Button><Button icon={<IconPlus />} theme="solid" onClick={() => setEditor({})}>新建链接</Button></div>
      </div>
      {!data.global_enabled ? <Banner type="info" title="安全默认：兼容接收面当前关闭" description="创建连接不会开放公网端点；确认上游来源、CIDR 和容量策略后，再打开热开关。" /> : null}
      {error && !connections.length ? <Banner type="danger" title="连接列表暂时不可用" description={error.message || String(error)} /> : null}
      {visibleConnections.length ? <div className="pool-hub-connection-grid">{visibleConnections.map((row) => { const id = row.connection.id || row.id; const busy = busyKey.startsWith(`${id}:`) ? busyKey.slice(id.length + 1) : ''; return <HubConnectionCard key={id} row={row} busy={busy} onEdit={(item) => setEditor(item)} onRotate={(item) => action('rotate', item, rotateSub2APIHubKey)} onTest={(item) => action('test', item, testSub2APIHubConnection)} onRevoke={(item) => { if (window.confirm(`确认撤销连接“${item.connection.name || item.connection.id}”？`)) void action('revoke', item, revokeSub2APIHubConnection); }} />; })}</div> : <div className="pool-hub-empty"><IconShield /><b>还没有兼容连接</b><span>创建一个连接后，上游只需替换 Base URL 和 Key 即可继续自动补号。</span><Button theme="solid" icon={<IconPlus />} onClick={() => setEditor({})}>创建第一个连接</Button></div>}
      <ConnectionEditor open={Boolean(editor)} initial={editor?.connection ? editor : editor} groups={data.groups || []} proxies={data.proxies || []} saving={saving} onCancel={() => { if (!saving) setEditor(null); }} onSubmit={save} />
      <OneTimeKey credential={credential} onClose={() => setCredential(null)} />
    </section>
  );
}
