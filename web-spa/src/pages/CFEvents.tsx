import React from 'react';
import * as PoolUI from '../components/pool/index.jsx';
import { IconRefresh } from '../components/pool/icons.jsx';
import PageHeader from '../components/PageHeader.jsx';
import ResourceTable from '../components/ResourceTable.jsx';
import { MetricRail, TextClamp } from '../components/DisplayPrimitives.jsx';
import MobileResourceCell from '../components/MobileResourceCell.jsx';
import { useCFEventsData } from '../features/observability/queries/events.ts';
import { fmtDateTime } from '../lib/format.js';
import { t } from '../lib/i18n.js';
import type { CFEventRow } from '../features/observability/model/types';

const { Button, Tag } = PoolUI as any;
const DataTable = ResourceTable as any;
const Clamp = TextClamp as any;
const MobileRow = MobileResourceCell as any;
const SummaryRail = MetricRail as any;

const statusColor = (s: unknown) => {
  const code = Number(s);
  if (!Number.isFinite(code)) return 'grey';
  return code >= 500 ? 'red' : code >= 400 ? 'amber' : 'green';
};

export default function CFEvents() {
  const { data: rows = [], loading, error, lastRefresh, reload: load } = useCFEventsData();
  const blockedCount = rows.filter((row) => Number(row.status) >= 400).length;
  const serverErrorCount = rows.filter((row) => Number(row.status) >= 500).length;
  const challengeCount = rows.filter((row) => String(row.category || '').toLowerCase().includes('challenge')).length;
  const cfMetrics = [
    { label: t('cf.events'), value: rows.length },
    { label: t('cf.passed'), value: rows.filter((row) => Number(row.status) < 400).length, tone: 'success' },
    { label: t('cf.challenges'), value: challengeCount, tone: challengeCount ? 'warning' : undefined },
    { label: '4xx / 5xx', value: `${blockedCount} / ${serverErrorCount}`, tone: blockedCount ? 'danger' : undefined },
  ];

  const cols: any[] = [
    {
      title: t('cf.event'),
      key: 'event',
      width: 440,
      render: (_: unknown, row: CFEventRow) => (
        <div className="pool-event-cell">
          <div className="pool-event-meta">
            <Tag size="small" color={statusColor(row.status)}>{row.status || '—'}</Tag>
            {row.category ? <Tag size="small">{row.category}</Tag> : null}
            <span>{fmtDateTime(row.created_at)}</span>
          </div>
          <Clamp>{row.message || '—'}</Clamp>
        </div>
      ),
    },
    {
      title: t('cf.identity'),
      key: 'identity',
      width: 280,
      render: (_: unknown, row: CFEventRow) => (
        <div className="pool-resource-summary">
          <Clamp>{row.account_id || '—'}</Clamp>
          <div className="pool-resource-summary__meta">
            {row.egress_id ? <Tag size="small">{row.egress_id}</Tag> : t('common.default_egress')}
          </div>
        </div>
      ),
    },
    { title: 'CF Ray', dataIndex: 'cf_ray', width: 180, render: (v: unknown) => <Clamp className="pool-mono">{String(v || '—')}</Clamp> },
  ];

  return (
    <div>
      <PageHeader title={t('cf.title')} subtitle={t('cf.subtitle')}
        actions={<Button icon={<IconRefresh />} onClick={load}>{t('common.refresh')}</Button>} />
      <div className="pool-resource-split">
        <DataTable
          error={error}
          onRetry={load}
          loading={loading}
          lastRefresh={lastRefresh}
          dataSource={rows}
          columns={cols}
          rowKey="id"
          pagination={{ pageSize: 25 }}
          className="pool-cf-events-table"
          density="compact"
          layout="fit"
          scroll={false}
          rowHeight={64}
          emptyTitle={t('cf.empty')}
          skeletonRows={8}
          skeletonCols={3}
          mobileRenderer={(row: CFEventRow) => (
            <MobileRow
              title={row.message || t('cf.missing_description')}
              subtitle={row.account_id || t('cf.unlinked_account')}
              badges={<><Tag color={statusColor(row.status)}>{row.status || '—'}</Tag>{row.category ? <Tag>{row.category}</Tag> : null}</>}
              details={[
                { label: t('cf.time'), value: fmtDateTime(row.created_at) },
                { label: t('cf.egress'), value: row.egress_id || t('common.default_egress') },
                { label: 'CF Ray', value: row.cf_ray || '—' },
              ]}
            />
          )}
          mobileListLabel={t('cf.title')}
        />
        {!error || lastRefresh ? <SummaryRail items={cfMetrics} /> : null}
      </div>
    </div>
  );
}
