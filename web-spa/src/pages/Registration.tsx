import React, { useEffect, useState } from 'react';
import { useNavigate } from 'react-router';
import * as PoolUI from '../components/pool/index.jsx';
import { IconRefresh, IconPlay, IconSetting } from '../components/pool/icons.jsx';
import LoadErrorBanner from '../components/LoadErrorBanner.jsx';
import PageHeader from '../components/PageHeader.jsx';
import CopyCodeBlock from '../components/CopyCodeBlock.jsx';
import ResourceTable from '../components/ResourceTable.jsx';
import { MetricRail, TextClamp } from '../components/DisplayPrimitives.jsx';
import { ReadinessPanel, TaskDetailDrawer, TaskProgress } from '../components/WorkflowPrimitives.jsx';
import { showErrorToast } from '../components/ErrorToast.jsx';
import { normalizeApiError } from '../api/errors';
import { t } from '../lib/i18n.js';
import {
  useRegistrationCountriesData, useRegistrationDashboardData, useRegistrationOptionsData,
  useRefreshSMSMarketMutation, useRegistrationStrategyData, useSaveRegistrationStrategyMutation,
  useSMSMarketData, useStartRegistrationJobMutation,
} from '../features/automation/queries/registration';
import {
  lockedIdentityForMethod, manualStartBlockers, methodUsesSMSCountry, normalizeRegisterMethod,
  readinessProviderCount,
} from '../features/automation/model/registration';
import type {
  RegistrationBlocker, RegistrationCountryStrategy, RegistrationIdentityMode,
  RegistrationJob, RegistrationProviderOption, RegistrationStartInput,
} from '../features/automation/model/registration';

const { Button, Toast, Typography, Form, Card, Tag, Select } = PoolUI as any;
const ErrorBanner = LoadErrorBanner as any;
const DataTable = ResourceTable as any;
const SummaryRail = MetricRail as any;
const Clamp = TextClamp as any;
const Readiness = ReadinessPanel as any;
const DetailDrawer = TaskDetailDrawer as any;
const Progress = TaskProgress as any;

const DEFAULT_PREFERRED = ['BR', 'CO', 'PL'];

const REGISTRATION_QUICKSTART = `export POOL_URL='https://POOL_HOST'
export ADMIN_TOKEN='ADMIN_TOKEN'
curl -fsS "$POOL_URL/admin/register/readiness" \\
  -H "Authorization: Bearer $ADMIN_TOKEN" | jq

curl -fsS -X POST "$POOL_URL/admin/register/batch" \\
  -H "Authorization: Bearer $ADMIN_TOKEN" -H 'Content-Type: application/json' \\
  --data '{"count":1,"method":"protocol_v2","identity_mode":"email","registration_egress_pool_id":"pool_registration_residential"}' | jq`;

const ENGINE_GUIDES = [
  { value: 'protocol_v2', name: '协议注册 v2', badge: '推荐', mode: '邮箱 OTP', detail: 'curl_cffi 浏览器指纹 + Sentinel PoW；当前主要协议注册引擎，资源占用低。' },
  { value: 'protocol', name: '内置协议注册', badge: '协议', mode: '邮箱 / 短信', detail: 'Go 原生协议流程，可手动选择邮箱或短信身份；适合兼容性回退。' },
  { value: 'browser_v3', name: '浏览器注册 v3', badge: '真实浏览器', mode: '邮箱优先 · 短信兜底', detail: 'Playwright + 隐身配置；页面变化时更有韧性，资源消耗高于协议引擎。' },
  { value: 'node', name: 'Node 浏览器注册', badge: 'Puppeteer', mode: '短信', detail: '隔离 Chrome 配置与短信中继；每次尝试使用独立浏览器资料和代理出口。' },
  { value: 'browser', name: '旧版浏览器注册', badge: '兼容', mode: '短信', detail: '保留的 Playwright 兼容引擎；仅在新版引擎不适配时使用。' },
];

interface RegistrationFormValues {
  count?: number | string;
  group_name?: string;
  method?: string;
  registration_egress_pool_id?: string;
  sms_provider?: string;
  mailbox_provider?: string;
}

function jobTag(status: unknown) {
  const value = String(status || 'unknown');
  const colors: Record<string, string> = { completed: 'green', running: 'blue', pending: 'grey', cancelled: 'amber', failed: 'red' };
  return <Tag color={colors[value] || 'grey'}>{value}</Tag>;
}

function blockerText(blocker: RegistrationBlocker): string {
  const message = t(`registration.blocker.${blocker.code}`, blocker.code);
  return blocker.detail ? message.replace('{detail}', blocker.detail) : message;
}

function providerOptionValue(option: RegistrationProviderOption): string {
  return typeof option === 'string' ? option : option.value;
}

function providerOptionLabel(option: RegistrationProviderOption): string {
  return typeof option === 'string' ? option : option.label;
}

export function RegistrationJobCard({ job, onOpen }: { job: RegistrationJob; onOpen: () => void }) {
  const group = job.group_name || t('registration.default_group');
  const egress = job.egress_id || t('registration.default_egress');
  return (
    <button
      type="button"
      className="pool-compact-record pool-registration-job-card"
      onClick={onOpen}
      aria-label={`${t('registration.drawer_title')} ${job.id || 'register-job'}`}
    >
      <span className="pool-compact-record__head">
        <span className="pool-compact-record__identity">
          <Clamp strong title={job.id || 'register-job'} ariaLabel={job.id || 'register-job'}>
            {job.id || 'register-job'}
          </Clamp>
          <span className="pool-compact-record__chips">
            <Tag size="small">{job.method || 'node'}</Tag>
            {job.identity_mode ? <Tag size="small" color="blue">{job.identity_mode}</Tag> : null}
          </span>
        </span>
        {jobTag(job.status)}
      </span>
      <Progress task={job} totalKey="total" successKey="succeeded" failedKey="failed" />
      <span className="pool-compact-record__foot">
        <span title={group}>{group}</span>
        <span aria-hidden="true">·</span>
        <span title={egress}>{egress}</span>
        <span className="pool-compact-record__disclosure" aria-hidden="true">›</span>
      </span>
    </button>
  );
}

export default function Registration() {
  const navigate = useNavigate();
  const [detailJob, setDetailJob] = useState<RegistrationJob | null>(null);
  const [strategy, setStrategy] = useState<RegistrationCountryStrategy>('auto');
  const [manualCountry, setManualCountry] = useState('');
  const [savedStrategy, setSavedStrategy] = useState<RegistrationCountryStrategy>('auto');
  const [savedManualCountry, setSavedManualCountry] = useState('');
  const [defaultMethod, setDefaultMethod] = useState('node');
  const [selectedMethod, setSelectedMethod] = useState('');
  const [identityMode, setIdentityMode] = useState<RegistrationIdentityMode>('phone');
  const [minPriceInput, setMinPriceInput] = useState('');
  const [maxPriceInput, setMaxPriceInput] = useState('');
  const [savedMinPrice, setSavedMinPrice] = useState(0);
  const [savedMaxPrice, setSavedMaxPrice] = useState(0);

  const dashboardQuery = useRegistrationDashboardData();
  const optionsQuery = useRegistrationOptionsData();
  const countriesQuery = useRegistrationCountriesData();
  const strategyQuery = useRegistrationStrategyData();
  const smsMarketQuery = useSMSMarketData();
  const refreshSMSMarketMutation = useRefreshSMSMarketMutation();
  const saveStrategyMutation = useSaveRegistrationStrategyMutation();
  const startMutation = useStartRegistrationJobMutation();

  const strategyConfig = strategyQuery.data;
  useEffect(() => {
    if (!strategyConfig) return;
    setStrategy(strategyConfig.strategy);
    setManualCountry(strategyConfig.manualCountry);
    setSavedStrategy(strategyConfig.strategy);
    setSavedManualCountry(strategyConfig.manualCountry);
    setDefaultMethod(strategyConfig.defaultMethod);
    setMinPriceInput(strategyConfig.minPrice > 0 ? String(strategyConfig.minPrice) : '');
    setMaxPriceInput(strategyConfig.maxPrice > 0 ? String(strategyConfig.maxPrice) : '');
    setSavedMinPrice(strategyConfig.minPrice);
    setSavedMaxPrice(strategyConfig.maxPrice);
    setIdentityMode(lockedIdentityForMethod(strategyConfig.defaultMethod) || 'phone');
  }, [strategyConfig]);

  const jobs = dashboardQuery.data?.jobs ?? [];
  const readiness = dashboardQuery.data?.readiness ?? null;
  const groups = optionsQuery.data?.groups ?? [];
  const pools = optionsQuery.data?.pools ?? [];
  const providerOptions = optionsQuery.data?.providers ?? { sms: [], mailbox: [], captcha: [] };
  const countries = countriesQuery.data ?? [];
  const starting = startMutation.isPending;
  const savingStrategy = saveStrategyMutation.isPending;
  const strategyReady = Boolean(strategyConfig || strategyQuery.error);

  const persistStrategy = async (nextStrategy: RegistrationCountryStrategy, manualIso: string, minPrice = Number(minPriceInput) || 0, maxPrice = Number(maxPriceInput) || 0) => {
    const nextManualCountry = nextStrategy === 'manual' ? manualIso : '';
    if (minPrice < 0 || maxPrice < 0 || minPrice > 1000 || maxPrice > 1000 || (minPrice > 0 && maxPrice > 0 && minPrice > maxPrice)) {
      throw new Error('接码价格范围无效：需要 0–1000 USD，且最低价不能高于最高价。');
    }
    if (nextStrategy === savedStrategy && nextManualCountry === savedManualCountry && minPrice === savedMinPrice && maxPrice === savedMaxPrice) return;
    await saveStrategyMutation.mutateAsync({ strategy: nextStrategy, manualCountry: nextManualCountry, minPrice, maxPrice });
    setSavedStrategy(nextStrategy);
    setSavedManualCountry(nextManualCountry);
    setSavedMinPrice(minPrice);
    setSavedMaxPrice(maxPrice);
    Toast.success(t('registration.strategy_saved'));
  };

  const saveSMSPolicy = async () => {
    try {
      await persistStrategy(strategy, manualCountry);
    } catch (error) {
      showErrorToast(error);
    }
  };

  const refreshSMSMarket = async () => {
    try {
      await refreshSMSMarketMutation.mutateAsync(undefined);
      Toast.success('国家价格与库存已重新扫描');
    } catch (error) {
      showErrorToast(error);
    }
  };

  const effectiveMethod = normalizeRegisterMethod(selectedMethod, defaultMethod);
  const activeEngineGuide = ENGINE_GUIDES.find((engine) => engine.value === effectiveMethod) || ENGINE_GUIDES[0];
  const lockedIdentityMode = lockedIdentityForMethod(effectiveMethod);
  const activeIdentityMode: RegistrationIdentityMode = lockedIdentityMode || identityMode;
  const smsCountryRequired = methodUsesSMSCountry(effectiveMethod, activeIdentityMode);
  const blockerModels = manualStartBlockers(readiness, activeIdentityMode, effectiveMethod);
  const blockers = blockerModels.map(blockerText);
  const countryMissing = smsCountryRequired && strategy === 'manual' && !manualCountry;
  const startBlockers = countryMissing ? [...blockers, t('registration.country_required')] : blockers;
  const readinessError = dashboardQuery.data?.readinessError?.userMessage
    || (dashboardQuery.error ? normalizeApiError(dashboardQuery.error).userMessage : '');

  const start = async (values: RegistrationFormValues) => {
    try {
      const requestMethod = values.method || '';
      const selectedEffectiveMethod = normalizeRegisterMethod(requestMethod, defaultMethod);
      const requestIdentityMode: RegistrationIdentityMode = lockedIdentityForMethod(selectedEffectiveMethod) || identityMode || 'phone';
      const requestUsesCountry = methodUsesSMSCountry(selectedEffectiveMethod, requestIdentityMode);
      const requestBlockers = manualStartBlockers(readiness, requestIdentityMode, selectedEffectiveMethod).map(blockerText);
      if (requestBlockers.length) {
        Toast.warning(requestBlockers[0]);
        return;
      }
      if (!strategyReady) {
        Toast.warning(t('registration.strategy_loading'));
        return;
      }
      if (requestUsesCountry && strategy === 'manual' && !manualCountry) {
        Toast.warning(t('registration.country_required'));
        return;
      }
      if (requestUsesCountry) await persistStrategy(strategy, manualCountry);
      const payload: RegistrationStartInput = {
        count: Number(values.count) || 1,
        group_name: values.group_name || '',
        method: requestMethod,
        registration_egress_pool_id: values.registration_egress_pool_id || '',
        sms_provider: values.sms_provider || '',
        mailbox_provider: values.mailbox_provider || '',
        identity_mode: requestIdentityMode,
        country: requestUsesCountry && strategy === 'manual' ? manualCountry : '',
      };
      await startMutation.mutateAsync(payload);
      Toast.success(t('registration.started'));
    } catch (error) {
      showErrorToast(error);
    }
  };

  const columns: any[] = [
    {
      title: t('registration.job'),
      key: 'job',
      width: 280,
      render: (_: unknown, row: RegistrationJob) => (
        <div className="pool-job-cell">
          <Clamp strong>{row.id || 'register-job'}</Clamp>
          <div className="pool-resource-summary__meta">
            <Tag size="small">{row.method || 'node'}</Tag>
            {row.identity_mode ? <Tag size="small" color="blue">{row.identity_mode}</Tag> : null}
          </div>
        </div>
      ),
    },
    {
      title: t('registration.progress'),
      key: 'progress',
      width: 320,
      render: (_: unknown, row: RegistrationJob) => <Progress task={row} totalKey="total" successKey="succeeded" failedKey="failed" />,
    },
    {
      title: t('registration.route'),
      key: 'route',
      width: 220,
      render: (_: unknown, row: RegistrationJob) => (
        <div className="pool-resource-summary">
          <Clamp>{row.group_name || t('registration.default_group')}</Clamp>
          <div className="pool-resource-summary__meta">{row.egress_id || t('registration.default_egress')}</div>
        </div>
      ),
    },
    { title: t('registration.status'), dataIndex: 'status', width: 120, render: jobTag },
  ];

  const jobMetrics = [
    { label: t('registration.jobs'), value: jobs.length },
    { label: t('registration.running'), value: jobs.filter((job) => ['pending', 'running'].includes(job.status || '')).length, tone: 'warning' },
    { label: t('registration.success'), value: jobs.reduce((sum, job) => sum + (Number(job.succeeded) || 0), 0), tone: 'success' },
    { label: t('registration.failed'), value: jobs.reduce((sum, job) => sum + (Number(job.failed) || 0), 0), tone: jobs.some((job) => Number(job.failed) > 0) ? 'danger' : undefined },
  ];
  const countryOptions = countries.map((country) => ({
    label: `${country.isoCode} - ${country.nameZh} (${country.name})`,
    value: country.isoCode,
  }));
  const providerSummary = readiness ? [
    ['mailbox', readinessProviderCount(readiness, 'mailbox')],
    ['email_otp', readinessProviderCount(readiness, 'email_otp')],
    ['sms', readinessProviderCount(readiness, 'sms')],
    ['captcha', readinessProviderCount(readiness, 'captcha')],
  ] : [];
  const registrationPools = pools.filter((pool) => !pool.purpose || pool.purpose === 'registration');
  const smsProviderOptions = [
    { label: t('registration.auto'), value: '' },
    { label: 'SMSBower', value: 'smsbower' },
    { label: 'HeroSMS', value: 'herosms' },
    ...providerOptions.sms
      .filter((option) => !['smsbower', 'herosms'].includes(providerOptionValue(option).toLowerCase()))
      .map((option) => ({ label: providerOptionLabel(option), value: providerOptionValue(option) })),
  ];
  const mailboxProviderOptions = [
    { label: t('registration.mailbox_default'), value: '' },
    ...providerOptions.mailbox.map((option) => ({
      label: providerOptionLabel(option),
      value: providerOptionValue(option),
    })),
  ];
  const smsMarket = smsMarketQuery.data;
  const visibleMarket = (smsMarket?.items || []).slice(0, 8);

  return (
    <div>
      <PageHeader title={t('registration.title')} subtitle={t('registration.subtitle')}
        actions={(
          <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}>
            <Button icon={<IconSetting />} onClick={() => navigate('/settings-v2?tab=registrar')}>{t('registration.credentials')}</Button>
            <Button icon={<IconRefresh />} onClick={dashboardQuery.reload}>{t('common.refresh')}</Button>
          </div>
        )} />

      <ErrorBanner error={optionsQuery.error || optionsQuery.data?.error} onRetry={optionsQuery.reload} title={t('registration.options_failed')} />
      <ErrorBanner error={countriesQuery.error} onRetry={countriesQuery.reload} title={t('registration.countries_failed')} />
      <ErrorBanner error={strategyQuery.error} onRetry={strategyQuery.reload} title={t('registration.strategy_failed')} />

      <Card className="pool-card pool-registration-quickstart" style={{ marginBottom: 18 }}>
        <div className="pool-registration-quickstart__copy">
          <Tag color="blue">最短路径</Tag>
          <h2>代理池 → 自建邮箱 → Provider → 单号 canary</h2>
          <p>页面会把缺项逐条列出。先完成前三项，再运行一号任务；成功后再提高数量或开启自动补池。</p>
          <div>
            <Button size="small" onClick={() => navigate('/egress')}>1. 住宅代理</Button>
            <Button size="small" onClick={() => navigate('/email-pool/cloudflare')}>2. 自建邮箱</Button>
            <Button size="small" onClick={() => navigate('/settings-v2?tab=registrar')}>3. Provider 凭据</Button>
          </div>
        </div>
        <CopyCodeBlock code={REGISTRATION_QUICKSTART} label="复制检查与单号命令" />
      </Card>

      <Card className="pool-card pool-registration-start-card" style={{ marginBottom: 18 }} title={t('registration.start_card')}>
        <div className="pool-registration-start-layout">
          <Readiness
            readiness={readiness}
            readinessError={readinessError}
            blockers={startBlockers}
            providerSummary={providerSummary}
            pool={readiness?.pool || {}}
          />
          <Form layout="horizontal" onSubmit={start} className="pool-registration-start-form">
            <Form.InputNumber field="count" label={t('registration.count')} initValue={1} min={1} max={100} disabled={starting || savingStrategy} />
            <Form.Select field="group_name" label={t('registration.group')} placeholder={t('registration.default')} disabled={starting || savingStrategy}
              optionList={[{ label: t('registration.default'), value: '' }, ...groups.map((group) => ({ label: group.name || t('registration.unknown'), value: group.name || '' }))]} />
            <Form.Select field="registration_egress_pool_id" label={t('registration.proxy_pool')} disabled={starting || savingStrategy}
              optionList={[
                { label: t('registration.use_egress_default'), value: '' },
                ...registrationPools.map((pool) => ({ label: `${pool.name || pool.id} (${pool.members?.length || 0})`, value: pool.id })),
              ]} />
            <Form.Select field="method" label={t('registration.engine')} initValue="" disabled={starting || savingStrategy}
              optionList={[
                { label: `${t('registration.default')} · ${ENGINE_GUIDES.find((engine) => engine.value === defaultMethod)?.name || defaultMethod}`, value: '' },
                ...ENGINE_GUIDES.map((engine) => ({ label: `${engine.name} · ${engine.mode}`, value: engine.value })),
              ]}
              onChange={(value: string) => {
                const nextMethod = value || '';
                const nextEffectiveMethod = normalizeRegisterMethod(nextMethod, defaultMethod);
                setSelectedMethod(nextMethod);
                setIdentityMode(lockedIdentityForMethod(nextEffectiveMethod) || identityMode || 'phone');
              }} />
            <div className="pool-registration-engine-summary" aria-live="polite">
              <span className="pool-registration-engine-summary__icon" aria-hidden="true">⌁</span>
              <span className="pool-registration-engine-summary__body">
                <span className="pool-registration-engine-summary__title">
                  {activeEngineGuide.name}
                  <Tag size="small" color={activeEngineGuide.value === 'protocol_v2' ? 'blue' : 'grey'}>{activeEngineGuide.badge}</Tag>
                  <Tag size="small">{activeEngineGuide.mode}</Tag>
                </span>
                <span>{activeEngineGuide.detail}</span>
              </span>
            </div>
            <div className="pool-registration-control">
              <Typography.Text size="small" className="pool-registration-field-label">{t('registration.identity')}</Typography.Text>
              <Select
                value={activeIdentityMode}
                aria-label={t('registration.identity')}
                disabled={Boolean(lockedIdentityMode) || starting || savingStrategy}
                optionList={[
                  { label: t('registration.email_identity'), value: 'email' },
                  { label: t('registration.phone_identity'), value: 'phone' },
                ]}
                onChange={(value: RegistrationIdentityMode) => setIdentityMode(value || 'phone')}
              />
            </div>

            {activeIdentityMode === 'email' ? (
              <Form.Select
                field="mailbox_provider"
                label={t('registration.mailbox_provider')}
                initValue=""
                disabled={starting || savingStrategy}
                optionList={mailboxProviderOptions}
              />
            ) : null}

            {smsCountryRequired ? (
              <div className="pool-registration-strategy-row">
                <Form.Select field="sms_provider" label={t('registration.sms_platform')} initValue="" disabled={starting || savingStrategy}
                  optionList={smsProviderOptions} />
                <div className="pool-registration-control">
                  <Typography.Text size="small" className="pool-registration-field-label">{t('registration.country_strategy')}</Typography.Text>
                  <Select value={strategy} aria-label={t('registration.country_strategy')} onChange={(value: RegistrationCountryStrategy) => {
                    const nextStrategy = value || 'auto';
                    setStrategy(nextStrategy);
                    if (nextStrategy === 'auto') setManualCountry('');
                    else if (!manualCountry && savedManualCountry) setManualCountry(savedManualCountry);
                  }} disabled={starting || savingStrategy}
                    optionList={[
                      { label: t('registration.recommended'), value: 'auto' },
                      { label: t('registration.manual_country'), value: 'manual' },
                    ]} />
                </div>
                {strategy === 'manual' ? (
                  <div className="pool-registration-control">
                    <Typography.Text size="small" className="pool-registration-field-label">{t('registration.manual_country')}</Typography.Text>
                    <Select
                      value={manualCountry} onChange={(value: string) => setManualCountry(value)}
                      aria-label={t('registration.manual_country')}
                      disabled={starting || savingStrategy}
                      placeholder={t('registration.country_search')}
                      optionList={countryOptions}
                      filter
                      emptyContent={t('registration.country_empty')}
                    />
                  </div>
                ) : (
                  <div className="pool-registration-strategy-note">
                    <Typography.Text size="small" type="secondary" className="pool-registration-strategy-note-main">
                      {t('registration.auto_country')}
                    </Typography.Text>
                    <span className="pool-registration-strategy-note-sub">{t('registration.auto_country_desc')}</span>
                    <Tag size="small">{t('registration.recommend_short')} {DEFAULT_PREFERRED.join(' > ')}</Tag>
                  </div>
                )}
              </div>
            ) : null}

            <div className="pool-registration-actions">
              <Button htmlType="submit" theme="solid" icon={<IconPlay />} loading={starting || savingStrategy}
                disabled={starting || savingStrategy || Boolean(readinessError) || !readiness || !strategyReady || startBlockers.length > 0}>{t('common.start')}</Button>
              {blockers.length > 0 ? (
                <Button icon={<IconSetting />} onClick={() => navigate('/settings-v2?tab=registrar')}>
                  {t('registration.configure')}
                </Button>
              ) : null}
            </div>
          </Form>
        </div>
      </Card>

      <ErrorBanner error={smsMarketQuery.error} onRetry={smsMarketQuery.reload} title="接码国家市场读取失败" />
      <Card className="pool-card pool-sms-market-card" style={{ marginBottom: 18 }} title="接码国家智能选择">
        <div className="pool-sms-market-head">
          <div>
            <Typography.Text strong>成功率优先，价格与库存共同决策</Typography.Text>
            <p className="pool-sms-market-description">
              系统每小时遍历各平台国家价格；累计至少 {smsMarket?.minimum_history_samples || 3} 次后按近 {smsMarket?.history_window_days || 14} 天成功率选择。冷启动按社区推荐 {((smsMarket?.preferred_countries || DEFAULT_PREFERRED).join(' › '))}。
            </p>
          </div>
          <div className="pool-sms-market-actions">
            <Button icon={<IconRefresh />} loading={refreshSMSMarketMutation.isPending} onClick={refreshSMSMarket}>立即比价</Button>
          </div>
        </div>
        <div className="pool-sms-price-policy">
          <label>
            <span>最低单价 · USD</span>
            <input className="pool-input" type="number" min="0" max="1000" step="0.001" placeholder="不限" value={minPriceInput} onChange={(event) => setMinPriceInput(event.target.value)} />
          </label>
          <label>
            <span>最高单价 · USD</span>
            <input className="pool-input" type="number" min="0" max="1000" step="0.001" placeholder="不限" value={maxPriceInput} onChange={(event) => setMaxPriceInput(event.target.value)} />
          </label>
          <Button theme="solid" loading={savingStrategy} onClick={saveSMSPolicy}>保存价格范围</Button>
          <span className="pool-sms-market-freshness">
            {smsMarket?.last_refreshed_at
              ? `${smsMarket.stale ? '数据待刷新' : '价格已同步'} · ${new Date(smsMarket.last_refreshed_at * 1000).toLocaleString()}`
              : '等待首次价格扫描'}
          </span>
        </div>
        <div className="pool-sms-market-grid">
          {visibleMarket.map((item, index) => (
            <article className={`pool-sms-market-item${item.eligible ? '' : ' is-ineligible'}`} key={`${item.provider}-${item.country_id}`}>
              <span className="pool-sms-market-rank">{index + 1}</span>
              <div className="pool-sms-market-country">
                <strong>{item.country_iso || item.country_id}</strong>
                <span title={item.country_name || item.provider}>{item.country_name || item.provider}</span>
              </div>
              <div className="pool-sms-market-stat"><span>成功率</span><strong>{Math.round(item.success_rate * 100)}%</strong><small>{item.attempts ? `${item.succeeded}/${item.attempts}` : '冷启动'}</small></div>
              <div className="pool-sms-market-stat"><span>单价</span><strong>${item.price.toFixed(3)}</strong><small>库存 {item.inventory}</small></div>
              <Tag size="small" color={item.selection_basis === 'historical_success_rate' ? 'green' : 'blue'}>
                {item.selection_basis === 'historical_success_rate' ? '历史概率' : '社区推荐'}
              </Tag>
            </article>
          ))}
          {!smsMarketQuery.loading && visibleMarket.length === 0 ? (
            <div className="pool-sms-market-empty">保存接码平台凭据后点击“立即比价”，国家价格、库存与历史成功率会显示在这里。</div>
          ) : null}
        </div>
      </Card>

      <div className="pool-toolbar pool-registration-toolbar">
        <Button icon={<IconRefresh />} onClick={dashboardQuery.reload}>{t('common.refresh')}</Button>
        <Typography.Text type="tertiary">{t('registration.output_note')}</Typography.Text>
      </div>
      <div className="pool-resource-split">
        <DataTable
          error={dashboardQuery.error}
          onRetry={dashboardQuery.reload}
          loading={dashboardQuery.loading}
          lastRefresh={dashboardQuery.lastRefresh}
          dataSource={jobs}
          columns={columns}
          rowKey={(row: RegistrationJob, index: number) => row.id || `registration-${index}`}
          pagination={{ pageSize: 15 }}
          className="pool-registration-jobs"
          density="compact"
          scroll={false}
          rowHeight={64}
          emptyTitle={t('registration.empty')}
          emptyDesc={t('registration.empty_desc')}
          emptyType="refresh"
          skeletonRows={6}
          skeletonCols={4}
          onRow={(row: RegistrationJob) => ({ onClick: () => setDetailJob(row) })}
          mobileRenderer={(row: RegistrationJob) => (
            <RegistrationJobCard job={row} onOpen={() => setDetailJob(row)} />
          )}
          mobileListLabel={t('registration.jobs')}
        />
        {!dashboardQuery.error || dashboardQuery.lastRefresh ? <SummaryRail items={jobMetrics} className="pool-registration-metrics" /> : null}
      </div>
      <DetailDrawer
        task={detailJob}
        visible={Boolean(detailJob)}
        onClose={() => setDetailJob(null)}
        title={detailJob ? `${t('registration.drawer_title')} · ${detailJob.id || 'register-job'}` : t('registration.drawer_title')}
        status={detailJob ? jobTag(detailJob.status) : null}
      />
    </div>
  );
}
