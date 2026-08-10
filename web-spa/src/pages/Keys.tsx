import React, { useState, useCallback, useMemo } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { Button, Toast } from '../components/pool/index.jsx';
import { IconPlus, IconRefresh } from '../components/pool/icons.jsx';
import ApiKeyCreateModal from '../components/ApiKeyCreateModal.tsx';
import ApiKeyEditModal from '../components/ApiKeyEditModal.tsx';
import ApiKeysTable from '../components/ApiKeysTable.tsx';
import LoadErrorBanner from '../components/LoadErrorBanner.jsx';
import PageHeader from '../components/PageHeader.jsx';
import { KeyCreatedPanel } from '../components/KeySecretTools.jsx';
import { showErrorToast } from '../components/ErrorToast.jsx';
import useDelayedReveal from '../hooks/useDelayedReveal.js';
import useKeyedAsyncAction from '../hooks/useKeyedAsyncAction.js';
import { t } from '../lib/i18n.js';
import type { ApiKeyCreateInput, ApiKeyRow, ApiKeyUpdateInput } from '../features/access/model/keys';
import { deleteAdminKey } from '../features/access/api/keys.ts';
import {
  apiKeyQueryKeys, useAdminKeyRoutingOptionsData, useAdminKeysData, useCreateAdminKeyMutation,
  useUpdateAdminKeyMutation,
} from '../features/access/queries/keys.ts';

const CreateModal = ApiKeyCreateModal as any;
const EditModal = ApiKeyEditModal as any;
const KeysTable = ApiKeysTable as any;
const ErrorBanner = LoadErrorBanner as any;
const CreatedPanel = KeyCreatedPanel as any;

export default function Keys() {
  const queryClient = useQueryClient();
  const [open, setOpen] = useState(false);
  const [editing, setEditing] = useState<ApiKeyRow | null>(null);
  const { value: created, reveal: revealCreated, clear: clearCreated } = useDelayedReveal();

  const { data: rows = [], loading, error, reload: load } = useAdminKeysData();
  const {
    data: routingOptions = { accountGroups: [], userGroups: [] },
    loading: routingLoading,
    error: routingError,
    reload: reloadRouting,
  } = useAdminKeyRoutingOptionsData();
  const createMutation = useCreateAdminKeyMutation();
  const updateMutation = useUpdateAdminKeyMutation();
  const userGroupNames = useMemo(() => Object.fromEntries(
    routingOptions.userGroups.map((group) => [group.id, group.name]),
  ), [routingOptions.userGroups]);

  const refresh = useCallback(() => {
    void load();
    void reloadRouting();
  }, [load, reloadRouting]);

  const { run: runDelete, running: deleting, isRunning: isDeleting } = useKeyedAsyncAction(async (_key: string, hash: string) => {
    try {
      await deleteAdminKey(hash);
      queryClient.setQueryData(apiKeyQueryKeys.admin, (current: ApiKeyRow[] | undefined) => (
        current?.filter((row) => (row.key_hash || row.hash) !== hash) || current
      ));
      void queryClient.invalidateQueries({ queryKey: apiKeyQueryKeys.adminAll });
      Toast.success(t('keys.deleted'));
    } catch (deleteError) {
      showErrorToast(deleteError);
    }
  });
  const remove = (hash: string) => runDelete(hash, hash);

  const create = useCallback(async (values: ApiKeyCreateInput) => {
    const res = await createMutation.mutateAsync(values);
    const result = (res || {}) as Record<string, unknown>;
    const key = String(result.key || result.secret || result.api_key || t('keys.response_fallback'));
    setOpen(false);
    revealCreated(key);
    Toast.success(t('keys.created'));
  }, [createMutation, revealCreated]);

  const update = useCallback(async (values: ApiKeyUpdateInput) => {
    try {
      const saved = await updateMutation.mutateAsync(values);
      queryClient.setQueryData(apiKeyQueryKeys.admin, (current: ApiKeyRow[] | undefined) => current?.map((row) => {
        const hash = row.key_hash || row.hash;
        return hash === values.hash ? { ...row, ...values, ...saved } : row;
      }) || current);
      setEditing(null);
      Toast.success('API Key 策略已更新');
    } catch (updateError) {
      showErrorToast(updateError);
    }
  }, [queryClient, updateMutation]);
  const openCreate = useCallback(() => {
    clearCreated();
    setOpen(true);
    void reloadRouting();
  }, [clearCreated, reloadRouting]);
  const openEdit = useCallback((row: ApiKeyRow) => {
    setEditing(row);
    void reloadRouting();
  }, [reloadRouting]);
  const editingHash = editing ? String(editing.key_hash || editing.hash || '') : '';

  return (
    <div>
      <PageHeader title={t('keys.admin_title')} subtitle={t('keys.admin_subtitle')}
        actions={<>
          <Button icon={<IconRefresh />} onClick={refresh} loading={loading || routingLoading}>{t('common.refresh')}</Button>
          <Button icon={<IconPlus />} theme="solid" onClick={openCreate}>{t('keys.create')}</Button>
        </>} />
      <ErrorBanner error={error} onRetry={load} />
      <ErrorBanner error={routingError} onRetry={reloadRouting} />
      <CreatedPanel secret={created} onClose={clearCreated} />
      <KeysTable
        rows={rows}
        loading={loading}
        onEdit={openEdit}
        onDelete={remove}
        userGroupNames={userGroupNames}
        deleteRunning={deleting}
        isDeleteRunning={isDeleting}
        isEditRunning={(hash: string) => updateMutation.isPending && hash === editingHash}
      />
      <CreateModal
        visible={open}
        mode="admin"
        accountGroups={routingOptions.accountGroups}
        userGroups={routingOptions.userGroups}
        onCancel={() => setOpen(false)}
        onCreate={create}
      />
      {editing ? (
        <EditModal
          key={editing.key_hash || editing.hash}
          visible
          row={editing}
          accountGroups={routingOptions.accountGroups}
          userGroups={routingOptions.userGroups}
          saving={updateMutation.isPending}
          onCancel={() => { if (!updateMutation.isPending) setEditing(null); }}
          onSave={update}
        />
      ) : null}
    </div>
  );
}
