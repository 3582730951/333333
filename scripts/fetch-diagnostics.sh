#!/usr/bin/env bash
set -euo pipefail

umask 077

usage() {
  cat <<'USAGE'
Download and extract a codex-pool diagnostics v3 bundle.

Usage:
  fetch-diagnostics.sh [options]

Options:
  --base-url URL       Pool server URL (default: http://127.0.0.1:8787)
  --admin-token TOKEN  Legacy admin Bearer token. Omit for open-admin installs.
  --output FILE        Destination ZIP path (default: timestamped path in cwd)
  --extract-dir DIR    Extraction directory (default: ZIP path without .zip)
  --timeout SECONDS    Maximum wait for the async job (default: 1800)
  --poll SECONDS       Poll interval (default: 2)
  --no-extract         Download and validate without extracting
  -h, --help           Show this help

Environment equivalents:
  CODEX_POOL_BASE_URL
  CODEX_POOL_ADMIN_TOKEN

Example:
  ./fetch-diagnostics.sh \
    --base-url https://pool.example.com \
    --admin-token "$ADMIN_TOKEN"
USAGE
}

die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"
}

json_job_id() {
  python3 -c '
import json, sys
data = json.load(sys.stdin)
value = data.get("job", {}).get("id") or data.get("id")
if not isinstance(value, str) or not value:
    raise SystemExit("diagnostic create response did not contain a job id")
print(value)
'
}

json_job_status() {
  python3 -c '
import json, sys
data = json.load(sys.stdin)
value = data.get("status") or data.get("job", {}).get("status")
if not isinstance(value, str) or not value:
    raise SystemExit("diagnostic status response did not contain a status")
print(value)
'
}

json_download_url() {
  python3 -c '
import json, sys
data = json.load(sys.stdin)
value = data.get("download_url") or data.get("download") or ""
print(value if isinstance(value, str) else "")
'
}

json_error_code() {
  python3 -c '
import json, sys
data = json.load(sys.stdin)
value = data.get("error_code") or data.get("error", {}).get("code") or "unknown"
print(value if isinstance(value, str) else "unknown")
'
}

base_url="${CODEX_POOL_BASE_URL:-http://127.0.0.1:8787}"
admin_token="${CODEX_POOL_ADMIN_TOKEN:-}"
output_file=""
extract_dir=""
timeout_seconds=1800
poll_seconds=2
extract_bundle=1

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
    --output)
      (($# >= 2)) || die "--output requires a value"
      output_file="$2"
      shift 2
      ;;
    --extract-dir)
      (($# >= 2)) || die "--extract-dir requires a value"
      extract_dir="$2"
      shift 2
      ;;
    --timeout)
      (($# >= 2)) || die "--timeout requires a value"
      timeout_seconds="$2"
      shift 2
      ;;
    --poll)
      (($# >= 2)) || die "--poll requires a value"
      poll_seconds="$2"
      shift 2
      ;;
    --no-extract)
      extract_bundle=0
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

require_command curl
require_command python3
require_command od
require_command mv
if ((extract_bundle)); then
  require_command unzip
fi

[[ "$timeout_seconds" =~ ^[1-9][0-9]*$ ]] || die "--timeout must be a positive integer"
[[ "$poll_seconds" =~ ^[1-9][0-9]*$ ]] || die "--poll must be a positive integer"

base_url="${base_url%/}"
case "$base_url" in
  http://*|https://*) ;;
  *) die "--base-url must begin with http:// or https://" ;;
esac

if [[ -z "$output_file" ]]; then
  output_file="$PWD/codex-pool-diagnostics-$(date +%Y%m%d-%H%M%S).zip"
fi
if [[ "$output_file" != *.zip ]]; then
  output_file="${output_file}.zip"
fi
if [[ -z "$extract_dir" ]]; then
  extract_dir="${output_file%.zip}"
fi

output_parent="$(dirname "$output_file")"
mkdir -p "$output_parent"
output_parent="$(cd "$output_parent" && pwd -P)"
output_file="$output_parent/$(basename "$output_file")"
partial_file="$(mktemp "$output_parent/.codex-pool-diagnostics.XXXXXX.partial")"
auth_config_file=""

cleanup() {
  if [[ -n "${partial_file:-}" && -f "$partial_file" ]]; then
    find "$partial_file" -maxdepth 0 -type f -delete
  fi
  if [[ -n "${auth_config_file:-}" && -f "$auth_config_file" ]]; then
    find "$auth_config_file" -maxdepth 0 -type f -delete
  fi
}
trap cleanup EXIT INT TERM

[[ "$admin_token" != *$'\n'* && "$admin_token" != *$'\r'* && "$admin_token" != *'"'* ]] ||
  die "admin token contains unsupported characters"

auth_args=()
if [[ -n "$admin_token" ]]; then
  # Keep credentials out of ps/proc command lines. curl reads this mode-0600 file,
  # and the EXIT trap removes it on success, error, or interruption.
  auth_config_file="$(mktemp "$output_parent/.codex-pool-curl-auth.XXXXXX.conf")"
  printf 'header = "Authorization: Bearer %s"\n' "$admin_token" >"$auth_config_file"
  chmod 0600 "$auth_config_file"
  auth_args=(--config "$auth_config_file")
fi

created="$(
  curl -fsS -X POST \
    "${auth_args[@]}" \
    -H 'Content-Type: application/json' \
    "$base_url/admin/diagnostics/jobs" \
    -d '{}'
)" || die "failed to create diagnostic job"

job_id="$(printf '%s' "$created" | json_job_id)" || die "invalid diagnostic create response"
printf 'diagnostic job: %s\n' "$job_id"

deadline=$(( $(date +%s) + timeout_seconds ))
job=""
queued_since=0
queued_warning_printed=0
while (( $(date +%s) < deadline )); do
  job="$(
    curl -fsS \
      "${auth_args[@]}" \
      "$base_url/admin/diagnostics/jobs/$job_id"
  )" || die "failed to query diagnostic job $job_id"

  status="$(printf '%s' "$job" | json_job_status)" || die "invalid diagnostic status response"
  printf 'status: %s\n' "$status"

  case "$status" in
    ready)
      break
      ;;
    failed|cancelled|expired)
      error_code="$(printf '%s' "$job" | json_error_code 2>/dev/null || printf unknown)"
      die "diagnostic job ended with status=$status error_code=$error_code"
      ;;
    queued)
      now="$(date +%s)"
      if ((queued_since == 0)); then
        queued_since="$now"
      elif ((queued_warning_printed == 0 && now - queued_since >= 60)); then
        printf '%s\n' \
          'warning: job has remained queued for 60 seconds; the active diagnostic worker may not be running.' \
          'check /readyz and active worker logs while this script continues waiting.' >&2
        queued_warning_printed=1
      fi
      sleep "$poll_seconds"
      ;;
    snapshotting|rendering|validating|running|pending)
      queued_since=0
      sleep "$poll_seconds"
      ;;
    *)
      die "diagnostic job returned unknown status: $status"
      ;;
  esac
done

[[ "${status:-}" == "ready" ]] || die "timed out waiting for diagnostic job $job_id"

download_url="$(printf '%s' "$job" | json_download_url)" || die "invalid diagnostic download response"
if [[ -z "$download_url" ]]; then
  download_url="/admin/diagnostics/jobs/$job_id/download"
fi
case "$download_url" in
  http://*|https://*) artifact_url="$download_url" ;;
  /*) artifact_url="$base_url$download_url" ;;
  *) artifact_url="$base_url/$download_url" ;;
esac

curl -fsS -L \
  "${auth_args[@]}" \
  "$artifact_url" \
  -o "$partial_file" || die "failed to download diagnostic artifact"

magic="$(od -An -tx1 -N2 "$partial_file" | tr -d '[:space:]')"
[[ "$magic" == "504b" ]] || die "downloaded response is not a ZIP archive (magic=$magic)"

if command -v unzip >/dev/null 2>&1; then
  unzip -tq "$partial_file" >/dev/null || die "downloaded ZIP failed integrity validation"
fi

[[ ! -e "$output_file" ]] || die "destination already exists: $output_file"
mv "$partial_file" "$output_file"
partial_file=""
chmod 0600 "$output_file"
printf 'ZIP: %s\n' "$output_file"

if ((extract_bundle)); then
  [[ ! -e "$extract_dir" ]] || die "extract destination already exists: $extract_dir"
  mkdir -m 0700 -p "$extract_dir"
  unzip -q "$output_file" -d "$extract_dir"
  find "$extract_dir" -type d -exec chmod 0700 {} +
  find "$extract_dir" -type f -exec chmod 0600 {} +
  printf 'extracted: %s\n' "$extract_dir"
fi

if [[ -n "$auth_config_file" && -f "$auth_config_file" ]]; then
  find "$auth_config_file" -maxdepth 0 -type f -delete
  auth_config_file=""
fi
trap - EXIT INT TERM
