import React, { useState, useEffect, useCallback, useMemo } from 'react';
import {
  Tabs, TabPane, Card, Toast, Typography, Button, Switch, Select,
  InputNumber, Input, Tag, Banner, Form,
} from '../components/pool/index.jsx';
import { IconSave, IconRefresh } from '../components/pool/icons.jsx';
import { get, post } from '../api.js';
import LoadErrorBanner from '../components/LoadErrorBanner.jsx';
import PageHeader from '../components/PageHeader.jsx';
import SettingsTabShell from '../components/SettingsTabShell.jsx';
import { showErrorToast } from '../components/ErrorToast.jsx';
import useAsyncAction from '../hooks/useAsyncAction.js';
import useAsyncResource from '../hooks/useAsyncResource.js';
import { addWindowListener } from '../lib/browserLifecycle.js';
import { browserHash, setBrowserHash } from '../lib/browserNavigation.js';
import { loadResourceGroup } from '../lib/resource.js';

// ── helpers ──────────────────────────────────────────────────────────────────

function hasPendingValue(pending, key) {
  return Object.prototype.hasOwnProperty.call(pending, key);
}

function valueOf(f, pending) {
  return hasPendingValue(pending, f.key) ? pending[f.key] : f.value;
}

function renderControl(f, v, setVal) {
  switch (f.type) {
    case 'bool':
      return <Switch checked={!!v} onChange={(c) => setVal(f.key, c)} />;
    case 'select':
      return <Select value={v ?? ''} onChange={(x) => setVal(f.key, x)} style={{ width: 220 }}
        optionList={(f.options || []).map((o) => ({ label: o === '' ? '（空）' : o, value: o }))} />;
    case 'int':
      return <InputNumber value={Number(v) || 0} onChange={(x) => setVal(f.key, x)} style={{ width: 160 }} />;
    default:
      return <Input value={v ?? ''} onChange={(x) => setVal(f.key, x)} style={{ width: 280 }} />;
  }
}

function settingsCenterURL(...sections) {
  return `/admin/settings-center?sections=${sections.join(',')}`;
}

function ensureCurrentOption(options, current, currentLabel = '当前值') {
  const list = Array.isArray(options) ? [...options] : [];
  if (current && !list.some((o) => o?.value === current)) {
    list.push({ label: `${current}（${currentLabel}）`, value: current });
  }
  return list;
}

function runtimePatchValues(values) {
  const { settings_errors: _settingsErrors, ...rest } = values || {};
  return rest;
}

function settingsFormKey(section, values) {
  try {
    return `${section}:${JSON.stringify(values || {})}`;
  } catch {
    return `${section}:unserializable`;
  }
}

function configCategories(fields) {
  return fields.reduce((acc, f) => {
    (acc[f.category] = acc[f.category] || []).push(f);
    return acc;
  }, {});
}

function configSettingsErrors(fields) {
  return Object.fromEntries(
    fields
      .filter((f) => typeof f.settings_error === 'string' && f.settings_error.trim())
      .map((f) => [f.key, f.settings_error])
  );
}

function ConfigFieldRow({ field, pending, onChange }) {
  const pendingValue = hasPendingValue(pending, field.key);
  return (
    <div className="pool-settings-row">
      <div className="pool-settings-row__meta">
        <div className="pool-settings-row__title">
          {field.label} {field.overridden && <Tag size="small" color="blue">已覆盖</Tag>}
          {field.settings_error && <Tag size="small" color="red">存量异常</Tag>}
          {field.effect === 'restart' && <Tag size="small" color="orange">需重启</Tag>}
          {pendingValue && <Tag size="small" color="green">待保存</Tag>}
        </div>
        <Typography.Text type="tertiary" size="small" className="pool-settings-row__help">{field.help}</Typography.Text>
        {field.settings_error && (
          <Typography.Text type="danger" size="small" className="pool-settings-row__error">
            {field.settings_error}
          </Typography.Text>
        )}
      </div>
      <div className="pool-settings-row__control">{renderControl(field, valueOf(field, pending), onChange)}</div>
      <Typography.Text type="quaternary" size="small" className="pool-settings-row__key">{field.key}</Typography.Text>
    </div>
  );
}

// ── ConfigTab ────────────────────────────────────────────────────────────────

function ConfigTab() {
  const [pending, setPending] = useState({});
  const [diffs, setDiffs] = useState(null);
  const [prevSnapshot, setPrevSnapshot] = useState(null);

  const fetchConfig = useCallback(async ({ signal }) => {
    const f = await get('/admin/config', undefined, { signal });
    return Array.isArray(f) ? f : [];
  }, []);

  const {
    data: fields = [],
    loading,
    error,
    lastRefresh,
    reload: load,
  } = useAsyncResource(fetchConfig, [fetchConfig], { initialData: [] });

  const refresh = useCallback(() => {
    setPending({});
    setDiffs(null);
    load();
  }, [load]);

  const setVal = (key, v) => setPending((p) => ({ ...p, [key]: v }));

  const { run: save, running: saving } = useAsyncAction(async () => {
    const changedKeys = Object.keys(pending);
    if (changedKeys.length === 0) { Toast.info('没有改动'); return; }
    const fieldByKey = new Map(fields.map((f) => [f.key, f]));
    const oldSnap = {};
    changedKeys.forEach((key) => {
      oldSnap[key] = fieldByKey.has(key) ? fieldByKey.get(key).value : '';
    });
    setPrevSnapshot({ oldSnap, pending: { ...pending } });
    try {
      const changed = {};
      changedKeys.forEach((k) => { changed[k] = pending[k]; });
      const r = await post('/admin/settings-center', [{ section: 'config', values: changed }]);
      const savedDiffs = r?.saved || [];
      const displayDiffs = savedDiffs.length > 0 ? savedDiffs : changedKeys.map((k) => ({
        section: 'config', key: k, old_value: oldSnap[k], new_value: pending[k],
      }));
      setDiffs(displayDiffs);
      setPending({});
      Toast.success(`已保存 ${changedKeys.length} 项配置`);
      await load();
    } catch (e) { showErrorToast(e); }
  });

  const { run: undo, running: undoing } = useAsyncAction(async () => {
    if (!prevSnapshot) return;
    try {
      await post('/admin/settings-center', [{ section: 'config', values: prevSnapshot.oldSnap }]);
      Toast.success('已撤销');
      setDiffs(null);
      setPrevSnapshot(null);
      setPending({});
      await load();
    } catch (e) { showErrorToast(e); }
  });

  const cats = useMemo(() => configCategories(fields), [fields]);
  const configErrors = useMemo(() => configSettingsErrors(fields), [fields]);

  return (
    <SettingsTabShell
      className="pool-settings-shell pool-automation-shell"
      loading={loading}
      lastRefresh={lastRefresh}
      error={error}
      onRetry={load}
      diffs={diffs}
      onUndo={undo}
      undoLoading={undoing}
      onClearDiffs={() => setDiffs(null)}
      settingsErrorTitle="通用配置读取异常"
      settingsErrors={configErrors}
      toolbar={
        <>
          <Button icon={<IconRefresh />} onClick={refresh}>刷新</Button>
          <Button icon={<IconSave />} theme="solid" loading={saving} onClick={save} disabled={Object.keys(pending).length === 0}>
            保存改动 ({Object.keys(pending).length})
          </Button>
        </>
      }
    >
      {Object.entries(cats).map(([cat, fs]) => (
        <Card key={cat} className="pool-card" title={cat} style={{ marginBottom: 16 }}>
          {fs.map((f) => (
            <ConfigFieldRow key={f.key} field={f} pending={pending} onChange={setVal} />
          ))}
        </Card>
      ))}
    </SettingsTabShell>
  );
}

// ── AutomationTab ────────────────────────────────────────────────────────────

const REG_TEMPLATES = [
  { id: 'email-only', name: '仅邮箱注册 (ChatGPT)', desc: '使用邮箱 OTP，无需住宅代理', platform: 'chatgpt', method: 'node', identity_mode: 'email', egress: 'egress_direct', mail_provider: '1secmail', needs: ['mailProvider', 'mailDomains'] },
  { id: 'phone-only', name: '仅手机注册 (ChatGPT + 住宅代理)', desc: 'hero-sms 手机号 + 住宅代理', platform: 'chatgpt', method: 'node', identity_mode: 'sms', sms_provider: 'herosms', needs: ['heroSmsApiKey', 'proxyHost', 'proxyPort', 'proxyUsername', 'proxyPassword'] },
  { id: 'full', name: '邮箱+手机完整注册 (ChatGPT)', desc: '邮箱优先，手机备选', platform: 'chatgpt', method: 'node', identity_mode: 'email', mail_provider: '1secmail', sms_provider: 'herosms', needs: ['heroSmsApiKey', 'proxyHost', 'proxyPort', 'proxyUsername', 'proxyPassword', 'mailProvider', 'mailDomains'] },
  { id: 'claude', name: 'Claude 注册', desc: 'Claude 账号注册（邮箱）', platform: 'claude', method: 'node', identity_mode: 'email', mail_provider: 'cloudflare', needs: ['mailProvider', 'mailDomains'] },
];

const EMPTY_AUTOMATION = { policies: {}, stats: null, readiness: null, automationErrors: {} };

const POLICY_TYPES = [
  { type: 'refill', title: '自动补池', desc: '池内活跃账号低于阈值时，自动发起注册补充。', fields: [
    { field: 'target', label: '目标数量', type: 'number' },
    { field: 'threshold', label: '触发阈值', type: 'number' },
    { field: 'interval', label: '检查间隔(秒)', type: 'number', w: 150 },
    { field: 'identity_mode', label: '身份模式', type: 'select', options: [{ label: '手机', value: 'sms' }, { label: '邮箱', value: 'email' }], w: 140 },
    { field: 'register_method', label: '注册引擎', type: 'select', options: [{ label: 'node', value: 'node' }, { label: 'protocol_v2', value: 'protocol_v2' }, { label: 'browser_v3', value: 'browser_v3' }], w: 160 },
    { field: 'platform', label: '平台', type: 'select', options: [{ label: 'ChatGPT', value: 'chatgpt' }, { label: 'Claude', value: 'claude' }], w: 140 },
    { field: 'group', label: '分组', type: 'group_select', ph: '默认' },
    { field: 'egress', label: '出口', type: 'egress_select', ph: 'egress_direct' },
  ]},
  { type: 'plus', title: '自动升级 Plus', desc: '定期把免费账号升级到 Plus 订阅。', fields: [
    { field: 'interval', label: '间隔(秒)', type: 'number', w: 150 },
    { field: 'daily_limit', label: '每日上限', type: 'number' },
    { field: 'payment_provider', label: '支付方式', type: 'select', options: [{ label: 'GoPay', value: 'gopay' }, { label: 'PayPal', value: 'paypal' }], w: 140 },
    { field: 'egress', label: '出口', type: 'egress_select', ph: 'egress_direct' },
  ]},
  { type: 'scheduled', title: '定时注册', desc: '按计划批量注册。', fields: [
    { field: 'interval', label: '间隔(秒)', type: 'number', w: 150 },
    { field: 'count', label: '每次数量', type: 'number' },
    { field: 'group', label: '分组', type: 'group_select', ph: '默认' },
  ]},
  { type: 'health', title: '健康巡检', desc: '定期对账号做存活/封禁巡检。', fields: [
    { field: 'interval', label: '间隔(秒)', type: 'number', w: 150 },
  ]},
];

function AutomationTab({ groups, egresses }) {
  const [template, setTemplate] = useState(null);
  const [diffs, setDiffs] = useState(null);

  const fetchAutomation = useCallback(async ({ signal }) => {
    const r = await get(settingsCenterURL('automation'), undefined, { signal });
    const auto = r.automation || {};
    const map = {};
    (auto.policies || []).forEach((p) => { map[p.type] = p; });
    return {
      policies: map,
      stats: auto.stats || null,
      readiness: auto.readiness || null,
      automationErrors: { policy: auto.policy_error || '', stats: auto.stats_error || '' },
    };
  }, []);

  const {
    data: automation = EMPTY_AUTOMATION,
    loading,
    error,
    lastRefresh,
    reload: load,
  } = useAsyncResource(fetchAutomation, [fetchAutomation], { initialData: EMPTY_AUTOMATION });

  const policies = automation.policies || {};
  const readiness = automation.readiness;
  const automationErrors = automation.automationErrors || {};

  const groupOptions = groups.map((g) => ({ label: g.name, value: g.name }));
  const egressOptions = egresses.map((e) => ({ label: `${e.name || e.id} (${e.type || 'direct'})`, value: e.id }));

  const { run: applyTemplate, running: applyingTemplate } = useAsyncAction(async (tid) => {
    try {
      const r = await post('/admin/settings-center/apply-template', { template_id: tid });
      setTemplate(r);
      Toast.success(`已应用模板: ${r.name}`);
    } catch (e) { showErrorToast(e); }
  });

  const { run: savePolicy, running: policySaving } = useAsyncAction(async (type, enabled, config) => {
    try {
      const r = await post('/admin/settings-center', [{ section: 'automation', values: { policy: { type, enabled, config } } }]);
      setDiffs(r?.saved || []);
      Toast.success(`${type} 已保存`);
      await load();
      return true;
    } catch (e) {
      showErrorToast(e);
      return false;
    }
  });

  return (
    <SettingsTabShell
      loading={loading}
      lastRefresh={lastRefresh}
      error={error}
      onRetry={load}
      diffs={diffs}
      onClearDiffs={() => setDiffs(null)}
      settingsErrorTitle="自动化数据读取异常"
      settingsErrors={automationErrors}
      toolbar={<Button icon={<IconRefresh />} onClick={load}>刷新</Button>}
    >

      {/* Readiness */}
      {readiness && (
        <Banner
          className="pool-automation-readiness"
          type={readiness.ready ? 'success' : 'warning'}
          closeIcon={null}
          description={(
            <div className="pool-automation-readiness__content">
              <div className="pool-automation-readiness__title">
                {readiness.ready ? '自动注册就绪' : '自动注册未就绪'}
              </div>
              <div className="pool-automation-readiness__desc">
                {readiness.ready ? '配置完整，可自动补池。' : ((readiness.blockers || []).join('; ') || '请检查注册器、Provider 与自动化策略配置。')}
              </div>
            </div>
          )}
        />
      )}

      {/* Templates */}
      <Card className="pool-card pool-automation-template" title="快速启动模板">
        <Typography.Text type="tertiary" size="small" className="pool-template-help">
          选择一个模板预填注册配置，只需填写标注了 <Tag size="small" color="orange">必填</Tag> 的字段即可启动。
        </Typography.Text>
        <div className="pool-template-chip-row">
          {REG_TEMPLATES.map((t) => (
            <Button key={t.id} className="pool-template-chip" type={template?.id === t.id ? 'primary' : 'tertiary'} size="small"
              disabled={applyingTemplate}
              title={t.name}
              onClick={() => applyTemplate(t.id)}>
              {t.name}
            </Button>
          ))}
        </div>
        {template && (
          <div className="pool-template-preview">
            <div className="pool-template-preview__title">{template.name} — {template.description}</div>
            <div className="pool-template-preview__meta">
              <span>平台: <Tag size="small">{template.platform}</Tag></span>
              <span>引擎: <Tag size="small">{template.method || 'node'}</Tag></span>
              <span>身份: <Tag size="small">{template.identity_mode || 'email'}</Tag></span>
              <span>出口: <Tag size="small">{template.egress || '(通过代理)'}</Tag></span>
            </div>
            <div className="pool-template-preview__needs">
              <span style={{ fontWeight: 500 }}>需要填写的凭据: </span>
              {(template.needs || []).map((n) => <Tag key={n} size="small" color="orange">{n}</Tag>)}
            </div>
          </div>
        )}
      </Card>

      {/* Policies */}
      <Typography.Title heading={5} className="pool-policy-section-title">自动化策略</Typography.Title>
      {POLICY_TYPES.map((t) => {
        const policy = policies[t.type];
        const enabled = !!policy?.enabled;
        return (
          <Card key={t.type} className="pool-card pool-policy-card"
            title={<span className="pool-policy-title">{t.title} {enabled ? <Tag color="green" size="small">已启用</Tag> : <Tag size="small">已停用</Tag>}</span>}
            headerExtraContent={<Switch checked={enabled} disabled={policySaving} onChange={(v) => savePolicy(t.type, v, policy?.config || {})} />}
          >
            <Typography.Text type="tertiary" size="small" className="pool-policy-desc">{t.desc}</Typography.Text>
            <SavePolicyFields fields={t.fields} initial={policy?.config || {}} disabled={!enabled || policySaving}
              groupOptions={groupOptions} egressOptions={egressOptions}
              onSave={(config) => savePolicy(t.type, enabled, config)} />
          </Card>
        );
      })}
    </SettingsTabShell>
  );
}

function SavePolicyFields({ fields, initial, disabled, groupOptions, egressOptions, onSave }) {
  const formApiRef = React.useRef(null);
  const [changed, setChanged] = useState(false);

  const { run: save, running: saving } = useAsyncAction(async () => {
    try {
      const v = formApiRef.current ? formApiRef.current.getValues() : {};
      const config = {};
      fields.forEach((f) => {
        let val = v[f.field];
        if (val === undefined || val === '') return;
        if (f.type === 'number') val = Number(val);
        config[f.field] = val;
      });
      const ok = await onSave(config);
      if (ok !== false) setChanged(false);
    } catch (e) { showErrorToast(e); }
  });

  return (
    <div>
      <Form key={settingsFormKey('policy', initial)} getFormApi={(a) => { formApiRef.current = a; }} initValues={initial}
        onChange={() => setChanged(true)}
        className="pool-policy-form" disabled={disabled}>
        {fields.map((f) => {
          if (f.type === 'group_select') {
            return <Form.Select key={f.field} field={f.field} label={f.label} optionList={groupOptions} style={{ width: f.w || 180 }} placeholder={f.ph} />;
          }
          if (f.type === 'egress_select') {
            return <Form.Select key={f.field} field={f.field} label={f.label} optionList={egressOptions} style={{ width: f.w || 180 }} placeholder={f.ph} />;
          }
          if (f.type === 'select') {
            return <Form.Select key={f.field} field={f.field} label={f.label} optionList={f.options} style={{ width: f.w || 180 }} />;
          }
          if (f.type === 'number') {
            return <Form.InputNumber key={f.field} field={f.field} label={f.label} style={{ width: f.w || 150 }} min={0} />;
          }
          return <Form.Input key={f.field} field={f.field} label={f.label} placeholder={f.ph} style={{ width: f.w || 180 }} />;
        })}
      </Form>
      <Button className="pool-policy-save" theme="solid" icon={<IconSave />} loading={saving} onClick={save} disabled={disabled || !changed}>
        保存
      </Button>
    </div>
  );
}

// ── RegistrarTab ─────────────────────────────────────────────────────────────

const KNOWN = ['phoneCountryCode', 'mailProvider', 'mailDomains', 'proxyHost', 'proxyPort', 'proxyUsername', 'proxyPassword'];
const SMS_PROVIDER_CARDS = [
  { key: 'smsbower', name: 'SMSBower' },
  { key: 'herosms', name: 'HeroSMS' },
];
const REGISTRAR_META_KEYS = new Set(['defaults', 'registrar_error', 'defaults_error']);
const EMPTY_REGISTRAR = { cfg: {}, smsProviders: [], registrarErrors: {} };

function registrarConfigOnly(section) {
  const out = {};
  Object.entries(section || {}).forEach(([key, value]) => {
    if (!REGISTRAR_META_KEYS.has(key)) out[key] = value;
  });
  return out;
}

function RegistrarTab() {
  const [diffs, setDiffs] = useState(null);
  const [prevSnapshot, setPrevSnapshot] = useState(null);

  const fetchRegistrar = useCallback(async ({ signal }) => {
    const { values, error: groupError } = await loadResourceGroup({
      registrar: { label: '注册器配置', load: () => get(settingsCenterURL('registrar'), undefined, { signal }) },
      providers: { label: '接码平台', load: () => get('/admin/register/providers', undefined, { signal }) },
    });
    const section = values.registrar?.registrar || {};
    const providerRows = values.providers?.providers || [];
    return {
      cfg: registrarConfigOnly(section),
      smsProviders: providerRows.filter((p) => p.type === 'sms' && SMS_PROVIDER_CARDS.some((card) => card.key === p.key)),
      registrarErrors: {
        config: section.registrar_error || '',
        defaults: section.defaults_error || '',
        providers: groupError ? '接码平台配置读取失败' : '',
      },
    };
  }, []);

  const {
    data: registrar = EMPTY_REGISTRAR,
    loading,
    error,
    lastRefresh,
    reload: load,
  } = useAsyncResource(fetchRegistrar, [fetchRegistrar], { initialData: EMPTY_REGISTRAR });

  const cfg = registrar.cfg || {};
  const smsProviders = registrar.smsProviders || [];
  const registrarErrors = registrar.registrarErrors || {};

  const { run: save, running: saving } = useAsyncAction(async (values) => {
    const oldSnap = { ...cfg };
    try {
      const out = {};
      if (values.advancedJSON && values.advancedJSON.trim()) {
        let extra;
        try { extra = JSON.parse(values.advancedJSON); }
        catch { Toast.error('高级 JSON 格式错误'); return; }
        Object.assign(out, registrarConfigOnly(extra));
      }
      KNOWN.forEach((k) => {
        const val = values[k];
        out[k] = val === undefined || val === null ? '' : val;
      });
      if (typeof out.mailDomains === 'string' && out.mailDomains.trim()) {
        out.mailDomains = out.mailDomains.split(',').map((s) => s.trim()).filter(Boolean);
      }
      if (out.heroSmsCountry !== undefined && out.heroSmsCountry !== '') out.heroSmsCountry = Number(out.heroSmsCountry);
      if (out.proxyPort !== undefined && out.proxyPort !== '') out.proxyPort = Number(out.proxyPort);
      const providers = SMS_PROVIDER_CARDS.map((card) => ({
        type: 'sms',
        key: card.key,
        display_name: card.name,
        enabled: values[`${card.key}_enabled`] !== false,
        priority: Number(values[`${card.key}_priority`]) || 0,
        config: {
          api_key: values[`${card.key}_api_key`] || '',
          service: values[`${card.key}_service`] || 'dr',
        },
      }));
      await post('/admin/register/providers', { providers });
      const r = await post('/admin/settings-center', [{ section: 'registrar', mode: 'replace', values: out }]);
      setDiffs(r?.saved || [{ section: 'registrar', key: 'sms_providers', old_value: 'saved', new_value: 'saved' }]);
      setPrevSnapshot({ oldSnap, oldProviders: smsProviders });
      Toast.success('注册器凭据已保存');
      await load();
    } catch (e) { showErrorToast(e); }
  });

  const { run: undo, running: undoing } = useAsyncAction(async () => {
    if (!prevSnapshot) return;
    try {
      if (Array.isArray(prevSnapshot.oldProviders)) {
        await post('/admin/register/providers', {
          providers: prevSnapshot.oldProviders.map((row) => ({
            type: 'sms',
            key: row.key,
            display_name: row.display_name || row.key,
            enabled: row.enabled !== false,
            priority: Number(row.priority) || 0,
            config: row.config || {},
          })),
        });
      }
      await post('/admin/settings-center', [{ section: 'registrar', mode: 'replace', values: prevSnapshot.oldSnap }]);
      Toast.success('已撤销');
      setDiffs(null);
      setPrevSnapshot(null);
      await load();
    } catch (e) { showErrorToast(e); }
  });

  const known = {};
  KNOWN.forEach((k) => { if (cfg[k] !== undefined) known[k] = cfg[k]; });
  SMS_PROVIDER_CARDS.forEach((card, index) => {
    const row = smsProviders.find((p) => p.key === card.key) || {};
    const providerConfig = row.config || {};
    known[`${card.key}_enabled`] = row.enabled !== false;
    known[`${card.key}_priority`] = row.priority ?? (card.key === 'smsbower' ? 100 : 90 - index);
    known[`${card.key}_api_key`] = providerConfig.api_key || '';
    known[`${card.key}_service`] = providerConfig.service || 'dr';
  });
  if (Array.isArray(known.mailDomains)) known.mailDomains = known.mailDomains.join(', ');
  const extra = {};
  Object.keys(cfg).forEach((k) => { if (!KNOWN.includes(k)) extra[k] = cfg[k]; });
  known.advancedJSON = Object.keys(extra).length ? JSON.stringify(extra, null, 2) : '';

  return (
    <SettingsTabShell
      loading={loading}
      lastRefresh={lastRefresh}
      error={error}
      onRetry={load}
      diffs={diffs}
      onUndo={undo}
      undoLoading={undoing}
      onClearDiffs={() => setDiffs(null)}
      settingsErrorTitle="注册器配置读取异常"
      settingsErrors={registrarErrors}
    >
      <Banner type="info" closeIcon={null} style={{ marginBottom: 12 }}
        description="配置自动注册所需的全部凭据。接码平台保存到 provider_settings；住宅代理区域需与手机号国家匹配。" />
      <Form key={settingsFormKey('registrar', known)} onSubmit={save} initValues={known} labelPosition="top" style={{ display: 'flex', flexWrap: 'wrap', gap: '0 24px' }}>
        <div style={{ width: '100%', display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(280px, 1fr))', gap: 12 }}>
          {SMS_PROVIDER_CARDS.map((card) => (
            <Card key={card.key} title={card.name} className="pool-card" bodyStyle={{ display: 'flex', flexWrap: 'wrap', gap: '0 12px' }}>
              <Form.Switch field={`${card.key}_enabled`} label="启用" />
              <Form.InputNumber field={`${card.key}_priority`} label="优先级" style={{ width: 120 }} min={0} max={1000} />
              <Form.Input field={`${card.key}_api_key`} label="API Key" mode="password" style={{ width: 260 }} placeholder="接码平台密钥" />
              <Form.Input field={`${card.key}_service`} label="服务代码" style={{ width: 120 }} placeholder="dr" />
            </Card>
          ))}
        </div>
        <Form.Input field="phoneCountryCode" label="手机号国家码" style={{ width: 140 }} placeholder="BR" />
        <Form.Input field="mailProvider" label="邮箱提供商" style={{ width: 180 }} placeholder="1secmail" />
        <Form.Input field="mailDomains" label="邮箱域名(逗号分隔)" style={{ width: 360 }} placeholder="guerrillamail.com, sharklasers.com" />
        <Form.Input field="proxyHost" label="住宅代理 Host" style={{ width: 220 }} placeholder="us2.cliproxy.io" />
        <Form.Input field="proxyPort" label="代理端口" style={{ width: 120 }} placeholder="3010" />
        <Form.Input field="proxyUsername" label="代理用户名" style={{ width: 320 }} placeholder="...-region-BR-sid-xxxx-t-5" />
        <Form.Input field="proxyPassword" label="代理密码" mode="password" style={{ width: 220 }} />
        <Form.TextArea field="advancedJSON" label="高级：其它键 (JSON)" style={{ width: '100%' }} autosize
          placeholder='{ "heroSmsCountryTopN": 10 }' />
        <div style={{ width: '100%', marginTop: 8 }}>
          <Button htmlType="submit" theme="solid" icon={<IconSave />} loading={saving}>保存凭据</Button>
          <Button style={{ marginLeft: 8 }} onClick={load}>重新加载</Button>
        </div>
      </Form>
    </SettingsTabShell>
  );
}

// ── LifecycleTab ─────────────────────────────────────────────────────────────

const EMPTY_LIFECYCLE_SETTINGS = { defaults: {}, defaultsError: '' };

function LifecycleTab({ groups, egresses, providerOpts }) {
  const [diffs, setDiffs] = useState(null);

  const fetchLifecycleSettings = useCallback(async ({ signal }) => {
    const r = await get(settingsCenterURL('lifecycle'), undefined, { signal });
    const lc = r.lifecycle || {};
    return {
      defaults: lc.defaults || {},
      defaultsError: lc.defaults_error || '',
    };
  }, []);

  const {
    data: lifecycleSettings = EMPTY_LIFECYCLE_SETTINGS,
    loading,
    error,
    lastRefresh,
    reload: load,
  } = useAsyncResource(fetchLifecycleSettings, [fetchLifecycleSettings], { initialData: EMPTY_LIFECYCLE_SETTINGS });

  const defaults = lifecycleSettings.defaults || {};
  const defaultsError = lifecycleSettings.defaultsError || '';

  const { run: save, running: saving } = useAsyncAction(async (values) => {
    try {
      const r = await post('/admin/settings-center', [{ section: 'lifecycle', values: { defaults: values } }]);
      setDiffs(r?.saved || []);
      Toast.success('生命周期默认值已保存');
      await load();
    } catch (e) { showErrorToast(e); }
  });

  const groupOptions = groups.map((g) => ({ label: g.name, value: g.name }));
  const egressOptions = egresses
    .filter((e) => e && e.id)
    .map((e) => ({ label: `${e.name || e.id} (${e.type || 'direct'})`, value: e.id }));
  const smsOptions = ensureCurrentOption(providerOpts?.sms, defaults.sms);
  const mailboxOptions = ensureCurrentOption(providerOpts?.mailbox, defaults.mailbox);
  const captchaOptions = ensureCurrentOption(providerOpts?.captcha, defaults.captcha);
  const defaultGroupOptions = ensureCurrentOption(groupOptions, defaults.group);
  const defaultEgressOptions = ensureCurrentOption(egressOptions, defaults.egress);

  return (
    <SettingsTabShell
      loading={loading}
      lastRefresh={lastRefresh}
      error={error}
      onRetry={load}
      diffs={diffs}
      onClearDiffs={() => setDiffs(null)}
    >
      {defaultsError && (
        <Banner type="danger" closeIcon={null} style={{ marginBottom: 12 }}
          title="生命周期默认值读取异常" description={defaultsError} />
      )}
      <Banner type="info" closeIcon={null} style={{ marginBottom: 12 }}
        description="设置生命周期任务（批量注册/升级 Plus）的默认值，创建新任务时自动填充。" />
      <Form key={settingsFormKey('lifecycle', defaults)} onSubmit={save} initValues={defaults} labelPosition="top" style={{ display: 'flex', flexWrap: 'wrap', gap: '0 24px' }}>
        <Form.Select field="sms" label="默认短信提供商" style={{ width: 220 }} optionList={[{ label: '未设置', value: '' }, ...smsOptions]} />
        <Form.Select field="mailbox" label="默认邮箱提供商" style={{ width: 220 }} optionList={[{ label: '未设置', value: '' }, ...mailboxOptions]} />
        <Form.Select field="captcha" label="默认验证码求解器" style={{ width: 220 }} optionList={[{ label: '未设置', value: '' }, ...captchaOptions]} />
        <Form.Select field="group" label="默认分组" style={{ width: 220 }} optionList={[{ label: '默认', value: '' }, ...defaultGroupOptions]} />
        <Form.Select field="egress" label="默认出口" style={{ width: 240 }} optionList={[{ label: '未设置', value: '' }, ...defaultEgressOptions]} />
        <div style={{ width: '100%', marginTop: 8 }}>
          <Button htmlType="submit" theme="solid" icon={<IconSave />} loading={saving}>保存默认值</Button>
        </div>
      </Form>
    </SettingsTabShell>
  );
}

// ── LoggingTab ───────────────────────────────────────────────────────────────

function LoggingTab() {
  const [diffs, setDiffs] = useState(null);

  const fetchLogging = useCallback(async ({ signal }) => {
    const r = await get(settingsCenterURL('logging'), undefined, { signal });
    return r.logging || {};
  }, []);

  const {
    data: logging = {},
    loading,
    error,
    lastRefresh,
    reload: load,
  } = useAsyncResource(fetchLogging, [fetchLogging], { initialData: {} });

  const { run: save, running: saving } = useAsyncAction(async (values) => {
    try {
      const r = await post('/admin/settings-center', [{ section: 'logging', values: runtimePatchValues(values) }]);
      setDiffs(r?.saved || []);
      Toast.success('日志配置已保存');
      await load();
    } catch (e) { showErrorToast(e); }
  });

  const loggingValues = runtimePatchValues(logging);

  return (
    <SettingsTabShell
      loading={loading}
      lastRefresh={lastRefresh}
      error={error}
      onRetry={load}
      diffs={diffs}
      onClearDiffs={() => setDiffs(null)}
      settingsErrorTitle="日志配置读取异常"
      settingsErrorSection={logging}
    >
      <Banner type="info" closeIcon={null} style={{ marginBottom: 12 }}
        description="控制注册任务日志的详细程度、失败阈值和保留策略。日志过于详细可能在低配 VPS 上占用较多磁盘空间。" />
      <Form key={settingsFormKey('logging', loggingValues)} onSubmit={save} initValues={loggingValues} labelPosition="left" labelWidth={140} style={{ maxWidth: 600 }}>
        <Form.Switch field="verbose_logging" label="详尽日志" />
        <Typography.Text type="tertiary" size="small" style={{ display: 'block', marginBottom: 12, marginLeft: 140 }}>
          开启后注册全流程各步骤记录结构化日志，方便 AI 分析微调。
        </Typography.Text>
        <Form.InputNumber field="failure_threshold" label="失败率阈值" min={0.1} max={1.0} step={0.1} style={{ width: 120 }} />
        <Typography.Text type="tertiary" size="small" style={{ display: 'block', marginBottom: 12, marginLeft: 140 }}>
          最近 10 个 job 失败率超过此值时自动降级注册并发到 1（默认 0.6）。
        </Typography.Text>
        <Form.InputNumber field="log_retention_days" label="日志保留天数" min={1} max={90} style={{ width: 120 }} />
        <Typography.Text type="tertiary" size="small" style={{ display: 'block', marginBottom: 12, marginLeft: 140 }}>
          超过此天数的注册事件日志将被自动清理。
        </Typography.Text>
        <div style={{ marginTop: 8 }}>
          {logging.degraded && <Tag color="red" style={{ marginBottom: 8 }}>系统已自动降级 — 注册失败率超标</Tag>}
          <Button htmlType="submit" theme="solid" icon={<IconSave />} loading={saving}>保存</Button>
          {logging.degraded && <Button style={{ marginLeft: 8 }} loading={saving} onClick={() => save({ ...loggingValues, degraded: false })}>清除降级状态</Button>}
        </div>
      </Form>
    </SettingsTabShell>
  );
}

// ── MemoryTab ────────────────────────────────────────────────────────────────

function MemoryTab() {
  const [diffs, setDiffs] = useState(null);

  const fetchMemory = useCallback(async ({ signal }) => {
    const r = await get(settingsCenterURL('memory'), undefined, { signal });
    return r.memory || {};
  }, []);

  const {
    data: memory = {},
    loading,
    error,
    lastRefresh,
    reload: load,
  } = useAsyncResource(fetchMemory, [fetchMemory], { initialData: {} });

  const { run: save, running: saving } = useAsyncAction(async (values) => {
    try {
      const r = await post('/admin/settings-center', [{ section: 'memory', values: runtimePatchValues(values) }]);
      setDiffs(r?.saved || []);
      Toast.success('内存配置已保存');
      await load();
    } catch (e) { showErrorToast(e); }
  });

  const memoryValues = runtimePatchValues(memory);

  return (
    <SettingsTabShell
      loading={loading}
      lastRefresh={lastRefresh}
      error={error}
      onRetry={load}
      diffs={diffs}
      onClearDiffs={() => setDiffs(null)}
      settingsErrorTitle="内存配置读取异常"
      settingsErrorSection={memory}
    >
      <Banner type="warning" closeIcon={null} style={{ marginBottom: 12 }}
        description="内存配置影响系统稳定性。低配 VPS (1-2GB RAM) 建议将批量大小设为 100 以下，并发设为 5 以下。" />
      <Form key={settingsFormKey('memory', memoryValues)} onSubmit={save} initValues={memoryValues} labelPosition="left" labelWidth={180} style={{ maxWidth: 600 }}>
        <Form.InputNumber field="lifecycle_batch_size" label="健康巡检批量大小" min={10} max={1000} style={{ width: 120 }} />
        <Typography.Text type="tertiary" size="small" style={{ display: 'block', marginBottom: 12, marginLeft: 180 }}>
          每次巡检处理的账号数，分批后每批完成后释放内存。
        </Typography.Text>
        <Form.InputNumber field="lifecycle_concurrency" label="健康巡检并发数" min={1} max={50} style={{ width: 120 }} />
        <Typography.Text type="tertiary" size="small" style={{ display: 'block', marginBottom: 12, marginLeft: 180 }}>
          同时巡检的账号数上限。
        </Typography.Text>
        <Form.InputNumber field="go_memory_limit_mb" label="Go 内存软限制 (MB)" min={0} max={32768} style={{ width: 120 }} />
        <Typography.Text type="tertiary" size="small" style={{ display: 'block', marginBottom: 12, marginLeft: 180 }}>
          0 = 不限制。Go 1.19+ 的软内存限制，达到后触发更频繁的 GC。
        </Typography.Text>
        <Form.InputNumber field="reg_combined_output_cap" label="注册子进程输出截断 (字节)" min={65536} max={10485760} style={{ width: 160 }} />
        <Typography.Text type="tertiary" size="small" style={{ display: 'block', marginBottom: 12, marginLeft: 180 }}>
          注册子进程 stdout/stderr 的最大捕获字节数，防止大输出 OOM。
        </Typography.Text>
        <Button htmlType="submit" theme="solid" icon={<IconSave />} loading={saving} style={{ marginTop: 8 }}>保存</Button>
      </Form>
    </SettingsTabShell>
  );
}

// ── SettingsV2 main ──────────────────────────────────────────────────────────

const SETTINGS_TAB_KEYS = ['config', 'automation', 'registrar', 'lifecycle', 'logging', 'memory'];

function settingsTabFromHash() {
  const hash = browserHash().replace('#', '');
  return SETTINGS_TAB_KEYS.includes(hash) ? hash : 'config';
}

function tabNeedsSharedOptions(tab) {
  return tab === 'automation' || tab === 'lifecycle';
}

const EMPTY_SHARED_OPTIONS = { groups: [], egresses: [], providerOpts: { sms: [], mailbox: [], captcha: [] }, error: null };

export default function SettingsV2() {
  const [activeTab, setActiveTab] = useState(settingsTabFromHash);
  const [mountedTabs, setMountedTabs] = useState(() => [settingsTabFromHash()]);
  const [sharedOptionsRequested, setSharedOptionsRequested] = useState(false);

  useEffect(() => {
    const syncHash = () => setActiveTab(settingsTabFromHash());
    syncHash();
    return addWindowListener('hashchange', syncHash);
  }, []);

  const setTab = (tab) => {
    if (!SETTINGS_TAB_KEYS.includes(tab)) return;
    setActiveTab(tab);
    setBrowserHash(tab);
  };

  useEffect(() => {
    setMountedTabs((tabs) => (tabs.includes(activeTab) ? tabs : [...tabs, activeTab]));
  }, [activeTab]);

  const fetchSharedOptions = useCallback(async ({ signal }) => {
    const { values, error } = await loadResourceGroup({
      groups: { label: '分组选项', load: () => get('/admin/groups', undefined, { signal }) },
      egresses: { label: '出口选项', load: () => get('/admin/egress-profiles', undefined, { signal }) },
      providers: { label: '服务商选项', load: () => get('/admin/register/providers/options', undefined, { signal }) },
    });
    return {
      groups: Array.isArray(values.groups) ? values.groups : values.groups?.groups || [],
      egresses: Array.isArray(values.egresses) ? values.egresses : values.egresses?.profiles || [],
      providerOpts: values.providers || EMPTY_SHARED_OPTIONS.providerOpts,
      error,
    };
  }, []);

  const {
    data: sharedOptions = EMPTY_SHARED_OPTIONS,
    error: sharedOptionsError,
    reload: reloadSharedOptions,
  } = useAsyncResource(fetchSharedOptions, [fetchSharedOptions], { initialData: EMPTY_SHARED_OPTIONS, auto: false });

  useEffect(() => {
    if (!tabNeedsSharedOptions(activeTab) || sharedOptionsRequested) return;
    setSharedOptionsRequested(true);
    reloadSharedOptions();
  }, [activeTab, sharedOptionsRequested, reloadSharedOptions]);

  const tabContent = (key, node) => (mountedTabs.includes(key) ? node : null);
  const groups = sharedOptions.groups || [];
  const egresses = sharedOptions.egresses || [];
  const providerOpts = sharedOptions.providerOpts || EMPTY_SHARED_OPTIONS.providerOpts;

  return (
    <div>
      <PageHeader title="设置中心" subtitle="统一管理所有系统配置、注册凭据、自动化策略、生命周期、日志与内存" />
      {tabNeedsSharedOptions(activeTab) ? (
        <LoadErrorBanner error={sharedOptionsError || sharedOptions.error} onRetry={reloadSharedOptions} title="共享选项读取失败" />
      ) : null}
      <Tabs className="pool-settings-tabs" activeKey={activeTab} onChange={setTab} tabPosition="left" style={{ minHeight: 500 }}>
        <TabPane tab="通用配置" itemKey="config">
          {tabContent('config', <ConfigTab />)}
        </TabPane>
        <TabPane tab="注册自动化" itemKey="automation">
          {tabContent('automation', <AutomationTab groups={groups} egresses={egresses} />)}
        </TabPane>
        <TabPane tab="注册器凭据" itemKey="registrar">
          {tabContent('registrar', <RegistrarTab />)}
        </TabPane>
        <TabPane tab="生命周期" itemKey="lifecycle">
          {tabContent('lifecycle', <LifecycleTab groups={groups} egresses={egresses} providerOpts={providerOpts} />)}
        </TabPane>
        <TabPane tab="日志与降级" itemKey="logging">
          {tabContent('logging', <LoggingTab />)}
        </TabPane>
        <TabPane tab="内存管理" itemKey="memory">
          {tabContent('memory', <MemoryTab />)}
        </TabPane>
      </Tabs>
    </div>
  );
}
