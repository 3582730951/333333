#!/usr/bin/env bash
set -euo pipefail

artifact_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(git -C "$artifact_dir" rev-parse --show-toplevel)"
patch="$artifact_dir/traffic-fallback.patch"

replace_console() {
  local archive=$1 manifest=$2
  rm -rf "$repo_root/internal/console/dist"
  mkdir -p "$repo_root/internal/console"
  tar -C "$repo_root/internal/console" -xzf "$archive"
  (
    cd "$repo_root"
    sha256sum -c "$manifest"
  )
}

case "${1:-rollback}" in
  rollback)
    git -C "$repo_root" apply --reverse --check "$patch"
    git -C "$repo_root" apply --reverse "$patch"
    replace_console \
      "$artifact_dir/traffic-fallback-baseline-console-dist.tar.gz" \
      "$artifact_dir/records/baseline-console-dist.sha256"
    (
      cd "$repo_root"
      sha256sum -c "$artifact_dir/records/baseline-source.sha256"
    )
    printf 'ROLLBACK_OK=1\nBEHAVIOR=traffic fallback source and embedded console restored to baseline\n'
    ;;
  restore)
    git -C "$repo_root" apply --check "$patch"
    git -C "$repo_root" apply "$patch"
    replace_console \
      "$artifact_dir/traffic-fallback-modified-console-dist.tar.gz" \
      "$artifact_dir/records/modified-console-dist.sha256"
    (
      cd "$repo_root"
      sha256sum -c "$artifact_dir/records/modified-source.sha256"
    )
    printf 'RESTORE_OK=1\nBEHAVIOR=traffic fallback source and embedded console restored to modified state\n'
    ;;
  *)
    printf 'usage: %s [rollback|restore]\n' "$0" >&2
    exit 2
    ;;
esac
