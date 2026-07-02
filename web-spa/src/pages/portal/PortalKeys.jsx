import React, { useState, useCallback } from 'react';
import { Button, Toast } from '../../components/pool/index.jsx';
import { IconRefresh, IconPlus } from '../../components/pool/icons.jsx';
import { get, post, patch, del } from '../../api.js';
import ApiKeyCreateModal from '../../components/ApiKeyCreateModal.jsx';
import ApiKeysTable from '../../components/ApiKeysTable.jsx';
import LoadErrorBanner from '../../components/LoadErrorBanner.jsx';
import PageHeader from '../../components/PageHeader.jsx';
import { KeyCreatedPanel } from '../../components/KeySecretTools.jsx';
import { showErrorToast } from '../../components/ErrorToast.jsx';
import useKeyedAsyncAction from '../../hooks/useKeyedAsyncAction.js';
import useAsyncResource from '../../hooks/useAsyncResource.js';
import useDelayedReveal from '../../hooks/useDelayedReveal.js';

const keyHash = (row) => row.key_hash || row.hash || '';

export default function PortalKeys() {
  const [createOpen, setCreateOpen] = useState(false);
  const { value: newKey, reveal: revealNewKey, clear: clearNewKey } = useDelayedReveal();

  const fetchRows = useCallback(async ({ signal }) => {
    const d = await get('/user/api-keys', undefined, { signal });
    return Array.isArray(d) ? d : [];
  }, []);
  const { data: rows = [], loading, error, reload: load } = useAsyncResource(fetchRows, [fetchRows], { initialData: [] });

  const create = useCallback(async (values) => {
    const r = await post('/user/api-keys', values);
    setCreateOpen(false);
    revealNewKey(r?.key || r?.secret || r?.api_key || '');
    Toast.success('已创建');
    await load();
  }, [load, revealNewKey]);

  const { run: runToggle, running: toggling, isRunning: isToggling } = useKeyedAsyncAction(async (hash, enabled) => {
    try { await patch(`/user/api-keys/${encodeURIComponent(hash)}`, { enabled }); await load(); }
    catch (e) { showErrorToast(e); }
  });
  const toggle = (row, enabled) => runToggle(keyHash(row), enabled);

  const { run: remove, running: deleting, isRunning: isDeleting } = useKeyedAsyncAction(async (hash) => {
    try { await del(`/user/api-keys/${encodeURIComponent(hash)}`); Toast.success('已删除'); await load(); }
    catch (e) { showErrorToast(e); }
  });

  return (
    <div>
      <PageHeader title="我的 API Key" subtitle="创建并管理你的调用密钥"
        actions={<>
          <Button icon={<IconPlus />} theme="solid" disabled={deleting || toggling} onClick={() => { clearNewKey(); setCreateOpen(true); }}>新建 Key</Button>
          <Button icon={<IconRefresh />} onClick={load}>刷新</Button>
        </>} />

      <LoadErrorBanner error={error} onRetry={load} />
      <KeyCreatedPanel secret={newKey} onClose={clearNewKey} />

      <ApiKeysTable rows={rows} loading={loading} mode="portal" onDelete={remove} onToggle={toggle}
        deleteRunning={deleting} isDeleteRunning={isDeleting}
        toggleRunning={toggling} isToggleRunning={isToggling} />

      <ApiKeyCreateModal visible={createOpen} mode="portal" onCancel={() => setCreateOpen(false)} onCreate={create} />
    </div>
  );
}
