import React from 'react';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { MemoryRouter } from 'react-router';
import { describe, expect, it, vi } from 'vitest';
import Users from '../src/pages/Users';
import { server } from './setup';

vi.mock('../src/hooks/useResponsiveLayout.js', () => ({
  default: () => ({ isMobile: false, isTablet: false, isDesktop: true }),
}));

const longEmail =
  'team-child-registration-identity-with-a-very-long-alias@subdomain.sample.test';

describe('users responsive presentation', () => {
  it('keeps long user identities inside the desktop email column', async () => {
    server.use(
      http.get('*/admin/users', () =>
        HttpResponse.json({
          users: [
            {
              id: 'user-long-1',
              email: longEmail,
              name: 'Registration lifecycle child account with a long name',
              role: 'user',
              status: 'active',
              created_at: 1_700_000_000,
            },
          ],
        }),
      ),
    );
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false, gcTime: 0 } },
    });

    render(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter>
          <Users />
        </MemoryRouter>
      </QueryClientProvider>,
    );

    const email = await screen.findByText(longEmail);
    expect(email).toHaveClass(
      'pool-text-clamp',
      'pool-text-clamp--strong',
      'pool-user-identity',
    );
    expect(email.closest('td')).toHaveAttribute('data-label', '邮箱');
  });

  it('clips long user cells and the shell account identity at their boundaries', () => {
    const css = readFileSync(
      resolve(process.cwd(), 'src/styles/components.css'),
      'utf8',
    );
    expect(css).toMatch(
      /\.pool-users-table \.pool-table td\s*\{[^}]*overflow:\s*hidden;/s,
    );
    expect(css).toMatch(
      /\.pool-users-table \.pool-user-identity\s*\{[^}]*display:\s*block;[^}]*width:\s*100%;[^}]*overflow:\s*hidden;[^}]*text-overflow:\s*ellipsis;[^}]*white-space:\s*nowrap;/s,
    );
    expect(css).toMatch(
      /\.pool-account-menu-button \.pool-button__label\s*\{[^}]*display:\s*flex;[^}]*min-width:\s*0;[^}]*max-width:\s*100%;/s,
    );
    expect(css).toMatch(
      /\.pool-account-menu-button \.pool-account-menu-ident\s*\{[^}]*flex:\s*1 1 auto;[^}]*overflow:\s*hidden;[^}]*text-overflow:\s*ellipsis;/s,
    );
  });
});
