import React from 'react';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { render, screen } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { MemoryRouter } from 'react-router';
import { describe, expect, it } from 'vitest';
import TeamLifecycle from '../src/pages/TeamLifecycle';
import { server } from './setup';

const longChildReference =
  'child-account-reference-with-a-very-long-identity-segment-for-responsive-layout-verification';

describe('team lifecycle presentation', () => {
  it('renders a durable flow, explicit quota threshold, and clamped identities', async () => {
    server.use(
      http.get('*/admin/team-lifecycle/workspaces', () =>
        HttpResponse.json({
          items: [{
            id: 'workspace-fixture',
            name: 'Primary lifecycle workspace',
            parent_account_id: 'parent-ref',
            workspace_ref: 'remote-ref',
            connector_kind: 'fixture',
            max_members: 8,
            status: 'active',
            updated_at: 1_800_000_000,
          }],
        }),
      ),
      http.get('*/admin/team-lifecycle/workflows', () =>
        HttpResponse.json({
          items: [{
            id: 'teamwf-fixture',
            workspace_id: 'workspace-fixture',
            parent_account_id: 'parent-ref',
            child_account_id: longChildReference,
            state: 'active',
            credential_path: 'access_reference',
            imported_account_id: 'imported-ref',
            quota_remaining_bps: 75,
            rotate_threshold_bps: 100,
            attempt: 0,
            max_attempts: 5,
            next_attempt_at: 1_800_000_100,
            error_class: '',
            shadow_mode: false,
            version: 9,
            created_at: 1_800_000_000,
            updated_at: 1_800_000_050,
            completed_at: 0,
          }],
        }),
      ),
      http.get('*/admin/team-lifecycle/stats', () =>
        HttpResponse.json({
          states: { active: 1 },
          readiness: {
            ready: true,
            workspace_create_ready: true,
            cycle_create_ready: true,
            parent_accounts: 1,
            mailbox_profiles: 1,
            mailbox_default_configured: true,
            mailbox_healthy: true,
            registration_ready: true,
            registration_method: 'protocol_v2',
            workspaces: 1,
            blockers: [],
          },
        }),
      ),
      http.get('*/admin/email-pool/cloudflare', () =>
        HttpResponse.json({ profiles: [] }),
      ),
      http.get('*/admin/accounts', () =>
        HttpResponse.json({ accounts: [{ id: 'parent-ref', label: 'Parent', email: 'parent@example.com', upstream_account_id: 'remote-ref', status: 'active' }] }),
      ),
    );

    render(<MemoryRouter><TeamLifecycle /></MemoryRouter>);

    expect(await screen.findByText(longChildReference)).toHaveClass(
      'pool-text-clamp',
      'pool-text-clamp--strong',
    );
    expect(screen.getAllByText('0.75%').length).toBeGreaterThan(0);
    expect(screen.getAllByText(/阈值 1.00%/).length).toBeGreaterThan(0);
    expect(screen.getByText('邀请成员')).toBeInTheDocument();
    expect(screen.getByText('解析凭据')).toBeInTheDocument();
    expect(screen.getByText('补位任务已排队')).toBeInTheDocument();
    expect(screen.getByText('四项配置，按顺序点完即可')).toBeInTheDocument();
    expect(screen.getAllByLabelText('已就绪')).toHaveLength(4);
  });

  it('uses bounded table cells and a flat ordered lifecycle ledger', () => {
    const css = readFileSync(
      resolve(process.cwd(), 'src/styles/components.css'),
      'utf8',
    );
    expect(css).toMatch(
      /\.pool-lifecycle-table \.pool-table td\s*\{[^}]*overflow:\s*hidden;/s,
    );
    expect(css).toMatch(
      /\.pool-lifecycle-flow::before\s*\{[^}]*display:\s*none;/s,
    );
    expect(css).toMatch(
      /\.pool-lifecycle-hero\s*\{[^}]*border-block:\s*1px solid var\(--pool-border\);[^}]*background:\s*transparent;/s,
    );
    expect(css).toMatch(
      /@media \(max-width:\s*767px\)[\s\S]*\.pool-lifecycle-flow\s*\{[^}]*grid-template-columns:\s*repeat\(2,\s*minmax\(0,\s*1fr\)\)/,
    );
    expect(css).not.toMatch(/\.pool-lifecycle-(?:hero|flow|threshold)[^{]*\{[^}]*(?:radial|linear|conic)-gradient/s);
  });
});
