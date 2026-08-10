import React, { useMemo } from 'react';
import { Banner, Button, Form, Modal } from './pool/index.jsx';
import { IconSave } from './pool/icons.jsx';
import { showErrorToast } from './ErrorToast.jsx';
import { t } from '../lib/i18n.js';
import type { ApiKeyRow, ApiKeyUpdateInput } from '../features/access/model/keys';


interface RawEditForm {
  label?: unknown;
  group_name?: unknown;
  user_group_id?: unknown;
  force_model?: unknown;
  force_effort?: unknown;
  enabled?: unknown;
  expires_at?: unknown;
}

interface ApiKeyEditModalProps {
  visible: boolean;
  row: ApiKeyRow;
  accountGroups?: Array<{ name: string }>;
  userGroups?: Array<{ id: string; name: string }>;
  saving?: boolean;
  onCancel: () => void;
  onSave: (values: ApiKeyUpdateInput) => Promise<unknown>;
}

function keyHash(row: ApiKeyRow) {
  return String(row.key_hash || row.hash || '');
}

function expiryInput(value: unknown) {
  const timestamp = Number(value) || 0;
  if (!timestamp) return '';
  const date = new Date(timestamp * 1000);
  if (Number.isNaN(date.getTime())) return '';
  return date.toISOString().slice(0, 16);
}

function effortOptions() {
  return ['', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max', 'ultra']
    .map((value) => ({ label: value || t('keys.unlimited'), value }));
}

export function cleanApiKeyUpdateValues(row: ApiKeyRow, values: RawEditForm): ApiKeyUpdateInput {
  const poolImport = row.key_type === 'pool_import';
  const cleaned: ApiKeyUpdateInput = {
    hash: keyHash(row),
    label: String(values.label || '').trim(),
    group_name: poolImport ? String(values.group_name || '').trim() : '',
    user_group_id: poolImport ? '' : String(values.user_group_id || '').trim(),
    force_model: poolImport ? '' : String(values.force_model || '').trim(),
    force_effort: poolImport ? '' : String(values.force_effort || '').trim(),
    enabled: values.enabled !== false,
    expires_at: 0,
  };
  if (values.expires_at) {
    const timestamp = Date.parse(String(values.expires_at));
    if (Number.isNaN(timestamp)) throw new Error(t('keys.invalid_expiry'));
    cleaned.expires_at = Math.floor(timestamp / 1000);
  }
  if (!cleaned.hash) throw new Error('API Key hash is missing');
  return cleaned;
}

export default function ApiKeyEditModal({
  visible,
  row,
  accountGroups = [],
  userGroups = [],
  saving = false,
  onCancel,
  onSave,
}: ApiKeyEditModalProps) {
  const poolImport = row.key_type === 'pool_import';
  const initialValues = useMemo(() => ({
    label: row.label || '',
    group_name: row.group_name || '',
    user_group_id: row.user_group_id || '',
    force_model: row.force_model || '',
    force_effort: row.force_effort || '',
    enabled: row.enabled !== false,
    expires_at: expiryInput(row.expires_at),
  }), [row]);

  const submit = (values: RawEditForm) => {
    try {
      void onSave(cleanApiKeyUpdateValues(row, values));
    } catch (error) {
      showErrorToast(error);
    }
  };

  return (
    <Modal
      title={`编辑 API Key · ${row.label || keyHash(row).slice(0, 12)}`}
      visible={visible}
      onCancel={onCancel}
      footer={null}
      maskClosable={!saving}
    >
      <Banner
        type="info"
        title={poolImport ? '账号池导入范围' : '推理路由策略'}
        description={poolImport
          ? '账号池导入 Key 只能选择账号池底层分组，新导入账号将进入该分组。'
          : '推理 API Key 只能选择用户分组，并使用该用户分组的混合目标与模型层级。'}
      />
      <Form initValues={initialValues} onSubmit={submit} labelPosition="left" labelWidth={126}>
        <Form.Input field="label" label={t('keys.label')} rules={[{ required: true, message: t('keys.label_required') }, { max: 80, message: t('keys.label_max') }]} />
        {poolImport ? (
          <Form.Select
            field="group_name"
            label="账号池底层分组"
            optionList={[
              { label: '不指定', value: '' },
              ...accountGroups.map((group) => ({ label: group.name, value: group.name })),
            ]}
          />
        ) : (
          <Form.Select
            field="user_group_id"
            label="用户分组"
            optionList={[
              { label: '不使用用户分组', value: '' },
              ...userGroups.map((group) => ({ label: group.name, value: group.id })),
            ]}
          />
        )}
        {!poolImport ? (
          <>
            <Form.Input field="force_model" label={t('keys.force_model')} placeholder={t('keys.force_model_hint')} />
            <Form.Select field="force_effort" label={t('keys.effort')} optionList={effortOptions()} />
          </>
        ) : null}
        <Form.Input field="expires_at" label={t('keys.expires_at')} placeholder={t('keys.expires_hint')} />
        <Form.Switch field="enabled" label={t('keys.enabled')} />
        <div className="pool-modal-actions">
          <Button onClick={onCancel} disabled={saving}>{t('common.cancel')}</Button>
          <Button htmlType="submit" theme="solid" icon={<IconSave />} loading={saving}>{t('common.save')}</Button>
        </div>
      </Form>
    </Modal>
  );
}
