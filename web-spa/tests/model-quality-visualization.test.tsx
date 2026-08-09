import React from 'react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { MemoryRouter } from 'react-router';
import { describe, expect, it, vi } from 'vitest';
import ModelQuality from '../src/pages/ModelQuality.jsx';
import { server } from './setup';

vi.mock('../src/hooks/useResponsiveLayout.js', () => ({
  default: () => ({ isMobile: false, isTablet: false, isDesktop: true }),
}));

const HOUR = 3600;
const NOW = 1_760_000_000;

// The two outcome enums the Go handler actually emits, taken from
// internal/api/model_quality.go: statuses[].last_outcome is one of
// pass / false_alarm / error / inconclusive / confirmed_anomaly, and runs[].outcome is one of
// pass / error / model_mismatch / incorrect. Any value the page does not map falls through to
// the raw identifier, which is the bug this file guards.
function payload() {
  return {
    enabled: true,
    running: false,
    interval_minutes: 60,
    reasoning_effort: 'medium',
    degraded_threshold: 2,
    statuses: [
      { group_name: 'alpha', model: 'model-fast', provider: 'codex', state: 'healthy', last_outcome: 'pass', consecutive_anomalies: 0, total_checks: 12, total_tokens: 4_000, last_latency_ms: 300, last_probe_at: NOW - 600 },
      { group_name: 'alpha', model: 'model-slow', provider: 'codex', state: 'degraded', last_outcome: 'confirmed_anomaly', consecutive_anomalies: 3, total_checks: 9, total_tokens: 3_000, last_latency_ms: 4_800, last_probe_at: NOW - 900 },
      { group_name: 'beta', model: 'model-flaky', provider: 'claude', state: 'suspect', last_outcome: 'false_alarm', consecutive_anomalies: 1, total_checks: 7, total_tokens: 2_000, last_latency_ms: 1_200, last_probe_at: NOW - 1_200 },
      { group_name: 'beta', model: 'model-down', provider: 'claude', state: 'unavailable', last_outcome: 'inconclusive', consecutive_anomalies: 0, consecutive_errors: 3, total_checks: 4, total_tokens: 0, last_latency_ms: 2_000, last_probe_at: NOW - 1_800 },
      { group_name: 'beta', model: 'model-new', provider: 'claude', state: 'unknown', last_outcome: '', consecutive_anomalies: 0, total_checks: 0, total_tokens: 0, last_latency_ms: 0, last_probe_at: 0 },
    ],
    runs: [
      { id: 1, created_at: NOW - 600, group_name: 'alpha', model: 'model-fast', phase: 'primary', outcome: 'pass', expected: '9', actual: '9', total_tokens: 500, latency_ms: 300 },
      { id: 2, created_at: NOW - 900, group_name: 'alpha', model: 'model-slow', phase: 'confirmation', outcome: 'incorrect', expected: '9', actual: '8', total_tokens: 400, latency_ms: 4_800 },
      { id: 3, created_at: NOW - 1_200, group_name: 'beta', model: 'model-flaky', phase: 'primary', outcome: 'model_mismatch', expected: '9', actual: '9', returned_model: 'other-model', total_tokens: 350, latency_ms: 1_200 },
      { id: 4, created_at: NOW - 1_800, group_name: 'beta', model: 'model-down', phase: 'primary', outcome: 'error', expected: '9', actual: '', total_tokens: 0, latency_ms: 2_000, error_kind: 'upstream_timeout' },
      { id: 5, created_at: NOW - 12 * HOUR, group_name: 'alpha', model: 'model-fast', phase: 'primary', outcome: 'pass', expected: '4', actual: '4', total_tokens: 480, latency_ms: 320 },
    ],
  };
}

function renderPage() {
  server.use(http.get('*/admin/model-quality', () => HttpResponse.json(payload())));
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter>
        <ModelQuality />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe('model quality outcome contract', () => {
  it('labels every outcome the backend can emit instead of leaking the raw identifier', async () => {
    renderPage();
    await screen.findByRole('list', { name: '分组模型响应延迟对比' });

    // One assertion per value in both enums. Before the fix only pass and error were mapped,
    // so the other five rendered as English identifiers in neutral grey -- including
    // confirmed_anomaly, the one value that means the model really is degraded.
    for (const label of ['通过', '复核通过', '复核异常', '答案错误', '模型不符', '复核失败', '请求错误']) {
      expect(screen.getAllByText(label).length).toBeGreaterThan(0);
    }
    for (const raw of ['false_alarm', 'confirmed_anomaly', 'inconclusive', 'model_mismatch', 'incorrect']) {
      expect(screen.queryByText(raw)).not.toBeInTheDocument();
    }
  });

  it('derives the pass rate from measured runs only, excluding transport errors', async () => {
    renderPage();
    await screen.findByRole('list', { name: '分组模型响应延迟对比' });

    // runs hold 2 pass, 2 anomalies (incorrect + model_mismatch) and 1 error. Only the first
    // four measured anything, so the rate is 2/4 = 50%. Counting the error would give 2/5 =
    // 40% and would mark a model down for its upstream being unreachable.
    expect(screen.getByRole('img', { name: '分组模型健康状态构成' })).toBeInTheDocument();
    expect(screen.getByText('4 次有效检测')).toBeInTheDocument();
    expect(screen.getByText('50%')).toBeInTheDocument();
  });

  it('ranks latency worst-first and reports the untested count separately from health', async () => {
    renderPage();
    await screen.findByRole('list', { name: '分组模型响应延迟对比' });

    const ranking = screen.getByRole('list', { name: '分组模型响应延迟对比' });
    const names = within(ranking).getAllByRole('listitem').map((item) => item.textContent || '');
    expect(names[0]).toContain('model-slow');
    expect(names[0]).toContain('4,800 ms');
    // model-new has no latency sample, so it must not occupy a bar at zero.
    expect(names.some((text) => text.includes('model-new'))).toBe(false);

    // The untested combination is reported as a count, not as a fifth meter colour.
    expect(screen.getByText('4 / 5 个分组模型已检测，1 个待首轮抽样')).toBeInTheDocument();
  });

  it('plots volume and anomalies on one 24-hour axis', async () => {
    renderPage();
    await screen.findByRole('list', { name: '分组模型响应延迟对比' });

    expect(screen.getByRole('img', { name: '近 24 小时每小时检测次数' })).toBeInTheDocument();
    expect(screen.getByRole('img', { name: '近 24 小时每小时异常次数' })).toBeInTheDocument();
    // incorrect + model_mismatch are anomalies; error is not.
    expect(screen.getByText(/共 2 次异常/)).toBeInTheDocument();
  });
});
