import React, { useEffect, useRef, useState } from 'react';
import * as PoolUI from './pool/index.jsx';
import { IconPlus } from './pool/icons.jsx';
import { showErrorToast } from './ErrorToast.jsx';
import useAsyncAction from '../hooks/useAsyncAction.js';
import { t } from '../lib/i18n.js';
import type { ApiKeyCreateInput } from '../features/access/model/keys';

const { Button, Form, Modal } = PoolUI as any;
type KeyMode = 'admin' | 'portal';
interface RawKeyForm {
  label?: unknown;
  key_type?: unknown;
  force_model?: unknown;
  force_effort?: unknown;
  expires_at?: unknown;
  group_name?: unknown;
  user_group_id?: unknown;
}
interface LegacyFormApi {
  reset?: () => void;
}
interface ApiKeyCreateModalProps {
  visible: boolean;
  mode?: KeyMode;
  accountGroups?: Array<{ name: string }>;
  userGroups?: Array<{ id: string; name: string }>;
  onCancel: () => void;
  onCreate: (values: ApiKeyCreateInput) => Promise<unknown>;
}

function effortOptions() {
  return ['', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max', 'ultra']
    .map((value) => ({ label: value || t('keys.unlimited'), value }));
}

export function cleanApiKeyValues(values: RawKeyForm, mode: KeyMode): ApiKeyCreateInput {
  const cleaned: ApiKeyCreateInput = {
    label: String(values.label || '').trim(),
    key_type: String(values.key_type || 'downstream').trim(),
    force_model: String(values.force_model || '').trim(),
    force_effort: String(values.force_effort || '').trim(),
  };
  if (values.expires_at) {
    const ts = Date.parse(String(values.expires_at));
    if (Number.isNaN(ts)) throw new Error(t('keys.invalid_expiry'));
    cleaned.expires_at = Math.floor(ts / 1000);
  }
  if (mode === 'admin') {
    if (cleaned.key_type === 'pool_import') {
      cleaned.group_name = String(values.group_name || '').trim();
      cleaned.user_group_id = '';
      cleaned.force_model = '';
      cleaned.force_effort = '';
    } else {
      cleaned.group_name = '';
      cleaned.user_group_id = String(values.user_group_id || '').trim();
    }
  }
  return cleaned;
}

export default function ApiKeyCreateModal({
  visible,
  mode = 'admin',
  accountGroups = [],
  userGroups = [],
  onCancel,
  onCreate,
}: ApiKeyCreateModalProps) {
  const formApi = useRef<LegacyFormApi | null>(null);
  const admin = mode === 'admin';
  const [keyType, setKeyType] = useState('downstream');

  useEffect(() => {
    if (visible) setKeyType('downstream');
  }, [visible]);

  const { run: submit, running: submitting } = useAsyncAction(async (values: RawKeyForm) => {
    try {
      await onCreate(cleanApiKeyValues(values, mode));
      formApi.current?.reset?.();
    } catch (error) {
      showErrorToast(error);
    }
  });

  return (
    <Modal
      title={admin ? t('keys.modal_admin_title') : t('keys.modal_portal_title')}
      visible={visible}
      onCancel={onCancel}
      footer={null}
      maskClosable={!submitting}
    >
      <Form
        getFormApi={(api: LegacyFormApi) => { formApi.current = api; }}
        labelPosition="left"
        labelWidth={90}
        onSubmit={submit}
      >
        <Form.Input field="label" label={admin ? t('keys.label') : t('keys.name')} placeholder={admin ? '' : t('keys.name_example')} rules={[{ required: true, message: t('keys.label_required') }, { max: 80, message: t('keys.label_max') }]} />
        {admin ? (
          <Form.Select
            field="key_type"
            label={t('keys.type')}
            value={keyType}
            onChange={setKeyType}
            initValue="downstream"
            optionList={[
              { label: t('keys.type_downstream'), value: 'downstream' },
              { label: t('keys.type_pool_import'), value: 'pool_import' },
            ]}
          />
        ) : null}
        {admin && keyType === 'pool_import' ? (
          <Form.Select
            field="group_name"
            label="账号池底层分组"
            placeholder="不指定"
            optionList={accountGroups.map((group) => ({ label: group.name, value: group.name }))}
            initValue=""
          />
        ) : null}
        {admin && keyType !== 'pool_import' ? (
          <Form.Select
            field="user_group_id"
            label="用户分组"
            optionList={[
              { label: '不使用用户分组', value: '' },
              ...userGroups.map((g) => ({ label: g.name, value: g.id })),
            ]}
            initValue=""
          />
        ) : null}
        {!admin || keyType !== 'pool_import' ? (
          <>
            <Form.Input field="force_model" label={t('keys.force_model')} placeholder={t('keys.force_model_hint')} />
            <Form.Select field="force_effort" label={t('keys.effort')} optionList={effortOptions()} initValue="" />
          </>
        ) : null}
        {admin ? <Form.Input field="expires_at" label={t('keys.expires_at')} placeholder={t('keys.expires_hint')} /> : null}
        <div style={{ display: 'flex', justifyContent: 'flex-end', gap: 8, marginTop: 12 }}>
          <Button onClick={onCancel} disabled={submitting}>{t('common.cancel')}</Button>
          <Button htmlType="submit" theme="solid" icon={<IconPlus />} loading={submitting}>{t('common.create')}</Button>
        </div>
      </Form>
    </Modal>
  );
}
