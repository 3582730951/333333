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
}).passthrough();

export async function fetchSystemMetrics(signal?: AbortSignal): Promise<SystemMetrics> {
  return parseApiResponse(systemMetricsSchema, await get('/admin/system', undefined, { signal })) as SystemMetrics;
}
