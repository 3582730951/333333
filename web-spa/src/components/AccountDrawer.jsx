import React, { useCallback, useEffect, useState } from 'react';
import { ConfirmDialog, Drawer, Modal, Tag, Button, Typography, Spin, Select, Switch, Toast, InputNumber } from './pool/index.jsx';
import { get, post, put } from '../api.js';
import LoadErrorBanner from './LoadErrorBanner.jsx';
import { Panel } from './PageHeader.jsx';
import { showErrorToast } from './ErrorToast.jsx';
import useAsyncAction from '../hooks/useAsyncAction.js';
import useAsyncResource from '../hooks/useAsyncResource.js';
import { fmtTokens, fmtInt, fmtDateTime, fmtRelative } from '../lib/format.js';
import {
  healthTestRequestBody, isKiroAccount, isKiroSuspended, isProtectedProbeQuarantine,
  isProviderAPIKeyAccount, requiresPaidHealthTest,
} from '../features/accounts/model/healthTest.ts';

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
  const [selectedSidecar, setSelectedSidecar] = useState('');
  const [selectedGroup, setSelectedGroup] = useState('');
  const [ignoreRateLimitControls, setIgnoreRateLimitControls] = useState(false);
  const [routingWeight, setRoutingWeight] = useState(100);
  const [retryMaxAttempts, setRetryMaxAttempts] = useState(0);
  const [reauthForm, setReauthForm] = useState({ login_email: '', password: '', otp_url: '', target_workspace_id: '', auto_enabled: false });
  const [oauthModal, setOauthModal] = useState({ open: false, session_id: '', auth_url: '', redirected: '', target_workspace_id: '' });

  const fetchDetails = useCallback(async ({ signal }) => {
    if (!account) return EMPTY_ACCOUNT_DETAIL;
    const encodedID = encodeURIComponent(account.id);
    const supportsCodexReauth = (account.provider || 'codex') === 'codex' && account.auth_method !== 'api_key' && account.credential_mode !== 'agent_identity';
    const [data, profiles, groups, codexReauth] = await Promise.all([
      get('/admin/audit', { account_id: account.id, limit: 10 }, { signal }),
      get('/admin/egress-profiles', undefined, { signal }),
      get('/admin/groups', undefined, { signal }),
      supportsCodexReauth
        ? get(`/admin/accounts/${encodedID}/codex-reauth-status`, undefined, { signal }).catch((err) => ({ error: err?.message || String(err) }))
        : Promise.resolve(null),
    ]);
    const rows = Array.isArray(data) ? data : data?.rows || [];
    return {
      audit: rows,
      profiles: Array.isArray(profiles) ? profiles : profiles?.profiles || profiles?.egress_profiles || [],
      groups: Array.isArray(groups) ? groups : groups?.groups || [],
      codexReauth,
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
    setSelectedSidecar(binding?.sidecar_egress_id || '');
  }, [account?.id, binding?.primary_egress_id, binding?.sidecar_egress_id]);

  useEffect(() => {
    setSelectedGroup(account?.group_name || '');
  }, [account?.id, account?.group_name]);

  useEffect(() => {
    setIgnoreRateLimitControls(Boolean(account?.ignore_rate_limit_controls));
  }, [account?.id, account?.ignore_rate_limit_controls]);

  useEffect(() => {
    setRoutingWeight(Number(account?.routing_weight) > 0 ? Number(account.routing_weight) : 100);
    setRetryMaxAttempts(Math.max(0, Math.min(3, Number(account?.retry_max_attempts) || 0)));
  }, [account?.id, account?.routing_weight, account?.retry_max_attempts]);

  useEffect(() => {
    const cfg = details.codexReauth?.config || {};
    setReauthForm({
      login_email: cfg.login_email || account?.email || '',
      password: '',
      otp_url: '',
      target_workspace_id: cfg.target_workspace_id || account?.upstream_account_id || '',
      auto_enabled: Boolean(cfg.auto_enabled),
    });
  }, [account?.id, account?.email, account?.upstream_account_id, details.codexReauth?.config?.updated_at]);

  const { run: saveDefaultEgress, running: savingDefaultEgress } = useAsyncAction(async () => {
    if (!account || !selectedEgress) return;
    try {
      const saved = await post(`/admin/accounts/${encodeURIComponent(account.id)}/egress-binding`, {
        primary_egress_id: selectedEgress,
        sidecar_egress_id: selectedSidecar,
      });
      Toast.success('出口与 Sidecar 绑定已保存');
      void onUpdated?.(account.id, saved);
    } catch (err) {
      showErrorToast(err);
    }
  });

  const { run: inheritGroupEgress, running: inheritingGroupEgress } = useAsyncAction(async () => {
    if (!account) return;
    try {
      const saved = await post(`/admin/accounts/${encodeURIComponent(account.id)}/egress-binding`, {
        inherit_group_egress: true,
      });
      Toast.success('已恢复为随分组出口');
      void onUpdated?.(account.id, saved);
    } catch (err) {
      showErrorToast(err);
    }
  });

  const { run: saveGroup, running: savingGroup } = useAsyncAction(async () => {
    if (!account || !selectedGroup) return;
    try {
      await post(`/admin/accounts/${encodeURIComponent(account.id)}/group`, { group: selectedGroup });
      Toast.success('分组已保存');
      void onUpdated?.(account.id, { group_name: selectedGroup });
    } catch (err) {
      showErrorToast(err);
    }
  });

  const { run: saveIgnoreRateLimitControls, running: savingIgnoreRateLimitControls } = useAsyncAction(async (enabled) => {
    if (!account) return;
    const previous = ignoreRateLimitControls;
    setIgnoreRateLimitControls(enabled);
    try {
      const saved = await post(`/admin/accounts/${encodeURIComponent(account.id)}/rate-limit-controls`, {
        ignore_rate_limit_controls: enabled,
      });
      Toast.success(enabled ? '此账号已忽略 429、冷却和隔离' : '此账号已恢复正常限流保护');
      void onUpdated?.(account.id, saved);
    } catch (err) {
      setIgnoreRateLimitControls(previous);
      showErrorToast(err);
    }
  });

  const { run: saveRoutingPolicy, running: savingRoutingPolicy } = useAsyncAction(async () => {
    if (!account) return;
    const weight = Number(routingWeight);
    const attempts = Number(retryMaxAttempts);
    if (!Number.isInteger(weight) || weight < 1 || weight > 1000 || !Number.isInteger(attempts) || attempts < 0 || attempts > 3) {
      Toast.error('权重需为 1–1000 的整数，尝试上限需为 0–3 的整数');
      return;
    }
    try {
      const saved = await post(`/admin/accounts/${encodeURIComponent(account.id)}/routing-policy`, {
        routing_weight: weight,
        retry_max_attempts: attempts,
      });
      Toast.success('账号分压与安全重试策略已保存');
      void onUpdated?.(account.id, saved);
    } catch (err) {
      showErrorToast(err);
    }
  });

  const updateReauthForm = (key, value) => setReauthForm((current) => ({ ...current, [key]: value }));

  const { run: saveCodexReauth, running: savingCodexReauth } = useAsyncAction(async () => {
    if (!account) return;
    try {
      await put(`/admin/accounts/${encodeURIComponent(account.id)}/codex-reauth-config`, {
        login_email: reauthForm.login_email,
        password: reauthForm.password,
        otp_url: reauthForm.otp_url,
        target_workspace_id: reauthForm.target_workspace_id,
        auto_enabled: reauthForm.auto_enabled,
      });
      Toast.success('Codex 重登配置已保存');
      setReauthForm((current) => ({ ...current, password: '', otp_url: '' }));
      void reload();
    } catch (err) {
      showErrorToast(err);
    }
  });

  const { run: runCodexReauth, running: runningCodexReauth } = useAsyncAction(async () => {
    if (!account) return;
    try {
      await post(`/admin/accounts/${encodeURIComponent(account.id)}/codex-reauth/run`, {});
      Toast.success('Codex 重登成功，授权凭据已更新');
      void onUpdated?.(account.id, { status: 'active', quarantine_until: 0, quarantine_reason: '' });
      void reload();
    } catch (err) {
      showErrorToast(err);
      void reload();
    }
  });

  const { run: startCodexReauthOAuth, running: startingCodexReauthOAuth } = useAsyncAction(async () => {
    if (!account) return;
    try {
      const result = await post(`/admin/accounts/${encodeURIComponent(account.id)}/codex-reauth/oauth/start`, {
        target_workspace_id: reauthForm.target_workspace_id,
      });
      setOauthModal({ open: true, session_id: result.session_id, auth_url: result.auth_url, redirected: '', target_workspace_id: result.target_workspace_id || reauthForm.target_workspace_id || '' });
    } catch (err) {
      showErrorToast(err);
    }
  });

  const { run: completeCodexReauthOAuth, running: completingCodexReauthOAuth } = useAsyncAction(async () => {
    if (!account || !oauthModal.session_id || !oauthModal.redirected.trim()) return;
    try {
      const updated = await post(`/admin/accounts/${encodeURIComponent(account.id)}/codex-reauth/oauth/complete`, {
        session_id: oauthModal.session_id,
        redirected: oauthModal.redirected,
      });
      Toast.success('OAuth 重登成功，已更新原账号');
      setOauthModal({ open: false, session_id: '', auth_url: '', redirected: '', target_workspace_id: '' });
      void onUpdated?.(account.id, updated);
      void reload();
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
  const sidecarOptions = [
    { label: '不绑定（使用 Go 直连/代理传输）', value: '' },
    ...profiles
      .filter((profile) => String(profile.type || '').toLowerCase() === 'curl_cffi_sidecar')
      .map((profile) => ({ label: `${profile.name || profile.id} (${profile.endpoint || '未配置 Endpoint'})`, value: profile.id })),
  ];
  const selectedProfile = profiles.find((profile) => profile.id === selectedEgress) || null;
  const primaryAlreadySidecar = String(selectedProfile?.type || '').toLowerCase() === 'curl_cffi_sidecar';
  const recommendClaudeSidecar = (account.provider || '') === 'claude' && !primaryAlreadySidecar && !selectedSidecar;
  const isActionLoading = (act) => Boolean(isActionLoadingProp?.(account.id, act));
  const isActionDisabled = (act) => actionDisabled || (actionRunning && !isActionLoading(act));
  const resetCredits = account.quota_summary?.reset_credits;
  const supportsCodexReauth = (account.provider || 'codex') === 'codex' && account.auth_method !== 'api_key' && account.credential_mode !== 'agent_identity';
  const codexReauth = details.codexReauth || null;
  const codexReauthConfig = codexReauth?.config || {};
  const latestCodexReauthJob = codexReauth?.latest_job || null;
  const kiroAccount = isKiroAccount(account);
  const kiroSuspended = isKiroSuspended(account);
  const providerAPIKey = isProviderAPIKeyAccount(account);
  const paidHealthTest = requiresPaidHealthTest(account);
  const protectedProbeQuarantine = isProtectedProbeQuarantine(account);
  const capabilities = Array.isArray(account.capabilities) ? account.capabilities : [];

  return (
    <Drawer title={account.label || account.id} visible={!!account} onCancel={onClose} width={520} className="pool-account-drawer">
      <LoadErrorBanner error={error} onRetry={reload} />
      <Panel title="身份" style={{ marginBottom: 14 }}>
        <Row k="账号 ID" v={<span className="pool-mono">{account.id}</span>} />
        <Row k="邮箱" v={account.email || '—'} />
        <Row k="提供商" v={<Tag>{account.provider || 'codex'}</Tag>} />
        <Row k="认证方式" v={<Tag color={providerAPIKey ? 'violet' : 'blue'}>{account.auth_method || 'oauth'}</Tag>} />
        {account.credential_mode === 'agent_identity' ? <Row k="凭据模式" v={<Tag color="cyan">Agent Identity</Tag>} /> : null}
        <Row k="计费方式" v={account.billing_mode === 'pay_as_you_go' ? <Tag color="violet">按量计费</Tag> : '订阅'} />
        {providerAPIKey ? <Row k="API Key" v={account.api_key_present ? '已加密保存' : '未检测到'} /> : null}
        <Row k="分组" v={account.group_name || '默认'} />
        <Row k="套餐" v={account.plan_type || '—'} />
        <Row k="状态" v={statusTag ? statusTag(account) : account.status} />
        <Row k="调度例外" v={ignoreRateLimitControls ? <Tag color="orange" size="small">忽略 429 / 冷却 / 隔离</Tag> : '无'} />
        <Row k="隔离" v={(account.quarantine_until || 0) > Math.floor(Date.now() / 1000) ? (protectedProbeQuarantine ? '无限期' : fmtRelative(account.quarantine_until)) : '否'} />
        {account.quarantine_reason ? <Row k="隔离原因" v={account.quarantine_reason} /> : null}
      </Panel>

      {kiroSuspended ? (
        <Panel title="AWS User ID 已暂停" style={{ marginBottom: 14 }}>
          <Typography.Text type="danger" as="p">
            AWS 因安全原因锁定了此身份。账号、API Key、能力和审计已保留，但会无限期停止调度。
          </Typography.Text>
          <Typography.Text type="tertiary" as="p">
            请先联系 AWS Support 完成身份验证；确认恢复后，由管理员执行下方 Kiro 双层测活。只有认证和真实推理都成功才会自动解除隔离。
          </Typography.Text>
        </Panel>
      ) : null}

      {providerAPIKey ? (
        <Panel title="上游 API Key" style={{ marginBottom: 14 }}>
          <Typography.Text as="p">此账号使用 Platform / Console 上游 Key，按量计费，不参与 OAuth 刷新、重登或订阅配额同步。</Typography.Text>
          <Typography.Text type="tertiary" as="p">手工测活会先免费读取模型列表；认证成功后发送一次最小推理。推理隔离只能由成功的付费双层测活解除。</Typography.Text>
        </Panel>
      ) : null}

      {account.credential_mode === 'agent_identity' ? (
        <Panel title="Agent Identity" style={{ marginBottom: 14 }}>
          <Typography.Text as="p">私钥已加密保存；每次请求动态签名，task 失效时会经账号绑定出口自动注册并重试一次。</Typography.Text>
          <Typography.Text type="tertiary" as="p">此模式不使用 OAuth access/refresh token，也不会显示 OAuth 重登操作。</Typography.Text>
        </Panel>
      ) : null}

      {account.kiro_auth ? (
        <Panel title="Kiro 认证" style={{ marginBottom: 14 }}>
          <Row k="认证方式" v={account.kiro_auth.auth_method || '—'} />
          <Row k="认证区域" v={account.kiro_auth.auth_region || '—'} />
          <Row k="API 区域" v={account.kiro_auth.api_region || '—'} />
          <Row k="端点" v={account.kiro_auth.endpoint || 'Kiro IDE'} />
          <Row k="敏感凭证" v={[account.kiro_auth.has_client_secret && 'Client Secret', account.kiro_auth.has_api_key && 'API Key'].filter(Boolean).join(' / ') || 'OAuth Token'} />
          <Row k="推理状态" v={kiroSuspended ? <Tag color="red" size="small">AWS User ID 已暂停</Tag> : '需双层测活确认'} />
        </Panel>
      ) : null}

      {!providerAPIKey ? (
        <Panel title="账号额度" style={{ marginBottom: 14 }}>
          <Row k="主动重置次数" v={formatResetCredits(resetCredits)} />
          <Row k="更新时间" v={resetCredits?.updated_at ? fmtRelative(resetCredits.updated_at) : '—'} />
        </Panel>
      ) : null}

      <Panel title="调度例外" style={{ marginBottom: 14 }}>
        <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 12 }}>
          <div>
            <div style={{ fontWeight: 600, fontSize: 13 }}>忽略 429、冷却与隔离</div>
            <Typography.Text size="small" type="tertiary">
              仅影响此账号。Codex 遇到 429 会保持同账号同链路重试，不向下游透传 429；Cloudflare 命中也不会自动冷却或隔离。
            </Typography.Text>
          </div>
          <Switch
            checked={ignoreRateLimitControls}
            disabled={savingIgnoreRateLimitControls}
            onChange={saveIgnoreRateLimitControls}
          />
        </div>
        <Typography.Text size="small" type="tertiary" as="p" style={{ marginTop: 8, display: 'block' }}>
          手动停用、出口不可用及并发上限仍然有效；关闭后，已有冷却和隔离记录会立即重新生效。
        </Typography.Text>
      </Panel>

      <Panel title="分压与凭证内重试" style={{ marginBottom: 14 }}>
        <div className="pool-grid pool-grid-2">
          <InputNumber
            label="新请求软权重"
            help="100 为中性；仅影响新会话选择，不迁移主 CLI、子 Agent 或既有 sticky 上下文。"
            min={1}
            max={1000}
            step={1}
            value={routingWeight}
            onChange={setRoutingWeight}
            disabled={savingRoutingPolicy}
          />
          <InputNumber
            label="同凭证总尝试上限"
            help="0/1 = 单次；2/3 = 最多两/三次。仅限尚未向下游返回字节且可完整重放的请求；上游断连时仍可能重复执行。"
            min={0}
            max={3}
            step={1}
            value={retryMaxAttempts}
            onChange={setRetryMaxAttempts}
            disabled={savingRoutingPolicy}
          />
        </div>
        <Typography.Text size="small" type="tertiary" as="p" style={{ marginTop: 8, display: 'block' }}>
          不会重试 400/401/403/429、Kiro、strict sticky、previous_response_id 或已开始流式输出的请求。
        </Typography.Text>
        <Button size="small" theme="solid" loading={savingRoutingPolicy} onClick={saveRoutingPolicy}>保存分压策略</Button>
      </Panel>

      <Panel title="模型与上下文能力" style={{ marginBottom: 14 }}>
        {!capabilities.length ? <Typography.Text type="tertiary">尚无已发现的模型能力</Typography.Text> : capabilities.map((item) => (
          <div key={`${item.model_slug}:${item.source || ''}`} style={{ display: 'flex', justifyContent: 'space-between', gap: 8, padding: '5px 0', borderBottom: '1px solid var(--pool-border)' }}>
            <span className="pool-mono" style={{ fontSize: 12 }}>{item.model_slug || '—'}</span>
            <span style={{ display: 'flex', gap: 5, flexWrap: 'wrap', justifyContent: 'flex-end' }}>
              <Tag size="small" color={item.availability_state === 'verified' ? 'green' : item.availability_state === 'unsupported' ? 'red' : 'amber'}>{item.availability_state || 'unverified'}</Tag>
              <Tag size="small" color={item.context_1m_state === 'supported' ? 'green' : item.context_1m_state === 'unsupported' ? 'grey' : 'amber'}>1M {item.context_1m_state || 'unknown'}</Tag>
            </span>
          </div>
        ))}
      </Panel>

      {supportsCodexReauth ? (
        <Panel title="Codex 重登配置" style={{ marginBottom: 14 }}>
          {codexReauth?.error ? (
            <Typography.Text type="danger">状态读取失败：{codexReauth.error}</Typography.Text>
          ) : (
            <>
              <Row k="配置" v={codexReauth?.configured ? <Tag color="green" size="small">已配置</Tag> : <Tag color="amber" size="small">未配置</Tag>} />
              <Row k="自动修复" v={codexReauthConfig.auto_enabled ? '开启' : '关闭'} />
              <Row k="密码" v={codexReauthConfig.password_configured ? '已保存（加密）' : '未保存'} />
              <Row k="OTP URL" v={codexReauthConfig.otp_url_configured ? '已保存（加密）' : '未保存'} />
              <Row k="目标 workspace" v={codexReauthConfig.target_workspace_id || reauthForm.target_workspace_id || '—'} />
              <Row k="最近状态" v={latestCodexReauthJob?.status || codexReauthConfig.last_status || (account.status === 'auth_expired' ? 'auth_expired' : '—')} />
              {latestCodexReauthJob?.last_error || codexReauthConfig.last_error ? (
                <Row k="最近错误" v={latestCodexReauthJob?.last_error || codexReauthConfig.last_error} />
              ) : null}
            </>
          )}

          <div style={{ display: 'grid', gap: 8, marginTop: 10 }}>
            <label className="pool-field">
              <span className="pool-field__label">登录邮箱</span>
              <input className="pool-input" value={reauthForm.login_email} onChange={(event) => updateReauthForm('login_email', event.target.value)} placeholder={account.email || 'name@example.com'} />
            </label>
            <label className="pool-field">
              <span className="pool-field__label">密码（留空保留已保存值）</span>
              <input className="pool-input" type="password" value={reauthForm.password} onChange={(event) => updateReauthForm('password', event.target.value)} placeholder={codexReauthConfig.password_configured ? '已保存；需要轮换时填写' : '用于 worker 登录'} />
            </label>
            <label className="pool-field">
              <span className="pool-field__label">OTP URL（留空保留已保存值）</span>
              <input className="pool-input" value={reauthForm.otp_url} onChange={(event) => updateReauthForm('otp_url', event.target.value)} placeholder={codexReauthConfig.otp_url_configured ? '已保存；需要轮换时填写' : '验证码收件箱 API URL'} />
            </label>
            <label className="pool-field">
              <span className="pool-field__label">K12 / 教师 workspace id</span>
              <input className="pool-input pool-mono" value={reauthForm.target_workspace_id} onChange={(event) => updateReauthForm('target_workspace_id', event.target.value)} placeholder="可选；填写后 OAuth 会校验 id_token" />
            </label>
            <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 10 }}>
              <span className="pool-muted" style={{ fontSize: 13 }}>auth_expired 时自动排队修复</span>
              <Switch checked={!!reauthForm.auto_enabled} onChange={(checked) => updateReauthForm('auto_enabled', checked)} />
            </div>
            <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}>
              <Button size="small" loading={savingCodexReauth} onClick={saveCodexReauth}>保存配置</Button>
              <Button size="small" loading={runningCodexReauth} disabled={!codexReauth?.configured} onClick={runCodexReauth}>自动修复 / 重登</Button>
              <Button size="small" loading={startingCodexReauthOAuth} onClick={startCodexReauthOAuth}>重新登录 OAuth</Button>
            </div>
          </div>
        </Panel>
      ) : null}

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
            <Row
              k="路由来源"
              v={binding.binding_scope === 'account'
                ? <Tag color="blue" size="small">账号单独指定</Tag>
                : <Tag color="green" size="small">随分组</Tag>}
            />
            <Row k="默认出口" v={binding.primary_egress_id || '—'} />
            <div style={{ display: 'flex', gap: 8, alignItems: 'center', margin: '8px 0' }}>
              <Select
                value={selectedEgress}
                onChange={(value) => {
                  setSelectedEgress(value);
                  if (String(profiles.find((profile) => profile.id === value)?.type || '').toLowerCase() === 'curl_cffi_sidecar') setSelectedSidecar('');
                }}
                optionList={egressOptions}
                placeholder="选择默认出口"
                disabled={savingDefaultEgress || !egressOptions.length}
                style={{ flex: 1, minWidth: 0 }}
              />
              <Button
                size="small"
                loading={savingDefaultEgress}
                disabled={!selectedEgress || (selectedEgress === binding.primary_egress_id && selectedSidecar === (binding.sidecar_egress_id || ''))}
                onClick={saveDefaultEgress}
              >保存</Button>
            </div>
            {binding.binding_scope === 'account' ? (
              <Button
                size="small"
                theme="borderless"
                loading={inheritingGroupEgress}
                disabled={savingDefaultEgress}
                onClick={inheritGroupEgress}
                style={{ marginBottom: 8 }}
              >恢复为随分组出口</Button>
            ) : null}
            <Row k="TLS/HTTP2 Sidecar" v={primaryAlreadySidecar ? '默认出口本身已是 Sidecar' : (binding.sidecar_egress_id || '未绑定')} />
            {!primaryAlreadySidecar ? (
              <Select
                value={selectedSidecar}
                onChange={setSelectedSidecar}
                optionList={sidecarOptions}
                disabled={savingDefaultEgress}
                style={{ width: '100%', marginBottom: 8 }}
              />
            ) : null}
            {recommendClaudeSidecar ? (
              <div style={{ padding: '8px 10px', marginBottom: 8, borderRadius: 8, background: 'var(--semi-color-warning-light-default)', color: 'var(--semi-color-warning)' }}>
                Claude 直连或普通代理仍会暴露 Go TLS/HTTP2 指纹。建议绑定 Sidecar；紧急绕过请使用系统配置中的 claude_force_direct，指纹覆盖使用 claude_ja3。
              </div>
            ) : null}
            {selectedSidecar && !primaryAlreadySidecar ? (
              <Typography.Text size="small" type="tertiary">
                实际链路：Sidecar → {selectedProfile?.name || selectedEgress} → 上游；出口 IP、冷却与审计仍归属默认出口。
              </Typography.Text>
            ) : null}
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
        {paidHealthTest ? (
          <ConfirmDialog
            title={kiroAccount ? '确认执行 Kiro 双层测活？' : '确认执行 API Key 双层测活？'}
            description="将先免费检查认证；认证正常后，会绑定此账号和当前出口发送 1 次最小推理请求，并可能产生少量上游费用。"
            confirmText="确认并测活"
            onConfirm={() => onAction(account.id, 'health-test', healthTestRequestBody(account, true))}
          >
            <Button loading={isActionLoading('health-test')} disabled={isActionDisabled('health-test')}>测活</Button>
          </ConfirmDialog>
        ) : (
          <Button loading={isActionLoading('health-test')} disabled={isActionDisabled('health-test')} onClick={() => onAction(account.id, 'health-test')}>测活</Button>
        )}
        <Button loading={isActionLoading('clear-quarantine')} disabled={protectedProbeQuarantine || isActionDisabled('clear-quarantine')} onClick={() => onAction(account.id, 'clear-quarantine')}>解除隔离</Button>
        <Button loading={isActionLoading('clear-cooldown')} disabled={isActionDisabled('clear-cooldown')} onClick={() => onAction(account.id, 'clear-cooldown')}>解除冷却</Button>
        {!providerAPIKey && account.credential_mode !== 'agent_identity' ? <Button loading={isActionLoading('refresh')} disabled={isActionDisabled('refresh')} onClick={() => onAction(account.id, 'refresh')}>刷新</Button> : null}
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
      <Modal
        visible={oauthModal.open}
        title="Codex OAuth 重新登录"
        width={640}
        okText="完成回写"
        confirmLoading={completingCodexReauthOAuth}
        onOk={completeCodexReauthOAuth}
        onCancel={() => setOauthModal({ open: false, session_id: '', auth_url: '', redirected: '', target_workspace_id: '' })}
      >
        <Typography.Text as="p">
          打开下面的官方 Codex OAuth 链接，登录目标账号后，将浏览器地址栏中的回调 URL 粘贴回来。
          {oauthModal.target_workspace_id ? ` 本次会校验 workspace：${oauthModal.target_workspace_id}` : ''}
        </Typography.Text>
        <div style={{ display: 'grid', gap: 8 }}>
          <a className="pool-mono" href={oauthModal.auth_url} target="_blank" rel="noreferrer" style={{ wordBreak: 'break-all' }}>{oauthModal.auth_url}</a>
          <textarea
            className="pool-textarea pool-mono"
            rows={5}
            value={oauthModal.redirected}
            onChange={(event) => setOauthModal((current) => ({ ...current, redirected: event.target.value }))}
            placeholder="http://localhost:1455/auth/callback?code=...&state=..."
          />
        </div>
      </Modal>
    </Drawer>
  );
}
