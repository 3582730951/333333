import React, { useCallback, useEffect, useMemo, useState } from 'react';
import { Banner, Button, Card, Switch, Tag, Toast } from '../components/pool/index.jsx';
import { IconCopy, IconDelete, IconEdit, IconGlobe, IconPlus, IconRefresh } from '../components/pool/icons.jsx';
import PageHeader from '../components/PageHeader.jsx';
import { del, get, post, put } from '../api.js';
import { showErrorToast } from '../components/ErrorToast.jsx';
import { openExternalURL } from '../lib/browserNavigation.js';
import { writeClipboard } from '../lib/browserClipboard.js';

const ROUTE_USER_GROUP = 'user_group';
const ROUTE_ACCOUNT_POOL = 'account_pool_group';

function emptyDraft() {
  return {
    id: '',
    slug: '',
    name: '',
    enabled: false,
    route_type: ROUTE_USER_GROUP,
    user_group_id: '',
    group_name: '',
    model: 'gpt-5.6-sol',
    title: '',
    welcome_message: '',
    max_history_messages: 24,
    rate_limit_per_minute: 30,
  };
}

function slugify(value) {
  return String(value || '')
    .trim()
    .toLowerCase()
    .replace(/[^a-z0-9_-]+/g, '-')
    .replace(/^-+|-+$/g, '')
    .slice(0, 64);
}

function optionLabel(options, value) {
  return options.find((item) => item.value === value)?.label || value || '未配置';
}

function normalizeRows(value, keys = []) {
  if (Array.isArray(value)) return value;
  if (value && typeof value === 'object') {
    for (const key of keys) {
      if (Array.isArray(value[key])) return value[key];
    }
    for (const key of ['items', 'rows', 'data']) {
      if (Array.isArray(value[key])) return value[key];
    }
  }
  return [];
}

export default function PublicChat() {
  const [links, setLinks] = useState([]);
  const [userGroups, setUserGroups] = useState([]);
  const [accountGroups, setAccountGroups] = useState([]);
  const [loading, setLoading] = useState(false);
  const [saving, setSaving] = useState(false);
  const [draft, setDraft] = useState(emptyDraft());

  const userGroupOptions = useMemo(() => userGroups.map((group) => ({
    value: group.id,
    label: `${group.name || group.id} (${group.id})`,
  })), [userGroups]);
  const accountGroupOptions = useMemo(() => accountGroups.map((group) => ({
    value: group.name,
    label: `${group.name}${Number.isFinite(group.active_account_count) ? ` · ${group.active_account_count} 个活跃账号` : ''}`,
  })), [accountGroups]);

  const load = useCallback(async () => {
    setLoading(true);
    try {
      const [chatLinks, ug, ag] = await Promise.all([
        get('/admin/public-chat/links'),
        get('/admin/user-groups'),
        get('/admin/groups'),
      ]);
      setLinks(normalizeRows(chatLinks, ['links', 'public_chat_links']));
      setUserGroups(normalizeRows(ug, ['user_groups']));
      setAccountGroups(normalizeRows(ag, ['groups']));
    } catch (error) {
      showErrorToast(error);
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => { load(); }, [load]);

  const updateDraft = (patch) => setDraft((current) => ({ ...current, ...patch }));
  const editLink = (link) => setDraft({
    ...emptyDraft(),
    ...link,
    max_history_messages: link.max_history_messages || 24,
    rate_limit_per_minute: link.rate_limit_per_minute || 30,
  });
  const resetDraft = () => setDraft(emptyDraft());

  const payload = () => ({
    slug: slugify(draft.slug),
    name: draft.name.trim(),
    enabled: Boolean(draft.enabled),
    route_type: draft.route_type,
    user_group_id: draft.route_type === ROUTE_USER_GROUP ? draft.user_group_id : '',
    group_name: draft.route_type === ROUTE_ACCOUNT_POOL ? draft.group_name : '',
    model: draft.model.trim() || 'gpt-5.6-sol',
    title: draft.title.trim(),
    welcome_message: draft.welcome_message.trim(),
    max_history_messages: Number(draft.max_history_messages) || 24,
    rate_limit_per_minute: Number(draft.rate_limit_per_minute) || 30,
  });

  const save = async () => {
    const body = payload();
    if (!body.slug) {
      Toast.error('请填写 URL Slug');
      return;
    }
    if (body.route_type === ROUTE_USER_GROUP && !body.user_group_id) {
      Toast.error('请选择承接流量的用户分组');
      return;
    }
    if (body.route_type === ROUTE_ACCOUNT_POOL && !body.group_name) {
      Toast.error('请选择承接流量的账号池分组');
      return;
    }
    setSaving(true);
    try {
      if (draft.id) await put(`/admin/public-chat/links/${encodeURIComponent(draft.id)}`, { ...body, id: draft.id });
      else await post('/admin/public-chat/links', body);
      Toast.success('在线聊天链接已保存');
      resetDraft();
      await load();
    } catch (error) {
      showErrorToast(error);
    } finally {
      setSaving(false);
    }
  };

  const toggle = async (link) => {
    try {
      await put(`/admin/public-chat/links/${encodeURIComponent(link.id)}`, { ...link, enabled: !link.enabled });
      await load();
    } catch (error) {
      showErrorToast(error);
    }
  };

  const remove = async (link) => {
    if (!window.confirm(`删除在线聊天链接 ${link.name || link.slug}？`)) return;
    try {
      await del(`/admin/public-chat/links/${encodeURIComponent(link.id)}`);
      Toast.success('已删除');
      if (draft.id === link.id) resetDraft();
      await load();
    } catch (error) {
      showErrorToast(error);
    }
  };

  const copy = async (text) => {
    if (await writeClipboard(text)) Toast.success('链接已复制');
    else Toast.error('复制失败');
  };

  const routeSummary = (link) => {
    if (link.route_type === ROUTE_USER_GROUP) return `用户分组：${link.route_label || optionLabel(userGroupOptions, link.user_group_id)}`;
    return `账号池分组：${link.route_label || optionLabel(accountGroupOptions, link.group_name)}`;
  };

  return (
    <div className="public-chat-admin-page">
      <PageHeader
        title="在线聊天"
        subtitle="生成无需登录的网页聊天 URL；管理员指定由哪个用户分组或账号池分组承接流量。"
        actions={<><Button icon={<IconRefresh />} loading={loading} onClick={load}>刷新</Button><Button icon={<IconPlus />} theme="solid" onClick={resetDraft}>新建链接</Button></>}
      />

      <Banner type="info" title="工作方式">
        访客只访问 /chat/&lt;slug&gt;，不会拿到 API Key。后端按这里配置的分组和模型内部转发到 /v1/chat/completions，并继续复用已有的强制模型、指令、Super-Instruct、用量统计和调度能力。
      </Banner>

      <div className="pool-grid pool-grid-2 public-chat-layout" style={{ alignItems: 'start', marginTop: 16 }}>
        <Card title={draft.id ? '编辑聊天链接' : '新建聊天链接'} className="pool-card">
          <div className="pool-form-grid">
            <label className="pool-field">
              <span className="pool-field__label">名称</span>
              <input className="pool-input" value={draft.name} onChange={(event) => {
                const name = event.target.value;
                updateDraft({ name, slug: draft.slug || slugify(name), title: draft.title || name });
              }} placeholder="例如：官网客服" />
            </label>
            <label className="pool-field">
              <span className="pool-field__label">URL Slug</span>
              <input className="pool-input" value={draft.slug} onChange={(event) => updateDraft({ slug: slugify(event.target.value) })} placeholder="support-chat" />
              <span className="pool-field__help">最终访问地址为 /chat/&lt;slug&gt;，仅允许英文、数字、- 和 _。</span>
            </label>
            <label className="pool-field">
              <span className="pool-field__label">页面标题</span>
              <input className="pool-input" value={draft.title} onChange={(event) => updateDraft({ title: event.target.value })} placeholder="在线聊天" />
            </label>
            <label className="pool-field">
              <span className="pool-field__label">模型</span>
              <input className="pool-input" value={draft.model} onChange={(event) => updateDraft({ model: event.target.value })} placeholder="gpt-5.6-sol" />
              <span className="pool-field__help">如果所选用户分组配置了强制模型，最终以用户分组强制模型为准。</span>
            </label>
            <label className="pool-field">
              <span className="pool-field__label">承接类型</span>
              <select className="pool-select" value={draft.route_type} onChange={(event) => updateDraft({ route_type: event.target.value })}>
                <option value={ROUTE_USER_GROUP}>用户分组（推荐）</option>
                <option value={ROUTE_ACCOUNT_POOL}>账号池分组</option>
              </select>
            </label>
            {draft.route_type === ROUTE_USER_GROUP ? (
              <label className="pool-field">
                <span className="pool-field__label">承接用户分组</span>
                <select className="pool-select" value={draft.user_group_id} onChange={(event) => updateDraft({ user_group_id: event.target.value })}>
                  <option value="">请选择用户分组</option>
                  {userGroupOptions.map((option) => <option key={option.value} value={option.value}>{option.label}</option>)}
                </select>
              </label>
            ) : (
              <label className="pool-field">
                <span className="pool-field__label">承接账号池分组</span>
                <select className="pool-select" value={draft.group_name} onChange={(event) => updateDraft({ group_name: event.target.value })}>
                  <option value="">请选择账号池分组</option>
                  {accountGroupOptions.map((option) => <option key={option.value} value={option.value}>{option.label}</option>)}
                </select>
              </label>
            )}
            <label className="pool-field">
              <span className="pool-field__label">历史消息上限</span>
              <input className="pool-input" type="number" min="2" max="100" value={draft.max_history_messages} onChange={(event) => updateDraft({ max_history_messages: event.target.value })} />
            </label>
            <label className="pool-field">
              <span className="pool-field__label">单 IP 每分钟限制</span>
              <input className="pool-input" type="number" min="1" max="600" value={draft.rate_limit_per_minute} onChange={(event) => updateDraft({ rate_limit_per_minute: event.target.value })} />
            </label>
            <label className="pool-field" style={{ gridColumn: '1 / -1' }}>
              <span className="pool-field__label">欢迎语</span>
              <textarea className="pool-input" rows="3" value={draft.welcome_message} onChange={(event) => updateDraft({ welcome_message: event.target.value })} placeholder="打开聊天页时展示给访客的提示。" />
            </label>
            <div className="pool-field" style={{ gridColumn: '1 / -1' }}>
              <span className="pool-field__label">启用</span>
              <Switch checked={draft.enabled} onChange={(value) => updateDraft({ enabled: value })} />
            </div>
          </div>
          <div className="pool-form-actions">
            <Button theme="solid" icon={<IconGlobe />} loading={saving} onClick={save}>{draft.id ? '保存修改' : '创建链接'}</Button>
            {draft.id ? <Button onClick={resetDraft}>取消编辑</Button> : null}
          </div>
        </Card>

        <Card title="已配置链接" className="pool-card">
          <div className="public-chat-list">
            {links.length === 0 ? <div className="pool-empty">暂无在线聊天链接</div> : links.map((link) => (
              <div className="public-chat-row" key={link.id}>
                <div className="public-chat-row__main">
                  <div className="public-chat-row__title">
                    <strong>{link.name || link.slug}</strong>
                    <Tag color={link.enabled ? 'green' : 'grey'}>{link.enabled ? '已启用' : '已关闭'}</Tag>
                  </div>
                  <div className="pool-resource-summary__meta">{routeSummary(link)}</div>
                  <div className="pool-resource-summary__meta">模型：{link.model} · 历史 {link.max_history_messages} · 限速 {link.rate_limit_per_minute}/分钟</div>
                  <code className="pool-mono">{link.public_url || `/chat/${link.slug}`}</code>
                </div>
                <div className="public-chat-row__actions">
                  <Button size="small" icon={<IconCopy />} onClick={() => copy(link.public_url || `/chat/${link.slug}`)}>复制</Button>
                  <Button size="small" icon={<IconGlobe />} onClick={() => openExternalURL(link.public_url || `/chat/${link.slug}`)}>打开</Button>
                  <Button size="small" icon={<IconEdit />} onClick={() => editLink(link)}>编辑</Button>
                  <Button size="small" onClick={() => toggle(link)}>{link.enabled ? '关闭' : '启用'}</Button>
                  <Button size="small" type="danger" icon={<IconDelete />} onClick={() => remove(link)}>删除</Button>
                </div>
              </div>
            ))}
          </div>
        </Card>
      </div>
    </div>
  );
}
