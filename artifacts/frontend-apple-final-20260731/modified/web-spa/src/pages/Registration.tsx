import React, { useEffect, useState } from 'react';
import { useNavigate } from 'react-router';
import * as PoolUI from '../components/pool/index.jsx';
import { IconRefresh, IconPlay, IconSetting } from '../components/pool/icons.jsx';
import LoadErrorBanner from '../components/LoadErrorBanner.jsx';
import PageHeader from '../components/PageHeader.jsx';
import ResourceTable from '../components/ResourceTable.jsx';
import { MetricRail, TextClamp } from '../components/DisplayPrimitives.jsx';
import { ReadinessPanel, TaskDetailDrawer, TaskProgress } from '../components/WorkflowPrimitives.jsx';
import { showErrorToast } from '../components/ErrorToast.jsx';
import { normalizeApiError } from '../api/errors';
import { t } from '../lib/i18n.js';
import {
  useRegistrationCountriesData, useRegistrationDashboardData, useRegistrationOptionsData,
  useRegistrationStrategyData, useSaveRegistrationStrategyMutation, useStartRegistrationJobMutation,
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

interface RegistrationFormValues {
  count?: number | string;
  group_name?: string;
  method?: string;
  registration_egress_pool_id?: string;
  sms_provider?: string;
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

  const dashboardQuery = useRegistrationDashboardData();
  const optionsQuery = useRegistrationOptionsData();
  const countriesQuery = useRegistrationCountriesData();
  const strategyQuery = useRegistrationStrategyData();
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

  const persistStrategy = async (nextStrategy: RegistrationCountryStrategy, manualIso: string) => {
    const nextManualCountry = nextStrategy === 'manual' ? manualIso : '';
    if (nextStrategy === savedStrategy && nextManualCountry === savedManualCountry) return;
    await saveStrategyMutation.mutateAsync({ strategy: nextStrategy, manualCountry: nextManualCountry });
    setSavedStrategy(nextStrategy);
    setSavedManualCountry(nextManualCountry);
    Toast.success(t('registration.strategy_saved'));
  };

  const effectiveMethod = normalizeRegisterMethod(selectedMethod, defaultMethod);
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
                { label: `${t('registration.default')} (${defaultMethod})`, value: '' },
                { label: 'protocol', value: 'protocol' },
                { label: 'node (puppeteer)', value: 'node' },
                { label: 'protocol_v2', value: 'protocol_v2' },
                { label: 'browser', value: 'browser' },
                { label: 'browser_v3', value: 'browser_v3' },
              ]}
              onChange={(value: string) => {
                const nextMethod = value || '';
                const nextEffectiveMethod = normalizeRegisterMethod(nextMethod, defaultMethod);
                setSelectedMethod(nextMethod);
                setIdentityMode(lockedIdentityForMethod(nextEffectiveMethod) || identityMode || 'phone');
              }} />
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
          layout="fit"
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
