import React, { useEffect, useState } from 'react';
import { useNavigate } from 'react-router';
import { Button, Toast, Typography, Form, Card, Tag, Select } from '../components/pool/index.jsx';
import { IconRefresh, IconPlay, IconSetting } from '../components/pool/icons.jsx';
import LoadErrorBanner from '../components/LoadErrorBanner.jsx';
import PageHeader from '../components/PageHeader.jsx';
import CopyCodeBlock from '../components/CopyCodeBlock.jsx';
import ResourceTable from '../components/ResourceTable.jsx';
import { MetricRail, TextClamp } from '../components/DisplayPrimitives.jsx';
import { ReadinessPanel, TaskDetailDrawer, TaskProgress } from '../components/WorkflowPrimitives.jsx';
import * as MicroCharts from '../components/MicroCharts.jsx';
import { showErrorToast } from '../components/ErrorToast.jsx';
import { normalizeApiError } from '../api/errors';
import { COLORS } from '../lib/chartTheme.js';
import { fmtDateTime, fmtDuration, fmtInt } from '../lib/format.js';
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
  RegistrationJob, RegistrationProviderOption, RegistrationStartInput, SMSMarketCandidate,
} from '../features/automation/model/registration';

const { RankedBars, StackedMeter } = MicroCharts as any;
const C = COLORS;
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

// Every status api/registration.go can persist. `queued` is what HandleRegisterBatch writes on
// insert and `completed_with_review` is what a partially-successful batch settles to -- both were
// missing here, so a job read as an untranslated grey `queued` for its first seconds and a batch
// that half-succeeded reported the same neutral grey as one that had not started.
const JOB_STATUS_COLOR: Record<string, string> = {
  queued: 'grey',
  pending: 'grey',
  running: 'blue',
  completed: 'green',
  completed_with_review: 'amber',
  cancelled: 'grey',
  failed: 'red',
};
// Statuses that mean the job still has work ahead of it. `queued` belongs here: it is the state
// every job passes through on insert, so counting only pending/running showed 0 in flight for the
// first seconds after pressing start -- exactly when the operator is watching.
const ACTIVE_JOB_STATUSES = new Set(['queued', 'pending', 'running']);

function jobStatusLabel(status: unknown): string {
  const value = String(status || 'unknown');
  return t(`registration.job_status.${value}`, value);
}

function jobTag(status: unknown) {
  const value = String(status || 'unknown');
  return <Tag color={JOB_STATUS_COLOR[value] || 'grey'}>{jobStatusLabel(value)}</Tag>;
}

// How long the batch ran, or has been running. `started_at` and `completed_at` are 0 until the
// job reaches each state, so an unstarted job has no duration to report rather than one measured
// from the epoch.
//
// fmtDuration floors to whole minutes, which is right for module uptime but wrong here: a batch
// that has been running 40 seconds would read `0分`, and a just-pressed start is exactly when this
// column is being watched. Under a minute is reported in seconds instead.
function jobDurationText(job: RegistrationJob): string {
  const started = Number(job.started_at) || 0;
  if (!started) return '—';
  const completed = Number(job.completed_at) || 0;
  const until = completed || Math.floor(Date.now() / 1000);
  const seconds = Math.max(0, until - started);
  return seconds < 60 ? `${seconds}${t('registration.seconds_unit')}` : fmtDuration(seconds);
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
  // Same substitution as the desktop table's 路由 column: group and egress are never present on a
  // job from HandleJobList, so this foot was two constants on every card. Duration and start time
  // are what the payload actually carries.
  const duration = jobDurationText(job);
  const startedText = fmtDateTime(Number(job.started_at) || Number(job.created_at) || 0);
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
      {job.error ? <span className="pool-registration-job-card__error" title={String(job.error)}>{String(job.error)}</span> : null}
      <span className="pool-compact-record__foot">
        <span title={duration}>{duration}</span>
        <span aria-hidden="true">·</span>
        <span title={startedText}>{startedText}</span>
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
      throw new Error(t('registration.market_price_invalid'));
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
      Toast.success(t('registration.market_refreshed'));
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
      width: 240,
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
      width: 260,
      render: (_: unknown, row: RegistrationJob) => <Progress task={row} totalKey="total" successKey="succeeded" failedKey="failed" />,
    },
    // This column used to be 路由 (group + egress). HandleJobList's `job` struct marshals neither
    // field, so both sides printed their fallback on every row -- 220px repeating 默认分组 / 默认出口
    // down the whole table. What the endpoint does return, and nothing rendered, is the timing:
    // when the batch started and how long it took (or has been running).
    {
      title: t('registration.duration'),
      key: 'timing',
      width: 160,
      render: (_: unknown, row: RegistrationJob) => (
        <div className="pool-resource-summary">
          <Clamp>{jobDurationText(row)}</Clamp>
          <div className="pool-resource-summary__meta">{fmtDateTime(Number(row.started_at) || Number(row.created_at) || 0)}</div>
        </div>
      ),
    },
    {
      title: t('registration.status'),
      dataIndex: 'status',
      width: 140,
      // The four columns declared 950px against a ~875px pane, so folding dropped the right-most
      // one -- status -- and the whole status vocabulary went behind a per-row expander at
      // 1440x900. Trimming the other three to 800px total makes them all fit, and the priority
      // makes 耗时 fold first at 1280 rather than the column carrying whether the job worked.
      priority: 10,
      render: (value: string | undefined, row: RegistrationJob) => (
        <div className="pool-resource-summary">
          {jobTag(value)}
          {/* A failed job's reason is in the payload and appeared nowhere in the UI, so the table
              said only "red" and the drawer repeated it. One line here, in full in the drawer. */}
          {row.error ? <div className="pool-resource-summary__meta pool-registration-job-error" title={String(row.error)}>{String(row.error)}</div> : null}
        </div>
      ),
    },
  ];

  const activeJobs = jobs.filter((job) => ACTIVE_JOB_STATUSES.has(job.status || '')).length;
  const succeededTotal = jobs.reduce((sum, job) => sum + (Number(job.succeeded) || 0), 0);
  const failedTotal = jobs.reduce((sum, job) => sum + (Number(job.failed) || 0), 0);
  const settledTotal = succeededTotal + failedTotal;
  // MetricRail draws a track for any entry carrying a `share`, so these three become small charts
  // instead of bare integers. The job count is the denominator for one and the numerator of
  // nothing, so it stays a plain number -- a total has nothing to be a fraction of.
  const jobMetrics = [
    { label: t('registration.jobs'), value: jobs.length },
    { label: t('registration.running'), value: activeJobs, tone: 'warning', share: jobs.length ? activeJobs / jobs.length : undefined },
    { label: t('registration.success'), value: succeededTotal, tone: 'success', share: settledTotal ? succeededTotal / settledTotal : undefined },
    { label: t('registration.failed'), value: failedTotal, tone: failedTotal > 0 ? 'danger' : undefined, share: settledTotal ? failedTotal / settledTotal : undefined },
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

  // A country's identity is its ISO code; the platform it was priced on is metadata, because the
  // same country appears once per provider and the codes alone would collide in the legend.
  const marketRowName = (item: SMSMarketCandidate) => `${item.country_iso || item.country_id} · ${item.provider}`;
  const marketRowKey = (item: SMSMarketCandidate) => `${item.provider}-${item.country_id}`;
  // Below the minimum sample count the backend has no history to rank on and falls back to the
  // community order, so the rate it reports is a default rather than a measurement. Saying so is
  // the difference between "50% success" and "not measured yet".
  const marketSamples = smsMarket?.minimum_history_samples || 3;
  const marketSuccessRows = visibleMarket.map((item) => {
    const measured = Number(item.attempts) >= marketSamples;
    const percent = Math.round((Number(item.success_rate) || 0) * 100);
    const notes = [
      measured ? `${item.succeeded}/${item.attempts}` : t('registration.market_cold'),
      `$${Number(item.price).toFixed(3)}`,
      `${t('registration.market_inventory')} ${fmtInt(item.inventory)}`,
    ];
    if (!item.eligible) notes.push(t('registration.market_ineligible'));
    return {
      key: marketRowKey(item),
      name: marketRowName(item),
      value: percent,
      // Ineligible rows are greyed rather than hidden: a country excluded by the price window is
      // still evidence about why the scheduler picked something else.
      color: !item.eligible ? C.grey : measured ? (percent >= 80 ? C.green : percent >= 60 ? C.amber : C.red) : C.blue,
      meta: notes.join(' · '),
    };
  });
  // Cheapest first, so the row the operator most likely wants leads and the bar grows away from
  // it. Zero-priced rows are kept because a free tier is a real answer, not missing data.
  const marketPriceRows = visibleMarket
    .map((item) => ({
      key: marketRowKey(item),
      name: marketRowName(item),
      value: Number(item.price) || 0,
      color: item.eligible ? C.blue : C.grey,
      meta: `${t('registration.market_inventory')} ${fmtInt(item.inventory)}${item.eligible ? '' : ` · ${t('registration.market_ineligible')}`}`,
    }))
    .sort((a, b) => a.value - b.value);
  // Two separate reasons this axis has to be set explicitly.
  //
  // RankedBars floors its own axis at 1 to stay safe against an all-zero set, which for a chart
  // measured in cents means a $1.00 ceiling: the priciest row would fill 40% of the track and the
  // gap between $0.02 and $0.12 would be a tenth of it.
  //
  // And the maximum cannot be the priciest row either. One country outside the price window at
  // $0.61 against a board of $0.03-$0.09 compressed every row the scheduler can actually choose
  // into the first 15% of the track -- the ranking was there and unreadable. The axis is the
  // priciest *eligible* row, so the resolution goes to the countries in contention; excluded rows
  // peg at full width, which is already what they are, and carry the grey and the 超出价格区间 note
  // that say so. With nothing eligible the whole board is the axis rather than no chart at all.
  const marketEligibleMax = visibleMarket.reduce(
    (max, item) => (item.eligible ? Math.max(max, Number(item.price) || 0) : max),
    0,
  );
  const marketPriceMax = marketEligibleMax || marketPriceRows.reduce((max, row) => Math.max(max, row.value), 0);
  const marketScanNote = t('registration.market_scan_note')
    .replace('{samples}', String(marketSamples))
    .replace('{days}', String(smsMarket?.history_window_days || 14))
    .replace('{order}', (smsMarket?.preferred_countries || DEFAULT_PREFERRED).join(' › '));
  const marketFreshness = smsMarket?.last_refreshed_at
    ? `${smsMarket.stale ? t('registration.market_stale') : t('registration.market_synced')} · ${fmtDateTime(smsMarket.last_refreshed_at)}`
    : t('registration.market_never');

  // Attempt-level composition across every job on the page. The rail above counts jobs and
  // accounts separately but never relates them, so a run that produced 12 accounts from 40
  // attempts looked identical to one that produced 12 from 12.
  const attemptTotals = jobs.reduce<{ succeeded: number; failed: number; remaining: number }>(
    (acc, job) => {
      const total = Number(job.total) || 0;
      const succeeded = Number(job.succeeded) || 0;
      const failed = Number(job.failed) || 0;
      return {
        succeeded: acc.succeeded + succeeded,
        failed: acc.failed + failed,
        // Only jobs still in flight have work outstanding; a settled job's shortfall is already
        // counted as failed or was cancelled, and charting it as "remaining" would imply the
        // batch is still running.
        remaining: acc.remaining + (ACTIVE_JOB_STATUSES.has(job.status || '') ? Math.max(0, total - succeeded - failed) : 0),
      };
    },
    { succeeded: 0, failed: 0, remaining: 0 },
  );
  const attemptSegments: Array<{ key: string; name: string; value: number; color: string }> = [
    { key: 'succeeded', name: t('registration.outcome_success'), value: attemptTotals.succeeded, color: C.green },
    { key: 'failed', name: t('registration.outcome_failed'), value: attemptTotals.failed, color: C.red },
    { key: 'remaining', name: t('registration.outcome_remaining'), value: attemptTotals.remaining, color: C.grey },
  ];

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

      <ErrorBanner error={smsMarketQuery.error} onRetry={smsMarketQuery.reload} title={t('registration.market_failed')} />
      <Card className="pool-card pool-sms-market-card" style={{ marginBottom: 18 }} title={t('registration.market_title')}>
        <div className="pool-sms-market-head">
          <div>
            <Typography.Text strong>{t('registration.market_sub')}</Typography.Text>
            <p className="pool-sms-market-description">{marketScanNote}</p>
          </div>
          <div className="pool-sms-market-actions">
            <Button icon={<IconRefresh />} loading={refreshSMSMarketMutation.isPending} onClick={refreshSMSMarket}>{t('registration.market_compare')}</Button>
          </div>
        </div>
        <div className="pool-sms-price-policy">
          <label>
            <span>{t('registration.market_min')} · USD</span>
            <input className="pool-input" type="number" min="0" max="1000" step="0.001" placeholder={t('registration.market_unlimited')} value={minPriceInput} onChange={(event) => setMinPriceInput(event.target.value)} />
          </label>
          <label>
            <span>{t('registration.market_max')} · USD</span>
            <input className="pool-input" type="number" min="0" max="1000" step="0.001" placeholder={t('registration.market_unlimited')} value={maxPriceInput} onChange={(event) => setMaxPriceInput(event.target.value)} />
          </label>
          <Button theme="solid" loading={savingStrategy} onClick={saveSMSPolicy}>{t('registration.market_save')}</Button>
          <span className="pool-sms-market-freshness">{marketFreshness}</span>
        </div>
        {/* Two ranked charts replaced a grid of eight cards carrying five numbers each. The
            backend ranks countries by success rate and breaks ties on price and stock, so those
            are the two axes worth drawing -- as a grid of forty figures the ordering the
            scheduler actually applies was invisible.
            Success rate sits on a fixed 0-100 axis so bar length is comparable between
            refreshes; price is normalised to the priciest row because there is no meaningful
            ceiling, and both read longer-as-worse: a long success bar is good, a long price bar
            is expensive. */}
        <div className="pool-sms-market-charts">
          <section className="pool-sms-market-chart">
            <h3>{t('registration.market_rank_label')}</h3>
            <RankedBars
              rows={marketSuccessRows}
              max={100}
              keepZero
              valueFormatter={(value: number) => `${value}%`}
              ariaLabel={t('registration.market_rank_label')}
              emptyText={t('registration.market_empty')}
            />
          </section>
          <section className="pool-sms-market-chart">
            <h3>{t('registration.market_price_label')}</h3>
            <RankedBars
              rows={marketPriceRows}
              max={marketPriceMax}
              keepZero
              valueFormatter={(value: number) => `$${value.toFixed(3)}`}
              ariaLabel={t('registration.market_price_label')}
              emptyText={t('registration.market_empty')}
            />
          </section>
        </div>
      </Card>

      <div className="pool-toolbar pool-registration-toolbar">
        <Button icon={<IconRefresh />} onClick={dashboardQuery.reload}>{t('common.refresh')}</Button>
        <Typography.Text type="tertiary">{t('registration.output_note')}</Typography.Text>
      </div>
      <div className="pool-resource-split pool-resource-split--wide-aside">
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
        {!dashboardQuery.error || dashboardQuery.lastRefresh ? (
          <div className="pool-registration-aside">
            <SummaryRail items={jobMetrics} className="pool-registration-metrics" />
            {/* Attempt-level composition, which the rail cannot express: its 成功 and 失败 cards are
                two independent numbers, and the shortfall between them and the batch target has no
                card at all. */}
            <section className="pool-registration-outcome">
              <h3>{t('registration.outcome')}</h3>
              <StackedMeter
                segments={attemptSegments}
                ariaLabel={t('registration.outcome')}
                valueFormatter={(value: number) => `${fmtInt(value)} ${t('registration.attempts_unit')}`}
              />
              {attemptSegments.every((segment) => segment.value <= 0) ? (
                <p className="pool-registration-outcome__empty">{t('registration.outcome_empty')}</p>
              ) : null}
            </section>
          </div>
        ) : null}
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
