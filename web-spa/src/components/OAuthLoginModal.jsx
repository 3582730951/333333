import React, { useState, useCallback, useRef, useEffect, useMemo } from 'react';
import {
  Modal, Tabs, TabPane, Form, Input, Select, Button, Typography, Toast, Divider, Tooltip, Tag,
} from './pool/index.jsx';
import {
  IconCopy, IconTick, IconRefresh, IconLink,
  IconChevronRight, IconCheckCircleStroked, IconFile,
} from './pool/icons.jsx';
import { get, oauthStart, oauthComplete, post } from '../api.js';
import { showErrorToast } from './ErrorToast.jsx';
import VendorLogo from './VendorLogo.jsx';
import useAsyncAction from '../hooks/useAsyncAction.js';
import { writeClipboard } from '../lib/browserClipboard.js';
import { clearBrowserInterval, clearBrowserTimeout, setBrowserInterval, setBrowserTimeout } from '../lib/browserLifecycle.js';
import { openExternalURL } from '../lib/browserNavigation.js';

const { Text } = Typography;

function egressOptionList(profiles = []) {
  const out = [];
  const seen = new Set();
  const add = (profile) => {
    const id = String(profile?.id || '').trim();
    if (!id || seen.has(id)) return;
    seen.add(id);
    out.push({ label: `${profile.name || id} (${profile.type || 'direct'})`, value: id });
  };
  add({ id: 'egress_direct', name: 'egress_direct', type: 'direct' });
  for (const profile of profiles || []) add(profile);
  return out;
}

function normalizeEgressResponse(data) {
  if (Array.isArray(data)) return data;
  return data?.profiles || data?.egress_profiles || [];
}

// OAuthLoginModal - 新版账号导入弹窗，支持：
// 1. ChatGPT/Codex OAuth 授权登录
// 2. Claude OAuth 授权登录
// 3. 手动导入 auth.json（兼容旧功能）
export default function OAuthLoginModal({ visible, onClose, onSuccess, open }) {
  // Support both prop names: visible (Semi UI convention) and open (Accounts page uses)
  const isVisible = visible ?? open;

  const [tab, setTab] = useState('chatgpt');
  const [sessionId, setSessionId] = useState('');
  const [authUrl, setAuthUrl] = useState('');
  const [redirected, setRedirected] = useState('');
  const [copied, setCopied] = useState(false);
  const [manualRaw, setManualRaw] = useState('');
  const [manualResult, setManualResult] = useState(null);
  const [kiroRaw, setKiroRaw] = useState('');
  const [kiroClientRaw, setKiroClientRaw] = useState('');
  const [kiroImportMode, setKiroImportMode] = useState('api_key');
  const [kiroApiKey, setKiroApiKey] = useState('');
  const [kiroApiRegion, setKiroApiRegion] = useState('us-east-1');
  const [kiroResult, setKiroResult] = useState(null);
  const [authMode, setAuthMode] = useState('oauth');
  const [providerApiKey, setProviderApiKey] = useState('');
  const [confirmProviderCost, setConfirmProviderCost] = useState(false);
  const [providerKeyResult, setProviderKeyResult] = useState(null);
  const [egressId, setEgressId] = useState('egress_direct');
  const [egressProfiles, setEgressProfiles] = useState([]);
  const countdownRef = useRef(null);
  const copyResetRef = useRef(null);
  const actionEpochRef = useRef(0);
  const [countdown, setCountdown] = useState(0);

  // Form fields
  const [label, setLabel] = useState('');
  const [groupName, setGroupName] = useState('');
  const [note, setNote] = useState('');
  const egressOptions = useMemo(() => egressOptionList(egressProfiles), [egressProfiles]);

  useEffect(() => {
    if (!isVisible) return undefined;
    let cancelled = false;
    get('/admin/egress-profiles')
      .then((data) => {
        if (!cancelled) setEgressProfiles(normalizeEgressResponse(data));
      })
      .catch((e) => {
        if (!cancelled) showErrorToast(e, { prefix: '出口列表读取失败' });
      });
    return () => { cancelled = true; };
  }, [isVisible]);

  // Cleanup on close
  useEffect(() => {
    if (!isVisible) {
      clearBrowserInterval(countdownRef.current);
      clearBrowserTimeout(copyResetRef.current);
    }
  }, [isVisible]);

  useEffect(() => () => {
    clearBrowserInterval(countdownRef.current);
    clearBrowserTimeout(copyResetRef.current);
  }, []);

  // Countdown timer
  useEffect(() => {
    if (countdown > 0) {
      countdownRef.current = setBrowserInterval(() => {
        setCountdown((c) => {
          if (c <= 1) {
            clearBrowserInterval(countdownRef.current);
            countdownRef.current = null;
            return 0;
          }
          return c - 1;
        });
      }, 1000);
    }
    return () => clearBrowserInterval(countdownRef.current);
  }, [countdown > 0]);

  const resetForm = useCallback(() => {
    actionEpochRef.current += 1;
    setSessionId('');
    setAuthUrl('');
    setRedirected('');
    setCopied(false);
    setManualRaw('');
    setManualResult(null);
    setKiroRaw('');
    setKiroClientRaw('');
    setKiroImportMode('api_key');
    setKiroApiKey('');
    setKiroApiRegion('us-east-1');
    setKiroResult(null);
    setAuthMode('oauth');
    setProviderApiKey('');
    setConfirmProviderCost(false);
    setProviderKeyResult(null);
    setLabel('');
    setGroupName('');
    setNote('');
    setEgressId('egress_direct');
    setCountdown(0);
    clearBrowserInterval(countdownRef.current);
    clearBrowserTimeout(copyResetRef.current);
  }, []);

  const handleClose = useCallback(() => {
    resetForm();
    onClose();
  }, [resetForm, onClose]);

  const { run: handleGenerate, running: generating } = useAsyncAction(async () => {
    const actionEpoch = actionEpochRef.current;
    try {
      const result = await oauthStart(tab);
      if (actionEpoch !== actionEpochRef.current) return;
      setSessionId(result.session_id || result.loginId || '');
      setAuthUrl(result.auth_url || result.authUrl || '');
      setCountdown(result.expires_in || 900);
      Toast.info('登录链接已生成，请在浏览器中完成授权');
    } catch (e) {
      if (actionEpoch !== actionEpochRef.current) return;
      showErrorToast(e, { prefix: '生成登录链接失败' });
    }
  });

  const handleCopyUrl = async () => {
    if (await writeClipboard(authUrl)) {
      setCopied(true);
      Toast.success('已复制到剪贴板');
      clearBrowserTimeout(copyResetRef.current);
      copyResetRef.current = setBrowserTimeout(() => setCopied(false), 2000);
      return true;
    }
    Toast.error('复制失败，请手动复制');
    return false;
  };

  const handleOpenInBrowser = () => {
    if (authUrl) {
      const opened = openExternalURL(authUrl);
      if (!opened) {
        Toast.warning('浏览器阻止了弹窗，已尝试复制授权链接');
        void handleCopyUrl();
      }
    }
  };

  const { run: handleComplete, running: completing } = useAsyncAction(async (redirectedValue) => {
    const actionEpoch = actionEpochRef.current;
    const val = (redirectedValue || redirected).trim();
    if (!val) {
      Toast.warning('请输入登录后的回调地址或授权码');
      return;
    }
    if (!sessionId) {
      Toast.warning('请先生成登录链接');
      return;
    }
    try {
      const result = await oauthComplete(sessionId, val, label, groupName, egressId);
      if (actionEpoch !== actionEpochRef.current) return;
      Toast.success({
        content: (
          <span>
            账号 <strong>{result.label || result.email || result.id}</strong> 导入成功！
          </span>
        ),
        duration: 3,
      });
      handleClose();
      if (onSuccess) onSuccess(result);
    } catch (e) {
      if (actionEpoch !== actionEpochRef.current) return;
      showErrorToast(e, { prefix: '导入失败' });
    }
  });

  const { run: handleManualImport, running: manualLoading } = useAsyncAction(async () => {
    const actionEpoch = actionEpochRef.current;
    const val = manualRaw.trim();
    if (!val) {
      Toast.warning('请输入 auth.json 内容');
      return;
    }
    try {
      // 调用后端的 import-auth-json 接口
      const result = await post('/admin/accounts/import-auth-json', {
        label,
        note,
        group_name: groupName,
        egress_id: egressId,
        auth_json_text: val,
      });
      if (actionEpoch !== actionEpochRef.current) return;
      if (Array.isArray(result.items)) {
        setManualResult(result);
        const summary = `导入 ${result.imported || 0}，重复 ${result.duplicates || 0}，失败 ${result.failed || 0}`;
        if ((result.failed || 0) > 0) Toast.warning(summary);
        else Toast.success(summary);
        if ((result.imported || 0) > 0 && onSuccess) onSuccess(result);
        if ((result.failed || 0) === 0) handleClose();
        return;
      }
      Toast.success({
        content: (
          <span>
            账号 <strong>{result.label || result.email || result.id}</strong> 导入成功！
          </span>
        ),
        duration: 3,
      });
      handleClose();
      if (onSuccess) onSuccess(result);
    } catch (e) {
      if (actionEpoch !== actionEpochRef.current) return;
      showErrorToast(e, { prefix: '导入失败' });
    }
  });

  const recognizedKiroCount = useMemo(() => {
    try {
      const parsed = JSON.parse(kiroRaw || 'null');
      if (Array.isArray(parsed)) return parsed.length;
      if (Array.isArray(parsed?.accounts)) return parsed.accounts.length;
      return parsed && typeof parsed === 'object' ? 1 : 0;
    } catch (_) { return 0; }
  }, [kiroRaw]);

  const { run: handleKiroImport, running: kiroLoading } = useAsyncAction(async () => {
    if (!kiroRaw.trim()) { Toast.warning('请粘贴 Kiro 凭证 JSON'); return; }
    try {
      const result = await post('/admin/accounts/import-kiro-json', {
        kiro_json_text: kiroRaw,
        kiro_client_json_text: kiroClientRaw,
        label,
        group_name: groupName,
        egress_id: egressId,
      }, { timeout: 120000 });
      setKiroResult(result);
      if (result.imported > 0) {
        Toast.success(`已导入 ${result.imported} 个 Kiro 账号`);
        if (onSuccess) onSuccess(result);
      } else if (result.failed > 0) Toast.warning('没有账号导入成功，请查看逐项结果');
    } catch (e) { showErrorToast(e, { prefix: 'Kiro 导入失败' }); }
  });

  const { run: handleKiroApiKeyImport, running: kiroApiKeyLoading } = useAsyncAction(async () => {
    if (!kiroApiKey.trim()) { Toast.warning('请输入 ksk_ API Key'); return; }
    try {
      const result = await post('/admin/accounts/import-kiro-api-key', {
        kiro_api_key: kiroApiKey.trim(), label, group_name: groupName,
        egress_id: egressId, api_region: kiroApiRegion.trim(),
      }, { timeout: 120000 });
      Toast.success(`Kiro API Key 账号 ${result.label || result.id} 已导入并验活`);
      handleClose();
      if (onSuccess) onSuccess(result);
    } catch (e) { showErrorToast(e, { prefix: 'Kiro API Key 导入失败' }); }
  });

  const { run: handleProviderApiKeyImport, running: providerApiKeyLoading } = useAsyncAction(async () => {
    if (!providerApiKey.trim()) { Toast.warning('请输入上游 API Key'); return; }
    if (!confirmProviderCost) { Toast.warning('请确认将执行一次可能计费的最小推理探针'); return; }
    const providerId = tab === 'claude' ? 'claude' : 'codex';
    try {
      const result = await post('/admin/accounts/import-key', {
        provider_id: providerId,
        api_key: providerApiKey.trim(),
        label,
        group_name: groupName,
        egress_id: egressId,
        confirm_cost: true,
      }, { timeout: 120000 });
      setProviderKeyResult(result);
      if (result.ready) Toast.success('认证与最小推理均通过，API Key 账号已就绪');
      else Toast.warning('认证已通过，但推理探针失败；账号已保存并无限期隔离');
      if (onSuccess) onSuccess(result);
    } catch (e) {
      setProviderKeyResult(e?.response?.data || e?.data || null);
      showErrorToast(e, { prefix: 'API Key 导入失败' });
    }
  });

  // Provider display info
  const providerInfo = {
    chatgpt: {
      name: 'ChatGPT / Codex',
      desc: '使用 OpenAI 账号授权登录',
      vendor: 'openai',
    },
    claude: {
      name: 'Claude',
      desc: '使用 Anthropic 账号授权登录',
      vendor: 'claude',
    },
    antigravity: {
      name: 'Antigravity (Google Cloud Code)',
      desc: '使用 Google 账号授权登录',
      vendor: 'google',
    },
    kiro: { name: 'Kiro', desc: '导入 API Key 或 Kiro IDE / KAM 凭证', vendor: 'kiro' },
  };

  const currentInfo = providerInfo[tab] || providerInfo.chatgpt;

  // Manual import tab content
  const manualTabContent = (
    <div className="pool-oauth-tab">
      <div className="pool-oauth-identity">
        <span className="pool-oauth-manual-icon"><IconFile /></span>
        <div className="pool-oauth-identity__copy">
          <Text strong className="pool-oauth-identity__name">手动导入</Text>
          <Text type="tertiary" className="pool-oauth-identity__desc">支持 auth.json、数组及 sub2api-data 备份</Text>
        </div>
      </div>

      <Form>
        <Form.Slot label="标签 (可选)">
          <Input
            placeholder="例如: 高频, 团队A"
            value={label}
            onChange={setLabel}
          />
        </Form.Slot>

        <Form.Slot label="备注 (可选)">
          <Input
            placeholder="例如: 主号 / 测试号"
            value={note}
            onChange={setNote}
          />
        </Form.Slot>

        <Form.Slot label="分组 (可选)">
          <Input
            placeholder="留空使用默认分组"
            value={groupName}
            onChange={setGroupName}
          />
        </Form.Slot>

        <Form.Slot label="账号默认出口">
          <Select
            value={egressId}
            onChange={setEgressId}
            optionList={egressOptions}
            placeholder="选择默认出口"
          />
        </Form.Slot>

        <Divider margin="16px 0" />

        <div style={{ marginBottom: 12 }}>
          <Text type="tertiary" style={{ fontSize: 13 }}>
            支持单个 auth.json、auth.json 数组或 other_sub2api 导出的 sub2api-data：
          </Text>
        </div>
        <textarea
          className="pool-textarea"
          placeholder={'{\n  "tokens": {\n    "access_token": "...",\n    "refresh_token": "..."\n  }\n}'}
          value={manualRaw}
          onChange={(e) => { setManualRaw(e.target.value); setManualResult(null); }}
          style={{
            width: '100%',
            minHeight: 200,
            padding: 12,
            borderRadius: 6,
            border: '1px solid var(--pool-border)',
            fontSize: 13,
            fontFamily: 'monospace',
            resize: 'vertical',
            background: 'var(--pool-bg-surface)',
            color: 'var(--pool-text)',
          }}
        />
        {manualResult ? (
          <div style={{ marginTop: 12, padding: 12, border: '1px solid var(--pool-border)', borderRadius: 6 }}>
            <Text strong>批量导入结果</Text>
            <Text as="p" type="tertiary" style={{ margin: '6px 0' }}>
              账号：导入 {manualResult.imported || 0} / 重复 {manualResult.duplicates || 0} / 失败 {manualResult.failed || 0}
              {manualResult.proxy_created || manualResult.proxy_reused || manualResult.proxy_failed
                ? `；出口：新建 ${manualResult.proxy_created || 0} / 复用 ${manualResult.proxy_reused || 0} / 失败 ${manualResult.proxy_failed || 0}` : ''}
            </Text>
            {(manualResult.items || []).filter((item) => item.action === 'failed').map((item) => (
              <Text key={`${item.index}:${item.name || ''}`} type="danger" as="p" style={{ margin: '4px 0' }}>
                第 {item.index} 条{item.name ? `（${item.name}）` : ''}：{item.message || '导入失败'}
              </Text>
            ))}
          </div>
        ) : null}
      </Form>

      <div style={{ marginTop: 16, display: 'flex', justifyContent: 'flex-end' }}>
        <Button
          type="primary"
          theme="solid"
          icon={<IconFile />}
          loading={manualLoading}
          disabled={!manualRaw.trim()}
          onClick={handleManualImport}
        >
          导入账号
        </Button>
      </div>
    </div>
  );

  const kiroTabContent = (
    <div className="pool-oauth-tab">
      <div className="pool-oauth-identity pool-oauth-identity--provider">
        <VendorLogo vendor="kiro" size={40} />
        <div className="pool-oauth-identity__copy">
          <Text strong className="pool-oauth-identity__name">Kiro 账号导入</Text>
          <Text type="tertiary" className="pool-oauth-identity__desc">API Key 直接导入，或使用原有凭证 JSON 批量导入</Text>
        </div>
      </div>
      <Form>
        <Form.Slot label="标签 (可选)"><Input value={label} onChange={setLabel} placeholder="批量导入时会自动追加序号" /></Form.Slot>
        <Form.Slot label="分组 (可选)"><Input value={groupName} onChange={setGroupName} placeholder="留空使用默认分组" /></Form.Slot>
        <Form.Slot label="账号默认出口"><Select value={egressId} onChange={setEgressId} optionList={egressOptions} /></Form.Slot>
      </Form>
      <div style={{ display: 'flex', gap: 8, marginBottom: 14 }}>
        <Button type={kiroImportMode === 'api_key' ? 'primary' : 'tertiary'} onClick={() => setKiroImportMode('api_key')}>API Key</Button>
        <Button type={kiroImportMode === 'json' ? 'primary' : 'tertiary'} onClick={() => setKiroImportMode('json')}>凭证 JSON</Button>
      </div>
      {kiroImportMode === 'api_key' ? (
        <div style={{ display: 'grid', gap: 12 }}>
          <Form>
            <Form.Slot label="Kiro API Key"><Input type="password" value={kiroApiKey} onChange={setKiroApiKey} placeholder="ksk_..." /></Form.Slot>
            <Form.Slot label="API 区域"><Input value={kiroApiRegion} onChange={setKiroApiRegion} placeholder="us-east-1" /></Form.Slot>
          </Form>
          <Text type="tertiary">Key 仅用于加密账号凭证；导入验活使用非生成型 Usage Limits 接口。</Text>
          <div style={{ display: 'flex', justifyContent: 'flex-end' }}>
            <Button type="primary" theme="solid" loading={kiroApiKeyLoading} disabled={!kiroApiKey.trim()} onClick={handleKiroApiKeyImport}>导入并验活</Button>
          </div>
        </div>
      ) : (
      <>
      <div style={{ display: 'grid', gap: 12 }}>
        <div>
          <Text strong>Token / 账号 JSON</Text>
          <Text type="tertiary" style={{ marginLeft: 8 }}>必填</Text>
          <textarea
            className="pool-textarea"
            value={kiroRaw}
            onChange={(e) => { setKiroRaw(e.target.value); setKiroResult(null); }}
            placeholder={'粘贴 kiro-auth-token.json、单个账号、数组或 { "accounts": [...] }'}
            style={{ width: '100%', minHeight: 135, marginTop: 6, padding: 12, borderRadius: 6, border: '1px solid var(--pool-border)', fontFamily: 'monospace', resize: 'vertical', background: 'var(--pool-bg-surface)', color: 'var(--pool-text)' }}
          />
        </div>
        <div>
          <Text strong>客户端注册 JSON</Text>
          <Text type="tertiary" style={{ marginLeft: 8 }}>Builder ID / IdC / Enterprise 必填，Social 与 API Key 可留空</Text>
          <textarea
            className="pool-textarea"
            value={kiroClientRaw}
            onChange={(e) => { setKiroClientRaw(e.target.value); setKiroResult(null); }}
            placeholder={'粘贴与 clientIdHash 对应的 <hash>.json，后端会自动合并 clientId/clientSecret'}
            style={{ width: '100%', minHeight: 120, marginTop: 6, padding: 12, borderRadius: 6, border: '1px solid var(--pool-border)', fontFamily: 'monospace', resize: 'vertical', background: 'var(--pool-bg-surface)', color: 'var(--pool-text)' }}
          />
        </div>
      </div>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginTop: 12 }}>
        <Text type="tertiary">识别账号：{recognizedKiroCount}；两份 JSON 仅在服务端内存中合并，敏感字段加密保存</Text>
        <Button type="primary" theme="solid" loading={kiroLoading} disabled={!kiroRaw.trim()} onClick={handleKiroImport}>导入并验活</Button>
      </div>
      {kiroResult?.results?.length ? (
        <div style={{ marginTop: 16, maxHeight: 180, overflow: 'auto' }}>
          {kiroResult.results.map((item) => (
            <div key={item.index} style={{ padding: '8px 0', borderTop: '1px solid var(--pool-border)', display: 'flex', gap: 8 }}>
              <Text strong>#{item.index + 1}</Text><Text>{item.status}</Text><Text type="tertiary">{item.label || item.error || ''}</Text>
            </div>
          ))}
        </div>
      ) : null}
      </>
      )}
    </div>
  );

  // OAuth tab content
  const oauthTabContent = (
    <div className="pool-oauth-tab">
      {!authUrl ? (
        <div>
          <div className="pool-oauth-identity pool-oauth-identity--provider">
            <VendorLogo vendor={currentInfo.vendor} size={40} />
            <div className="pool-oauth-identity__copy">
              <Text strong className="pool-oauth-identity__name">
                {currentInfo.name}
              </Text>
              <Text type="tertiary" className="pool-oauth-identity__desc">
                {currentInfo.desc}
              </Text>
            </div>
          </div>

          <div style={{ display: 'flex', gap: 8, marginBottom: 16 }}>
            <Button
              type={authMode === 'oauth' ? 'primary' : 'tertiary'}
              onClick={() => { setAuthMode('oauth'); setProviderKeyResult(null); }}
            >OAuth</Button>
            <Button
              type={authMode === 'api_key' ? 'primary' : 'tertiary'}
              onClick={() => { setAuthMode('api_key'); setProviderKeyResult(null); }}
            >API Key</Button>
          </div>

          <Form>
            <Form.Slot label="备注 (可选)">
              <Input
                placeholder="例如: 主号 / 测试号"
                value={label}
                onChange={setLabel}
              />
            </Form.Slot>

            <Form.Slot label="分组 (可选)">
              <Input
                placeholder="留空使用默认分组"
                value={groupName}
                onChange={setGroupName}
              />
            </Form.Slot>

            <Form.Slot label="账号默认出口">
              <Select
                value={egressId}
                onChange={setEgressId}
                optionList={egressOptions}
                placeholder="选择默认出口"
              />
            </Form.Slot>
          </Form>

          {authMode === 'api_key' ? (
            <div style={{ display: 'grid', gap: 12, marginTop: 14 }}>
              <Form>
                <Form.Slot label={`${currentInfo.name} 上游 API Key`}>
                  <Input
                    type="password"
                    value={providerApiKey}
                    onChange={(value) => { setProviderApiKey(value); setProviderKeyResult(null); }}
                    placeholder={tab === 'claude' ? 'sk-ant-...' : 'sk-...'}
                  />
                </Form.Slot>
              </Form>
              <div style={{ padding: 12, borderRadius: 6, background: 'var(--pool-bg-surface-2)' }}>
                <Text strong>按量计费凭据</Text>
                <Text type="tertiary" as="p" style={{ margin: '6px 0 0', lineHeight: 1.6 }}>
                  该 Key 属于上游 Platform / Console，与下游 cap_* Key 完全分离。系统先免费读取模型列表；认证成功后仅发送一次固定提示“Reply exactly OK”的最小推理。若推理失败，账号会保留但无限期隔离。
                </Text>
              </div>
              <label style={{ display: 'flex', alignItems: 'flex-start', gap: 8, fontSize: 13, cursor: 'pointer' }}>
                <input
                  type="checkbox"
                  checked={confirmProviderCost}
                  onChange={(event) => setConfirmProviderCost(event.target.checked)}
                  style={{ marginTop: 3 }}
                />
                <span>我确认认证成功后执行 1 次可能产生少量费用的最小推理探针。</span>
              </label>
              <div style={{ display: 'flex', justifyContent: 'flex-end' }}>
                <Button
                  type="primary"
                  theme="solid"
                  loading={providerApiKeyLoading}
                  disabled={!providerApiKey.trim() || !confirmProviderCost}
                  onClick={handleProviderApiKeyImport}
                >导入并执行双层测活</Button>
              </div>
              {providerKeyResult ? (
                <div style={{ display: 'grid', gap: 8, padding: 12, border: '1px solid var(--pool-border)', borderRadius: 6 }}>
                  <div style={{ display: 'flex', justifyContent: 'space-between', gap: 8 }}>
                    <Text strong>探针结果</Text>
                    <Tag color={providerKeyResult.ready ? 'green' : providerKeyResult.quarantined ? 'red' : 'orange'}>
                      {providerKeyResult.ready ? '已就绪' : providerKeyResult.quarantined ? '已保存并隔离' : '认证失败，未保存'}
                    </Tag>
                  </div>
                  <Text>免费认证探针：{providerKeyResult.auth_probe?.alive ? '通过' : '失败'} · {providerKeyResult.auth_probe?.state || 'unknown'} · HTTP {providerKeyResult.auth_probe?.http_status || '—'}</Text>
                  <Text>最小推理探针：{providerKeyResult.inference_probe?.alive ? '通过' : providerKeyResult.inference_probe?.checked ? '失败' : '未执行'} · {providerKeyResult.inference_probe?.state || 'unknown'}{providerKeyResult.inference_probe?.model ? ` · ${providerKeyResult.inference_probe.model}` : ''}</Text>
                  {providerKeyResult.quarantine_reason ? <Text type="danger">隔离原因：{providerKeyResult.quarantine_reason}</Text> : null}
                </div>
              ) : null}
            </div>
          ) : (
            <div style={{ marginTop: 20, textAlign: 'center' }}>
              <Button
                type="primary"
                theme="solid"
                size="large"
                icon={<IconLink />}
                loading={generating}
                onClick={handleGenerate}
                style={{ minWidth: 200 }}
              >
                {generating ? '正在生成...' : '生成授权链接'}
              </Button>
            </div>
          )}
        </div>
      ) : (
        <div>
          <div className="pool-oauth-identity pool-oauth-identity--compact">
            <VendorLogo vendor={currentInfo.vendor} size={28} />
            <div className="pool-oauth-identity__copy">
              <Text strong className="pool-oauth-identity__name">{currentInfo.name}</Text>
              <Text type="tertiary" className="pool-oauth-identity__desc">授权链接已生成</Text>
            </div>
          </div>

          {/* Auth URL Section */}
          <div style={{ marginBottom: 16 }}>
            <div
              style={{
                display: 'flex',
                alignItems: 'center',
                justifyContent: 'space-between',
                marginBottom: 8,
              }}
            >
              <Text strong>授权链接</Text>
              {countdown > 0 && (
                <Text type="tertiary" style={{ fontSize: 12 }}>
                  有效期: {Math.floor(countdown / 60)}:{String(countdown % 60).padStart(2, '0')}
                </Text>
              )}
            </div>
            <div
              style={{
                display: 'flex',
                gap: 8,
                alignItems: 'stretch',
              }}
            >
              <Input
                value={authUrl}
                readOnly
                style={{
                  flex: 1,
                  fontFamily: 'monospace',
                  fontSize: 12,
                }}
              />
              <Tooltip content={copied ? '已复制' : '复制链接'}>
                <Button
                  icon={copied ? <IconTick /> : <IconCopy />}
                  onClick={handleCopyUrl}
                  style={{ flexShrink: 0 }}
                />
              </Tooltip>
              <Button
                icon={<IconChevronRight />}
                onClick={handleOpenInBrowser}
                style={{ flexShrink: 0 }}
              >
                打开
              </Button>
            </div>
          </div>

          {/* Instructions */}
          <div
            style={{
              padding: '12px 16px',
              background: 'var(--pool-bg-surface-2)',
              borderRadius: 6,
              marginBottom: 16,
            }}
          >
            <Text type="tertiary" style={{ fontSize: 13, lineHeight: 1.6 }}>
              <strong>操作步骤：</strong>
              <br />
              1. 点击"打开"或复制链接到浏览器
              <br />
              2. 在打开的页面登录您的账号
              <br />
              3. 登录成功后，复制浏览器地址栏的完整网址
              <br />
              4. 粘贴到下方输入框完成授权
            </Text>
          </div>

          {/* Manual callback input */}
          <div style={{ marginBottom: 16 }}>
            <Text strong style={{ marginBottom: 8, display: 'block' }}>
              粘贴回调地址
            </Text>
            <div style={{ display: 'flex', gap: 8 }}>
              <Input
                placeholder="粘贴登录后的完整网址，或页面显示的 code#state"
                value={redirected}
                onChange={setRedirected}
                style={{ flex: 1 }}
              />
              <Button
                type="primary"
                theme="solid"
                icon={<IconCheckCircleStroked />}
                loading={completing}
                disabled={!redirected.trim()}
                onClick={() => handleComplete()}
              >
                完成授权
              </Button>
            </div>
          </div>

          {/* Alternative: Regenerate */}
          <div style={{ textAlign: 'center' }}>
            <Button
              theme="borderless"
              icon={<IconRefresh />}
              onClick={() => {
                setAuthUrl('');
                setSessionId('');
                setRedirected('');
              }}
            >
              重新生成链接
            </Button>
          </div>
        </div>
      )}
    </div>
  );

  return (
    <Modal
      title="添加账号"
      className="pool-account-import-modal"
      visible={isVisible}
      onCancel={handleClose}
      footer={null}
      width={620}
      maskClosable={false}
    >
      <Tabs
        className="pool-account-import-tabs"
        activeKey={tab}
        onChange={(k) => {
          setTab(k);
          resetForm();
        }}
        style={{ marginBottom: 16 }}
      >
        <TabPane
          tab={(
            <span className="pool-vendor-tab">
              <VendorLogo vendor="openai" size={18} />
              <span>ChatGPT / Codex</span>
            </span>
          )}
          itemKey="chatgpt"
        >
          {oauthTabContent}
        </TabPane>
        <TabPane
          tab={(<span className="pool-vendor-tab"><VendorLogo vendor="kiro" size={18} /><span>Kiro</span></span>)}
          itemKey="kiro"
        >
          {kiroTabContent}
        </TabPane>
        <TabPane
          tab={(
            <span className="pool-vendor-tab">
              <VendorLogo vendor="claude" size={18} />
              <span>Claude</span>
            </span>
          )}
          itemKey="claude"
        >
          {oauthTabContent}
        </TabPane>
        <TabPane
          tab={(
            <span className="pool-vendor-tab">
              <VendorLogo vendor="google" size={18} />
              <span>Antigravity</span>
            </span>
          )}
          itemKey="antigravity"
        >
          {oauthTabContent}
        </TabPane>
        <TabPane
          tab={(
            <span className="pool-vendor-tab">
              <span className="pool-oauth-tab-icon"><IconFile /></span>
              <span>手动导入</span>
            </span>
          )}
          itemKey="manual"
        >
          {manualTabContent}
        </TabPane>
      </Tabs>
    </Modal>
  );
}
