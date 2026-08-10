import React, { useCallback, useEffect, useMemo, useState } from 'react';
import { useNavigate } from 'react-router';
import { Button, Card, Form, Modal, Tag, Toast, Typography } from '../components/pool/index.jsx';
import { IconPlus, IconPulse, IconRefresh, IconStop, IconUndo, IconUserGroup } from '../components/pool/icons.jsx';
import PageHeader from '../components/PageHeader.jsx';
import ResourceTable from '../components/ResourceTable.jsx';
import MobileResourceCell from '../components/MobileResourceCell.jsx';
import LoadErrorBanner from '../components/LoadErrorBanner.jsx';
import { MetricRail, TextClamp, TinyMeter } from '../components/DisplayPrimitives.jsx';
import { get, post } from '../api.js';
import { showErrorToast } from '../components/ErrorToast.jsx';
import { fmtRelative } from '../lib/format.js';
import { t } from '../lib/i18n.js';

const DataTable = ResourceTable as any;
const MobileRow = MobileResourceCell as any;
const Clamp = TextClamp as any;
const SummaryRail = MetricRail as any;
const Meter = TinyMeter as any;
const ErrorBanner = LoadErrorBanner as any;

interface TeamWorkspace {
  id: string;
  name: string;
  parent_account_id: string;
  workspace_ref: string;
  connector_kind: string;
  max_members: number;
  status: string;
  mailbox_provider_key?: string;
  required_email_domain?: string;
  same_domain_required: boolean;
  updated_at: number;
}

interface TeamWorkflow {
  id: string;
  workspace_id: string;
  parent_account_id: string;
  child_account_id: string;
  state: string;
  credential_path?: string;
  imported_account_id?: string;
  replacement_method?: string;
  replacement_job_ref?: string;
  mailbox_provider_key?: string;
  required_email_domain?: string;
  quota_remaining_bps: number;
  rotate_threshold_bps: number;
  attempt: number;
  max_attempts: number;
  next_attempt_at: number;
  error_class?: string;
  shadow_mode: boolean;
  version: number;
  created_at: number;
  updated_at: number;
  completed_at: number;
}

interface TeamEvent {
  id: number;
  sequence: number;
  from_state: string;
  to_state: string;
  event_type: string;
  detail_json: string;
  created_at: number;
}

interface TeamAccount {
  id: string;
  label?: string;
  email?: string;
  upstream_account_id?: string;
  plan_type?: string;
  status?: string;
}

interface LifecycleReadiness {
  ready: boolean;
  workspace_create_ready: boolean;
  cycle_create_ready: boolean;
  parent_accounts: number;
  mailbox_profiles: number;
  mailbox_default_configured: boolean;
  mailbox_healthy: boolean;
  registration_ready: boolean;
  registration_method?: string;
  workspaces: number;
  blockers: Array<{ code: string; message: string; href: string }>;
}

interface LifecycleSnapshot {
  workspaces: TeamWorkspace[];
  workflows: TeamWorkflow[];
  states: Record<string, number>;
  mailboxProfiles: Array<{ provider_key: string; display_name: string; domain: string; enabled: boolean; default_for_team?: boolean }>;
  accounts: TeamAccount[];
  readiness: LifecycleReadiness | null;
}

const EMPTY_SNAPSHOT: LifecycleSnapshot = { workspaces: [], workflows: [], states: {}, mailboxProfiles: [], accounts: [], readiness: null };

function newOperationKey(prefix: string): string {
  const random = globalThis.crypto?.randomUUID?.() || `${Date.now()}-${Math.random().toString(36).slice(2)}`;
  return `${prefix}-${random}`;
}

function stateLabel(state: string): string {
  return t(`team_lifecycle.state.${state}`, state.replaceAll('_', ' '));
}

function stateTag(state: string) {
  const colors: Record<string, string> = {
    active: 'green',
    completed: 'green',
    retry_wait: 'amber',
    review_required: 'red',
    cancelled: 'grey',
    queued: 'grey',
    removing: 'amber',
    phone_verification: 'blue',
  };
  return <Tag color={colors[state] || 'blue'}>{stateLabel(state)}</Tag>;
}

function percentFromBPS(value: number): string {
  if (!Number.isFinite(value) || value < 0) return t('common.unknown');
  const percent = value / 100;
  return `${percent < 10 ? percent.toFixed(2) : percent.toFixed(1)}%`;
}

function quotaCell(workflow: TeamWorkflow) {
  if (workflow.quota_remaining_bps < 0) {
    return <span className="pool-muted">{t('common.unknown')}</span>;
  }
  const low = workflow.quota_remaining_bps <= workflow.rotate_threshold_bps;
  return (
    <div className="pool-lifecycle-quota">
      <strong className={low ? 'pool-lifecycle-quota--low' : ''}>{percentFromBPS(workflow.quota_remaining_bps)}</strong>
      <Meter
        value={workflow.quota_remaining_bps}
        max={10000}
        tone={low ? 'danger' : 'accent'}
        label={`${percentFromBPS(workflow.quota_remaining_bps)} ${t('team_lifecycle.remaining')}`}
      />
      <span>{t('team_lifecycle.threshold')} {percentFromBPS(workflow.rotate_threshold_bps)}</span>
    </div>
  );
}

export default function TeamLifecycle() {
  const navigate = useNavigate();
  const [snapshot, setSnapshot] = useState<LifecycleSnapshot>(EMPTY_SNAPSHOT);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<unknown>(null);
  const [lastRefresh, setLastRefresh] = useState<Date | null>(null);
  const [workspaceOpen, setWorkspaceOpen] = useState(false);
  const [cycleOpen, setCycleOpen] = useState(false);
  const [saving, setSaving] = useState(false);
  const [cycleKey, setCycleKey] = useState('');
  const [detail, setDetail] = useState<TeamWorkflow | null>(null);
  const [events, setEvents] = useState<TeamEvent[]>([]);
  const [eventsLoading, setEventsLoading] = useState(false);
  const [manualChild, setManualChild] = useState(false);
  const [workspaceAdvanced, setWorkspaceAdvanced] = useState(false);
  const [cycleAdvanced, setCycleAdvanced] = useState(false);

  const load = useCallback(async () => {
    setLoading(true);
    try {
      const [workspacePayload, workflowPayload, statsPayload, mailboxPayload, accountPayload] = await Promise.all([
        get('/admin/team-lifecycle/workspaces', { limit: 200 }),
        get('/admin/team-lifecycle/workflows', { limit: 200 }),
        get('/admin/team-lifecycle/stats'),
        get('/admin/email-pool/cloudflare').catch(() => ({ profiles: [] })),
        get('/admin/accounts', { page: 1, pageSize: 200 }).catch(() => ({ accounts: [] })),
      ]);
      setSnapshot({
        workspaces: Array.isArray(workspacePayload?.items) ? workspacePayload.items : [],
        workflows: Array.isArray(workflowPayload?.items) ? workflowPayload.items : [],
        states: statsPayload?.states && typeof statsPayload.states === 'object' ? statsPayload.states : {},
        mailboxProfiles: Array.isArray(mailboxPayload?.profiles) ? mailboxPayload.profiles : [],
        accounts: Array.isArray(accountPayload) ? accountPayload
          : (Array.isArray(accountPayload?.accounts) ? accountPayload.accounts
            : (Array.isArray(accountPayload?.rows) ? accountPayload.rows : [])),
        readiness: statsPayload?.readiness && typeof statsPayload.readiness === 'object'
          ? statsPayload.readiness as LifecycleReadiness
          : null,
      });
      setError(null);
      setLastRefresh(new Date());
    } catch (loadError) {
      setError(loadError);
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  const workspaceByID = useMemo(
    () => new Map(snapshot.workspaces.map((workspace) => [workspace.id, workspace])),
    [snapshot.workspaces],
  );
  const total = snapshot.workflows.length;
  const active = snapshot.states.active || 0;
  const waiting = (snapshot.states.queued || 0) + (snapshot.states.retry_wait || 0);
  const review = snapshot.states.review_required || 0;
  const completed = snapshot.states.completed || 0;
  const accountOptions = snapshot.accounts
    .filter((account) => account.status !== 'disabled')
    .map((account) => ({
      label: `${account.label || account.email || account.id} · ${account.email || account.id}${account.plan_type ? ` · ${account.plan_type}` : ''}`,
      value: account.id,
    }));
  const parentAccountOptions = snapshot.accounts
    .filter((account) => account.status !== 'disabled' && Boolean(account.upstream_account_id))
    .map((account) => ({
      label: `${account.label || account.email || account.id} · ${account.email || account.id}${account.plan_type ? ` · ${account.plan_type}` : ''}`,
      value: account.id,
    }));

  const openWorkspace = () => {
    setWorkspaceAdvanced(false);
    setWorkspaceOpen(true);
  };

  const openCycle = () => {
    setCycleKey(newOperationKey('team-cycle'));
    setManualChild(false);
    setCycleAdvanced(false);
    setCycleOpen(true);
  };

  const readiness = snapshot.readiness;
  const firstBlocker = readiness?.blockers?.[0]?.message || '';
  const setupChecks = [
    {
      key: 'parent', ready: (readiness?.parent_accounts || 0) > 0,
      title: '1. 母号凭据', detail: readiness ? `${readiness.parent_accounts} 个可用 Team 母号` : '正在检查母号与 workspace ID',
      action: '管理账号', run: () => navigate('/accounts'),
    },
    {
      key: 'mailbox', ready: Boolean(readiness?.mailbox_default_configured && readiness?.mailbox_healthy),
      title: '2. 同域邮箱', detail: readiness?.mailbox_default_configured
        ? (readiness.mailbox_healthy ? '团队默认邮箱检测通过' : '默认邮箱尚未通过连接检测')
        : '连接 Worker 并设为团队默认',
      action: '配置邮箱', run: () => navigate('/email-pool/cloudflare'),
    },
    {
      key: 'registration', ready: Boolean(readiness?.registration_ready),
      title: '3. 自动补号', detail: readiness?.registration_ready
        ? `${readiness.registration_method || '默认'} 已通过 canary`
        : '配置注册器、住宅代理并通过 canary',
      action: '配置注册', run: () => navigate('/registration'),
    },
    {
      key: 'workspace', ready: (readiness?.workspaces ?? snapshot.workspaces.length) > 0,
      title: '4. Team 空间', detail: `${readiness?.workspaces ?? snapshot.workspaces.length} 个已配置空间`,
      action: '创建空间', run: openWorkspace,
    },
  ];

  const saveWorkspace = async (values: Record<string, unknown>) => {
    setSaving(true);
    try {
      await post('/admin/team-lifecycle/workspaces', {
        name: values.name,
        parent_account_id: values.parent_account_id,
        workspace_ref: values.workspace_ref,
        connector_kind: 'native',
        max_members: Number(values.max_members) || 10,
        status: 'active',
        mailbox_provider_key: values.mailbox_provider_key || '',
        required_email_domain: values.required_email_domain || '',
        same_domain_required: values.same_domain_required !== false,
      });
      Toast.success(t('team_lifecycle.workspace_saved'));
      setWorkspaceOpen(false);
      await load();
    } catch (saveError) {
      showErrorToast(saveError);
    } finally {
      setSaving(false);
    }
  };

  const saveCycle = async (values: Record<string, unknown>) => {
    const workspace = workspaceByID.get(String(values.workspace_id || ''));
    if (!workspace) {
      Toast.error(t('team_lifecycle.workspace_required'));
      return;
    }
    setSaving(true);
    try {
      await post('/admin/team-lifecycle/workflows', {
        workspace_id: workspace.id,
        parent_account_id: workspace.parent_account_id,
        child_account_id: values.child_account_id === '__manual__' ? values.child_identity : values.child_account_id,
        replacement_method: values.replacement_method || '',
        rotate_threshold_percent: Number(values.rotate_threshold_percent) || 1,
        max_attempts: Number(values.max_attempts) || 5,
        shadow_mode: values.shadow_mode === true,
      }, { headers: { 'Idempotency-Key': cycleKey } });
      Toast.success(t('team_lifecycle.cycle_saved'));
      setCycleOpen(false);
      await load();
    } catch (saveError) {
      showErrorToast(saveError);
    } finally {
      setSaving(false);
    }
  };

  const openDetail = async (workflow: TeamWorkflow) => {
    setDetail(workflow);
    setEvents([]);
    setEventsLoading(true);
    try {
      const payload = await get(`/admin/team-lifecycle/workflows/${encodeURIComponent(workflow.id)}/events`, { limit: 300 });
      setEvents(Array.isArray(payload?.items) ? payload.items : []);
    } catch (loadError) {
      showErrorToast(loadError);
    } finally {
      setEventsLoading(false);
    }
  };

  const action = async (workflow: TeamWorkflow, operation: 'cancel' | 'retry') => {
    try {
      await post(`/admin/team-lifecycle/workflows/${encodeURIComponent(workflow.id)}/${operation}`, {});
      Toast.success(operation === 'retry' ? t('team_lifecycle.retry_saved') : t('team_lifecycle.cancelled'));
      setDetail(null);
      await load();
    } catch (actionError) {
      showErrorToast(actionError);
    }
  };

  const rowActions = (workflow: TeamWorkflow) => (
    <div className="pool-row-actions pool-row-actions--compact">
      <Button size="small" theme="borderless" onClick={() => void openDetail(workflow)}>
        {t('common.details')}
      </Button>
      {workflow.state === 'review_required' ? (
        <Button size="small" theme="borderless" icon={<IconUndo />} onClick={() => void action(workflow, 'retry')}>
          {t('team_lifecycle.retry')}
        </Button>
      ) : null}
      {!['completed', 'cancelled', 'review_required'].includes(workflow.state) ? (
        <Button size="small" theme="borderless" type="danger" icon={<IconStop />} onClick={() => void action(workflow, 'cancel')}>
          {t('common.cancel')}
        </Button>
      ) : null}
    </div>
  );

  const columns: any[] = [
    {
      title: t('team_lifecycle.child'),
      dataIndex: 'child_account_id',
      width: 260,
      render: (value: string, workflow: TeamWorkflow) => (
        <div className="pool-lifecycle-identity">
          <Clamp strong title={value} ariaLabel={value}>{value}</Clamp>
          <Clamp muted title={workflow.imported_account_id || workflow.id}>
            {workflow.imported_account_id || workflow.id}
          </Clamp>
        </div>
      ),
    },
    {
      title: t('team_lifecycle.workspace'),
      dataIndex: 'workspace_id',
      width: 210,
      render: (value: string) => {
        const workspace = workspaceByID.get(value);
        return (
          <div className="pool-lifecycle-identity">
            <Clamp strong title={workspace?.name || value}>{workspace?.name || value}</Clamp>
            <Clamp muted title={value}>{value}</Clamp>
            {workspace?.required_email_domain ? (
              <span className="pool-lifecycle-domain">@{workspace.required_email_domain}</span>
            ) : null}
          </div>
        );
      },
    },
    { title: t('team_lifecycle.state'), dataIndex: 'state', width: 160, render: stateTag },
    {
      title: t('team_lifecycle.credential_path'),
      dataIndex: 'credential_path',
      width: 154,
      render: (value: string, workflow: TeamWorkflow) => (
        <div className="pool-lifecycle-path">
          <Tag size="small">{value || t('team_lifecycle.pending')}</Tag>
          <span>
            {workflow.shadow_mode ? t('team_lifecycle.shadow') : t('team_lifecycle.execute')}
            {' · '}
            {workflow.replacement_method || t('team_lifecycle.default_registration')}
          </span>
        </div>
      ),
    },
    { title: t('team_lifecycle.quota'), dataIndex: 'quota_remaining_bps', width: 190, render: (_: number, workflow: TeamWorkflow) => quotaCell(workflow) },
    {
      title: t('team_lifecycle.retry_budget'),
      dataIndex: 'attempt',
      width: 130,
      render: (value: number, workflow: TeamWorkflow) => (
        <span className="pool-nowrap">{value} / {workflow.max_attempts}</span>
      ),
    },
    {
      title: t('team_lifecycle.updated'),
      dataIndex: 'updated_at',
      width: 150,
      render: (value: number) => <span className="pool-nowrap">{value ? fmtRelative(value) : '—'}</span>,
    },
    { title: t('common.actions'), dataIndex: 'actions', width: 190, fixed: 'right', render: (_: unknown, workflow: TeamWorkflow) => rowActions(workflow) },
  ];

  const flowSteps = [
    ['01', t('team_lifecycle.flow_invite')],
    ['02', t('team_lifecycle.flow_resolve')],
    ['03', t('team_lifecycle.flow_login')],
    ['04', t('team_lifecycle.flow_phone')],
    ['05', t('team_lifecycle.flow_import')],
    ['06', t('team_lifecycle.flow_observe')],
    ['07', t('team_lifecycle.flow_rotate')],
    ['08', t('team_lifecycle.flow_repeat')],
  ];

  return (
    <div className="pool-team-lifecycle">
      <PageHeader
        title={t('team_lifecycle.title')}
        subtitle={t('team_lifecycle.subtitle')}
        actions={(
          <>
            <Button icon={<IconUserGroup />} disabled={Boolean(readiness && !readiness.workspace_create_ready)} title={firstBlocker} onClick={openWorkspace}>{t('team_lifecycle.new_workspace')}</Button>
            <Button theme="solid" icon={<IconPlus />} disabled={readiness ? !readiness.cycle_create_ready : !snapshot.workspaces.length} title={firstBlocker} onClick={openCycle}>{t('team_lifecycle.new_cycle')}</Button>
            <Button icon={<IconRefresh />} loading={loading} onClick={() => void load()}>{t('common.refresh')}</Button>
          </>
        )}
      />

      <section className="pool-lifecycle-readiness" aria-label="Team 生命周期配置清单">
        <div className="pool-lifecycle-readiness__head">
          <div><span>开始前</span><h2>四项配置，按顺序点完即可</h2></div>
          <Tag color={readiness?.ready ? 'green' : 'orange'}>{readiness?.ready ? '可以启动循环' : '还需完成配置'}</Tag>
        </div>
        <ol className="pool-lifecycle-readiness__grid">
          {setupChecks.map((check) => (
            <li key={check.key} className={check.ready ? 'is-ready' : ''}>
              <span className="pool-lifecycle-readiness__state" aria-label={check.ready ? '已就绪' : '待配置'}>{check.ready ? '✓' : '!'}</span>
              <div><strong>{check.title}</strong><p>{check.detail}</p></div>
              <Button size="small" theme="borderless" onClick={check.run}>{check.ready ? '查看' : check.action}</Button>
            </li>
          ))}
        </ol>
        {firstBlocker ? <p className="pool-lifecycle-readiness__next"><strong>下一步：</strong>{firstBlocker}</p> : null}
      </section>

      <section className="pool-lifecycle-hero" aria-label={t('team_lifecycle.flow_title')}>
        <div className="pool-lifecycle-hero__copy">
          <span className="pool-lifecycle-kicker"><IconPulse /> {t('team_lifecycle.durable_orchestration')}</span>
          <h2>{t('team_lifecycle.flow_title')}</h2>
          <p>{t('team_lifecycle.flow_description')}</p>
        </div>
        <div className="pool-lifecycle-threshold" aria-label={`${t('team_lifecycle.threshold')} 1%`}>
          <strong>1%</strong>
          <span>{t('team_lifecycle.rotation_line')}</span>
        </div>
        <ol className="pool-lifecycle-flow">
          {flowSteps.map(([index, label]) => (
            <li className="pool-lifecycle-flow__step" key={index}>
              <span>{index}</span>
              <strong>{label}</strong>
            </li>
          ))}
        </ol>
      </section>

      {/* active / waiting / review / completed are mutually exclusive states of the same
          workflow set, so each is a genuine part of `total` -- and the active card was already
          printing that denominator as its own detail, handing the reader both numbers and
          leaving them to divide. The track draws that division. `workspaces` counts rooms, not
          cycles, so it has no denominator here and stays a bare number. total===0 yields no
          share at all rather than a zero-width track, which would imply a measured zero. */}
      <SummaryRail
        className="pool-lifecycle-metrics"
        items={[
          { key: 'workspaces', label: t('team_lifecycle.workspaces'), value: snapshot.workspaces.length, detail: t('team_lifecycle.rooms'), tone: 'accent' },
          { key: 'active', label: t('team_lifecycle.active'), value: active, detail: `${total} ${t('team_lifecycle.total_cycles')}`, tone: 'success', share: total ? active / total : undefined },
          { key: 'waiting', label: t('team_lifecycle.waiting'), value: waiting, detail: t('team_lifecycle.durable_queue'), tone: waiting ? 'warning' : 'neutral', share: total ? waiting / total : undefined },
          { key: 'review', label: t('team_lifecycle.review'), value: review, detail: t('team_lifecycle.operator_review'), tone: review ? 'danger' : 'neutral', share: total ? review / total : undefined },
          { key: 'completed', label: t('team_lifecycle.completed'), value: completed, detail: t('team_lifecycle.replacement_queued'), tone: 'accent', share: total ? completed / total : undefined },
        ]}
      />

      {error ? <ErrorBanner error={error} title={t('team_lifecycle.load_failed')} onRetry={load} /> : null}

      <DataTable
        loading={loading}
        lastRefresh={lastRefresh}
        dataSource={snapshot.workflows}
        columns={columns}
        rowKey="id"
        pagination={{ pageSize: 20 }}
        minScrollX={1444}
        safeActionWidth={190}
        className="pool-lifecycle-table"
        emptyTitle={t('team_lifecycle.empty')}
        emptyDesc={t('team_lifecycle.empty_desc')}
        emptyAction={snapshot.workspaces.length ? <Button theme="solid" icon={<IconPlus />} disabled={Boolean(readiness && !readiness.cycle_create_ready)} onClick={openCycle}>{t('team_lifecycle.new_cycle')}</Button> : <Button theme="solid" icon={<IconUserGroup />} disabled={Boolean(readiness && !readiness.workspace_create_ready)} onClick={openWorkspace}>{t('team_lifecycle.new_workspace')}</Button>}
        skeletonRows={7}
        skeletonCols={8}
        mobileRenderer={(workflow: TeamWorkflow) => (
          <MobileRow
            title={<Clamp strong title={workflow.child_account_id}>{workflow.child_account_id}</Clamp>}
            subtitle={workflow.id}
            badges={<>{stateTag(workflow.state)}{workflow.shadow_mode ? <Tag>{t('team_lifecycle.shadow')}</Tag> : null}</>}
            details={[
              { label: t('team_lifecycle.workspace'), value: workspaceByID.get(workflow.workspace_id)?.name || workflow.workspace_id },
              { label: t('team_lifecycle.credential_path'), value: workflow.credential_path || t('team_lifecycle.pending') },
              { label: t('team_lifecycle.quota'), value: quotaCell(workflow) },
              { label: t('team_lifecycle.retry_budget'), value: `${workflow.attempt} / ${workflow.max_attempts}` },
              { label: t('team_lifecycle.updated'), value: workflow.updated_at ? fmtRelative(workflow.updated_at) : '—' },
            ]}
            actions={rowActions(workflow)}
          />
        )}
        mobileListLabel={t('team_lifecycle.mobile_list')}
      />

      <Modal open={workspaceOpen} visible={workspaceOpen} width={680} title={t('team_lifecycle.new_workspace')} footer={null} onCancel={() => { if (!saving) setWorkspaceOpen(false); }}>
        <div className="pool-lifecycle-form-intro">
          <span className="pool-lifecycle-form-intro__number">1</span>
          <div><strong>选择母号并建立 Team 房间</strong><p>只需选择账号池里的母号。系统会读取 Team workspace ID，并用内置连接器完成邀请、移除与额度观察。</p></div>
        </div>
        <Form onSubmit={saveWorkspace} initValues={{ max_members: 10, mailbox_provider_key: snapshot.mailboxProfiles.find((profile) => profile.default_for_team)?.provider_key || '', same_domain_required: true }}>
          <Form.Input field="name" label={t('team_lifecycle.workspace_name')} placeholder="例如：Free 子号循环 · A 组" rules={[{ required: true }]} />
          <Form.Select field="parent_account_id" label="母号（账号池）" filter placeholder="搜索名称、邮箱或账号 ID" rules={[{ required: true }]} optionList={parentAccountOptions} />
          <div className="pool-lifecycle-auto-field">
            <span>连接方式</span><strong>内置 Team API</strong><Tag size="small" color="green">自动</Tag>
            <small>凭据从加密账号池读取，不需要再次粘贴 Token。</small>
          </div>
          <Button theme="borderless" onClick={() => setWorkspaceAdvanced((value) => !value)}>
            {workspaceAdvanced ? '收起高级设置' : '高级设置（通常不用改）'}
          </Button>
          {workspaceAdvanced ? <div className="pool-lifecycle-advanced-fields">
            <Form.Input field="workspace_ref" label="Team workspace ID（高级覆盖）" placeholder="通常留空，由母号自动识别" />
            <Form.InputNumber field="max_members" label={t('team_lifecycle.max_members')} min={1} max={10000} />
            <Form.Select
              field="mailbox_provider_key"
              label={t('team_lifecycle.mailbox_provider')}
              optionList={[
                { label: t('team_lifecycle.mailbox_default'), value: '' },
                ...snapshot.mailboxProfiles.filter((profile) => profile.enabled).map((profile) => ({
                  label: `${profile.display_name} · @${profile.domain}`,
                  value: profile.provider_key,
                })),
              ]}
            />
            <Form.Input field="required_email_domain" label={t('team_lifecycle.required_domain')} placeholder={t('team_lifecycle.required_domain_auto')} />
            <Form.Switch field="same_domain_required" label={t('team_lifecycle.same_domain_required')} />
            <Typography.Text type="tertiary" size="small">{t('team_lifecycle.same_domain_help')}</Typography.Text>
          </div> : null}
          <div className="pool-lifecycle-modal-actions">
            <Button onClick={() => setWorkspaceOpen(false)}>{t('common.cancel')}</Button>
            <Button theme="solid" htmlType="submit" loading={saving}>{t('common.save')}</Button>
          </div>
        </Form>
      </Modal>

      <Modal open={cycleOpen} visible={cycleOpen} width={680} title={t('team_lifecycle.new_cycle')} footer={null} onCancel={() => { if (!saving) setCycleOpen(false); }}>
        <div className="pool-lifecycle-form-intro">
          <span className="pool-lifecycle-form-intro__number">2</span>
          <div><strong>选择子号并启动循环</strong><p>系统依次执行邀请、令牌解析、OAuth/add_phone、入池、额度观察、踢出和补号。</p></div>
        </div>
        <Form onSubmit={saveCycle} initValues={{ workspace_id: snapshot.workspaces[0]?.id || '', replacement_method: '', rotate_threshold_percent: 1, max_attempts: 5, shadow_mode: false }}>
          <Form.Select field="workspace_id" label={t('team_lifecycle.workspace')} filter rules={[{ required: true }]} optionList={snapshot.workspaces.map((workspace) => ({
            label: `${workspace.name} · ${workspace.id}`,
            value: workspace.id,
          }))} />
          <Form.Select
            field="child_account_id"
            label="首个子号"
            placeholder="从账号池选择"
            rules={[{ required: true }]}
            onChange={(value: string) => setManualChild(value === '__manual__')}
            optionList={[...accountOptions, { label: '手动填写邮箱或账号 ID…', value: '__manual__' }]}
          />
          {manualChild ? <Form.Input field="child_identity" label="子号邮箱 / 账号 ID" placeholder="child@example.com" rules={[{ required: true }]} /> : null}
          <Button theme="borderless" onClick={() => setCycleAdvanced((value) => !value)}>
            {cycleAdvanced ? '收起高级策略' : '高级策略（默认：1% 自动轮换，失败重试 5 次）'}
          </Button>
          {cycleAdvanced ? <div className="pool-lifecycle-advanced-fields">
            <Form.Select field="replacement_method" label={t('team_lifecycle.replacement_method')} optionList={[
              { label: t('team_lifecycle.default_registration'), value: '' },
              { label: t('team_lifecycle.protocol_registration'), value: 'protocol_v2' },
              { label: t('team_lifecycle.browser_registration'), value: 'browser_v3' },
            ]} />
            <Form.InputNumber field="rotate_threshold_percent" label={t('team_lifecycle.threshold_percent')} min={0.01} max={100} step={0.01} />
            <Form.InputNumber field="max_attempts" label={t('team_lifecycle.max_attempts')} min={1} max={20} />
            <Form.Switch field="shadow_mode" label={t('team_lifecycle.shadow_first')} />
            <Typography.Text type="tertiary" size="small">{t('team_lifecycle.shadow_help')}</Typography.Text>
          </div> : null}
          <div className="pool-lifecycle-cycle-preview">
            <span>邀请子号</span><i>→</i><span>令牌 / OAuth</span><i>→</i><span>add_phone</span><i>→</i><span>额度 1%</span><i>→</i><span>踢出补号</span>
          </div>
          <div className="pool-lifecycle-modal-actions">
            <Button onClick={() => setCycleOpen(false)}>{t('common.cancel')}</Button>
            <Button theme="solid" htmlType="submit" loading={saving}>{t('team_lifecycle.create_plan')}</Button>
          </div>
        </Form>
      </Modal>

      <Modal open={Boolean(detail)} visible={Boolean(detail)} title={t('team_lifecycle.event_timeline')} footer={null} width={760} onCancel={() => setDetail(null)}>
        {detail ? (
          <>
            <Card className="pool-lifecycle-detail-summary">
              <div><span>{t('team_lifecycle.child')}</span><Clamp strong title={detail.child_account_id}>{detail.child_account_id}</Clamp></div>
              <div><span>{t('team_lifecycle.state')}</span>{stateTag(detail.state)}</div>
              <div><span>{t('team_lifecycle.quota')}</span><strong>{percentFromBPS(detail.quota_remaining_bps)}</strong></div>
            </Card>
            <div className="pool-lifecycle-events" aria-busy={eventsLoading}>
              {eventsLoading ? <div className="pool-muted">{t('common.loading')}</div> : events.map((event) => (
                <article className="pool-lifecycle-event" key={event.id}>
                  <span className="pool-lifecycle-event__dot" />
                  <div>
                    <div className="pool-lifecycle-event__head">
                      <strong>{event.event_type.replaceAll('_', ' ')}</strong>
                      <time>{fmtRelative(event.created_at)}</time>
                    </div>
                    <p>{event.from_state || '∅'} <span aria-hidden="true">→</span> {event.to_state}</p>
                  </div>
                </article>
              ))}
              {!eventsLoading && !events.length ? <div className="pool-muted">{t('team_lifecycle.no_events')}</div> : null}
            </div>
          </>
        ) : null}
      </Modal>
    </div>
  );
}
