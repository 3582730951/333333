import { describe, expect, it } from 'vitest';
import { accountCredentialPresentation, quotaPresentation } from '../src/pages/Accounts.jsx';
import { compactIdentity } from '../src/pages/Dashboard';

describe('account and dashboard presentation helpers', () => {
  it('turns quota_summary into a clear remaining-capacity presentation', () => {
    const quota = quotaPresentation({
      quota_summary: {
        sync_reason: 'ok',
        primary: {
          used_percent: 90,
          remaining_tokens: 1_000,
          limit_tokens: 10_000,
          limit_window_seconds: 18_000,
        },
      },
    });

    expect(quota.percent).toBe(90);
    expect(quota.remainingPercent).toBe(10);
    expect(quota.tone).toBe('danger');
    expect(quota.windowLabel).toBe('5 小时窗口');
    expect(quota.detail).toContain('Token 剩余');
  });

  it('uses a human-readable reason when quota has not synchronized', () => {
    const quota = quotaPresentation({
      quota_summary: { sync_reason: 'token_expired', primary: null },
    });

    expect(quota.percent).toBeNull();
    expect(quota.reasonLabel).toBe('凭据已过期');
    expect(quota.tone).toBe('muted');
  });

  it('keeps both ends of long account identities so adjacent labels remain distinct', () => {
    const first = compactIdentity('ui-demo-account-production-0001');
    const second = compactIdentity('ui-demo-account-production-0002');

    expect(first).toContain('…');
    expect(first).toMatch(/0001$/);
    expect(second).toMatch(/0002$/);
    expect(first).not.toBe(second);
  });

  it('separates API-key identities from login accounts, including Kiro metadata', () => {
    expect(accountCredentialPresentation({ auth_method: 'api_key' })).toMatchObject({ key: 'api_key', label: 'API Key' });
    expect(accountCredentialPresentation({ kiro_auth: { auth_method: 'api_key' } })).toMatchObject({ key: 'api_key', label: 'API Key' });
    expect(accountCredentialPresentation({ auth_method: 'oauth' })).toMatchObject({ key: 'account', label: '登录账号' });
    expect(accountCredentialPresentation({ credential_mode: 'agent_identity' })).toMatchObject({ key: 'account', detail: 'Agent Identity' });
  });
});
