import React, { useState, useCallback } from 'react';
import {
  ActionMenu, Button, ConfirmDialog, Tag, Toast, Modal, Form, Typography, Input, Select,
} from '../components/pool/index.jsx';
import { IconRefresh, IconPlus, IconSearch, IconDownload } from '../components/pool/icons.jsx';
import { get, post, batchOp } from '../api.js';
import PageHeader from '../components/PageHeader.jsx';
import ResourceTable from '../components/ResourceTable.jsx';
import AccountDrawer from '../components/AccountDrawer.jsx';
import OAuthLoginModal from '../components/OAuthLoginModal.jsx';
import MobileResourceCell from '../components/MobileResourceCell.jsx';
import { TextClamp, TinyMeter } from '../components/DisplayPrimitives.jsx';
import { showErrorToast } from '../components/ErrorToast.jsx';
import useAsyncAction from '../hooks/useAsyncAction.js';
import useKeyedAsyncAction from '../hooks/useKeyedAsyncAction.js';
import useAsyncResource from '../hooks/useAsyncResource.js';
import { toCSV, downloadCSV } from '../lib/csv.js';
import { loadResourceGroup } from '../lib/resource.js';
import { fmtInt, fmtTokens } from '../lib/format.js';

const now = () => Math.floor(Date.now() / 1000);

function statusTag(a) {
  const n = now();
  if ((a.quarantine_until || 0) > n) return <Tag color="red">隔离</Tag>;
  if (a.egress_binding && a.egress_binding.recheck_pending) return <Tag color="orange">待测活</Tag>;
  if (a.egress_binding && (a.egress_binding.cooldown_until || 0) > n) return <Tag color="amber">冷却</Tag>;
  if (a.status === 'active') return <Tag color="green">可用</Tag>;
  return <Tag color="grey">{a.status || '未知'}</Tag>;
}

function normalizeAccounts(data) {
  if (Array.isArray(data)) return { rows: data, total: data.length };
  return { rows: data?.accounts || [], total: data?.total || 0 };
}

const EMPTY_ACCOUNT_DATA = { rows: [], total: 0, groups: [], error: null };
const accountActionKey = (id, act) => `${id}:${act}`;

function accountUsage(account) {
  return account?.usage || {};
}

function quotaPercent(account) {
  const candidates = [account.quota_pct, account.quota_percent, account.quota_used_pct, account.usage_pct];
  const direct = candidates.find((value) => Number.isFinite(Number(value)));
  if (direct !== undefined) return Math.max(0, Math.min(100, Number(direct)));
  const remaining = Number(account.remaining_tokens ?? account.quota_remaining_tokens);
  const limit = Number(account.limit_tokens ?? account.quota_limit_tokens);
  if (Number.isFinite(remaining) && Number.isFinite(limit) && limit > 0) {
    return Math.max(0, Math.min(100, Math.round(((limit - remaining) / limit) * 100)));
  }
  return null;
}

function routeSummary(account) {
  const binding = account.egress_binding || {};
  const primary = binding.primary_egress_id || account.egress_id || '默认出口';
  const model = account.force_model || account.model || '';
  const effort = account.force_effort || account.effort || '';
  return { primary, model, effort };
}

export default function Accounts() {
  const [page, setPage] = useState(1);
  const [pageSize] = useState(50);
  const [importOpen, setImportOpen] = useState(false);
  const [search, setSearch] = useState('');
  const [searchInput, setSearchInput] = useState('');
  const [selected, setSelected] = useState([]);
  const [drawerAcct, setDrawerAcct] = useState(null);
  const [moveOpen, setMoveOpen] = useState(false);
  const [moveGroup, setMoveGroup] = useState('');

  const fetchAccountData = useCallback(async ({ signal }) => {
    const { values, error } = await loadResourceGroup({
      accounts: { label: '账号列表', load: () => get('/admin/accounts', { page, pageSize, search }, { signal }) },
      groups: { label: '分组选项', load: () => get('/admin/groups', undefined, { signal }) },
    });
    if (error?.failures?.some((failure) => failure.key === 'accounts')) throw error;
    const accountData = normalizeAccounts(values.accounts);
    return {
      ...accountData,
      groups: Array.isArray(values.groups) ? values.groups : values.groups?.groups || [],
      error,
    };
  }, [page, pageSize, search]);

  const {
    data = EMPTY_ACCOUNT_DATA,
    loading,
    error,
    lastRefresh,
    reload: load,
  } = useAsyncResource(fetchAccountData, [fetchAccountData], { initialData: EMPTY_ACCOUNT_DATA });
  const rows = data.rows || [];
  const total = data.total || 0;
  const groups = data.groups || [];
  const loadError = error || data.error;

  const doSearch = () => { setPage(1); setSearch(searchInput.trim()); };
  const onPageChange = (cur) => { setPage(cur); };

  const {
    run: runAccountAction,
    running: accountActionRunning,
    isRunning: isAccountActionKeyRunning,
  } = useKeyedAsyncAction(async (_key, id, act) => {
    try {
      await post(`/admin/accounts/${id}/${act}`, {});
      Toast.success(`${act} 已执行`);
      const nextData = await load();
      if (drawerAcct?.id === id) {
        if (act === 'delete') {
          setDrawerAcct(null);
        } else {
          const fresh = nextData?.rows?.find((row) => row.id === id);
          if (fresh) setDrawerAcct(fresh);
        }
      }
      return true;
    } catch (e) {
      showErrorToast(e);
      return false;
    }
  });
  const action = (id, act) => runAccountAction(accountActionKey(id, act), id, act);

  const handleAccountUpdated = async (id, patch = {}) => {
    const nextData = await load();
    const fresh = nextData?.rows?.find((row) => row.id === id);
    if (fresh) {
      setDrawerAcct(fresh);
    } else if (drawerAcct?.id === id) {
      setDrawerAcct((current) => current ? { ...current, egress_binding: patch } : current);
    }
  };

  const { run: bulkAction, running: bulkActionRunning } = useAsyncAction(async (act, label) => {
    if (!selected.length) return;
    const result = await batchOp(label, selected, (id) => post(`/admin/accounts/${id}/${act}`, {}));
    const failedIDs = result.failed.map((item) => item.id);
    if (result.failed.length) {
      const firstErrors = result.failed.slice(0, 3).map((item) => {
        const requestID = item.request_id ? `，请求 ID: ${item.request_id}` : '';
        return `${item.id}: ${item.error}${requestID}`;
      }).join('；');
      Toast.warning(`「${label}」完成：成功 ${result.success.length}，失败 ${result.failed.length}。${firstErrors}`);
    } else {
      Toast.success(`已对 ${result.success.length} 个账号执行「${label}」`);
    }
    setSelected(failedIDs);
    await load();
  });

  const { run: bulkMove, running: bulkMoveRunning } = useAsyncAction(async () => {
    try {
      await post('/admin/accounts/assign-group', { ids: selected, group: moveGroup });
      Toast.success(`已移动 ${selected.length} 个账号到「${moveGroup || '默认'}」`);
      setMoveOpen(false);
      setSelected([]);
      await load();
    } catch (e) { showErrorToast(e); }
  });
  const anyAccountOperationRunning = accountActionRunning || bulkActionRunning || bulkMoveRunning;
  const isAccountActionLoading = (id, act) => isAccountActionKeyRunning(accountActionKey(id, act));

  const renderAccountActions = (r) => (
    <ActionMenu
      label="账号操作"
      items={[
        {
          label: isAccountActionLoading(r.id, 'health-test') ? '测活中' : '测活',
          disabled: anyAccountOperationRunning && !isAccountActionLoading(r.id, 'health-test'),
          onSelect: () => action(r.id, 'health-test'),
        },
        {
          label: isAccountActionLoading(r.id, 'clear-quarantine') ? '解隔中' : '解隔',
          disabled: anyAccountOperationRunning && !isAccountActionLoading(r.id, 'clear-quarantine'),
          onSelect: () => action(r.id, 'clear-quarantine'),
        },
        { label: '详情', disabled: anyAccountOperationRunning, onSelect: () => setDrawerAcct(r) },
        {
          label: isAccountActionLoading(r.id, 'delete') ? '删除中' : '删除',
          destructive: true,
          disabled: anyAccountOperationRunning && !isAccountActionLoading(r.id, 'delete'),
          confirm: {
            title: '确认删除该账号？',
            description: `账号 ${r.label || r.email || r.id} 删除后不可恢复。`,
            confirmText: '删除',
          },
          onSelect: () => action(r.id, 'delete'),
        },
      ]}
    />
  );

  const filtered = rows; // filtering is now server-side

  const exportCSV = () => {
    const cols = [
      { title: 'id', get: (r) => r.id }, { title: 'label', get: (r) => r.label }, { title: 'email', get: (r) => r.email },
      { title: 'provider', get: (r) => r.provider }, { title: 'group', get: (r) => r.group_name },
      { title: 'plan', get: (r) => r.plan_type }, { title: 'status', get: (r) => r.status },
    ];
    const ok = downloadCSV('accounts.csv', toCSV(filtered, cols));
    if (!ok) Toast.error('导出失败，请检查浏览器下载权限');
  };

  const columns = [
    {
      title: '账号',
      dataIndex: 'label',
      width: 330,
      sorter: (a, b) => String(a.label || a.id).localeCompare(String(b.label || b.id)),
      render: (_, r) => (
        <div className="pool-account-summary">
          <div className="pool-account-titleline">
            <TextClamp strong onClick={() => setDrawerAcct(r)}>{r.label || r.id}</TextClamp>
            <Tag size="small">{r.provider || 'codex'}</Tag>
          </div>
          <div className="pool-account-metaline">
            <TextClamp muted className="pool-account-email">{r.email || r.id}</TextClamp>
            <span className="pool-account-mini">{[r.group_name || '默认', r.plan_type || '未设置'].join(' / ')}</span>
          </div>
        </div>
      ),
    },
    { title: '健康', key: 'status', width: 92, render: (_, r) => statusTag(r) },
    {
      title: '配额',
      key: 'quota',
      width: 132,
      render: (_, r) => {
        const pct = quotaPercent(r);
        return (
          <div className="pool-quota-cell">
            <span>{pct === null ? '未同步' : `${pct}% 已用`}</span>
            <TinyMeter value={pct ?? 0} label={pct === null ? '未同步' : `${pct}% 已用`} />
          </div>
        );
      },
    },
    {
      title: '路由',
      key: 'route',
      width: 190,
      render: (_, r) => {
        const route = routeSummary(r);
        return (
          <div className="pool-resource-summary">
            <TextClamp>{route.primary}</TextClamp>
            <div className="pool-resource-summary__meta">
              {route.model ? <Tag size="small">{route.model}</Tag> : <span>默认模型</span>}
              {route.effort ? <Tag size="small" color="violet">{route.effort}</Tag> : null}
            </div>
          </div>
        );
      },
    },
    {
      title: '用量',
      key: 'usage',
      width: 132,
      render: (_, r) => {
        const usage = accountUsage(r);
        const requests = usage.requests ?? r.requests;
        const tokens = usage.total_tokens ?? r.total_tokens;
        return (
          <div className="pool-resource-summary">
            <TextClamp>{tokens == null ? '—' : fmtTokens(tokens)}</TextClamp>
            <div className="pool-resource-summary__meta">{requests == null ? '请求未同步' : `${fmtInt(requests)} 次请求`}</div>
          </div>
        );
      },
    },
    { title: '操作', key: 'ops', width: 236, render: (_, r) => renderAccountActions(r) },
  ];
  const mobileColumns = [
    {
      title: '账号',
      dataIndex: 'label',
      render: (_, r) => (
        <MobileResourceCell
          title={<Typography.Text link onClick={() => setDrawerAcct(r)}>{r.label || r.id}</Typography.Text>}
          subtitle={r.email}
          badges={<>{statusTag(r)}<Tag>{r.provider || 'codex'}</Tag></>}
          details={[
            { label: '分组', value: r.group_name || '默认' },
            { label: '套餐', value: r.plan_type || '未设置' },
            { label: '出口', value: routeSummary(r).primary },
          ]}
          actions={renderAccountActions(r)}
        />
      ),
    },
  ];

  return (
    <div>
      <PageHeader title="账号池" subtitle={`共 ${total} 个账号`}
        actions={<>
          <Input prefix={<IconSearch />} value={searchInput} onChange={setSearchInput}
            onEnterPress={doSearch} style={{ width: 220 }} placeholder="搜索 标签/邮箱/分组" showClear onClear={doSearch} />
          <Button icon={<IconSearch />} onClick={doSearch}>搜索</Button>
          <Button icon={<IconDownload />} onClick={exportCSV}>导出 CSV</Button>
          <Button icon={<IconRefresh />} onClick={() => load()}>刷新</Button>
          <Button icon={<IconPlus />} theme="solid" onClick={() => setImportOpen(true)}>添加账号</Button>
        </>} />

      {selected.length > 0 && (
        <div className="pool-bulkbar">
          <span>已选 <b>{selected.length}</b> 项</span>
          <Button size="small" loading={bulkActionRunning} disabled={accountActionRunning || bulkMoveRunning} onClick={() => bulkAction('health-test', '测活')}>批量测活</Button>
          <Button size="small" loading={bulkActionRunning} disabled={accountActionRunning || bulkMoveRunning} onClick={() => bulkAction('clear-quarantine', '解隔离')}>批量解隔离</Button>
          <Button size="small" disabled={anyAccountOperationRunning} onClick={() => { setMoveGroup(''); setMoveOpen(true); }}>移动分组</Button>
          <ConfirmDialog
            title={`删除选中的 ${selected.length} 个账号？`}
            description="批量删除后不可恢复，失败项会保留在已选列表中。"
            destructive
            confirmText="批量删除"
            onConfirm={() => bulkAction('delete', '删除')}
          >
            <Button size="small" type="danger" loading={bulkActionRunning} disabled={accountActionRunning || bulkMoveRunning}>批量删除</Button>
          </ConfirmDialog>
          <Button size="small" theme="borderless" disabled={anyAccountOperationRunning} onClick={() => setSelected([])}>取消</Button>
        </div>
      )}

      <ResourceTable
        error={loadError}
        onRetry={load}
        loading={loading}
        lastRefresh={lastRefresh}
        dataSource={filtered}
        columns={columns}
        rowKey="id"
        pagination={{ pageSize, total, currentPage: page, onPageChange: onPageChange }}
        size="middle"
        rowSelection={{ selectedRowKeys: selected, onChange: (keys) => setSelected(keys) }}
        className="pool-mobile-table pool-accounts-table"
        density="account"
        minScrollX={1092}
        rowHeight={72}
        mobileColumns={mobileColumns}
        mobileScroll={false}
        emptyTitle="账号池为空"
        emptyDesc="导入 auth.json 或开启自动注册来填充账号池"
        emptyType="accounts"
        emptyAction={<Button theme="solid" icon={<IconPlus />} onClick={() => setImportOpen(true)}>导入 auth.json</Button>}
        skeletonRows={8}
        skeletonCols={6}
      />

      <AccountDrawer account={drawerAcct} usage={drawerAcct ? drawerAcct.usage : null}
        statusTag={statusTag} onAction={action} actionRunning={accountActionRunning}
        actionDisabled={bulkActionRunning || bulkMoveRunning} isActionLoading={isAccountActionLoading}
        onUpdated={handleAccountUpdated} onClose={() => setDrawerAcct(null)} />
      <Modal title="批量移动到分组" visible={moveOpen} onCancel={() => { if (!bulkMoveRunning) setMoveOpen(false); }} onOk={bulkMove} confirmLoading={bulkMoveRunning} okText="移动">
        <Select value={moveGroup} onChange={setMoveGroup} style={{ width: '100%' }} placeholder="选择目标分组"
          optionList={groups.map((g) => ({ label: g.name, value: g.name }))} />
      </Modal>
      <OAuthLoginModal open={importOpen} onClose={() => setImportOpen(false)} onSuccess={load} />
    </div>
  );
}
