// @ts-nocheck
import React, { useCallback, useMemo, useState } from 'react';
import { useNavigate } from 'react-router';
import {
  Button, ConfirmDialog, Modal, Tag, Textarea, Toast, Typography, Input,
} from '../components/pool/index.jsx';
import { IconRefresh, IconPlus, IconSearch, IconDelete, IconGlobe } from '../components/pool/icons.jsx';
import PageScaffold from '../components/PageScaffold.tsx';
import ResourceTable from '../components/ResourceTable.jsx';
import MobileResourceCell from '../components/MobileResourceCell.jsx';
import {
  ActionGroup, MetricRail, TextClamp,
} from '../components/DisplayPrimitives.jsx';
import useAsyncAction from '../hooks/useAsyncAction.js';
import useAsyncResource from '../hooks/useAsyncResource.js';
import { showErrorToast } from '../components/ErrorToast.jsx';
import { fmtDateTime, middleEllipsis } from '../lib/format.js';
import { t } from '../lib/i18n.js';
import {
  fetchEmailPool, importEmailAccounts, deleteEmailAccounts, testEmailAccount,
} from '../features/accounts/api/emailPool';
import type { EmailAccount } from '../features/accounts/api/emailPool';

const PAGE_SIZE = 50;
const STATUS_META: Record<string, { color: string; key: string }> = {
  idle: { color: 'green', key: 'email_pool.status_idle' },
  ready: { color: 'green', key: 'email_pool.status_ready' },
  in_use: { color: 'blue', key: 'email_pool.status_in_use' },
  used: { color: 'gray', key: 'email_pool.status_used' },
  error: { color: 'red', key: 'email_pool.status_error' },
};

function StatusTag({ status }: { status: string }) {
  const meta = STATUS_META[status] || { color: 'gray', key: '' };
  return <Tag color={meta.color}>{meta.key ? t(meta.key) : (status || t('common.unknown'))}</Tag>;
}

function countOf(counts: Record<string, number>, key: string) {
  const value = Number(counts?.[key]);
  return Number.isFinite(value) && value > 0 ? value : 0;
}

function shareDetail(value: number, total: number) {
  if (!total) return t('email_pool.no_entries');
  return t('email_pool.share').replace('{value}', `${Math.round((value / total) * 100)}%`);
}

export default function EmailPool() {
  const navigate = useNavigate();
  const [searchInput, setSearchInput] = useState('');
  const [search, setSearch] = useState('');
  const [page, setPage] = useState(1);
  const [importModalOpen, setImportModalOpen] = useState(false);
  const [importText, setImportText] = useState('');
  const [selectedIds, setSelectedIds] = useState<Set<string>>(new Set());
  const [deleteRequest, setDeleteRequest] = useState<{ ids: string[]; label: string } | null>(null);

  const loadData = useCallback(async ({ signal }: { signal?: AbortSignal } = {}) => {
    return fetchEmailPool({ page, pageSize: PAGE_SIZE, search }, signal);
  }, [page, search]);
  const emptyData = { accounts: [], total: 0, page: 1, pageSize: PAGE_SIZE, counts: {} };
  const {
    data = emptyData,
    reload: load,
    loading,
    error: loadError,
    lastRefresh,
  } = useAsyncResource(loadData, [page, search], { initialData: emptyData });
  const accounts = data.accounts || [];
  const total = data.total || 0;
  const counts = data.counts || {};
  const hasLoadedData = Boolean(lastRefresh);

  const { run: doImport, running: importing } = useAsyncAction(async () => {
    try {
      const result = await importEmailAccounts({ text: importText });
      Toast.success(t('email_pool.imported').replace('{count}', String(result.imported)));
      if (result.parse_errors?.length) {
        Toast.warning(t('email_pool.import_partial').replace('{count}', String(result.parse_errors.length)));
      }
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
      Toast.success(t('email_pool.deleted').replace('{count}', String(ids.length)));
      setSelectedIds((current) => new Set([...current].filter((id) => !ids.includes(id))));
      void load();
    } catch (error) {
      showErrorToast(error);
    }
  });

  const { run: doTest, running: testing } = useAsyncAction(async (id: string) => {
    try {
      const result = await testEmailAccount(id);
      if (result.ok) Toast.success(t('email_pool.test_ok').replace('{email}', result.email));
      else Toast.error(t('email_pool.test_failed').replace('{error}', result.error || t('common.unknown')));
      void load();
    } catch (error) {
      showErrorToast(error);
    }
  });

  const applySearch = () => {
    setPage(1);
    setSearch(searchInput.trim());
  };

  const clearSearch = () => {
    setSearchInput('');
    setSearch('');
    setPage(1);
  };

  const available = countOf(counts, 'idle') + countOf(counts, 'ready');
  const inUse = countOf(counts, 'in_use');
  const used = countOf(counts, 'used');
  const failed = countOf(counts, 'error');
  const metrics = [
    { key: 'total', label: t('email_pool.total'), value: total, detail: t('email_pool.all_entries') },
    { key: 'available', label: t('email_pool.available'), value: available, detail: shareDetail(available, total), tone: 'success' },
    { key: 'in-use', label: t('email_pool.in_use'), value: inUse, detail: shareDetail(inUse, total), tone: 'info' },
    { key: 'used', label: t('email_pool.used'), value: used, detail: shareDetail(used, total) },
    { key: 'error', label: t('email_pool.error'), value: failed, detail: shareDetail(failed, total), tone: 'danger' },
  ];

  const selected = [...selectedIds];
  const selectedOnPage = accounts.filter((account) => selectedIds.has(account.id)).length;
  const allPageSelected = Boolean(accounts.length) && selectedOnPage === accounts.length;
  const togglePageSelection = () => {
    setSelectedIds((current) => {
      const next = new Set(current);
      if (allPageSelected) accounts.forEach((account) => next.delete(account.id));
      else accounts.forEach((account) => next.add(account.id));
      return next;
    });
  };

  const renderActions = useCallback((account: EmailAccount) => (
    <ActionGroup compact className="pool-email-row-actions">
      <Button
        size="small"
        loading={testing}
        aria-label={`${t('email_pool.test')} ${account.email}`}
        onClick={() => void doTest(account.id)}
      >
        {t('email_pool.test')}
      </Button>
      <Button
        size="small"
        type="danger"
        aria-label={`${t('common.delete')} ${account.email}`}
        onClick={() => setDeleteRequest({ ids: [account.id], label: account.email })}
      >
        {t('common.delete')}
      </Button>
    </ActionGroup>
  ), [doTest, testing]);

  const columns = useMemo(() => [
    {
      title: t('email_pool.email'),
      dataIndex: 'email',
      width: 310,
      render: (value: string) => (
        <TextClamp
          strong
          className="pool-mono pool-email-address"
          title={value}
          ariaLabel={value}
        >
          {value}
        </TextClamp>
      ),
    },
    {
      title: t('email_pool.status'),
      dataIndex: 'status',
      width: 112,
      render: (value: string) => <StatusTag status={value} />,
    },
    {
      title: t('email_pool.group'),
      dataIndex: 'group_name',
      width: 168,
      render: (value?: string) => <TextClamp title={value || undefined}>{value || t('email_pool.default_group')}</TextClamp>,
    },
    {
      title: t('email_pool.last_error'),
      dataIndex: 'error_message',
      width: 240,
      render: (value?: string) => <TextClamp title={value || undefined} muted={!value}>{value || '—'}</TextClamp>,
    },
    {
      title: t('email_pool.last_used'),
      dataIndex: 'last_used_at',
      width: 170,
      render: (value?: number) => value ? fmtDateTime(value) : t('email_pool.never_used'),
    },
    {
      title: t('email_pool.actions'),
      key: 'actions',
      width: 156,
      render: (_: unknown, account: EmailAccount) => renderActions(account),
    },
  ], [renderActions]);

  const mobileRenderer = useCallback((account: EmailAccount, meta: {
    selected: boolean;
    toggleSelected: (checked?: boolean) => void;
  }) => (
    <MobileResourceCell
      selectable
      selected={meta.selected}
      selectLabel={`${t('email_pool.email')}: ${account.email}`}
      onSelect={() => meta.toggleSelected(!meta.selected)}
      title={(
        <TextClamp
          strong
          className="pool-mono"
          title={account.email}
          ariaLabel={account.email}
        >
          {middleEllipsis(account.email)}
        </TextClamp>
      )}
      subtitle={account.group_name || t('email_pool.default_group')}
      badges={<StatusTag status={account.status} />}
      details={[
        {
          label: t('email_pool.last_used'),
          value: account.last_used_at ? fmtDateTime(account.last_used_at) : t('email_pool.never_used'),
        },
        account.error_message ? {
          label: t('email_pool.last_error'),
          value: <TextClamp title={account.error_message}>{middleEllipsis(account.error_message, 28, 12)}</TextClamp>,
        } : null,
      ].filter(Boolean)}
      actions={renderActions(account)}
    />
  ), [renderActions]);

  return (
    <PageScaffold
      title={t('nav.email_pool')}
      description={t('page.email_pool.desc')}
      ready={Boolean(lastRefresh || loadError)}
      actions={(
        <>
          <Button icon={<IconGlobe />} onClick={() => navigate('/email-pool/cloudflare')}>
            {t('email_pool.self_hosted')}
          </Button>
          <Button icon={<IconPlus />} theme="solid" onClick={() => setImportModalOpen(true)}>
            {t('email_pool.import')}
          </Button>
          <Button
            icon={<IconRefresh />}
            aria-label={t('common.refresh')}
            title={t('common.refresh')}
            loading={loading && Boolean(lastRefresh)}
            onClick={() => void load()}
          >
            {t('common.refresh')}
          </Button>
        </>
      )}
      filters={(
        <div className="pool-email-toolbar">
          <Input
            aria-label={t('email_pool.search_placeholder')}
            placeholder={t('email_pool.search_placeholder')}
            value={searchInput}
            onChange={setSearchInput}
            onEnterPress={applySearch}
            prefix={<IconSearch />}
            showClear
            onClear={clearSearch}
            className="pool-email-search"
          />
          <Button icon={<IconSearch />} onClick={applySearch}>{t('common.search')}</Button>
          <Button
            theme="outline"
            disabled={!accounts.length}
            aria-pressed={allPageSelected}
            onClick={togglePageSelection}
          >
            {allPageSelected ? t('email_pool.deselect_page') : t('email_pool.select_page')}
          </Button>
          {selected.length > 0 ? (
            <Button
              type="danger"
              icon={<IconDelete />}
              onClick={() => setDeleteRequest({
                ids: selected,
                label: t('email_pool.selected_count').replace('{count}', String(selected.length)),
              })}
            >
              {t('email_pool.delete_selected').replace('{count}', String(selected.length))}
            </Button>
          ) : null}
        </div>
      )}
      summary={hasLoadedData ? <MetricRail items={metrics} className="pool-metric-rail--band pool-email-metrics" /> : null}
    >
      <ResourceTable
        error={loadError}
        errorTitle={t('email_pool.load_failed')}
        onRetry={load}
        loading={loading}
        lastRefresh={lastRefresh}
        dataSource={accounts}
        columns={columns}
        rowKey="id"
        pagination={{
          pageSize: PAGE_SIZE,
          total,
          currentPage: page,
          onPageChange: setPage,
        }}
        rowSelection={{
          selectedRowKeys: selected,
          onChange: (keys: string[]) => setSelectedIds(new Set(keys)),
          getCheckboxProps: (account: EmailAccount) => ({
            'aria-label': `${t('email_pool.email')}: ${account.email}`,
          }),
        }}
        minScrollX={1156}
        safeActionWidth={156}
        density="regular"
        rowHeight={68}
        mobileRenderer={mobileRenderer}
        mobileScroll={false}
        mobileListLabel={t('email_pool.mobile_list')}
        emptyTitle={t('email_pool.empty_title')}
        emptyDesc={t('email_pool.empty_desc')}
        emptyType="accounts"
        emptyAction={<Button theme="solid" icon={<IconPlus />} onClick={() => setImportModalOpen(true)}>{t('email_pool.import')}</Button>}
        skeletonRows={7}
        skeletonCols={6}
        className="pool-email-table"
      />

      <Modal
        open={importModalOpen}
        onCancel={() => setImportModalOpen(false)}
        title={t('email_pool.import_title')}
        footer={null}
      >
        <Typography.Text className="pool-email-import-help">
          {t('email_pool.import_help')}<br />
          <code>email----password----client_id----refresh_token</code>
        </Typography.Text>
        <Textarea
          rows={8}
          className="pool-email-import-textarea"
          value={importText}
          onChange={setImportText}
          aria-label={t('email_pool.import_input')}
          placeholder={`user1@sample.test----password----client-id----refresh-token\nuser2@sample.test----password----client-id----refresh-token`}
        />
        <div className="pool-email-modal-actions">
          <Button theme="outline" onClick={() => setImportModalOpen(false)}>{t('common.cancel')}</Button>
          <Button
            theme="solid"
            loading={importing}
            disabled={!importText.trim() || importing}
            onClick={() => void doImport()}
          >
            {t('email_pool.import')}
          </Button>
        </div>
      </Modal>
      <ConfirmDialog
        open={Boolean(deleteRequest)}
        title={t('email_pool.delete_title').replace('{label}', deleteRequest?.label || t('email_pool.entry'))}
        description={t('email_pool.delete_desc')}
        confirmText={t('common.delete')}
        cancelText={t('common.cancel')}
        destructive
        onCancel={() => { if (!deleting) setDeleteRequest(null); }}
        onConfirm={() => { if (deleteRequest) void doDelete(deleteRequest.ids); }}
      />
    </PageScaffold>
  );
}
