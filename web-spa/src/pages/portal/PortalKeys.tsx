import React, { useState, useCallback } from 'react';
import * as PoolUI from '../../components/pool/index.jsx';
import { IconRefresh, IconPlus } from '../../components/pool/icons.jsx';
import ApiKeyCreateModal from '../../components/ApiKeyCreateModal.tsx';
import ApiKeysTable from '../../components/ApiKeysTable.tsx';
import LoadErrorBanner from '../../components/LoadErrorBanner.jsx';
import PageHeader from '../../components/PageHeader.jsx';
import { KeyCreatedPanel } from '../../components/KeySecretTools.jsx';
import { showErrorToast } from '../../components/ErrorToast.jsx';
import useDelayedReveal from '../../hooks/useDelayedReveal.js';
import { t } from '../../lib/i18n.js';
import type { ApiKeyCreateInput, ApiKeyRow } from '../../features/access/model/keys';
import {
  useCreatePortalKeyMutation, useDeletePortalKeyMutation, usePortalKeysData, useUpdatePortalKeyMutation,
} from '../../features/access/queries/keys.ts';

const { Button, Toast } = PoolUI as any;
const CreateModal = ApiKeyCreateModal as any;
const KeysTable = ApiKeysTable as any;
const ErrorBanner = LoadErrorBanner as any;
const CreatedPanel = KeyCreatedPanel as any;

const keyHash = (row: ApiKeyRow) => row.key_hash || row.hash || '';

export default function PortalKeys() {
  const [createOpen, setCreateOpen] = useState(false);
  const { value: newKey, reveal: revealNewKey, clear: clearNewKey } = useDelayedReveal();

  const { data: rows = [], loading, error, reload: load } = usePortalKeysData();
  const createMutation = useCreatePortalKeyMutation();
  const updateMutation = useUpdatePortalKeyMutation();
  const deleteMutation = useDeletePortalKeyMutation();

  const create = useCallback(async (values: ApiKeyCreateInput) => {
    const r = await createMutation.mutateAsync(values);
    const result = (r || {}) as Record<string, unknown>;
    setCreateOpen(false);
    revealNewKey(String(result.key || result.secret || result.api_key || ''));
    Toast.success(t('keys.created'));
  }, [createMutation, revealNewKey]);

  const runToggle = async (hash: string, enabled: boolean) => {
    try { await updateMutation.mutateAsync({ hash, enabled }); }
    catch (e) { showErrorToast(e); }
  };
  const toggling = updateMutation.isPending;
  const isToggling = (hash: string) => toggling && updateMutation.variables?.hash === hash;
  const toggle = (row: ApiKeyRow, enabled: boolean) => runToggle(keyHash(row), enabled);

  const remove = async (hash: string) => {
    try { await deleteMutation.mutateAsync(hash); Toast.success(t('keys.deleted')); }
    catch (e) { showErrorToast(e); }
  };
  const deleting = deleteMutation.isPending;
  const isDeleting = (hash: string) => deleting && deleteMutation.variables === hash;

  return (
    <div>
      <PageHeader title={t('keys.portal_title')} subtitle={t('keys.portal_subtitle')}
        actions={<>
          <Button icon={<IconPlus />} theme="solid" disabled={deleting || toggling} onClick={() => { clearNewKey(); setCreateOpen(true); }}>{t('keys.new')}</Button>
          <Button icon={<IconRefresh />} onClick={load}>{t('common.refresh')}</Button>
        </>} />

      <ErrorBanner error={error} onRetry={load} />
      <CreatedPanel secret={newKey} onClose={clearNewKey} />

      <KeysTable rows={rows} loading={loading} mode="portal" onDelete={remove} onToggle={toggle}
        deleteRunning={deleting} isDeleteRunning={isDeleting}
        toggleRunning={toggling} isToggleRunning={isToggling} />

      <CreateModal visible={createOpen} mode="portal" onCancel={() => setCreateOpen(false)} onCreate={create} />
    </div>
  );
}
