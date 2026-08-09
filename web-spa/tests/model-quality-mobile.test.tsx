import React from 'react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { MemoryRouter } from 'react-router';
import { describe, expect, it, vi } from 'vitest';
import ModelQuality from '../src/pages/ModelQuality.jsx';
import { fmtTokens } from '../src/lib/format.js';
import { server } from './setup';

vi.mock('../src/hooks/useResponsiveLayout.js', () => ({
  default: () => ({ isMobile: true, isTablet: false, isDesktop: false }),
}));

function statusRow(index: number) {
  const suffix = String(index).padStart(2, '0');
  return {
    group_name: index > 20 ? 'legacy-team' : 'cyber',
    model: `quality-model-${suffix}`,
    provider: 'codex',
    state: index % 2 ? 'healthy' : 'suspect',
    last_outcome: index % 2 ? 'pass' : 'anomaly',
    consecutive_anomalies: index % 2 ? 0 : 1,
    last_expected: '42',
    last_actual: index % 2 ? '42' : '41',
    last_returned_model: `verified-model-${suffix}`,
    total_tokens: 1_200 + index,
    last_latency_ms: 180 + index,
    last_probe_at: 1_700_000_000 + index,
  };
}

describe('model quality mobile presentation', () => {
  it('keeps the first page compact, preserves pagination, and exposes every secondary field in a drawer', async () => {
    server.use(http.get('*/admin/model-quality', () => HttpResponse.json({
      enabled: true,
      interval_minutes: 60,
      reasoning_effort: 'medium',
      degraded_threshold: 2,
      running: false,
      statuses: Array.from({ length: 21 }, (_, index) => statusRow(index + 1)),
      runs: [],
    })));
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false, gcTime: 0 } },
    });

    render(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter>
          <ModelQuality />
        </MemoryRouter>
      </QueryClientProvider>,
    );

    expect(await screen.findByText('quality-model-01')).toBeInTheDocument();
    // Every query below is scoped to the mobile list. The page also renders a latency
    // ranking and a state legend, both of which are lists and both of which name models, so
    // page-wide queries would match the charts instead of the rows they mean to check --
    // including model 21, which is the slowest sample and so always appears in the ranking
    // regardless of which table page is showing.
    const rows = () => screen.getByRole('list', { name: '分组模型状态' });
    expect(within(rows()).getAllByRole('listitem')).toHaveLength(20);
    expect(within(rows()).queryByText('quality-model-21')).not.toBeInTheDocument();
    expect(within(rows()).queryByText('verified-model-01')).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: '下一页' }));
    expect(await within(rows()).findByText('quality-model-21')).toBeInTheDocument();
    expect(screen.getByText('2 / 2')).toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: '查看 quality-model-21 详情' }));
    const drawer = await screen.findByRole('dialog', { name: '模型质量 · quality-model-21' });
    expect(within(drawer).getByText('legacy-team')).toBeInTheDocument();
    expect(within(drawer).getByText('verified-model-21')).toBeInTheDocument();
    expect(within(drawer).getByText('42 / 42')).toBeInTheDocument();
    expect(within(drawer).getByText(fmtTokens(1_221))).toBeInTheDocument();
    expect(within(drawer).getByRole('button', { name: '立即检测' })).toBeEnabled();
  });
});
