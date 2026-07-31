import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react';

import { Button, Form, Tag, Toast, Typography } from './pool/index.jsx';
import { IconPulse, IconRefresh } from './pool/icons.jsx';
import { post } from '../api.js';
import { showErrorToast } from './ErrorToast.jsx';
import useAsyncAction from '../hooks/useAsyncAction.js';
import { parseProxyEndpoint } from '../lib/proxyEndpoint';

function cx(...parts) {
  return parts.filter(Boolean).join(' ');
}

export const EGRESS_TEMPLATES = [
  {
    id: 'proxy_url',
    name: '代理 URL',
    desc: 'HTTP / HTTPS 代理账号密码',
    defaults: {
      type: 'http_proxy',
      endpoint: '',
      proxy_auth_mode: '',
      chain_proxy: '',
      region: '',
      exit_ip: '',
      ip_mode: 'dynamic_residential',
      provider_key: 'proxy',
      dynamic_config_json: '{}',
      max_concurrency: 16,
    },
  },
  {
    id: 'socks5',
    name: 'SOCKS5',
    desc: 'SOCKS5 / SOCKS5H 代理 URL',
    defaults: {
      type: 'socks5h_proxy',
      endpoint: '',
      proxy_auth_mode: '',
      chain_proxy: '',
      region: '',
      exit_ip: '',
      ip_mode: 'dynamic_residential',
      provider_key: 'socks5',
      dynamic_config_json: '{}',
      max_concurrency: 16,
    },
  },
  {
    id: 'cliproxy_api',
    name: 'CLIPProxy API',
    desc: 'API 白名单提取 region 锁定 IP',
    defaults: {
      type: 'http_proxy',
      endpoint: '',
      proxy_auth_mode: 'api_whitelist',
      api_base: 'https://api.cliproxy.io',
      proxy_api_key: '',
      api_num: 1,
      api_time: 10,
      region: 'BR',
      exit_ip: '',
      ip_mode: 'dynamic_residential',
      provider_key: 'cliproxy',
      dynamic_config_json: '{}',
      max_concurrency: 10,
    },
  },
  {
    id: 'sidecar',
    name: 'Sidecar',
    desc: 'curl_cffi / JA3 浏览器指纹',
    defaults: {
      type: 'curl_cffi_sidecar',
      endpoint: 'http://127.0.0.1:8790',
      proxy_auth_mode: '',
      chain_proxy: '',
      region: '',
      exit_ip: '',
      ip_mode: 'local_sidecar',
      provider_key: 'cuff',
      dynamic_config_json: '{}',
      max_concurrency: 0,
    },
  },
  {
    id: 'direct',
    name: '直连',
    desc: '服务器本机出口，仅用于低风险流程',
    defaults: {
      type: 'direct',
      endpoint: '',
      proxy_auth_mode: '',
      chain_proxy: '',
      region: '',
      exit_ip: '',
      ip_mode: 'datacenter',
      provider_key: 'direct',
      dynamic_config_json: '{}',
      max_concurrency: 0,
    },
  },
];

const TYPE_OPTIONS = [
  { value: 'direct', label: 'direct (直连)' },
  { value: 'http_proxy', label: 'http_proxy (HTTP 代理)' },
  { value: 'https_proxy', label: 'https_proxy' },
  { value: 'socks5_proxy', label: 'socks5_proxy' },
  { value: 'socks5h_proxy', label: 'socks5h_proxy' },
  { value: 'curl_cffi_sidecar', label: 'curl_cffi_sidecar (JA3 伪装)' },
];

const AUTH_MODE_OPTIONS = [
  { value: '', label: '账号密码模式' },
  { value: 'api_whitelist', label: 'API 白名单模式' },
];

const IP_MODE_OPTIONS = [
  { value: 'static_residential', label: '静态住宅 IP' },
  { value: 'dynamic_residential', label: '动态住宅 IP' },
  { value: 'datacenter', label: '机房 / 普通代理' },
  { value: 'local_sidecar', label: '本地 sidecar / cuff' },
];

function parseDynamicConfig(value) {
  const text = String(value || '').trim();
  if (!text) return {};
  try { return JSON.parse(text); }
  catch { return text; }
}

function inferTemplate(values = {}) {
  if ((values.proxy_auth_mode || '') === 'api_whitelist') return 'cliproxy_api';
  if (values.type === 'curl_cffi_sidecar') return 'sidecar';
  if (values.type === 'socks5_proxy' || values.type === 'socks5h_proxy') return 'socks5';
  if (!values.type || values.type === 'direct') return 'direct';
  return 'proxy_url';
}

export function endpointPlaceholder(type, proxyAuthMode) {
  if (proxyAuthMode === 'api_whitelist') return 'API 白名单模式无需 Endpoint';
  if (type === 'socks5_proxy' || type === 'socks5h_proxy') return 'socks5://user:pass@host:port';
  if (type === 'curl_cffi_sidecar') return 'http://127.0.0.1:8790';
  if (type === 'direct') return '直连无需 Endpoint';
  return 'http://user:pass@host:port';
}

function initialWithTemplate(values = {}) {
  const template = EGRESS_TEMPLATES.find((item) => item.id === inferTemplate(values)) || EGRESS_TEMPLATES[0];
  return { ...template.defaults, ...values };
}

function profilePayload(values) {
  return {
    ...values,
    dynamic_config_json: parseDynamicConfig(values.dynamic_config_json),
  };
}

export default function EgressProfileForm({ initialValues, saving, onSubmit, getFormApi }) {
  const formApi = useRef(null);
  const initial = useMemo(() => initialWithTemplate(initialValues), [initialValues]);
  const [selectedTemplate, setSelectedTemplate] = useState(() => inferTemplate(initial));
  const [profileType, setProfileType] = useState(initial.type || 'http_proxy');
  const [authMode, setAuthMode] = useState(initial.proxy_auth_mode || '');
  const [showAdvanced, setShowAdvanced] = useState(Boolean(initialValues?.id));
  const [testResult, setTestResult] = useState(null);
  const [proxyPreview, setProxyPreview] = useState(null);
  const [proxyInputError, setProxyInputError] = useState('');

  useEffect(() => {
    const next = initialWithTemplate(initialValues);
    setSelectedTemplate(inferTemplate(next));
    setProfileType(next.type || 'http_proxy');
    setAuthMode(next.proxy_auth_mode || '');
    setShowAdvanced(Boolean(initialValues?.id));
    setTestResult(null);
    setProxyPreview(null);
    setProxyInputError('');
  }, [initialValues]);

  const bindFormApi = useCallback((api) => {
    formApi.current = api;
    getFormApi?.(api);
  }, [getFormApi]);

  const applyTemplate = useCallback((template) => {
    setSelectedTemplate(template.id);
    setProfileType(template.defaults.type || 'http_proxy');
    setAuthMode(template.defaults.proxy_auth_mode || '');
    setTestResult(null);
    formApi.current?.setValues?.((current) => ({
      ...current,
      ...template.defaults,
      id: current.id || '',
      name: current.name || '',
    }));
  }, []);

  const { run: testConnection, running: testing } = useAsyncAction(async () => {
    try {
      const values = formApi.current?.getValues?.() || initial;
      const result = await post('/admin/egress-profiles/test', { profile: profilePayload(values) });
      setTestResult(result);
      if (result?.exit_ip) formApi.current?.setValue?.('exit_ip', result.exit_ip);
      if (result?.region) formApi.current?.setValue?.('region', result.region);
      if (Array.isArray(result?.warnings) && result.warnings.length) {
        Toast.warning(result.warnings[0]);
      } else {
        Toast.success('连接测试通过');
      }
    } catch (err) {
      setTestResult(null);
      showErrorToast(err);
    }
  });

  const isApiMode = authMode === 'api_whitelist';
  const isDirect = profileType === 'direct';
  const isSidecar = profileType === 'curl_cffi_sidecar';

  const inspectProxyInput = useCallback((value, normalize = false) => {
    const raw = String(value || '').trim();
    if (!raw) {
      setProxyPreview(null);
      setProxyInputError('');
      return;
    }
    try {
      const parsed = parseProxyEndpoint(raw, profileType);
      setProxyPreview(parsed);
      setProxyInputError('');
      if (normalize) {
        formApi.current?.setValue?.('endpoint', parsed.endpoint);
        if (parsed.egressType !== profileType) {
          formApi.current?.setValue?.('type', parsed.egressType);
          setProfileType(parsed.egressType);
          setSelectedTemplate(parsed.egressType.startsWith('socks5') ? 'socks5' : 'proxy_url');
        }
      }
    } catch (error) {
      setProxyPreview(null);
      setProxyInputError(error instanceof Error ? error.message : '代理格式无法识别');
    }
  }, [profileType]);

  return (
    <Form
      key={initial.id || 'new-egress-profile'}
      getFormApi={bindFormApi}
      initValues={initial}
      onSubmit={onSubmit}
      labelPosition="top"
      className="pool-egress-wizard"
    >
      <div className="pool-egress-template-grid">
        {EGRESS_TEMPLATES.map((template) => (
          <button
            key={template.id}
            type="button"
            className={cx('pool-egress-template', selectedTemplate === template.id ? 'pool-egress-template--active' : '')}
            onClick={() => applyTemplate(template)}
          >
            <span className="pool-egress-template__name">{template.name}</span>
            <span className="pool-egress-template__desc">{template.desc}</span>
          </button>
        ))}
      </div>

      <div className="pool-egress-form-grid">
        <Form.Input field="id" label="ID" disabled={Boolean(initialValues?.id)} placeholder="egress_xxx" />
        <Form.Input field="name" label="名称" placeholder="cliproxy BR residential" />
      </div>

      <div className="pool-egress-form-grid">
        <Form.Select
          field="type"
          label="类型"
          optionList={TYPE_OPTIONS}
          onChange={(value) => {
            setProfileType(value);
            if (value === 'direct') {
              setAuthMode('');
              formApi.current?.setValue?.('proxy_auth_mode', '');
              formApi.current?.setValue?.('endpoint', '');
            }
          }}
        />
        <Form.Select
          field="proxy_auth_mode"
          label="代理模式"
          optionList={AUTH_MODE_OPTIONS}
          onChange={(value) => {
            const next = value || '';
            setAuthMode(next);
            if (next === 'api_whitelist') {
              setProfileType('http_proxy');
              formApi.current?.setValue?.('type', 'http_proxy');
              formApi.current?.setValue?.('endpoint', '');
            }
          }}
        />
      </div>

      {isApiMode ? (
        <div className="pool-egress-form-grid">
          <Form.Input field="api_base" label="CLIPProxy API Base URL" placeholder="https://api.cliproxy.io" />
          <Form.Input field="proxy_api_key" mode="password" label="CLIPProxy API Key" placeholder="API token" />
          <Form.InputNumber field="api_num" label="提取数量" min={1} max={10} />
          <Form.InputNumber field="api_time" label="轮转分钟" min={1} max={60} />
        </div>
      ) : null}

      {!isDirect && !isApiMode ? (
        <>
          <Form.Input
            field="endpoint"
            label={isSidecar ? 'Sidecar Endpoint' : '住宅代理地址'}
            placeholder={endpointPlaceholder(profileType, authMode)}
            onChange={(value) => { if (!isSidecar) inspectProxyInput(value, false); }}
            onBlur={(event) => { if (!isSidecar) inspectProxyInput(event?.target?.value, true); }}
          />
          {!isSidecar ? (
            <div className="pool-proxy-parser" aria-live="polite">
              <div className="pool-proxy-parser__formats">
                <span>自动识别</span>
                <code>host:port:user:pass</code>
                <code>user:pass@host:port</code>
                <code>host:port@user:pass</code>
                <code>socks5://user:pass@host:port</code>
              </div>
              {proxyPreview ? (
                <div className="pool-proxy-parser__result">
                  <Tag color="green">已解析</Tag>
                  <Typography.Text size="small">{proxyPreview.masked}</Typography.Text>
                </div>
              ) : null}
              {proxyInputError ? <Typography.Text type="danger" size="small">{proxyInputError}</Typography.Text> : null}
            </div>
          ) : null}
        </>
      ) : null}

      {isSidecar ? (
        <Form.Input field="chain_proxy" label="Chain Proxy" placeholder="socks5h://127.0.0.1:40000" />
      ) : null}

      <div className="pool-egress-form-grid">
        <Form.Input field="region" label="地区/国家" placeholder="BR / US / Rand" />
        <Form.Input field="exit_ip" label="出口 IP" placeholder="测试后自动填充" />
        <Form.InputNumber field="max_concurrency" label="并发上限（0=自适应）" min={0} />
      </div>

      <div className="pool-egress-form-actions">
        <Button icon={<IconPulse />} loading={testing} disabled={saving} onClick={testConnection}>测试连接</Button>
        <Button theme="borderless" icon={<IconRefresh />} onClick={() => setShowAdvanced((value) => !value)}>
          {showAdvanced ? '收起高级' : '高级配置'}
        </Button>
        {testResult ? (
          <span className="pool-egress-test-result">
            <Tag color={testResult.warnings?.length ? 'orange' : 'green'}>{testResult.warnings?.length ? '需确认' : '通过'}</Tag>
            <Typography.Text size="small">{testResult.exit_ip || '未知 IP'} · {testResult.region || '未知地区'}</Typography.Text>
          </span>
        ) : null}
      </div>

      {testResult?.warnings?.length ? (
        <div className="pool-egress-test-warning">
          {testResult.warnings.map((warning) => <div key={warning}>{warning}</div>)}
        </div>
      ) : null}

      {showAdvanced ? (
        <div className="pool-egress-advanced">
          <div className="pool-egress-form-grid">
            <Form.Select field="ip_mode" label="IP 模式" optionList={IP_MODE_OPTIONS} />
            <Form.Input field="provider_key" label="服务商标识" placeholder="cliproxy / cuff / warp" />
          </div>
          {!isSidecar ? <Form.Input field="chain_proxy" label="Chain Proxy" placeholder="socks5h://127.0.0.1:40000" /> : null}
          <Form.TextArea field="dynamic_config_json" label="动态代理配置 JSON" autosize placeholder='{ "rotation": "sid" }' />
        </div>
      ) : null}
    </Form>
  );
}
