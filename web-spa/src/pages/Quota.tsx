import React, { useMemo } from 'react';
import { Button, Progress, Tag, Toast } from '../components/pool/index.jsx';
import { IconRefresh, IconDownload } from '../components/pool/icons.jsx';
import PageHeader from '../components/PageHeader.jsx';
import ResourceTable from '../components/ResourceTable.jsx';
import MobileResourceCell from '../components/MobileResourceCell.jsx';
import { TextClamp } from '../components/DisplayPrimitives.jsx';
import * as MicroCharts from '../components/MicroCharts.jsx';
import { useQuotaData } from '../features/observability/queries/events.ts';
import { refreshQuota } from '../features/observability/api/events.ts';
import useAsyncAction from '../hooks/useAsyncAction.js';
import { toCSV, downloadCSV } from '../lib/csv.js';
import { fmtTokens, fmtRelative, fmtInt, middleEllipsis } from '../lib/format.js';
import { COLORS } from '../lib/chartTheme.js';
import { t } from '../lib/i18n.js';
import { isTokenMeteredQuotaLimiter, quotaUsageDetails, quotaWindowUsedPercent } from '../lib/quotaWindows.js';
import type { QuotaRow } from '../features/observability/model/types';
import { formatPlanLabel } from '../features/accounts/model/planFormatter.ts';

const DataTable = ResourceTable as any;
const MobileRow = MobileResourceCell as any;
const { RankedBars, RadialGauge, StackedMeter } = MicroCharts as any;
const C = COLORS;
const pctColor = (p: number) => (p >= 90 ? 'var(--pool-danger)' : p >= 70 ? 'var(--pool-warning)' : 'var(--pool-success)');

// Primary-window usage is the number operators actually schedule against; fall back
// to the flat field for backends that predate quota_summary.
function primaryUsed(row: QuotaRow): number | null {
  const value = row.quota_summary?.primary?.used_percent ?? row.used_percent;
  return value == null || Number(value) < 0 ? null : Number(value);
}
function syncReason(row: QuotaRow): string {
  return String(row.quota_summary?.sync_reason || row.status || 'never_polled');
}

// Codex reports two different "credit" concepts and they must not be conflated:
//   quota_summary.credits       — the extra paid balance beyond the plan's windows
//                                 (has_credits / unlimited / balance + spend control)
//   quota_summary.reset_credits — a count of discrete rate-limit resets
// Only the first is a spendable allowance, so it drives the headline panel.
type ExtraCredits = {
  hasCredits: boolean;
  unlimited: boolean;
  spendControl: boolean;
  balance: string;
  limit: string;
  used: string;
  remaining: string;
  usedPercent: number | null;
  spendReached: boolean;
  status: string;
};

function extraCredits(row: QuotaRow): ExtraCredits | null {
  const credits = row.quota_summary?.credits;
  if (!credits) return null;
  const usedPercent = Number(credits.used_percent);
  return {
    hasCredits: Boolean(credits.has_credits),
    unlimited: Boolean(credits.unlimited),
    spendControl: Boolean(credits.spend_control),
    balance: String(credits.balance || '').trim(),
    limit: String(credits.limit || '').trim(),
    used: String(credits.used || '').trim(),
    remaining: String(credits.remaining || '').trim(),
    usedPercent: Number.isFinite(usedPercent) && usedPercent >= 0 ? usedPercent : null,
    spendReached: Boolean(credits.spend_control_reached),
    status: String(credits.status || '').trim(),
  };
}

function resetCredits(row: QuotaRow): { status: string; available: number | null } | null {
  const credits = row.quota_summary?.reset_credits;
  if (!credits) return null;
  const status = String(credits.status || 'unknown');
  const available = credits.available_count == null ? null : Number(credits.available_count);
  return { status, available: status === 'ok' && Number.isFinite(available) ? available : null };
}

export default function Quota() {
  const {
    data: rows = [],
    loading,
    error,
    lastRefresh,
    reload: load,
  } = useQuotaData();

  const { run: refreshUpstreamQuota, running: refreshingUpstream } = useAsyncAction(async () => {
    try {
		const result = await refreshQuota();
		await load();
		if (result.reused) {
			Toast.info('最近两分钟已刷新，已直接显示最新额度');
		} else if (result.failed > 0) {
        Toast.warning(`额度已更新 ${result.updated} 个，失败 ${result.failed} 个`);
      } else {
        Toast.success(`额度已更新 ${result.updated} 个账号`);
      }
    } catch (error: any) {
      Toast.error(error?.userMessage || error?.message || '额度刷新失败');
    }
  });

  const bar = (p: number | null | undefined) => {
    if (p == null || p < 0) return <span className="pool-muted">{t('common.unknown')}</span>;
    const v = Math.round(p);
    return <div style={{ minWidth: 130 }}><Progress percent={v} stroke={pctColor(v)} showInfo size="small" format={() => v + '%'} /></div>;
  };

  const exportCSV = () => {
    const ok = downloadCSV('quota.csv', toCSV(rows, [
      { title: 'account', get: (r: QuotaRow) => r.label || r.account_id }, { title: 'provider', get: (r: QuotaRow) => r.provider },
      { title: 'plan', get: (r: QuotaRow) => formatPlanLabel(r.plan_type, r.plan_presentation) }, { title: 'oauth_rate_limit_tier', get: (r: QuotaRow) => r.oauth_rate_limit_tier },
      { title: '5h_used_pct', get: (r: QuotaRow) => quotaWindowUsedPercent(r, '5h') },
      { title: '7d_used_pct', get: (r: QuotaRow) => quotaWindowUsedPercent(r, '7d') },
      { title: 'remaining_tokens', get: (r: QuotaRow) => r.quota_summary?.primary?.remaining_tokens ?? r.remaining_tokens },
      { title: 'sync_reason', get: (r: QuotaRow) => r.quota_summary?.sync_reason ?? r.status },
    ]));
    if (!ok) Toast.error(t('quota.export_failed'));
  };

  const overview = useMemo(() => {
    const withUsage = rows.map((row) => ({ row, used: primaryUsed(row) })).filter((item) => item.used !== null) as Array<{ row: QuotaRow; used: number }>;
    const critical = withUsage.filter((item) => item.used >= 90).length;
    const warning = withUsage.filter((item) => item.used >= 70 && item.used < 90).length;
    const healthy = withUsage.length - critical - warning;
    const average = withUsage.length
      ? withUsage.reduce((sum, item) => sum + item.used, 0) / withUsage.length
      : null;
    // Only token-metered windows can be summed: Claude's 5h/7d OAuth windows and
    // unified/token windows are input-token budgets, while Codex's 7d_polled is
    // percentage-only, Kiro's kiro_usage is a credit count, and Cursor's
    // cursor_monthly is a request count. Summing across those units produced a
    // meaningless headline total, so non-token limiters are excluded and the
    // per-provider breakdown is shown in the panel note instead.
    const remaining = rows.reduce((sum, row) => {
      const limiter = row.quota_summary?.primary?.limiter_type || row.limiter_type || '';
      if (!isTokenMeteredQuotaLimiter(limiter)) return sum;
      const value = row.quota_summary?.primary?.remaining_tokens ?? row.remaining_tokens;
      return value == null || Number(value) < 0 ? sum : sum + Number(value);
    }, 0);
    // Per-provider token remainders, so the caption can say what it is summing.
    const remainingByProvider = rows.reduce((acc, row) => {
      const limiter = row.quota_summary?.primary?.limiter_type || row.limiter_type || '';
      if (!isTokenMeteredQuotaLimiter(limiter)) return acc;
      const value = row.quota_summary?.primary?.remaining_tokens ?? row.remaining_tokens;
      if (value == null || Number(value) < 0) return acc;
      const provider = row.provider || 'unknown';
      acc[provider] = (acc[provider] || 0) + Number(value);
      return acc;
    }, {} as Record<string, number>);
    const stale = rows.filter((row) => {
      const reason = syncReason(row);
      return reason !== 'ok';
    }).length;
    const pressure = withUsage
      .slice()
      .sort((a, b) => b.used - a.used)
      .slice(0, 6)
      .map((item) => ({
        key: `${item.row.account_id}:${item.row.limiter_type || ''}`,
        name: item.row.label || item.row.account_id || t('common.unnamed_account'),
        value: item.used,
        color: pctColor(item.used),
        meta: item.row.provider ? `${item.row.provider}${item.row.plan_type || item.row.plan_presentation ? ` · ${formatPlanLabel(item.row.plan_type, item.row.plan_presentation)}` : ''}` : undefined,
      }));
    return { critical, warning, healthy, average, remaining, remainingByProvider, stale, pressure, measured: withUsage.length };
  }, [rows]);

  // Credit-metered accounts form their own population; the panel only exists when the
  // deployment actually has some, so a Claude-only pool never shows a dead card.
  const credits = useMemo(() => {
    const entries = rows
      .map((row) => ({ row, credit: extraCredits(row) }))
      .filter((item) => item.credit !== null) as Array<{ row: QuotaRow; credit: ExtraCredits }>;
    if (!entries.length) return null;

    const unlimited = entries.filter((item) => item.credit.unlimited).length;
    // "Depleted" means the extra balance is genuinely gone. A spend-control-only
    // row (no credits block upstream) has has_credits=false but nothing was used
    // up — counting it here read like every Free/Plus account had run dry.
    const depleted = entries.filter((item) => !item.credit.unlimited && !item.credit.hasCredits && !item.credit.spendControl).length;
    const spendOnly = entries.filter((item) => !item.credit.unlimited && !item.credit.hasCredits && item.credit.spendControl).length;
    const spendReached = entries.filter((item) => item.credit.spendReached).length;
    // Only accounts with a real spend-control percentage can be ranked; an account
    // with credits but no workspace limit has nothing to plot.
    const ranked = entries
      .filter((item) => item.credit.usedPercent !== null)
      .sort((a, b) => (b.credit.usedPercent || 0) - (a.credit.usedPercent || 0))
      .slice(0, 6)
      .map((item) => ({
        key: `${item.row.account_id}:credits`,
        name: item.row.label || item.row.account_id || t('common.unnamed_account'),
        value: item.credit.usedPercent as number,
        color: pctColor(item.credit.usedPercent as number),
        meta: item.credit.limit && item.credit.used
          ? t('quota.credits_of').replace('{used}', item.credit.used).replace('{limit}', item.credit.limit)
          : item.credit.balance || undefined,
      }));
    // Balances are upstream-formatted strings (currency, possibly hidden), so they are
    // listed verbatim rather than summed into a fake total.
    const balances = entries
      .filter((item) => item.credit.balance && !item.credit.unlimited)
      .slice(0, 6)
      .map((item) => ({
        key: item.row.account_id,
        name: item.row.label || item.row.account_id || t('common.unnamed_account'),
        balance: item.credit.balance,
      }));
    return { accounts: entries.length, unlimited, depleted, spendOnly, spendReached, ranked, balances };
  }, [rows]);

  const cols: any[] = [
    // Shortened from the middle rather than wrapped. pool-break-value set overflow-wrap:
    // anywhere, which broke ids at whatever character hit the column edge — the 230px cell
    // rendered acct_low_hit_high_spend_002 as "…spend_00" over "2", leaving the reader unable
    // to tell the suffix from a wrapped digit. Accounts already shortens ids this way.
    //
    // 15/10 keeps the result inside the cell so the CSS clamp never fires and eats the tail:
    // the column is 230px with 14px of cell padding either side, and at 600 weight / 14px in
    // --pool-font-sans an identifier character measures ~7.46px, so 202px of content holds 26.
    // 15 + 1 + 10 = 26 at the ceiling; the full value is on the element as a title either way.
    { title: t('quota.account'), dataIndex: 'account_id', width: 230, render: (v: unknown, r: QuotaRow) => {
      const name = r.label || String(v || '');
      return <TextClamp strong title={name} ariaLabel={name}>{middleEllipsis(name, 15, 10)}</TextClamp>;
    } },
    { title: t('quota.provider'), dataIndex: 'provider', width: 96, render: (v: any) => v ? <Tag>{v}</Tag> : '—' },
    { title: t('quota.plan'), dataIndex: 'plan_type', width: 140, render: (_v: any, r: QuotaRow) => r.plan_type || r.plan_presentation ? <Tag>{formatPlanLabel(r.plan_type, r.plan_presentation)}</Tag> : '—' },
    { title: 'OAuth Tier', dataIndex: 'oauth_rate_limit_tier', width: 140, render: (v: any) => v ? <span className="pool-mono">{v}</span> : '—' },
    { title: t('quota.window'), dataIndex: 'limiter_type', width: 150, render: (v: any) => v ? <span className="pool-mono">{v}</span> : '—' },
    { title: t('quota.usage_5h'), dataIndex: 'used_percent', width: 170, sorter: (a: QuotaRow, b: QuotaRow) => (quotaWindowUsedPercent(a, '5h') || 0) - (quotaWindowUsedPercent(b, '5h') || 0), defaultSortOrder: 'descend', render: (_v: number, r: QuotaRow) => bar(quotaWindowUsedPercent(r, '5h')) },
    { title: t('quota.usage_7d'), dataIndex: 'secondary_7d_used_pct', width: 170, sorter: (a: QuotaRow, b: QuotaRow) => (quotaWindowUsedPercent(a, '7d') || 0) - (quotaWindowUsedPercent(b, '7d') || 0), render: (_v: number, r: QuotaRow) => bar(quotaWindowUsedPercent(r, '7d')) },
    { title: t('quota.remaining_tokens'), dataIndex: 'remaining_tokens', width: 150, sorter: (a: QuotaRow, b: QuotaRow) => ((a.quota_summary?.primary?.remaining_tokens ?? a.remaining_tokens) || 0) - ((b.quota_summary?.primary?.remaining_tokens ?? b.remaining_tokens) || 0), render: (v: number, r: QuotaRow) => {
      const remaining = r.quota_summary?.primary?.remaining_tokens ?? v;
      return remaining == null || remaining < 0 ? '—' : fmtTokens(remaining);
    } },
    { title: t('quota.credits_column'), key: 'extra_credits', width: 150, render: (_: unknown, r: QuotaRow) => {
      const credit = extraCredits(r);
      // No credits block at all means this provider does not meter an extra balance.
      if (!credit) return <span className="pool-muted">—</span>;
      if (credit.unlimited) return <Tag color="green">{t('quota.credits_unlimited_tag')}</Tag>;
      if (credit.spendReached) return <Tag color="red">{t('quota.credits_spend_reached_tag')}</Tag>;
      // A spend-control-only row means "no extra balance", not "balance used up".
      if (!credit.hasCredits && !credit.spendControl) return <Tag color="red">{t('quota.credits_depleted_tag')}</Tag>;
      if (!credit.hasCredits) return <Tag color="amber">{t('quota.credits_spend_only_tag')}</Tag>;
      if (credit.balance) return <span className="pool-nowrap">{credit.balance}</span>;
      // Upstream can confirm credits exist while hiding the amount.
      return <Tag color="green">{t('quota.credits_available_tag')}</Tag>;
    } },
    { title: t('quota.sync'), dataIndex: 'status', width: 170, render: (v: string, r: QuotaRow) => {
      const reason = r.quota_summary?.sync_reason || v || 'never_polled';
      const color = String(reason).startsWith('error/') || reason === 'token_expired' ? 'red' : reason === 'ok' ? 'green' : 'amber';
      return <Tag color={color}>{reason}</Tag>;
    } },
    { title: t('quota.reset'), dataIndex: 'reset_at', width: 150, render: (v: number, r: QuotaRow) => <span className="pool-nowrap">{(r.quota_summary?.primary?.reset_at ?? v) ? fmtRelative(r.quota_summary?.primary?.reset_at ?? v) : '—'}</span> },
  ];

  return (
    <div className="pool-quota-page">
      <PageHeader title={t('quota.title')} subtitle={t('quota.subtitle')}
        actions={<>
          <Button icon={<IconDownload />} onClick={exportCSV}>{t('common.export')}</Button>
          <Button icon={<IconRefresh />} loading={refreshingUpstream} onClick={refreshUpstreamQuota}>刷新上游额度</Button>
        </>} />

      {overview.measured ? (
        <section className="pool-quota-overview">
          <div className="pool-chart-card pool-quota-overview__gauge">
            <div className="head"><div><div className="t">{t('quota.pressure_title')}</div><div className="s">{t('quota.pressure_desc')}</div></div></div>
            <div className="pool-quota-overview__gauge-body">
              <RadialGauge
                value={(overview.average ?? 0) / 100}
                size={128}
                color={pctColor(overview.average ?? 0)}
                caption={t('quota.average_caption')}
                label={t('quota.remaining_total').replace('{value}', fmtTokens(overview.remaining))}
              />
              {Object.keys(overview.remainingByProvider).length > 1 ? (
                <p className="pool-quota-overview__note">
                  {t('quota.remaining_breakdown').replace('{list}', Object.entries(overview.remainingByProvider)
                    .map(([provider, value]) => `${provider} ${fmtTokens(value)}`).join(' · '))}
                </p>
              ) : null}
              <StackedMeter
                segments={[
                  { key: 'healthy', name: t('quota.band_healthy'), value: overview.healthy, color: C.green },
                  { key: 'warning', name: t('quota.band_warning'), value: overview.warning, color: C.amber },
                  { key: 'critical', name: t('quota.band_critical'), value: overview.critical, color: C.red },
                ]}
                valueFormatter={fmtInt}
                ariaLabel={t('quota.pressure_title')}
              />
            </div>
          </div>
          <div className="pool-chart-card pool-quota-overview__ranked">
            <div className="head"><div><div className="t">{t('quota.top_pressure')}</div><div className="s">{t('quota.top_pressure_desc')}</div></div></div>
            <RankedBars
              rows={overview.pressure}
              max={100}
              valueFormatter={(value: number) => `${Math.round(value)}%`}
              ariaLabel={t('quota.top_pressure')}
            />
            {overview.stale ? (
              <p className="pool-quota-overview__note">{t('quota.stale_note').replace('{count}', fmtInt(overview.stale))}</p>
            ) : null}
          </div>
        </section>
      ) : null}

      {credits ? (
        <section className="pool-chart-card pool-quota-credits">
          <div className="head">
            <div>
              <div className="t">{t('quota.credits_title')}</div>
              <div className="s">{t('quota.credits_desc')}</div>
            </div>
          </div>
          <div className="pool-quota-credits__body">
            <dl className="pool-quota-credits__facts">
              <div>
                <dt>{t('quota.credits_accounts')}</dt>
                <dd>{fmtInt(credits.accounts)}</dd>
              </div>
              {credits.unlimited ? (
                <div>
                  <dt>{t('quota.credits_unlimited')}</dt>
                  <dd>{fmtInt(credits.unlimited)}</dd>
                </div>
              ) : null}
              <div>
                <dt>{t('quota.credits_depleted')}</dt>
                <dd className={credits.depleted ? 'pool-danger-text' : ''}>{fmtInt(credits.depleted)}</dd>
              </div>
              {credits.spendOnly ? (
                <div>
                  <dt>{t('quota.credits_spend_only')}</dt>
                  <dd>{fmtInt(credits.spendOnly)}</dd>
                </div>
              ) : null}
              {credits.spendReached ? (
                <div>
                  <dt>{t('quota.credits_spend_reached')}</dt>
                  <dd className="pool-danger-text">{fmtInt(credits.spendReached)}</dd>
                </div>
              ) : null}
            </dl>
            <div className="pool-quota-credits__ranked">
              {credits.ranked.length ? (
                <RankedBars
                  rows={credits.ranked}
                  keepZero
                  max={100}
                  valueFormatter={(value: number) => `${Math.round(value)}%`}
                  ariaLabel={t('quota.credits_title')}
                />
              ) : credits.balances.length ? (
                // No workspace spend limit to plot against — show the balances instead
                // of an empty chart.
                <ul className="pool-quota-credits__balances">
                  {credits.balances.map((item) => (
                    <li key={item.key}>
                      <span title={item.name}>{item.name}</span>
                      <b>{item.balance}</b>
                    </li>
                  ))}
                </ul>
              ) : (
                <div className="pool-chart-empty">{t('quota.credits_no_limit')}</div>
              )}
            </div>
          </div>
        </section>
      ) : null}

      <DataTable
        error={error}
        onRetry={load}
        loading={loading}
        lastRefresh={lastRefresh}
        dataSource={rows}
        columns={cols}
        rowKey={(r: QuotaRow) => `${r.account_id}:${r.provider || ''}:${r.model || ''}:${r.limiter_type || ''}`}
        pagination={{ pageSize: 20 }}
        className="pool-quota-table"
        emptyTitle={t('quota.empty')}
        skeletonRows={8}
        skeletonCols={10}
        mobileRenderer={(row: QuotaRow) => {
          const primary = row.quota_summary?.primary || {};
          const usageDetails = quotaUsageDetails(row).map(({ kind, usedPercent }) => {
            const percentage = usedPercent == null ? null : Number(usedPercent);
            return {
              label: t(kind === '5h' ? 'quota.usage_5h' : 'quota.usage_7d'),
              value: percentage == null || !Number.isFinite(percentage) || percentage < 0
                ? t('common.unknown')
                : `${Math.round(percentage)}%`,
            };
          });
          const remaining = primary.remaining_tokens ?? row.remaining_tokens;
          return (
            <MobileRow
              title={row.label || row.account_id || t('common.unnamed_account')}
              subtitle={row.account_id}
              badges={<><Tag>{row.provider || t('common.unknown')}</Tag>{row.plan_type || row.plan_presentation ? <Tag>{formatPlanLabel(row.plan_type, row.plan_presentation)}</Tag> : null}</>}
              details={[
                ...usageDetails,
                { label: t('quota.remaining_tokens'), value: remaining == null || remaining < 0 ? t('common.unknown') : fmtTokens(remaining) },
                ...(() => {
                  const credit = extraCredits(row);
                  if (!credit) return [];
                  const value = credit.unlimited
                    ? t('quota.credits_unlimited_tag')
                    : credit.spendReached
                      ? t('quota.credits_spend_reached_tag')
                      : !credit.hasCredits
                        ? t('quota.credits_depleted_tag')
                        : credit.balance || t('quota.credits_available_tag');
                  const detail = [{ label: t('quota.credits_column'), value }];
                  if (credit.limit && credit.used) {
                    detail.push({
                      label: t('quota.credits_spend_limit'),
                      value: t('quota.credits_of').replace('{used}', credit.used).replace('{limit}', credit.limit),
                    });
                  }
                  return detail;
                })(),
                ...(() => {
                  const reset = resetCredits(row);
                  if (!reset) return [];
                  return [{
                    label: t('quota.reset_credits_column'),
                    value: reset.available === null ? t('common.unknown') : t('quota.credits_unit').replace('{count}', String(reset.available)),
                  }];
                })(),
                { label: t('quota.window'), value: row.limiter_type || '—' },
                { label: t('quota.sync_status'), value: row.quota_summary?.sync_reason || row.status || 'never_polled' },
                { label: t('quota.reset'), value: (primary.reset_at ?? row.reset_at) ? fmtRelative(primary.reset_at ?? row.reset_at) : '—' },
              ]}
            />
          );
        }}
        mobileListLabel={t('quota.mobile_label')}
      />
    </div>
  );
}
