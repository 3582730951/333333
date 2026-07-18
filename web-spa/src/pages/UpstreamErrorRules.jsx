import React, { useCallback, useEffect, useMemo, useState } from 'react';
import { Button, Drawer, Tag, Toast, Switch, Select } from '../components/pool/index.jsx';
import { IconPlus, IconRefresh, IconEdit, IconDelete, IconPlay } from '../components/pool/icons.jsx';
import PageHeader from '../components/PageHeader.jsx';
import { get, post, patch, del } from '../api.js';
import { showErrorToast } from '../components/ErrorToast.jsx';

const ACCOUNT_ACTIONS = [
  ['builtin', '沿用系统默认逻辑'],
  ['none', '不处理账号'],
  ['cooldown', '冷却账号'],
  ['cooldown_recheck', '冷却并标记复查'],
  ['quarantine', '隔离账号'],
  ['auto_continue', '自动续写(截断时继续，不惩罚账号)'],
];
const DOWNSTREAM_ACTIONS = [
  ['builtin', '沿用系统默认逻辑'],
  ['failover', '自动切换上游'],
  ['pass', '透传给下游'],
  ['custom_error', '返回自定义错误'],
  ['neutralize', '返回中性错误'],
  ['idle_stream', '流式心跳空转'],
  ['intercept', '拦截命中内容并继续'],
  ['hide_safety_buffering', '隐藏安全检查等待提示'],
  ['heartbeat_finish', '发送一次心跳后干净结束'],
];
const ENTRYPOINTS = [
  ['responses', 'Responses'],
  ['chat_completions', 'Chat Completions'],
  ['claude_messages', 'Claude Messages'],
  ['claude_passthrough', 'Claude Passthrough'],
  ['custom_openai', 'Custom OpenAI-compatible'],
];

const emptyRule = () => ({
  name: '',
  enabled: false,
  priority: 100,
  providers: [],
  entrypoints: [],
  model_patterns: [],
  status_codes: [],
  body_keywords: [],
  match_mode: 'any',
  account_action: 'builtin',
  downstream_action: 'builtin',
  response_status: 503,
  custom_message: '',
  cooldown_seconds: 1800,
  prefer_retry_after: true,
  idle_seconds: 60,
  idle_ping_seconds: 15,
  skip_log: false,
  filter_account_action: false,
  keyword_case_sensitive: true,
  description: '',
});

const splitList = (value) => String(value || '').split(/[\n,]/).map((x) => x.trim()).filter(Boolean);
const splitInts = (value) => splitList(value).map((x) => Number(x)).filter((x) => Number.isInteger(x) && x > 0);
const joinList = (value) => Array.isArray(value) ? value.join('\n') : '';
const joinInts = (value) => Array.isArray(value) ? value.join(', ') : '';
const labelOf = (pairs, value) => pairs.find(([id]) => id === value)?.[1] || value || '默认';

function normalizeForEditor(rule = {}) {
  const source = rule || {};
  return {
    ...emptyRule(),
    ...source,
    status_codes_text: joinInts(source.status_codes),
    body_keywords_text: joinList(source.body_keywords),
    manual_patterns_text: joinList(source.model_patterns),
    entrypoint: source.entrypoints?.[0] || '',
    provider: source.providers?.[0] || '',
  };
}

function humanSummary(rule) {
  const scope = [rule.providers?.join(' / ') || '全部平台', rule.model_patterns?.join('、') || '全部模型'].join(' · ');
  if (rule.downstream_action === 'hide_safety_buffering') {
    return `对 ${scope} 的 Responses 流隐藏安全检查等待提示；仅移除 safety_buffering 字段，其他事件（包括 response.completed）继续下发。`;
  }
  const status = rule.status_codes?.length ? `${rule.status_codes.join('/')} 状态码` : '任意状态码';
  const kw = rule.body_keywords?.length ? `body 包含 ${rule.body_keywords.join('、')}` : '任意响应内容';
  return `当 ${scope} 返回 ${status} 且 ${kw} 时，${labelOf(ACCOUNT_ACTIONS, rule.account_action)}，并${labelOf(DOWNSTREAM_ACTIONS, rule.downstream_action)}。`;
}

function ProviderCascade({ model_options, form, setForm }) {
  const providers = model_options?.providers || [];
  const selectedProvider = providers.find((p) => p.id === form.provider) || providers[0];
  const selectedFamily = selectedProvider?.families?.find((f) => f.id === form.family) || selectedProvider?.families?.[0];
  const selectedModel = selectedFamily?.models?.find((m) => m.id === form.model) || null;

  const setProvider = (id) => {
    const p = providers.find((item) => item.id === id);
    const f = p?.families?.[0];
    setForm((current) => ({ ...current, provider: id, providers: id ? [id] : [], family: f?.id || '', model: '', model_scope: 'all' }));
  };
  const setFamily = (id) => setForm((current) => ({ ...current, family: id, model: '', model_scope: 'family' }));
  const setModel = (id) => setForm((current) => ({ ...current, model: id, model_scope: id ? 'model' : 'family' }));

  return (
    <div className="upstream-rule-cascade">
      <label><span>平台 / 供应商</span><select className="pool-select" value={form.provider || ''} onChange={(e) => setProvider(e.target.value)}><option value="">全部平台</option>{providers.map((p) => <option key={p.id} value={p.id}>{p.label}</option>)}</select></label>
      <label><span>模型系列</span><select className="pool-select" value={form.family || ''} onChange={(e) => setFamily(e.target.value)} disabled={!selectedProvider}><option value="">全部系列</option>{selectedProvider?.families?.map((f) => <option key={f.id} value={f.id}>{f.label}</option>)}</select></label>
      <label><span>具体模型</span><select className="pool-select" value={form.model || ''} onChange={(e) => setModel(e.target.value)} disabled={!selectedFamily}><option value="">当前系列全部模型</option>{selectedFamily?.models?.map((m) => <option key={m.id} value={m.id}>{m.label}</option>)}</select></label>
      <div className="upstream-rule-scope-pills">
        <button type="button" className={form.model_scope === 'all' ? 'active' : ''} onClick={() => setForm((c) => ({ ...c, model_scope: 'all', model: '', family: '' }))}>全部模型</button>
        <button type="button" className={form.model_scope === 'family' ? 'active' : ''} onClick={() => setForm((c) => ({ ...c, model_scope: 'family', model: '' }))}>当前系列全部模型</button>
        <button type="button" className={form.include_future ? 'active' : ''} onClick={() => setForm((c) => ({ ...c, include_future: !c.include_future }))}>未来新模型也自动包含</button>
        <button type="button" className={form.manual_pattern_enabled ? 'active' : ''} onClick={() => setForm((c) => ({ ...c, manual_pattern_enabled: !c.manual_pattern_enabled }))}>手动 pattern</button>
      </div>
      {selectedModel ? <div className="pool-help-text">已选择：{selectedProvider?.label} / {selectedFamily?.label} / {selectedModel.label}</div> : null}
    </div>
  );
}

export default function UpstreamErrorRules() {
  const [rules, setRules] = useState([]);
  const [model_options, setModelOptions] = useState({ providers: [] });
  const [loading, setLoading] = useState(false);
  const [editor, setEditor] = useState(null);
  const [form, setForm] = useState(emptyRule());
  const [expanded, setExpanded] = useState({});
  const [testInput, setTestInput] = useState({ provider: 'claude', entrypoint: 'claude_messages', model: 'claude-sonnet-4.5', status: 429, body: 'quota reached', streaming: false });
  const [testResult, setTestResult] = useState(null);

  const load = useCallback(async () => {
    setLoading(true);
    try {
      const [rows, options] = await Promise.all([
        get('/admin/upstream-error-rules'),
        get('/admin/upstream-error-rules/model-options'),
      ]);
      setRules(Array.isArray(rows) ? rows : []);
      setModelOptions(options || { providers: [] });
    } catch (e) { showErrorToast(e); }
    finally { setLoading(false); }
  }, []);
  useEffect(() => { load(); }, [load]);

  const stats = useMemo(() => ({
    enabled: rules.filter((r) => r.enabled).length,
    failover: rules.filter((r) => r.downstream_action === 'failover').length,
    pass: rules.filter((r) => r.downstream_action === 'pass').length,
    idle: rules.filter((r) => r.downstream_action === 'idle_stream').length,
  }), [rules]);

  const openEditor = (rule) => {
    const values = normalizeForEditor(rule);
    setEditor(rule?.id ? { mode: 'edit', id: rule.id } : { mode: 'create' });
    setForm(values);
  };

  const buildPayload = () => {
    const modelPatterns = new Set(splitList(form.manual_patterns_text));
    if (form.model_scope === 'model' && form.model) modelPatterns.add(form.model);
    if (form.model_scope === 'family' && form.family) modelPatterns.add(`${form.family}*`);
    if (form.include_future && form.family) modelPatterns.add(`${form.family}*`);
    return {
      name: form.name,
      enabled: !!form.enabled,
      priority: Number(form.priority) || 100,
      providers: form.provider ? [form.provider] : [],
      entrypoints: form.entrypoint ? [form.entrypoint] : [],
      model_patterns: [...modelPatterns],
      status_codes: splitInts(form.status_codes_text),
      body_keywords: form.downstream_action === 'hide_safety_buffering' ? [] : splitList(form.body_keywords_text),
      match_mode: form.match_mode || 'any',
      account_action: form.downstream_action === 'hide_safety_buffering' ? (form.account_action === 'auto_continue' ? 'auto_continue' : 'none') : (form.account_action || 'builtin'),
      downstream_action: form.downstream_action || 'builtin',
      response_status: Number(form.response_status) || 0,
      custom_message: form.custom_message || '',
      cooldown_seconds: Number(form.cooldown_seconds) || 0,
      prefer_retry_after: !!form.prefer_retry_after,
      idle_seconds: form.idle_infinite ? -1 : Number(form.idle_seconds) || 0,
      idle_ping_seconds: Number(form.idle_ping_seconds) || 15,
      skip_log: !!form.skip_log,
      filter_account_action: form.downstream_action === 'hide_safety_buffering' ? false : !!form.filter_account_action,
      keyword_case_sensitive: !!form.keyword_case_sensitive,
      description: form.description || '',
    };
  };

  const save = async () => {
    try {
      const payload = buildPayload();
      if (editor?.mode === 'edit') await patch(`/admin/upstream-error-rules/${encodeURIComponent(editor.id)}`, payload);
      else await post('/admin/upstream-error-rules', payload);
      Toast.success('规则已保存');
      setEditor(null);
      await load();
    } catch (e) { showErrorToast(e); }
  };

  const removeRule = async (rule) => {
    if (!window.confirm(`删除规则 ${rule.name || rule.id}？`)) return;
    try { await del(`/admin/upstream-error-rules/${encodeURIComponent(rule.id)}`); Toast.success('已删除'); await load(); }
    catch (e) { showErrorToast(e); }
  };

  const toggleRule = async (rule) => {
    try { await patch(`/admin/upstream-error-rules/${encodeURIComponent(rule.id)}`, { enabled: !rule.enabled }); await load(); }
    catch (e) { showErrorToast(e); }
  };

  const runTest = async () => {
    try {
      const res = await post('/admin/upstream-error-rules/test', { ...testInput, status: Number(testInput.status) || 0 });
      setTestResult(res);
    } catch (e) { showErrorToast(e); }
  };

  return (
    <div className="upstream-rules-page">
      <PageHeader title="上游错误规则" subtitle="按状态码、响应内容、模型和入口控制上游错误处理方式" actions={<><Button icon={<IconRefresh />} onClick={load} loading={loading}>刷新</Button><Button icon={<IconPlus />} theme="solid" onClick={() => openEditor(null)}>新建规则</Button></>} />

      <section className="upstream-rule-overview">
        <div><span>已启用规则数</span><strong>{stats.enabled}</strong></div>
        <div><span>自动切换规则数</span><strong>{stats.failover}</strong></div>
        <div><span>透传规则数</span><strong>{stats.pass}</strong></div>
        <div><span>空转规则数</span><strong>{stats.idle}</strong></div>
      </section>

      <section className="upstream-rule-list">
        {rules.length === 0 ? <div className="upstream-rule-empty">暂无规则。点击“新建规则”开始配置。</div> : null}
        {rules.map((rule) => (
          <article key={rule.id} className="upstream-rule-card">
            <div className="upstream-rule-row">
              <div className="upstream-rule-priority">#{rule.priority}</div>
              <div className="upstream-rule-main">
                <div className="upstream-rule-title"><strong>{rule.name || rule.id}</strong><Tag color={rule.enabled ? 'green' : 'grey'}>{rule.enabled ? '启用' : '停用'}</Tag></div>
                <div className="upstream-rule-meta"><span>{rule.providers?.join(', ') || '全部平台'}</span><span>{rule.entrypoints?.join(', ') || '全部入口'}</span><span>{rule.model_patterns?.join(', ') || '全部模型'}</span></div>
              </div>
              <div className="upstream-rule-condition"><b>匹配条件</b><span>{rule.status_codes?.join(', ') || '任意状态'} · {rule.body_keywords?.join(', ') || '任意 body'}</span></div>
              <div className="upstream-rule-actions"><Tag>{labelOf(ACCOUNT_ACTIONS, rule.account_action)}</Tag><Tag color="blue">{labelOf(DOWNSTREAM_ACTIONS, rule.downstream_action)}</Tag></div>
              <div className="upstream-rule-ops"><Switch checked={!!rule.enabled} onChange={() => toggleRule(rule)} /><Button size="small" icon={<IconEdit />} onClick={() => openEditor(rule)}>编辑</Button><Button size="small" icon={<IconDelete />} onClick={() => removeRule(rule)}>删除</Button></div>
            </div>
            <button type="button" className="upstream-rule-summary" onClick={() => setExpanded((x) => ({ ...x, [rule.id]: !x[rule.id] }))}>命中后会发生什么</button>
            {expanded[rule.id] ? <p className="upstream-rule-natural">{humanSummary(rule)}</p> : null}
          </article>
        ))}
      </section>

      <section className="upstream-rule-test-panel">
        <div className="upstream-rule-section-head"><h2>测试与预览</h2><p>输入模拟上游错误，查看命中的规则、账号动作、下游动作和最终响应预览。</p></div>
        <div className="upstream-rule-test-grid">
          <input className="pool-input" placeholder="provider" value={testInput.provider} onChange={(e) => setTestInput((x) => ({ ...x, provider: e.target.value }))} />
          <select className="pool-select" value={testInput.entrypoint} onChange={(e) => setTestInput((x) => ({ ...x, entrypoint: e.target.value }))}>{ENTRYPOINTS.map(([id, label]) => <option key={id} value={id}>{label}</option>)}</select>
          <input className="pool-input" placeholder="model" value={testInput.model} onChange={(e) => setTestInput((x) => ({ ...x, model: e.target.value }))} />
          <input className="pool-input" type="number" placeholder="status" value={testInput.status} onChange={(e) => setTestInput((x) => ({ ...x, status: e.target.value }))} />
          <textarea className="pool-textarea" placeholder="模拟 body" value={testInput.body} onChange={(e) => setTestInput((x) => ({ ...x, body: e.target.value }))} />
          <Button icon={<IconPlay />} theme="solid" onClick={runTest}>测试匹配</Button>
        </div>
        {testResult ? <pre className="upstream-rule-preview">{JSON.stringify(testResult, null, 2)}</pre> : null}
      </section>

      <Drawer open={!!editor} onClose={() => setEditor(null)} title={editor?.mode === 'edit' ? '编辑规则' : '新建规则'} width="min(760px, 94vw)" className="upstream-rule-sheet" footer={<><Button onClick={() => setEditor(null)}>取消</Button><Button theme="solid" onClick={save}>保存规则</Button></>}>
        <div className="upstream-rule-form">
          <section><h3>A. 适用范围</h3><div className="upstream-rule-form-grid"><label><span>规则名称</span><input className="pool-input" value={form.name || ''} onChange={(e) => setForm((x) => ({ ...x, name: e.target.value }))} /></label><label><span>优先级</span><input className="pool-input" type="number" value={form.priority ?? 100} onChange={(e) => setForm((x) => ({ ...x, priority: e.target.value }))} /></label><label><span>入口类型</span><select className="pool-select" value={form.entrypoint || ''} onChange={(e) => setForm((x) => ({ ...x, entrypoint: e.target.value }))}><option value="">全部入口</option>{ENTRYPOINTS.map(([id, label]) => <option key={id} value={id}>{label}</option>)}</select></label><label><span>状态</span><Switch checked={!!form.enabled} onChange={(enabled) => setForm((x) => ({ ...x, enabled }))} /></label></div><ProviderCascade model_options={model_options} form={form} setForm={setForm} />{form.manual_pattern_enabled ? <textarea className="pool-textarea" placeholder={'gpt-5*\nclaude-sonnet-*\nmy-provider-model-*'} value={form.manual_patterns_text || ''} onChange={(e) => setForm((x) => ({ ...x, manual_patterns_text: e.target.value }))} /> : null}</section>
          <section><h3>B. 匹配条件</h3><div className="upstream-rule-form-grid"><label><span>状态码（status_codes）</span><input className="pool-input" placeholder="429, 500, 529" value={form.status_codes_text || ''} onChange={(e) => setForm((x) => ({ ...x, status_codes_text: e.target.value }))} /></label><label><span>Body 关键词（body_keywords）</span><textarea className="pool-textarea" disabled={form.downstream_action === 'hide_safety_buffering'} placeholder={form.downstream_action === 'hide_safety_buffering' ? '此动作按协议字段处理，无需填写关键词' : 'quota\noverloaded'} value={form.downstream_action === 'hide_safety_buffering' ? '' : (form.body_keywords_text || '')} onChange={(e) => setForm((x) => ({ ...x, body_keywords_text: e.target.value }))} /></label><label><span>匹配模式</span><Select value={form.match_mode} onChange={(v) => setForm((x) => ({ ...x, match_mode: v }))} optionList={[{ value: 'any', label: '任一条件命中' }, { value: 'all', label: '全部条件命中' }]} /></label></div>{form.downstream_action === 'hide_safety_buffering' ? <p className="upstream-rule-note">该动作不匹配英文提示文本，而是删除 Responses SSE 顶层 safety_buffering 字段；response.created、内容增量和 response.completed 都会继续下发。</p> : null}</section>
          <section><h3>C. 上游账号处理</h3><div className="upstream-rule-form-grid"><label><span>账号动作</span><Select disabled={form.downstream_action === 'hide_safety_buffering'} value={form.downstream_action === 'hide_safety_buffering' ? 'none' : form.account_action} onChange={(v) => setForm((x) => ({ ...x, account_action: v }))} optionList={ACCOUNT_ACTIONS.map(([value, label]) => ({ value, label }))} /></label><label><span>冷却时长（秒）</span><input className="pool-input" type="number" disabled={form.downstream_action === 'hide_safety_buffering'} value={form.cooldown_seconds || 0} onChange={(e) => setForm((x) => ({ ...x, cooldown_seconds: e.target.value }))} /></label><label><span>优先使用 Retry-After</span><Switch disabled={form.downstream_action === 'hide_safety_buffering'} checked={!!form.prefer_retry_after} onChange={(prefer_retry_after) => setForm((x) => ({ ...x, prefer_retry_after }))} /></label></div></section>
          <section><h3>D. 下游响应处理</h3><div className="upstream-rule-form-grid"><label><span>下游动作</span><Select value={form.downstream_action} onChange={(v) => setForm((x) => ({ ...x, downstream_action: v, ...(v === 'hide_safety_buffering' ? { entrypoint: 'responses', body_keywords_text: '', account_action: 'none', filter_account_action: false, idle_ping_seconds: x.idle_ping_seconds || 15 } : {}) }))} optionList={DOWNSTREAM_ACTIONS.map(([value, label]) => ({ value, label }))} /></label>{form.downstream_action === 'custom_error' ? <><label><span>响应状态码</span><input className="pool-input" type="number" value={form.response_status || 503} onChange={(e) => setForm((x) => ({ ...x, response_status: e.target.value }))} /></label><label><span>自定义错误消息</span><input className="pool-input" value={form.custom_message || ''} onChange={(e) => setForm((x) => ({ ...x, custom_message: e.target.value }))} /></label></> : null}{form.downstream_action === 'hide_safety_buffering' ? <><label><span>协议保活间隔（秒）</span><input className="pool-input" type="number" min="1" max="60" value={form.idle_ping_seconds || 15} onChange={(e) => setForm((x) => ({ ...x, idle_ping_seconds: e.target.value }))} /></label><p className="upstream-rule-note">等待安全检查期间发送 response.in_progress 心跳，同时保持 WebSocket 和 SSE 不因空闲而断开；建议 15 秒。</p></> : null}{form.downstream_action === 'idle_stream' ? <><label><span>心跳间隔</span><input className="pool-input" type="number" value={form.idle_ping_seconds || 15} onChange={(e) => setForm((x) => ({ ...x, idle_ping_seconds: e.target.value }))} /></label><label><span>空转时长</span><input className="pool-input" type="number" disabled={!!form.idle_infinite} value={form.idle_seconds || 0} onChange={(e) => setForm((x) => ({ ...x, idle_seconds: e.target.value }))} /></label><label><span>无限空转</span><Switch checked={!!form.idle_infinite} onChange={(idle_infinite) => setForm((x) => ({ ...x, idle_infinite }))} /></label><p className="upstream-rule-note">命中后会释放上游和账号资源，只保持下游 SSE 连接。</p></> : null}</div></section>
          <details><summary>高级项</summary><label><span>描述</span><textarea className="pool-textarea" value={form.description || ''} onChange={(e) => setForm((x) => ({ ...x, description: e.target.value }))} /></label><label><span>跳过日志</span><Switch checked={!!form.skip_log} onChange={(skip_log) => setForm((x) => ({ ...x, skip_log }))} /></label><label><span>关键词区分大小写</span><Switch disabled={form.downstream_action === 'hide_safety_buffering'} checked={!!form.keyword_case_sensitive} onChange={(keyword_case_sensitive) => setForm((x) => ({ ...x, keyword_case_sensitive }))} /></label><label><span>过滤命中时执行账号动作</span><Switch disabled={form.downstream_action === 'hide_safety_buffering'} checked={form.downstream_action === 'hide_safety_buffering' ? false : !!form.filter_account_action} onChange={(filter_account_action) => setForm((x) => ({ ...x, filter_account_action }))} /></label></details>
          <section><h3>E. 测试与预览</h3><p className="upstream-rule-note">保存后可在页面底部测试面板点击“测试匹配”。当前摘要：{humanSummary(buildPayload())}</p></section>
        </div>
      </Drawer>
    </div>
  );
}
