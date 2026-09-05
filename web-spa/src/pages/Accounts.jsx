import React, { useState, useCallback, useEffect, useRef } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import {
  ActionMenu, Button, ConfirmDialog, Tag, Toast, Modal, Form, Typography, Input, Select,
} from '../components/pool/index.jsx';
import { IconRefresh, IconPlus, IconSearch, IconDownload, IconFile } from '../components/pool/icons.jsx';
import { post, batchOp } from '../api.js';
import PageHeader from '../components/PageHeader.jsx';
import ResourceTable from '../components/ResourceTable.jsx';
import AccountDrawer from '../components/AccountDrawer.jsx';
import Sub2APIHubPanel from '../components/Sub2APIHubPanel.jsx';
import OAuthLoginModal from '../components/OAuthLoginModal.jsx';
import MobileResourceCell from '../components/MobileResourceCell.jsx';
import { TextClamp, TinyMeter } from '../components/DisplayPrimitives.jsx';
import { showErrorToast } from '../components/ErrorToast.jsx';
import useAsyncAction from '../hooks/useAsyncAction.js';
import useInstantMutation from '../hooks/useInstantMutation.ts';
import useKeyedAsyncAction from '../hooks/useKeyedAsyncAction.js';
import useResponsiveLayout from '../hooks/useResponsiveLayout.js';
import { toCSV, downloadCSV } from '../lib/csv.js';
import { fmtInt, fmtRelative, fmtTokens, fmtUSD, middleEllipsis } from '../lib/format.js';
import { quotaWindowForKind, quotaUsageDetails, quotaWindowLabel } from '../lib/quotaWindows.js';
import { accountQueryKeys, useAccountsPage } from '../features/accounts/queries/accounts.ts';
import {
  downloadConfirmedAccountExport, fetchAccountArchive, fetchAccountsPage,
  importAccountArchive, preflightAccountExport,
} from '../features/accounts/api/accounts.ts';
import {
  seedAccountRates, subscribeAccountRateBatches, subscribeAccountRateFeed, useAccountRequestRate,
} from '../features/accounts/live/accountRates.ts';
import { abortController, abortSignal, createAbortController } from '../lib/browserAbort.js';
import { downloadBlob } from '../lib/browserDownload.js';
import { clearBrowserTimeout, setBrowserTimeout } from '../lib/browserLifecycle.js';
import {
  healthBatchPresentation, healthResultPresentation, healthTestRequestBody,
  isKiroSuspended, isProtectedProbeQuarantine, requiresPaidHealthTest, selectedHasPaidProbe,
} from '../features/accounts/model/healthTest.ts';
import { formatPlanLabel } from '../features/accounts/model/planFormatter.ts';

const now = () => Math.floor(Date.now() / 1000);
const MANUAL_FORCE_ISOLATION_UNTIL = 253402300799;

function statusInfo(a) {
  const n = now();
  const ignoresRateLimitControls = Boolean(a.ignore_rate_limit_controls);
  const hasIgnoredControlState = (a.quarantine_until || 0) > n
    || Boolean(a.egress_binding?.recheck_pending)
    || (a.egress_binding?.cooldown_until || 0) > n;
  if (ignoresRateLimitControls && hasIgnoredControlState) return { label: '例外调度', color: 'orange', hint: '已忽略此账号的 429、冷却与隔离状态' };
  if (!ignoresRateLimitControls && (a.quarantine_reason === 'provider_api_key_inference_probe_pending' || a.quarantine_reason === 'kiro_import_validation_pending')) {
    return { label: '后台验证中', color: 'blue', hint: '凭据已保存；验证完成前不参与调度' };
  }
  if (!ignoresRateLimitControls && isKiroSuspended(a)) return { label: 'AWS User ID 已暂停', color: 'red', hint: '无限期隔离；需 AWS 支持处理后双层测活' };
  if (!ignoresRateLimitControls && (a.quarantine_until || 0) > n) return { label: '隔离中', color: 'red', hint: '暂不参与调度' };
  if (!ignoresRateLimitControls && a.egress_binding && a.egress_binding.recheck_pending) return { label: '待复测', color: 'orange', hint: '等待重新测活' };
  if (!ignoresRateLimitControls && a.egress_binding && (a.egress_binding.cooldown_until || 0) > n) return { label: '冷却中', color: 'amber', hint: '临时退避' };
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
  'clear-cooldown': '解除冷却',
  'reset-credits': '主动重置',
  'force-isolation': '强制隔离',
  delete: '删除',
};

function accountUsage(account) {
  return account?.usage || {};
}

const REQUEST_RATE_STATE = {
  live: { label: '实时', hint: '滚动 60 秒' },
  stale: { label: '数据延迟', hint: '持久化暂时延迟' },
  unavailable: { label: '暂不可用', hint: '等待采样器恢复' },
};

function AccountRateMetric({ account, compact = false }) {
  const rate = useAccountRequestRate(account.id, account.request_rate);
  const state = REQUEST_RATE_STATE[rate.state] || REQUEST_RATE_STATE.unavailable;
  const known = rate.state === 'live' || rate.state === 'stale';
  const active = known && (rate.logical_rpm > 0 || rate.tpm > 0);
  const title = known
    ? `逻辑请求 ${fmtInt(rate.logical_rpm)} RPM · Token ${fmtTokens(rate.tpm)} TPM · 上游尝试 ${fmtInt(rate.attempt_rpm)} RPM · 根任务 ${fmtInt(rate.root_rpm)} · 子 agent ${fmtInt(rate.subagent_rpm)} · 未识别 ${fmtInt(rate.unknown_rpm)}`
    : `${state.label} · ${state.hint}`;
  if (compact) {
    return (
      <Tag
        size="small"
        color={active ? 'blue' : rate.state === 'stale' ? 'amber' : 'grey'}
        className={active ? 'pool-account-rpm-chip pool-account-rpm-chip--active' : 'pool-account-rpm-chip'}
        title={title}
      >
        {known ? `${fmtInt(rate.logical_rpm)} RPM · ${fmtTokens(rate.tpm)} TPM` : 'RPM / TPM —'}
      </Tag>
    );
  }
  return (
    <div
      className={`pool-account-rpm${active ? ' pool-account-rpm--active' : ''}`}
      aria-label={known ? `${title}，${state.hint}，${state.label}` : `RPM 和 TPM ${state.label}`}
      title={title}
    >
      <strong>{known ? fmtInt(rate.logical_rpm) : '—'} <span>RPM</span></strong>
      <small>{known ? `${fmtTokens(rate.tpm)} TPM` : state.label}</small>
    </div>
  );
}

function AccountRateDetail({ account }) {
  const rate = useAccountRequestRate(account.id, account.request_rate);
  if (rate.state === 'unavailable') return '暂不可用 · 等待采样器恢复';
  return `${fmtInt(rate.logical_rpm)} RPM · ${fmtTokens(rate.tpm)} TPM · 根 ${fmtInt(rate.root_rpm)} / 子 ${fmtInt(rate.subagent_rpm)} / 未识别 ${fmtInt(rate.unknown_rpm)} · 上游尝试 ${fmtInt(rate.attempt_rpm)} · ${rate.state === 'stale' ? '数据延迟' : '滚动 60 秒'} · ${rate.sampled_at ? `${fmtRelative(rate.sampled_at)}采样` : '刚刚采样'}`;
}

function ExportPreflightSummary({ preview, policy }) {
  if (!preview) return null;
  const blocked = policy === 'fail_all' && preview.incompatible > 0;
  return (
    <div className="pool-export-preflight" role="status" aria-live="polite">
      <div className="pool-export-preflight__summary">
        <Tag color={preview.compatible ? 'green' : 'grey'}>{preview.compatible} 个兼容</Tag>
        <Tag color={preview.incompatible ? 'red' : 'green'}>{preview.incompatible} 个不兼容</Tag>
        <span>确认令牌 5 分钟内有效且只能使用一次。</span>
      </div>
      {blocked ? <div className="pool-callout pool-callout--warning">当前策略为“发现不兼容即全部停止”。请取消后改为“跳过并生成报告”，或重新选择账号。</div> : null}
      <div className="pool-export-preflight__items">
        {preview.items.map((item) => (
          <div className="pool-export-preflight__item" key={item.account_code} data-compatible={item.compatible ? 'true' : 'false'}>
            <div>
              <strong>{item.account_code}</strong>
              <Tag size="small" color={item.compatible ? 'green' : 'red'}>{item.compatible ? '可导出' : '不可导出'}</Tag>
              {item.planned_filename ? <span>{item.planned_filename}</span> : null}
            </div>
            {item.secret_types.length ? <small>将包含：{item.secret_types.join('、')}</small> : null}
            {item.errors.map((message) => <p className="pool-export-preflight__error" key={`e:${message}`}>{message}</p>)}
            {item.warnings.map((message) => <p className="pool-export-preflight__warning" key={`w:${message}`}>{message}</p>)}
          </div>
        ))}
      </div>
      <p className="pool-field__help">预检只展示账号代号、文件名和凭据类型，不会把 Token、邮箱、代理地址或密码写入页面。</p>
    </div>
  );
}

export function accountCredentialPresentation(account) {
  const method = String(account?.auth_method || account?.kiro_auth?.auth_method || '').trim().toLowerCase();
  const apiKey = method === 'api_key' || Boolean(account?.api_key_present) || account?.billing_mode === 'pay_as_you_go';
  if (apiKey) return { key: 'api_key', label: 'API Key', detail: '按密钥认证', color: 'violet' };
  if (account?.credential_mode === 'agent_identity') {
    return { key: 'account', label: '登录账号', detail: 'Agent Identity', color: 'cyan' };
  }
  const detail = method === 'access_token' ? '访问令牌' : method === 'cookie' ? '会话登录' : 'OAuth / 账号授权';
  return { key: 'account', label: '登录账号', detail, color: 'blue' };
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

const QUOTA_REASON_LABELS = {
  ok: '额度已同步',
  never_polled: '等待首次同步',
  stale: '额度数据已过期',
  partial: '额度数据不完整',
  inactive: '账号当前未启用',
  token_missing: '缺少可用凭据',
  token_expired: '凭据已过期',
  unsupported_api_key_billing: '按量计费不提供订阅额度',
  unsupported_cursor_api_key_billing: 'Cursor User API Key 不提供个人额度接口',
  unsupported_claude_non_oauth: '当前认证方式不提供订阅额度',
  unsupported_provider: '当前提供商不提供额度同步',
};

function quotaReasonLabel(reason) {
  const key = String(reason || 'never_polled');
  if (QUOTA_REASON_LABELS[key]) return QUOTA_REASON_LABELS[key];
  if (key.startsWith('error/')) return '额度同步失败';
  return '额度状态待确认';
}

export function quotaPresentation(account) {
  const primary = account?.quota_summary?.primary || {};
  const percent = quotaPercent(account);
  const reason = quotaReason(account);
  const usedPercent = percent == null ? null : Math.round(percent);
  const remainingPercent = usedPercent == null ? null : Math.max(0, 100 - usedPercent);
  const tone = usedPercent == null ? 'muted' : usedPercent >= 90 ? 'danger' : usedPercent >= 70 ? 'warning' : 'success';
  const remainingTokens = Number(primary.remaining_tokens ?? account?.remaining_tokens ?? account?.quota_remaining_tokens);
  const limitTokens = Number(primary.limit_tokens ?? account?.limit_tokens ?? account?.quota_limit_tokens);
  const remainingRequests = Number(primary.remaining_requests);
  const limitRequests = Number(primary.limit_requests);
  const details = [];
  if (Number.isFinite(remainingTokens) && remainingTokens >= 0) {
    details.push(`${fmtTokens(remainingTokens)} Token 剩余${Number.isFinite(limitTokens) && limitTokens > 0 ? ` / ${fmtTokens(limitTokens)}` : ''}`);
  } else if (Number.isFinite(remainingRequests) && remainingRequests >= 0) {
    details.push(`${fmtInt(remainingRequests)} 次请求剩余${Number.isFinite(limitRequests) && limitRequests > 0 ? ` / ${fmtInt(limitRequests)}` : ''}`);
  }
  const resetAt = Number(primary.reset_at);
  if (Number.isFinite(resetAt) && resetAt > 0) details.push(`${fmtRelative(resetAt)}重置`);
  // The USD estimate only ever carries a real upstream-reported balance
  // (payg_credits_balance); subscription windows (window_based) never produce
  // dollars and surface no numbers here — the truthful utilization is the
  // primary/secondary window percentages above, per the sub2api model.
  const estimate = account?.quota_summary?.estimate;
  // `estimated` and `method` are asserted together on purpose. Today the backend
  // only ever sets estimated=true alongside payg_credits_balance
  // (internal/api/quota_estimate.go), so this is not a behaviour change -- it is
  // the guard that keeps a future estimating path from rendering as dollars a
  // figure upstream never reported.
  const estUSD = estimate && estimate.estimated && estimate.method === 'payg_credits_balance'
    && Number.isFinite(Number(estimate.remaining_usd)) && Number(estimate.remaining_usd) >= 0
    ? {
        remaining: Number(estimate.remaining_usd),
        extra: Number(estimate.extra_usd) || 0,
        plan: String(estimate.plan || '').trim(),
        method: String(estimate.method || ''),
      }
    : null;
  return {
    percent: usedPercent,
    remainingPercent,
    tone,
    reason,
    reasonLabel: quotaReasonLabel(reason),
    windowLabel: quotaWindowLabel(primary),
    detail: details.join(' · '),
    estUSD,
  };
}

function quotaEstLabel(est) {
  if (!est) return null;
  const parts = [`≈ ${fmtUSD(est.remaining)} 剩余`];
  if (est.extra > 0) parts.push(`+${fmtUSD(est.extra)} 额外`);
  if (est.plan) parts.push(est.plan);
  return parts.join(' · ');
}

function QuotaWindowMeter({ account, detail, compact = false }) {
  const fallback = detail.kind === '5h' ? '5 小时窗口' : '7 天窗口';
  const summary = account?.quota_summary || {};
  const window = quotaWindowForKind(account, detail.kind)
    || (detail.kind === '5h' ? summary.primary : null)
    || {};
  const percent = detail.usedPercent == null ? null : Math.max(0, Math.min(100, Math.round(Number(detail.usedPercent))));
  const remaining = percent == null ? null : 100 - percent;
  const tone = percent == null ? 'muted' : percent >= 90 ? 'danger' : percent >= 70 ? 'warning' : 'success';
  const details = [];
  const remainingTokens = Number(window.remaining_tokens);
  const limitTokens = Number(window.limit_tokens);
  if (Number.isFinite(remainingTokens) && remainingTokens >= 0) {
    details.push(`${fmtTokens(remainingTokens)} Token 剩余${Number.isFinite(limitTokens) && limitTokens > 0 ? ` / ${fmtTokens(limitTokens)}` : ''}`);
  }
  const resetAt = Number(window.reset_at);
  if (Number.isFinite(resetAt) && resetAt > 0) details.push(`${fmtRelative(resetAt)}重置`);
  const label = quotaWindowLabel(window, fallback);
  if (percent == null) {
    return (
      <div className={`pool-account-quota pool-account-quota--unknown${compact ? ' pool-account-quota--compact' : ''}`}>
        <div className="pool-account-quota__head"><strong>{label}</strong></div>
        <span className="pool-account-quota__meta">窗口数据待同步</span>
      </div>
    );
  }
  const ariaLabel = `${label}，已用 ${percent}%，剩余 ${remaining}%${details.length ? `，${details.join(' · ')}` : ''}`;
  return (
    <div className={`pool-account-quota pool-account-quota--${tone}${compact ? ' pool-account-quota--compact' : ''}`} aria-label={ariaLabel}>
      <div className="pool-account-quota__head"><strong>{remaining}% <span>剩余</span></strong><span>{label}</span></div>
      <TinyMeter value={percent} label={ariaLabel} tone={tone} />
      <span className="pool-account-quota__meta">{details.join(' · ') || `${percent}% 已用`}</span>
    </div>
  );
}

function AccountQuota({ account, compact = false }) {
  const windows = quotaUsageDetails(account);
  if (windows.length) {
    const fallback = quotaPresentation(account);
    const estLabel = quotaEstLabel(fallback.estUSD);
    return (
      <div className="pool-account-quota-stack">
        {windows.map((detail) => <QuotaWindowMeter key={detail.kind} account={account} detail={detail} compact={compact} />)}
        {estLabel ? <span className="pool-account-quota__usd">{estLabel}</span> : null}
      </div>
    );
  }
  const quota = quotaPresentation(account);
  const estLabel = quotaEstLabel(quota.estUSD);
  if (quota.percent == null) {
    return (
      <div className={`pool-account-quota pool-account-quota--unknown${compact ? ' pool-account-quota--compact' : ''}`}>
        <div className="pool-account-quota__head">
          <strong>额度待同步</strong>
        </div>
        <span className="pool-account-quota__meta">{quota.reasonLabel}</span>
        {estLabel ? <span className="pool-account-quota__usd">{estLabel}</span> : null}
      </div>
    );
  }
  const ariaLabel = `${quota.windowLabel}，已用 ${quota.percent}%，剩余 ${quota.remainingPercent}%${quota.detail ? `，${quota.detail}` : ''}${estLabel ? `，${estLabel}` : ''}`;
  return (
    <div className={`pool-account-quota pool-account-quota--${quota.tone}${compact ? ' pool-account-quota--compact' : ''}`} aria-label={ariaLabel}>
      <div className="pool-account-quota__head">
        <strong>{quota.remainingPercent}% <span>剩余</span></strong>
        <span>{quota.percent}% 已用</span>
      </div>
      <TinyMeter value={quota.percent} label={ariaLabel} tone={quota.tone} />
      <span className="pool-account-quota__meta">
        {quota.windowLabel}{quota.detail ? ` · ${quota.detail}` : ''}
      </span>
      {estLabel ? <span className="pool-account-quota__usd">{estLabel}</span> : null}
    </div>
  );
}

function routeSummary(account) {
  const binding = account.egress_binding || {};
  const exit = binding.primary_egress_id || account.egress_id || '默认出口';
  const primary = binding.sidecar_egress_id ? `${exit} · via ${binding.sidecar_egress_id}` : exit;
  const model = account.force_model || account.model || '';
  const effort = account.force_effort || account.effort || '';
  return { primary, model, effort };
}

function mergeAccountUpdate(account, patch) {
  if (!account || !patch || typeof patch !== 'object') return account;
  const isEgressBinding = Object.prototype.hasOwnProperty.call(patch, 'primary_egress_id')
    || Object.prototype.hasOwnProperty.call(patch, 'standby_egress_ids')
    || Object.prototype.hasOwnProperty.call(patch, 'sidecar_egress_id');
  if (isEgressBinding) {
    return { ...account, egress_binding: { ...(account.egress_binding || {}), ...patch } };
  }
  const normalized = patch.group && !patch.group_name ? { ...patch, group_name: patch.group } : patch;
  const merged = { ...account, ...normalized };
  if (Object.prototype.hasOwnProperty.call(normalized, 'egress_binding')) {
    merged.egress_binding = {
      ...(account.egress_binding || {}),
      ...(normalized.egress_binding || {}),
    };
  }
  return merged;
}

const clearedCooldownPatch = {
  egress_binding: {
    cooldown_until: 0,
    recheck_pending: false,
  },
};

export default function Accounts() {
  const responsive = useResponsiveLayout();
  const queryClient = useQueryClient();
  const [page, setPage] = useState(1);
  const [pageSize] = useState(50);
  const [importOpen, setImportOpen] = useState(false);
  const [search, setSearch] = useState('');
  const [searchInput, setSearchInput] = useState('');
  const [authType, setAuthType] = useState('all');
  const [groupFilter, setGroupFilter] = useState('all');
  const [exportFormat, setExportFormat] = useState('backup');
  const [exportIncludeProxies, setExportIncludeProxies] = useState(false);
  const [exportIncompatiblePolicy, setExportIncompatiblePolicy] = useState('fail_all');
  const [strictExportConfirmation, setStrictExportConfirmation] = useState(null);
  const [selected, setSelected] = useState([]);
  const [selectedAccountMeta, setSelectedAccountMeta] = useState({});
  const [selectMode, setSelectMode] = useState(false);
  const [drawerAcct, setDrawerAcct] = useState(null);
  const [moveOpen, setMoveOpen] = useState(false);
  const [moveGroup, setMoveGroup] = useState('');
  const [moveIDs, setMoveIDs] = useState([]);
  const [archiveImportOpen, setArchiveImportOpen] = useState(false);
  const [archiveFile, setArchiveFile] = useState(null);
  const [hubOpen, setHubOpen] = useState(false);
  const accountArchiveAbortRef = useRef(null);
  const importRefreshTimersRef = useRef([]);

  useEffect(() => () => {
    abortController(accountArchiveAbortRef.current);
    accountArchiveAbortRef.current = null;
    importRefreshTimersRef.current.forEach(clearBrowserTimeout);
    importRefreshTimersRef.current = [];
  }, []);

  const {
    data = EMPTY_ACCOUNT_DATA,
    loading,
    refreshing,
    error,
    lastRefresh,
    reload: load,
  } = useAccountsPage({ page, pageSize, search, authType, group: groupFilter === 'all' ? '' : groupFilter });
  const rows = data.rows || [];
  const total = data.total || 0;
  const groups = data.groups || [];
	const accountRateIDs = rows.map((account) => account.id).join(',');
	useEffect(() => {
		seedAccountRates(rows);
	}, [rows]);
	useEffect(() => subscribeAccountRateFeed(accountRateIDs ? accountRateIDs.split(',') : []), [accountRateIDs]);
	useEffect(() => subscribeAccountRateBatches((batch) => {
		queryClient.setQueryData(['pool', 'account-rates', 'live'], (current = {}) => ({ ...current, ...batch }));
	}), [queryClient]);
  const loadError = error || data.error;
  const accountByID = new Map(rows.map((account) => [account.id, account]));
  const selectedAccounts = selected.map((id) => accountByID.get(id) || selectedAccountMeta[id] || { id });
  const bulkHealthIncludesPaidProbe = selectedHasPaidProbe(selectedAccounts, selected);
  const handleSelectionChange = useCallback((keys) => {
    setSelected(keys);
    setSelectedAccountMeta((previous) => {
      const visible = new Map(rows.map((account) => [account.id, account]));
      return Object.fromEntries(keys.map((id) => [id, visible.get(id) || previous[id] || { id }]));
    });
  }, [rows]);

  const doSearch = () => { setPage(1); setSearch(searchInput.trim()); };
  const onPageChange = (cur) => { setPage(cur); };

  const refreshCurrentAccounts = useCallback(async () => {
    const params = { page, pageSize, search, authType, group: groupFilter === 'all' ? '' : groupFilter };
    const next = await fetchAccountsPage(params);
    queryClient.setQueryData(accountQueryKeys.list(params), (current) => ({
      ...(current || { groups: [], error: null }),
      ...next,
    }));
    return next;
  }, [authType, groupFilter, page, pageSize, queryClient, search]);

  const handleAccountImported = useCallback((result) => {
    const candidate = result && typeof result === 'object' && result.id ? result : null;
    if (candidate && page === 1 && !search) {
      const credential = accountCredentialPresentation(candidate);
      const groupMatches = groupFilter === 'all' || String(candidate.group_name || '') === groupFilter;
      if (groupMatches && (authType === 'all' || authType === credential.key)) {
        const params = { page, pageSize, search, authType, group: groupFilter === 'all' ? '' : groupFilter };
        queryClient.setQueryData(accountQueryKeys.list(params), (current) => {
          if (!current || !Array.isArray(current.rows)) return current;
          const exists = current.rows.some((row) => row.id === candidate.id);
          return {
            ...current,
            rows: exists
              ? current.rows.map((row) => row.id === candidate.id ? mergeAccountUpdate(row, candidate) : row)
              : [candidate, ...current.rows].slice(0, pageSize),
            total: exists ? current.total : Number(current.total || 0) + 1,
          };
        });
      }
    }

    void refreshCurrentAccounts().catch(() => {});
    if (!result?.validation_pending) return;

    // Validation polling is deliberately bounded and coalesced: at most three
    // lightweight account-list reads per add operation, even when the add button
    // is used repeatedly. No upstream probe is triggered by these reads.
    importRefreshTimersRef.current.forEach(clearBrowserTimeout);
    importRefreshTimersRef.current = [2500, 10_000, 30_000].map((delay) => setBrowserTimeout(() => {
      void refreshCurrentAccounts().catch(() => {});
    }, delay));
  }, [authType, groupFilter, page, pageSize, queryClient, refreshCurrentAccounts, search]);

  const updateCachedAccount = useCallback((id, patch) => {
    queryClient.setQueriesData({ queryKey: accountQueryKeys.all }, (current) => {
      if (!current || !Array.isArray(current.rows)) return current;
      return {
        ...current,
        rows: current.rows.map((account) => account.id === id ? mergeAccountUpdate(account, patch) : account),
      };
    });
    setDrawerAcct((current) => current?.id === id ? mergeAccountUpdate(current, patch) : current);
  }, [queryClient]);

  const removeCachedAccount = useCallback((id) => {
    queryClient.setQueriesData({ queryKey: accountQueryKeys.all }, (current) => {
      if (!current || !Array.isArray(current.rows)) return current;
      const rows = current.rows.filter((account) => account.id !== id);
      return rows.length === current.rows.length ? current : {
        ...current,
        rows,
        total: Math.max(0, Number(current.total || current.rows.length) - 1),
      };
    });
    setDrawerAcct((current) => current?.id === id ? null : current);
  }, [queryClient]);

  const handleAccountUpdated = useCallback((id, patch = {}) => {
    updateCachedAccount(id, patch);
    void Promise.resolve(load()).then((nextData) => {
      const fresh = nextData?.rows?.find((row) => row.id === id);
      if (fresh) updateCachedAccount(id, fresh);
    }).catch(() => {});
  }, [load, updateCachedAccount]);

  const {
    run: runAccountAction,
    running: accountActionRunning,
    activeKeys: activeAccountActionKeys,
    isRunning: isAccountActionKeyRunning,
  } = useKeyedAsyncAction(async (_key, id, act, requestBody = {}) => {
    const before = accountByID.get(id) || selectedAccountMeta[id] || (drawerAcct?.id === id ? drawerAcct : null);
    const optimisticPatch = act === 'clear-quarantine'
      ? { status: 'active', quarantine_until: 0, quarantine_reason: '' }
      : act === 'clear-cooldown'
        ? clearedCooldownPatch
        : act === 'force-isolation'
          ? { quarantine_until: MANUAL_FORCE_ISOLATION_UNTIL, quarantine_reason: 'manual_force_isolation', ignore_rate_limit_controls: false }
        : null;
    if (optimisticPatch) updateCachedAccount(id, optimisticPatch);
    try {
      const result = await post(`/admin/accounts/${encodeURIComponent(id)}/${act}`, requestBody);
      if (act === 'health-test') {
        const presentation = healthResultPresentation(result);
        Toast[presentation.tone](presentation.message);
      } else {
        Toast.success(`${ACCOUNT_ACTION_LABEL[act] || '操作'}已完成`);
      }
      if (act === 'delete') removeCachedAccount(id);
      if (act === 'clear-quarantine') updateCachedAccount(id, {
        status: 'active',
        quarantine_until: 0,
        quarantine_reason: '',
      });
      if (act === 'clear-cooldown') updateCachedAccount(id, clearedCooldownPatch);
      if (act === 'force-isolation') updateCachedAccount(id, {
        quarantine_until: MANUAL_FORCE_ISOLATION_UNTIL,
        quarantine_reason: 'manual_force_isolation',
        ignore_rate_limit_controls: false,
      });
      void Promise.resolve(load()).then((nextData) => {
        if (act === 'delete') return;
        const fresh = nextData?.rows?.find((row) => row.id === id);
        if (fresh) updateCachedAccount(id, fresh);
      }).catch(() => {});
      return true;
    } catch (e) {
      if (optimisticPatch && before) updateCachedAccount(id, before);
      showErrorToast(e);
      return false;
    }
  });
  const action = (id, act, requestBody = {}) => runAccountAction(accountActionKey(id, act), id, act, requestBody);

  const { run: bulkAction, running: bulkActionRunning } = useAsyncAction(async (act, label, costConfirmed = false) => {
    if (!selected.length) return;
    const healthEntries = [];
    const result = await batchOp(label, selected, async (id) => {
      const account = accountByID.get(id) || selectedAccountMeta[id] || { id };
      const response = await post(
        `/admin/accounts/${encodeURIComponent(id)}/${act}`,
        act === 'health-test' ? healthTestRequestBody(account, costConfirmed) : {},
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
    if (act === 'delete') result.success.forEach(removeCachedAccount);
    if (act === 'clear-quarantine') result.success.forEach((id) => updateCachedAccount(id, {
      status: 'active',
      quarantine_until: 0,
      quarantine_reason: '',
    }));
    if (act === 'clear-cooldown') result.success.forEach((id) => updateCachedAccount(id, clearedCooldownPatch));
    setSelected(failedIDs);
    void load();
  });

  const { run: runBulkMove, pending: bulkMoveRunning } = useInstantMutation({
    mutationFn: ({ ids, group }) => post('/admin/accounts/assign-group', { ids, group }),
    optimistic: ({ ids, group }) => {
      const moved = new Set(ids);
      const queries = queryClient.getQueriesData({ queryKey: accountQueryKeys.all });
      const drawer = drawerAcct;
      queryClient.setQueriesData({ queryKey: accountQueryKeys.all }, (current) => current ? {
        ...current,
        rows: (current.rows || []).map((account) => moved.has(account.id) ? { ...account, group_name: group } : account),
      } : current);
      setDrawerAcct((current) => current && moved.has(current.id) ? { ...current, group_name: group } : current);
      return { queries, drawer };
    },
    rollback: (snapshot) => {
      snapshot.queries.forEach(([key, value]) => queryClient.setQueryData(key, value));
      setDrawerAcct(snapshot.drawer);
    },
    onSuccess: (_result, { ids, group }) => {
      Toast.success(`已移动 ${ids.length} 个账号到「${group || '默认'}」`);
      setMoveOpen(false);
      setMoveIDs([]);
      setSelected([]);
      void load();
    },
  });
  const bulkMove = () => {
    const ids = moveIDs.length ? moveIDs : selected;
    if (!ids.length) return Promise.resolve();
    return runBulkMove({ ids, group: moveGroup }).catch((error) => {
      showErrorToast(error);
    });
  };

  const { run: prepareAccountExport, running: accountExportPreparing } = useAsyncAction(async (ids = []) => {
    abortController(accountArchiveAbortRef.current);
    const controller = createAbortController();
    accountArchiveAbortRef.current = controller;
    try {
      if (exportFormat !== 'backup') {
        if (!ids.length) {
          Toast.warning('严格格式导出必须明确选择账号，不能隐式导出整个账号池');
          return false;
        }
        const request = {
          account_ids: ids,
          format: exportFormat,
          include_proxies: exportFormat === 'sub2api-v1' && exportIncludeProxies,
          incompatible_policy: exportIncompatiblePolicy,
        };
        const preview = await preflightAccountExport(request, abortSignal(controller));
        if (controller?.signal.aborted) return false;
        setStrictExportConfirmation({ request, preview });
        return true;
      }
      const archive = await fetchAccountArchive(ids, exportFormat, abortSignal(controller));
      if (controller?.signal.aborted) return false;
      if (!downloadBlob(archive.filename, archive.blob)) {
        Toast.error('浏览器未能开始下载，请检查下载权限');
        return false;
      }
      if (archive.skipped) {
        Toast.warning(`已导出 ${archive.exported || '可兼容'} 个账号，跳过 ${archive.skipped} 个不兼容账号`);
      } else {
        Toast.success(ids.length ? `已导出 ${ids.length} 个账号` : '已导出全部账号');
      }
      return true;
    } catch (e) {
      if (controller?.signal.aborted) return false;
      showErrorToast(e);
      return false;
    } finally {
      if (accountArchiveAbortRef.current === controller) accountArchiveAbortRef.current = null;
    }
  });

  const { run: confirmStrictAccountExport, running: accountExportConfirming } = useAsyncAction(async () => {
    const pending = strictExportConfirmation;
    if (!pending) return false;
    if (pending.preview.compatible === 0) {
      Toast.warning('没有可导出的兼容账号');
      return false;
    }
    if (pending.request.incompatible_policy === 'fail_all' && pending.preview.incompatible > 0) {
      Toast.warning('当前策略会因不兼容账号停止，请调整策略或选择');
      return false;
    }
    abortController(accountArchiveAbortRef.current);
    const controller = createAbortController();
    accountArchiveAbortRef.current = controller;
    try {
      const archive = await downloadConfirmedAccountExport(
        pending.request,
        pending.preview.confirmation_nonce,
        abortSignal(controller),
      );
      if (controller?.signal.aborted) return false;
      if (!downloadBlob(archive.filename, archive.blob)) {
        Toast.error('浏览器未能开始下载，请检查下载权限');
        return false;
      }
      setStrictExportConfirmation(null);
      if (archive.skipped) {
        Toast.warning(`已安全导出 ${archive.exported || pending.preview.compatible} 个账号，并跳过 ${archive.skipped} 个不兼容账号`);
      } else {
        Toast.success(`已安全导出 ${archive.exported || pending.preview.compatible} 个账号`);
      }
      return true;
    } catch (error) {
      if (controller?.signal.aborted) return false;
      showErrorToast(error);
      return false;
    } finally {
      if (accountArchiveAbortRef.current === controller) accountArchiveAbortRef.current = null;
    }
  });

  const accountExportRunning = accountExportPreparing || accountExportConfirming;
  const exportAccountBackup = prepareAccountExport;

  const { run: restoreAccountBackup, running: accountImportRunning } = useAsyncAction(async () => {
    if (!archiveFile) {
      Toast.warning('请先选择 JSON 或 ZIP 账号备份');
      return false;
    }
    abortController(accountArchiveAbortRef.current);
    const controller = createAbortController();
    accountArchiveAbortRef.current = controller;
    try {
      const result = await importAccountArchive(archiveFile, abortSignal(controller));
      if (controller?.signal.aborted) return false;
      setArchiveImportOpen(false);
      setArchiveFile(null);
      setSelected([]);
      setSelectedAccountMeta({});
      setPage(1);
      await load();
      if (controller?.signal.aborted) return false;
      Toast.success(`完整导入 ${result.recognized} 个账号（新增 ${result.imported}，覆盖 ${result.replaced}）`);
      return true;
    } catch (e) {
      if (controller?.signal.aborted) return false;
      showErrorToast(e);
      return false;
    } finally {
      if (accountArchiveAbortRef.current === controller) accountArchiveAbortRef.current = null;
    }
  });

  const chooseArchiveFile = (event) => {
    const file = event.target.files?.[0] || null;
    if (!file) {
      setArchiveFile(null);
      return;
    }
    const lowerName = String(file.name || '').toLowerCase();
    if (!lowerName.endsWith('.json') && !lowerName.endsWith('.zip')) {
      Toast.error('仅支持 .json 或 .zip 账号备份');
      event.target.value = '';
      setArchiveFile(null);
      return;
    }
    if (file.size > 64 * 1024 * 1024) {
      Toast.error('账号备份不能超过 64 MiB');
      event.target.value = '';
      setArchiveFile(null);
      return;
    }
    setArchiveFile(file);
  };

  const anyAccountOperationRunning = accountActionRunning || bulkActionRunning || bulkMoveRunning || accountExportRunning || accountImportRunning;
  const isAccountActionLoading = (id, act) => isAccountActionKeyRunning(accountActionKey(id, act));
  const isAccountRowRunning = (id) => [...activeAccountActionKeys].some((key) => String(key).startsWith(`${id}:`));

  const renderAccountActions = (r) => {
    const rowRunning = isAccountRowRunning(r.id);
    const batchRunning = bulkActionRunning || bulkMoveRunning;
    return <ActionMenu
      label="账号操作"
      items={[
        {
          label: isAccountActionLoading(r.id, 'health-test') ? '测活中' : '测活',
          disabled: batchRunning || (rowRunning && !isAccountActionLoading(r.id, 'health-test')),
          confirm: requiresPaidHealthTest(r) ? {
            title: '确认执行双层测活？',
            description: '将先免费检查认证；认证正常后，会绑定此账号和出口发送 1 次最小推理请求，并可能产生少量上游费用。',
            confirmText: '确认并测活',
          } : undefined,
          onSelect: () => action(r.id, 'health-test', healthTestRequestBody(r, requiresPaidHealthTest(r))),
        },
        {
          label: isAccountActionLoading(r.id, 'clear-quarantine') ? '解除中' : '解除隔离',
          disabled: isProtectedProbeQuarantine(r) || batchRunning || (rowRunning && !isAccountActionLoading(r.id, 'clear-quarantine')),
          onSelect: () => action(r.id, 'clear-quarantine'),
        },
        {
          label: isAccountActionLoading(r.id, 'clear-cooldown') ? '解除中' : '解除冷却',
          disabled: batchRunning || (rowRunning && !isAccountActionLoading(r.id, 'clear-cooldown')),
          onSelect: () => action(r.id, 'clear-cooldown'),
        },
        {
          label: '移动分组',
          disabled: batchRunning || rowRunning,
          onSelect: () => {
            setMoveIDs([r.id]);
            setMoveGroup(r.group_name || '');
            setMoveOpen(true);
          },
        },
        {
          label: accountExportRunning ? '导出中' : '导出账号',
          disabled: batchRunning || rowRunning || accountImportRunning,
          onSelect: () => exportAccountBackup([r.id]),
        },
        { label: '详情', disabled: batchRunning || rowRunning, onSelect: () => setDrawerAcct(r) },
        {
          label: isAccountActionLoading(r.id, 'delete') ? '删除中' : '删除',
          destructive: true,
          disabled: batchRunning || (rowRunning && !isAccountActionLoading(r.id, 'delete')),
          confirm: {
            title: '确认删除该账号？',
            description: `账号 ${r.label || r.email || r.id} 删除后不可恢复。`,
            confirmText: '删除',
          },
          onSelect: () => action(r.id, 'delete'),
        },
      ]}
    />
  };

  const filtered = rows; // filtering is now server-side

  const exportCSV = () => {
    const cols = [
      { title: 'id', get: (r) => r.id }, { title: 'label', get: (r) => r.label }, { title: 'email', get: (r) => r.email },
      { title: 'provider', get: (r) => r.provider }, { title: 'group', get: (r) => r.group_name },
      { title: 'plan', get: (r) => formatPlanLabel(r.plan_type, r.plan_presentation) }, { title: 'auth_method', get: (r) => `${r.auth_method || ''} ${r.credential_mode || ''}` },
      { title: 'billing_mode', get: (r) => r.billing_mode }, { title: 'status', get: (r) => r.status },
		{ title: 'logical_rpm_60s', get: (r) => r.request_rate?.state === 'unavailable' ? '' : r.request_rate?.logical_rpm },
		{ title: 'attempt_rpm_60s', get: (r) => r.request_rate?.state === 'unavailable' ? '' : r.request_rate?.attempt_rpm },
		{ title: 'root_rpm_60s', get: (r) => r.request_rate?.state === 'unavailable' ? '' : r.request_rate?.root_rpm },
		{ title: 'subagent_rpm_60s', get: (r) => r.request_rate?.state === 'unavailable' ? '' : r.request_rate?.subagent_rpm },
		{ title: 'unknown_rpm_60s', get: (r) => r.request_rate?.state === 'unavailable' ? '' : r.request_rate?.unknown_rpm },
		{ title: 'tpm_60s', get: (r) => r.request_rate?.state === 'unavailable' ? '' : r.request_rate?.tpm },
		{ title: 'input_tpm_60s', get: (r) => r.request_rate?.state === 'unavailable' ? '' : r.request_rate?.input_tpm },
		{ title: 'cached_input_tpm_60s', get: (r) => r.request_rate?.state === 'unavailable' ? '' : r.request_rate?.cached_input_tpm },
		{ title: 'output_tpm_60s', get: (r) => r.request_rate?.state === 'unavailable' ? '' : r.request_rate?.output_tpm },
    ];
    const ok = downloadCSV('accounts.csv', toCSV(filtered, cols));
    if (!ok) Toast.error('导出失败，请检查浏览器下载权限');
  };

  const columns = [
    {
      title: '账号',
      dataIndex: 'label',
      width: 300,
      sorter: (a, b) => String(a.label || a.id).localeCompare(String(b.label || b.id)),
      render: (_, r) => (
        <div className="pool-account-summary">
          <div className="pool-account-titleline">
            <TextClamp
              strong
              className="pool-account-primary-id"
              title={r.label || r.id}
              ariaLabel={r.label || r.id}
              onClick={() => setDrawerAcct(r)}
            >
              {middleEllipsis(r.label || r.id, 30, 14)}
            </TextClamp>
            <Tag size="small">{r.provider || 'codex'}</Tag>
          </div>
          <div className="pool-account-metaline">
            <TextClamp muted className="pool-account-email" title={r.email || r.id} ariaLabel={r.email || r.id}>{middleEllipsis(r.email || r.id)}</TextClamp>
          </div>
        </div>
      ),
    },
    {
      title: '账号类型',
      key: 'credential_type',
      width: 142,
      render: (_, r) => {
        const credential = accountCredentialPresentation(r);
        return (
          <div className="pool-resource-summary">
            <div><Tag size="small" color={credential.color}>{credential.label}</Tag></div>
            <div className="pool-resource-summary__meta">{credential.detail}</div>
          </div>
        );
      },
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
            <div className="pool-resource-summary__meta">{r.billing_mode === 'pay_as_you_go' ? '按量计费' : formatPlanLabel(r.plan_type, r.plan_presentation)}</div>
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
		title: <span title="账号滚动 60 秒逻辑请求 RPM、Token TPM，并单列重试/故障转移产生的上游尝试">实时负载</span>,
		key: 'request_rate',
		width: 136,
		align: 'right',
		render: (_, r) => <AccountRateMetric account={r} />,
	},
	{
      title: '用量 / 额度',
      key: 'usage',
      width: 220,
      align: 'right',
      render: (_, r) => {
        const usage = accountUsage(r);
        const requests = usage.requests ?? r.requests;
        const tokens = usage.total_tokens ?? r.total_tokens;
        return (
          <div className="pool-account-usage">
            <div className="pool-account-usage__totals">
              <strong>{tokens == null ? 'Token 未同步' : `${fmtTokens(tokens)} Token`}</strong>
              <span>{requests == null ? '请求未同步' : `${fmtInt(requests)} 次请求`}</span>
            </div>
            <AccountQuota account={r} />
          </div>
        );
      },
    },
    { title: '操作', key: 'ops', width: 72, render: (_, r) => renderAccountActions(r) },
  ];
  // ResourceTable keeps this compact fallback column alongside the richer mobile card renderer.
  // That preserves a table-shaped accessible fallback if the renderer is ever intentionally
  // removed, rather than making the mobile contract depend on a hidden desktop column set.
  const mobileColumns = [columns[0]];
  const mobileAccountCell = (r, mobileMeta = {}) => {
    const route = routeSummary(r);
    const usage = accountUsage(r);
    const requests = usage.requests ?? r.requests;
    const tokens = usage.total_tokens ?? r.total_tokens;
    const accountName = r.label || r.id;
    const credential = accountCredentialPresentation(r);
    const accountTitle = (
      <TextClamp strong title={accountName} ariaLabel={accountName}>
        {middleEllipsis(accountName, 20, 8)}
      </TextClamp>
    );
    return (
      <MobileResourceCell
        selectable={selectMode}
        selected={Boolean(mobileMeta.selected)}
        selectLabel={`选择 ${accountName}`}
        onSelect={() => mobileMeta.toggleSelected?.(!mobileMeta.selected)}
        title={selectMode
          ? accountTitle
          : <Typography.Text link onClick={() => setDrawerAcct(r)}>{accountTitle}</Typography.Text>}
        subtitle={<TextClamp title={r.email || r.id} ariaLabel={r.email || r.id}>{middleEllipsis(r.email || r.id, 22, 12)}</TextClamp>}
        badges={statusTag(r)}
        chips={<>
          <Tag size="small">{r.provider || 'codex'}</Tag>
          <Tag size="small" color={credential.color}>{credential.label}</Tag>
          <Tag size="small" title={r.group_name || '默认'}>{middleEllipsis(r.group_name || '默认', 12, 6)}</Tag>
          {r.plan_type || r.plan_presentation ? <Tag size="small">{formatPlanLabel(r.plan_type, r.plan_presentation)}</Tag> : null}
          {r.billing_mode === 'pay_as_you_go' ? <Tag size="small" color="violet">按量计费</Tag> : null}
          <Tag size="small" color="blue" title={route.primary}>{middleEllipsis(route.primary, 14, 8)}</Tag>
			<AccountRateMetric account={r} compact />
        </>}
        details={[
          {
            label: '累计用量',
            value: `${tokens == null ? 'Token 未同步' : `${fmtTokens(tokens)} Token`}${requests == null ? '' : ` · ${fmtInt(requests)} 次请求`}`,
          },
          { label: '额度', value: <AccountQuota account={r} compact /> },
			{
				label: '实时负载',
				value: <AccountRateDetail account={r} />,
			},
        ]}
        actions={!selectMode ? renderAccountActions(r) : null}
      />
    );
  };
  return (
    <div>
      <PageHeader
        title="账号池"
        subtitle={!lastRefresh && loading ? '正在读取账号…' : !lastRefresh && loadError ? '账号数据暂时不可用' : `共 ${total} 个账号`}
        actions={<>
          <Input prefix={<IconSearch />} value={searchInput} onChange={setSearchInput}
            onEnterPress={doSearch} style={{ width: responsive.isMobile ? 210 : 220 }} placeholder="搜索 标签/邮箱/分组" showClear onClear={doSearch} />
          <Select
            value={authType}
            onChange={(value) => { setAuthType(value); setPage(1); }}
            style={{ width: 132 }}
            optionList={[
              { label: '全部类型', value: 'all' },
              { label: 'API Key', value: 'api_key' },
              { label: '登录账号', value: 'account' },
            ]}
          />
          <Select
            value={groupFilter}
            onChange={(value) => { setGroupFilter(value); setPage(1); }}
            style={{ width: 148 }}
            optionList={[
              { label: '全部分组', value: 'all' },
              ...groups.map((group) => ({ label: group.name, value: group.name })),
            ]}
          />
          <Button icon={<IconSearch />} onClick={doSearch}>搜索</Button>
          <Button icon={<IconDownload />} onClick={exportCSV}>导出 CSV</Button>
          <Select
            value={exportFormat}
            onChange={setExportFormat}
            style={{ width: 168 }}
            optionList={[
              { label: '完整账号备份', value: 'backup' },
              { label: 'Sub2API data v1', value: 'sub2api-v1' },
              { label: 'CLIProxyAPI', value: 'cliproxyapi' },
              { label: 'Codex auth.json', value: 'codex-auth' },
            ]}
          />
          {exportFormat !== 'backup' ? (
            <Select
              value={exportIncompatiblePolicy}
              onChange={setExportIncompatiblePolicy}
              style={{ width: 176 }}
              optionList={[
                { label: '不兼容则全部停止', value: 'fail_all' },
                { label: '跳过并生成报告', value: 'skip_with_report' },
              ]}
            />
          ) : null}
          {exportFormat === 'sub2api-v1' ? (
            <label className="pool-inline-check" title="只导出可由 Sub2API data v1 表达的账号出口；预检会明确提示遗漏">
              <input type="checkbox" checked={exportIncludeProxies} onChange={(event) => setExportIncludeProxies(event.target.checked)} />
              包含可兼容代理
            </label>
          ) : null}
          <Button
            icon={<IconDownload />}
            loading={accountExportRunning}
            disabled={accountImportRunning || exportFormat !== 'backup'}
            title={exportFormat === 'backup' ? '导出完整账号池备份' : '严格格式需要明确选择账号'}
            onClick={() => exportAccountBackup([])}
          >一键导出全部</Button>
          <Button icon={<IconDownload />} loading={accountExportRunning} disabled={!selected.length || accountImportRunning} onClick={() => exportAccountBackup([...selected])}>一键导出所选{selected.length ? `(${selected.length})` : ''}</Button>
          <Button icon={<IconFile />} disabled={accountExportRunning || accountImportRunning} onClick={() => setArchiveImportOpen(true)}>一键导入账号池</Button>
          <Button icon={<IconRefresh />} loading={refreshing} onClick={() => load()}>刷新</Button>
          {responsive.isMobile ? (
            <Button onClick={() => { setSelectMode((value) => !value); if (selectMode) setSelected([]); }}>
              {selectMode ? '完成' : '选择'}
            </Button>
          ) : null}
          {!responsive.isMobile ? <Button icon={<IconPlus />} theme="solid" onClick={() => setImportOpen(true)}>添加账号</Button> : null}
          <Button className="pool-hub-entry-button" onClick={() => setHubOpen(true)}>号池链接</Button>
        </>} />

      {selected.length > 0 && (
        <div className="pool-bulkbar">
          <span>已选 <b>{selected.length}</b> 项</span>
          {bulkHealthIncludesPaidProbe ? (
            <ConfirmDialog
              title="确认批量执行付费双层测活？"
              description="所选账号中包含 Kiro 或上游 API Key。每个认证正常的此类账号会发送 1 次最小推理请求，并可能产生少量上游费用；其他账号保持免费测活。"
              confirmText="确认并批量测活"
              onConfirm={() => bulkAction('health-test', '测活', true)}
            >
              <Button size="small" loading={bulkActionRunning} disabled={accountActionRunning || bulkMoveRunning}>批量测活</Button>
            </ConfirmDialog>
          ) : (
            <Button size="small" loading={bulkActionRunning} disabled={accountActionRunning || bulkMoveRunning} onClick={() => bulkAction('health-test', '测活')}>批量测活</Button>
          )}
          <Button size="small" loading={bulkActionRunning} disabled={accountActionRunning || bulkMoveRunning} onClick={() => bulkAction('clear-quarantine', '解除隔离')}>批量解除隔离</Button>
          <Button size="small" loading={bulkActionRunning} disabled={accountActionRunning || bulkMoveRunning} onClick={() => bulkAction('clear-cooldown', '解除冷却')}>批量解除冷却</Button>
          <Button size="small" disabled={anyAccountOperationRunning} onClick={() => { setMoveIDs([...selected]); setMoveGroup(''); setMoveOpen(true); }}>移动分组</Button>
          <Button size="small" icon={<IconDownload />} loading={accountExportRunning} disabled={accountActionRunning || bulkActionRunning || bulkMoveRunning || accountImportRunning} onClick={() => exportAccountBackup([...selected])}>一键导出所选</Button>
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
		minScrollX={1410}
        rowHeight={72}
        mobileColumns={mobileColumns}
        mobileRenderer={mobileAccountCell}
        mobileListLabel="账号列表"
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
        statusTag={statusTag} onAction={action} actionRunning={drawerAcct ? isAccountRowRunning(drawerAcct.id) : false}
        actionDisabled={bulkActionRunning || bulkMoveRunning} isActionLoading={isAccountActionLoading}
        onUpdated={handleAccountUpdated} onClose={() => setDrawerAcct(null)} />
      <Modal
        title="Sub2API 号池链接"
        visible={hubOpen}
        onCancel={() => setHubOpen(false)}
        footer={null}
        width="min(1120px, calc(100vw - 32px))"
        className="pool-hub-modal"
      >
        <Sub2APIHubPanel />
      </Modal>
      <Modal title={moveIDs.length === 1 ? '移动账号到分组' : '批量移动到分组'} visible={moveOpen} onCancel={() => { if (!bulkMoveRunning) { setMoveOpen(false); setMoveIDs([]); } }} onOk={bulkMove} confirmLoading={bulkMoveRunning} okText="移动">
        <Select value={moveGroup} onChange={setMoveGroup} style={{ width: '100%' }} placeholder="选择目标分组"
          optionList={groups.map((g) => ({ label: g.name, value: g.name }))} />
      </Modal>
      <Modal
        title="一键完整导入账号池"
        visible={archiveImportOpen}
        onCancel={() => {
          if (!accountImportRunning) {
            setArchiveImportOpen(false);
            setArchiveFile(null);
          }
        }}
        onOk={restoreAccountBackup}
        confirmLoading={accountImportRunning}
        okText="完整导入"
        maskClosable={!accountImportRunning}
      >
        <div className="pool-field">
          <label className="pool-field__label" htmlFor="account-archive-file">账号备份文件</label>
          <span>
            <input
              id="account-archive-file"
              className="pool-input"
              type="file"
              accept=".json,.zip,application/json,application/zip"
              disabled={accountImportRunning}
              onChange={chooseArchiveFile}
            />
            <div className="pool-field__help">
              支持本系统逐账号 JSON、批量 ZIP，以及历史 auth.json、账号数组、sub2api v1 和 Kiro JSON。匹配 ID 的账号会被完整覆盖，未包含的账号保持不变。
            </div>
            {archiveFile ? <div className="pool-resource-summary__meta">已选择：{archiveFile.name} · {Math.max(1, Math.ceil(archiveFile.size / 1024))} KiB</div> : null}
          </span>
        </div>
      </Modal>
      <ConfirmDialog
        open={Boolean(strictExportConfirmation)}
        title="确认导出敏感账号凭据？"
        description={<ExportPreflightSummary
          preview={strictExportConfirmation?.preview}
          policy={strictExportConfirmation?.request?.incompatible_policy}
        />}
        confirmText="确认并下载"
        busy={accountExportConfirming}
        onCancel={() => {
          if (!accountExportConfirming) setStrictExportConfirmation(null);
        }}
        onConfirm={confirmStrictAccountExport}
      />
      <OAuthLoginModal open={importOpen} onClose={() => setImportOpen(false)} onSuccess={handleAccountImported} />
    </div>
  );
}
