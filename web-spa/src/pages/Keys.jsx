import React, { useState, useCallback } from 'react';
import { Button, Toast } from '../components/pool/index.jsx';
import { IconPlus, IconRefresh } from '../components/pool/icons.jsx';
import { get, post, del } from '../api.js';
import ApiKeyCreateModal from '../components/ApiKeyCreateModal.jsx';
import ApiKeysTable from '../components/ApiKeysTable.jsx';
import LoadErrorBanner from '../components/LoadErrorBanner.jsx';
import PageHeader from '../components/PageHeader.jsx';
import { KeyCreatedPanel } from '../components/KeySecretTools.jsx';
import { showErrorToast } from '../components/ErrorToast.jsx';
import useKeyedAsyncAction from '../hooks/useKeyedAsyncAction.js';
import useAsyncResource from '../hooks/useAsyncResource.js';
import useDelayedReveal from '../hooks/useDelayedReveal.js';

export default function Keys() {
  const [open, setOpen] = useState(false);
  const { value: created, reveal: revealCreated, clear: clearCreated } = useDelayedReveal();

  const fetchRows = useCallback(async ({ signal }) => {
    const k = await get('/admin/api-keys', undefined, { signal });
    return Array.isArray(k) ? k : k?.keys || [];
  }, []);
  const { data: rows = [], loading, error, reload: load } = useAsyncResource(fetchRows, [fetchRows], { initialData: [] });

  const { run: remove, running: deleting, isRunning: isDeleting } = useKeyedAsyncAction(async (hash) => {
    try { await del(`/admin/api-keys/${encodeURIComponent(hash)}`); Toast.success('已删除'); await load(); }
    catch (e) { showErrorToast(e); }
  });

  const create = useCallback(async (values) => {
    const res = await post('/admin/api-keys', values);
    const key = res?.key || res?.secret || res?.api_key || '(请在响应中查看)';
    setOpen(false);
    revealCreated(key);
    Toast.success('已创建');
    await load();
  }, [load, revealCreated]);

  return (
    <div>
      <PageHeader title="API Keys" subtitle="下游调用密钥（管理员级）"
        actions={<>
          <Button icon={<IconRefresh />} onClick={load}>刷新</Button>
          <Button icon={<IconPlus />} theme="solid" onClick={() => { clearCreated(); setOpen(true); }}>创建 Key</Button>
        </>} />
      <LoadErrorBanner error={error} onRetry={load} />
      <KeyCreatedPanel secret={created} onClose={clearCreated} />
      <ApiKeysTable rows={rows} loading={loading} onDelete={remove} deleteRunning={deleting} isDeleteRunning={isDeleting} />
      <ApiKeyCreateModal visible={open} mode="admin" onCancel={() => setOpen(false)} onCreate={create} />
    </div>
  );
}
