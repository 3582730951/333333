#!/usr/bin/env bash
set -Eeuo pipefail

umask 077

script_version="2"
base_url="${CODEX_POOL_BASE_URL:-http://127.0.0.1:8787}"
admin_token="${CODEX_POOL_ADMIN_TOKEN:-}"
database_path="${CODEX_POOL_DATABASE:-}"
output_file=""
app_zip=""
journal_since="6 hours ago"
app_timeout=300
collect_app=1

usage() {
  cat <<'USAGE'
Collect a redacted Codex Pool VPS support bundle.

Usage:
  collect-vps-diagnostics.sh [options]

Options:
  --base-url URL       Public/local pool URL (default: http://127.0.0.1:8787)
  --admin-token TOKEN  Admin token. Prefer CODEX_POOL_ADMIN_TOKEN or an interactive read.
  --database FILE      SQLite database path (default: auto-detect)
  --app-zip FILE       Include an already completed application diagnostics ZIP
  --output FILE        Destination .tar.gz (default: timestamped path in cwd)
  --journal-since TEXT journalctl --since value (default: "6 hours ago")
  --app-timeout SEC    Maximum application diagnostics wait (default: 300)
  --no-app             Do not request/include the application diagnostics ZIP
  -h, --help           Show help

The bundle never copies the database, key files, config contents, credentials,
cookies, request bodies, or process command lines. Text is redacted and DLP-scanned.

Example:
  read -rsp "Admin token: " CODEX_POOL_ADMIN_TOKEN; export CODEX_POOL_ADMIN_TOKEN; echo
  ./collect-vps-diagnostics.sh --output /root/codex-pool-vps-diagnostics.tar.gz
  unset CODEX_POOL_ADMIN_TOKEN
USAGE
}

die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"
}

while (($# > 0)); do
  case "$1" in
    --base-url)
      (($# >= 2)) || die "--base-url requires a value"
      base_url="$2"
      shift 2
      ;;
    --admin-token)
      (($# >= 2)) || die "--admin-token requires a value"
      admin_token="$2"
      shift 2
      ;;
    --database)
      (($# >= 2)) || die "--database requires a value"
      database_path="$2"
      shift 2
      ;;
    --app-zip)
      (($# >= 2)) || die "--app-zip requires a value"
      app_zip="$2"
      shift 2
      ;;
    --output)
      (($# >= 2)) || die "--output requires a value"
      output_file="$2"
      shift 2
      ;;
    --journal-since)
      (($# >= 2)) || die "--journal-since requires a value"
      journal_since="$2"
      shift 2
      ;;
    --app-timeout)
      (($# >= 2)) || die "--app-timeout requires a value"
      app_timeout="$2"
      shift 2
      ;;
    --no-app)
      collect_app=0
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      die "unknown option: $1"
      ;;
  esac
done

require_command bash
require_command curl
require_command find
require_command python3
require_command sha256sum
require_command tar

[[ "$app_timeout" =~ ^[1-9][0-9]*$ ]] || die "--app-timeout must be a positive integer"
[[ ${#journal_since} -le 80 && "$journal_since" != *$'\n'* && "$journal_since" != *$'\r'* ]] ||
  die "--journal-since is invalid"
[[ "$admin_token" != *$'\n'* && "$admin_token" != *$'\r'* && "$admin_token" != *'"'* ]] ||
  die "admin token contains unsupported characters"

base_url="${base_url%/}"
case "$base_url" in
  http://*|https://*) ;;
  *) die "--base-url must use http:// or https://" ;;
esac

timestamp="$(date -u +%Y%m%dT%H%M%SZ)"
if [[ -z "$output_file" ]]; then
  output_file="$PWD/codex-pool-vps-diagnostics-$timestamp.tar.gz"
elif [[ "$output_file" != /* ]]; then
  output_file="$PWD/$output_file"
fi
output_parent="$(dirname "$output_file")"
mkdir -p "$output_parent"
output_parent="$(cd "$output_parent" && pwd -P)"
output_file="$output_parent/$(basename "$output_file")"
[[ ! -e "$output_file" ]] || die "destination already exists: $output_file"

work_root="$(mktemp -d "${TMPDIR:-/tmp}/codex-pool-vps-diagnostics.XXXXXX")"
bundle_root="$work_root/codex-pool-vps-diagnostics"
mkdir -p "$bundle_root/system" "$bundle_root/service" "$bundle_root/storage" "$bundle_root/application"

cleanup() {
  if [[ -n "${work_root:-}" && -d "$work_root" && "$work_root" == "${TMPDIR:-/tmp}"/codex-pool-vps-diagnostics.* ]]; then
    find "$work_root" -depth -delete 2>/dev/null || true
  fi
}
trap cleanup EXIT INT TERM

export VPS_DIAG_REDACT_SECRET="$admin_token"
export VPS_DIAG_REDACT_HOSTNAME="$(hostname 2>/dev/null || true)"

redact_stream() {
  python3 -c '
import ipaddress, os, re, sys
text = sys.stdin.read()
secret = os.environ.get("VPS_DIAG_REDACT_SECRET", "")
hostname = os.environ.get("VPS_DIAG_REDACT_HOSTNAME", "")
if secret:
    text = text.replace(secret, "<REDACTED_ADMIN_TOKEN>")
if hostname:
    text = re.sub(re.escape(hostname), "<HOST>", text, flags=re.I)
patterns = [
    (r"-----BEGIN [^-]{0,40}PRIVATE KEY-----.*?-----END [^-]{0,40}PRIVATE KEY-----", "<REDACTED_PRIVATE_KEY>"),
    (r"(?i)\bAuthorization\s*[:=]\s*(?:Bearer\s+)?[^\s,\";]+", "Authorization: <REDACTED>"),
    (r"(?i)\bBearer\s+[A-Za-z0-9._~+/=-]{8,}", "Bearer <REDACTED>"),
    (r"\bsk-(?:ant-)?[A-Za-z0-9_-]{10,}", "<REDACTED_API_TOKEN>"),
    (r"\b[A-Za-z0-9_-]{16,}\.[A-Za-z0-9_-]{16,}\.[A-Za-z0-9_-]{8,}\b", "<REDACTED_JWT>"),
    (r"(?i)\b([A-Z0-9_]*(?:TOKEN|SECRET|PASSWORD|PRIVATE_KEY|API_KEY|DSN))\s*=\s*[^\s]+", r"\1=<REDACTED>"),
    (r"(?i)\b[A-Z0-9._%+-]+@[A-Z0-9.-]+\.[A-Z]{2,}\b", "<EMAIL>"),
    (r"(?<![A-Za-z0-9])(?:\d{1,3}\.){3}\d{1,3}(?![A-Za-z0-9])", "<IPV4>"),
    (r"\bacc_[A-Za-z0-9_-]{8,}\b", "ACC-REDACTED"),
    (r"(?i)([?&](?:token|key|secret|code|signature|sig)=)[^&\s]+", r"\1<REDACTED>"),
    (r"(?<![A-Za-z0-9])[A-Fa-f0-9]{48,}(?![A-Za-z0-9])", "<REDACTED_HIGH_ENTROPY>"),
    (r"/home/[^/\s]+", "/home/<USER>"),
    (r"/root(?=/|\s|$)", "/<ROOT>"),
]
for pattern, replacement in patterns:
    if replacement == "<EMAIL>":
        def redact_email(match):
            value = match.group(0)
            # A systemd template instance such as worker@release.service is a unit,
            # not an email address; retaining it is essential for A/B diagnostics.
            if value.lower().endswith((".service", ".socket", ".target", ".timer")):
                return value
            return replacement
        text = re.sub(pattern, redact_email, text)
    else:
        text = re.sub(pattern, replacement, text, flags=re.S if "PRIVATE KEY" in pattern else 0)

# Validate IPv6 candidates instead of treating timestamps (HH:MM:SS) as addresses.
def redact_ipv6(match):
    value = match.group(0)
    try:
        return "<IPV6>" if ipaddress.ip_address(value).version == 6 else value
    except ValueError:
        return value
text = re.sub(r"(?<![0-9A-Fa-f:])[0-9A-Fa-f:]{2,}(?![0-9A-Fa-f:])", redact_ipv6, text)
sys.stdout.write(text)
'
}

capture() {
  local relative="$1"
  shift
  mkdir -p "$(dirname "$bundle_root/$relative")"
  (
    set +e
    printf 'command:'
    printf ' %q' "$@"
    printf '\n\n'
    "$@"
    status=$?
    printf '\nexit_status: %d\n' "$status"
    exit 0
  ) 2>&1 | redact_stream >"$bundle_root/$relative"
}

capture_shell() {
  local relative="$1"
  local script="$2"
  capture "$relative" bash -lc "$script"
}

curl_config=()
if [[ -n "$admin_token" ]]; then
  curl_auth_file="$work_root/curl-auth.conf"
  printf 'header = "Authorization: Bearer %s"\n' "$admin_token" >"$curl_auth_file"
  chmod 0600 "$curl_auth_file"
  curl_config=(--config "$curl_auth_file")
fi

capture "system/uname.txt" uname -a
capture "system/os-release.txt" cat /etc/os-release
capture "system/uptime.txt" uptime
capture "system/memory.txt" free -h
capture "system/disk-filesystems.txt" df -hPT
capture "system/disk-inodes.txt" df -hiPT
if command -v findmnt >/dev/null 2>&1; then
  capture "system/mounts.txt" findmnt -rn -o TARGET,FSTYPE
fi
if command -v ss >/dev/null 2>&1; then
  capture "system/listening-sockets.txt" ss -lntup
fi
capture "system/processes.txt" ps -eo pid,ppid,user,stat,etimes,pcpu,pmem,rss,comm
capture_shell "system/limits.txt" 'ulimit -a'

if command -v systemctl >/dev/null 2>&1; then
  capture "service/units.txt" systemctl list-units --type=service --all --no-pager 'codex-pool*'
  capture "service/unit-files.txt" systemctl list-unit-files --no-pager 'codex-pool*'
  capture "service/status.txt" systemctl status --no-pager --full 'codex-pool*'
  capture "service/properties.txt" systemctl show 'codex-pool*' \
    --property=Id,LoadState,ActiveState,SubState,MainPID,ExecMainStatus,FragmentPath,User,Group,WorkingDirectory,ExecStart,Restart,MemoryCurrent,CPUUsageNSec
  capture "service/unit-definitions.txt" systemctl cat 'codex-pool*'
fi

if command -v journalctl >/dev/null 2>&1; then
  capture "service/handoff-journal.txt" journalctl -u codex-pool-handoff.service \
    --since "$journal_since" --no-pager -o short-iso -n 20000
  capture "service/worker-journal.txt" journalctl -u 'codex-pool-worker@*' \
    --since "$journal_since" --no-pager -o short-iso -n 40000
  capture "service/sidecar-journal.txt" journalctl -u codex-pool-sidecar.service \
    --since "$journal_since" --no-pager -o short-iso -n 10000
fi

capture "service/livez.txt" curl -sS -D - --max-time 10 "$base_url/livez"
capture "service/readyz.txt" curl -sS -D - --max-time 10 "$base_url/readyz"
capture "service/standbyz.txt" curl -sS -D - --max-time 10 "$base_url/standbyz"
if ((${#curl_config[@]} > 0)); then
  capture "application/diagnostic-jobs.json" curl -sS --max-time 15 \
    "${curl_config[@]}" "$base_url/admin/diagnostics/jobs"
fi

if command -v docker >/dev/null 2>&1; then
  capture "service/docker-containers.txt" docker ps -a --no-trunc \
    --format 'table {{.ID}}\t{{.Image}}\t{{.Status}}\t{{.Ports}}\t{{.Names}}'
  capture "service/docker-images.txt" docker image ls --no-trunc \
    --format 'table {{.ID}}\t{{.Repository}}:{{.Tag}}\t{{.CreatedSince}}\t{{.Size}}'
fi

for path in /var/lib/codex-pool /var/lib/codex-pool/data /var/lib/codex-pool/run /etc/codex-pool /usr/local/lib/codex-pool; do
  if [[ -e "$path" ]]; then
    capture "storage/stat-$(printf '%s' "$path" | tr '/' '_').txt" stat -c '%n %F %a %U:%G %s %y' "$path"
  fi
done
if [[ -d /var/lib/codex-pool ]]; then
  capture "storage/usage.txt" du -x -h -d 2 /var/lib/codex-pool
  capture "storage/layout.txt" find /var/lib/codex-pool -xdev -maxdepth 3 \
    -printf '%y %m %u:%g %s %p -> %l\n'
fi
if [[ -e /var/lib/codex-pool/run/active-worker.sock ]]; then
  capture "service/active-worker-link.txt" readlink -f /var/lib/codex-pool/run/active-worker.sock
fi

if [[ -z "$database_path" ]]; then
  for candidate in /var/lib/codex-pool/pool.sqlite3 /var/lib/codex-pool/codex-pool.sqlite3; do
    if [[ -f "$candidate" ]]; then
      database_path="$candidate"
      break
    fi
  done
fi

if [[ -n "$database_path" && -f "$database_path" ]]; then
  python3 - "$database_path" >"$bundle_root/storage/database-metadata.json.raw" 2>&1 <<'PY'
import json
import sqlite3
import sys
from pathlib import Path

path = Path(sys.argv[1]).resolve()
result = {
    "database_file": path.name,
    "database_size": path.stat().st_size,
    "queries": {},
}
try:
    connection = sqlite3.connect(f"file:{path}?mode=ro", uri=True, timeout=5)
    connection.row_factory = sqlite3.Row
    tables = {
        row[0] for row in connection.execute(
            "SELECT name FROM sqlite_master WHERE type='table'"
        )
    }
    result["quick_check"] = [
        row[0] for row in connection.execute("PRAGMA quick_check")
    ]
    for pragma in ("journal_mode", "auto_vacuum", "page_count", "freelist_count"):
        result[pragma] = connection.execute(f"PRAGMA {pragma}").fetchone()[0]

    queries = {
        "accounts_by_provider_status": (
            "accounts",
            """SELECT COALESCE(NULLIF(TRIM(provider),''),'legacy_inferred') AS provider,
                      status, COUNT(*) AS count
                 FROM accounts GROUP BY 1,2 ORDER BY 1,2""",
        ),
        "model_capability_summary": (
            "account_model_capabilities",
            """SELECT COALESCE(NULLIF(TRIM(a.provider),''),'legacy_inferred') AS provider,
                      m.model_slug, m.availability_state, COUNT(*) AS count
                 FROM account_model_capabilities m JOIN accounts a ON a.id=m.account_id
                GROUP BY 1,2,3 ORDER BY 1,2,3""",
        ),
        "targeted_accounts_by_provider_status": (
            "user_group_targets",
            """SELECT t.target_type,
                      COALESCE(NULLIF(TRIM(a.provider),''),'legacy_inferred') AS provider,
                      COALESCE(a.status,'no_matching_account') AS status,
                      COUNT(DISTINCT a.id) AS account_count
                 FROM user_group_targets t
                 LEFT JOIN account_group_memberships m
                   ON t.target_type='base_group' AND m.group_name=t.target_ref
                 LEFT JOIN accounts a ON a.id=m.account_id
                GROUP BY 1,2,3 ORDER BY 1,2,3""",
        ),
        "diagnostic_jobs": (
            "diagnostic_jobs",
            """SELECT status, format_version, COALESCE(error_code,'') AS error_code,
                      COUNT(*) AS count, MIN(created_at) AS oldest, MAX(updated_at) AS newest
                 FROM diagnostic_jobs GROUP BY 1,2,3 ORDER BY 1,2,3""",
        ),
        "storage_resources": (
            "storage_resources",
            """SELECT resource_type, state, retention_class, COUNT(*) AS count,
                      COALESCE(SUM(size_bytes),0) AS total_bytes
                 FROM storage_resources GROUP BY 1,2,3 ORDER BY 1,2,3""",
        ),
        "maintenance_leases": (
            "maintenance_leases",
            """SELECT lease_name, COUNT(*) AS count, MAX(fencing_token) AS max_fencing_token,
                      MAX(expires_at) AS latest_expiry
                 FROM maintenance_leases GROUP BY 1 ORDER BY 1""",
        ),
        "user_group_target_types": (
            "user_group_targets",
            """SELECT target_type, COUNT(*) AS count
                 FROM user_group_targets GROUP BY 1 ORDER BY 1""",
        ),
    }
    for name, (required_table, query) in queries.items():
        if required_table in tables:
            result["queries"][name] = [dict(row) for row in connection.execute(query)]
    result["table_row_counts"] = {}
    for table in sorted(tables):
        if table.startswith("sqlite_"):
            continue
        try:
            result["table_row_counts"][table] = connection.execute(
                f'SELECT COUNT(*) FROM "{table}"'
            ).fetchone()[0]
        except sqlite3.Error:
            result["table_row_counts"][table] = "unavailable"
    connection.close()
except Exception as error:
    result["error"] = f"{type(error).__name__}: {error}"
print(json.dumps(result, ensure_ascii=False, indent=2, sort_keys=True))
PY
  redact_stream <"$bundle_root/storage/database-metadata.json.raw" \
    >"$bundle_root/storage/database-metadata.json"
  find "$bundle_root/storage/database-metadata.json.raw" -maxdepth 0 -type f -delete
else
  printf '{"status":"database_not_found_or_not_sqlite"}\n' >"$bundle_root/storage/database-metadata.json"
fi

included_app_zip=0
if ((collect_app)); then
  if [[ -z "$app_zip" ]]; then
    newest_app_zip=""
    for candidate in /var/lib/codex-pool/data/diagnostics/diagjob_*.zip; do
      [[ -f "$candidate" ]] || continue
      if [[ -z "$newest_app_zip" || "$candidate" -nt "$newest_app_zip" ]]; then
        newest_app_zip="$candidate"
      fi
    done
    if [[ -n "$newest_app_zip" ]]; then
      app_zip="$newest_app_zip"
    fi
  fi
  if [[ -n "$app_zip" && -f "$app_zip" ]] &&
    [[ "$(od -An -tx1 -N2 "$app_zip" 2>/dev/null | tr -d '[:space:]')" == "504b" ]]; then
    cp "$app_zip" "$bundle_root/application/codex-pool-diagnostics.zip"
    included_app_zip=1
  else
    script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
    fetch_script="$script_dir/fetch-diagnostics.sh"
    if [[ -f "$fetch_script" ]]; then
      (
        set +e
        CODEX_POOL_BASE_URL="$base_url" CODEX_POOL_ADMIN_TOKEN="$admin_token" \
          bash "$fetch_script" \
            --output "$bundle_root/application/codex-pool-diagnostics.zip" \
            --timeout "$app_timeout" --no-extract
        status=$?
        printf '\nexit_status: %d\n' "$status"
        exit 0
      ) 2>&1 | redact_stream >"$bundle_root/application/fetch.log"
      if [[ -f "$bundle_root/application/codex-pool-diagnostics.zip" ]] &&
        [[ "$(od -An -tx1 -N2 "$bundle_root/application/codex-pool-diagnostics.zip" | tr -d '[:space:]')" == "504b" ]]; then
        included_app_zip=1
      fi
    else
      printf 'fetch-diagnostics.sh was not found next to this collector.\n' \
        >"$bundle_root/application/fetch.log"
    fi
  fi
fi

python3 - "$bundle_root" "$script_version" "$timestamp" "$included_app_zip" <<'PY'
import json
import platform
import sys
from pathlib import Path

root, version, created, included = sys.argv[1:]
manifest = {
    "format": "codex-pool-vps-diagnostics",
    "format_version": version,
    "created_at_utc": created,
    "application_bundle_included": included == "1",
    "python_version": platform.python_version(),
    "collection_policy": {
        "database_copied": False,
        "config_contents_copied": False,
        "key_files_copied": False,
        "process_arguments_copied": False,
        "text_redacted": True,
        "dlp_scanned": True,
    },
}
Path(root, "manifest.json").write_text(
    json.dumps(manifest, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
    encoding="utf-8",
)
PY

(
  cd "$bundle_root"
  find . -type f ! -name checksums.sha256 -print0 |
    sort -z |
    xargs -0 sha256sum >checksums.sha256
)

python3 - "$bundle_root" <<'PY'
import re
import sys
from pathlib import Path

root = Path(sys.argv[1])
patterns = {
    "private_key": re.compile(rb"-----BEGIN [^-]{0,40}PRIVATE KEY-----", re.I),
    "bearer": re.compile(rb"\bBearer\s+[A-Za-z0-9._~+/=-]{8,}", re.I),
    "api_token": re.compile(rb"\bsk-(?:ant-)?[A-Za-z0-9_-]{10,}"),
    "jwt": re.compile(rb"\b[A-Za-z0-9_-]{16,}\.[A-Za-z0-9_-]{16,}\.[A-Za-z0-9_-]{8,}\b"),
}
email_pattern = re.compile(rb"\b[A-Z0-9._%+-]+@[A-Z0-9.-]+\.[A-Z]{2,}\b", re.I)
hits = []
for path in root.rglob("*"):
    if not path.is_file() or path.suffix.lower() in {".zip", ".gz", ".png", ".jpg", ".jpeg"}:
        continue
    data = path.read_bytes()
    for name, pattern in patterns.items():
        if pattern.search(data):
            hits.append(f"{path.relative_to(root)}:{name}")
    for match in email_pattern.finditer(data):
        if not match.group(0).lower().endswith(
            (b".service", b".socket", b".target", b".timer")
        ):
            hits.append(f"{path.relative_to(root)}:email")
            break
if hits:
    print("DLP validation failed: " + ", ".join(hits), file=sys.stderr)
    raise SystemExit(1)
PY

partial_file="$(mktemp "$output_parent/.codex-pool-vps-diagnostics.XXXXXX.partial")"
tar -czf "$partial_file" -C "$work_root" "$(basename "$bundle_root")"
tar -tzf "$partial_file" >/dev/null
mv "$partial_file" "$output_file"
chmod 0600 "$output_file"

printf 'VPS diagnostics: %s\n' "$output_file"
if ((included_app_zip)); then
  printf 'application diagnostics: included\n'
else
  printf 'application diagnostics: unavailable; system and service evidence is still included\n'
fi
