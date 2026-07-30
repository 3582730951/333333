#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

usage() {
  cat <<'USAGE'
Export every codex-pool account as a ZIP containing one JSON per account.

Zero-config:
  sudo ./scripts/maintenance/export-account-pool.sh

Options:
  --config PATH      Config JSON. Default auto-detects common install paths.
  --out PATH         Output ZIP path. Default: ./account-pool-YYYYmmdd-HHMMSS.zip
  --base-url URL     Admin HTTP base URL. Default derived from listen_addr.
  --admin-token TOK  Admin token. Default reads env/token file/config/data_dir key.
  --timeout SEC      HTTP timeout. Default: 1800.
  --direct-only      Skip HTTP and export from SQLite directly.
  --help             Show this help.
USAGE
}

log() { printf '%s\n' "$*" >&2; }
die() { log "ERROR: $*"; exit 1; }

script_dir="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(CDPATH= cd -- "$script_dir/../.." && pwd)"
config_path="${CODEX_POOL_CONFIG:-}"
out_path="${CODEX_POOL_EXPORT_OUT:-}"
base_url="${CODEX_POOL_BASE_URL:-}"
admin_token="${CODEX_POOL_ADMIN_TOKEN:-}"
timeout_seconds="${CODEX_POOL_EXPORT_TIMEOUT:-1800}"
direct_only=0

while (($#)); do
  case "$1" in
    --config) (($# >= 2)) || die "--config requires a path"; config_path="$2"; shift 2 ;;
    --out) (($# >= 2)) || die "--out requires a path"; out_path="$2"; shift 2 ;;
    --base-url) (($# >= 2)) || die "--base-url requires a URL"; base_url="$2"; shift 2 ;;
    --admin-token) (($# >= 2)) || die "--admin-token requires a token"; admin_token="$2"; shift 2 ;;
    --timeout) (($# >= 2)) || die "--timeout requires seconds"; timeout_seconds="$2"; shift 2 ;;
    --direct-only) direct_only=1; shift ;;
    --help|-h) usage; exit 0 ;;
    *) die "unknown argument: $1" ;;
  esac
done
[[ "$timeout_seconds" =~ ^[0-9]+$ && "$timeout_seconds" -gt 0 ]] || die "--timeout must be a positive integer"

if [[ -z "$config_path" ]]; then
  for candidate in \
    /etc/codex-pool/config.json \
    /var/lib/codex-pool/config.json \
    "$repo_root/config.local.json" \
    "$repo_root/config.json" \
    "$repo_root/config.example.json"; do
    [[ -f "$candidate" ]] && { config_path="$candidate"; break; }
  done
fi
[[ -n "$config_path" && -f "$config_path" ]] || die "config not found; pass --config /path/to/config.json"

if [[ -z "$out_path" ]]; then
  out_path="$PWD/account-pool-$(date -u +%Y%m%d-%H%M%S).zip"
fi
case "$out_path" in /*) ;; *) out_path="$PWD/$out_path" ;; esac
mkdir -p -- "$(dirname -- "$out_path")"

metadata_file="$(mktemp)"
count_file="$(mktemp)"
tmp_body="$(mktemp)"
tmp_headers="$(mktemp)"
cleanup() { rm -f -- "$metadata_file" "$count_file" "$tmp_body" "$tmp_headers" 2>/dev/null || true; }
trap cleanup EXIT

python3 - "$config_path" "$base_url" "$admin_token" >"$metadata_file" <<'PY'
import json, os, pathlib, shlex, sys
config_path, base_url_arg, token_arg = sys.argv[1:4]
cfg = json.loads(pathlib.Path(config_path).read_text())

def read_secret(path):
    path = (path or '').strip()
    if not path:
        return ''
    try:
        return pathlib.Path(path).read_text(errors='ignore').strip()
    except Exception:
        return ''

def derive_base_url():
    if base_url_arg.strip():
        return base_url_arg.strip().rstrip('/')
    listen = str(cfg.get('listen_addr') or '127.0.0.1:8787').strip()
    if listen.startswith(('http://', 'https://')):
        return listen.rstrip('/')
    host, port = '127.0.0.1', '8787'
    if listen.startswith('['):
        end = listen.find(']')
        host = listen[1:end] if end >= 0 else '::1'
        rest = listen[end+1:] if end >= 0 else ''
        if rest.startswith(':') and rest[1:]:
            port = rest[1:]
    elif ':' in listen:
        host, port = listen.rsplit(':', 1)
        host = host or '127.0.0.1'
        port = port or '8787'
    elif listen:
        port = listen
    if host in ('', '0.0.0.0', '::', '[::]'):
        host = '127.0.0.1'
    if ':' in host and not host.startswith('['):
        host = '[' + host + ']'
    return f'http://{host}:{port}'.rstrip('/')

def derive_token():
    if token_arg.strip():
        return token_arg.strip()
    if os.environ.get('CODEX_POOL_ADMIN_TOKEN', '').strip():
        return os.environ['CODEX_POOL_ADMIN_TOKEN'].strip()
    for path in [
        os.environ.get('CODEX_POOL_ADMIN_TOKEN_FILE', ''),
        os.path.join(os.environ.get('CREDENTIALS_DIRECTORY', ''), 'admin.token') if os.environ.get('CREDENTIALS_DIRECTORY') else '',
    ]:
        value = read_secret(path)
        if value:
            return value
    if str(cfg.get('admin_token') or '').strip():
        return str(cfg.get('admin_token')).strip()
    data_dir = str(cfg.get('data_dir') or '').strip()
    if data_dir:
        for path in [os.path.join(data_dir, 'keys', 'admin.token'), os.path.join(os.path.dirname(data_dir), 'data', 'keys', 'admin.token')]:
            value = read_secret(path)
            if value:
                return value
    return ''

database_path = str(cfg.get('database_path') or '').strip()
if database_path and not database_path.startswith('file:'):
    p = pathlib.Path(database_path)
    if not p.is_absolute():
        p = pathlib.Path(config_path).resolve().parent / p
    database_path = str(p.resolve())
items = {
    'BASE_URL': derive_base_url(),
    'ADMIN_TOKEN': derive_token(),
    'CONFIG_PATH': str(pathlib.Path(config_path).resolve()),
    'DATABASE_PATH': database_path,
    'STORAGE_DRIVER': str(cfg.get('storage_driver') or 'sqlite').strip().lower(),
}
for key, value in items.items():
    print(f"{key}={shlex.quote(value)}")
PY
# shellcheck disable=SC1090
source "$metadata_file"
config_path="$CONFIG_PATH"
base_url="$BASE_URL"
admin_token="$ADMIN_TOKEN"
database_path="${DATABASE_PATH:-}"
storage_driver="${STORAGE_DRIVER:-sqlite}"

expected_accounts=""
if [[ "$storage_driver" != "postgres" && -n "$database_path" && -f "$database_path" ]]; then
  expected_accounts="$(python3 - "$database_path" <<'PY'
import sqlite3, sys
try:
    con = sqlite3.connect(f"file:{sys.argv[1]}?mode=ro", uri=True)
    print(con.execute("SELECT COUNT(*) FROM accounts").fetchone()[0])
except Exception:
    pass
PY
)"
fi
if [[ "$expected_accounts" =~ ^[0-9]+$ ]]; then
  log "Detected account rows in database: $expected_accounts"
else
  expected_accounts=""
fi

convert_response_to_zip() {
  local src="$1" dst="$2" count_out="$3"
  [[ -s "$src" ]] || return 1
  python3 - "$src" "$dst" "$count_out" <<'PY'
import json, pathlib, re, sys, time, zipfile
src = pathlib.Path(sys.argv[1]); dst = pathlib.Path(sys.argv[2]); count_out = pathlib.Path(sys.argv[3])
raw = src.read_bytes(); head = raw[:64]

def is_portable(doc):
    return isinstance(doc, dict) and doc.get('type') == 'codex-account-pool-account' and isinstance(doc.get('account'), dict)

def items_from_json(doc):
    if isinstance(doc, list):
        return doc
    if is_portable(doc):
        return [doc]
    if isinstance(doc, dict):
        for key in ('accounts', 'rows', 'items', 'data'):
            value = doc.get(key)
            if isinstance(value, list):
                return value
    return []

def intv(v):
    try: return int(v or 0)
    except Exception: return 0

def boolv(v):
    if isinstance(v, bool): return v
    if isinstance(v, (int, float)): return v != 0
    return str(v).strip().lower() in ('1','true','yes','on')

def is_custom_provider(provider):
    return str(provider or '').strip() not in ('', 'codex', 'claude', 'kiro', 'antigravity')

def portable_from_item(item, index):
    if is_portable(item):
        return item
    if not isinstance(item, dict):
        raise ValueError('item is not object')
    account = item.get('account') if isinstance(item.get('account'), dict) else None
    token = item.get('token') if isinstance(item.get('token'), dict) else None
    if account is None:
        aid = item.get('id') or item.get('account_id') or item.get('email') or f'account-{index+1}'
        account = {
            'id': str(aid),
            'label': item.get('label') or item.get('name') or item.get('email') or str(aid),
            'group_name': item.get('group_name') or item.get('group') or 'cyber',
            'upstream_account_id': item.get('upstream_account_id') or '',
            'chatgpt_user_id': item.get('chatgpt_user_id') or '',
            'email': item.get('email') or '',
            'plan_type': item.get('plan_type') or item.get('plan') or '',
            'provider': item.get('provider') or '',
            'status': item.get('status') or 'active',
            'is_fedramp': boolv(item.get('is_fedramp')),
            'ignore_rate_limit_controls': boolv(item.get('ignore_rate_limit_controls')),
            'quarantine_until': intv(item.get('quarantine_until')),
            'quarantine_reason': item.get('quarantine_reason') or '',
            'created_at': intv(item.get('created_at')),
            'updated_at': intv(item.get('updated_at')),
        }
    if is_custom_provider(account.get('provider')) and not isinstance(item.get('custom_provider'), dict):
        raise ValueError('legacy JSON contains custom provider rows without provider definitions; direct SQLite export is required')
    if token is None:
        token = {
            'auth_method': item.get('auth_method') or '',
            'credential_mode': item.get('credential_mode') or '',
            'access_token': item.get('access_token') or '',
            'refresh_token': item.get('refresh_token') or '',
            'openai_api_key': item.get('openai_api_key') or item.get('api_key') or '',
            'id_token_raw': item.get('id_token_raw') or item.get('id_token') or '',
            'last_refresh': intv(item.get('last_refresh')),
            'expires_at': intv(item.get('expires_at')),
            'scopes': item.get('scopes') or '',
            'oauth_rate_limit_tier': item.get('oauth_rate_limit_tier') or '',
            'created_at': intv(item.get('token_created_at') or item.get('created_at')),
            'updated_at': intv(item.get('token_updated_at') or item.get('updated_at')),
        }
    doc = {'type':'codex-account-pool-account','version':1,'exported_at':int(time.time()),'account':account,'token':token}
    if isinstance(item.get('custom_provider'), dict):
        doc['custom_provider'] = item['custom_provider']
    if isinstance(item.get('egress_profiles'), list):
        doc['egress_profiles'] = item['egress_profiles']
    return doc

def zip_name(doc, index):
    account = doc.get('account') if isinstance(doc.get('account'), dict) else {}
    raw_name = str(account.get('id') or account.get('label') or account.get('email') or f'account-{index+1}')
    safe = re.sub(r'[^A-Za-z0-9_.-]+', '_', raw_name).strip('._-')[:96] or f'account-{index+1}'
    return f'account-{safe}.json'

if head.startswith((b'PK\x03\x04', b'PK\x05\x06', b'PK\x07\x08')):
    count = 0
    with zipfile.ZipFile(src, 'r') as zf:
        bad = zf.testzip()
        if bad: raise SystemExit(f'bad zip member: {bad}')
        for name in zf.namelist():
            if name.endswith('/'): continue
            try: doc = json.loads(zf.read(name).decode('utf-8'))
            except Exception: continue
            if is_portable(doc): count += 1
    if count <= 0: raise SystemExit('zip contains no account backup JSON files')
    dst.write_bytes(raw); count_out.write_text(str(count)); raise SystemExit(0)

try:
    doc = json.loads(raw.decode('utf-8'))
except Exception as exc:
    raise SystemExit(f'not zip/json: {exc}')
items = items_from_json(doc)
if not items:
    raise SystemExit('JSON response is not account list')
dst.parent.mkdir(parents=True, exist_ok=True)
used = set()
with zipfile.ZipFile(dst, 'w', compression=zipfile.ZIP_DEFLATED) as zf:
    for index, item in enumerate(items):
        portable = portable_from_item(item, index)
        name = zip_name(portable, index)
        if name in used: name = name[:-5] + f'-{index+1}.json'
        used.add(name)
        zf.writestr(name, json.dumps(portable, ensure_ascii=False, indent=2).encode('utf-8') + b'\n')
with zipfile.ZipFile(dst, 'r') as zf:
    bad = zf.testzip()
    if bad: raise SystemExit(f'bad zip member after conversion: {bad}')
count_out.write_text(str(len(used)))
PY
}

try_api_export() {
  local format="$1" label="$2"
  : >"$count_file"
  : >"$tmp_headers"
  : >"$tmp_body"
  local url="${base_url%/}/admin/accounts/export?format=${format}"
  local curl_args=(--noproxy '*' --fail --silent --show-error --location --connect-timeout 2 --max-time "$timeout_seconds" -D "$tmp_headers" -o "$tmp_body")
  [[ -n "$admin_token" ]] && curl_args+=(-H "Authorization: Bearer $admin_token")
  log "Exporting account pool through admin API (${label}): $url"
  if ! curl "${curl_args[@]}" "$url"; then
    log "Admin API ${label} export did not complete."
    return 1
  fi
  if ! convert_response_to_zip "$tmp_body" "$out_path" "$count_file"; then
    log "Admin API ${label} response was not a usable account export."
    return 1
  fi
  local exported_count
  exported_count="$(cat "$count_file" 2>/dev/null || true)"
  if [[ -n "$expected_accounts" && "$expected_accounts" -gt 0 && "$exported_count" != "$expected_accounts" ]]; then
    log "Admin API ${label} archive contains ${exported_count:-0} account JSON file(s), but database has $expected_accounts account row(s)."
    rm -f -- "$out_path"
    return 1
  fi
  chmod 600 "$out_path" || true
  log "OK: exported account pool ZIP to $out_path (${exported_count:-unknown} account files)"
  printf '%s\n' "$out_path"
  return 0
}

if (( direct_only == 0 )) && command -v curl >/dev/null 2>&1; then
  try_api_export backup portable && exit 0
  try_api_export json legacy-json && exit 0
fi

[[ "$storage_driver" != "postgres" ]] || die "direct PostgreSQL export needs the admin API; fix admin API access or pass --base-url/--admin-token"
[[ -n "$database_path" && -f "$database_path" ]] || die "SQLite database not found for direct export: ${database_path:-<empty>}"

log "Exporting account pool directly from SQLite without Go: $database_path"
python3 - "$database_path" "$out_path" "$expected_accounts" <<'PY'
import json, pathlib, re, sqlite3, sys, time, zipfile

db_path = pathlib.Path(sys.argv[1]); out_path = pathlib.Path(sys.argv[2])
expected = int(sys.argv[3]) if len(sys.argv) > 3 and str(sys.argv[3]).isdigit() else 0

def table_exists(con, name):
    return con.execute("SELECT 1 FROM sqlite_master WHERE type='table' AND name=?", (name,)).fetchone() is not None

def columns(con, table):
    if not table_exists(con, table): return []
    return [row[1] for row in con.execute(f"PRAGMA table_info({table})")]

def fetch_by_account(con, table, account_ids):
    cols = columns(con, table)
    if not cols or 'account_id' not in cols or not account_ids: return {}
    placeholders = ','.join(['?'] * len(account_ids))
    select_cols = ', '.join('"%s"' % c.replace('"','""') for c in cols)
    out = {}
    for row in con.execute(f"SELECT {select_cols} FROM {table} WHERE account_id IN ({placeholders})", account_ids):
        item = dict(zip(cols, row))
        out.setdefault(str(item.get('account_id') or ''), []).append(item)
    return out

def fetch_by_id(con, table, ids, id_col='id'):
    cols = columns(con, table)
    clean = []
    seen = set()
    for value in ids:
        value = str(value or '').strip()
        if value and value not in seen:
            clean.append(value); seen.add(value)
    if not cols or id_col not in cols or not clean: return {}
    placeholders = ','.join(['?'] * len(clean))
    select_cols = ', '.join('"%s"' % c.replace('"','""') for c in cols)
    out = {}
    for row in con.execute(f"SELECT {select_cols} FROM {table} WHERE {id_col} IN ({placeholders})", clean):
        item = dict(zip(cols, row))
        out[str(item.get(id_col) or '')] = item
    return out

def first(mapping, account_id):
    return (mapping.get(account_id) or [None])[0]

def intv(v):
    try: return int(v or 0)
    except Exception: return 0

def boolv(v):
    if isinstance(v, bool): return v
    if isinstance(v, (int, float)): return v != 0
    return str(v).strip().lower() in ('1','true','yes','on')

def prune(v):
    if isinstance(v, dict):
        return {k: prune(x) for k, x in v.items() if x not in (None, '', [])}
    if isinstance(v, list):
        return [prune(x) for x in v]
    return v

def safe_name(value, index):
    safe = re.sub(r'[^A-Za-z0-9_.-]+', '_', str(value or '')).strip('._-')[:96]
    return f"account-{safe or ('account-%d' % (index+1))}.json"

def json_list(raw):
    try:
        value = json.loads(str(raw or '').strip() or '[]')
    except Exception:
        return []
    return [str(x).strip() for x in value if str(x).strip()] if isinstance(value, list) else []

def json_dict(raw):
    try:
        value = json.loads(str(raw or '').strip() or '{}')
    except Exception:
        return {}
    return {str(k).strip(): str(v).strip() for k, v in value.items() if str(k).strip() and str(v).strip()} if isinstance(value, dict) else {}

def split_ids(raw):
    return [x.strip() for x in str(raw or '').split(',') if x.strip()]

def is_custom_provider(provider):
    return str(provider or '').strip() not in ('', 'codex', 'claude', 'kiro', 'antigravity')

def custom_provider_doc(row):
    if not row: return None
    return {
        'id': row.get('id') or '',
        'name': row.get('name') or row.get('id') or '',
        'base_url': row.get('base_url') or '',
        'upstream_protocol': row.get('upstream_protocol') or 'chat_completions',
        'transport_profile': row.get('transport_profile') or 'generic',
        'egress_ids': json_list(row.get('egress_ids')),
        'enabled': boolv(row.get('enabled')),
        'auto_discover_models': boolv(row.get('auto_discover_models')),
        'models': json_list(row.get('models_json')),
        'model_mappings': json_dict(row.get('model_mappings_json')),
        'created_at': intv(row.get('created_at')),
        'updated_at': intv(row.get('updated_at')),
    }

def egress_profile_doc(row):
    if not row: return None
    return {
        'id': row.get('id') or '',
        'name': row.get('name') or row.get('id') or '',
        'type': row.get('type') or 'direct',
        'endpoint': row.get('endpoint') or '',
        'chain_proxy': row.get('chain_proxy') or '',
        'region': row.get('region') or '',
        'exit_ip': row.get('exit_ip') or '',
        'stream_capable': boolv(row.get('stream_capable')),
        'health': row.get('health') or 'healthy',
        'latency_millis': intv(row.get('latency_millis')),
        'cf_score': intv(row.get('cf_score')),
        'last_cf_ray': row.get('last_cf_ray') or '',
        'cooldown_until': intv(row.get('cooldown_until')),
        'max_concurrency': intv(row.get('max_concurrency')),
        'created_at': intv(row.get('created_at')),
        'updated_at': intv(row.get('updated_at')),
        'proxy_auth_mode': row.get('proxy_auth_mode') or '',
        'proxy_api_key': row.get('proxy_api_key') or '',
        'ip_mode': row.get('ip_mode') or '',
        'provider_key': row.get('provider_key') or '',
        'dynamic_config_json': row.get('dynamic_config_json') or '{}',
    }

con = sqlite3.connect(f"file:{db_path}?mode=ro", uri=True)
account_cols = columns(con, 'accounts')
if not account_cols: raise SystemExit('accounts table not found')
accounts = [dict(zip(account_cols, row)) for row in con.execute('SELECT * FROM accounts ORDER BY created_at, id')]
if not accounts: raise SystemExit('account pool is empty')
account_ids = [str(a.get('id') or '') for a in accounts]
tokens = fetch_by_account(con, 'account_auth_tokens', account_ids)
bindings = fetch_by_account(con, 'account_egress_bindings', account_ids)
kiro = fetch_by_account(con, 'account_kiro_credentials', account_ids)
antigravity = fetch_by_account(con, 'account_antigravity_credentials', account_ids)
sessions = fetch_by_account(con, 'account_session_cookies', account_ids)
cookies = fetch_by_account(con, 'account_injected_cookies', account_ids)
capabilities = fetch_by_account(con, 'account_model_capabilities', account_ids)
catalog = fetch_by_account(con, 'account_model_catalog_status', account_ids)
reauth = fetch_by_account(con, 'account_codex_reauth_config', account_ids)
memberships = fetch_by_account(con, 'account_group_memberships', account_ids)
provider_ids = sorted({str(a.get('provider') or '').strip() for a in accounts if is_custom_provider(a.get('provider'))})
custom_provider_rows = fetch_by_id(con, 'custom_providers', provider_ids)
missing_providers = [pid for pid in provider_ids if pid not in custom_provider_rows]
if missing_providers:
    raise SystemExit('direct export cannot build a portable backup because custom provider definitions are missing: ' + ','.join(missing_providers))
egress_ids = set()
for rows in bindings.values():
    for binding in rows:
        egress_ids.add(str(binding.get('primary_egress_id') or '').strip())
        egress_ids.update(split_ids(binding.get('standby_egress_ids')))
        egress_ids.add(str(binding.get('sidecar_egress_id') or '').strip())
for rows in cookies.values():
    for cookie in rows:
        egress_ids.add(str(cookie.get('egress_id') or '').strip())
for row in custom_provider_rows.values():
    egress_ids.update(json_list(row.get('egress_ids')))
egress_ids.discard('')
egress_rows = fetch_by_id(con, 'egress_profiles', sorted(egress_ids))
missing_egresses = sorted(eid for eid in egress_ids if eid not in egress_rows)
if missing_egresses:
    raise SystemExit('direct export cannot build a portable backup because egress definitions are missing: ' + ','.join(missing_egresses))
exported_at = int(time.time())
out_path.parent.mkdir(parents=True, exist_ok=True)
tmp = out_path.with_name(out_path.name + '.tmp')
used = set(); encrypted_seen = False
with zipfile.ZipFile(tmp, 'w', compression=zipfile.ZIP_DEFLATED) as zf:
    for index, acc in enumerate(accounts):
        aid = str(acc.get('id') or '')
        tok = first(tokens, aid) or {}
        token = {
            'auth_method': tok.get('auth_method') or '', 'credential_mode': tok.get('credential_mode') or '',
            'access_token': tok.get('access_token') or '', 'refresh_token': tok.get('refresh_token') or '',
            'openai_api_key': tok.get('openai_api_key') or '', 'id_token_raw': tok.get('id_token_raw') or '',
            'agent_runtime_id': tok.get('agent_runtime_id') or '', 'agent_private_key': tok.get('agent_private_key') or '',
            'agent_task_id': tok.get('agent_task_id') or '', 'last_refresh': intv(tok.get('last_refresh')),
            'expires_at': intv(tok.get('expires_at')), 'scopes': tok.get('scopes') or '',
            'oauth_rate_limit_tier': tok.get('oauth_rate_limit_tier') or '',
            'created_at': intv(tok.get('created_at')), 'updated_at': intv(tok.get('updated_at')),
        }
        if any(isinstance(v, str) and v.startswith('enc:v') for v in token.values()):
            encrypted_seen = True
        account = {
            'id': aid, 'label': acc.get('label') or aid, 'group_name': acc.get('group_name') or 'cyber',
            'upstream_account_id': acc.get('upstream_account_id') or '', 'chatgpt_user_id': acc.get('chatgpt_user_id') or '',
            'email': acc.get('email') or '', 'plan_type': acc.get('plan_type') or '', 'provider': acc.get('provider') or '',
            'status': acc.get('status') or 'active', 'is_fedramp': boolv(acc.get('is_fedramp')),
            'ignore_rate_limit_controls': boolv(acc.get('ignore_rate_limit_controls')),
            'quarantine_until': intv(acc.get('quarantine_until')), 'quarantine_reason': acc.get('quarantine_reason') or '',
            'created_at': intv(acc.get('created_at')), 'updated_at': intv(acc.get('updated_at')),
        }
        doc = {'type':'codex-account-pool-account','version':1,'exported_at':exported_at,'account':account,'token':token}
        for key, mapping in [('egress_binding', bindings), ('kiro_credentials', kiro), ('antigravity_credentials', antigravity), ('model_catalog_status', catalog), ('codex_reauth_config', reauth)]:
            item = first(mapping, aid)
            if item: doc[key] = item
        provider_id = str(account.get('provider') or '').strip()
        if is_custom_provider(provider_id):
            doc['custom_provider'] = custom_provider_doc(custom_provider_rows[provider_id])
        referenced_egress_ids = set()
        if doc.get('egress_binding'):
            binding = doc['egress_binding']
            referenced_egress_ids.add(str(binding.get('primary_egress_id') or '').strip())
            referenced_egress_ids.update(split_ids(binding.get('standby_egress_ids')))
            referenced_egress_ids.add(str(binding.get('sidecar_egress_id') or '').strip())
        item = first(sessions, aid)
        if item: doc['session_cookie'] = item.get('cookie') or ''
        if cookies.get(aid):
            doc['injected_cookies'] = cookies[aid]
            for cookie in cookies[aid]:
                referenced_egress_ids.add(str(cookie.get('egress_id') or '').strip())
        if doc.get('custom_provider'):
            referenced_egress_ids.update(doc['custom_provider'].get('egress_ids') or [])
        referenced_egress_ids.discard('')
        if referenced_egress_ids:
            doc['egress_profiles'] = [egress_profile_doc(egress_rows[eid]) for eid in sorted(referenced_egress_ids)]
        if capabilities.get(aid): doc['model_capabilities'] = capabilities[aid]
        if memberships.get(aid): doc['group_memberships'] = memberships[aid]
        name = safe_name(aid or account.get('label'), index)
        if name in used: name = name[:-5] + f'-{index+1}.json'
        used.add(name)
        zf.writestr(name, json.dumps(prune(doc), ensure_ascii=False, indent=2).encode('utf-8') + b'\n')
with zipfile.ZipFile(tmp, 'r') as zf:
    bad = zf.testzip()
    if bad: raise SystemExit(f'bad zip member: {bad}')
if expected and len(used) != expected:
    raise SystemExit(f'direct export wrote {len(used)} account files, expected {expected}')
tmp.replace(out_path)
if encrypted_seen:
    print('WARNING: direct SQLite fallback copied encrypted credential strings; use the admin API export on the matching running service for plaintext portable credentials.', file=sys.stderr)
print(f"OK: direct SQLite export wrote {len(used)} account JSON files to {out_path}", file=sys.stderr)
print(out_path)
PY
chmod 600 "$out_path" || true
