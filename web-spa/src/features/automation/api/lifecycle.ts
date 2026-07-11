import { z } from 'zod';
import { del, get, post } from '../../../api.js';
import { createApiError, parseApiResponse } from '../../../api/contracts';
import type {
  LifecycleDashboard, LifecycleEgress, LifecycleGroup, LifecycleOptions,
  LifecycleProviderOptions, LifecycleService, LifecycleTask, LifecycleTaskCreateInput,
} from '../model/lifecycle';

const taskSchema = z.object({
  id: z.string(),
  task_type: z.string().optional(),
  status: z.string().optional(),
}).passthrough();

export const lifecycleTasksResponseSchema = z.union([
  z.array(taskSchema),
  z.object({ tasks: z.array(taskSchema).optional() }).passthrough().transform((value) => value.tasks ?? []),
]);

const serviceObjectSchema = z.object({
  id: z.string().optional(),
  name: z.string().optional(),
  service: z.string().optional(),
  status: z.string().optional(),
  last_error: z.string().optional(),
}).passthrough();
const serviceSchema = z.union([
  serviceObjectSchema,
  z.string().transform((value) => ({ name: value, status: value })),
  z.boolean().transform((value) => ({ name: 'service', status: value ? 'alive' : 'unreachable' })),
]);
export const lifecycleServicesResponseSchema = z.union([
  z.array(serviceSchema),
  z.object({ services: z.array(serviceSchema) }).passthrough().transform((value) => value.services),
  z.record(z.string(), z.unknown()).transform((value) => Object.values(value)).pipe(z.array(serviceSchema)),
]);

const groupSchema = z.object({ name: z.string() }).passthrough();
const groupsResponseSchema = z.union([
  z.array(groupSchema),
  z.object({ groups: z.array(groupSchema).optional() }).passthrough().transform((value) => value.groups ?? []),
]);

const egressSchema = z.object({ id: z.string() }).passthrough();
const egressResponseSchema = z.union([
  z.array(egressSchema),
  z.object({ profiles: z.array(egressSchema).optional(), egresses: z.array(egressSchema).optional() })
    .passthrough()
    .transform((value) => value.profiles ?? value.egresses ?? []),
]);

const providerOptionSchema = z.union([
  z.string(),
  z.object({ label: z.string(), value: z.string() }).passthrough(),
]);
export const lifecycleProviderOptionsSchema = z.object({
  sms: z.array(providerOptionSchema).optional(),
  mailbox: z.array(providerOptionSchema).optional(),
  captcha: z.array(providerOptionSchema).optional(),
}).passthrough().transform((value) => ({
  sms: value.sms ?? [],
  mailbox: value.mailbox ?? [],
  captcha: value.captcha ?? [],
}));

function partialError(code: string, message: string, failures: unknown[]) {
  return createApiError({ code, userMessage: message, retryable: true, cause: failures });
}

export async function fetchLifecycleTasks(signal?: AbortSignal): Promise<LifecycleTask[]> {
  return parseApiResponse(lifecycleTasksResponseSchema, await get('/admin/lifecycle/tasks', undefined, { signal })) as LifecycleTask[];
}

export async function fetchLifecycleServices(signal?: AbortSignal): Promise<LifecycleService[]> {
  return parseApiResponse(lifecycleServicesResponseSchema, await get('/admin/lifecycle/services', undefined, { signal })) as LifecycleService[];
}

export async function fetchLifecycleDashboard(signal?: AbortSignal): Promise<LifecycleDashboard> {
  const [tasks, services] = await Promise.allSettled([
    fetchLifecycleTasks(signal),
    fetchLifecycleServices(signal),
  ]);
  if (tasks.status === 'rejected') throw tasks.reason;
  if (services.status === 'fulfilled') return { tasks: tasks.value, services: services.value, serviceError: null };
  return {
    tasks: tasks.value,
    services: [],
    serviceError: partialError('LIFECYCLE_SERVICES_FAILED', '任务已加载，但外部服务状态暂时不可用。', [services.reason]),
  };
}

async function fetchLifecycleGroups(signal?: AbortSignal): Promise<LifecycleGroup[]> {
  return parseApiResponse(groupsResponseSchema, await get('/admin/groups', undefined, { signal })) as LifecycleGroup[];
}

async function fetchLifecycleEgresses(signal?: AbortSignal): Promise<LifecycleEgress[]> {
  return parseApiResponse(egressResponseSchema, await get('/admin/egress-profiles', undefined, { signal })) as LifecycleEgress[];
}

async function fetchLifecycleProviders(signal?: AbortSignal): Promise<LifecycleProviderOptions> {
  return parseApiResponse(lifecycleProviderOptionsSchema, await get('/admin/register/providers/options', undefined, { signal })) as LifecycleProviderOptions;
}

export async function fetchLifecycleOptions(signal?: AbortSignal): Promise<LifecycleOptions> {
  const results = await Promise.allSettled([
    fetchLifecycleGroups(signal),
    fetchLifecycleEgresses(signal),
    fetchLifecycleProviders(signal),
  ]);
  const failures = results.filter((result) => result.status === 'rejected').map((result) => result.reason);
  return {
    groups: results[0].status === 'fulfilled' ? results[0].value : [],
    egresses: results[1].status === 'fulfilled' ? results[1].value : [],
    providers: results[2].status === 'fulfilled' ? results[2].value : { sms: [], mailbox: [], captcha: [] },
    error: failures.length ? partialError('LIFECYCLE_OPTIONS_FAILED', '部分表单选项读取失败，相关字段已暂时禁用。', failures) : null,
  };
}

export async function createLifecycleTask(input: LifecycleTaskCreateInput) {
  return post('/admin/lifecycle/tasks', input);
}

export async function cancelLifecycleTask(id: string) {
  return del(`/admin/lifecycle/tasks/${encodeURIComponent(id)}`);
}
