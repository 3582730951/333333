import type { ApiError } from '../../../model/contracts';

export interface LifecycleTask {
  id: string;
  task_type?: string;
  method?: string;
  platform?: string;
  group_name?: string;
  egress_id?: string;
  status?: string;
  target_count?: number;
  completed_count?: number;
  success_count?: number;
  failed_count?: number;
  created_at?: number;
  updated_at?: number;
  [key: string]: unknown;
}

export interface LifecycleService {
  id?: string;
  name?: string;
  service?: string;
  status?: string;
  last_error?: string;
  [key: string]: unknown;
}

export interface LifecycleGroup {
  name: string;
  [key: string]: unknown;
}

export interface LifecycleEgress {
  id: string;
  name?: string;
  type?: string;
  [key: string]: unknown;
}

export type LifecycleProviderOption = string | { label: string; value: string; [key: string]: unknown };

export interface LifecycleProviderOptions {
  sms: LifecycleProviderOption[];
  mailbox: LifecycleProviderOption[];
  captcha: LifecycleProviderOption[];
}

export interface LifecycleDashboard {
  tasks: LifecycleTask[];
  services: LifecycleService[];
  serviceError: ApiError | null;
}

export interface LifecycleOptions {
  groups: LifecycleGroup[];
  egresses: LifecycleEgress[];
  providers: LifecycleProviderOptions;
  error: ApiError | null;
}

export interface LifecycleTaskCreateInput {
  task_type: string;
  platform: string;
  target_count: number;
  group_name: string;
  concurrency: number;
  egress_id: string;
  sms_provider: string;
  mailbox_provider: string;
  payment_method: string;
  password: string;
}
