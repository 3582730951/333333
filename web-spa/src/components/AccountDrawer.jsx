import React, { useCallback } from 'react';
import { SideSheet, Tag, Button, Typography, Popconfirm, Spin } from '@douyinfe/semi-ui';
import { get } from '../api.js';
import LoadErrorBanner from './LoadErrorBanner.jsx';
import { Panel } from './PageHeader.jsx';
import useAsyncResource from '../hooks/useAsyncResource.js';
import { fmtTokens, fmtInt, fmtDateTime, fmtRelative } from '../lib/format.js';

const Row = ({ k, v }) => (
  <div style={{ display: 'flex', justifyContent: 'space-between', gap: 16, padding: '5px 0', fontSize: 13 }}>
    <span className="pool-muted" style={{ flexShrink: 0 }}>{k}</span>
    <span style={{ fontWeight: 500, textAlign: 'right', wordBreak: 'break-all' }}>{v}</span>
  </div>
);

const EMPTY_ACCOUNT_DETAIL = { audit: [] };

// Account detail drawer: identity, egress binding, usage, recent audit + quick actions.
export default function AccountDrawer({
  account,
  usage,
  statusTag,
  onAction,
  actionRunning = false,
  actionDisabled = false,
  isActionLoading: isActionLoadingProp,
  onClose,
}) {
  const fetchDetails = useCallback(async ({ signal }) => {
    if (!account) return EMPTY_ACCOUNT_DETAIL;
    const data = await get('/admin/audit', { account_id: account.id, limit: 10 }, { signal });
    const rows = Array.isArray(data) ? data : data?.rows || [];
    return {
      audit: rows,
    };
  }, [account]);

  const {
    data: details = EMPTY_ACCOUNT_DETAIL,
    loading,
    error,
    reload,
  } = useAsyncResource(fetchDetails, [fetchDetails], {
    initialData: EMPTY_ACCOUNT_DETAIL,
    auto: Boolean(account),
    resetDataOnReload: true,
  });

  if (!account) return null;
  const u = usage;
  const binding = account.egress_binding || null;
  const audit = details.audit || [];
  const isActionLoading = (act) => Boolean(isActionLoadingProp?.(account.id, act));
  const isActionDisabled = (act) => actionDisabled || (actionRunning && !isActionLoading(act));

  return (
    <SideSheet title={account.label || account.id} visible={!!account} onCancel={onClose} width={520} className="pool-account-drawer">
      <LoadErrorBanner error={error} onRetry={reload} />
      <Panel title="身份" style={{ marginBottom: 14 }}>
        <Row k="账号 ID" v={<span className="pool-mono">{account.id}</span>} />
        <Row k="邮箱" v={account.email || '—'} />
        <Row k="提供商" v={<Tag>{account.provider || 'codex'}</Tag>} />
        <Row k="分组" v={account.group_name || '默认'} />
        <Row k="套餐" v={account.plan_type || '—'} />
        <Row k="状态" v={statusTag ? statusTag(account) : account.status} />
      </Panel>

      <Panel title="出口绑定" style={{ marginBottom: 14 }}>
        {!binding ? <Typography.Text type="tertiary">暂无出口绑定数据</Typography.Text> : (
          <>
            <Row k="主出口" v={binding.primary_egress_id || '—'} />
            <Row k="备用出口" v={binding.standby_egress_ids || '—'} />
            <Row k="冷却至" v={binding.cooldown_until ? fmtRelative(binding.cooldown_until) : '—'} />
            <Row k="待复测" v={binding.recheck_pending ? <Tag color="amber" size="small">是</Tag> : '否'} />
          </>
        )}
      </Panel>

      {u ? (
        <Panel title="累计用量" style={{ marginBottom: 14 }}>
          <Row k="请求数" v={fmtInt(u.requests)} />
          <Row k="输入 / 输出" v={`${fmtTokens(u.prompt_tokens)} / ${fmtTokens(u.completion_tokens)}`} />
          <Row k="缓存" v={fmtTokens(u.cached_tokens)} />
          <Row k="总 Token" v={<b>{fmtTokens(u.total_tokens)}</b>} />
        </Panel>
      ) : null}

      <Panel title="近期审计" style={{ marginBottom: 14 }}>
        {loading ? <Spin /> : !audit.length ? <Typography.Text type="tertiary">暂无审计记录</Typography.Text> : audit.map((a, i) => (
          <div key={i} style={{ fontSize: 12.5, padding: '5px 0', borderBottom: '1px solid var(--semi-color-border)' }}>
            <span className="pool-muted">{fmtDateTime(a.created_at)}</span> <Tag size="small">{a.action}</Tag> {a.state || ''}
          </div>
        ))}
      </Panel>

      <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}>
        <Button loading={isActionLoading('health-test')} disabled={isActionDisabled('health-test')} onClick={() => onAction(account.id, 'health-test')}>测活</Button>
        <Button loading={isActionLoading('clear-quarantine')} disabled={isActionDisabled('clear-quarantine')} onClick={() => onAction(account.id, 'clear-quarantine')}>解隔离</Button>
        <Button loading={isActionLoading('refresh')} disabled={isActionDisabled('refresh')} onClick={() => onAction(account.id, 'refresh')}>刷新</Button>
        <Popconfirm title="删除该账号？" onConfirm={async () => { if (await onAction(account.id, 'delete')) onClose(); }}>
          <Button type="danger" loading={isActionLoading('delete')} disabled={isActionDisabled('delete')}>删除</Button>
        </Popconfirm>
      </div>
    </SideSheet>
  );
}
