// @ts-nocheck
import React, { useState, useCallback } from 'react';
import {
  Button, ConfirmDialog, Modal, Tag, Toast, Typography, Input,
} from '../components/pool/index.jsx';
import { IconRefresh, IconPlus, IconSearch, IconDelete } from '../components/pool/icons.jsx';
import PageHeader from '../components/PageHeader.jsx';
import PageScaffold from '../components/PageScaffold.tsx';
import useAsyncAction from '../hooks/useAsyncAction.js';
import useAsyncResource from '../hooks/useAsyncResource.js';
import { showErrorToast } from '../components/ErrorToast.jsx';
import {
  fetchEmailPool, importEmailAccounts, deleteEmailAccounts, testEmailAccount,
} from '../features/accounts/api/emailPool';
import type { EmailAccount } from '../features/accounts/api/emailPool';

const STATUS_COLORS: Record<string, string> = {
  idle: 'green',
  in_use: 'blue',
  used: 'gray',
  error: 'red',
};

function StatusTag({ status }: { status: string }) {
  const color = STATUS_COLORS[status] || 'gray';
  return <Tag color={color}>{status}</Tag>;
}

export default function EmailPool() {
  const [searchInput, setSearchInput] = useState('');
  const [search, setSearch] = useState('');
  const [page, setPage] = useState(1);
  const [importModalOpen, setImportModalOpen] = useState(false);
  const [importText, setImportText] = useState('');
  const [selectedIds, setSelectedIds] = useState<Set<string>>(new Set());
  const [deleteRequest, setDeleteRequest] = useState<{ ids: string[]; label: string } | null>(null);

  const loadData = useCallback(async ({ signal }: { signal?: AbortSignal } = {}) => {
    return fetchEmailPool({ page, pageSize: 50, search }, signal);
  }, [page, search]);
  const emptyData = { accounts: [], total: 0, page: 1, pageSize: 50, counts: {} };
  const { data = emptyData, reload: load, loading, error: loadError } = useAsyncResource(
    loadData,
    [page, search],
    { initialData: emptyData },
  );
  const accounts = data.accounts || [];
  const total = data.total || 0;
  const counts = data.counts || {};

  const { run: doImport, running: importing } = useAsyncAction(async () => {
    try {
      const result = await importEmailAccounts({ text: importText });
      Toast.success(`Imported ${result.imported} email accounts`);
      setImportModalOpen(false);
      setImportText('');
      void load();
    } catch (error) {
      showErrorToast(error);
    }
  });

  const { run: doDelete, running: deleting } = useAsyncAction(async (ids: string[]) => {
    if (!ids.length) return;
    setDeleteRequest(null);
    try {
      await deleteEmailAccounts(ids);
      Toast.success(`Deleted ${ids.length} email account(s)`);
      setSelectedIds((current) => new Set([...current].filter((id) => !ids.includes(id))));
      void load();
    } catch (error) {
      showErrorToast(error);
    }
  });

  const { run: doTest } = useAsyncAction(async (id: string) => {
    try {
      const result = await testEmailAccount(id);
      if (result.ok) Toast.success(`Email ${result.email} is working`);
      else Toast.error(`Email test failed: ${result.error || 'unknown'}`);
      void load();
    } catch (error) {
      showErrorToast(error);
    }
  });

  const applySearch = () => {
    setPage(1);
    setSearch(searchInput.trim());
  };

  const toggleSelect = useCallback((id: string) => {
    setSelectedIds((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id); else next.add(id);
      return next;
    });
  }, []);

  return (
    <PageScaffold>
      <PageHeader
        title="Email Pool"
        description="Manage Outlook/Hotmail email accounts for ChatGPT registration"
        actions={
          <>
            <Button onClick={() => setImportModalOpen(true)}><IconPlus /> Import</Button>
            <Button onClick={load}><IconRefresh /></Button>
          </>
        }
      />

      {/* Stats bar */}
      <div style={{ display: 'flex', gap: 16, marginBottom: 16, flexWrap: 'wrap' }}>
        <div className="pool-stat-card"><Typography.Text type="tertiary" style={{ display: 'block' }}>Total</Typography.Text><Typography.Text strong style={{ display: 'block', fontSize: 20 }}>{total}</Typography.Text></div>
        <div className="pool-stat-card" style={{ color: 'var(--pool-green)' }}><Typography.Text type="tertiary" style={{ display: 'block' }}>Idle</Typography.Text><Typography.Text strong style={{ display: 'block', fontSize: 20 }}>{counts.idle || 0}</Typography.Text></div>
        <div className="pool-stat-card" style={{ color: 'var(--pool-blue)' }}><Typography.Text type="tertiary" style={{ display: 'block' }}>In Use</Typography.Text><Typography.Text strong style={{ display: 'block', fontSize: 20 }}>{counts.in_use || 0}</Typography.Text></div>
        <div className="pool-stat-card" style={{ color: 'var(--pool-gray)' }}><Typography.Text type="tertiary" style={{ display: 'block' }}>Used</Typography.Text><Typography.Text strong style={{ display: 'block', fontSize: 20 }}>{counts.used || 0}</Typography.Text></div>
        <div className="pool-stat-card" style={{ color: 'var(--pool-red)' }}><Typography.Text type="tertiary" style={{ display: 'block' }}>Error</Typography.Text><Typography.Text strong style={{ display: 'block', fontSize: 20 }}>{counts.error || 0}</Typography.Text></div>
      </div>

      {/* Search bar */}
      <div style={{ display: 'flex', gap: 8, marginBottom: 16 }}>
        <Input
          placeholder="Search email..."
          value={searchInput}
          onChange={setSearchInput}
          onEnterPress={applySearch}
          prefix={<IconSearch />}
          showClear
          onClear={() => { setSearch(''); setPage(1); }}
          style={{ maxWidth: 300 }}
        />
        <Button icon={<IconSearch />} onClick={applySearch}>Search</Button>
        {selectedIds.size > 0 && (
          <Button type="danger" onClick={() => setDeleteRequest({ ids: [...selectedIds], label: `${selectedIds.size} email account(s)` })}>
            <IconDelete /> Delete ({selectedIds.size})
          </Button>
        )}
      </div>

      {/* Error state */}
      {loadError && (
        <div style={{ marginBottom: 16, padding: 12, background: 'var(--pool-bg-surface)', borderRadius: 8 }}>
          <Typography.Text type="danger">Failed to load: {loadError.message}</Typography.Text>
          <Button onClick={load} style={{ marginLeft: 8 }}>Retry</Button>
        </div>
      )}

      {/* Table */}
      {loading && !accounts.length ? (
        <Typography.Text>Loading...</Typography.Text>
      ) : (
        <table className="pool-table" style={{ width: '100%' }}>
          <thead>
            <tr>
              <th style={{ width: 40 }}><input type="checkbox" onChange={(e) => {
                if (e.target.checked) setSelectedIds(new Set(accounts.map(a => a.id)));
                else setSelectedIds(new Set());
              }} /></th>
              <th>Email</th>
              <th>Status</th>
              <th>Group</th>
              <th>Error</th>
              <th>Last Used</th>
              <th>Actions</th>
            </tr>
          </thead>
          <tbody>
            {accounts.map((a) => (
              <tr key={a.id}>
                <td><input type="checkbox" checked={selectedIds.has(a.id)} onChange={() => toggleSelect(a.id)} /></td>
                <td><Typography.Text className="pool-mono">{a.email}</Typography.Text></td>
                <td><StatusTag status={a.status} /></td>
                <td>{a.group_name || '-'}</td>
                <td style={{ maxWidth: 200, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                  {a.error_message || '-'}
                </td>
                <td>{a.last_used_at ? new Date(a.last_used_at * 1000).toLocaleDateString() : '-'}</td>
                <td>
                  <div style={{ display: 'flex', gap: 4 }}>
                    <Button size="small" onClick={() => doTest(a.id)}>Test</Button>
                    <Button size="small" type="danger" onClick={() => setDeleteRequest({ ids: [a.id], label: a.email })}>Delete</Button>
                  </div>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      {/* Pagination */}
      {total > 50 && (
        <div style={{ display: 'flex', justifyContent: 'center', gap: 8, marginTop: 16 }}>
          <Button disabled={page <= 1} onClick={() => setPage(p => p - 1)}>Prev</Button>
          <Typography.Text>Page {page} (Total: {total})</Typography.Text>
          <Button disabled={page * 50 >= total} onClick={() => setPage(p => p + 1)}>Next</Button>
        </div>
      )}

      {/* Import Modal */}
      <Modal open={importModalOpen} onCancel={() => setImportModalOpen(false)} title="Import Email Accounts" footer={null}>
        <Typography.Text style={{ display: 'block', marginBottom: 8 }}>
          Paste email accounts, one per line:<br />
          <code>email----password----client_id----refresh_token</code>
        </Typography.Text>
        <textarea
          rows={8}
          style={{ width: '100%', fontFamily: 'monospace', padding: 8, border: '1px solid var(--pool-border)', borderRadius: 4 }}
          value={importText}
          onChange={(e) => setImportText(e.target.value)}
          placeholder={`user1@hotmail.com----password123----client-id----refresh_token\nuser2@outlook.com----password456----client-id----refresh_token`}
        />
        <div style={{ display: 'flex', justifyContent: 'flex-end', gap: 8, marginTop: 16 }}>
          <Button theme="outline" onClick={() => setImportModalOpen(false)}>Cancel</Button>
          <Button theme="solid" onClick={doImport} disabled={!importText.trim() || importing}>Import</Button>
        </div>
      </Modal>
      <ConfirmDialog
        open={Boolean(deleteRequest)}
        title={`Delete ${deleteRequest?.label || 'email account'}?`}
        description="Deleted email pool entries cannot be recovered."
        confirmText="Delete"
        cancelText="Cancel"
        destructive
        onCancel={() => { if (!deleting) setDeleteRequest(null); }}
        onConfirm={() => { if (deleteRequest) void doDelete(deleteRequest.ids); }}
      />
    </PageScaffold>
  );
}
