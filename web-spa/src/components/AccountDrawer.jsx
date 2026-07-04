import React, { useCallback, useEffect, useState } from 'react';
import { ConfirmDialog, Drawer, Tag, Button, Typography, Spin, Select, Toast } from './pool/index.jsx';
import { get, post } from '../api.js';
import LoadErrorBanner from './LoadErrorBanner.jsx';
import { Panel } from './PageHeader.jsx';
import { showErrorToast } from './ErrorToast.jsx';
import useAsyncAction from '../hooks/useAsyncAction.js';
import useAsyncResource from '../hooks/useAsyncResource.js';
import { fmtTokens, fmtInt, fmtDateTime, fmtRelative } from '../lib/format.js';

const Row = ({ k, v }) => (
  <div style={{ display: 'flex', justifyContent: 'space-between', gap: 16, padding: '5px 0', fontSize: 13 }}>
    <span className="pool-muted" style={{ flexShrink: 0 }}>{k}</span>
    <span style={{ fontWeight: 500, textAlign: 'right', wordBreak: 'break-all' }}>{v}</span>
  </div>
);

const EMPTY_ACCOUNT_DETAIL = { audit: [] };

function formatResetCredits(credits) {
  if (!credits || credits.status !== 'ok' || credits.available_count == null) return '未知';
  return `${credits.available_count} 次`;
}

// Account detail drawer: identity, egress binding, usage, recent audit + quick actions.
export default function AccountDrawer({
  account,
  usage,
  statusTag,
  onAction,
  actionRunning = false,
  actionDisabled = false,
  isActionLoading: isActionLoadingProp,
  onUpdated,
  onClose,
}) {
  const binding = account?.egress_binding || null;
  const [selectedEgress, setSelectedEgress] = useState('');
  const [selectedGroup, setSelectedGroup] = useState('');

  const fetchDetails = useCallback(async ({ signal }) => {
    if (!account) return EMPTY_ACCOUNT_DETAIL;
    const [data, profiles, groups] = await Promise.all([
      get('/admin/audit', { account_id: account.id, limit: 10 }, { signal }),
      get('/admin/egress-profiles', undefined, { signal }),
      get('/admin/groups', undefined, { signal }),
    ]);
    const rows = Array.isArray(data) ? data : data?.rows || [];
    return {
      audit: rows,
      profiles: Array.isArray(profiles) ? profiles : profiles?.profiles || profiles?.egress_profiles || [],
      groups: Array.isArray(groups) ? groups : groups?.groups || [],
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

  useEffect(() => {
    setSelectedEgress(binding?.primary_egress_id || '');
  }, [account?.id, binding?.primary_egress_id]);

  useEffect(() => {
    setSelectedGroup(account?.group_name || '');
  }, [account?.id, account?.group_name]);

  const { run: saveDefaultEgress, running: savingDefaultEgress } = useAsyncAction(async () => {
    if (!account || !selectedEgress) return;
    try {
      const saved = await post(`/admin/accounts/${encodeURIComponent(account.id)}/egress-binding`, {
        primary_egress_id: selectedEgress,
      });
      Toast.success('默认出口已保存');
      await onUpdated?.(account.id, saved);
    } catch (err) {
      showErrorToast(err);
    }
  });

  const { run: saveGroup, running: savingGroup } = useAsyncAction(async () => {
    if (!account || !selectedGroup) return;
    try {
      await post(`/admin/accounts/${encodeURIComponent(account.id)}/group`, { group: selectedGroup });
      Toast.success('分组已保存');
      await onUpdated?.(account.id, { group_name: selectedGroup });
    } catch (err) {
      showErrorToast(err);
    }
  });

  if (!account) return null;
  const u = usage;
  const audit = details.audit || [];
  const profiles = details.profiles || [];
  const groups = details.groups || [];
  const groupPolicy = groups.find((group) => group.name === (selectedGroup || account.group_name)) || null;
  const egressOptions = profiles.map((profile) => ({
    label: `${profile.name || profile.id} (${profile.type || 'direct'})`,
    value: profile.id,
  }));
  const isActionLoading = (act) => Boolean(isActionLoadingProp?.(account.id, act));
  const isActionDisabled = (act) => actionDisabled || (actionRunning && !isActionLoading(act));
  const resetCredits = account.quota_summary?.reset_credits;

  return (
    <Drawer title={account.label || account.id} visible={!!account} onCancel={onClose} width={520} className="pool-account-drawer">
      <LoadErrorBanner error={error} onRetry={reload} />
      <Panel title="身份" style={{ marginBottom: 14 }}>
        <Row k="账号 ID" v={<span className="pool-mono">{account.id}</span>} />
        <Row k="邮箱" v={account.email || '—'} />
        <Row k="提供商" v={<Tag>{account.provider || 'codex'}</Tag>} />
        <Row k="分组" v={account.group_name || '默认'} />
        <Row k="套餐" v={account.plan_type || '—'} />
        <Row k="状态" v={statusTag ? statusTag(account) : account.status} />
      </Panel>

      <Panel title="账号额度" style={{ marginBottom: 14 }}>
        <Row k="主动重置次数" v={formatResetCredits(resetCredits)} />
        <Row k="更新时间" v={resetCredits?.updated_at ? fmtRelative(resetCredits.updated_at) : '—'} />
      </Panel>

      <Panel title="分组策略" style={{ marginBottom: 14 }}>
        <div style={{ display: 'flex', gap: 8, alignItems: 'center', marginBottom: 8 }}>
          <Select
            value={selectedGroup}
            onChange={setSelectedGroup}
            optionList={groups.map((group) => ({ label: group.name, value: group.name }))}
            placeholder="选择分组"
            disabled={savingGroup || !groups.length}
            style={{ flex: 1, minWidth: 0 }}
          />
          <Button
            size="small"
            loading={savingGroup}
            disabled={!selectedGroup || selectedGroup === account.group_name}
            onClick={saveGroup}
          >保存</Button>
        </div>
        <Row k="继承模型" v={groupPolicy?.force_model || '默认模型'} />
        <Row k="继承努力级别" v={groupPolicy?.force_effort || '默认'} />
        <Row k="成员" v={`${groupPolicy?.active_account_count ?? 0} / ${groupPolicy?.account_count ?? 0} 活跃`} />
      </Panel>

      <Panel title="出口绑定" style={{ marginBottom: 14 }}>
        {!binding ? <Typography.Text type="tertiary">暂无出口绑定数据</Typography.Text> : (
          <>
            <Row k="默认出口" v={binding.primary_egress_id || '—'} />
            <div style={{ display: 'flex', gap: 8, alignItems: 'center', margin: '8px 0' }}>
              <Select
                value={selectedEgress}
                onChange={setSelectedEgress}
                optionList={egressOptions}
                placeholder="选择默认出口"
                disabled={savingDefaultEgress || !egressOptions.length}
                style={{ flex: 1, minWidth: 0 }}
              />
              <Button
                size="small"
                loading={savingDefaultEgress}
                disabled={!selectedEgress || selectedEgress === binding.primary_egress_id}
                onClick={saveDefaultEgress}
              >保存</Button>
            </div>
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
          <div key={i} style={{ fontSize: 12.5, padding: '5px 0', borderBottom: '1px solid var(--pool-border)' }}>
            <span className="pool-muted">{fmtDateTime(a.created_at)}</span> <Tag size="small">{a.action}</Tag> {a.state || ''}
          </div>
        ))}
      </Panel>

      <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}>
        <Button loading={isActionLoading('health-test')} disabled={isActionDisabled('health-test')} onClick={() => onAction(account.id, 'health-test')}>测活</Button>
        <Button loading={isActionLoading('clear-quarantine')} disabled={isActionDisabled('clear-quarantine')} onClick={() => onAction(account.id, 'clear-quarantine')}>解隔离</Button>
        <Button loading={isActionLoading('refresh')} disabled={isActionDisabled('refresh')} onClick={() => onAction(account.id, 'refresh')}>刷新</Button>
        <ConfirmDialog
          title="删除该账号？"
          description={`账号 ${account.label || account.email || account.id} 删除后不可恢复。`}
          destructive
          confirmText="删除"
          onConfirm={async () => { if (await onAction(account.id, 'delete')) onClose(); }}
        >
          <Button type="danger" loading={isActionLoading('delete')} disabled={isActionDisabled('delete')}>删除</Button>
        </ConfirmDialog>
      </div>
    </Drawer>
  );
}
