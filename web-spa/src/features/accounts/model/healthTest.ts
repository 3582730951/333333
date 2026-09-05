import type { AccountRow } from './types';

export const KIRO_SUSPENSION_REASON = 'aws_user_suspended';
export const PROVIDER_API_KEY_QUARANTINE_PREFIX = 'provider_api_key_inference_probe_failed:';
export const PROVIDER_API_KEY_QUARANTINE_PENDING = 'provider_api_key_inference_probe_pending';

export interface HealthProbeStage {
  checked?: boolean;
  alive?: boolean;
  state?: string;
  http_status?: number;
  error_code?: string;
}

export interface HealthTestResult {
  provider?: string;
  auth_method?: string;
  alive?: boolean;
  ready?: boolean;
  state?: string;
  reason?: string;
  http_status?: number;
  auth_probe?: HealthProbeStage;
  inference_probe?: HealthProbeStage;
}

export function isKiroAccount(account?: Pick<AccountRow, 'provider'> | null): boolean {
  return String(account?.provider || '').toLowerCase() === 'kiro';
}

export function isKiroSuspended(account?: Pick<AccountRow, 'provider' | 'quarantine_reason'> | null): boolean {
  return isKiroAccount(account) && account?.quarantine_reason === KIRO_SUSPENSION_REASON;
}

export function isProviderAPIKeyAccount(account?: Pick<AccountRow, 'provider' | 'auth_method'> | null): boolean {
  const provider = String(account?.provider || '').toLowerCase();
  return account?.auth_method === 'api_key' && (provider === 'codex' || provider === 'claude');
}

export function requiresPaidHealthTest(account?: Pick<AccountRow, 'provider' | 'auth_method'> | null): boolean {
  return isKiroAccount(account) || isProviderAPIKeyAccount(account);
}

export function isProtectedProbeQuarantine(account?: Pick<AccountRow, 'provider' | 'auth_method' | 'quarantine_reason'> | null): boolean {
  return isKiroSuspended(account)
    || (isProviderAPIKeyAccount(account) && (
      String(account?.quarantine_reason || '') === PROVIDER_API_KEY_QUARANTINE_PENDING
      || String(account?.quarantine_reason || '').startsWith(PROVIDER_API_KEY_QUARANTINE_PREFIX)
    ));
}

export function healthTestRequestBody(account?: Pick<AccountRow, 'provider' | 'auth_method'> | null, costConfirmed = false, model = '') {
  const body: Record<string, unknown> = {};
  if (requiresPaidHealthTest(account) && costConfirmed) body.confirm_cost = true;
  const selected = String(model || '').trim();
  if (selected) body.model = selected;
  return body;
}

export function selectedHasKiro(accounts: AccountRow[], selectedIDs: string[]): boolean {
  const selected = new Set(selectedIDs);
  return accounts.some((account) => selected.has(account.id) && isKiroAccount(account));
}

export function selectedHasPaidProbe(accounts: AccountRow[], selectedIDs: string[]): boolean {
  const selected = new Set(selectedIDs);
  return accounts.some((account) => selected.has(account.id) && requiresPaidHealthTest(account));
}

export type HealthResultTone = 'success' | 'warning' | 'error';

export function healthResultPresentation(result: HealthTestResult): { tone: HealthResultTone; message: string } {
  const provider = String(result?.provider || '').toLowerCase();
  const providerAPIKey = result?.auth_method === 'api_key' && (provider === 'codex' || provider === 'claude');
  if (provider !== 'kiro' && !providerAPIKey) {
    const healthy = Boolean(result?.ready ?? result?.alive);
    const status = result?.http_status || '—';
    return {
      tone: healthy ? 'success' : 'error',
      message: `${healthy ? '测活正常' : '测活失败'} · ${result?.state || 'unknown'} · HTTP ${status}`,
    };
  }

  const auth = result.auth_probe || {};
  const inference = result.inference_probe || {};
  const authText = auth.alive ? '认证正常' : `认证失败（${auth.state || 'unknown'}）`;
  let inferenceText = '推理未验证';
  let tone: HealthResultTone = auth.alive ? 'warning' : 'error';
  if (inference.checked) {
    if (inference.alive) {
      inferenceText = '推理可用';
      tone = 'success';
    } else if (provider === 'kiro' && (inference.error_code === 'kiro_account_suspended' || inference.state === 'banned')) {
      inferenceText = '推理暂停（AWS User ID 已暂停）';
      tone = 'error';
    } else {
      inferenceText = `推理失败（${inference.state || 'unknown'}）`;
      tone = 'error';
    }
  }
  return { tone, message: `${authText}；${inferenceText}` };
}

export interface HealthBatchEntry {
  account: AccountRow;
  result: HealthTestResult;
}

export function healthBatchPresentation(entries: HealthBatchEntry[], requestFailures = 0): { tone: HealthResultTone; message: string } {
  let kiroTotal = 0;
  let authAlive = 0;
  let inferenceAlive = 0;
  let inferenceSuspended = 0;
  let inferenceUnchecked = 0;
  let inferenceFailed = 0;
  let otherAlive = 0;
  let otherFailed = 0;
  let apiKeyTotal = 0;
  let apiKeyAuthAlive = 0;
  let apiKeyInferenceAlive = 0;
  let apiKeyInferenceUnchecked = 0;
  let apiKeyInferenceFailed = 0;

  for (const { account, result } of entries) {
    if (!isKiroAccount(account)) {
      if (isProviderAPIKeyAccount(account)) {
        apiKeyTotal += 1;
        if (result.auth_probe?.alive) apiKeyAuthAlive += 1;
        const inference = result.inference_probe || {};
        if (!inference.checked) apiKeyInferenceUnchecked += 1;
        else if (inference.alive) apiKeyInferenceAlive += 1;
        else apiKeyInferenceFailed += 1;
        continue;
      }
      if (result.ready ?? result.alive) otherAlive += 1;
      else otherFailed += 1;
      continue;
    }
    kiroTotal += 1;
    if (result.auth_probe?.alive) authAlive += 1;
    const inference = result.inference_probe || {};
    if (!inference.checked) inferenceUnchecked += 1;
    else if (inference.alive) inferenceAlive += 1;
    else if (inference.error_code === 'kiro_account_suspended' || inference.state === 'banned') inferenceSuspended += 1;
    else inferenceFailed += 1;
  }

  const parts: string[] = [];
  if (kiroTotal) {
    parts.push(`Kiro 认证正常 ${authAlive}/${kiroTotal}`);
    parts.push(`推理可用 ${inferenceAlive}`);
    if (inferenceSuspended) parts.push(`推理暂停 ${inferenceSuspended}`);
    if (inferenceUnchecked) parts.push(`推理未验证 ${inferenceUnchecked}`);
    if (inferenceFailed) parts.push(`推理失败 ${inferenceFailed}`);
  }
  if (apiKeyTotal) {
    parts.push(`API Key 认证正常 ${apiKeyAuthAlive}/${apiKeyTotal}`);
    parts.push(`API Key 推理可用 ${apiKeyInferenceAlive}`);
    if (apiKeyInferenceUnchecked) parts.push(`API Key 推理未验证 ${apiKeyInferenceUnchecked}`);
    if (apiKeyInferenceFailed) parts.push(`API Key 推理失败 ${apiKeyInferenceFailed}`);
  }
  if (otherAlive || otherFailed) parts.push(`其他提供商正常 ${otherAlive}、失败 ${otherFailed}`);
  if (requestFailures) parts.push(`请求失败 ${requestFailures}`);
  const hasFailure = requestFailures > 0 || otherFailed > 0 || inferenceSuspended > 0 || inferenceFailed > 0 || inferenceUnchecked > 0 || authAlive < kiroTotal
    || apiKeyInferenceFailed > 0 || apiKeyInferenceUnchecked > 0 || apiKeyAuthAlive < apiKeyTotal;
  return {
    tone: hasFailure ? (entries.length ? 'warning' : 'error') : 'success',
    message: `批量测活完成：${parts.join('；') || '无结果'}`,
  };
}
