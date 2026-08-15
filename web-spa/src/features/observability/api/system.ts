import { z } from 'zod';
import { get } from '../../../api.js';
import { parseApiResponse } from '../../../api/contracts';
import type { SystemMetrics } from '../model/system';

const numericFields = {
  usage_pct: z.coerce.number().optional(),
  cores: z.coerce.number().optional(),
  load1: z.coerce.number().optional(),
};

const processSchema = z.object({
  pid: z.union([z.string(), z.number()]),
  comm: z.string().optional(),
  kind: z.string().optional(),
  rss_kb: z.coerce.number().optional(),
}).passthrough();

const eventSchema = z.object({
  time_unix: z.coerce.number().optional(),
  module: z.string().optional(),
  type: z.string().optional(),
  message: z.string().optional(),
  panic: z.string().optional(),
  uptime_millis: z.coerce.number().optional(),
  backoff_millis: z.coerce.number().optional(),
}).passthrough();

const moduleSchema = z.object({
  name: z.string().optional(),
  status: z.string().optional(),
  restart_count: z.coerce.number().optional(),
  panic_count: z.coerce.number().optional(),
  unexpected_exit_count: z.coerce.number().optional(),
  uptime_millis: z.coerce.number().optional(),
  last_uptime_millis: z.coerce.number().optional(),
  restart_backoff_millis: z.coerce.number().optional(),
  next_restart_unix: z.coerce.number().optional(),
  last_event_unix: z.coerce.number().optional(),
  last_message: z.string().optional(),
  last_panic: z.string().optional(),
}).passthrough();

const passiveHealthSeriesSchema = z.object({
  provider: z.string(),
  model: z.string(),
  egress_id: z.string(),
  health: z.string(),
  observations: z.coerce.number(),
  health_samples: z.coerce.number(),
  successes: z.coerce.number(),
  failures: z.coerce.number(),
  canceled: z.coerce.number(),
  rate_limited: z.coerce.number(),
  success_ewma: z.coerce.number(),
  latency_ewma_ms: z.coerce.number(),
  last_status_code: z.coerce.number(),
  last_error_class: z.string().optional(),
  first_observed_at: z.coerce.number(),
  last_observed_at: z.coerce.number(),
}).passthrough();

const passiveProviderHealthSchema = z.object({
  generated_at: z.coerce.number().optional(),
  retention_seconds: z.coerce.number().optional(),
  max_series: z.coerce.number().optional(),
  series_count: z.coerce.number().default(0),
  evictions: z.coerce.number().optional(),
  series: z.array(passiveHealthSeriesSchema).nullish().transform((value) => value ?? []),
}).passthrough();

const compatibilityManifestSchema = z.object({
  enabled: z.boolean().default(false),
  source: z.string().optional(),
  state: z.string().optional(),
  digest: z.string().optional(),
  generation: z.coerce.number().optional(),
  fetched_at: z.coerce.number().optional(),
  expires_at: z.coerce.number().optional(),
  last_attempt_at: z.coerce.number().optional(),
  last_success_at: z.coerce.number().optional(),
  last_error: z.string().optional(),
  snapshot_slot: z.string().optional(),
  signature_checked: z.boolean().optional(),
  canary: z.string().optional(),
  model_count: z.coerce.number().optional(),
}).passthrough();

export const systemMetricsSchema = z.object({
  supported: z.boolean().default(true),
  uptime_seconds: z.coerce.number().optional(),
  cpu: z.object(numericFields).passthrough().optional(),
  mem: z.object({ used_pct: z.coerce.number().optional(), used_kb: z.coerce.number().optional(), total_kb: z.coerce.number().optional() }).passthrough().optional(),
  disk: z.object({
    used_pct: z.coerce.number().optional(),
    used_bytes: z.coerce.number().optional(),
    total_bytes: z.coerce.number().optional(),
    free_bytes: z.coerce.number().optional(),
    path: z.string().optional(),
  }).passthrough().optional(),
  network: z.object({
    interfaces: z.coerce.number().optional(),
    interface_names: z.array(z.string()).nullish().transform((value) => value ?? []),
    rx_bytes: z.coerce.number().optional(),
    tx_bytes: z.coerce.number().optional(),
    rx_bytes_per_sec: z.coerce.number().optional(),
    tx_bytes_per_sec: z.coerce.number().optional(),
    total_bytes_per_sec: z.coerce.number().optional(),
  }).passthrough().optional(),
  disk_guard: z.object({
    level: z.string(),
    free_percent: z.coerce.number(),
    forced_context_ttl_seconds: z.coerce.number().optional(),
    contexts_deleted: z.coerce.number().optional(),
    logs_deleted: z.coerce.number().optional(),
    last_run_at: z.coerce.number().optional(),
    last_error: z.string().optional(),
  }).passthrough().optional(),
  registration: z.object({
    total_rss_kb: z.coerce.number().optional(),
    node: z.coerce.number().optional(),
    chrome: z.coerce.number().optional(),
    xvfb: z.coerce.number().optional(),
    procs: z.array(processSchema).nullish().transform((value) => value ?? []),
  }).passthrough().optional(),
  go: z.object({ goroutines: z.coerce.number().optional(), sys_bytes: z.coerce.number().optional() }).passthrough().optional(),
  supervisor_events: z.array(eventSchema).nullish().transform((value) => value ?? []),
  supervisor_modules: z.array(moduleSchema).nullish().transform((value) => value ?? []),
  compatibility_manifest: compatibilityManifestSchema.optional(),
  passive_provider_health: passiveProviderHealthSchema.optional(),
}).passthrough();

export async function fetchSystemMetrics(signal?: AbortSignal): Promise<SystemMetrics> {
  return parseApiResponse(systemMetricsSchema, await get('/admin/system', undefined, { signal })) as SystemMetrics;
}
