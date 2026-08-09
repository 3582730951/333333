export interface SystemProcess {
  pid: number | string;
  comm?: string;
  kind?: string;
  rss_kb?: number;
  [key: string]: unknown;
}

export interface SupervisorEvent {
  time_unix?: number;
  module?: string;
  type?: string;
  message?: string;
  panic?: string;
  uptime_millis?: number;
  backoff_millis?: number;
  [key: string]: unknown;
}

export interface SupervisorModule {
  name?: string;
  status?: string;
  restart_count?: number;
  panic_count?: number;
  unexpected_exit_count?: number;
  uptime_millis?: number;
  last_uptime_millis?: number;
  restart_backoff_millis?: number;
  next_restart_unix?: number;
  last_event_unix?: number;
  last_message?: string;
  last_panic?: string;
  [key: string]: unknown;
}

export interface SystemMetrics {
  supported: boolean;
  uptime_seconds?: number;
  cpu?: { usage_pct?: number; cores?: number; load1?: number; [key: string]: unknown };
  mem?: { used_pct?: number; used_kb?: number; total_kb?: number; [key: string]: unknown };
  disk?: { used_pct?: number; used_bytes?: number; total_bytes?: number; free_bytes?: number; path?: string; [key: string]: unknown };
  network?: { interfaces?: number; interface_names?: string[]; rx_bytes?: number; tx_bytes?: number; rx_bytes_per_sec?: number; tx_bytes_per_sec?: number; total_bytes_per_sec?: number; [key: string]: unknown };
  disk_guard?: {
    level: string;
    free_percent: number;
    free_bytes?: number;
    // One entry per distinct filesystem backing a managed path, each tagged with the roles that
    // live on it (data | spool | journal | diagnostics | database). Several roles collapse into
    // one entry when they share a device, which is the common single-volume deployment.
    filesystems?: Array<{ roles?: string[]; level?: string; free_percent?: number; free_bytes?: number }>;
    forced_context_ttl_seconds?: number;
    contexts_deleted?: number;
    goals_deleted?: number;
    goal_bytes_reclaimed?: number;
    logs_deleted?: number;
    last_run_at?: number;
    database_writable?: boolean;
    journal_writable?: boolean;
    spool_writable?: boolean;
    background_paused?: boolean;
    large_requests_paused?: boolean;
    admission_blocked?: boolean;
    last_error?: string;
    [key: string]: unknown;
  };
  registration?: { total_rss_kb?: number; node?: number; chrome?: number; xvfb?: number; procs?: SystemProcess[]; [key: string]: unknown };
  go?: { goroutines?: number; sys_bytes?: number; [key: string]: unknown };
  supervisor_events?: SupervisorEvent[];
  supervisor_modules?: SupervisorModule[];
  [key: string]: unknown;
}
