import React, { useState, useCallback } from 'react';
import {
  ActionMenu, Button, ConfirmDialog, Tag, Toast, Modal, Form, Typography, Input, Select,
} from '../components/pool/index.jsx';
import { IconRefresh, IconPlus, IconSearch, IconDownload } from '../components/pool/icons.jsx';
import { post, batchOp } from '../api.js';
import PageHeader from '../components/PageHeader.jsx';
import ResourceTable from '../components/ResourceTable.jsx';
import AccountDrawer from '../components/AccountDrawer.jsx';
import OAuthLoginModal from '../components/OAuthLoginModal.jsx';
import MobileResourceCell from '../components/MobileResourceCell.jsx';
import { TextClamp, TinyMeter } from '../components/DisplayPrimitives.jsx';
import { showErrorToast } from '../components/ErrorToast.jsx';
import useAsyncAction from '../hooks/useAsyncAction.js';
import useKeyedAsyncAction from '../hooks/useKeyedAsyncAction.js';
import useResponsiveLayout from '../hooks/useResponsiveLayout.js';
import { toCSV, downloadCSV } from '../lib/csv.js';
import { fmtInt, fmtTokens } from '../lib/format.js';
import { useAccountsPage } from '../features/accounts/queries/accounts.ts';
import {
  healthBatchPresentation, healthResultPresentation, healthTestRequestBody,
  isKiroAccount, isKiroSuspended, selectedHasKiro,
} from '../features/accounts/model/healthTest.ts';

const now = () => Math.floor(Date.now() / 1000);

function statusInfo(a) {
  const n = now();
  if (isKiroSuspended(a)) return { label: 'AWS User ID 已暂停', color: 'red', hint: '无限期隔离；需 AWS 支持处理后双层测活' };
  if ((a.quarantine_until || 0) > n) return { label: '隔离中', color: 'red', hint: '暂不参与调度' };
  if (a.egress_binding && a.egress_binding.recheck_pending) return { label: '待复测', color: 'orange', hint: '等待重新测活' };
  if (a.egress_binding && (a.egress_binding.cooldown_until || 0) > n) return { label: '冷却中', color: 'amber', hint: '临时退避' };
  const map = {
    active: { label: '可用', color: 'green', hint: '可立即调度' },
    permission_denied: { label: '权限受限', color: 'red', hint: '凭据或权限需要处理' },
    unreachable: { label: '不可达', color: 'grey', hint: '网络或出口不可用' },
    disabled: { label: '已停用', color: 'grey', hint: '不会参与调度' },
    quarantine: { label: '隔离中', color: 'red', hint: '暂不参与调度' },
  };
  return map[a.status] || { label: a.status || '未知', color: 'grey', hint: '等待状态同步' };
}

function statusTag(a) {
  const info = statusInfo(a);
  return <Tag color={info.color}>{info.label}</Tag>;
}

const EMPTY_ACCOUNT_DATA = { rows: [], total: 0, groups: [], error: null };
const accountActionKey = (id, act) => `${id}:${act}`;
const ACCOUNT_ACTION_LABEL = {
  'health-test': '测活',
  'clear-quarantine': '解除隔离',
  delete: '删除',
};

function accountUsage(account) {
  return account?.usage || {};
}

function quotaPercent(account) {
  const summaryPct = account?.quota_summary?.primary?.used_percent;
  if (Number.isFinite(Number(summaryPct)) && Number(summaryPct) >= 0) {
    return Math.max(0, Math.min(100, Math.round(Number(summaryPct))));
  }
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

function quotaReason(account) {
  return account?.quota_summary?.sync_reason || (quotaPercent(account) == null ? 'never_polled' : 'ok');
}

function routeSummary(account) {
  const binding = account.egress_binding || {};
  const primary = binding.primary_egress_id || account.egress_id || '默认出口';
  const model = account.force_model || account.model || '';
  const effort = account.force_effort || account.effort || '';
  return { primary, model, effort };
}

function middleEllipsis(value, head = 24, tail = 14) {
  const text = String(value || '');
  if (text.length <= head + tail + 1) return text;
  return `${text.slice(0, head)}…${text.slice(-tail)}`;
}

export default function Accounts() {
  const responsive = useResponsiveLayout();
  const [page, setPage] = useState(1);
  const [pageSize] = useState(50);
  const [importOpen, setImportOpen] = useState(false);
  const [search, setSearch] = useState('');
  const [searchInput, setSearchInput] = useState('');
  const [selected, setSelected] = useState([]);
  const [selectedAccountMeta, setSelectedAccountMeta] = useState({});
  const [selectMode, setSelectMode] = useState(false);
  const [drawerAcct, setDrawerAcct] = useState(null);
  const [moveOpen, setMoveOpen] = useState(false);
  const [moveGroup, setMoveGroup] = useState('');
  const [moveIDs, setMoveIDs] = useState([]);

  const {
    data = EMPTY_ACCOUNT_DATA,
    loading,
    error,
    lastRefresh,
    reload: load,
  } = useAccountsPage({ page, pageSize, search });
  const rows = data.rows || [];
  const total = data.total || 0;
  const groups = data.groups || [];
  const loadError = error || data.error;
  const accountByID = new Map(rows.map((account) => [account.id, account]));
  const selectedAccounts = selected.map((id) => accountByID.get(id) || selectedAccountMeta[id] || { id });
  const bulkHealthIncludesKiro = selectedHasKiro(selectedAccounts, selected);
  const handleSelectionChange = useCallback((keys) => {
    setSelected(keys);
    setSelectedAccountMeta((previous) => {
      const visible = new Map(rows.map((account) => [account.id, account]));
      return Object.fromEntries(keys.map((id) => [id, visible.get(id) || previous[id] || { id }]));
    });
  }, [rows]);

  const doSearch = () => { setPage(1); setSearch(searchInput.trim()); };
  const onPageChange = (cur) => { setPage(cur); };

  const {
    run: runAccountAction,
    running: accountActionRunning,
    isRunning: isAccountActionKeyRunning,
  } = useKeyedAsyncAction(async (_key, id, act, requestBody = {}) => {
    try {
      const result = await post(`/admin/accounts/${encodeURIComponent(id)}/${act}`, requestBody);
      if (act === 'health-test') {
        const presentation = healthResultPresentation(result);
        Toast[presentation.tone](presentation.message);
      } else {
        Toast.success(`${ACCOUNT_ACTION_LABEL[act] || '操作'}已完成`);
      }
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
  const action = (id, act, requestBody = {}) => runAccountAction(accountActionKey(id, act), id, act, requestBody);

  const handleAccountUpdated = async (id, patch = {}) => {
    const nextData = await load();
    const fresh = nextData?.rows?.find((row) => row.id === id);
    if (fresh) {
      setDrawerAcct(fresh);
    } else if (drawerAcct?.id === id) {
      setDrawerAcct((current) => current ? { ...current, egress_binding: patch } : current);
    }
  };

  const { run: bulkAction, running: bulkActionRunning } = useAsyncAction(async (act, label, kiroCostConfirmed = false) => {
    if (!selected.length) return;
    const healthEntries = [];
    const result = await batchOp(label, selected, async (id) => {
      const account = accountByID.get(id) || selectedAccountMeta[id] || { id };
      const response = await post(
        `/admin/accounts/${encodeURIComponent(id)}/${act}`,
        act === 'health-test' ? healthTestRequestBody(account, kiroCostConfirmed) : {},
      );
      if (act === 'health-test') healthEntries.push({ account, result: response });
      return response;
    });
    const failedIDs = result.failed.map((item) => item.id);
    if (act === 'health-test') {
      const presentation = healthBatchPresentation(healthEntries, result.failed.length);
      Toast[presentation.tone](presentation.message);
      if (result.failed.length) {
        const firstErrors = result.failed.slice(0, 3).map((item) => `${item.id}: ${item.error}`).join('；');
        Toast.warning(firstErrors);
      }
    } else if (result.failed.length) {
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
    const ids = moveIDs.length ? moveIDs : selected;
    if (!ids.length) return;
    try {
      await post('/admin/accounts/assign-group', { ids, group: moveGroup });
      Toast.success(`已移动 ${ids.length} 个账号到「${moveGroup || '默认'}」`);
      setMoveOpen(false);
      setMoveIDs([]);
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
          confirm: isKiroAccount(r) ? {
            title: '确认执行 Kiro 双层测活？',
            description: '将先免费检查认证；认证正常后，会绑定此账号和出口发送 1 次最小推理请求并消耗少量 credits。',
            confirmText: '确认并测活',
          } : undefined,
          onSelect: () => action(r.id, 'health-test', healthTestRequestBody(r, isKiroAccount(r))),
        },
        {
          label: isAccountActionLoading(r.id, 'clear-quarantine') ? '解隔中' : '解隔',
          disabled: isKiroSuspended(r) || (anyAccountOperationRunning && !isAccountActionLoading(r.id, 'clear-quarantine')),
          onSelect: () => action(r.id, 'clear-quarantine'),
        },
        {
          label: '移动分组',
          disabled: anyAccountOperationRunning,
          onSelect: () => {
            setMoveIDs([r.id]);
            setMoveGroup(r.group_name || '');
            setMoveOpen(true);
          },
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
            <TextClamp muted className="pool-account-email" title={r.email || r.id}>{middleEllipsis(r.email || r.id)}</TextClamp>
          </div>
        </div>
      ),
    },
    {
      title: '健康',
      key: 'status',
      width: 160,
      render: (_, r) => {
        const info = statusInfo(r);
        return (
          <div className="pool-resource-summary">
            <div>{statusTag(r)}</div>
            <div className="pool-resource-summary__meta">{info.hint}</div>
          </div>
        );
      },
    },
    {
      title: '分组 / 套餐',
      key: 'group_plan',
      width: 156,
      render: (_, r) => {
        return (
          <div className="pool-resource-summary">
            <TextClamp>{r.group_name || '默认'}</TextClamp>
            <div className="pool-resource-summary__meta">{r.plan_type || '未设置套餐'}</div>
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
      width: 150,
      align: 'right',
      render: (_, r) => {
        const usage = accountUsage(r);
        const requests = usage.requests ?? r.requests;
        const tokens = usage.total_tokens ?? r.total_tokens;
        const pct = quotaPercent(r);
        const reason = quotaReason(r);
        return (
          <div className="pool-quota-cell">
            <TextClamp>{tokens == null ? '—' : fmtTokens(tokens)}</TextClamp>
            <div className="pool-resource-summary__meta">{requests == null ? '请求未同步' : `${fmtInt(requests)} 次请求`}</div>
            <span className="pool-resource-summary__meta">{pct === null ? `配额未同步 · ${reason}` : `配额 ${pct}% 已用`}</span>
            <TinyMeter value={pct ?? 0} label={pct === null ? '未同步' : `${pct}% 已用`} />
          </div>
        );
      },
    },
    { title: '操作', key: 'ops', width: 72, render: (_, r) => renderAccountActions(r) },
  ];
  const mobileAccountCell = (r) => {
    const route = routeSummary(r);
    const usage = accountUsage(r);
    const requests = usage.requests ?? r.requests;
    const tokens = usage.total_tokens ?? r.total_tokens;
    return (
      <MobileResourceCell
        title={selectMode
          ? middleEllipsis(r.label || r.id, 20, 8)
          : <Typography.Text link onClick={() => setDrawerAcct(r)}>{middleEllipsis(r.label || r.id, 20, 8)}</Typography.Text>}
        subtitle={middleEllipsis(r.email || r.id, 22, 12)}
        badges={statusTag(r)}
        chips={<>
          <Tag size="small">{r.provider || 'codex'}</Tag>
          <Tag size="small">{r.group_name || '默认'}</Tag>
          {r.plan_type ? <Tag size="small">{r.plan_type}</Tag> : null}
          <Tag size="small" color="blue">{route.primary}</Tag>
        </>}
        details={[
          { label: '用量', value: tokens == null ? 'Token 未同步' : `${fmtTokens(tokens)} Token` },
          { label: '请求', value: requests == null ? null : `${fmtInt(requests)} 次请求` },
        ]}
        actions={!selectMode ? renderAccountActions(r) : null}
      />
    );
  };
  const mobileColumns = [
    {
      title: '账号',
      dataIndex: 'label',
      render: (_, r) => mobileAccountCell(r),
    },
  ];

  return (
    <div>
      <PageHeader
        title="账号池"
        subtitle={!lastRefresh && loading ? '正在读取账号…' : !lastRefresh && loadError ? '账号数据暂时不可用' : `共 ${total} 个账号`}
        actions={<>
          <Input prefix={<IconSearch />} value={searchInput} onChange={setSearchInput}
            onEnterPress={doSearch} style={{ width: responsive.isMobile ? 210 : 220 }} placeholder="搜索 标签/邮箱/分组" showClear onClear={doSearch} />
          <Button icon={<IconSearch />} onClick={doSearch}>搜索</Button>
          {!responsive.isMobile ? <Button icon={<IconDownload />} onClick={exportCSV}>导出 CSV</Button> : null}
          <Button icon={<IconRefresh />} onClick={() => load()}>刷新</Button>
          {responsive.isMobile ? (
            <Button onClick={() => { setSelectMode((value) => !value); if (selectMode) setSelected([]); }}>
              {selectMode ? '完成' : '选择'}
            </Button>
          ) : null}
          {!responsive.isMobile ? <Button icon={<IconPlus />} theme="solid" onClick={() => setImportOpen(true)}>添加账号</Button> : null}
        </>} />

      {selected.length > 0 && (
        <div className="pool-bulkbar">
          <span>已选 <b>{selected.length}</b> 项</span>
          {bulkHealthIncludesKiro ? (
            <ConfirmDialog
              title="确认批量执行 Kiro 双层测活？"
              description="所选账号中包含 Kiro。每个认证正常的 Kiro 账号会发送 1 次最小推理请求并消耗少量 credits；其他提供商保持原测活方式。"
              confirmText="确认并批量测活"
              onConfirm={() => bulkAction('health-test', '测活', true)}
            >
              <Button size="small" loading={bulkActionRunning} disabled={accountActionRunning || bulkMoveRunning}>批量测活</Button>
            </ConfirmDialog>
          ) : (
            <Button size="small" loading={bulkActionRunning} disabled={accountActionRunning || bulkMoveRunning} onClick={() => bulkAction('health-test', '测活')}>批量测活</Button>
          )}
          <Button size="small" loading={bulkActionRunning} disabled={accountActionRunning || bulkMoveRunning} onClick={() => bulkAction('clear-quarantine', '解隔离')}>批量解隔离</Button>
          <Button size="small" disabled={anyAccountOperationRunning} onClick={() => { setMoveIDs([...selected]); setMoveGroup(''); setMoveOpen(true); }}>移动分组</Button>
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
        rowSelection={!responsive.isMobile || selectMode ? { selectedRowKeys: selected, onChange: handleSelectionChange } : undefined}
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

      {responsive.isMobile && !selectMode ? (
        <div className="pool-sticky-mobile-action">
          <Button icon={<IconPlus />} theme="solid" onClick={() => setImportOpen(true)}>添加账号</Button>
        </div>
      ) : null}

      <AccountDrawer account={drawerAcct} usage={drawerAcct ? drawerAcct.usage : null}
        statusTag={statusTag} onAction={action} actionRunning={accountActionRunning}
        actionDisabled={bulkActionRunning || bulkMoveRunning} isActionLoading={isAccountActionLoading}
        onUpdated={handleAccountUpdated} onClose={() => setDrawerAcct(null)} />
      <Modal title={moveIDs.length === 1 ? '移动账号到分组' : '批量移动到分组'} visible={moveOpen} onCancel={() => { if (!bulkMoveRunning) { setMoveOpen(false); setMoveIDs([]); } }} onOk={bulkMove} confirmLoading={bulkMoveRunning} okText="移动">
        <Select value={moveGroup} onChange={setMoveGroup} style={{ width: '100%' }} placeholder="选择目标分组"
          optionList={groups.map((g) => ({ label: g.name, value: g.name }))} />
      </Modal>
      <OAuthLoginModal open={importOpen} onClose={() => setImportOpen(false)} onSuccess={load} />
    </div>
  );
}
