import React from 'react';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { MemoryRouter } from 'react-router';
import { describe, expect, it, vi } from 'vitest';
import EmailPool from '../src/pages/EmailPool';
import { server } from './setup';

vi.mock('../src/hooks/useResponsiveLayout.js', () => ({
  default: () => ({ isMobile: true, isTablet: false, isDesktop: false }),
}));

const longEmail = 'registration-automation-production-owner-with-an-extraordinarily-long-identity@subdomain.sample.test';

describe('email pool responsive presentation', () => {
  it('hard-clips every desktop text cell so long mailbox identities cannot cross columns', () => {
    const css = readFileSync(resolve(process.cwd(), 'src/styles/components.css'), 'utf8');
    expect(css).toMatch(/\.pool-email-table \.pool-table td\s*\{[^}]*overflow:\s*hidden;/s);
    expect(css).toMatch(/\.pool-email-table \.pool-text-clamp\s*\{[^}]*display:\s*block;[^}]*overflow:\s*hidden;[^}]*text-overflow:\s*ellipsis;[^}]*white-space:\s*nowrap;/s);
    expect(css).toMatch(/\.pool-email-address\s*\{[^}]*display:\s*block;[^}]*width:\s*100%;[^}]*overflow:\s*hidden;/s);
  });

  it('uses compact mobile cards while preserving long identities, status, selection, and row actions', async () => {
    server.use(http.get('*/admin/email-pool', () => HttpResponse.json({
      accounts: [
        {
          id: 'mail-1',
          email: longEmail,
          status: 'ready',
          group_name: 'production-registration-group-with-a-long-name',
          error_message: '',
          last_used_at: 1_700_000_000,
          created_at: 1_699_000_000,
          updated_at: 1_700_000_000,
        },
        {
          id: 'mail-2',
          email: 'second-account@sample.test',
          status: 'error',
          group_name: '',
          error_message: 'token refresh was rejected by the upstream mailbox provider',
          created_at: 1_699_000_000,
          updated_at: 1_700_000_000,
        },
      ],
      total: 2,
      page: 1,
      pageSize: 50,
      counts: { ready: 1, error: 1 },
    })));
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false, gcTime: 0 } },
    });

    render(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter>
          <EmailPool />
        </MemoryRouter>
      </QueryClientProvider>,
    );

    const list = await screen.findByRole('list', { name: '邮箱账号列表' });
    expect(list).toHaveClass('pool-email-table');
    expect(within(list).getAllByRole('listitem')).toHaveLength(2);
    expect(screen.queryByRole('table')).not.toBeInTheDocument();
    expect(screen.getByLabelText(longEmail)).toBeInTheDocument();
    expect(within(list).getByText('可用')).toBeInTheDocument();
    expect(within(list).getByText('异常')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: `测试 ${longEmail}` })).toBeEnabled();
    expect(screen.getByRole('button', { name: `删除 ${longEmail}` })).toBeEnabled();

    const metrics = screen.getByRole('complementary', { name: '指标摘要' });
    expect(within(metrics).getByText('邮箱总数')).toBeInTheDocument();
    expect(within(metrics).getAllByText('占总量 50%')).toHaveLength(2);

    fireEvent.click(screen.getByRole('button', { name: '选择本页' }));
    expect(await screen.findByRole('button', { name: '删除所选（2）' })).toBeEnabled();
    expect(screen.getByRole('button', { name: '取消本页' })).toHaveAttribute('aria-pressed', 'true');
  });
});
