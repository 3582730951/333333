import React, { useState, useEffect, useCallback } from 'react';
import { Button, Toast, Typography, Form, Card, Tag, Select } from '../components/pool/index.jsx';
import { IconRefresh, IconPlay, IconSetting } from '../components/pool/icons.jsx';
import { useNavigate } from 'react-router-dom';
import { get, post, errMsg } from '../api.js';
import LoadErrorBanner from '../components/LoadErrorBanner.jsx';
import PageHeader from '../components/PageHeader.jsx';
import ResourceTable from '../components/ResourceTable.jsx';
import { MetricRail, TextClamp } from '../components/DisplayPrimitives.jsx';
import { ReadinessPanel, TaskDetailDrawer, TaskProgress } from '../components/WorkflowPrimitives.jsx';
import { showErrorToast } from '../components/ErrorToast.jsx';
import useAsyncAction from '../hooks/useAsyncAction.js';
import useAsyncResource from '../hooks/useAsyncResource.js';
import useVisibleInterval from '../hooks/useVisibleInterval.js';
import { loadResourceGroup } from '../lib/resource.js';

const jobTag = (s) => {
  const map = { completed: 'green', running: 'blue', pending: 'grey', cancelled: 'amber', failed: 'red' };
  return <Tag color={map[s] || 'grey'}>{s}</Tag>;
};

const DEFAULT_PREFERRED = ['BR', 'CO', 'PL'];
const EMPTY_REGISTRATION = { jobs: [], readiness: null, readinessError: '' };
const EMPTY_OPTIONS = { groups: [], egresses: [], pools: [], providerOpts: { sms: [], mailbox: [], captcha: [] }, error: null };

const readinessProviderCount = (readiness, key) => Number(readiness?.providers?.[key] || 0);

const normalizeRegisterMethod = (method, fallback = 'node') => String(method || fallback || 'node').trim().toLowerCase();

const lockedIdentityForMethod = (method) => {
  switch (normalizeRegisterMethod(method)) {
    case 'node':
    case 'browser':
      return 'phone';
    case 'protocol_v2':
    case 'browser_v3':
      return 'email';
    default:
      return '';
  }
};

const methodUsesSMSCountry = (method, identityMode) => {
  const m = normalizeRegisterMethod(method);
  return m === 'node' || m === 'browser' || m === 'browser_v3' || (m === 'protocol' && identityMode === 'phone');
};

const methodRequiresSMSProvider = (method, identityMode) => {
  const m = normalizeRegisterMethod(method);
  return m === 'node' || m === 'browser' || (m === 'protocol' && identityMode === 'phone');
};

const methodRequiresMailboxProvider = (method, identityMode) => normalizeRegisterMethod(method) === 'protocol' && identityMode === 'email';

const methodRequiresEmailOTPProvider = (method) => ['protocol_v2', 'browser_v3'].includes(normalizeRegisterMethod(method));

const manualStartBlockers = (readiness, identityMode, method) => {
  if (!readiness) return [];
  const blockers = [];
  if (readiness.provider_error) blockers.push(`注册 Provider 异常: ${readiness.provider_error}`);
  if ((readiness.blockers || []).some((b) => String(b).includes('注册子系统'))) blockers.push('注册子系统未初始化');
  if (methodRequiresMailboxProvider(method, identityMode) && readinessProviderCount(readiness, 'mailbox') < 1) blockers.push('邮箱注册缺少 mailbox provider');
  if (methodRequiresEmailOTPProvider(method) && readinessProviderCount(readiness, 'email_otp') < 1) blockers.push('邮箱注册缺少 hotmail_otp provider');
  if (methodRequiresSMSProvider(method, identityMode) && readinessProviderCount(readiness, 'sms') < 1) blockers.push('短信注册缺少 SMS provider');
  return [...new Set(blockers)];
};

export default function Registration() {
  const navigate = useNavigate();
  const [detailJob, setDetailJob] = useState(null);
  // Country strategy & reference data
  const [strategy, setStrategy] = useState('auto'); // "auto" | "manual"
  const [manualCountry, setManualCountry] = useState('');
  const [savedStrategy, setSavedStrategy] = useState('auto');
  const [savedManualCountry, setSavedManualCountry] = useState('');
  const [defaultMethod, setDefaultMethod] = useState('node');
  const [selectedMethod, setSelectedMethod] = useState('');
  const [identityMode, setIdentityMode] = useState('phone');

  const fetchRegistration = useCallback(async ({ signal }) => {
    const { values, error } = await loadResourceGroup({
      jobs: { label: '注册任务', load: () => get('/admin/register/batch', undefined, { signal }) },
      readiness: { label: '依赖检查', load: () => get('/admin/register/readiness', undefined, { signal }) },
    });
    if (error?.failures?.some((failure) => failure.key === 'jobs')) throw error;
    const readinessFailure = error?.failures?.find((failure) => failure.key === 'readiness');
    return {
      jobs: Array.isArray(values.jobs) ? values.jobs : values.jobs?.jobs || [],
      readiness: values.readiness || null,
      readinessError: readinessFailure ? errMsg(readinessFailure.error) : '',
    };
  }, []);

  const {
    data: registration = EMPTY_REGISTRATION,
    loading,
    error,
    lastRefresh,
    reload: load,
  } = useAsyncResource(fetchRegistration, [fetchRegistration], { initialData: EMPTY_REGISTRATION });

  useVisibleInterval(load, 5000);

  // Load group + egress options for the Select dropdowns.
  const fetchOptions = useCallback(async ({ signal }) => {
    const { values, error } = await loadResourceGroup({
      groups: { label: '分组选项', load: () => get('/admin/groups', undefined, { signal }) },
      pools: { label: '注册池选项', load: () => get('/admin/egress-pools', undefined, { signal }) },
      providers: { label: '服务商选项', load: () => get('/admin/register/providers/options', undefined, { signal }) },
    });
    return {
      groups: Array.isArray(values.groups) ? values.groups : values.groups?.groups || [],
      pools: Array.isArray(values.pools) ? values.pools : values.pools?.pools || [],
      providerOpts: values.providers || EMPTY_OPTIONS.providerOpts,
      error,
    };
  }, []);

  const {
    data: options = EMPTY_OPTIONS,
    error: optionsError,
    reload: reloadOptions,
  } = useAsyncResource(fetchOptions, [fetchOptions], { initialData: EMPTY_OPTIONS });

  const fetchCountries = useCallback(async ({ signal }) => {
    const list = await get('/admin/register/countries', undefined, { signal });
    return Array.isArray(list) ? list : [];
  }, []);

  const {
    data: countries = [],
    error: countriesError,
    reload: loadCountries,
  } = useAsyncResource(fetchCountries, [fetchCountries], { initialData: [], keepDataOnError: false });

  const fetchStrategyConfig = useCallback(async ({ signal }) => {
    const cfg = await get('/admin/config', undefined, { signal });
    const byKey = {};
    (Array.isArray(cfg) ? cfg : []).forEach((f) => { byKey[f.key] = f.value; });
    const nextStrategy = byKey.sms_platform_strategy === 'manual' ? 'manual' : 'auto';
    return {
      strategy: nextStrategy,
      manualCountry: nextStrategy === 'manual' ? (byKey.sms_manual_country || '') : '',
      defaultMethod: normalizeRegisterMethod(byKey.default_register_method, 'node'),
    };
  }, []);

  const {
    data: strategyConfig,
    error: strategyError,
    reload: loadStrategyConfig,
  } = useAsyncResource(fetchStrategyConfig, [fetchStrategyConfig], { initialData: null });

  useEffect(() => {
    if (!strategyConfig) return;
    setStrategy(strategyConfig.strategy);
    setManualCountry(strategyConfig.manualCountry);
    setSavedStrategy(strategyConfig.strategy);
    setSavedManualCountry(strategyConfig.manualCountry);
    setDefaultMethod(strategyConfig.defaultMethod);
    setIdentityMode(lockedIdentityForMethod(strategyConfig.defaultMethod) || 'phone');
  }, [strategyConfig]);

  const { run: saveStrategy, running: savingStrategy } = useAsyncAction(async (strat, manualIso) => {
    const nextStrategy = strat === 'manual' ? 'manual' : 'auto';
    const nextManualCountry = nextStrategy === 'manual' ? (manualIso || '') : '';
    if (nextStrategy === savedStrategy && nextManualCountry === savedManualCountry) return;
    const patch = [
      { section: 'config', values: {
        sms_platform_strategy: nextStrategy,
        sms_manual_country: nextManualCountry,
      }},
    ];
    await post('/admin/settings-center', patch);
    setSavedStrategy(nextStrategy);
    setSavedManualCountry(nextManualCountry);
    Toast.success('国家策略已保存');
  });

  const { run: start, running: starting } = useAsyncAction(async (v) => {
    try {
      const requestMethod = v.method || '';
      const effectiveMethod = normalizeRegisterMethod(requestMethod, defaultMethod);
      const requestIdentityMode = lockedIdentityForMethod(effectiveMethod) || identityMode || 'phone';
      const smsCountryRequired = methodUsesSMSCountry(effectiveMethod, requestIdentityMode);
      const blockers = manualStartBlockers(readiness, requestIdentityMode, effectiveMethod);
      if (blockers.length) {
        Toast.warning(blockers[0]);
        return;
      }
      if (!strategyReady) {
        Toast.warning('国家策略正在读取中');
        return;
      }
      if (smsCountryRequired && strategy === 'manual' && !manualCountry) {
        Toast.warning('指定国家模式需要选择国家');
        return;
      }
      if (smsCountryRequired) await saveStrategy(strategy, manualCountry);
      const payload = {
        count: Number(v.count) || 1,
        group_name: v.group_name || '',
        method: requestMethod,
        registration_egress_pool_id: v.registration_egress_pool_id || '',
        sms_provider: v.sms_provider || '',
        identity_mode: requestIdentityMode,
        country: smsCountryRequired && strategy === 'manual' ? (manualCountry || '') : '',
      };
      await post('/admin/register/batch', payload);
      Toast.success('注册任务已启动');
      await load();
    } catch (e) { showErrorToast(e); }
  });

  const columns = [
    {
      title: '任务',
      key: 'job',
      width: 280,
      render: (_, row) => (
        <div className="pool-job-cell">
          <TextClamp strong>{row.id || 'register-job'}</TextClamp>
          <div className="pool-resource-summary__meta">
            <Tag size="small">{row.method || 'node'}</Tag>
            {row.identity_mode ? <Tag size="small" color="blue">{row.identity_mode}</Tag> : null}
          </div>
        </div>
      ),
    },
    {
      title: '进度',
      key: 'progress',
      width: 320,
      render: (_, row) => <TaskProgress task={row} totalKey="total" successKey="succeeded" failedKey="failed" />,
    },
    {
      title: '路由',
      key: 'route',
      width: 220,
      render: (_, row) => (
        <div className="pool-resource-summary">
          <TextClamp>{row.group_name || '默认分组'}</TextClamp>
          <div className="pool-resource-summary__meta">{row.egress_id || '默认出口'}</div>
        </div>
      ),
    },
    { title: '状态', dataIndex: 'status', width: 120, render: jobTag },
  ];

  const jobs = registration.jobs || [];
  const jobMetrics = [
    { label: '任务数', value: jobs.length },
    { label: '运行中', value: jobs.filter((job) => ['pending', 'running'].includes(job.status)).length, tone: 'warning' },
    { label: '成功', value: jobs.reduce((sum, job) => sum + (Number(job.succeeded) || 0), 0), tone: 'success' },
    { label: '失败', value: jobs.reduce((sum, job) => sum + (Number(job.failed) || 0), 0), tone: jobs.some((job) => Number(job.failed) > 0) ? 'danger' : undefined },
  ];
  const readiness = registration.readiness;
  const readinessError = registration.readinessError || '';
  const groups = options.groups || [];
  const pools = options.pools || [];
  const providerOpts = options.providerOpts || EMPTY_OPTIONS.providerOpts;
  const strategyReady = Boolean(strategyConfig || strategyError);

  // Build country option list for the searchable Select.
  const countryOpts = countries.map((c) => ({
    label: `${c.isoCode} - ${c.nameZh} (${c.name})`,
    value: c.isoCode,
  }));
  const effectiveMethod = normalizeRegisterMethod(selectedMethod, defaultMethod);
  const lockedIdentityMode = lockedIdentityForMethod(effectiveMethod);
  const activeIdentityMode = lockedIdentityMode || identityMode;
  const smsCountryRequired = methodUsesSMSCountry(effectiveMethod, activeIdentityMode);
  const blockers = manualStartBlockers(readiness, activeIdentityMode, effectiveMethod);
  const countryMissing = smsCountryRequired && strategy === 'manual' && !manualCountry;
  const startBlockers = countryMissing ? [...blockers, '指定国家模式需要选择国家'] : blockers;
  const providerSummary = readiness ? [
    ['mailbox', readinessProviderCount(readiness, 'mailbox')],
    ['email_otp', readinessProviderCount(readiness, 'email_otp')],
    ['sms', readinessProviderCount(readiness, 'sms')],
    ['captcha', readinessProviderCount(readiness, 'captcha')],
  ] : [];
  const pool = readiness?.pool || {};
  const registrationPools = pools.filter((p) => !p.purpose || p.purpose === 'registration');
  const smsProviderOptions = [
    { label: '自动', value: '' },
    { label: 'SMSBower', value: 'smsbower' },
    { label: 'HeroSMS', value: 'herosms' },
    ...(providerOpts.sms || [])
      .filter((opt) => !['smsbower', 'herosms'].includes(String(opt.value || '').toLowerCase()))
      .map((opt) => ({ label: opt.label, value: opt.value })),
  ];

  return (
    <div>
      <PageHeader title="自动注册" subtitle="发起注册任务、选择国家策略并查看注册进度"
        actions={(
          <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}>
            <Button icon={<IconSetting />} onClick={() => navigate('/settings-v2#registrar')}>注册器凭据</Button>
            <Button icon={<IconRefresh />} onClick={load}>刷新</Button>
          </div>
        )} />

      <LoadErrorBanner error={error} onRetry={load} />
      <LoadErrorBanner error={optionsError || options.error} onRetry={reloadOptions} title="注册表单选项读取失败" />
      <LoadErrorBanner error={countriesError} onRetry={loadCountries} title="国家列表读取失败" />
      <LoadErrorBanner error={strategyError} onRetry={loadStrategyConfig} title="注册策略读取失败" />

      <Card className="pool-card pool-registration-start-card" style={{ marginBottom: 18 }} title="启动注册任务">
        <div className="pool-registration-start-layout">
          <ReadinessPanel
            readiness={readiness}
            readinessError={readinessError}
            blockers={startBlockers}
            providerSummary={providerSummary}
            pool={pool}
          />
        <Form layout="horizontal" onSubmit={start} className="pool-registration-start-form">
          <Form.InputNumber field="count" label="数量" initValue={1} min={1} max={100} disabled={starting || savingStrategy} />
          <Form.Select field="group_name" label="分组" placeholder="默认" disabled={starting || savingStrategy}
            optionList={[{ label: '默认', value: '' }, ...(groups || []).map((g) => ({ label: g?.name || '未知', value: g?.name || '' }))]} />
          <Form.Select field="registration_egress_pool_id" label="注册代理池" disabled={starting || savingStrategy}
            optionList={[
              { label: '使用出口页默认', value: '' },
              ...(registrationPools || []).map((p) => ({ label: `${p.name || p.id} (${p.members?.length || 0})`, value: p.id })),
            ]} />
          <Form.Select field="method" label="引擎" initValue="" disabled={starting || savingStrategy}
            optionList={[
              { label: `默认 (${defaultMethod})`, value: '' },
              { label: 'protocol', value: 'protocol' },
              { label: 'node (puppeteer)', value: 'node' },
              { label: 'protocol_v2', value: 'protocol_v2' },
              { label: 'browser', value: 'browser' },
              { label: 'browser_v3', value: 'browser_v3' },
            ]}
            onChange={(v) => {
              const nextMethod = v || '';
              const nextEffectiveMethod = normalizeRegisterMethod(nextMethod, defaultMethod);
              setSelectedMethod(nextMethod);
              setIdentityMode(lockedIdentityForMethod(nextEffectiveMethod) || identityMode || 'phone');
            }} />
          <div className="pool-registration-control">
            <Typography.Text size="small" className="pool-registration-field-label">身份</Typography.Text>
            <Select
              value={activeIdentityMode}
              disabled={!!lockedIdentityMode || starting || savingStrategy}
              optionList={[
                { label: '邮箱验证', value: 'email' },
                { label: '短信验证', value: 'phone' },
              ]}
              onChange={(v) => setIdentityMode(v || 'phone')}
            />
          </div>

          {smsCountryRequired && (
          <div className="pool-registration-strategy-row">
            <Form.Select field="sms_provider" label="SMS 平台" initValue="" disabled={starting || savingStrategy}
              optionList={smsProviderOptions} />
            <div className="pool-registration-control">
              <Typography.Text size="small" className="pool-registration-field-label">国家策略</Typography.Text>
              <Select value={strategy} onChange={(v) => {
                const nextStrategy = v || 'auto';
                setStrategy(nextStrategy);
                if (nextStrategy === 'auto') {
                  setManualCountry('');
                } else if (!manualCountry && savedManualCountry) {
                  setManualCountry(savedManualCountry);
                }
              }} disabled={starting || savingStrategy}
                optionList={[
                  { label: '默认推荐', value: 'auto' },
                  { label: '指定国家', value: 'manual' },
                ]} />
            </div>
            {strategy === 'manual' && (
              <div className="pool-registration-control">
                <Typography.Text size="small" className="pool-registration-field-label">指定国家</Typography.Text>
                <Select
                  value={manualCountry} onChange={(v) => setManualCountry(v)}
                  disabled={starting || savingStrategy}
                  placeholder="搜索并选择国家（中英文）"
                  optionList={countryOpts}
                  filter
                  emptyContent="未找到匹配的国家"
                />
              </div>
            )}
            {strategy === 'auto' && (
              <div className="pool-registration-strategy-note">
                <Typography.Text size="small" type="secondary" className="pool-registration-strategy-note-main">
                  自动选择最优国家
                </Typography.Text>
                <span className="pool-registration-strategy-note-sub">综合成功率、价格、库存</span>
                <Tag size="small">推荐 {DEFAULT_PREFERRED.join(' > ')}</Tag>
              </div>
            )}
          </div>
          )}

          <div className="pool-registration-actions">
            <Button htmlType="submit" theme="solid" icon={<IconPlay />} loading={starting || savingStrategy}
              disabled={starting || savingStrategy || !!readinessError || !readiness || !strategyReady || startBlockers.length > 0}>启动</Button>
            {blockers.length > 0 && (
              <Button icon={<IconSetting />} onClick={() => navigate('/settings-v2#registrar')}>
                处理配置
              </Button>
            )}
          </div>
        </Form>
        </div>
      </Card>

      <div className="pool-toolbar pool-registration-toolbar">
        <Button icon={<IconRefresh />} onClick={load}>刷新</Button>
        <Typography.Text type="tertiary">注册成功后产出 auth.json，自动入池。</Typography.Text>
      </div>
      <div className="pool-resource-split">
        <ResourceTable
          loading={loading}
          lastRefresh={lastRefresh}
          dataSource={jobs}
          columns={columns}
          rowKey="id"
          pagination={{ pageSize: 15 }}
          className="pool-registration-jobs"
          density="compact"
          layout="fit"
          scroll={false}
          rowHeight={64}
          emptyTitle="暂无注册任务"
          emptyDesc="通过上方表单启动后会显示进度"
          emptyType="refresh"
          skeletonRows={6}
          skeletonCols={4}
          onRow={(row) => ({ onClick: () => setDetailJob(row) })}
        />
        <MetricRail items={jobMetrics} />
      </div>
      <TaskDetailDrawer
        task={detailJob}
        visible={!!detailJob}
        onClose={() => setDetailJob(null)}
        title={detailJob ? `注册任务 · ${detailJob.id || 'register-job'}` : '注册任务'}
        status={detailJob ? jobTag(detailJob.status) : null}
      />
    </div>
  );
}
