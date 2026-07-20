import { describe, expect, it } from 'vitest';
import {
  healthBatchPresentation,
  healthResultPresentation,
  healthTestRequestBody,
  isKiroSuspended,
  isProtectedProbeQuarantine,
  isProviderAPIKeyAccount,
  selectedHasPaidProbe,
  selectedHasKiro,
} from '../src/features/accounts/model/healthTest';

describe('Kiro health-test UI contracts', () => {
  const kiro = { id: 'kiro-1', provider: 'kiro' };
  const codex = { id: 'codex-1', provider: 'codex' };
  const platform = { id: 'platform-1', provider: 'codex', auth_method: 'api_key' };

  it('requires cost confirmation only for selected Kiro accounts', () => {
    expect(selectedHasKiro([kiro, codex], ['codex-1'])).toBe(false);
    expect(selectedHasKiro([kiro, codex], ['codex-1', 'kiro-1'])).toBe(true);
    expect(healthTestRequestBody(codex, true)).toEqual({});
    expect(healthTestRequestBody(kiro, false)).toEqual({});
    expect(healthTestRequestBody(kiro, true)).toEqual({ confirm_cost: true });
  });

  it('requires the same explicit cost confirmation for provider API-key probes', () => {
    expect(isProviderAPIKeyAccount(platform)).toBe(true);
    expect(selectedHasPaidProbe([codex, platform], ['platform-1'])).toBe(true);
    expect(healthTestRequestBody(platform, false)).toEqual({});
    expect(healthTestRequestBody(platform, true)).toEqual({ confirm_cost: true });
    expect(isProtectedProbeQuarantine({ ...platform, quarantine_reason: 'provider_api_key_inference_probe_failed:billing_error' })).toBe(true);
    expect(isProtectedProbeQuarantine({ ...platform, quarantine_reason: 'provider_api_key_inference_probe_pending' })).toBe(true);
    expect(healthResultPresentation({
      provider: 'codex', auth_method: 'api_key', ready: false,
      auth_probe: { alive: true, state: 'alive' },
      inference_probe: { checked: true, alive: false, state: 'inference_failed', error_code: 'billing_error' },
    })).toEqual({ tone: 'error', message: '认证正常；推理失败（inference_failed）' });
  });

  it('distinguishes auth health from suspended and unchecked inference', () => {
    expect(healthResultPresentation({
      provider: 'kiro', ready: false,
      auth_probe: { alive: true, state: 'alive', http_status: 200 },
      inference_probe: { checked: true, alive: false, state: 'banned', http_status: 503, error_code: 'kiro_account_suspended' },
    })).toEqual({ tone: 'error', message: '认证正常；推理暂停（AWS User ID 已暂停）' });

    expect(healthResultPresentation({
      provider: 'kiro', ready: false,
      auth_probe: { alive: false, state: 'auth_expired', http_status: 401 },
      inference_probe: { checked: false, alive: false, state: 'not_checked' },
    })).toEqual({ tone: 'error', message: '认证失败（auth_expired）；推理未验证' });

    expect(healthResultPresentation({
      provider: 'kiro', ready: true,
      auth_probe: { alive: true, state: 'alive', http_status: 200 },
      inference_probe: { checked: true, alive: true, state: 'alive', http_status: 200 },
    })).toEqual({ tone: 'success', message: '认证正常；推理可用' });
  });

  it('summarizes mixed-provider batches without treating HTTP 200 probe failures as healthy', () => {
    const summary = healthBatchPresentation([
      {
        account: kiro,
        result: {
          provider: 'kiro', auth_probe: { alive: true },
          inference_probe: { checked: true, alive: false, state: 'banned', error_code: 'kiro_account_suspended' },
        },
      },
      { account: codex, result: { provider: 'codex', ready: true, alive: true } },
    ]);
    expect(summary.tone).toBe('warning');
    expect(summary.message).toContain('Kiro 认证正常 1/1');
    expect(summary.message).toContain('推理暂停 1');
    expect(summary.message).toContain('其他提供商正常 1、失败 0');
  });

  it('recognizes the permanent AWS suspension reason for list and drawer rendering', () => {
    expect(isKiroSuspended({ provider: 'kiro', quarantine_reason: 'aws_user_suspended' })).toBe(true);
    expect(isKiroSuspended({ provider: 'kiro', quarantine_reason: 'ban: account_suspended' })).toBe(false);
    expect(isKiroSuspended({ provider: 'codex', quarantine_reason: 'aws_user_suspended' })).toBe(false);
  });
});
