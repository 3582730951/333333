import { useCallback, useEffect, useMemo, useState } from 'react';
import { useLocation, useNavigate } from 'react-router';
import { Banner, Button, Card, ConfirmDialog, Input, InputNumber, Select, Switch, Tag, Toast, Typography } from '../components/pool/index.jsx';
import LoadErrorBanner from '../components/LoadErrorBanner.jsx';
import PageHeaderBase from '../components/PageHeader.jsx';
import { showErrorToast } from '../components/ErrorToast.jsx';
import { IconRefresh, IconSave } from '../components/pool/icons.jsx';
import { useAIConfigSettingsData, useSaveSettingsMutation } from '../features/settings/queries/settings';
import type { AISettingsDomain, ConfigField, SettingsValues } from '../features/settings/model/settings';
import { t } from '../lib/i18n.js';
import { addWindowListener, requestBrowserAnimationFrame } from '../lib/browserLifecycle.js';
import { getDocumentElementById } from '../lib/browserDocument.js';
import { dispatchBrowserEvent } from '../lib/browserEvents.js';

const PageHeader = PageHeaderBase as any;

const CODEX_CLIENT_POLICY_FIELDS = new Set([
  'codex_install_effort',
  'codex_install_approval_policy',
  'codex_install_sandbox_mode',
]);

function selectOptionLabel(field: ConfigField, option: string) {
  if (option !== '') return option;
  return CODEX_CLIENT_POLICY_FIELDS.has(field.key)
    ? t('settings.preserve_codex_config')
    : t('settings.empty_option');
}

export const AI_SETTINGS_SECTIONS = [
  { domain: 'chatgpt', slug: 'chatgpt', label: 'ChatGPT' },
  { domain: 'claude', slug: 'claude', label: 'Claude' },
  { domain: 'kiro', slug: 'kiro', label: 'Kiro' },
  { domain: 'antigravity', slug: 'antigravity', label: 'Antigravity' },
  { domain: 'codex', slug: 'codex', label: 'Codex' },
  { domain: 'claude_code', slug: 'claude-code', label: 'Claude Code' },
] as const satisfies ReadonlyArray<{ domain: AISettingsDomain; slug: string; label: string }>;

const DOMAIN_COPY: Record<AISettingsDomain, { title: string; subtitleKey: string }> = {
  chatgpt: { title: 'ChatGPT', subtitleKey: 'ai_settings.chatgpt_subtitle' },
  claude: { title: 'Claude', subtitleKey: 'ai_settings.claude_subtitle' },
  kiro: { title: 'Kiro', subtitleKey: 'ai_settings.kiro_subtitle' },
  antigravity: { title: 'Antigravity', subtitleKey: 'ai_settings.antigravity_subtitle' },
  codex: { title: 'Codex', subtitleKey: 'ai_settings.codex_subtitle' },
  claude_code: { title: 'Claude Code', subtitleKey: 'ai_settings.claude_code_subtitle' },
};

function domainFromPath(pathname: string): AISettingsDomain {
  const slug = pathname.split('/').filter(Boolean).at(-1) || 'chatgpt';
  return AI_SETTINGS_SECTIONS.find((section) => section.slug === slug)?.domain ?? 'chatgpt';
}

function hasOwnValue(values: SettingsValues, key: string) {
  return Object.prototype.hasOwnProperty.call(values, key);
}

function fieldValue(field: ConfigField, pending: SettingsValues) {
  return hasOwnValue(pending, field.key) ? pending[field.key] : field.value;
}


function fieldControl(
  field: ConfigField,
  value: unknown,
  onChange: (key: string, value: unknown) => void,
  controlId: string,
  describedBy?: string,
) {
  const accessibilityProps = {
    id: controlId,
    'aria-describedby': describedBy,
    disabled: field.effect === 'restart',
  };
  switch (field.type) {
    case 'bool':
      return <Switch {...accessibilityProps} checked={Boolean(value)} onChange={(next: boolean) => onChange(field.key, next)} />;
    case 'select':
      return (
        <Select
          {...accessibilityProps}
          aria-label={field.label}
          value={value ?? ''}
          onChange={(next: unknown) => onChange(field.key, next)}
          optionList={(field.options || []).map((option) => ({ label: selectOptionLabel(field, option), value: option }))}
          style={{ width: 240 }}
        />
      );
    case 'int':
      return (
        <InputNumber
          {...accessibilityProps}
          aria-label={field.label}
          value={Number(value) || 0}
          min={field.key === 'codex_install_context_window' ? 0 : undefined}
          max={field.key === 'codex_install_context_window' ? 4194304 : undefined}
          step={field.key === 'codex_install_context_window' ? 1024 : undefined}
          onChange={(next: number) => onChange(field.key, next)}
          style={{ width: field.key === 'codex_install_context_window' ? 240 : 180 }}
        />
      );
    default:
      return <Input {...accessibilityProps} aria-label={field.label} value={value ?? ''} onChange={(next: unknown) => onChange(field.key, next)} style={{ width: 300 }} />;
  }
}

function ConfigFieldRow({ field, pending, onChange }: { field: ConfigField; pending: SettingsValues; onChange: (key: string, value: unknown) => void }) {
  const changed = hasOwnValue(pending, field.key);
  const controlId = `setting-control-${field.key}`;
  const helpId = field.help ? `setting-help-${field.key}` : undefined;
  const errorId = field.settings_error ? `setting-error-${field.key}` : undefined;
  const describedBy = [helpId, errorId].filter(Boolean).join(' ') || undefined;
  return (
    <div className="pool-settings-row pool-ai-settings-row" id={`setting-${field.key}`} data-setting-key={field.key}>
      <div className="pool-settings-row__meta">
        <label className="pool-settings-row__title" htmlFor={controlId}>
          {field.label}
          {field.scope === 'global' ? <Tag size="small" color="blue">{t('ai_settings.global')}</Tag> : null}
          {field.overridden ? <Tag size="small" color="blue">{t('settings.overridden')}</Tag> : null}
          {field.effect === 'restart' ? <Tag size="small" color="orange">{t('settings.restart_required')}</Tag> : null}
          {changed ? <Tag size="small" color="green">{t('settings.pending')}</Tag> : null}
        </label>
        {field.help ? <Typography.Text id={helpId} type="tertiary" size="small" className="pool-settings-row__help">{field.help}</Typography.Text> : null}
        {field.settings_error ? <Typography.Text id={errorId} type="danger" size="small" className="pool-settings-row__error">{field.settings_error}</Typography.Text> : null}
        <Typography.Text type="quaternary" size="small" className="pool-settings-row__key">{field.key}</Typography.Text>
      </div>
      <div className="pool-settings-row__control">{fieldControl(field, fieldValue(field, pending), onChange, controlId, describedBy)}</div>
    </div>
  );
}

export default function AISettings() {
  const location = useLocation();
  const navigate = useNavigate();
  const domain = domainFromPath(location.pathname);
  const copy = DOMAIN_COPY[domain];
  const [pending, setPending] = useState<SettingsValues>({});
  const [leaveTarget, setLeaveTarget] = useState<string | null>(null);
  const { data: responseFields = [], loading, error, reload } = useAIConfigSettingsData(domain);
  const saveMutation = useSaveSettingsMutation();
  const fields = useMemo(
    () => responseFields
      .filter((field) => field.placement === 'ai_settings' && field.domain === domain)
      .sort((left, right) => left.order - right.order || left.key.localeCompare(right.key)),
    [domain, responseFields],
  );
  const groups = useMemo(() => fields.reduce<Record<string, ConfigField[]>>((result, field) => {
    const section = field.section || field.category || 'general';
    (result[section] ||= []).push(field);
    return result;
  }, {}), [fields]);
  const dirty = Object.keys(pending).length > 0;

  useEffect(() => {
    setPending({});
    setLeaveTarget(null);
  }, [domain]);

  useEffect(() => {
    const onBeforeUnload = (event: BeforeUnloadEvent) => {
      if (!dirty) return;
      event.preventDefault();
      event.returnValue = '';
    };
    return addWindowListener('beforeunload', onBeforeUnload);
  }, [dirty]);

  useEffect(() => {
    dispatchBrowserEvent('pool-ai-settings-dirty', dirty);
  }, [dirty]);

  useEffect(() => () => {
    dispatchBrowserEvent('pool-ai-settings-dirty', false);
  }, []);

  useEffect(() => {
    if (!fields.length || !location.hash) return;
    const anchor = decodeURIComponent(location.hash.slice(1));
    const normalized = anchor.replace(/^setting-/, '').replaceAll('-', '_');
    const target = fields.find((field) => field.key === anchor || field.key === normalized)
      ?? fields.find((field) => field.section === anchor || field.section.replaceAll('_', '-') === anchor)
      ?? (anchor === 'model' ? fields.find((field) => field.scope === 'model') : undefined);
    if (!target) return;
    requestBrowserAnimationFrame(() => getDocumentElementById(`setting-${target.key}`)?.scrollIntoView({ block: 'center' }));
  }, [fields, location.hash]);

  const changeField = useCallback((key: string, value: unknown) => {
    setPending((current) => ({ ...current, [key]: value }));
  }, []);

  const openSection = (path: string) => {
    if (path === location.pathname) return;
    if (dirty) {
      setLeaveTarget(path);
      return;
    }
    navigate(path);
  };

  const save = async () => {
    if (!dirty) return;
    try {
      const values = { ...pending };
      await saveMutation.mutateAsync([{ section: 'config', values }]);
      setPending({});
      Toast.success(`${t('settings.saved_prefix')} ${Object.keys(values).length} ${t('settings.saved_config_suffix')}`);
    } catch (saveError) {
      showErrorToast(saveError);
    }
  };

  const refresh = () => {
    if (dirty && !window.confirm(t('ai_settings.discard_refresh'))) return;
    setPending({});
    reload();
  };

  return (
    <div className="pool-ai-settings-page">
      <PageHeader
        title={t('ai_settings.title')}
        subtitle={t('ai_settings.subtitle')}
        actions={(
          <>
            <Button icon={<IconRefresh />} onClick={refresh}>{t('common.refresh')}</Button>
            <Button icon={<IconSave />} theme="solid" loading={saveMutation.isPending} disabled={!dirty} onClick={save}>
              {t('settings.save_changes')} ({Object.keys(pending).length})
            </Button>
          </>
        )}
      />
      <div className="pool-ai-settings-layout">
        <nav className="pool-ai-settings-nav" aria-label={t('ai_settings.secondary_nav')}>
          {AI_SETTINGS_SECTIONS.map((section) => {
            const active = section.domain === domain;
            return (
              <button
                key={section.domain}
                type="button"
                className={active ? 'is-active' : ''}
                aria-current={active ? 'page' : undefined}
                onClick={() => openSection(`/settings/ai/${section.slug}`)}
              >
                {section.label}
              </button>
            );
          })}
        </nav>
        <div className="pool-ai-settings-content">
          <div className="pool-ai-settings-heading">
            <div>
              <Typography.Title heading={2} style={{ margin: 0 }}>{copy.title}</Typography.Title>
              <Typography.Text type="tertiary" className="pool-ai-settings-heading__subtitle">{t(copy.subtitleKey)}</Typography.Text>
              <Typography.Text type="quaternary" size="small" className="pool-ai-settings-heading__count">
                {t('ai_settings.field_count').replace('{count}', String(fields.length))}
              </Typography.Text>
            </div>
            {dirty ? <Tag color="orange">{t('ai_settings.unsaved_count').replace('{count}', String(Object.keys(pending).length))}</Tag> : null}
          </div>
          <LoadErrorBanner error={error} onRetry={reload} title={t('ai_settings.load_failed')} />
          {loading && fields.length === 0 ? (
            <Card className="pool-card"><div className="pool-skel pool-ai-settings-skeleton" /></Card>
          ) : Object.keys(groups).length ? (
            <div className="pool-ai-settings-sections">
              {Object.entries(groups).map(([section, sectionFields]) => (
                <Card
                  className="pool-card pool-ai-settings-card"
                  key={section}
                  title={sectionFields[0]?.category || section}
                  headerExtraContent={<span className="pool-ai-settings-card__count">{sectionFields.length}</span>}
                >
                  {sectionFields.map((field) => <ConfigFieldRow key={field.key} field={field} pending={pending} onChange={changeField} />)}
                </Card>
              ))}
            </div>
          ) : (
            <Banner type="info" title={t('ai_settings.empty_title')} description={t('ai_settings.empty_description')} />
          )}
        </div>
      </div>
      <ConfirmDialog
        open={Boolean(leaveTarget)}
        title={t('ai_settings.leave_title')}
        description={t('ai_settings.leave_description')}
        confirmText={t('ai_settings.discard_leave')}
        cancelText={t('common.cancel')}
        destructive
        onCancel={() => setLeaveTarget(null)}
        onConfirm={() => {
          const target = leaveTarget;
          setPending({});
          setLeaveTarget(null);
          if (target) navigate(target);
        }}
      />
    </div>
  );
}
