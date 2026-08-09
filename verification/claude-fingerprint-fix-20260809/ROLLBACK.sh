#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
repo_dir=${1:-}
if [[ -z "$repo_dir" ]]; then
  repo_dir=$(git -C "$script_dir" rev-parse --show-toplevel 2>/dev/null || true)
fi
repo_dir=${repo_dir%/}
if [[ -z "$repo_dir" || "$repo_dir" == "/" || ! -f "$repo_dir/go.mod" ]]; then
  echo "rollback: pass the codex-account-pool repository root as the first argument" >&2
  exit 2
fi

tracked_files=(
  README.md
  config.example.json
  internal/api/account_probe.go
  internal/api/admin_config.go
  internal/api/admin_fingerprint_check.go
  internal/api/auto_continue.go
  internal/api/chat_claude.go
  internal/api/chat_custom.go
  internal/api/claude_cache_diagnostics.go
  internal/api/claude_cache_diagnostics_gate_test.go
  internal/api/claude_cache_prewarm.go
  internal/api/claude_cache_prewarm_shape_test.go
  internal/api/claude_cache_singleflight.go
  internal/api/claude_code_docker_e2e_test.go
  internal/api/claude_gateway_docker_e2e_test.go
  internal/api/claude_oauth_egress_test.go
  internal/api/claude_refresh.go
  internal/api/config_fields.go
  internal/api/config_hotreload_test.go
  internal/api/custom_protocol_matrix_test.go
  internal/api/custom_provider_admin_probe.go
  internal/api/custom_provider_routing_test.go
  internal/api/httputil.go
  internal/api/messages.go
  internal/api/model_quality.go
  internal/api/model_quality_test.go
  internal/api/moderation.go
  internal/api/oauth.go
  internal/api/oauth_test.go
  internal/api/override.go
  internal/api/provider_api_key.go
  internal/api/provider_api_key_test.go
  internal/api/settings_center.go
  internal/cloak/billing_test.go
  internal/cloak/cloak.go
  internal/config/config.go
  internal/config/config_test.go
  internal/config/fingerprint_warnings_test.go
  internal/identity/fleet_test.go
  internal/identity/identity.go
  internal/prompt/anthropic.go
  internal/prompt/prompt.go
  internal/upstream/anthropic.go
  internal/upstream/anthropic_test.go
  internal/upstream/claude_egress_coverage_test.go
  internal/upstream/claude_relay_leak_test.go
  internal/upstream/claude_sidecar_shape_test.go
  internal/upstream/claude_sidecar_test.go
  internal/upstream/claude_stdlib_alpn_test.go
  internal/upstream/client.go
  internal/upstream/custom_claude_wire_shape_test.go
  internal/upstream/egress_client.go
  internal/upstream/inprocess.go
  internal/upstream/openai_compat.go
  internal/upstream/openai_compat_claude.go
  internal/upstream/openai_compat_claude_test.go
  internal/upstream/tlsclient/tlsclient.go
  internal/upstream/tlsclient/tlsclient_test.go
  sidecar/curl_cffi_sidecar.py
  sidecar/test_async_sidecar.py
)

new_files=(
  internal/anthropicwire/json_order.go
  internal/anthropicwire/json_order_test.go
  internal/api/admin_fingerprint_check_test.go
  internal/api/claude_probe_wire.go
  internal/api/claude_probe_wire_test.go
)

original_root="$script_dir/original"
originals_ready=true
for path in "${tracked_files[@]}"; do
  if [[ ! -f "$original_root/$path" ]]; then
    originals_ready=false
    break
  fi
done

if [[ "$originals_ready" == true ]]; then
  for path in "${tracked_files[@]}"; do
    mkdir -p -- "$repo_dir/$(dirname -- "$path")"
    cp -p -- "$original_root/$path" "$repo_dir/$path"
  done
  for path in "${new_files[@]}"; do
    rm -f -- "$repo_dir/$path"
  done
else
  patch_file="$script_dir/DIFF_FILE.patch"
  if [[ ! -f "$patch_file" ]]; then
    echo "rollback: original snapshots and DIFF_FILE.patch are unavailable" >&2
    exit 3
  fi
  git -C "$repo_dir" apply --reverse --check "$patch_file"
  git -C "$repo_dir" apply --reverse "$patch_file"
fi

for path in "${tracked_files[@]}"; do
  if [[ "$originals_ready" == true ]] && ! cmp -s -- "$original_root/$path" "$repo_dir/$path"; then
    echo "rollback: verification failed for $path" >&2
    exit 4
  fi
done
for path in "${new_files[@]}"; do
  if [[ -e "$repo_dir/$path" ]]; then
    echo "rollback: verification failed; new file remains: $path" >&2
    exit 5
  fi
done

echo "rollback: restored 60 tracked files and removed 5 added files"
