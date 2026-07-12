package storage

// lifecycleSchemaSQL contains all lifecycle management related tables
const lifecycleSchemaSQL = `
-- Proxy configurations (fixed IP, dynamic IP, rotating gateway)
CREATE TABLE IF NOT EXISTS proxy_configs(
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  proxy_type TEXT NOT NULL,
  proxy_provider TEXT NOT NULL,
  proxy_url TEXT NOT NULL DEFAULT '',
  api_url TEXT NOT NULL DEFAULT '',
  api_key TEXT NOT NULL DEFAULT '',
  api_params TEXT NOT NULL DEFAULT '{}',
  gateway_url TEXT NOT NULL DEFAULT '',
  luminati_username TEXT NOT NULL DEFAULT '',
  luminati_password TEXT NOT NULL DEFAULT '',
  luminati_zone TEXT NOT NULL DEFAULT '',
  oxylabs_username TEXT NOT NULL DEFAULT '',
  oxylabs_password TEXT NOT NULL DEFAULT '',
  country TEXT NOT NULL DEFAULT '',
  fingerprint_enabled INTEGER NOT NULL DEFAULT 0,
  fingerprint_mode TEXT NOT NULL DEFAULT 'per_account',
  total_used INTEGER NOT NULL DEFAULT 0,
  success_count INTEGER NOT NULL DEFAULT 0,
  fail_count INTEGER NOT NULL DEFAULT 0,
  last_used_at INTEGER NOT NULL DEFAULT 0,
  enabled INTEGER NOT NULL DEFAULT 1,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_proxy_configs_enabled ON proxy_configs(enabled);

-- Proxy usage records
CREATE TABLE IF NOT EXISTS proxy_usage_records(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  proxy_config_id TEXT NOT NULL,
  account_id TEXT NOT NULL DEFAULT '',
  task_id TEXT NOT NULL DEFAULT '',
  extracted_ip TEXT NOT NULL DEFAULT '',
  proxy_url TEXT NOT NULL DEFAULT '',
  success INTEGER NOT NULL DEFAULT 0,
  error_message TEXT NOT NULL DEFAULT '',
  duration_ms INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  FOREIGN KEY(proxy_config_id) REFERENCES proxy_configs(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_proxy_usage_config ON proxy_usage_records(proxy_config_id);
CREATE INDEX IF NOT EXISTS idx_proxy_usage_task ON proxy_usage_records(task_id);
CREATE INDEX IF NOT EXISTS idx_proxy_usage_created_at ON proxy_usage_records(created_at);

-- Mailbox providers
CREATE TABLE IF NOT EXISTS mailbox_providers(
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  provider_type TEXT NOT NULL,
  config_json TEXT NOT NULL DEFAULT '{}',
  enabled INTEGER NOT NULL DEFAULT 1,
  is_default INTEGER NOT NULL DEFAULT 0,
  total_used INTEGER NOT NULL DEFAULT 0,
  success_count INTEGER NOT NULL DEFAULT 0,
  fail_count INTEGER NOT NULL DEFAULT 0,
  last_used_at INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_mailbox_providers_enabled ON mailbox_providers(enabled);

-- SMS providers
CREATE TABLE IF NOT EXISTS sms_providers(
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  provider_type TEXT NOT NULL,
  config_json TEXT NOT NULL DEFAULT '{}',
  enabled INTEGER NOT NULL DEFAULT 1,
  is_default INTEGER NOT NULL DEFAULT 0,
  total_used INTEGER NOT NULL DEFAULT 0,
  success_count INTEGER NOT NULL DEFAULT 0,
  fail_count INTEGER NOT NULL DEFAULT 0,
  last_used_at INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sms_providers_enabled ON sms_providers(enabled);

-- Lifecycle tasks
CREATE TABLE IF NOT EXISTS lifecycle_tasks(
  id TEXT PRIMARY KEY,
  task_type TEXT NOT NULL,
  platform TEXT NOT NULL DEFAULT 'chatgpt',
  status TEXT NOT NULL DEFAULT 'pending',
  config_json TEXT NOT NULL DEFAULT '{}',
  target_count INTEGER NOT NULL DEFAULT 0,
  completed_count INTEGER NOT NULL DEFAULT 0,
  success_count INTEGER NOT NULL DEFAULT 0,
  failed_count INTEGER NOT NULL DEFAULT 0,
  result_json TEXT NOT NULL DEFAULT '{}',
  created_at INTEGER NOT NULL,
  started_at INTEGER NOT NULL DEFAULT 0,
  finished_at INTEGER NOT NULL DEFAULT 0,
  created_by TEXT NOT NULL DEFAULT '',
  priority INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_lifecycle_tasks_status ON lifecycle_tasks(status);
CREATE INDEX IF NOT EXISTS idx_lifecycle_tasks_created ON lifecycle_tasks(created_at DESC);

-- Task logs
CREATE TABLE IF NOT EXISTS lifecycle_task_logs(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  task_id TEXT NOT NULL,
  account_index INTEGER NOT NULL DEFAULT 0,
  level TEXT NOT NULL DEFAULT 'info',
  message TEXT NOT NULL,
  timestamp INTEGER NOT NULL,
  FOREIGN KEY(task_id) REFERENCES lifecycle_tasks(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_lifecycle_logs_task_time ON lifecycle_task_logs(task_id, timestamp);
CREATE INDEX IF NOT EXISTS idx_lifecycle_logs_timestamp ON lifecycle_task_logs(timestamp);

-- GoPay accounts pool
CREATE TABLE IF NOT EXISTS gopay_accounts(
  id TEXT PRIMARY KEY,
  phone TEXT NOT NULL UNIQUE,
  pin TEXT NOT NULL,
  access_token TEXT NOT NULL DEFAULT '',
  refresh_token TEXT NOT NULL DEFAULT '',
  balance_idr INTEGER NOT NULL DEFAULT 0,
  status TEXT NOT NULL DEFAULT 'active',
  usage_count INTEGER NOT NULL DEFAULT 0,
  last_used_at INTEGER NOT NULL DEFAULT 0,
  last_balance_check INTEGER NOT NULL DEFAULT 0,
  notes TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_gopay_status ON gopay_accounts(status);

-- PayPal accounts pool
CREATE TABLE IF NOT EXISTS paypal_accounts(
  id TEXT PRIMARY KEY,
  email TEXT NOT NULL UNIQUE,
  password TEXT NOT NULL,
  region TEXT NOT NULL DEFAULT 'us',
  status TEXT NOT NULL DEFAULT 'active',
  balance_usd REAL NOT NULL DEFAULT 0,
  usage_count INTEGER NOT NULL DEFAULT 0,
  last_used_at INTEGER NOT NULL DEFAULT 0,
  last_balance_check INTEGER NOT NULL DEFAULT 0,
  notes TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_paypal_status ON paypal_accounts(status);

-- Lifecycle events
CREATE TABLE IF NOT EXISTS lifecycle_events(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  account_id TEXT NOT NULL,
  event_type TEXT NOT NULL,
  event_data TEXT NOT NULL DEFAULT '{}',
  task_id TEXT NOT NULL DEFAULT '',
  timestamp INTEGER NOT NULL,
  FOREIGN KEY(account_id) REFERENCES accounts(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_lifecycle_events_account ON lifecycle_events(account_id, timestamp DESC);
CREATE INDEX IF NOT EXISTS idx_lifecycle_events_type ON lifecycle_events(event_type, timestamp DESC);
CREATE INDEX IF NOT EXISTS idx_lifecycle_events_timestamp ON lifecycle_events(timestamp);
`
