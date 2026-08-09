import React from 'react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { MemoryRouter } from 'react-router';
import { describe, expect, it, vi } from 'vitest';
import System from '../src/pages/System.tsx';
import { server } from './setup';

vi.mock('../src/hooks/useResponsiveLayout.js', () => ({
  default: () => ({ isMobile: false, isTablet: false, isDesktop: true }),
}));

const HOUR = 3600;
const NOW = 1_760_000_000;

// Shapes follow the Go payload: sysmetrics.Snapshot for cpu/mem/disk/network/go/registration and
// api.DiskGuardSnapshot for disk_guard. Both are value types there, so production JSON always
// carries network and disk_guard -- the page must never fall back to a placeholder for them.
function payload() {
  return {
    supported: true,
    uptime_seconds: 456_789,
    cpu: { usage_pct: 18, cores: 4, load1: 0.42 },
    mem: { total_kb: 8_388_608, used_kb: 4_128_768, used_pct: 49 },
    disk: { path: '/', total_bytes: 68_719_476_736, used_bytes: 21_474_836_480, free_bytes: 47_244_640_256, used_pct: 31 },
    network: { interfaces: 3, rx_bytes_per_sec: 1_258_291, tx_bytes_per_sec: 389_120, total_bytes_per_sec: 1_647_411 },
    disk_guard: {
      level: 'pressure',
      free_percent: 12.4,
      free_bytes: 8_522_825_728,
      filesystems: [
        { roles: ['data', 'database'], level: 'pressure', free_percent: 12.4, free_bytes: 8_522_825_728 },
        { roles: ['journal'], level: 'normal', free_percent: 41.2, free_bytes: 28_306_407_424 },
      ],
      forced_context_ttl_seconds: 1_800,
      contexts_deleted: 1_284,
      goal_bytes_reclaimed: 2_254_857_830,
      last_run_at: NOW - 240,
      database_writable: true,
      journal_writable: true,
      spool_writable: true,
      background_paused: true,
      large_requests_paused: false,
      admission_blocked: false,
    },
    registration: {
      total_rss_kb: 196_608,
      node: 1,
      chrome: 2,
      xvfb: 1,
      procs: [
        { pid: 4101, comm: 'node', kind: 'node', rss_kb: 65_536 },
        { pid: 4108, comm: 'chrome', kind: 'chrome', rss_kb: 98_304 },
        { pid: 4126, comm: 'Xvfb', kind: 'xvfb', rss_kb: 8_192 },
      ],
    },
    go: { goroutines: 42, sys_bytes: 134_217_728 },
    // Every value of the module status enum, so the composition meter and the restart ranking
    // both have something to separate.
    supervisor_modules: [
      { name: 'mod-running', status: 'running', restart_count: 0, panic_count: 0, unexpected_exit_count: 0, uptime_millis: 456_000 },
      { name: 'mod-running-2', status: 'running', restart_count: 1, panic_count: 0, unexpected_exit_count: 0, uptime_millis: 256_000 },
      { name: 'mod-restarting', status: 'restarting', restart_count: 6, panic_count: 0, unexpected_exit_count: 5, restart_backoff_millis: 8_000 },
      { name: 'mod-panic', status: 'panic', restart_count: 11, panic_count: 4, unexpected_exit_count: 1, last_panic: 'index out of range' },
      { name: 'mod-stopped', status: 'stopped', restart_count: 0, panic_count: 0, unexpected_exit_count: 0 },
    ],
    supervisor_events: [
      { time_unix: NOW - 300, module: 'mod-running-2', type: 'event', message: 'heartbeat' },
      { time_unix: NOW - 900, module: 'mod-panic', type: 'panic_restart', message: 'panic', panic: 'index out of range' },
      { time_unix: NOW - 1_500, module: 'mod-restarting', type: 'unexpected_exit', message: 'socket closed' },
      { time_unix: NOW - 6 * HOUR, module: 'mod-panic', type: 'panic', message: 'panic', panic: 'index out of range' },
      { time_unix: NOW - 6 * HOUR, module: 'mod-running', type: 'event', message: 'rotation' },
    ],
  };
}

function renderPage(overrides: Record<string, unknown> = {}) {
  server.use(http.get('*/admin/system', () => HttpResponse.json({ ...payload(), ...overrides })));
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter>
        <System />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

const guardBars = () => screen.getByRole('list', { name: '各文件系统占用' });
const restartBars = () => screen.getByRole('list', { name: '重启次数排行' });

describe('system page visualisation', () => {
  it('ranks filesystems by usage on an absolute axis, fullest first', async () => {
    renderPage();
    const rows = within(await screen.findByRole('list', { name: '各文件系统占用' })).getAllByRole('listitem');

    // 12.4% free is 88% used and must sort above 41.2% free (59% used). Charting free space
    // instead would put the healthiest filesystem at the top with the longest bar.
    expect(rows).toHaveLength(2);
    expect(rows[0]).toHaveTextContent('数据目录 / 数据库');
    expect(rows[0]).toHaveTextContent('88%');
    expect(rows[0]).toHaveTextContent('剩余 12%');
    expect(rows[1]).toHaveTextContent('用量流水');
    expect(rows[1]).toHaveTextContent('59%');

    // The axis is pinned to 100, so the fullest filesystem is 88% of the track rather than all
    // of it -- otherwise the widest row always looks full no matter what it holds.
    const fill = rows[0].querySelector('.pool-ranked__track > span') as HTMLElement | null;
    expect(fill).not.toBeNull();
    expect(fill?.style.width).toBe('88%');
  });

  it('shows only degraded guard flags and keeps the nominal case to one chip', async () => {
    renderPage();
    await screen.findByRole('list', { name: '各文件系统占用' });

    expect(screen.getByText('已暂停后台任务')).toBeInTheDocument();
    // large_requests_paused and admission_blocked are false and all three writable flags true,
    // so none of them may render -- listing what is fine trains the eye past what is not.
    for (const absent of ['已暂停大请求', '已拒绝新请求', '数据库不可写', '流水不可写', '缓存不可写', '读写正常']) {
      expect(screen.queryByText(absent)).not.toBeInTheDocument();
    }
  });

  it('collapses to a single healthy chip when nothing is degraded', async () => {
    const guard = { ...payload().disk_guard, background_paused: false };
    renderPage({ disk_guard: guard });
    await screen.findByRole('list', { name: '各文件系统占用' });

    expect(screen.getByText('读写正常')).toBeInTheDocument();
    expect(screen.queryByText('已暂停后台任务')).not.toBeInTheDocument();
  });

  it('counts module states and ranks restarts ahead of current status', async () => {
    renderPage();
    await screen.findByRole('list', { name: '各文件系统占用' });

    expect(screen.getByText('2 / 5 个模块运行中')).toBeInTheDocument();

    // mod-panic restarted 11 times and outranks mod-restarting at 6; mod-running-2 is running
    // yet still appears because one restart is worth reporting. The two modules that never
    // restarted are absent.
    const rows = within(restartBars()).getAllByRole('listitem');
    expect(rows).toHaveLength(3);
    expect(rows[0]).toHaveTextContent('mod-panic');
    // The counts are labelled in the UI language, not with the Go field name. `panic` as a bare
    // English word was the only untranslated string in an otherwise Chinese meta line.
    expect(rows[0]).toHaveTextContent('崩溃 4');
    expect(rows[1]).toHaveTextContent('mod-restarting');
    expect(rows[2]).toHaveTextContent('mod-running-2');
    expect(within(restartBars()).queryByText('mod-stopped')).not.toBeInTheDocument();
  });

  it('separates problem events from routine ones on the 24 hour strip', async () => {
    renderPage();
    await screen.findByRole('list', { name: '各文件系统占用' });

    expect(screen.getByRole('img', { name: '近 24 小时每小时事件次数' })).toBeInTheDocument();
    expect(screen.getByRole('img', { name: '近 24 小时每小时异常事件次数' })).toBeInTheDocument();
    // panic_restart, unexpected_exit and panic count; the two plain events do not.
    expect(screen.getByText(/共 3 次异常事件/)).toBeInTheDocument();
  });

  it('renders network throughput and the Go runtime instead of a zero placeholder', async () => {
    renderPage();
    await screen.findByRole('list', { name: '各文件系统占用' });

    expect(screen.getByText('1.6 MB/s')).toBeInTheDocument();
    expect(screen.getByText(/接收 1\.2 MB\/s/)).toBeInTheDocument();
    expect(screen.getByText(/3 个网卡/)).toBeInTheDocument();
    expect(screen.getByText(/goroutine · 内存 128 MB/)).toBeInTheDocument();
  });

  it('ranks helper processes by resident memory', async () => {
    renderPage();
    const rows = within(await screen.findByRole('list', { name: '子进程内存排行' })).getAllByRole('listitem');

    expect(rows).toHaveLength(3);
    expect(rows[0]).toHaveTextContent('chrome');
    expect(rows[0]).toHaveTextContent('96 MB');
    expect(rows[1]).toHaveTextContent('node');
    expect(rows[2]).toHaveTextContent('Xvfb');
  });

  it('omits the guard card entirely when the payload has no guard snapshot', async () => {
    renderPage({ disk_guard: undefined });
    await screen.findByRole('list', { name: '重启次数排行' });

    expect(screen.queryByRole('list', { name: '各文件系统占用' })).not.toBeInTheDocument();
    expect(screen.queryByText('磁盘空间守卫')).not.toBeInTheDocument();
  });
});
