import React, { useState, useCallback, useRef, useEffect } from 'react';
import {
  Modal, Tabs, TabPane, Form, Input, Button, Typography, Toast, Divider, Tooltip,
} from '@douyinfe/semi-ui';
import {
  IconCopy, IconTick, IconRefresh, IconLink,
  IconChevronRight, IconCheckCircleStroked, IconFile,
} from '@douyinfe/semi-icons';
import { oauthStart, oauthComplete, post } from '../api.js';
import { showErrorToast } from './ErrorToast.jsx';
import VendorLogo from './VendorLogo.jsx';
import useAsyncAction from '../hooks/useAsyncAction.js';
import { writeClipboard } from '../lib/browserClipboard.js';
import { clearBrowserInterval, clearBrowserTimeout, setBrowserInterval, setBrowserTimeout } from '../lib/browserLifecycle.js';
import { openExternalURL } from '../lib/browserNavigation.js';

const { Text } = Typography;

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
  const countdownRef = useRef(null);
  const copyResetRef = useRef(null);
  const actionEpochRef = useRef(0);
  const [countdown, setCountdown] = useState(0);

  // Form fields
  const [label, setLabel] = useState('');
  const [groupName, setGroupName] = useState('');
  const [note, setNote] = useState('');

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
    setLabel('');
    setGroupName('');
    setNote('');
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
      const result = await oauthComplete(sessionId, val, label, groupName);
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
        auth_json_text: val,
      });
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
  };

  const currentInfo = providerInfo[tab] || providerInfo.chatgpt;

  // Manual import tab content
  const manualTabContent = (
    <div className="pool-oauth-tab">
      <div className="pool-oauth-identity">
        <span className="pool-oauth-manual-icon"><IconFile /></span>
        <div className="pool-oauth-identity__copy">
          <Text strong className="pool-oauth-identity__name">手动导入</Text>
          <Text type="tertiary" className="pool-oauth-identity__desc">粘贴本地 auth.json 内容导入账号</Text>
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

        <Divider margin="16px 0" />

        <div style={{ marginBottom: 12 }}>
          <Text type="tertiary" style={{ fontSize: 13 }}>
            请粘贴完整的 auth.json 内容：
          </Text>
        </div>
        <textarea
          className="pool-textarea"
          placeholder={'{\n  "tokens": {\n    "access_token": "...",\n    "refresh_token": "..."\n  }\n}'}
          value={manualRaw}
          onChange={(e) => setManualRaw(e.target.value)}
          style={{
            width: '100%',
            minHeight: 200,
            padding: 12,
            borderRadius: 6,
            border: '1px solid var(--semi-color-border)',
            fontSize: 13,
            fontFamily: 'monospace',
            resize: 'vertical',
            background: 'var(--semi-color-bg-1)',
            color: 'var(--semi-color-text-1)',
          }}
        />
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
          </Form>

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
              background: 'var(--semi-color-fill-0)',
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
      visible={isVisible}
      onCancel={handleClose}
      footer={null}
      width={560}
      maskClosable={false}
      destroyOnClose
    >
      <Tabs
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
