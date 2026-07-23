import React, { useState, useCallback, useEffect } from 'react';
import {
  Button, Modal, Tag, Toast, Typography, Input,
} from '../components/pool/index.jsx';
import { IconRefresh, IconPlus, IconSearch, IconTrash } from '../components/pool/icons.jsx';
import PageHeader from '../components/PageHeader.jsx';
import PageScaffold from '../components/PageScaffold.tsx';
import useAsyncAction from '../hooks/useAsyncAction.js';
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
  const [accounts, setAccounts] = useState<EmailAccount[]>([]);
  const [total, setTotal] = useState(0);
  const [counts, setCounts] = useState<Record<string, number>>({});
  const [search, setSearch] = useState('');
  const [page, setPage] = useState(1);
  const [importModalOpen, setImportModalOpen] = useState(false);
  const [importText, setImportText] = useState('');
  const [selectedIds, setSelectedIds] = useState<Set<string>>(new Set());

  const loadData = useCallback(async () => {
    const result = await fetchEmailPool({ page, pageSize: 50, search });
    setAccounts(result.accounts);
    setTotal(result.total);
    setCounts(result.counts || {});
  }, [page, search]);

  const { run: load, running: loading, error: loadError } = useAsyncAction(loadData);

  const { run: doImport, running: importing } = useAsyncAction(async () => {
    const result = await importEmailAccounts({ text: importText });
    Toast.success(`Imported ${result.imported} email accounts`);
    setImportModalOpen(false);
    setImportText('');
    await load();
  });

  const { run: doDelete } = useAsyncAction(async () => {
    if (selectedIds.size === 0) return;
    if (!confirm(`Delete ${selectedIds.size} email account(s)?`)) return;
    await deleteEmailAccounts(Array.from(selectedIds));
    Toast.success(`Deleted ${selectedIds.size} email account(s)`);
    setSelectedIds(new Set());
    await load();
  });

  const { run: doTest } = useAsyncAction(async (id: string) => {
    const result = await testEmailAccount(id);
    if (result.ok) {
      Toast.success(`Email ${result.email} is working`);
    } else {
      Toast.error(`Email test failed: ${result.error || 'unknown'}`);
    }
    await load();
  });

  useEffect(() => { load(); }, [load]);

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
        <div className="pool-stat-card"><Typography variant="label">Total</Typography><Typography variant="heading">{total}</Typography></div>
        <div className="pool-stat-card" style={{ color: 'var(--pool-green)' }}><Typography variant="label">Idle</Typography><Typography variant="heading">{counts.idle || 0}</Typography></div>
        <div className="pool-stat-card" style={{ color: 'var(--pool-blue)' }}><Typography variant="label">In Use</Typography><Typography variant="heading">{counts.in_use || 0}</Typography></div>
        <div className="pool-stat-card" style={{ color: 'var(--pool-gray)' }}><Typography variant="label">Used</Typography><Typography variant="heading">{counts.used || 0}</Typography></div>
        <div className="pool-stat-card" style={{ color: 'var(--pool-red)' }}><Typography variant="label">Error</Typography><Typography variant="heading">{counts.error || 0}</Typography></div>
      </div>

      {/* Search bar */}
      <div style={{ display: 'flex', gap: 8, marginBottom: 16 }}>
        <Input
          placeholder="Search email..."
          value={search}
          onChange={(e: React.ChangeEvent<HTMLInputElement>) => { setSearch(e.target.value); setPage(1); }}
          prefix={<IconSearch />}
          style={{ maxWidth: 300 }}
        />
        {selectedIds.size > 0 && (
          <Button variant="danger" onClick={doDelete}>
            <IconTrash /> Delete ({selectedIds.size})
          </Button>
        )}
      </div>

      {/* Error state */}
      {loadError && (
        <div style={{ marginBottom: 16, padding: 12, background: 'var(--pool-bg-surface)', borderRadius: 8 }}>
          <Typography variant="body" style={{ color: 'var(--pool-red)' }}>Failed to load: {loadError.message}</Typography>
          <Button onClick={load} style={{ marginLeft: 8 }}>Retry</Button>
        </div>
      )}

      {/* Table */}
      {loading && !accounts.length ? (
        <Typography variant="body">Loading...</Typography>
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
                <td><Typography variant="body" style={{ fontFamily: 'monospace' }}>{a.email}</Typography></td>
                <td><StatusTag status={a.status} /></td>
                <td>{a.group_name || '-'}</td>
                <td style={{ maxWidth: 200, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                  {a.error_message || '-'}
                </td>
                <td>{a.last_used_at ? new Date(a.last_used_at * 1000).toLocaleDateString() : '-'}</td>
                <td>
                  <div style={{ display: 'flex', gap: 4 }}>
                    <Button size="small" onClick={() => doTest(a.id)}>Test</Button>
                    <Button size="small" variant="danger" onClick={() => {
                      if (confirm(`Delete ${a.email}?`)) {
                        deleteEmailAccounts([a.id]).then(() => load());
                      }
                    }}>Delete</Button>
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
          <Typography variant="body">Page {page} (Total: {total})</Typography>
          <Button disabled={page * 50 >= total} onClick={() => setPage(p => p + 1)}>Next</Button>
        </div>
      )}

      {/* Import Modal */}
      <Modal open={importModalOpen} onClose={() => setImportModalOpen(false)} title="Import Email Accounts">
        <Typography variant="body" style={{ marginBottom: 8 }}>
          Paste email accounts, one per line:<br />
          <code>email----password----client_id----refresh_token</code>
        </Typography>
        <textarea
          rows={8}
          style={{ width: '100%', fontFamily: 'monospace', padding: 8, border: '1px solid var(--pool-border)', borderRadius: 4 }}
          value={importText}
          onChange={(e) => setImportText(e.target.value)}
          placeholder={`user1@hotmail.com----password123----client-id----refresh_token\nuser2@outlook.com----password456----client-id----refresh_token`}
        />
        <div style={{ display: 'flex', justifyContent: 'flex-end', gap: 8, marginTop: 16 }}>
          <Button variant="secondary" onClick={() => setImportModalOpen(false)}>Cancel</Button>
          <Button onClick={doImport} disabled={!importText.trim() || importing}>Import</Button>
        </div>
      </Modal>
    </PageScaffold>
  );
}
