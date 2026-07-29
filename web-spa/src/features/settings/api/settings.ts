import { z } from 'zod';
import { del, get, post } from '../../../api.js';
import { createApiError, parseApiResponse } from '../../../api/contracts';
import type {
  AdvancedSettings, AdvancedSettingsKind, AdvancedSettingsSaveInput, AutomationSettings, ConfigField, ProviderOptions, ProviderSetting,
  ContextJournalClearResponse, LogClearResponse, RegistrarSaveInput, RegistrarSettings, SettingsEgress, SettingsGroup, SettingsPatch,
  SettingsSaveResponse, SettingsSection, SettingsTemplate, SettingsValues, SharedSettingsOptions,
} from '../model/settings';
import type { AISettingsDomain } from '../model/settings';

const valuesSchema = z.record(z.string(), z.unknown());
const settingsDiffSchema = z.object({
  section: z.string(),
  key: z.string(),
  old_value: z.unknown().optional(),
  new_value: z.unknown().optional(),
}).passthrough();
export const settingsSaveResponseSchema = z.object({
  saved: z.array(settingsDiffSchema).optional(),
}).passthrough().transform((value) => ({ ...value, saved: value.saved ?? [] }));

const logRecordCountsSchema = z.object({
  audit_log: z.coerce.number().int().nonnegative(),
  cf_events: z.coerce.number().int().nonnegative(),
  usage_records: z.coerce.number().int().nonnegative(),
  registration_task_events: z.coerce.number().int().nonnegative(),
  lifecycle_task_logs: z.coerce.number().int().nonnegative(),
  lifecycle_events: z.coerce.number().int().nonnegative(),
  proxy_usage_records: z.coerce.number().int().nonnegative(),
  terminal_billing_holds: z.coerce.number().int().nonnegative(),
});
export const logClearResponseSchema = z.object({
  ok: z.boolean(),
  deleted: logRecordCountsSchema,
  deleted_total: z.coerce.number().int().nonnegative(),
  preserved_active_billing_holds: z.coerce.number().int().nonnegative(),
  space_reclaimed: z.boolean(),
  reclaim_warning: z.string().optional().default(''),
  retention_days: z.coerce.number().int().positive(),
}).passthrough();

export const contextJournalClearResponseSchema = z.object({
  ok: z.boolean(),
  deleted_contexts: z.coerce.number().int().nonnegative(),
  space_reclaimed: z.boolean(),
  reclaim_warning: z.string().optional().default(''),
  ttl_seconds: z.coerce.number().int().positive(),
  completed_at: z.coerce.number().int().nonnegative(),
}).passthrough();

const configFieldSchema = z.object({
  key: z.string(),
  label: z.string().optional(),
  category: z.string().optional(),
  type: z.enum(['bool', 'select', 'int', 'string', 'csv']).optional(),
  effect: z.string().optional(),
  options: z.array(z.string()).optional(),
  help: z.string().optional(),
  placement: z.enum(['ai_settings', 'system_settings', 'feature_page']).optional(),
  domain: z.enum(['chatgpt', 'claude', 'kiro', 'antigravity', 'codex', 'claude_code']).nullable().optional(),
  scope: z.enum(['model', 'global']).optional(),
  section: z.string().optional(),
  order: z.coerce.number().int().nonnegative().optional(),
  value: z.unknown().optional(),
  overridden: z.boolean().optional(),
  settings_error: z.string().optional(),
}).passthrough().transform((field) => ({
  ...field,
  label: field.label ?? labelFromKey(field.key),
  category: field.category ?? '运行时配置',
  type: field.type ?? fieldTypeFromValue(field.value),
  effect: field.effect ?? 'hot',
  options: field.options ?? [],
  help: field.help ?? '',
  placement: field.placement ?? 'system_settings',
  domain: field.domain ?? null,
  scope: field.scope ?? 'global',
  section: field.section ?? 'general',
  order: field.order ?? 0,
  overridden: field.overridden ?? false,
  settings_error: field.settings_error ?? '',
}));

function labelFromKey(key: string) {
  return key.split('_').filter(Boolean).map((part) => part.charAt(0).toUpperCase() + part.slice(1)).join(' ');
}

function fieldTypeFromValue(value: unknown): ConfigField['type'] {
  if (typeof value === 'boolean') return 'bool';
  if (typeof value === 'number' && Number.isInteger(value)) return 'int';
  return 'string';
}

function normalizeConfigInput(value: unknown): unknown[] {
  if (Array.isArray(value)) return value;
  if (!value || typeof value !== 'object') return [];
  const record = value as Record<string, unknown>;
  for (const key of ['fields', 'config', 'rows']) {
    if (Array.isArray(record[key])) return record[key] as unknown[];
  }
  const values = record.values && typeof record.values === 'object' ? record.values as Record<string, unknown> : record;
  return Object.entries(values)
    .filter(([, item]) => item === null || ['string', 'number', 'boolean'].includes(typeof item))
    .map(([key, item]) => ({ key, value: item }));
}

export const configFieldsResponseSchema = z.unknown()
  .transform(normalizeConfigInput)
  .pipe(z.array(configFieldSchema));

const policySchema = z.object({
  type: z.string(),
  enabled: z.boolean().optional(),
  config: valuesSchema.optional(),
}).passthrough();
const readinessSchema = z.object({ ready: z.boolean().optional(), blockers: z.array(z.string()).optional() }).passthrough();
export const automationSectionSchema = z.object({
  automation: z.object({
    policies: z.array(policySchema).optional(),
    stats: valuesSchema.nullish(),
    readiness: readinessSchema.nullish(),
    policy_error: z.string().optional(),
    stats_error: z.string().optional(),
  }).passthrough().optional(),
}).passthrough().transform((view): AutomationSettings => {
  const automation = view.automation ?? {};
  return {
    policies: Object.fromEntries((automation.policies ?? []).map((policy) => [policy.type, policy])),
    stats: automation.stats ?? null,
    readiness: automation.readiness ?? null,
    automationErrors: { policy: automation.policy_error ?? '', stats: automation.stats_error ?? '' },
  };
});

const providerSchema = z.object({
  id: z.string().optional(),
  type: z.string(),
  key: z.string(),
  display_name: z.string().optional(),
  enabled: z.boolean().optional(),
  priority: z.coerce.number().optional(),
  config: valuesSchema.optional(),
}).passthrough();
export const providersResponseSchema = z.union([
  z.array(providerSchema),
  z.object({ providers: z.array(providerSchema).optional() }).passthrough().transform((value) => value.providers ?? []),
]);

export const registrarSectionSchema = z.object({
  registrar: valuesSchema.optional(),
}).passthrough().transform((view) => view.registrar ?? {});

function runtimeSectionSchema(section: 'logging' | 'memory') {
  return z.object({ [section]: valuesSchema.optional() }).passthrough()
    .transform((view) => (view[section] ?? {}) as SettingsValues);
}
export const loggingSectionSchema = runtimeSectionSchema('logging');
export const memorySectionSchema = runtimeSectionSchema('memory');

const groupSchema = z.object({ name: z.string() }).passthrough();
export const settingsGroupsSchema = z.union([
  z.array(groupSchema),
  z.object({ groups: z.array(groupSchema).optional() }).passthrough().transform((value) => value.groups ?? []),
]);
const egressSchema = z.object({ id: z.string(), name: z.string().optional(), type: z.string().optional() }).passthrough();
export const settingsEgressesSchema = z.union([
  z.array(egressSchema),
  z.object({ profiles: z.array(egressSchema).optional(), egresses: z.array(egressSchema).optional() })
    .passthrough().transform((value) => value.profiles ?? value.egresses ?? []),
]);
const optionSchema = z.union([z.string(), z.object({ label: z.string(), value: z.string() }).passthrough()]);
export const settingsProviderOptionsSchema = z.object({
  sms: z.array(optionSchema).optional(),
  mailbox: z.array(optionSchema).optional(),
  captcha: z.array(optionSchema).optional(),
}).passthrough().transform((value): ProviderOptions => ({
  ...value,
  sms: value.sms ?? [], mailbox: value.mailbox ?? [], captcha: value.captcha ?? [],
}));

export const settingsTemplateSchema = z.object({
  id: z.string(),
  name: z.string(),
  description: z.string().optional(),
  platform: z.string().optional(),
  method: z.string().optional(),
  identity_mode: z.string().optional(),
  egress: z.string().optional(),
  needs: z.array(z.string()).optional(),
  saved: z.array(settingsDiffSchema).optional(),
}).passthrough().transform((value) => ({ ...value, saved: value.saved ?? [] }));

const thinkingOverrideSchema = z.object({
  mode: z.string(),
  level: z.string().optional(),
  budget: z.coerce.number().int().optional(),
}).passthrough();
export const thinkingSettingsSchema = z.object({
  enabled: z.boolean(),
  default_mode: z.string(),
  default_level: z.string(),
  default_budget: z.coerce.number().int().nonnegative(),
  providers: z.record(z.string(), thinkingOverrideSchema),
  models: z.record(z.string(), thinkingOverrideSchema),
}).passthrough();

export const moderationSettingsSchema = z.object({
  enabled: z.boolean(),
  model: z.string(),
  auto_translate: z.boolean(),
  words: z.array(z.string()),
}).passthrough();

const thinkingSaveResponseSchema = z.object({ status: z.string() }).passthrough();

const advancedSettingsDefinition = {
  thinking: { endpoint: '/admin/thinking', schema: thinkingSettingsSchema },
  moderation: { endpoint: '/admin/moderation', schema: moderationSettingsSchema },
} as const;

function partialError(code: string, message: string, failures: unknown[]) {
  return createApiError({ code, userMessage: message, retryable: true, cause: failures });
}

function settingsCenterURL(...sections: SettingsSection[]) {
  return `/admin/settings-center?sections=${sections.join(',')}`;
}

export async function fetchConfigSettings(signal?: AbortSignal): Promise<ConfigField[]> {
  return parseApiResponse(configFieldsResponseSchema, await get('/admin/config', { placement: 'system_settings' }, { signal })) as ConfigField[];
}

export async function fetchAIConfigSettings(domain: AISettingsDomain, signal?: AbortSignal): Promise<ConfigField[]> {
  return parseApiResponse(configFieldsResponseSchema, await get('/admin/config', {
    placement: 'ai_settings',
    domain,
  }, { signal })) as ConfigField[];
}

export async function fetchAutomationSettings(signal?: AbortSignal): Promise<AutomationSettings> {
  return parseApiResponse(automationSectionSchema, await get(settingsCenterURL('automation'), undefined, { signal }));
}

export async function fetchRegistrarSettings(signal?: AbortSignal): Promise<RegistrarSettings> {
  const [sectionResult, providersResult] = await Promise.allSettled([
    get(settingsCenterURL('registrar'), undefined, { signal }),
    get('/admin/register/providers', undefined, { signal }),
  ]);
  if (sectionResult.status === 'rejected') throw sectionResult.reason;
  const section = parseApiResponse(registrarSectionSchema, sectionResult.value) as SettingsValues;
  const providers = providersResult.status === 'fulfilled'
    ? parseApiResponse(providersResponseSchema, providersResult.value) as ProviderSetting[]
    : [];
  const metaKeys = new Set(['defaults', 'registrar_error', 'defaults_error']);
  const cfg = Object.fromEntries(Object.entries(section).filter(([key]) => !metaKeys.has(key)));
  return {
    cfg,
    smsProviders: providers.filter((provider) => provider.type === 'sms' && ['smsbower', 'herosms'].includes(provider.key)),
    mailboxProviders: providers.filter((provider) => provider.type === 'mailbox'
      && ['tempmail', 'cloudflare', 'imap'].includes(provider.key)),
    captchaProviders: providers.filter((provider) => provider.type === 'captcha'
      && ['yescaptcha', '2captcha'].includes(provider.key)),
    emailProviders: providers.filter((provider) => provider.type === 'email'
      && provider.key === 'hotmail_otp'),
    registrarErrors: {
      config: typeof section.registrar_error === 'string' ? section.registrar_error : '',
      defaults: typeof section.defaults_error === 'string' ? section.defaults_error : '',
      providers: providersResult.status === 'rejected' ? '接码平台配置读取失败' : '',
    },
  };
}

export async function fetchLoggingSettings(signal?: AbortSignal): Promise<SettingsValues> {
  return parseApiResponse(loggingSectionSchema, await get(settingsCenterURL('logging'), undefined, { signal }));
}

export async function fetchMemorySettings(signal?: AbortSignal): Promise<SettingsValues> {
  return parseApiResponse(memorySectionSchema, await get(settingsCenterURL('memory'), undefined, { signal }));
}

export async function fetchSharedSettingsOptions(signal?: AbortSignal): Promise<SharedSettingsOptions> {
  const results = await Promise.allSettled([
    get('/admin/groups', undefined, { signal }),
    get('/admin/egress-profiles', undefined, { signal }),
    get('/admin/register/providers/options', undefined, { signal }),
  ]);
  const failures = results.filter((result) => result.status === 'rejected').map((result) => result.reason);
  return {
    groups: results[0].status === 'fulfilled' ? parseApiResponse(settingsGroupsSchema, results[0].value) as SettingsGroup[] : [],
    egresses: results[1].status === 'fulfilled' ? parseApiResponse(settingsEgressesSchema, results[1].value) as SettingsEgress[] : [],
    providerOpts: results[2].status === 'fulfilled'
      ? parseApiResponse(settingsProviderOptionsSchema, results[2].value)
      : { sms: [], mailbox: [], captcha: [] },
    error: failures.length ? partialError('SETTINGS_OPTIONS_FAILED', '部分设置选项暂时不可用。', failures) : null,
  };
}

export async function saveSettingsPatches(patches: SettingsPatch[]): Promise<SettingsSaveResponse> {
  return parseApiResponse(settingsSaveResponseSchema, await post('/admin/settings-center', patches));
}

export async function clearLogRecords(): Promise<LogClearResponse> {
  return parseApiResponse(logClearResponseSchema, await del('/admin/logs', undefined, { timeout: 30 * 60 * 1000 })) as LogClearResponse;
}

export async function clearContextJournal(): Promise<ContextJournalClearResponse> {
  return parseApiResponse(contextJournalClearResponseSchema, await del('/admin/context-journal', undefined, { timeout: 30 * 60 * 1000 })) as ContextJournalClearResponse;
}

export async function applySettingsTemplate(templateId: string): Promise<SettingsTemplate> {
  return parseApiResponse(settingsTemplateSchema, await post('/admin/settings-center/apply-template', { template_id: templateId })) as SettingsTemplate;
}

export async function saveRegistrarSettings(input: RegistrarSaveInput): Promise<SettingsSaveResponse> {
  if (input.providers.length) await post('/admin/register/providers', { providers: input.providers });
  return saveSettingsPatches([{ section: 'registrar', mode: 'replace', values: input.values }]);
}

export async function fetchAdvancedSettings(kind: AdvancedSettingsKind, signal?: AbortSignal): Promise<AdvancedSettings> {
  if (kind === 'thinking') {
    return parseApiResponse(thinkingSettingsSchema, await get(advancedSettingsDefinition.thinking.endpoint, undefined, { signal }));
  }
  return parseApiResponse(moderationSettingsSchema, await get(advancedSettingsDefinition.moderation.endpoint, undefined, { signal }));
}

export async function saveAdvancedSettings({ kind, values }: AdvancedSettingsSaveInput): Promise<AdvancedSettings | { status: string }> {
  if (kind === 'thinking') {
    const validated = parseApiResponse(thinkingSettingsSchema, values);
    return parseApiResponse(thinkingSaveResponseSchema, await post(advancedSettingsDefinition.thinking.endpoint, validated));
  }
  const validated = parseApiResponse(moderationSettingsSchema, values);
  return parseApiResponse(moderationSettingsSchema, await post(advancedSettingsDefinition.moderation.endpoint, validated));
}
