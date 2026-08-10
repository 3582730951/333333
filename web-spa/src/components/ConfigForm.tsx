import React, { useMemo, useRef, useState } from 'react';
import { Form, Button, Toast, Banner, LoadingState } from './pool/index.jsx';
import { IconRefresh, IconSave } from './pool/icons.jsx';
import LoadErrorBannerBase from './LoadErrorBanner.jsx';
import PageHeaderBase from './PageHeader.jsx';
import { showErrorToast } from './ErrorToast.jsx';
import { useAdvancedSettingsData, useSaveAdvancedSettingsMutation } from '../features/settings/queries/settings';
import type { AdvancedSettingsKind, SettingsValues } from '../features/settings/model/settings';
import { t } from '../lib/i18n.js';

const LoadErrorBanner = LoadErrorBannerBase as any;
const PageHeader = PageHeaderBase as any;

type FieldKind = 'array' | 'json' | 'bool' | 'number' | 'string';

interface ConfigFormProps {
  title: string;
  subtitle: string;
  kind: AdvancedSettingsKind;
  embedded?: boolean;
}

interface DynamicFormApi {
  getValues: () => SettingsValues;
  setValues: (values: SettingsValues, options?: { isOverride?: boolean }) => void;
}

function formStateFor(data: SettingsValues) {
  const meta: Record<string, FieldKind> = {};
  const values: SettingsValues = {};
  for (const [key, value] of Object.entries(data)) {
    if (Array.isArray(value)) {
      meta[key] = 'array';
      values[key] = value.join('\n');
    } else if (value && typeof value === 'object') {
      meta[key] = 'json';
      values[key] = JSON.stringify(value, null, 2);
    } else if (typeof value === 'boolean') {
      meta[key] = 'bool';
      values[key] = value;
    } else if (typeof value === 'number') {
      meta[key] = 'number';
      values[key] = value;
    } else {
      meta[key] = 'string';
      values[key] = value ?? '';
    }
  }
  return { meta, values };
}

export default function ConfigForm({ title, subtitle, kind, embedded = false }: ConfigFormProps) {
  const apiRef = useRef<DynamicFormApi | null>(null);
  const [dirty, setDirty] = useState(false);
  const { data, loading, error, lastRefresh, reload } = useAdvancedSettingsData(kind);
  const saveMutation = useSaveAdvancedSettingsMutation();
  const prepared = useMemo(() => data ? formStateFor(data) : { meta: {}, values: {} }, [data]);

  const save = async () => {
    if (!apiRef.current || !data) return;
    const values = apiRef.current.getValues();
    const output: SettingsValues = {};
    for (const [key, fieldKind] of Object.entries(prepared.meta)) {
      const raw = values[key];
      if (fieldKind === 'array') {
        output[key] = String(raw || '').split('\n').map((item) => item.trim()).filter(Boolean);
      } else if (fieldKind === 'json') {
        try {
          output[key] = JSON.parse(String(raw || 'null'));
        } catch {
          Toast.error(`${key}: ${t('settings.invalid_json')}`);
          return;
        }
      } else if (fieldKind === 'number') {
        output[key] = Number(raw);
      } else {
        output[key] = raw;
      }
    }
    try {
      await saveMutation.mutateAsync({ kind, values: output });
      Toast.success(t('settings.advanced_saved'));
      setDirty(false);
    } catch (saveError) {
      showErrorToast(saveError);
    }
  };

  const entries = data ? Object.entries(data) : [];
  const firstFailure = Boolean(error && !lastRefresh && !loading);
  return (
    <div className={`pool-config-page ${embedded ? 'pool-config-page--embedded' : ''}`}>
      <PageHeader title={title} subtitle={subtitle}
        actions={<>
          <Button icon={<IconRefresh />} onClick={reload} loading={loading}>{t('common.refresh')}</Button>
          <Button icon={<IconSave />} theme="solid" loading={saveMutation.isPending} onClick={save} disabled={!dirty || !data}>{t('common.save')}</Button>
        </>} />
      {firstFailure ? <LoadErrorBanner error={error} onRetry={reload} title={t('settings.advanced_load_failed')} /> : null}
      {!firstFailure ? <LoadErrorBanner error={error} onRetry={reload} title={error ? t('settings.refresh_stale') : undefined} /> : null}
      {loading && !data ? <LoadingState title={t('settings.loading')} /> : null}
      {data && !entries.length ? <Banner type="info" closeIcon={null} description={t('settings.advanced_empty')} /> : null}
      {data ? (
        <div className="pool-panel pool-config-panel">
          <Form
            initValues={prepared.values}
            getFormApi={(api: DynamicFormApi) => { apiRef.current = api; }}
            onChange={() => setDirty(true)}
            labelPosition="left"
            labelWidth={176}
            className="pool-config-form"
          >
            {entries.map(([key, value]) => {
              const fieldKind = prepared.meta[key];
              if (fieldKind === 'bool' || typeof value === 'boolean') return <Form.Switch key={key} field={key} label={key} className="pool-config-field" />;
              if (fieldKind === 'number' || typeof value === 'number') return <Form.InputNumber key={key} field={key} label={key} className="pool-config-field pool-config-field--short" style={{ width: 'min(100%, 220px)' }} />;
              if (Array.isArray(value)) return <Form.TextArea key={key} field={key} label={`${key} (${t('settings.one_per_line')})`} autosize rows={3} className="pool-config-field pool-config-field--text" style={{ width: 'min(100%, 560px)' }} />;
              if (value && typeof value === 'object') return <Form.TextArea key={key} field={key} label={`${key} (JSON)`} autosize rows={4} className="pool-config-field pool-config-field--json pool-mono" style={{ width: '100%' }} />;
              return <Form.Input key={key} field={key} label={key} className="pool-config-field" style={{ width: 'min(100%, 480px)' }} />;
            })}
          </Form>
        </div>
      ) : null}
    </div>
  );
}
