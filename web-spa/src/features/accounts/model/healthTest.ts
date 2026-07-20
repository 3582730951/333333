import type { AccountRow } from './types';

export const KIRO_SUSPENSION_REASON = 'aws_user_suspended';

export interface HealthProbeStage {
  checked?: boolean;
  alive?: boolean;
  state?: string;
  http_status?: number;
  error_code?: string;
}

export interface HealthTestResult {
  provider?: string;
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

export function healthTestRequestBody(account?: Pick<AccountRow, 'provider'> | null, costConfirmed = false) {
  return isKiroAccount(account) && costConfirmed ? { confirm_cost: true } : {};
}

export function selectedHasKiro(accounts: AccountRow[], selectedIDs: string[]): boolean {
  const selected = new Set(selectedIDs);
  return accounts.some((account) => selected.has(account.id) && isKiroAccount(account));
}

export type HealthResultTone = 'success' | 'warning' | 'error';

export function healthResultPresentation(result: HealthTestResult): { tone: HealthResultTone; message: string } {
  if (String(result?.provider || '').toLowerCase() !== 'kiro') {
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
    } else if (inference.error_code === 'kiro_account_suspended' || inference.state === 'banned') {
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

  for (const { account, result } of entries) {
    if (!isKiroAccount(account)) {
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
  if (otherAlive || otherFailed) parts.push(`其他提供商正常 ${otherAlive}、失败 ${otherFailed}`);
  if (requestFailures) parts.push(`请求失败 ${requestFailures}`);
  const hasFailure = requestFailures > 0 || otherFailed > 0 || inferenceSuspended > 0 || inferenceFailed > 0 || inferenceUnchecked > 0 || authAlive < kiroTotal;
  return {
    tone: hasFailure ? (entries.length ? 'warning' : 'error') : 'success',
    message: `批量测活完成：${parts.join('；') || '无结果'}`,
  };
}
