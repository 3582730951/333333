#!/usr/bin/env bash
set -euo pipefail

artifact_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="${ROOT:-$(cd "$artifact_dir/../.." && pwd)}"
baseline="${BASELINE_DIR:-$artifact_dir/source-baseline}"
files=(
  internal/api/body_meta.go
  internal/api/body_source_test.go
  internal/api/diagnostics_export.go
  internal/api/diagnostics_export_test.go
  internal/api/server.go
  internal/api/model_capability_errors.go
  internal/api/kiro_diagnostics_test.go
  internal/api/user_group_route_test.go
  scripts/analyze_goal_context_diagnostics.py
)

for file in "${files[@]}"; do
  test -f "$baseline/$file"
  mkdir -p "$root/$(dirname "$file")"
  cp -p "$baseline/$file" "$root/$file"
done

printf "restored diagnostics-fix baseline into %s\n" "$root"
for file in "${files[@]}"; do
  sha256sum "$root/$file"
done
