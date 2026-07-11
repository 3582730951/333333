import React, { useState, useCallback } from 'react';
import * as PoolUI from '../components/pool/index.jsx';
import { IconPlus, IconRefresh } from '../components/pool/icons.jsx';
import ApiKeyCreateModal from '../components/ApiKeyCreateModal.tsx';
import ApiKeysTable from '../components/ApiKeysTable.tsx';
import LoadErrorBanner from '../components/LoadErrorBanner.jsx';
import PageHeader from '../components/PageHeader.jsx';
import { KeyCreatedPanel } from '../components/KeySecretTools.jsx';
import { showErrorToast } from '../components/ErrorToast.jsx';
import useDelayedReveal from '../hooks/useDelayedReveal.js';
import { t } from '../lib/i18n.js';
import type { ApiKeyCreateInput } from '../features/access/model/keys';
import { useAdminKeysData, useCreateAdminKeyMutation, useDeleteAdminKeyMutation } from '../features/access/queries/keys.ts';

const { Button, Toast } = PoolUI as any;
const CreateModal = ApiKeyCreateModal as any;
const KeysTable = ApiKeysTable as any;
const ErrorBanner = LoadErrorBanner as any;
const CreatedPanel = KeyCreatedPanel as any;

export default function Keys() {
  const [open, setOpen] = useState(false);
  const { value: created, reveal: revealCreated, clear: clearCreated } = useDelayedReveal();

  const { data: rows = [], loading, error, reload: load } = useAdminKeysData();
  const createMutation = useCreateAdminKeyMutation();
  const deleteMutation = useDeleteAdminKeyMutation();

  const remove = async (hash: string) => {
    try { await deleteMutation.mutateAsync(hash); Toast.success(t('keys.deleted')); }
    catch (e) { showErrorToast(e); }
  };
  const deleting = deleteMutation.isPending;
  const isDeleting = (hash: string) => deleting && deleteMutation.variables === hash;

  const create = useCallback(async (values: ApiKeyCreateInput) => {
    const res = await createMutation.mutateAsync(values);
    const result = (res || {}) as Record<string, unknown>;
    const key = String(result.key || result.secret || result.api_key || t('keys.response_fallback'));
    setOpen(false);
    revealCreated(key);
    Toast.success(t('keys.created'));
  }, [createMutation, revealCreated]);

  return (
    <div>
      <PageHeader title={t('keys.admin_title')} subtitle={t('keys.admin_subtitle')}
        actions={<>
          <Button icon={<IconRefresh />} onClick={load}>{t('common.refresh')}</Button>
          <Button icon={<IconPlus />} theme="solid" onClick={() => { clearCreated(); setOpen(true); }}>{t('keys.create')}</Button>
        </>} />
      <ErrorBanner error={error} onRetry={load} />
      <CreatedPanel secret={created} onClose={clearCreated} />
      <KeysTable rows={rows} loading={loading} onDelete={remove} deleteRunning={deleting} isDeleteRunning={isDeleting} />
      <CreateModal visible={open} mode="admin" onCancel={() => setOpen(false)} onCreate={create} />
    </div>
  );
}
