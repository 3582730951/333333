import { z } from 'zod';
import { get, patch, post, statusCode } from '../../../api.js';
import { createApiError, parseApiResponse } from '../../../api/contracts';
import { normalizeRegisterMethod } from '../model/registration';
import type {
  RegistrationCountry, RegistrationDashboard, RegistrationJob, RegistrationOptions,
  RegistrationGroup, RegistrationPool, RegistrationProviderOptions, RegistrationReadiness,
  RegistrationStartInput, RegistrationStrategyConfig, RegistrationStrategyInput, SMSMarketSnapshot,
} from '../model/registration';

type UnknownRecord = Record<string, unknown>;

function isRecord(value: unknown): value is UnknownRecord {
  return value !== null && typeof value === 'object' && !Array.isArray(value);
}

function firstDefined(record: UnknownRecord, keys: string[]): unknown {
  for (const key of keys) {
    if (record[key] !== undefined) return record[key];
  }
  return undefined;
}

function compatibilityBoolean(value: unknown): unknown {
  if (typeof value === 'boolean') return value;
  if (typeof value === 'number') return value !== 0;
  if (typeof value === 'string') {
    const normalized = value.trim().toLowerCase();
    if (['true', '1', 'yes', 'on', 'enabled', 'ready'].includes(normalized)) return true;
    if (['false', '0', 'no', 'off', 'disabled', ''].includes(normalized)) return false;
  }
  return value;
}

function normalizeRegistrationJob(value: unknown): unknown {
  if (!isRecord(value)) return value;
  const normalized: UnknownRecord = { ...value };
  const aliasKeys = [
    'job_id', 'jobId', 'state', 'success_count', 'successCount', 'failed_count',
    'fail_count', 'failureCount', 'createdAt', 'groupName', 'identityMode',
  ];
  for (const key of aliasKeys) delete normalized[key];
  const canonical: Array<[string, unknown]> = [
    ['id', firstDefined(value, ['id', 'job_id', 'jobId'])],
    ['status', firstDefined(value, ['status', 'state'])],
    ['succeeded', firstDefined(value, ['succeeded', 'success_count', 'successCount'])],
    ['failed', firstDefined(value, ['failed', 'failed_count', 'fail_count', 'failureCount'])],
    ['total', firstDefined(value, ['total', 'count', 'amount'])],
    ['created_at', firstDefined(value, ['created_at', 'createdAt'])],
    ['group_name', firstDefined(value, ['group_name', 'groupName', 'group'])],
    ['identity_mode', firstDefined(value, ['identity_mode', 'identityMode', 'identity'])],
  ];
  for (const [key, next] of canonical) {
    if (next === undefined || next === null || next === '') delete normalized[key];
    else normalized[key] = next;
  }
  return normalized;
}

function normalizeRegistrationJobsResponse(value: unknown): unknown {
  if (Array.isArray(value)) return value.map(normalizeRegistrationJob);
  if (!isRecord(value)) return value;
  for (const key of ['jobs', 'tasks', 'items', 'rows']) {
    if (key in value) {
      const rows = value[key];
      return Array.isArray(rows) ? rows.map(normalizeRegistrationJob) : rows;
    }
  }
  const nested = firstDefined(value, ['data', 'result', 'payload']);
  if (nested !== undefined) return normalizeRegistrationJobsResponse(nested);
  // The canonical endpoint has historically represented an empty queue as
  // either `{}` or a metadata-only success object. Preserve that contract,
  // while leaving error/unknown payloads invalid so they surface to operators.
  const metadataKeys = new Set(['request_id', 'requestId', 'success', 'ok', 'message']);
  const contentKeys = Object.keys(value).filter((key) => !metadataKeys.has(key));
  const explicitError = 'error' in value || 'errors' in value;
  if (!explicitError && contentKeys.length === 0) return [];
  return value;
}

const jobSchema = z.preprocess(normalizeRegistrationJob, z.object({
  id: z.string().optional(),
  status: z.string().optional(),
  total: z.coerce.number().int().optional(),
  succeeded: z.coerce.number().int().optional(),
  failed: z.coerce.number().int().optional(),
  created_at: z.coerce.number().int().optional(),
}).passthrough());
export const registrationJobsResponseSchema = z.preprocess(
  normalizeRegistrationJobsResponse,
  z.array(jobSchema),
);

function normalizeProviderCounts(value: unknown): unknown {
  if (!isRecord(value)) return value;
  const counts: UnknownRecord = {};
  for (const [rawKey, count] of Object.entries(value)) {
    const key = rawKey.trim().toLowerCase().replaceAll('-', '_');
    const canonical = ['mail', 'email', 'mailboxes'].includes(key)
      ? 'mailbox'
      : ['emailotp', 'email_otp_provider', 'hotmail_otp'].includes(key) ? 'email_otp' : key;
    counts[canonical] = count;
  }
  return counts;
}

function normalizeRegistrationReadiness(value: unknown): unknown {
  if (!isRecord(value)) return value;
  const nested = firstDefined(value, ['data', 'result', 'payload']);
  if (isRecord(nested)) return normalizeRegistrationReadiness({ ...value, ...nested, data: undefined, result: undefined, payload: undefined });
  const normalized: UnknownRecord = { ...value };
  for (const key of ['is_ready', 'isReady', 'provider_counts', 'providerCounts', 'reasons', 'errors', 'registrationEnabled']) {
    delete normalized[key];
  }
  const ready = firstDefined(value, ['ready', 'is_ready', 'isReady']);
  const enabled = firstDefined(value, ['registration_enabled', 'registrationEnabled']);
  const providers = firstDefined(value, ['providers', 'provider_counts', 'providerCounts']);
  const blockers = firstDefined(value, ['blockers', 'reasons', 'errors']);
  if (ready !== undefined) normalized.ready = compatibilityBoolean(ready);
  if (enabled !== undefined) normalized.registration_enabled = compatibilityBoolean(enabled);
  if (providers !== undefined) normalized.providers = normalizeProviderCounts(providers);
  if (blockers !== undefined) normalized.blockers = blockers;
  return normalized;
}

export const registrationReadinessSchema = z.preprocess(normalizeRegistrationReadiness, z.object({
  ready: z.preprocess(compatibilityBoolean, z.boolean()).optional(),
  registration_enabled: z.preprocess(compatibilityBoolean, z.boolean()).optional(),
  providers: z.record(z.string(), z.coerce.number()).optional(),
  blockers: z.array(z.string()).optional(),
  provider_error: z.string().optional(),
  policy_error: z.string().optional(),
  pool: z.record(z.string(), z.unknown()).optional(),
}).passthrough());

function normalizeGroup(value: unknown): unknown {
  if (typeof value === 'string') return { name: value };
  if (!isRecord(value)) return value;
  return { ...value, name: firstDefined(value, ['name', 'group_name', 'groupName', 'id']) };
}

function normalizeGroupsResponse(value: unknown): unknown {
  if (Array.isArray(value)) return value.map(normalizeGroup);
  if (!isRecord(value)) return value;
  const rows = firstDefined(value, ['groups', 'items', 'rows', 'list']);
  if (Array.isArray(rows)) return rows.map(normalizeGroup);
  const nested = firstDefined(value, ['data', 'result']);
  return nested === undefined ? value : normalizeGroupsResponse(nested);
}

const groupSchema = z.object({ name: z.string() }).passthrough();
const groupsResponseSchema = z.preprocess(normalizeGroupsResponse, z.array(groupSchema));

const providerOptionSchema = z.union([
  z.string(),
  z.object({ label: z.string(), value: z.string() }).passthrough(),
]);
function normalizeProviderOption(value: unknown): unknown {
  if (typeof value === 'string') return value;
  if (!isRecord(value)) return value;
  return {
    ...value,
    label: firstDefined(value, ['label', 'display_name', 'displayName', 'name', 'key', 'value']),
    value: firstDefined(value, ['value', 'key', 'provider_key', 'providerKey', 'name']),
  };
}

function providerRowEnabled(value: UnknownRecord): boolean {
  const raw = firstDefined(value, ['enabled', 'active', 'is_enabled', 'isEnabled']);
  return raw === undefined ? true : compatibilityBoolean(raw) === true;
}

function normalizeProviderOptionsResponse(value: unknown): unknown {
  if (!isRecord(value)) return value;
  const nested = firstDefined(value, ['data', 'result', 'options']);
  if (isRecord(nested)) return normalizeProviderOptionsResponse(nested);
  if (Array.isArray(value.providers)) {
    const output: Record<'sms' | 'mailbox' | 'captcha', unknown[]> = { sms: [], mailbox: [], captcha: [] };
    for (const provider of value.providers) {
      if (!isRecord(provider) || !providerRowEnabled(provider)) continue;
      const rawType = String(firstDefined(provider, ['type', 'provider_type', 'providerType', 'kind']) || '').trim().toLowerCase();
      const type = ['mail', 'email', 'mailbox'].includes(rawType) ? 'mailbox' : rawType;
      if (!(type in output)) continue;
      const option = normalizeProviderOption(provider);
      if (isRecord(option) && typeof option.label === 'string' && typeof option.value === 'string') {
        output[type as keyof typeof output].push({ label: option.label, value: option.value });
      }
    }
    return output;
  }
  return {
    ...value,
    sms: Array.isArray(value.sms) ? value.sms.map(normalizeProviderOption) : [],
    mailbox: Array.isArray(value.mailbox) ? value.mailbox.map(normalizeProviderOption) : [],
    captcha: Array.isArray(value.captcha) ? value.captcha.map(normalizeProviderOption) : [],
  };
}

export const registrationProviderOptionsSchema = z.preprocess(
  normalizeProviderOptionsResponse,
  z.object({
    sms: z.array(providerOptionSchema).optional(),
    mailbox: z.array(providerOptionSchema).optional(),
    captcha: z.array(providerOptionSchema).optional(),
  }).passthrough().transform((value) => ({
    sms: value.sms ?? [],
    mailbox: value.mailbox ?? [],
    captcha: value.captcha ?? [],
  })),
);

const poolSchema = z.object({
  id: z.string(),
  name: z.string().optional(),
  purpose: z.string().optional(),
  members: z.array(z.unknown()).optional(),
}).passthrough();
function normalizePool(value: unknown): unknown {
  if (!isRecord(value)) return value;
  return {
    ...value,
    id: firstDefined(value, ['id', 'pool_id', 'poolId', 'value']),
    name: firstDefined(value, ['name', 'display_name', 'displayName', 'label']),
  };
}

function normalizePoolsResponse(value: unknown): unknown {
  if (Array.isArray(value)) return value.map(normalizePool);
  if (!isRecord(value)) return value;
  const rows = firstDefined(value, ['pools', 'items', 'rows', 'list']);
  if (Array.isArray(rows)) return rows.map(normalizePool);
  const nested = firstDefined(value, ['data', 'result']);
  return nested === undefined ? value : normalizePoolsResponse(nested);
}

const poolsResponseSchema = z.preprocess(normalizePoolsResponse, z.array(poolSchema));

function normalizeCountry(value: unknown): unknown {
  if (!isRecord(value)) return value;
  const normalized: UnknownRecord = { ...value };
  for (const key of ['iso_code', 'country_code', 'countryCode', 'code', 'iso2', 'country_name', 'countryName', 'name_en', 'nameEn', 'name_zh', 'chinese_name', 'chineseName']) {
    delete normalized[key];
  }
  normalized.isoCode = firstDefined(value, ['isoCode', 'iso_code', 'country_code', 'countryCode', 'code', 'iso2']);
  normalized.name = firstDefined(value, ['name', 'country_name', 'countryName', 'name_en', 'nameEn']);
  normalized.nameZh = firstDefined(value, ['nameZh', 'name_zh', 'chinese_name', 'chineseName']) ?? '';
  return normalized;
}

function normalizeCountriesResponse(value: unknown): unknown {
  if (Array.isArray(value)) return value.map(normalizeCountry);
  if (!isRecord(value)) return value;
  const rows = firstDefined(value, ['countries', 'items', 'rows', 'list']);
  if (Array.isArray(rows)) return rows.map(normalizeCountry);
  const nested = firstDefined(value, ['data', 'result']);
  return nested === undefined ? value : normalizeCountriesResponse(nested);
}

export const registrationCountriesSchema = z.preprocess(normalizeCountriesResponse, z.array(z.object({
  isoCode: z.string(),
  name: z.string(),
  nameZh: z.string().default(''),
}).passthrough()));

const smsMarketCandidateSchema = z.object({
  provider: z.string(),
  service: z.string().default('dr'),
  country_id: z.coerce.string(),
  country_iso: z.string().default(''),
  country_name: z.string().optional(),
  price: z.coerce.number(),
  inventory: z.coerce.number().int(),
  provider_rank: z.coerce.number().int(),
  balance: z.coerce.number(),
  fetched_at: z.coerce.number().int(),
  attempts: z.coerce.number().int().default(0),
  succeeded: z.coerce.number().int().default(0),
  success_rate: z.coerce.number().default(0.5),
  score: z.coerce.number().default(0),
  eligible: z.boolean().default(false),
  selection_basis: z.string().default('community_cold_start'),
}).passthrough();

export const smsMarketSchema = z.object({
  items: z.array(smsMarketCandidateSchema).default([]),
  min_price: z.coerce.number().default(0),
  max_price: z.coerce.number().default(0),
  preferred_countries: z.array(z.string()).default(['BR', 'CO', 'PL']),
  cold_start_policy: z.string().default('community_recommended_order'),
  history_window_days: z.coerce.number().int().default(14),
  minimum_history_samples: z.coerce.number().int().default(3),
  refresh_interval_seconds: z.coerce.number().int().default(3600),
  last_refreshed_at: z.coerce.number().int().default(0),
  stale: z.boolean().default(true),
  refreshed_rows: z.coerce.number().int().default(0),
  warning: z.string().default(''),
}).passthrough();

const configFieldSchema = z.object({ key: z.string(), value: z.unknown() }).passthrough();
function normalizeConfigResponse(value: unknown): unknown {
  if (Array.isArray(value)) return value;
  if (!isRecord(value)) return value;
  for (const key of ['fields', 'config']) {
    if (Array.isArray(value[key])) return value[key];
    if (isRecord(value[key])) return normalizeConfigResponse(value[key]);
  }
  const nested = firstDefined(value, ['data', 'result']);
  if (isRecord(nested) || Array.isArray(nested)) return normalizeConfigResponse(nested);
  const ignored = new Set(['request_id', 'requestId', 'success', 'message']);
  return Object.entries(value)
    .filter(([key]) => !ignored.has(key))
    .map(([key, fieldValue]) => ({ key, value: fieldValue }));
}

export const registrationConfigResponseSchema = z.preprocess(
  normalizeConfigResponse,
  z.array(configFieldSchema),
);

function partialError(code: string, message: string, failures: unknown[]) {
  return createApiError({ code, userMessage: message, retryable: true, cause: failures });
}

async function fetchRegistrationJobs(signal?: AbortSignal): Promise<RegistrationJob[]> {
  try {
    return parseApiResponse(registrationJobsResponseSchema, await get('/admin/register/batch', undefined, { signal })) as RegistrationJob[];
  } catch (error) {
    if (![404, 405, 410, 501].includes(statusCode(error))) throw error;
    return parseApiResponse(registrationJobsResponseSchema, await get('/admin/register/email/jobs', undefined, { signal })) as RegistrationJob[];
  }
}

async function fetchRegistrationReadiness(signal?: AbortSignal): Promise<RegistrationReadiness> {
  return parseApiResponse(registrationReadinessSchema, await get('/admin/register/readiness', undefined, { signal })) as RegistrationReadiness;
}

export async function fetchRegistrationDashboard(signal?: AbortSignal): Promise<RegistrationDashboard> {
  const [jobs, readiness] = await Promise.allSettled([
    fetchRegistrationJobs(signal),
    fetchRegistrationReadiness(signal),
  ]);
  if (jobs.status === 'rejected') throw jobs.reason;
  if (readiness.status === 'fulfilled') return { jobs: jobs.value, readiness: readiness.value, readinessError: null };
  return {
    jobs: jobs.value,
    readiness: null,
    readinessError: partialError('REGISTRATION_READINESS_FAILED', '注册任务已加载，但依赖状态暂时不可用。', [readiness.reason]),
  };
}

async function fetchRegistrationGroups(signal?: AbortSignal): Promise<RegistrationGroup[]> {
  return parseApiResponse(groupsResponseSchema, await get('/admin/groups', undefined, { signal })) as RegistrationGroup[];
}

async function fetchRegistrationPools(signal?: AbortSignal): Promise<RegistrationPool[]> {
  return parseApiResponse(poolsResponseSchema, await get('/admin/egress-pools', undefined, { signal })) as RegistrationPool[];
}

async function fetchRegistrationProviders(signal?: AbortSignal): Promise<RegistrationProviderOptions> {
  try {
    return parseApiResponse(registrationProviderOptionsSchema, await get('/admin/register/providers/options', undefined, { signal })) as RegistrationProviderOptions;
  } catch (error) {
    if (![404, 405, 410, 501].includes(statusCode(error))) throw error;
    return parseApiResponse(registrationProviderOptionsSchema, await get('/admin/register/providers', undefined, { signal })) as RegistrationProviderOptions;
  }
}

export async function fetchRegistrationOptions(signal?: AbortSignal): Promise<RegistrationOptions> {
  const results = await Promise.allSettled([
    fetchRegistrationGroups(signal),
    fetchRegistrationPools(signal),
    fetchRegistrationProviders(signal),
  ]);
  const failures = results.filter((result) => result.status === 'rejected').map((result) => result.reason);
  return {
    groups: results[0].status === 'fulfilled' ? results[0].value : [],
    pools: results[1].status === 'fulfilled' ? results[1].value : [],
    providers: results[2].status === 'fulfilled' ? results[2].value : { sms: [], mailbox: [], captcha: [] },
    error: failures.length ? partialError('REGISTRATION_OPTIONS_FAILED', '部分注册表单选项暂时不可用。', failures) : null,
  };
}

export async function fetchRegistrationCountries(signal?: AbortSignal): Promise<RegistrationCountry[]> {
  return parseApiResponse(registrationCountriesSchema, await get('/admin/register/countries', undefined, { signal })) as RegistrationCountry[];
}

export function adaptRegistrationStrategy(fields: Array<{ key: string; value: unknown }>): RegistrationStrategyConfig {
  const values = new Map(fields.map((field) => [field.key, field.value]));
  const strategy = values.get('sms_platform_strategy') === 'manual' ? 'manual' : 'auto';
  return {
    strategy,
    manualCountry: strategy === 'manual' ? String(values.get('sms_manual_country') || '') : '',
    defaultMethod: normalizeRegisterMethod(values.get('default_register_method'), 'protocol_v2'),
    minPrice: Number(values.get('sms_min_price') || 0),
    maxPrice: Number(values.get('sms_max_price') || 0),
  };
}

export async function fetchRegistrationStrategy(signal?: AbortSignal): Promise<RegistrationStrategyConfig> {
  const fields = parseApiResponse(registrationConfigResponseSchema, await get('/admin/config', undefined, { signal }));
  return adaptRegistrationStrategy(fields);
}

export async function saveRegistrationStrategy(input: RegistrationStrategyInput) {
  const strategy = input.strategy === 'manual' ? 'manual' : 'auto';
  const values = {
    sms_platform_strategy: strategy,
    sms_manual_country: strategy === 'manual' ? input.manualCountry : '',
    sms_min_price: Number.isFinite(input.minPrice) ? input.minPrice : 0,
    sms_max_price: Number.isFinite(input.maxPrice) ? input.maxPrice : 0,
  };
  try {
    return await post('/admin/settings-center', [{ section: 'config', values }]);
  } catch (error) {
    if (![404, 405, 410, 501].includes(statusCode(error))) throw error;
    return patch('/admin/config', values);
  }
}

export async function fetchSMSMarket(signal?: AbortSignal): Promise<SMSMarketSnapshot> {
  return parseApiResponse(smsMarketSchema, await get('/admin/register/sms-market', undefined, { signal })) as SMSMarketSnapshot;
}

export async function refreshSMSMarket(): Promise<SMSMarketSnapshot> {
  return parseApiResponse(smsMarketSchema, await post('/admin/register/sms-market', {})) as SMSMarketSnapshot;
}

export async function startRegistrationJob(input: RegistrationStartInput) {
  try {
    return await post('/admin/register/batch', input);
  } catch (error) {
    if (![404, 405, 410, 501].includes(statusCode(error))) throw error;
    return post('/admin/register/email/start', {
      count: input.count,
      group_name: input.group_name,
      egress_pool_id: input.registration_egress_pool_id,
    });
  }
}
