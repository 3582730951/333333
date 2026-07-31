import { z } from 'zod';
import { get, post } from '../../../api.js';
import { createApiError, parseApiResponse } from '../../../api/contracts';
import { normalizeRegisterMethod } from '../model/registration';
import type {
  RegistrationCountry, RegistrationDashboard, RegistrationJob, RegistrationOptions,
  RegistrationGroup, RegistrationPool, RegistrationProviderOptions, RegistrationReadiness,
  RegistrationStartInput, RegistrationStrategyConfig, RegistrationStrategyInput, SMSMarketSnapshot,
} from '../model/registration';

const jobSchema = z.object({ id: z.string().optional(), status: z.string().optional() }).passthrough();
export const registrationJobsResponseSchema = z.union([
  z.array(jobSchema),
  z.object({ jobs: z.array(jobSchema).optional() }).passthrough().transform((value) => value.jobs ?? []),
]);

export const registrationReadinessSchema = z.object({
  ready: z.boolean().optional(),
  providers: z.record(z.string(), z.coerce.number()).optional(),
  blockers: z.array(z.string()).optional(),
  provider_error: z.string().optional(),
  policy_error: z.string().optional(),
  pool: z.record(z.string(), z.unknown()).optional(),
}).passthrough();

const groupSchema = z.object({ name: z.string() }).passthrough();
const groupsResponseSchema = z.union([
  z.array(groupSchema),
  z.object({ groups: z.array(groupSchema).optional() }).passthrough().transform((value) => value.groups ?? []),
]);

const providerOptionSchema = z.union([
  z.string(),
  z.object({ label: z.string(), value: z.string() }).passthrough(),
]);
export const registrationProviderOptionsSchema = z.object({
  sms: z.array(providerOptionSchema).optional(),
  mailbox: z.array(providerOptionSchema).optional(),
  captcha: z.array(providerOptionSchema).optional(),
}).passthrough().transform((value) => ({
  sms: value.sms ?? [],
  mailbox: value.mailbox ?? [],
  captcha: value.captcha ?? [],
}));

const poolSchema = z.object({
  id: z.string(),
  name: z.string().optional(),
  purpose: z.string().optional(),
  members: z.array(z.unknown()).optional(),
}).passthrough();
const poolsResponseSchema = z.union([
  z.array(poolSchema),
  z.object({ pools: z.array(poolSchema).optional() }).passthrough().transform((value) => value.pools ?? []),
]);

export const registrationCountriesSchema = z.array(z.object({
  isoCode: z.string(),
  name: z.string(),
  nameZh: z.string().default(''),
}).passthrough());

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
export const registrationConfigResponseSchema = z.union([
  z.array(configFieldSchema),
  z.object({ config: z.array(configFieldSchema).optional(), fields: z.array(configFieldSchema).optional() })
    .passthrough()
    .transform((value) => value.config ?? value.fields ?? []),
]);

function partialError(code: string, message: string, failures: unknown[]) {
  return createApiError({ code, userMessage: message, retryable: true, cause: failures });
}

async function fetchRegistrationJobs(signal?: AbortSignal): Promise<RegistrationJob[]> {
  return parseApiResponse(registrationJobsResponseSchema, await get('/admin/register/batch', undefined, { signal })) as RegistrationJob[];
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
  return parseApiResponse(registrationProviderOptionsSchema, await get('/admin/register/providers/options', undefined, { signal })) as RegistrationProviderOptions;
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
  return post('/admin/settings-center', [{
    section: 'config',
    values: {
      sms_platform_strategy: strategy,
      sms_manual_country: strategy === 'manual' ? input.manualCountry : '',
      sms_min_price: Number.isFinite(input.minPrice) ? input.minPrice : 0,
      sms_max_price: Number.isFinite(input.maxPrice) ? input.maxPrice : 0,
    },
  }]);
}

export async function fetchSMSMarket(signal?: AbortSignal): Promise<SMSMarketSnapshot> {
  return parseApiResponse(smsMarketSchema, await get('/admin/register/sms-market', undefined, { signal })) as SMSMarketSnapshot;
}

export async function refreshSMSMarket(): Promise<SMSMarketSnapshot> {
  return parseApiResponse(smsMarketSchema, await post('/admin/register/sms-market', {})) as SMSMarketSnapshot;
}

export async function startRegistrationJob(input: RegistrationStartInput) {
  return post('/admin/register/batch', input);
}
