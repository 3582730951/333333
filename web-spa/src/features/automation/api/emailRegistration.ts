import { get, post } from '../../../api.js';

export interface EmailRegSettings {
  count: number;
  group_name: string;
  egress_pool_id?: string;
  concurrency: number;
}

export interface EmailRegJob {
  id: string;
  platform: string;
  method: string;
  total: number;
  succeeded: number;
  failed: number;
  status: string;
  started_at?: number;
  completed_at?: number;
  error?: string;
  created_at: number;
  updated_at: number;
}

export interface EmailRegEvent {
  id: number;
  task_id: string;
  level: string;
  message: string;
  detail_json?: string;
  created_at: number;
}

export interface EmailRegStartResponse {
  job_id: string;
  status: string;
  count: number;
  group_name: string;
  egress_pool_id?: string;
  available_emails: number;
}

export async function fetchEmailRegConfig(): Promise<EmailRegSettings> {
  return get('/admin/register/email/config');
}

export async function saveEmailRegConfig(settings: EmailRegSettings): Promise<EmailRegSettings> {
  return post('/admin/register/email/config', settings);
}

export async function startEmailRegistration(input: EmailRegSettings): Promise<EmailRegStartResponse> {
  return post('/admin/register/email/start', input);
}

export async function fetchEmailRegJobs(limit = 50): Promise<{ jobs: EmailRegJob[] }> {
  return get(`/admin/register/email/jobs?limit=${limit}`);
}

export async function fetchEmailRegJobStatus(id: string): Promise<EmailRegJob> {
  return get(`/admin/register/email/job/status?id=${encodeURIComponent(id)}`);
}

export async function fetchEmailRegJobEvents(id: string): Promise<{ job_id: string; events: EmailRegEvent[] }> {
  return get(`/admin/register/email/job/events?id=${encodeURIComponent(id)}`);
}

export async function cancelEmailRegJob(id: string): Promise<{ job_id: string; status: string }> {
  return post(`/admin/register/email/job/${encodeURIComponent(id)}`);
}
