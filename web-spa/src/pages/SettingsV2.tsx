import React, { useState, useEffect, useCallback, useMemo, type ReactNode } from 'react';
import { useLocation, useNavigate, useSearchParams } from 'react-router';
import * as PoolUI from '../components/pool/index.jsx';
import { IconChevronDown, IconSave, IconRefresh, IconSearch, IconSetting } from '../components/pool/icons.jsx';
import LoadErrorBanner from '../components/LoadErrorBanner.jsx';
import PageHeaderBase from '../components/PageHeader.jsx';
import SettingsTabShellBase from '../components/SettingsTabShell.jsx';
import ConfigFormBase from '../components/ConfigForm';
import { showErrorToast } from '../components/ErrorToast.jsx';
import {
  useApplySettingsTemplateMutation, useAutomationSettingsData, useClearContextJournalMutation, useClearLogRecordsMutation, useConfigSettingsData,
  useLoggingSettingsData, useMemorySettingsData, useRegistrarSettingsData, useSaveRegistrarMutation, useSaveSettingsMutation,
  useSharedSettingsOptions,
} from '../features/settings/queries/settings';
import type {
  AutomationPolicy, AutomationSettings, ConfigField, ProviderSetting, RegistrarSettings,
  SettingsDiff, SettingsEgress, SettingsGroup, SettingsOption, SettingsSection, SettingsTemplate, SettingsValues,
  SharedSettingsOptions,
} from '../features/settings/model/settings';
import { t } from '../lib/i18n.js';

const {
  Tabs, TabPane, Card, Toast, Typography, Button, Switch, Select, ConfirmDialog,
  InputNumber, Input, Tag, Banner, Form,
} = PoolUI as any;
const PageHeader = PageHeaderBase as any;
const SettingsTabShell = SettingsTabShellBase as any;
const ConfigForm = ConfigFormBase as any;

// ── helpers ──────────────────────────────────────────────────────────────────

function hasPendingValue(pending: SettingsValues, key: string) {
  return Object.prototype.hasOwnProperty.call(pending, key);
}

function valueOf(f: ConfigField, pending: SettingsValues) {
  return hasPendingValue(pending, f.key) ? pending[f.key] : f.value;
}

function renderControl(f: ConfigField, v: unknown, setVal: (key: string, value: unknown) => void) {
  switch (f.type) {
    case 'bool':
      return <Switch aria-label={f.label} checked={!!v} onChange={(c: boolean) => setVal(f.key, c)} />;
    case 'select':
      return <Select aria-label={f.label} value={v ?? ''} onChange={(x: unknown) => setVal(f.key, x)} style={{ width: 220 }}
        optionList={(f.options || []).map((o: string) => ({ label: o === '' ? t('settings.empty_option') : o, value: o }))} />;
    case 'int':
      return <InputNumber aria-label={f.label} value={Number(v) || 0} onChange={(x: number) => setVal(f.key, x)} style={{ width: 160 }} />;
    default:
      return <Input aria-label={f.label} value={v ?? ''} onChange={(x: unknown) => setVal(f.key, x)} style={{ width: 280 }} />;
  }
}

function ensureCurrentOption(options: Array<string | SettingsOption> | undefined, current: unknown, currentLabel = t('settings.current_value')): SettingsOption[] {
  const list = (options || []).map((option) => typeof option === 'string' ? { label: option, value: option } : option);
  const currentValue = current == null ? '' : String(current);
  if (currentValue && !list.some((option) => option.value === currentValue)) {
    list.push({ label: `${currentValue}（${currentLabel}）`, value: currentValue });
  }
  return list;
}

function runtimePatchValues(values: SettingsValues | undefined): SettingsValues {
  const { settings_errors: _settingsErrors, ...rest } = values || {};
  return rest;
}

function settingsFormKey(section: string, values: SettingsValues | undefined) {
  try {
    return `${section}:${JSON.stringify(values || {})}`;
  } catch {
    return `${section}:unserializable`;
  }
}

function configCategories(fields: ConfigField[]): Record<string, ConfigField[]> {
  return fields.reduce<Record<string, ConfigField[]>>((acc, f) => {
    (acc[f.category] = acc[f.category] || []).push(f);
    return acc;
  }, {});
}

export function filterConfigCategories(fields: ConfigField[], query: string): Array<[string, ConfigField[]]> {
  const normalized = query.trim().toLocaleLowerCase();
  return Object.entries(configCategories(fields))
    .map(([category, categoryFields]) => {
      if (!normalized) return [category, categoryFields] as [string, ConfigField[]];
      const visible = categoryFields.filter((field) => (
        [category, field.label, field.help, field.key]
          .filter(Boolean)
          .some((value) => String(value).toLocaleLowerCase().includes(normalized))
      ));
      return [category, visible] as [string, ConfigField[]];
    })
    .filter(([, categoryFields]) => categoryFields.length > 0);
}

function configSettingsErrors(fields: ConfigField[]) {
  return Object.fromEntries(
    fields
      .filter((f) => typeof f.settings_error === 'string' && f.settings_error.trim())
      .map((f) => [f.key, f.settings_error])
  );
}

export function SettingsCategorySection({
  category,
  fields,
  pending,
  onChange,
  initialOpen = false,
  forceOpen = false,
}: {
  category: string;
  fields: ConfigField[];
  pending: SettingsValues;
  onChange: (key: string, value: unknown) => void;
  initialOpen?: boolean;
  forceOpen?: boolean;
}) {
  const panelId = React.useId();
  const hasError = fields.some((field) => Boolean(field.settings_error));
  const pendingCount = fields.filter((field) => hasPendingValue(pending, field.key)).length;
  const restartCount = fields.filter((field) => field.effect === 'restart').length;
  const [expanded, setExpanded] = useState(initialOpen || hasError);
  const open = forceOpen || expanded;

  useEffect(() => {
    if (hasError) setExpanded(true);
  }, [hasError]);

  return (
    <section className="pool-settings-category" data-expanded={open ? 'true' : 'false'}>
      <button
        type="button"
        className="pool-settings-category__trigger"
        aria-expanded={open}
        aria-controls={panelId}
        onClick={() => setExpanded((value) => !value)}
      >
        <span className="pool-settings-category__leading">
          <span className="pool-settings-category__icon" aria-hidden="true"><IconChevronDown /></span>
          <span className="pool-settings-category__copy">
            <strong>{category}</strong>
            <span>{t('settings.fields_count').replace('{count}', String(fields.length))}</span>
          </span>
        </span>
        <span className="pool-settings-category__badges">
          {pendingCount > 0 ? <Tag size="small" color="green">{t('settings.pending_count').replace('{count}', String(pendingCount))}</Tag> : null}
          {hasError ? <Tag size="small" color="red">{t('settings.stored_error')}</Tag> : null}
          {restartCount > 0 ? <Tag size="small" color="orange">{t('settings.restart_count').replace('{count}', String(restartCount))}</Tag> : null}
        </span>
      </button>
      <div id={panelId} className="pool-settings-category__body" hidden={!open}>
        {fields.map((field) => (
          <ConfigFieldRow key={field.key} field={field} pending={pending} onChange={onChange} />
        ))}
      </div>
    </section>
  );
}

export function SettingsDisclosure({
  title,
  subtitle,
  badge,
  initialOpen = false,
  children,
}: {
  title: ReactNode;
  subtitle?: ReactNode;
  badge?: ReactNode;
  initialOpen?: boolean;
  children: ReactNode;
}) {
  const panelId = React.useId();
  const [expanded, setExpanded] = useState(initialOpen);
  return (
    <section className="pool-settings-category pool-settings-disclosure" data-expanded={expanded ? 'true' : 'false'}>
      <button
        type="button"
        className="pool-settings-category__trigger"
        aria-expanded={expanded}
        aria-controls={panelId}
        onClick={() => setExpanded((value) => !value)}
      >
        <span className="pool-settings-category__leading">
          <span className="pool-settings-category__icon" aria-hidden="true"><IconChevronDown /></span>
          <span className="pool-settings-category__copy">
            <strong>{title}</strong>
            {subtitle ? <span>{subtitle}</span> : null}
          </span>
        </span>
        {badge ? <span className="pool-settings-category__badges">{badge}</span> : null}
      </button>
      <div id={panelId} className="pool-settings-category__body pool-settings-disclosure__body" hidden={!expanded}>
        {children}
      </div>
    </section>
  );
}

function ConfigFieldRow({ field, pending, onChange }: { field: ConfigField; pending: SettingsValues; onChange: (key: string, value: unknown) => void }) {
  const pendingValue = hasPendingValue(pending, field.key);
  return (
    <div className="pool-settings-row">
      <div className="pool-settings-row__meta">
        <div className="pool-settings-row__title">
          {field.label} {field.overridden && <Tag size="small" color="blue">{t('settings.overridden')}</Tag>}
          {field.settings_error && <Tag size="small" color="red">{t('settings.stored_error')}</Tag>}
          {field.effect === 'restart' && <Tag size="small" color="orange">{t('settings.restart_required')}</Tag>}
          {pendingValue && <Tag size="small" color="green">{t('settings.pending')}</Tag>}
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
  const [pending, setPending] = useState<SettingsValues>({});
  const [diffs, setDiffs] = useState<SettingsDiff[] | null>(null);
  const [prevSnapshot, setPrevSnapshot] = useState<{ oldSnap: SettingsValues; pending: SettingsValues } | null>(null);
  const [query, setQuery] = useState('');

  const {
    data: fields = [],
    loading,
    error,
    lastRefresh,
    reload: load,
  } = useConfigSettingsData();
  const saveMutation = useSaveSettingsMutation();
  const undoMutation = useSaveSettingsMutation();
  const templateMutation = useApplySettingsTemplateMutation();

  const refresh = useCallback(() => {
    setPending({});
    setDiffs(null);
    load();
  }, [load]);

  const setVal = (key: string, v: unknown) => setPending((p) => ({ ...p, [key]: v }));

  const save = async () => {
    const changedKeys = Object.keys(pending);
    if (changedKeys.length === 0) { Toast.info(t('settings.no_changes')); return; }
    const fieldByKey = new Map(fields.map((f) => [f.key, f]));
    const oldSnap: SettingsValues = {};
    changedKeys.forEach((key) => {
      oldSnap[key] = fieldByKey.get(key)?.value ?? '';
    });
    setPrevSnapshot({ oldSnap, pending: { ...pending } });
    try {
      const changed: SettingsValues = {};
      changedKeys.forEach((k) => { changed[k] = pending[k]; });
      const r = await saveMutation.mutateAsync([{ section: 'config', values: changed }]);
      const savedDiffs = r?.saved || [];
      const displayDiffs = savedDiffs.length > 0 ? savedDiffs : changedKeys.map((k) => ({
        section: 'config', key: k, old_value: oldSnap[k], new_value: pending[k],
      }));
      setDiffs(displayDiffs);
      setPending({});
      Toast.success(`${t('settings.saved_prefix')} ${changedKeys.length} ${t('settings.saved_config_suffix')}`);
    } catch (e) { showErrorToast(e); }
  };

  const undo = async () => {
    if (!prevSnapshot) return;
    try {
      await undoMutation.mutateAsync([{ section: 'config', values: prevSnapshot.oldSnap }]);
      Toast.success(t('settings.undo_done'));
      setDiffs(null);
      setPrevSnapshot(null);
      setPending({});
    } catch (e) { showErrorToast(e); }
  };

  const applyOptimalTemplate = async () => {
    try {
      const r = await templateMutation.mutateAsync('optimal-stable-models-v1');
      const savedDiffs = r?.saved || [];
      const oldSnap: SettingsValues = {};
      savedDiffs.forEach((d) => {
        if (d?.section === 'config' && d?.key) oldSnap[d.key] = d.old_value;
      });
      setPrevSnapshot(savedDiffs.length > 0 ? { oldSnap, pending: {} } : null);
      setDiffs(savedDiffs);
      setPending({});
      Toast.success(`${t('settings.applied_template')}: ${r?.name || t('settings.recommended_template')}`);
    } catch (e) { showErrorToast(e); }
  };
  const saving = saveMutation.isPending;
  const undoing = undoMutation.isPending;
  const applyingTemplate = templateMutation.isPending;

  const visibleCategories = useMemo(() => filterConfigCategories(fields, query), [fields, query]);
  const configErrors = useMemo(() => configSettingsErrors(fields), [fields]);
  const searching = Boolean(query.trim());

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
      settingsErrorTitle={t('settings.general_failed')}
      settingsErrors={configErrors}
      toolbarClassName="pool-settings-commandbar"
      toolbar={
        <>
          <Input
            className="pool-settings-search"
            value={query}
            onChange={setQuery}
            prefix={<IconSearch />}
            showClear
            onClear={() => setQuery('')}
            aria-label={t('settings.search')}
            placeholder={t('settings.search_placeholder')}
          />
          <div className="pool-settings-commandbar__actions">
            <Button className="pool-settings-refresh-button" aria-label={t('common.refresh')} icon={<IconRefresh />} onClick={refresh} disabled={saving || undoing || applyingTemplate}>
              {t('common.refresh')}
            </Button>
            <Button aria-label={t('settings.apply_recommended')} icon={<IconSetting />} loading={applyingTemplate} disabled={saving || undoing} onClick={applyOptimalTemplate}>
              <span className="pool-settings-action-label--long">{t('settings.apply_recommended')}</span>
              <span className="pool-settings-action-label--short">{t('settings.apply_recommended_short')}</span>
            </Button>
            <Button aria-label={`${t('settings.save_changes')} (${Object.keys(pending).length})`} icon={<IconSave />} theme="solid" loading={saving} onClick={save} disabled={Object.keys(pending).length === 0 || applyingTemplate || undoing}>
              <span className="pool-settings-action-label--long">{t('settings.save_changes')}</span>
              <span className="pool-settings-action-label--short">{t('settings.save_short')}</span>
              {' '}({Object.keys(pending).length})
            </Button>
          </div>
        </>
      }
    >
      {visibleCategories.length > 0 ? (
        <div className="pool-settings-categories" aria-label={t('settings.sections')}>
          {visibleCategories.map(([category, categoryFields], index) => (
            <SettingsCategorySection
              key={category}
              category={category}
              fields={categoryFields}
              pending={pending}
              onChange={setVal}
              initialOpen={index === 0}
              forceOpen={searching}
            />
          ))}
        </div>
      ) : fields.length > 0 ? (
        <Card className="pool-card pool-settings-empty" title={t('settings.no_search_results')}>
          <Typography.Text type="tertiary">{t('settings.no_search_results_desc').replace('{query}', query.trim())}</Typography.Text>
        </Card>
      ) : (
        <Card className="pool-card pool-settings-empty" title={t('settings.no_general')}>
          <Typography.Text type="tertiary">{t('settings.no_general_desc')}</Typography.Text>
        </Card>
      )}
    </SettingsTabShell>
  );
}

// ── AutomationTab ────────────────────────────────────────────────────────────

interface RegistrationTemplateDefinition {
  id: string;
  name: string;
  desc: string;
  platform: string;
  method: string;
  identity_mode: string;
  egress?: string;
  mail_provider?: string;
  sms_provider?: string;
  needs: string[];
}

const REG_TEMPLATES: RegistrationTemplateDefinition[] = [
  { id: 'email-only', name: '仅邮箱注册 (ChatGPT)', desc: '使用邮箱 OTP，无需住宅代理', platform: 'chatgpt', method: 'node', identity_mode: 'email', egress: 'egress_direct', mail_provider: 'tempmail', needs: ['mailboxProvider'] },
  { id: 'phone-only', name: '仅手机注册 (ChatGPT + 住宅代理)', desc: 'hero-sms 手机号 + 住宅代理', platform: 'chatgpt', method: 'node', identity_mode: 'sms', sms_provider: 'herosms', needs: ['heroSmsApiKey', 'proxyHost', 'proxyPort', 'proxyUsername', 'proxyPassword'] },
  { id: 'full', name: '邮箱+手机完整注册 (ChatGPT)', desc: '邮箱优先，手机备选', platform: 'chatgpt', method: 'node', identity_mode: 'email', mail_provider: 'tempmail', sms_provider: 'herosms', needs: ['heroSmsApiKey', 'proxyHost', 'proxyPort', 'proxyUsername', 'proxyPassword', 'mailboxProvider'] },
];

const EMPTY_AUTOMATION: AutomationSettings = { policies: {}, stats: null, readiness: null, automationErrors: {} };

interface PolicyFieldDefinition {
  field: string;
  label: string;
  type: 'number' | 'select' | 'group_select' | 'egress_select' | 'string';
  options?: SettingsOption[];
  w?: number;
  ph?: string;
}

interface PolicyDefinition {
  type: string;
  title: string;
  desc: string;
  fields: PolicyFieldDefinition[];
}

const POLICY_TYPES: PolicyDefinition[] = [
  { type: 'refill', title: '自动补池', desc: '池内活跃账号低于阈值时，自动发起注册补充。', fields: [
    { field: 'target', label: '目标数量', type: 'number' },
    { field: 'threshold', label: '触发阈值', type: 'number' },
    { field: 'interval', label: '检查间隔(秒)', type: 'number', w: 150 },
    { field: 'identity_mode', label: '身份模式', type: 'select', options: [{ label: '手机', value: 'sms' }, { label: '邮箱', value: 'email' }], w: 140 },
    { field: 'register_method', label: '注册引擎', type: 'select', options: [{ label: 'node', value: 'node' }, { label: 'protocol_v2', value: 'protocol_v2' }, { label: 'browser_v3', value: 'browser_v3' }], w: 160 },
    { field: 'platform', label: '平台', type: 'select', options: [{ label: 'ChatGPT', value: 'chatgpt' }], w: 140 },
    { field: 'group', label: '分组', type: 'group_select', ph: '默认' },
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

function AutomationTab({ groups, egresses }: { groups: SettingsGroup[]; egresses: SettingsEgress[] }) {
  const [template, setTemplate] = useState<SettingsTemplate | null>(null);
  const [diffs, setDiffs] = useState<SettingsDiff[] | null>(null);

  const {
    data: automation = EMPTY_AUTOMATION,
    loading,
    error,
    lastRefresh,
    reload: load,
  } = useAutomationSettingsData();
  const templateMutation = useApplySettingsTemplateMutation();
  const policyMutation = useSaveSettingsMutation();

  const policies = automation.policies || {};
  const readiness = automation.readiness;
  const automationErrors = automation.automationErrors || {};

  const groupOptions = groups.map((g) => ({ label: g.name, value: g.name }));
  const egressOptions = egresses.map((e) => ({ label: `${e.name || e.id} (${e.type || 'direct'})`, value: e.id }));

  const applyTemplate = async (tid: string) => {
    try {
      const r = await templateMutation.mutateAsync(tid);
      setTemplate(r);
      Toast.success(`已应用模板: ${r.name}`);
    } catch (e) { showErrorToast(e); }
  };

  const savePolicy = async (type: string, enabled: boolean, config: SettingsValues) => {
    try {
      const r = await policyMutation.mutateAsync([{ section: 'automation', values: { policy: { type, enabled, config } } }]);
      setDiffs(r?.saved || []);
      Toast.success(`${type} 已保存`);
      return true;
    } catch (e) {
      showErrorToast(e);
      return false;
    }
  };
  const applyingTemplate = templateMutation.isPending;
  const policySaving = policyMutation.isPending;

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
            headerExtraContent={<Switch checked={enabled} disabled={policySaving} onChange={(v: boolean) => savePolicy(t.type, v, policy?.config || {})} />}
          >
            <Typography.Text type="tertiary" size="small" className="pool-policy-desc">{t.desc}</Typography.Text>
            <SavePolicyFields fields={t.fields} initial={policy?.config || {}} disabled={!enabled || policySaving}
              groupOptions={groupOptions} egressOptions={egressOptions}
              onSave={(config: SettingsValues) => savePolicy(t.type, enabled, config)} />
          </Card>
        );
      })}
    </SettingsTabShell>
  );
}

function SavePolicyFields({ fields, initial, disabled, groupOptions, egressOptions, onSave }: {
  fields: PolicyFieldDefinition[];
  initial: SettingsValues;
  disabled: boolean;
  groupOptions: SettingsOption[];
  egressOptions: SettingsOption[];
  onSave: (values: SettingsValues) => Promise<boolean>;
}) {
  const formApiRef = React.useRef<{ getValues: () => SettingsValues } | null>(null);
  const [changed, setChanged] = useState(false);

  const save = async () => {
    try {
      const v = formApiRef.current ? formApiRef.current.getValues() : {};
      const config: SettingsValues = {};
      fields.forEach((f) => {
        let val = v[f.field];
        if (val === undefined || val === '') return;
        if (f.type === 'number') val = Number(val);
        config[f.field] = val;
      });
      const ok = await onSave(config);
      if (ok !== false) setChanged(false);
    } catch (e) {
      showErrorToast(e);
    }
  };

  return (
    <div>
      <Form key={settingsFormKey('policy', initial)} getFormApi={(a: { getValues: () => SettingsValues }) => { formApiRef.current = a; }} initValues={initial}
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
      <Button className="pool-policy-save" theme="solid" icon={<IconSave />} loading={disabled && changed} onClick={save} disabled={disabled || !changed}>
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
  { key: 'smsactivate', name: 'SMS-Activate' },
  { key: 'smspool', name: 'SMSPool' },
];
const MAILBOX_PROVIDER_CARDS = [
  { key: 'tempmail', name: 'TempMail.lol（免配置）', description: '内置公共临时邮箱，无需 API Key；上游可能限制公共邮箱域名。' },
  { key: 'mailtm', name: 'mail.tm（免密钥）', description: '标准 accounts / token / messages 协议，按任务创建隔离邮箱会话。' },
  { key: 'mailgw', name: 'mail.gw（免密钥）', description: 'mail.tm 兼容协议的备用邮箱来源，可独立设置优先级。' },
  { key: 'cloudflare', name: 'Cloudflare / MoeMail', description: '推荐用于稳定批量注册，需要自建 Worker 地址和邮箱域名。' },
  { key: 'imap', name: 'IMAP 固定邮箱', description: 'Gmail、Outlook 或自有域名邮箱；固定地址通常只适合单账号注册。' },
];
const CAPTCHA_PROVIDER_CARDS = [
  { key: 'yescaptcha', name: 'YesCaptcha' },
  { key: '2captcha', name: '2Captcha' },
];
const REGISTRAR_META_KEYS = new Set(['defaults', 'registrar_error', 'defaults_error']);
const EMPTY_REGISTRAR: RegistrarSettings = {
  cfg: {}, smsProviders: [], mailboxProviders: [], captchaProviders: [], emailProviders: [], registrarErrors: {},
};
const PROVIDER_SECRET_KEY = /(?:^|_)(?:api_key|admin_token|auth_token|password|email|base_email|otp_url|client_id|client_secret)$/i;
const REGISTRAR_SECRET_KEYS = new Set(['proxyUsername', 'proxyPassword']);

export function registrarProviderPayload(values: SettingsValues): ProviderSetting[] {
  const providers: ProviderSetting[] = SMS_PROVIDER_CARDS.map((card) => ({
    type: 'sms',
    key: card.key,
    display_name: card.name,
    enabled: values[`${card.key}_enabled`] === true,
    priority: Number(values[`${card.key}_priority`]) || 0,
    config: {
      api_key: values[`${card.key}_api_key`] || '',
      service: values[`${card.key}_service`] || 'dr',
      max_price: values[`${card.key}_max_price`] || '',
    },
  }));
  providers.push(...MAILBOX_PROVIDER_CARDS.map((card) => ({
    type: 'mailbox',
    key: card.key,
    display_name: card.name,
    enabled: values[`${card.key}_enabled`] === true,
    priority: Number(values[`${card.key}_priority`]) || 0,
    config: card.key === 'cloudflare' ? {
      api_url: values.cloudflare_api_url || '',
      admin_token: values.cloudflare_admin_token || '',
      domain: values.cloudflare_domain || '',
    } : card.key === 'imap' ? {
      host: values.imap_host || '',
      port: String(Number(values.imap_port) || 993),
      email: values.imap_email || '',
      password: values.imap_password || '',
      use_tls: values.imap_tls !== false,
    } : {},
  })));
  providers.push(...CAPTCHA_PROVIDER_CARDS.map((card) => ({
    type: 'captcha',
    key: card.key,
    display_name: card.name,
    enabled: values[`${card.key}_enabled`] === true,
    priority: Number(values[`${card.key}_priority`]) || 0,
    config: { api_key: values[`${card.key}_api_key`] || '' },
  })));
  providers.push({
    type: 'email',
    key: 'hotmail_otp',
    display_name: 'Hotmail OTP',
    enabled: values.hotmail_otp_enabled === true,
    priority: Number(values.hotmail_otp_priority) || 0,
    config: {
      base_email: values.hotmail_base_email || '',
      otp_url: values.hotmail_otp_url || '',
      auth_token: values.hotmail_otp_auth_token || '',
    },
  });
  return providers;
}

function providerIdentity(provider: ProviderSetting) {
  return `${provider.type}\u0000${provider.key}`;
}

export function changedRegistrarProviders(next: ProviderSetting[], baseline: ProviderSetting[]) {
  const previous = new Map(baseline.map((provider) => [providerIdentity(provider), provider]));
  return next.filter((provider) => JSON.stringify(provider) !== JSON.stringify(previous.get(providerIdentity(provider))));
}

function providerSecretsChanged(next: ProviderSetting[], baseline: ProviderSetting[]) {
  const previous = new Map(baseline.map((provider) => [providerIdentity(provider), provider]));
  return next.some((provider) => {
    const oldConfig = previous.get(providerIdentity(provider))?.config || {};
    return Object.entries(provider.config || {}).some(([key, value]) => (
      PROVIDER_SECRET_KEY.test(key)
      && String(value ?? '').trim() !== ''
      && value !== oldConfig[key]
    ));
  });
}

function registrarSecretsChanged(next: SettingsValues, baseline: SettingsValues) {
  return [...REGISTRAR_SECRET_KEYS].some((key) => next[key] !== baseline[key]);
}

function registrarConfigOnly(section: SettingsValues | undefined): SettingsValues {
  const out: SettingsValues = {};
  Object.entries(section || {}).forEach(([key, value]) => {
    if (!REGISTRAR_META_KEYS.has(key) && !key.endsWith('_configured')) out[key] = value;
  });
  return out;
}

function RegistrarTab() {
  const [diffs, setDiffs] = useState<SettingsDiff[] | null>(null);
  const [prevSnapshot, setPrevSnapshot] = useState<{ oldSnap: SettingsValues; oldProviders: ProviderSetting[] } | null>(null);
  const [dirty, setDirty] = useState(false);

  const {
    data: registrar = EMPTY_REGISTRAR,
    loading,
    error,
    lastRefresh,
    reload: load,
  } = useRegistrarSettingsData();
  const saveMutation = useSaveRegistrarMutation();
  const undoMutation = useSaveRegistrarMutation();

  useEffect(() => {
    if (!dirty) return undefined;
    const warnBeforeUnload = (event: BeforeUnloadEvent) => {
      event.preventDefault();
      event.returnValue = '';
    };
    window.addEventListener('beforeunload', warnBeforeUnload);
    return () => window.removeEventListener('beforeunload', warnBeforeUnload);
  }, [dirty]);

  const cfg = registrar.cfg || {};
  const smsProviders = registrar.smsProviders || [];
  const mailboxProviders = registrar.mailboxProviders || [];
  const captchaProviders = registrar.captchaProviders || [];
  const emailProviders = registrar.emailProviders || [];
  const registrarErrors = registrar.registrarErrors || {};

  const save = async (values: SettingsValues) => {
    const oldSnap = registrarConfigOnly(cfg);
    try {
      const out: SettingsValues = {};
      if (typeof values.advancedJSON === 'string' && values.advancedJSON.trim()) {
        let extra: unknown;
        try { extra = JSON.parse(values.advancedJSON); }
        catch { Toast.error('高级 JSON 格式错误'); return; }
        if (!extra || typeof extra !== 'object' || Array.isArray(extra)) {
          Toast.error('高级 JSON 必须是对象');
          return;
        }
        Object.assign(out, registrarConfigOnly(extra as SettingsValues));
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
      const nextProviders = registrarProviderPayload(values);
      const baselineProviders = registrarProviderPayload(known);
      const providers = changedRegistrarProviders(nextProviders, baselineProviders);
      const r = await saveMutation.mutateAsync({ providers, values: out });
      const savedDiffs = [...(r?.saved || [])];
      if (providers.length) {
        savedDiffs.push({ section: 'registrar', key: 'providers', old_value: 0, new_value: providers.length });
      }
      setDiffs(savedDiffs);
      const containsSecretChange = providerSecretsChanged(providers, baselineProviders)
        || registrarSecretsChanged(out, oldSnap);
      if (containsSecretChange) {
        setPrevSnapshot(null);
      } else {
        const changedKeys = new Set(providers.map(providerIdentity));
        setPrevSnapshot({
          oldSnap,
          oldProviders: baselineProviders.filter((provider) => changedKeys.has(providerIdentity(provider))),
        });
      }
      setDirty(false);
      if (r?.warning || r?.reloadOk === false) {
        Toast.warning(r.warning || '配置已保存，但运行时刷新失败；服务仍使用上一份有效配置。');
      }
      Toast.success('注册器凭据已保存');
    } catch (e) { showErrorToast(e); }
  };

  const undo = async () => {
    if (!prevSnapshot) return;
    try {
      const providers = Array.isArray(prevSnapshot.oldProviders)
        ? prevSnapshot.oldProviders.map((row) => ({
            type: row.type,
            key: row.key,
            display_name: row.display_name || row.key,
            enabled: row.enabled !== false,
            priority: Number(row.priority) || 0,
            config: row.config || {},
          }))
        : [];
      await undoMutation.mutateAsync({ providers, values: prevSnapshot.oldSnap });
      Toast.success('已撤销');
      setDiffs(null);
      setPrevSnapshot(null);
      setDirty(false);
    } catch (e) { showErrorToast(e); }
  };
  const saving = saveMutation.isPending;
  const undoing = undoMutation.isPending;

  const known: SettingsValues = {};
  KNOWN.forEach((k) => { if (cfg[k] !== undefined) known[k] = cfg[k]; });
  SMS_PROVIDER_CARDS.forEach((card, index) => {
    const row = smsProviders.find((p) => p.key === card.key);
    const providerConfig = row?.config || {};
    known[`${card.key}_enabled`] = row ? row.enabled !== false : false;
    known[`${card.key}_priority`] = row?.priority ?? (card.key === 'smsbower' ? 100 : 90 - index);
    known[`${card.key}_api_key`] = providerConfig.api_key || '';
    known[`${card.key}_service`] = providerConfig.service || 'dr';
    known[`${card.key}_max_price`] = providerConfig.max_price || '';
  });
  CAPTCHA_PROVIDER_CARDS.forEach((card, index) => {
    const row = captchaProviders.find((p) => p.key === card.key);
    known[`${card.key}_enabled`] = row ? row.enabled !== false : false;
    known[`${card.key}_priority`] = row?.priority ?? (100 - index * 10);
    known[`${card.key}_api_key`] = row?.config?.api_key || '';
  });
  const hotmailOTP = emailProviders.find((p) => p.key === 'hotmail_otp');
  known.hotmail_otp_enabled = hotmailOTP ? hotmailOTP.enabled !== false : false;
  known.hotmail_otp_priority = hotmailOTP?.priority ?? 100;
  known.hotmail_base_email = hotmailOTP?.config?.base_email || '';
  known.hotmail_otp_url = hotmailOTP?.config?.otp_url || '';
  known.hotmail_otp_auth_token = '';
  MAILBOX_PROVIDER_CARDS.forEach((card, index) => {
    const row = mailboxProviders.find((p) => p.key === card.key);
    const providerConfig = row?.config || {};
    known[`${card.key}_enabled`] = row ? row.enabled !== false : false;
    known[`${card.key}_priority`] = row?.priority ?? (100 - index * 10);
    if (card.key === 'cloudflare') {
      known.cloudflare_api_url = providerConfig.api_url || '';
      known.cloudflare_admin_token = providerConfig.admin_token || providerConfig.api_key || '';
      known.cloudflare_domain = providerConfig.domain || '';
    } else if (card.key === 'imap') {
      known.imap_host = providerConfig.host || '';
      known.imap_port = providerConfig.port || 993;
      known.imap_email = providerConfig.email || '';
      known.imap_password = providerConfig.password || '';
      known.imap_tls = providerConfig.use_tls !== false;
    }
  });
  if (Array.isArray(known.mailDomains)) known.mailDomains = known.mailDomains.join(', ');
  const extra: SettingsValues = {};
  Object.keys(cfg).forEach((k) => { if (!KNOWN.includes(k) && !k.endsWith('_configured')) extra[k] = cfg[k]; });
  known.advancedJSON = Object.keys(extra).length ? JSON.stringify(extra, null, 2) : '';
  const smsEnabled = SMS_PROVIDER_CARDS.filter((card) => known[`${card.key}_enabled`] === true).length;
  const captchaEnabled = CAPTCHA_PROVIDER_CARDS.filter((card) => known[`${card.key}_enabled`] === true).length;
  const mailboxEnabled = MAILBOX_PROVIDER_CARDS.filter((card) => known[`${card.key}_enabled`] === true).length;

  return (
    <SettingsTabShell
      loading={loading}
      lastRefresh={lastRefresh}
      error={error}
      onRetry={load}
      diffs={diffs}
      onUndo={prevSnapshot ? undo : undefined}
      undoLoading={undoing}
      onClearDiffs={() => setDiffs(null)}
      settingsErrorTitle="注册器配置读取异常"
      settingsErrors={registrarErrors}
    >
      <Banner type="info" closeIcon={null} style={{ marginBottom: 12 }}
        description="邮箱和接码配置会直接保存到注册流水线实际使用的 provider_settings；住宅代理区域需与手机号国家匹配。" />
      {dirty ? (
        <Banner type="warning" closeIcon={null} style={{ marginBottom: 12 }}
          title="有未保存的更改"
          description="保存后才会应用到注册流水线；为防止误丢失，保存前已暂停重新加载。" />
      ) : null}
      <Form key={settingsFormKey('registrar', known)} onSubmit={save} onValueChange={() => setDirty(true)} initValues={known} labelPosition="top" className="pool-registrar-form">
        <SettingsDisclosure
          title="接码平台"
          subtitle={`已启用 ${smsEnabled} / ${SMS_PROVIDER_CARDS.length}`}
          badge={<Tag size="small">{SMS_PROVIDER_CARDS.length} 个提供商</Tag>}
        >
          <div className="pool-registrar-card-grid">
            {SMS_PROVIDER_CARDS.map((card) => (
              <Card key={card.key} title={card.name} className="pool-card pool-registrar-provider-card" bodyStyle={{ display: 'flex', flexWrap: 'wrap', gap: '0 12px' }}>
                <Form.Switch field={`${card.key}_enabled`} label="启用" />
                <Form.InputNumber field={`${card.key}_priority`} label="优先级" style={{ width: 120 }} min={0} max={1000} />
                <Form.Input field={`${card.key}_api_key`} label="API Key" mode="password" style={{ width: 260 }} placeholder="接码平台密钥" />
                <Form.Input field={`${card.key}_service`} label="服务代码" style={{ width: 120 }} placeholder="dr" />
                {card.key === 'smspool' && <Form.Input field={`${card.key}_max_price`} label="最高单价" style={{ width: 120 }} placeholder="0.20" />}
              </Card>
            ))}
          </div>
        </SettingsDisclosure>

        <SettingsDisclosure
          title="验证码求解器"
          subtitle={`已启用 ${captchaEnabled} / ${CAPTCHA_PROVIDER_CARDS.length}`}
          badge={<Tag size="small">{CAPTCHA_PROVIDER_CARDS.length} 个提供商</Tag>}
        >
          <div className="pool-registrar-card-grid">
            {CAPTCHA_PROVIDER_CARDS.map((card) => (
              <Card key={card.key} title={card.name} className="pool-card pool-registrar-provider-card" bodyStyle={{ display: 'flex', flexWrap: 'wrap', gap: '0 12px' }}>
                <Form.Switch field={`${card.key}_enabled`} label="启用" />
                <Form.InputNumber field={`${card.key}_priority`} label="优先级" style={{ width: 120 }} min={0} max={1000} />
                <Form.Input field={`${card.key}_api_key`} label="API Key" mode="password" style={{ width: 260 }} />
              </Card>
            ))}
          </div>
        </SettingsDisclosure>

        <SettingsDisclosure
          title="Hotmail OTP"
          subtitle="protocol_v2 / browser_v3 邮箱验证码"
          badge={<Tag size="small" color={known.hotmail_otp_enabled === true ? 'green' : undefined}>{known.hotmail_otp_enabled === true ? '已启用' : '已停用'}</Tag>}
        >
          <Card title="Hotmail OTP Reader" className="pool-card pool-registrar-provider-card" bodyStyle={{ display: 'flex', flexWrap: 'wrap', gap: '0 12px' }}>
            <Form.Switch field="hotmail_otp_enabled" label="启用" />
            <Form.InputNumber field="hotmail_otp_priority" label="优先级" style={{ width: 120 }} min={0} max={1000} />
            <Form.Input field="hotmail_base_email" label="基础邮箱" style={{ width: 260 }}
              placeholder={hotmailOTP?.config?.base_email_configured ? '已加密配置；留空保留' : 'account@outlook.com'} />
            <Form.Input field="hotmail_otp_url" label="OTP Reader URL" style={{ width: 360 }}
              placeholder={hotmailOTP?.config?.otp_url_configured ? '已加密配置；留空保留' : 'https://otp.example.com/read'} />
            <Form.Input field="hotmail_otp_auth_token" label="OTP Relay Token" mode="password" style={{ width: 260 }}
              placeholder={hotmailOTP?.config?.auth_token_configured ? '已加密配置；留空保留' : 'Bearer token'} />
          </Card>
        </SettingsDisclosure>

        <SettingsDisclosure
          title="邮箱提供商"
          subtitle={`已启用 ${mailboxEnabled} / ${MAILBOX_PROVIDER_CARDS.length}`}
          badge={<Tag size="small">{MAILBOX_PROVIDER_CARDS.length} 个提供商</Tag>}
        >
          <div className="pool-registrar-card-grid">
            {MAILBOX_PROVIDER_CARDS.map((card) => (
              <Card key={card.key} title={card.name} className="pool-card pool-registrar-provider-card" bodyStyle={{ display: 'flex', flexWrap: 'wrap', gap: '0 12px' }}>
                <Typography.Text type="tertiary" size="small" style={{ width: '100%', marginBottom: 8 }}>{card.description}</Typography.Text>
                <Form.Switch field={`${card.key}_enabled`} label="启用" />
                <Form.InputNumber field={`${card.key}_priority`} label="优先级" style={{ width: 120 }} min={0} max={1000} />
                {card.key === 'cloudflare' && <>
                  <Form.Input field="cloudflare_api_url" label="Worker API URL" style={{ width: 280 }} placeholder="https://mail.example.com" />
                  <Form.Input field="cloudflare_domain" label="邮箱域名" style={{ width: 220 }} placeholder="example.com" />
                  <Form.Input field="cloudflare_admin_token" label="Admin Token（可选）" mode="password" style={{ width: 260 }} />
                </>}
                {card.key === 'imap' && <>
                  <Form.Input field="imap_host" label="IMAP Host" style={{ width: 220 }} placeholder="imap.gmail.com" />
                  <Form.InputNumber field="imap_port" label="端口" style={{ width: 120 }} min={1} max={65535} />
                  <Form.Input field="imap_email" label="邮箱地址" style={{ width: 260 }} />
                  <Form.Input field="imap_password" label="密码 / App Password" mode="password" style={{ width: 260 }} />
                  <Form.Switch field="imap_tls" label="TLS" />
                </>}
              </Card>
            ))}
          </div>
        </SettingsDisclosure>

        <SettingsDisclosure
          title="住宅代理与高级选项"
          subtitle="国家匹配、代理凭据与扩展 JSON"
          badge={<Tag size="small" color="orange">敏感配置</Tag>}
        >
          <div className="pool-registrar-fields">
            <Form.Input field="phoneCountryCode" label="手机号国家码" style={{ width: 140 }} placeholder="BR" />
            <Form.Input field="proxyHost" label="住宅代理 Host" style={{ width: 220 }} placeholder="us2.cliproxy.io" />
            <Form.Input field="proxyPort" label="代理端口" style={{ width: 120 }} placeholder="3010" />
            <Form.Input field="proxyUsername" label="代理用户名" style={{ width: 320 }}
              placeholder={cfg.proxyUsername_configured ? '已加密配置；留空保留' : '...-region-BR-sid-xxxx-t-5'} />
            <Form.Input field="proxyPassword" label="代理密码" mode="password" style={{ width: 220 }}
              placeholder={cfg.proxyPassword_configured ? '已加密配置；留空保留' : '输入代理密码'} />
            <Form.TextArea field="advancedJSON" label="高级：其它键 (JSON)" className="pool-registrar-advanced" style={{ width: '100%' }} autosize
              placeholder='{ "heroSmsCountryTopN": 10 }' />
          </div>
        </SettingsDisclosure>

        <div className="pool-registrar-actions">
          <Button htmlType="submit" theme="solid" icon={<IconSave />} loading={saving} disabled={!dirty}>保存凭据</Button>
          <Button onClick={load} disabled={dirty}>重新加载</Button>
        </div>
      </Form>
    </SettingsTabShell>
  );
}

// ── LoggingTab ───────────────────────────────────────────────────────────────

function LoggingTab() {
  const [diffs, setDiffs] = useState<SettingsDiff[] | null>(null);
  const [clearOpen, setClearOpen] = useState(false);
  const [contextClearOpen, setContextClearOpen] = useState(false);

  const {
    data: logging = {},
    loading,
    error,
    lastRefresh,
    reload: load,
  } = useLoggingSettingsData();
  const saveMutation = useSaveSettingsMutation();
  const clearMutation = useClearLogRecordsMutation();
  const contextClearMutation = useClearContextJournalMutation();

  const save = async (values: SettingsValues) => {
    try {
      const r = await saveMutation.mutateAsync([{ section: 'logging', values: runtimePatchValues(values) }]);
      setDiffs(r?.saved || []);
      Toast.success('日志配置已保存');
    } catch (e) { showErrorToast(e); }
  };
  const saving = saveMutation.isPending;

  const clearLogs = async () => {
    try {
      const result = await clearMutation.mutateAsync(undefined);
      setClearOpen(false);
      const reclaimed = result.space_reclaimed ? '，数据库空间已回收' : '，记录已删除但数据库压缩未完成';
      Toast.success(`已清空 ${result.deleted_total.toLocaleString()} 条日志${reclaimed}`);
      if (result.reclaim_warning) Toast.warning(result.reclaim_warning);
    } catch (e) { showErrorToast(e); }
  };

  const clearContexts = async () => {
    try {
      const result = await contextClearMutation.mutateAsync(undefined);
      setContextClearOpen(false);
      const reclaimed = result.space_reclaimed ? '，数据库空间已回收' : '，上下文已删除但数据库压缩未完成';
      Toast.success(`已清空 ${result.deleted_contexts.toLocaleString()} 条加密上下文${reclaimed}`);
      if (result.reclaim_warning) Toast.warning(result.reclaim_warning);
    } catch (e) { showErrorToast(e); }
  };

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
        description="控制结构化日志的详细程度、失败阈值和统一保留策略。系统每天清理超过保留天数的审计、用量、代理、注册、生命周期和已结束计费记录，并每 7 天压缩一次数据库。" />
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
          默认 7 天。超过此天数的审计、Cloudflare、用量、代理、注册、生命周期日志及已结束计费记录会被自动清理；进行中的计费 hold 不受影响。
        </Typography.Text>
        <div style={{ marginTop: 8 }}>
          {Boolean(logging.degraded) && <Tag color="red" style={{ marginBottom: 8 }}>系统已自动降级 — 注册失败率超标</Tag>}
          <Button htmlType="submit" theme="solid" icon={<IconSave />} loading={saving}>保存</Button>
          {Boolean(logging.degraded) && <Button style={{ marginLeft: 8 }} loading={saving} onClick={() => save({ ...loggingValues, degraded: false })}>清除降级状态</Button>}
        </div>
      </Form>
      <Card title="磁盘空间清理" className="pool-card" style={{ maxWidth: 600, marginTop: 20 }}>
        <Typography.Text type="tertiary" size="small" style={{ display: 'block', marginBottom: 12 }}>
          立即删除全部可清理日志并执行 SQLite WAL 截断和 VACUUM。进行中的请求计费 hold 会保留，账号、配置和凭据不会删除。
        </Typography.Text>
        <Button type="danger" loading={clearMutation.isPending} onClick={() => setClearOpen(true)}>清空全部日志</Button>
      </Card>
      <Card title="上下文磁盘回收" className="pool-card" style={{ maxWidth: 600, marginTop: 20 }}>
        <Typography.Text type="tertiary" size="small" style={{ display: 'block', marginBottom: 12 }}>
          加密 Responses 重建上下文默认保留 1 小时。此操作会清空 SQLite context_journal 表并执行 WAL 截断和 VACUUM，账号、凭据、用量和审计日志不受影响。
        </Typography.Text>
        <Button type="danger" loading={contextClearMutation.isPending} onClick={() => setContextClearOpen(true)}>清空上下文并回收空间</Button>
      </Card>
      <ConfirmDialog
        open={clearOpen}
        title="确认清空全部日志？"
        description={(
          <div className="pool-confirm-copy">
            <p>此操作不可撤销，将删除全部审计、Cloudflare、用量、代理、注册和生命周期日志，以及已结束的计费 hold。</p>
            <p>进行中的计费 hold、账号、配置、凭据和模型能力数据会保留。</p>
          </div>
        )}
        confirmText="确认清空"
        cancelText="取消"
        destructive
        onCancel={() => setClearOpen(false)}
        onConfirm={clearLogs}
      />
      <ConfirmDialog
        open={contextClearOpen}
        title="确认清空全部上下文？"
        description={(
          <div className="pool-confirm-copy">
            <p>此操作不可撤销，将删除所有用于跨账号重建的加密 Responses 上下文，并立即压缩 SQLite 数据库。</p>
            <p>已有会话后续换号时可能进入 degraded 模式；账号、凭据、用量和审计日志不会删除。</p>
          </div>
        )}
        confirmText="清空并回收"
        cancelText="取消"
        destructive
        onCancel={() => setContextClearOpen(false)}
        onConfirm={clearContexts}
      />
    </SettingsTabShell>
  );
}

// ── MemoryTab ────────────────────────────────────────────────────────────────

function MemoryTab() {
  const [diffs, setDiffs] = useState<SettingsDiff[] | null>(null);

  const {
    data: memory = {},
    loading,
    error,
    lastRefresh,
    reload: load,
  } = useMemorySettingsData();
  const saveMutation = useSaveSettingsMutation();

  const save = async (values: SettingsValues) => {
    try {
      const r = await saveMutation.mutateAsync([{ section: 'memory', values: runtimePatchValues(values) }]);
      setDiffs(r?.saved || []);
      Toast.success('内存配置已保存');
    } catch (e) { showErrorToast(e); }
  };
  const saving = saveMutation.isPending;

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

const SETTINGS_TAB_KEYS = ['config', 'automation', 'registrar', 'logging', 'memory', 'thinking', 'moderation'] as const;
type SettingsTabKey = typeof SETTINGS_TAB_KEYS[number];

function isSettingsTabKey(tab: string | null): tab is SettingsTabKey {
  return tab != null && (SETTINGS_TAB_KEYS as readonly string[]).includes(tab);
}

function tabNeedsSharedOptions(tab: SettingsTabKey) {
  return tab === 'automation';
}

const EMPTY_SHARED_OPTIONS: SharedSettingsOptions = { groups: [], egresses: [], providerOpts: { sms: [], mailbox: [], captcha: [] }, error: null };

export default function SettingsV2() {
  const location = useLocation();
  const navigate = useNavigate();
  const [searchParams, setSearchParams] = useSearchParams();
  const requestedTab = searchParams.get('tab');
  const activeTab: SettingsTabKey = isSettingsTabKey(requestedTab) ? requestedTab : 'config';
  const [mountedTabs, setMountedTabs] = useState<SettingsTabKey[]>(() => [activeTab]);

  useEffect(() => {
    const legacyTab = location.hash.replace('#', '');
    if (!searchParams.has('tab') && isSettingsTabKey(legacyTab)) {
      navigate({ pathname: location.pathname, search: `?tab=${legacyTab}` }, { replace: true });
    }
  }, [location.hash, location.pathname, navigate, searchParams]);

  const setTab = (tab: string) => {
    if (!isSettingsTabKey(tab)) return;
    const next = new URLSearchParams(searchParams);
    next.set('tab', tab);
    setSearchParams(next, { replace: true });
  };

  useEffect(() => {
    setMountedTabs((tabs) => (tabs.includes(activeTab) ? tabs : [...tabs, activeTab]));
  }, [activeTab]);

  const {
    data: sharedOptions = EMPTY_SHARED_OPTIONS,
    error: sharedOptionsError,
    reload: reloadSharedOptions,
  } = useSharedSettingsOptions(tabNeedsSharedOptions(activeTab));

  const tabContent = (key: SettingsTabKey, node: ReactNode) => (mountedTabs.includes(key) ? node : null);
  const groups = sharedOptions.groups || [];
  const egresses = sharedOptions.egresses || [];

  return (
    <div>
      <PageHeader title={t('settings.title')} subtitle={t('settings.subtitle')} />
      {tabNeedsSharedOptions(activeTab) ? (
        <LoadErrorBanner error={sharedOptionsError || sharedOptions.error} onRetry={reloadSharedOptions} title={t('settings.shared_options_failed')} />
      ) : null}
      <Tabs keepMounted className="pool-settings-tabs" activeKey={activeTab} onChange={setTab} tabPosition="left" style={{ minHeight: 500 }}>
        <TabPane tab={t('settings.general_tab')} itemKey="config">
          {tabContent('config', <ConfigTab />)}
        </TabPane>
        <TabPane tab={t('settings.automation_tab')} itemKey="automation">
          {tabContent('automation', <AutomationTab groups={groups} egresses={egresses} />)}
        </TabPane>
        <TabPane tab={t('settings.registrar_tab')} itemKey="registrar">
          {tabContent('registrar', <RegistrarTab />)}
        </TabPane>
        <TabPane tab={t('settings.logging_tab')} itemKey="logging">
          {tabContent('logging', <LoggingTab />)}
        </TabPane>
        <TabPane tab={t('settings.memory_tab')} itemKey="memory">
          {tabContent('memory', <MemoryTab />)}
        </TabPane>
        <TabPane tab={t('settings.thinking_tab')} itemKey="thinking">
          {tabContent('thinking', <ConfigForm embedded title={t('settings.thinking_title')} subtitle={t('settings.thinking_subtitle')} kind="thinking" />)}
        </TabPane>
        <TabPane tab={t('settings.moderation_tab')} itemKey="moderation">
          {tabContent('moderation', <ConfigForm embedded title={t('settings.moderation_title')} subtitle={t('settings.moderation_subtitle')} kind="moderation" />)}
        </TabPane>
      </Tabs>
    </div>
  );
}
