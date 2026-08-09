import React from 'react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { MemoryRouter } from 'react-router';
import { describe, expect, it, vi } from 'vitest';
import Registration from '../src/pages/Registration.tsx';
import { server } from './setup';

vi.mock('../src/hooks/useResponsiveLayout.js', () => ({
  default: () => ({ isMobile: false, isTablet: false, isDesktop: true }),
}));

// Durations are measured against the wall clock for a job that has not finished, so the fixture
// is anchored to "now" rather than to a constant: a running job started 300s ago must read 5分
// whenever the suite happens to execute.
const NOW = Math.floor(Date.now() / 1000);

// Every status internal/api/registration.go can persist, in one payload. `queued` is what
// HandleRegisterBatch writes on insert and `completed_with_review` is what a partially
// successful batch settles to -- the page used to map neither. Fields are the ones
// HandleJobList's inline `job` struct marshals, which is why no row carries group_name or
// egress_id: those are not in the response and any column rendering them can only print a
// fallback.
function jobs() {
  return [
    { id: 'job-queued', platform: 'chatgpt', method: 'protocol_v2', total: 4, succeeded: 0, failed: 0, status: 'queued', started_at: 0, completed_at: 0, error: '', created_at: NOW - 30 },
    { id: 'job-running', platform: 'chatgpt', method: 'protocol_v2', total: 10, succeeded: 3, failed: 1, status: 'running', started_at: NOW - 300, completed_at: 0, error: '', created_at: NOW - 320 },
    { id: 'job-review', platform: 'chatgpt', method: 'browser_v3', total: 6, succeeded: 4, failed: 2, status: 'completed_with_review', started_at: NOW - 1_800, completed_at: NOW - 1_200, error: '', created_at: NOW - 1_860 },
    { id: 'job-done', platform: 'chatgpt', method: 'protocol_v2', total: 5, succeeded: 5, failed: 0, status: 'completed', started_at: NOW - 3_600, completed_at: NOW - 3_558, error: '', created_at: NOW - 3_640 },
    { id: 'job-failed', platform: 'chatgpt', method: 'node', total: 8, succeeded: 0, failed: 3, status: 'failed', started_at: NOW - 7_200, completed_at: NOW - 7_000, error: 'sms provider returned no numbers for BR after 3 attempts', created_at: NOW - 7_260 },
  ];
}

// Shape is provider.SMSMarketCandidate with SMSPriceSnapshot embedded. The ordering here is the
// backend's (success rate first), so the success chart must not re-sort and the price chart must.
function marketItems() {
  return [
    { provider: 'smsbower', service: 'dr', country_id: '73', country_iso: 'BR', country_name: 'Brazil', price: 0.05, inventory: 1_200, provider_rank: 1, balance: 12.5, fetched_at: NOW - 600, attempts: 40, succeeded: 36, success_rate: 0.9, score: 0.88, eligible: true, selection_basis: 'measured_success_rate' },
    { provider: 'herosms', service: 'dr', country_id: '39', country_iso: 'CO', country_name: 'Colombia', price: 0.09, inventory: 640, provider_rank: 2, balance: 8.25, fetched_at: NOW - 600, attempts: 20, succeeded: 14, success_rate: 0.7, score: 0.66, eligible: true, selection_basis: 'measured_success_rate' },
    { provider: 'smsbower', service: 'dr', country_id: '15', country_iso: 'PL', country_name: 'Poland', price: 0.1, inventory: 180, provider_rank: 3, balance: 12.5, fetched_at: NOW - 600, attempts: 13, succeeded: 5, success_rate: 0.38, score: 0.35, eligible: true, selection_basis: 'measured_success_rate' },
    { provider: 'herosms', service: 'dr', country_id: '6', country_iso: 'ID', country_name: 'Indonesia', price: 0.02, inventory: 90, provider_rank: 4, balance: 8.25, fetched_at: NOW - 600, attempts: 1, succeeded: 1, success_rate: 0.5, score: 0.3, eligible: true, selection_basis: 'community_cold_start' },
    { provider: 'smsbower', service: 'dr', country_id: '187', country_iso: 'US', country_name: 'United States', price: 0.4, inventory: 40, provider_rank: 5, balance: 12.5, fetched_at: NOW - 600, attempts: 30, succeeded: 27, success_rate: 0.9, score: 0, eligible: false, selection_basis: 'measured_success_rate' },
  ];
}

function market() {
  return {
    items: marketItems(),
    min_price: 0.01,
    max_price: 0.25,
    preferred_countries: ['BR', 'CO', 'PL'],
    cold_start_policy: 'community_recommended_order',
    history_window_days: 14,
    minimum_history_samples: 3,
    refresh_interval_seconds: 3_600,
    last_refreshed_at: NOW - 600,
    stale: false,
    refreshed_rows: 5,
    warning: '',
  };
}

function readiness() {
  return {
    ready: true,
    registration_enabled: true,
    providers: { mailbox: 2, email_otp: 1, sms: 2, captcha: 1 },
    blockers: [],
    pool: { id: 'pool_registration_residential', healthy: 3, total: 4 },
  };
}

interface Overrides {
  jobs?: unknown;
  market?: Record<string, unknown>;
}

function renderPage(overrides: Overrides = {}) {
  server.use(
    http.get('*/admin/register/batch', () => HttpResponse.json({ jobs: overrides.jobs ?? jobs() })),
    http.get('*/admin/register/readiness', () => HttpResponse.json(readiness())),
    http.get('*/admin/register/sms-market', () => HttpResponse.json({ ...market(), ...(overrides.market || {}) })),
    http.get('*/admin/register/countries', () => HttpResponse.json([{ iso_code: 'BR', country_name: 'Brazil', name_zh: '巴西' }])),
    http.get('*/admin/register/providers/options', () => HttpResponse.json({ sms: [], mailbox: [], captcha: [] })),
    http.get('*/admin/groups', () => HttpResponse.json([])),
    http.get('*/admin/egress-pools', () => HttpResponse.json([])),
    http.get('*/admin/config', () => HttpResponse.json({ sms_platform_strategy: 'auto', default_register_method: 'protocol_v2', sms_min_price: 0.01, sms_max_price: 0.25 })),
  );
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter>
        <Registration />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

const jobRows = () => within(screen.getByRole('table')).getAllByRole('row').slice(1);
const rowFor = (id: string) => jobRows().find((row) => row.textContent?.includes(id)) as HTMLElement;
// 成功 and 失败 each appear three times on this page -- as a rail label, inside every row's
// progress cell, and in the outcome legend -- so the rail is addressed through its own landmark
// rather than by text alone.
const rail = () => within(screen.getByRole('complementary', { name: '指标摘要' }));
const metricCard = (label: string) => rail().getByText(label).closest('.pool-metric-card') as HTMLElement;
const cardTrack = (label: string) => metricCard(label).querySelector('.pool-metric-card__track > span') as HTMLElement;
const successBars = () => screen.findByRole('list', { name: '各国近期成功率' });
const priceBars = () => screen.getByRole('list', { name: '各国单价' });
const fillWidth = (row: HTMLElement) => (row.querySelector('.pool-ranked__track > span') as HTMLElement | null)?.style.width;

describe('registration job status contract', () => {
  it('labels every status the backend can persist instead of leaking the raw identifier', async () => {
    renderPage();
    await screen.findByText('job-queued');

    // queued and completed_with_review were both missing from the colour map, so one read as an
    // untranslated grey `queued` and the other reported the same neutral grey as a job that had
    // not started.
    expect(within(rowFor('job-queued')).getByText('排队中')).toBeInTheDocument();
    expect(within(rowFor('job-running')).getByText('进行中')).toBeInTheDocument();
    expect(within(rowFor('job-review')).getByText('完成待复核')).toBeInTheDocument();
    expect(within(rowFor('job-done')).getByText('已完成')).toBeInTheDocument();
    expect(within(rowFor('job-failed')).getByText('失败')).toBeInTheDocument();
    for (const raw of ['queued', 'completed_with_review', 'cancelled']) {
      expect(screen.queryByText(raw)).not.toBeInTheDocument();
    }
  });

  it('separates a half-successful batch from an untouched one by colour', async () => {
    renderPage();
    await screen.findByText('job-review');

    // Grey is "nothing has happened here". A batch that produced four accounts and needs two
    // reviewed is not that, and it is not a clean green either.
    expect(within(rowFor('job-review')).getByText('完成待复核')).toHaveClass('pool-tag--amber');
    expect(within(rowFor('job-queued')).getByText('排队中')).toHaveClass('pool-tag--grey');
    expect(within(rowFor('job-done')).getByText('已完成')).toHaveClass('pool-tag--green');
    expect(within(rowFor('job-failed')).getByText('失败')).toHaveClass('pool-tag--red');
  });

  it('counts a queued job as in flight', async () => {
    renderPage();
    await screen.findByText('job-queued');

    // The rail filtered on pending/running only, so the 运行中 card read 0 for the first seconds
    // after pressing start -- exactly when it is being watched. queued + running is 2 of 5.
    expect(within(metricCard('运行中')).getByText('2')).toBeInTheDocument();
    expect(cardTrack('运行中').style.width).toBe('40%');
  });

  it('reports success and failure as a share of what settled, not of the job count', async () => {
    renderPage();
    await screen.findByText('job-queued');

    // 12 succeeded and 6 failed across the page: 67% / 33% of the 18 attempts that resolved.
    // Dividing by the five jobs instead would put both bars under a tenth of the track.
    expect(within(metricCard('成功')).getByText('12')).toBeInTheDocument();
    expect(cardTrack('成功').style.width).toBe('67%');
    expect(within(metricCard('失败')).getByText('6')).toBeInTheDocument();
    expect(cardTrack('失败').style.width).toBe('33%');
    // A total is the denominator of nothing, so it stays a plain number with no track.
    expect(metricCard('任务数').querySelector('.pool-metric-card__track')).toBeNull();
  });
});

describe('registration job timing', () => {
  it('reports how long each batch ran instead of a constant', async () => {
    renderPage();
    await screen.findByText('job-running');

    // The column this replaced rendered group_name and egress_id, neither of which HandleJobList
    // marshals -- 220px of 默认分组 / 默认出口 repeated down every row.
    expect(within(rowFor('job-running')).getByText('5分')).toBeInTheDocument();
    expect(within(rowFor('job-review')).getByText('10分')).toBeInTheDocument();
    expect(screen.queryByText('默认分组')).not.toBeInTheDocument();
    expect(screen.queryByText('默认出口')).not.toBeInTheDocument();
  });

  it('reports a sub-minute batch in seconds rather than as zero', async () => {
    renderPage();
    await screen.findByText('job-done');

    // fmtDuration floors to whole minutes, which would print 0分 for a 42-second batch.
    expect(within(rowFor('job-done')).getByText('42秒')).toBeInTheDocument();
  });

  it('leaves the duration blank for a job that has not started', async () => {
    renderPage();
    await screen.findByText('job-queued');

    // started_at is 0 until the job is picked up, so there is no duration to report -- measuring
    // from the epoch would print 57 years.
    expect(within(rowFor('job-queued')).getByText('—')).toBeInTheDocument();
    expect(screen.queryByText(/1970/)).not.toBeInTheDocument();
  });
});

describe('registration failure reporting', () => {
  it('shows why a batch failed in the row and in full in the drawer', async () => {
    renderPage();
    await screen.findByText('job-failed');

    const reason = 'sms provider returned no numbers for BR after 3 attempts';
    // The payload carried `error` and nothing rendered it, so a failed job was a red tag and
    // nothing else -- in the table and in the drawer.
    const inRow = within(rowFor('job-failed')).getByText(reason);
    expect(inRow).toHaveAttribute('title', reason);

    fireEvent.click(rowFor('job-failed'));
    const drawer = await screen.findByRole('dialog');
    const failure = drawer.querySelector('.pool-task-detail__failure') as HTMLElement;
    expect(failure).not.toBeNull();
    expect(within(failure).getByText(reason)).toBeInTheDocument();
    // Prose, not a value: it sits outside the definition grid so it wraps instead of being
    // clamped to one line.
    expect(failure.closest('.pool-task-detail__grid')).toBeNull();
  });

  it('omits the timestamps a job has not reached rather than dating them to 1970', async () => {
    renderPage();
    await screen.findByText('job-queued');

    fireEvent.click(rowFor('job-queued'));
    const drawer = await screen.findByRole('dialog');
    expect(within(drawer).queryByText('开始时间')).not.toBeInTheDocument();
    expect(within(drawer).queryByText('结束时间')).not.toBeInTheDocument();
    expect(within(drawer).queryByText(/1970/)).not.toBeInTheDocument();
    // A failed reason is absent too, rather than rendering an empty block.
    expect(drawer.querySelector('.pool-task-detail__failure')).toBeNull();
  });
});

describe('registration outcome composition', () => {
  it('charts attempts, counting outstanding work only for jobs still in flight', async () => {
    renderPage();
    await screen.findByRole('img', { name: '产出构成' });

    const legend = within(screen.getByRole('img', { name: '产出构成' }).closest('.pool-stacked-meter') as HTMLElement);
    expect(legend.getByText('成功').nextElementSibling).toHaveTextContent('12 个号');
    expect(legend.getByText('失败').nextElementSibling).toHaveTextContent('6 个号');
    // Only queued (4 of 4) and running (6 of 10) have work ahead of them. The failed job's
    // 5-account shortfall is not "remaining": that batch is over.
    expect(legend.getByText('待完成').nextElementSibling).toHaveTextContent('10 个号');
  });

  it('explains the empty case instead of leaving a blank panel', async () => {
    renderPage({ jobs: [] });
    await screen.findByText('暂无注册任务');

    // StackedMeter returns null on a zero total, so without this line the section is a heading
    // over nothing.
    expect(screen.getByText('启动任务后这里显示成功与失败的构成')).toBeInTheDocument();
    expect(screen.queryByRole('img', { name: '产出构成' })).not.toBeInTheDocument();
  });
});

describe('sms market ranking', () => {
  it('keeps the backend ranking on the success chart and re-sorts the price chart', async () => {
    renderPage();
    const rows = within(await screen.findByRole('list', { name: '各国近期成功率' })).getAllByRole('listitem');

    // The handler returns candidates already ranked; re-sorting here would show an order the
    // scheduler does not use.
    expect(rows.map((row) => row.textContent?.slice(0, 2))).toEqual(['BR', 'CO', 'PL', 'ID', 'US']);
    // Price has no meaningful ceiling and the operator wants the cheapest first, so this one is
    // sorted ascending.
    const priced = within(priceBars()).getAllByRole('listitem');
    expect(priced.map((row) => row.textContent?.slice(0, 2))).toEqual(['ID', 'BR', 'CO', 'PL', 'US']);
    expect(within(priced[0]).getByText('$0.020')).toBeInTheDocument();
    expect(within(priced[4]).getByText('$0.400')).toBeInTheDocument();
  });

  it('pins the success axis to 100 so bar length survives a refresh', async () => {
    renderPage();
    const rows = within(await successBars()).getAllByRole('listitem');
    // Normalising to the best row would draw the leader full-width whatever it measured: 90%
    // and 38% would both look complete on a page where they were the maximum.
    expect(fillWidth(rows[0])).toBe('90%');
    expect(fillWidth(rows[2])).toBe('38%');
  });

  it('gives the price axis to the countries actually in contention', async () => {
    renderPage();
    await successBars();
    const priced = within(priceBars()).getAllByRole('listitem');

    // Two ways this axis went wrong. RankedBars floors its own axis at 1, so against a $1.00
    // ceiling the whole cent-scale board sat in the left tenth of the track. Handing it the
    // priciest row instead put US at $0.40 in charge, which squeezed the four rows the scheduler
    // can choose between into the first quarter. The axis is the priciest eligible row: PL.
    expect(fillWidth(priced[3])).toBe('100%');
    expect(fillWidth(priced[2])).toBe('90%');
    expect(fillWidth(priced[1])).toBe('50%');
    expect(fillWidth(priced[0])).toBe('20%');
    // The excluded row pegs at full width -- which is what "off the scale" means -- and it is
    // already grey and labelled, so it cannot be misread as the cheapest.
    expect(fillWidth(priced[4])).toBe('100%');
    expect(priced[4]).toHaveTextContent('超出价格区间');
  });

  it('falls back to the whole board when the price window excludes everything', async () => {
    const items = marketItems().map((item) => ({ ...item, eligible: false }));
    renderPage({ market: { items } });
    await successBars();
    const priced = within(priceBars()).getAllByRole('listitem');

    // No eligible row means no eligible maximum. Ranking the board is still worth more than
    // collapsing every bar against a $1.00 default.
    expect(fillWidth(priced[4])).toBe('100%');
    expect(fillWidth(priced[1])).toBe('12.5%');
  });

  it('marks a rate the backend guessed as a cold start rather than a measurement', async () => {
    renderPage();
    const rows = within(await successBars()).getAllByRole('listitem');

    // ID has 1 attempt against minimum_history_samples of 3, so its 50% is the community
    // fallback, not history. Printing 1/1 next to it would claim a measurement that does not exist.
    expect(rows[3]).toHaveTextContent('冷启动');
    expect(rows[3]).not.toHaveTextContent('1/1');
    // Measured rows show the sample they were computed from.
    expect(rows[0]).toHaveTextContent('36/40');
    expect(rows[0]).toHaveTextContent('库存 1,200');
  });

  it('keeps a country the price window excluded, and says why', async () => {
    renderPage();
    const rows = within(await successBars()).getAllByRole('listitem');

    // US measures 90% and is still not chosen. Hiding it leaves the operator without the reason
    // the scheduler passed over the best success rate on the board.
    expect(rows[4]).toHaveTextContent('US');
    expect(rows[4]).toHaveTextContent('超出价格区间');
    expect((rows[4].querySelector('.pool-ranked__dot') as HTMLElement).style.background).toBe('var(--chart-gray)');
    // Eligible rows are coloured by measured band, not all one accent.
    expect((rows[0].querySelector('.pool-ranked__dot') as HTMLElement).style.background).toBe('var(--chart-green)');
    expect((rows[2].querySelector('.pool-ranked__dot') as HTMLElement).style.background).toBe('var(--chart-red)');
  });

  it('reports staleness against the refresh it actually got', async () => {
    renderPage({ market: { stale: true } });
    await screen.findByRole('list', { name: '各国单价' });

    expect(screen.getByText(/数据待刷新/)).toBeInTheDocument();
    expect(screen.queryByText(/价格已同步/)).not.toBeInTheDocument();
  });

  it('says the market has never been scanned instead of dating it to the epoch', async () => {
    renderPage({ market: { last_refreshed_at: 0, items: [] } });
    await screen.findByText('等待首次价格扫描');

    expect(screen.queryByText(/1970/)).not.toBeInTheDocument();
    // Two empty charts, each saying so, rather than two bare headings.
    expect(screen.getAllByText(/保存接码平台凭据后/)).toHaveLength(2);
  });
});
