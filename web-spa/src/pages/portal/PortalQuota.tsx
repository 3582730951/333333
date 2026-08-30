import { Button, LoadingState, Tag } from '../../components/pool/index.jsx';
import { IconRefresh } from '../../components/pool/icons.jsx';
import LoadErrorBanner from '../../components/LoadErrorBanner.jsx';
import PageHeader, { Panel } from '../../components/PageHeader.jsx';
import { fmtDateTime, fmtInt, fmtUSD } from '../../lib/format.js';
import { t } from '../../lib/i18n.js';
import { usePortalQuotaData } from '../../features/portal/queries/details';

export default function PortalQuota() {
  const { data, loading, refreshing, error, reload } = usePortalQuotaData();
  if (!data && loading) return <LoadingState title={t('portal_details.quota_loading')} />;
  const totals = data?.valuation;
  const usdSettled = totals ? totals.api_micro_usd_settled / 1_000_000 : null;
  const usdEstimated = totals ? totals.api_micro_usd_provisional / 1_000_000 : null;
  const creditsSettled = totals ? totals.chatgpt_milli_credits_settled / 1_000 : null;
  const creditsEstimated = totals ? totals.chatgpt_milli_credits_provisional / 1_000 : null;
  return (
    <div className="pool-portal-page">
      <PageHeader title={t('portal_details.quota_title')} subtitle={t('portal_details.quota_subtitle')}
        actions={<Button icon={<IconRefresh />} onClick={reload} loading={refreshing}>{t('common.refresh')}</Button>} />
      <LoadErrorBanner error={error} onRetry={reload} title={t('portal_details.load_failed')} />
      <div className="pool-portal-quota-grid" aria-live="polite">
        <article className="pool-panel pool-portal-quota-card"><span>{t('portal_details.api_equivalent')}</span><strong>{usdSettled == null ? '—' : fmtUSD(usdSettled)}</strong><small>{t('portal_details.provisional')}: {usdEstimated == null ? '—' : fmtUSD(usdEstimated)}</small></article>
        <article className="pool-panel pool-portal-quota-card"><span>{t('portal_details.chatgpt_credits')}</span><strong>{creditsSettled == null ? '—' : creditsSettled.toLocaleString(undefined, { maximumFractionDigits: 3 })}</strong><small>{t('portal_details.provisional')}: {creditsEstimated == null ? '—' : creditsEstimated.toLocaleString(undefined, { maximumFractionDigits: 3 })}</small></article>
        <article className="pool-panel pool-portal-quota-card"><span>{t('portal_details.accuracy')}</span><strong><Tag color={data?.accuracy === 'settled' ? 'green' : data?.accuracy === 'partial' ? 'amber' : 'blue'}>{data?.accuracy || '—'}</Tag></strong><small>{fmtInt(totals?.unavailable_events)} {t('portal_details.unavailable_events')}</small></article>
      </div>
      <Panel title={t('portal_details.method')}>
        <dl className="pool-portal-definition-list">
          <div><dt>{t('portal_details.period')}</dt><dd>{fmtDateTime(data?.period.from)} — {fmtDateTime(data?.period.to)}</dd></div>
          <div><dt>{t('portal_details.settled_events')}</dt><dd>{fmtInt(totals?.settled_events)}</dd></div>
          <div><dt>{t('portal_details.estimated_events')}</dt><dd>{fmtInt(totals?.provisional_events)}</dd></div>
          <div><dt>{t('portal_details.catalog')}</dt><dd>{data?.catalog?.id || t('portal_details.unavailable')}</dd></div>
          <div><dt>{t('portal_details.catalog_effective')}</dt><dd>{fmtDateTime(data?.catalog?.effective_at)}</dd></div>
          <div><dt>{t('portal_details.updated')}</dt><dd>{fmtDateTime(data?.updated_at)}</dd></div>
        </dl>
        <p className="pool-portal-disclosure">{t('portal_details.quota_disclosure')}</p>
      </Panel>
    </div>
  );
}
