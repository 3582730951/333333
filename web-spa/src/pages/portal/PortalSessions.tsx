import { useState } from 'react';
import { Button, ConfirmDialog, LoadingState, Tag, Toast } from '../../components/pool/index.jsx';
import { IconRefresh } from '../../components/pool/icons.jsx';
import LoadErrorBanner from '../../components/LoadErrorBanner.jsx';
import PageHeader, { Panel } from '../../components/PageHeader.jsx';
import { fmtDateTime } from '../../lib/format.js';
import { t } from '../../lib/i18n.js';
import { usePortalSessionsData, useRevokePortalSessionMutation } from '../../features/portal/queries/details';
import type { PortalSession } from '../../features/portal/model/details';

export default function PortalSessions() {
  const { data = [], loading, refreshing, error, reload } = usePortalSessionsData();
  const revoke = useRevokePortalSessionMutation();
  const [selected, setSelected] = useState<PortalSession | null>(null);
  if (!data.length && loading) return <LoadingState title={t('portal_details.sessions_loading')} />;
  return (
    <div className="pool-portal-page">
      <PageHeader title={t('portal_details.sessions_title')} subtitle={t('portal_details.sessions_subtitle')}
        actions={<Button icon={<IconRefresh />} onClick={reload} loading={refreshing}>{t('common.refresh')}</Button>} />
      <LoadErrorBanner error={error || revoke.error} onRetry={error ? reload : undefined} title={t('portal_details.load_failed')} />
      <Panel title={t('portal_details.active_sessions')}>
        <div className="pool-portal-session-list" aria-live="polite">
          {data.map((session) => <article key={session.id} className="pool-portal-session">
            <div><div className="pool-portal-session__title"><b>{session.user_agent || t('portal_details.unknown_client')}</b>{session.current ? <Tag color="green">{t('portal_details.current')}</Tag> : null}</div>
              <small>{t('portal_details.created')} {fmtDateTime(session.created_at)} · {t('portal_details.expires')} {fmtDateTime(session.expires_at)}</small>
            </div>
            <Button type="danger" theme="outline" disabled={revoke.isPending} onClick={() => setSelected(session)}>{t('portal_details.revoke')}</Button>
          </article>)}
          {!data.length ? <p className="pool-portal-disclosure">{t('portal_details.no_sessions')}</p> : null}
        </div>
      </Panel>
      <ConfirmDialog open={Boolean(selected)} title={t('portal_details.revoke_title')}
        description={selected?.current ? t('portal_details.revoke_current_desc') : t('portal_details.revoke_desc')}
        confirmText={t('portal_details.revoke')} destructive busy={revoke.isPending}
        onCancel={() => setSelected(null)} onConfirm={async () => {
          if (!selected) return;
          const current = selected.current;
          await revoke.mutateAsync(selected.id);
          setSelected(null);
          Toast.success(t('portal_details.revoked'));
          if (current) window.location.assign('/');
        }} />
    </div>
  );
}
