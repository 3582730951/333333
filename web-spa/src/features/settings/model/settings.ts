import type { ApiError } from '../../../model/contracts';

export type SettingsSection = 'config' | 'automation' | 'registrar' | 'logging' | 'memory';
export type SettingsValues = Record<string, unknown>;
export type ConfigPlacement = 'ai_settings' | 'system_settings' | 'feature_page';
export type AISettingsDomain = 'chatgpt' | 'claude' | 'kiro' | 'antigravity' | 'codex' | 'claude_code';
export type ConfigScope = 'model' | 'global';

export interface ConfigField {
  key: string;
  label: string;
  category: string;
  type: 'bool' | 'select' | 'int' | 'string' | 'csv';
  effect: string;
  options: string[];
  help: string;
  placement: ConfigPlacement;
  domain: AISettingsDomain | null;
  scope: ConfigScope;
  section: string;
  order: number;
  value?: unknown;
  overridden: boolean;
  settings_error: string;
  [key: string]: unknown;
}

export interface SettingsDiff {
  section: string;
  key: string;
  old_value?: unknown;
  new_value?: unknown;
}

export interface SettingsSaveResponse {
  saved: SettingsDiff[];
  reloadOk?: boolean;
  warning?: string;
}

export interface LogRecordCounts {
  audit_log: number;
  cf_events: number;
  usage_records: number;
  usage_events: number;
  registration_task_events: number;
  lifecycle_task_logs: number;
  lifecycle_events: number;
  proxy_usage_records: number;
  terminal_billing_holds: number;
}

export interface LogClearResponse {
  ok: boolean;
  deleted: LogRecordCounts;
  deleted_total: number;
  preserved_active_billing_holds: number;
  space_reclaimed: boolean;
  reclaim_warning: string;
  retention_days: number;
}

export interface ContextJournalClearResponse {
  ok: boolean;
  deleted_contexts: number;
  space_reclaimed: boolean;
  reclaim_warning: string;
  ttl_seconds: number;
  completed_at: number;
}

export interface SettingsPatch {
  section: SettingsSection;
  key?: string;
  value?: unknown;
  values?: SettingsValues;
  mode?: 'replace' | string;
}

export interface SettingsTemplate extends SettingsValues {
  id: string;
  name: string;
  description?: string;
  platform?: string;
  method?: string;
  identity_mode?: string;
  egress?: string;
  needs?: string[];
  saved: SettingsDiff[];
}

export interface AutomationPolicy {
  type: string;
  enabled?: boolean;
  config?: SettingsValues;
  [key: string]: unknown;
}

export interface AutomationSettings {
  policies: Record<string, AutomationPolicy>;
  stats: SettingsValues | null;
  readiness: ({ ready?: boolean; blockers?: string[] } & SettingsValues) | null;
  automationErrors: Record<string, string>;
}

export interface ProviderSetting {
  id?: string;
  type: string;
  key: string;
  display_name?: string;
  enabled?: boolean;
  priority?: number;
  config?: SettingsValues;
  [key: string]: unknown;
}

export interface RegistrarSettings {
  cfg: SettingsValues;
  smsProviders: ProviderSetting[];
  mailboxProviders: ProviderSetting[];
  captchaProviders: ProviderSetting[];
  emailProviders: ProviderSetting[];
  registrarErrors: Record<string, string>;
}

export interface SettingsOption {
  label: string;
  value: string;
  [key: string]: unknown;
}

export interface SettingsGroup extends SettingsValues {
  name: string;
}

export interface SettingsEgress extends SettingsValues {
  id: string;
  name?: string;
  type?: string;
}

export interface ProviderOptions extends SettingsValues {
  sms: Array<string | SettingsOption>;
  mailbox: Array<string | SettingsOption>;
  captcha: Array<string | SettingsOption>;
}

export interface SharedSettingsOptions {
  groups: SettingsGroup[];
  egresses: SettingsEgress[];
  providerOpts: ProviderOptions;
  error: ApiError | null;
}

export interface RegistrarSaveInput {
  providers: ProviderSetting[];
  values: SettingsValues;
}

export type AdvancedSettingsKind = 'thinking' | 'moderation';

export interface ThinkingOverride extends SettingsValues {
  mode: string;
  level?: string;
  budget?: number;
}

export interface ThinkingSettings extends SettingsValues {
  enabled: boolean;
  default_mode: string;
  default_level: string;
  default_budget: number;
  providers: Record<string, ThinkingOverride>;
  models: Record<string, ThinkingOverride>;
}

export interface ModerationSettings extends SettingsValues {
  enabled: boolean;
  model: string;
  auto_translate: boolean;
  words: string[];
}

export type AdvancedSettings = ThinkingSettings | ModerationSettings;

export interface AdvancedSettingsSaveInput {
  kind: AdvancedSettingsKind;
  values: SettingsValues;
}
