import { useMemo, useState } from 'react';
import { Button, DataTable, Form, Tag } from '../../components/pool/index.jsx';
import { IconRefresh } from '../../components/pool/icons.jsx';
import LoadErrorBanner from '../../components/LoadErrorBanner.jsx';
import PageHeader, { Panel } from '../../components/PageHeader.jsx';
import { fmtDateTime, fmtTokens, fmtUSD } from '../../lib/format.js';
import { t } from '../../lib/i18n.js';
import { usePortalUsageEventsData } from '../../features/portal/queries/details';
import type { PortalUsageEvent, PortalValuation } from '../../features/portal/model/details';

function nullableTokens(value: number | null) {
  return value == null ? '—' : fmtTokens(value);
}

function valuationLabel(rows: PortalValuation[]) {
  const usd = rows.find((row) => row.valuation_kind === 'api_usd_equivalent');
  const credits = rows.find((row) => row.valuation_kind === 'chatgpt_credits');
  const values: string[] = [];
  if (usd?.amount_units != null && usd.unit_scale > 0) values.push(`${fmtUSD(usd.amount_units / usd.unit_scale)} API`);
  if (credits?.amount_units != null && credits.unit_scale > 0) values.push(`${(credits.amount_units / credits.unit_scale).toLocaleString(undefined, { maximumFractionDigits: 3 })} Credits`);
  return values.length ? values.join(' · ') : t('portal_details.unavailable');
}

export default function PortalUsage() {
  const [modelDraft, setModelDraft] = useState('');
  const [model, setModel] = useState('');
  const [tier, setTier] = useState<'' | 'default' | 'fast'>('');
  const [cursor, setCursor] = useState('');
  const [history, setHistory] = useState<string[]>([]);
  const filters = useMemo(() => ({ model, service_tier: tier, cursor, limit: 50 }), [cursor, model, tier]);
  const { data, loading, refreshing, error, reload } = usePortalUsageEventsData(filters);
  const rows = data?.items || [];

  const columns = [
    {
      title: t('portal_details.time'), dataIndex: 'created_at', width: 120,
      render: (value: unknown) => fmtDateTime(Number(value)),
    },
    {
      title: t('portal_details.model'), dataIndex: 'models',
      render: (_: unknown, row: PortalUsageEvent) => (
        <div className="pool-portal-usage__model"><b>{row.models.observed || row.models.resolved || row.models.requested || '—'}</b>
          {row.models.requested && row.models.requested !== row.models.observed ? <small>{row.models.requested} → {row.models.observed || row.models.resolved}</small> : null}
        </div>
      ),
    },
    {
      title: t('portal_details.tier'), dataIndex: 'service_tier', width: 116,
      render: (_: unknown, row: PortalUsageEvent) => <Tag color={row.service_tier.billed === 'fast' ? 'violet' : 'blue'}>{row.service_tier.billed || 'unknown'}</Tag>,
    },
    {
      title: t('portal_details.tokens'), dataIndex: 'tokens',
      render: (_: unknown, row: PortalUsageEvent) => (
        <div className="pool-portal-token-grid">
          <span>{t('portal_details.input')} <b>{nullableTokens(row.tokens.input_total)}</b></span>
          <span>{t('portal_details.cached')} <b>{nullableTokens(row.tokens.cached_read)}</b></span>
          <span>{t('portal_details.output')} <b>{nullableTokens(row.tokens.output_total)}</b></span>
          <span>{t('portal_details.reasoning')} <b>{nullableTokens(row.tokens.output_reasoning)}</b></span>
        </div>
      ),
    },
    {
      title: t('portal_details.valuation'), dataIndex: 'valuations',
      render: (value: unknown) => valuationLabel((value as PortalValuation[]) || []),
    },
    {
      title: t('portal_details.accuracy'), dataIndex: 'settlement_state', width: 118,
      render: (_: unknown, row: PortalUsageEvent) => <Tag color={row.settlement_state === 'settled' && !row.estimated ? 'green' : row.integrity_error ? 'red' : 'amber'}>
        {row.integrity_error ? 'integrity error' : row.estimated ? 'estimated' : row.settlement_state}
      </Tag>,
    },
  ];

  const applyFilters = () => {
    setCursor('');
    setHistory([]);
    setModel(modelDraft.trim());
  };

  return (
    <div className="pool-portal-page">
      <PageHeader title={t('portal_details.usage_title')} subtitle={t('portal_details.usage_subtitle')}
        actions={<Button icon={<IconRefresh />} onClick={reload} loading={refreshing}>{t('common.refresh')}</Button>} />
      <LoadErrorBanner error={error} onRetry={reload} title={t('portal_details.load_failed')} />
      <Panel title={t('portal_details.filters')}>
        <div className="pool-portal-filters">
          <Form.Input value={modelDraft} onChange={(value: string) => setModelDraft(value)} placeholder={t('portal_details.model_filter')} onEnterPress={applyFilters} />
          <label><span>{t('portal_details.tier')}</span><select value={tier} onChange={(event) => { setTier(event.target.value as '' | 'default' | 'fast'); setCursor(''); setHistory([]); }}>
            <option value="">{t('portal_details.all')}</option><option value="default">Standard</option><option value="fast">Fast</option>
          </select></label>
          <Button theme="solid" onClick={applyFilters}>{t('portal_details.apply')}</Button>
        </div>
      </Panel>
      <Panel title={t('portal_details.requests')}>
        <DataTable<PortalUsageEvent> dataSource={rows} columns={columns} rowKey="usage_event_id" loading={loading}
          pagination={false} scroll={{ x: 980 }} aria-label={t('portal_details.requests')} />
        <div className="pool-portal-pagination" aria-live="polite">
          <Button disabled={!history.length || loading} onClick={() => {
            setHistory((current) => { const next = current.slice(); setCursor(next.pop() || ''); return next; });
          }}>{t('portal_details.previous')}</Button>
          <span>{rows.length ? `${rows.length} ${t('portal_details.items')}` : t('portal_details.no_items')}</span>
          <Button disabled={!data?.has_more || !data.next_cursor || loading} onClick={() => {
            setHistory((current) => [...current, cursor]); setCursor(data?.next_cursor || '');
          }}>{t('portal_details.next')}</Button>
        </div>
      </Panel>
    </div>
  );
}
