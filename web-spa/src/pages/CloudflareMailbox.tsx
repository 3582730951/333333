// @ts-nocheck
import React, { useCallback, useMemo, useState } from 'react';
import { useNavigate } from 'react-router';
import * as PoolUI from '../components/pool/index.jsx';
import {
  IconCheckCircleStroked, IconDelete, IconGlobe, IconRefresh, IconSave,
} from '../components/pool/icons.jsx';
import PageHeader from '../components/PageHeader.jsx';
import CopyCodeBlock from '../components/CopyCodeBlock.jsx';
import LoadErrorBanner from '../components/LoadErrorBanner.jsx';
import { MetricRail, TextClamp } from '../components/DisplayPrimitives.jsx';
import useAsyncAction from '../hooks/useAsyncAction.js';
import useAsyncResource from '../hooks/useAsyncResource.js';
import { showErrorToast } from '../components/ErrorToast.jsx';
import { fmtRelative } from '../lib/format.js';
import { t } from '../lib/i18n.js';
import {
  deleteCloudflareMailboxProfile,
  fetchCloudflareMailboxConfig,
  saveCloudflareMailboxProfile,
  testCloudflareMailboxProfile,
} from '../features/accounts/api/emailPool';
import type {
  CloudflareMailboxProfile,
  CloudflareMailboxSaveInput,
} from '../features/accounts/api/emailPool';

const {
  Button, Card, ConfirmDialog, Form, Tag, Toast, Typography,
} = PoolUI as any;
const ErrorBanner = LoadErrorBanner as any;
const SummaryRail = MetricRail as any;
const Clamp = TextClamp as any;

function healthTone(status?: string) {
  if (status === 'healthy') return 'green';
  if (status === 'unhealthy') return 'red';
  return 'grey';
}

function profileFormValues(profile?: CloudflareMailboxProfile | null) {
  return {
    provider_key: profile?.provider_key || '',
    display_name: profile?.display_name || '',
    api_url: profile?.api_url || '',
    domain: profile?.domain || '',
    admin_token: '',
    enabled: profile?.enabled ?? true,
    default_for_registration: profile?.default_for_registration ?? true,
    default_for_team: profile?.default_for_team ?? true,
  };
}

export default function CloudflareMailbox() {
  const navigate = useNavigate();
  const [editing, setEditing] = useState<CloudflareMailboxProfile | null>(null);
  const [formRevision, setFormRevision] = useState(0);
  const [deleteProfile, setDeleteProfile] = useState<CloudflareMailboxProfile | null>(null);
  const loadData = useCallback(({ signal }: { signal?: AbortSignal } = {}) => fetchCloudflareMailboxConfig(signal), []);
  const emptyData = { profiles: [], defaults: {}, deployment: { steps: [], references: [] } };
  const {
    data = emptyData, loading, error, reload, lastRefresh,
  } = useAsyncResource(loadData, [], { initialData: emptyData });
  const profiles = data.profiles || [];

  const selectProfile = (profile: CloudflareMailboxProfile | null) => {
    setEditing(profile);
    setFormRevision((value) => value + 1);
  };

  const { run: saveAndTest, running: saving } = useAsyncAction(async (values: Record<string, unknown>) => {
    const input: CloudflareMailboxSaveInput = {
      provider_key: String(values.provider_key || ''),
      display_name: String(values.display_name || ''),
      api_url: String(values.api_url || ''),
      domain: String(values.domain || ''),
      admin_token: String(values.admin_token || ''),
      enabled: values.enabled !== false,
      default_for_registration: values.default_for_registration !== false,
      default_for_team: values.default_for_team !== false,
    };
    try {
      const saved = await saveCloudflareMailboxProfile(input);
      const probe = await testCloudflareMailboxProfile({ provider_key: saved.provider_key });
      if (probe.ok) {
        Toast.success(t('cf_mail.saved_tested').replace('{latency}', String(probe.latency_ms)));
      } else {
        Toast.warning(t('cf_mail.saved_probe_failed').replace('{message}', probe.message));
      }
      await reload();
      const updated = (data.profiles || []).find((item) => item.provider_key === saved.provider_key);
      if (updated) selectProfile(updated);
    } catch (saveError) {
      showErrorToast(saveError);
    }
  });

  const { run: probeProfile, running: probing } = useAsyncAction(async (profile: CloudflareMailboxProfile) => {
    try {
      const result = await testCloudflareMailboxProfile({ provider_key: profile.provider_key });
      if (result.ok) Toast.success(t('cf_mail.test_ok').replace('{latency}', String(result.latency_ms)));
      else Toast.error(t('cf_mail.test_failed').replace('{message}', result.message));
      await reload();
    } catch (probeError) {
      showErrorToast(probeError);
    }
  });

  const { run: removeProfile, running: deleting } = useAsyncAction(async () => {
    if (!deleteProfile) return;
    try {
      await deleteCloudflareMailboxProfile(deleteProfile.provider_key);
      Toast.success(t('cf_mail.deleted'));
      if (editing?.provider_key === deleteProfile.provider_key) selectProfile(null);
      setDeleteProfile(null);
      await reload();
    } catch (deleteError) {
      showErrorToast(deleteError);
    }
  });

  const healthy = profiles.filter((profile) => profile.health?.last_status === 'healthy').length;
  const defaults = profiles.filter((profile) => profile.default_for_registration || profile.default_for_team).length;
  const metrics = useMemo(() => [
    { key: 'profiles', label: t('cf_mail.profiles'), value: profiles.length, detail: t('cf_mail.profiles_detail'), tone: 'accent' },
    { key: 'healthy', label: t('cf_mail.healthy'), value: healthy, detail: t('cf_mail.health_detail'), tone: healthy ? 'success' : 'neutral' },
    { key: 'defaults', label: t('cf_mail.defaults'), value: defaults, detail: t('cf_mail.defaults_detail') },
    { key: 'domains', label: t('cf_mail.domains'), value: new Set(profiles.map((profile) => profile.domain)).size, detail: t('cf_mail.same_domain_detail') },
  ], [profiles, healthy, defaults]);

  const activeProfile = editing || null;
  const initValues = profileFormValues(activeProfile);

  return (
    <div className="pool-cf-mail">
      <PageHeader
        title={t('cf_mail.title')}
        subtitle={t('cf_mail.subtitle')}
        actions={(
          <>
            <Button onClick={() => navigate('/email-pool')}>{t('cf_mail.back')}</Button>
            <Button icon={<IconRefresh />} loading={loading} onClick={() => void reload()}>{t('common.refresh')}</Button>
          </>
        )}
      />

      <section className="pool-cf-mail-hero">
        <div className="pool-cf-mail-hero__copy">
          <span className="pool-cf-mail-kicker"><IconGlobe /> {t('cf_mail.self_hosted')}</span>
          <h2>{t('cf_mail.hero_title')}</h2>
          <p>{t('cf_mail.hero_desc')}</p>
          <div className="pool-cf-mail-hero__badges">
            <Tag color="blue">Cloudflare Email Routing</Tag>
            <Tag>D1</Tag>
            <Tag color="green">{t('cf_mail.same_domain')}</Tag>
          </div>
        </div>
        <div className="pool-cf-mail-domain-preview" aria-label={t('cf_mail.domain_preview')}>
          <span>@</span>
          <strong>{activeProfile?.domain || profiles[0]?.domain || 'mail.example.com'}</strong>
          <small>{t('cf_mail.domain_preview_note')}</small>
        </div>
      </section>

      <SummaryRail items={metrics} className="pool-cf-mail-metrics" />
      {error ? <ErrorBanner error={error} title={t('cf_mail.load_failed')} onRetry={reload} /> : null}

      <div className="pool-cf-mail-layout">
        <section className="pool-cf-mail-setup">
          <div className="pool-section-heading">
            <div>
              <span>{t('cf_mail.step_label')}</span>
              <h3>{activeProfile ? t('cf_mail.edit_profile') : t('cf_mail.new_profile')}</h3>
            </div>
            {activeProfile ? <Tag>{activeProfile.provider_key}</Tag> : <Tag color="blue">{t('cf_mail.recommended')}</Tag>}
          </div>
          <Form
            key={`${activeProfile?.provider_key || 'new'}-${formRevision}`}
            initValues={initValues}
            labelPosition="top"
            className="pool-cf-mail-form"
            onSubmit={saveAndTest}
          >
            <Form.Input field="provider_key" label={t('cf_mail.provider_key')} disabled={Boolean(activeProfile)} placeholder={t('cf_mail.provider_key_auto')} />
            <Form.Input field="display_name" label={t('cf_mail.display_name')} placeholder="Cloudflare · example.com" />
            <Form.Input field="api_url" label={t('cf_mail.api_url')} rules={[{ required: true }]} placeholder="https://mail.example.com" />
            <Form.Input field="domain" label={t('cf_mail.domain')} rules={[{ required: true }]} placeholder="example.com" />
            <Form.Input
              field="admin_token"
              label={t('cf_mail.admin_token')}
              mode="password"
              placeholder={activeProfile?.admin_token_configured ? t('cf_mail.secret_keep') : t('cf_mail.secret_optional')}
            />
            <div className="pool-cf-mail-switches">
              <Form.Switch field="enabled" label={t('cf_mail.enabled')} />
              <Form.Switch field="default_for_registration" label={t('cf_mail.default_registration')} />
              <Form.Switch field="default_for_team" label={t('cf_mail.default_team')} />
            </div>
            <Typography.Text type="tertiary" size="small">{t('cf_mail.secret_note')}</Typography.Text>
            <div className="pool-cf-mail-form__actions">
              <Button theme="solid" htmlType="submit" icon={<IconSave />} loading={saving}>
                {t('cf_mail.save_test')}
              </Button>
              {activeProfile ? <Button onClick={() => selectProfile(null)}>{t('cf_mail.new_profile')}</Button> : null}
            </div>
          </Form>
        </section>

        <aside className="pool-cf-mail-guide">
          <div className="pool-section-heading">
            <div><span>{t('cf_mail.guide_label')}</span><h3>{t('cf_mail.guide_title')}</h3></div>
          </div>
          <ol className="pool-cf-mail-steps">
            {(data.deployment?.steps || []).map((step, index) => (
              <li key={step}>
                <span>{String(index + 1).padStart(2, '0')}</span>
                <p>{t(`cf_mail.guide_step_${index + 1}`, step)}</p>
              </li>
            ))}
          </ol>
          <div className="pool-cf-mail-quickstart">
            <strong>复制粘贴部署</strong>
            <p>在项目根目录执行；把两个 example.com 换成 Cloudflare 中的实际域名。</p>
            <CopyCodeBlock code={(data.deployment?.quickstart || []).join('\n')} label="复制全部命令" />
            <small>仓库路径：<code>{data.deployment?.repository_path || 'deploy/cloudflare-mailbox'}</code></small>
          </div>
          <div className="pool-cf-mail-references">
            {(data.deployment?.references || []).map((reference, index) => (
              <a key={reference} href={reference} target="_blank" rel="noreferrer">Cloudflare 官方步骤 {index + 1}</a>
            ))}
          </div>
          <div className="pool-cf-mail-note">
            <IconCheckCircleStroked />
            <div>
              <strong>{t('cf_mail.least_privilege')}</strong>
              <p>{t('cf_mail.least_privilege_desc')}</p>
            </div>
          </div>
        </aside>
      </div>

      <section className="pool-cf-mail-profiles">
        <div className="pool-section-heading">
          <div><span>{t('cf_mail.inventory_label')}</span><h3>{t('cf_mail.configured_profiles')}</h3></div>
          <small>{lastRefresh ? `${t('common.updated')} ${fmtRelative(Math.floor(lastRefresh.getTime() / 1000))}` : ''}</small>
        </div>
        <div className="pool-cf-mail-profile-grid">
          {profiles.map((profile) => (
            <Card
              key={profile.provider_key}
              className={`pool-cf-mail-profile ${editing?.provider_key === profile.provider_key ? 'is-selected' : ''}`}
            >
              <div className="pool-cf-mail-profile__head">
                <div>
                  <Clamp strong title={profile.display_name}>{profile.display_name}</Clamp>
                  <Clamp muted title={profile.api_url}>{profile.api_url}</Clamp>
                </div>
                <Tag color={healthTone(profile.health?.last_status)}>{profile.health?.last_status || t('common.unknown')}</Tag>
              </div>
              <div className="pool-cf-mail-profile__domain">@{profile.domain}</div>
              <div className="pool-cf-mail-profile__meta">
                <span>{t('cf_mail.latency')} <strong>{profile.health?.latency_ms || 0} ms</strong></span>
                <span>{t('cf_mail.failures')} <strong>{profile.health?.consecutive_failures || 0}</strong></span>
              </div>
              <div className="pool-cf-mail-profile__flags">
                {profile.default_for_registration ? <Tag size="small" color="blue">{t('cf_mail.registration_default_short')}</Tag> : null}
                {profile.default_for_team ? <Tag size="small" color="green">{t('cf_mail.team_default_short')}</Tag> : null}
                {profile.admin_token_configured ? <Tag size="small">{t('cf_mail.secret_configured')}</Tag> : null}
              </div>
              <div className="pool-cf-mail-profile__actions">
                <Button size="small" onClick={() => selectProfile(profile)}>{t('common.edit')}</Button>
                <Button size="small" loading={probing} onClick={() => void probeProfile(profile)}>{t('email_pool.test')}</Button>
                <Button size="small" type="danger" icon={<IconDelete />} onClick={() => setDeleteProfile(profile)}>{t('common.delete')}</Button>
              </div>
            </Card>
          ))}
          {!profiles.length && !loading ? (
            <button type="button" className="pool-cf-mail-empty" onClick={() => selectProfile(null)}>
              <IconGlobe />
              <strong>{t('cf_mail.empty')}</strong>
              <span>{t('cf_mail.empty_desc')}</span>
            </button>
          ) : null}
        </div>
      </section>

      <ConfirmDialog
        open={Boolean(deleteProfile)}
        title={t('cf_mail.delete_title').replace('{domain}', deleteProfile?.domain || '')}
        description={t('cf_mail.delete_desc')}
        confirmText={t('common.delete')}
        cancelText={t('common.cancel')}
        destructive
        onCancel={() => { if (!deleting) setDeleteProfile(null); }}
        onConfirm={() => void removeProfile()}
      />
    </div>
  );
}
