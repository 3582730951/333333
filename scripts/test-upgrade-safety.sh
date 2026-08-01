#!/usr/bin/env bash
set -Eeuo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

test_install_dispatch() {
  local fixture="${TMP}/dispatch"
  mkdir -p "$fixture/scripts" "$fixture/etc" "$fixture/data" "$fixture/app" "$fixture/systemd"
  cp "$ROOT/install.sh" "$fixture/install.sh"
  cat >"$fixture/scripts/install.sh" <<'EOF'
#!/usr/bin/env bash
printf 'install:%s\n' "$*" >>"${DISPATCH_LOG:?}"
EOF
  cat >"$fixture/update.sh" <<'EOF'
#!/usr/bin/env bash
printf 'update:%s\n' "$*" >>"${DISPATCH_LOG:?}"
EOF
  chmod +x "$fixture/install.sh" "$fixture/scripts/install.sh" "$fixture/update.sh"
  local log="${fixture}/dispatch.log"
  local -a envs=(
    "DISPATCH_LOG=$log" "CONFIG_FILE=${fixture}/etc/config.json"
    "DATA_DIR=${fixture}/data" "APP_DIR=${fixture}/app" "SYSTEMD_DIR=${fixture}/systemd"
  )

  env -u CODEX_POOL_INSTALL_MODE "${envs[@]}" bash "$fixture/install.sh" --minimal >/dev/null
  grep -q '^install:.*--minimal' "$log" || fail "fresh tree did not use canonical installer"

  : >"${fixture}/data/pool.sqlite3"
  env -u CODEX_POOL_INSTALL_MODE "${envs[@]}" bash "$fixture/install.sh" --minimal >/dev/null
  [[ "$(tail -1 "$log")" == update:* ]] || fail "existing DB did not auto-dispatch to update.sh"

  env -u CODEX_POOL_INSTALL_MODE "${envs[@]}" bash "$fixture/install.sh" --fresh-install --minimal >/dev/null
  [[ "$(tail -1 "$log")" == install:* ]] || fail "--fresh-install override was ignored"
  grep -q -- '--fresh-install' "$log" && fail "wrapper-only flag leaked to canonical installer"

  rm -f "${fixture}/data/pool.sqlite3"
  env -u CODEX_POOL_INSTALL_MODE "${envs[@]}" bash "$fixture/install.sh" --update --minimal >/dev/null
  [[ "$(tail -1 "$log")" == update:* ]] || fail "--update override was ignored"
}

test_backup_rotation() {
  local fixture="${TMP}/backups" db="${TMP}/pool.sqlite3"
  mkdir -p "$fixture"
  if command -v sqlite3 >/dev/null 2>&1; then
    sqlite3 "$db" 'CREATE TABLE records(id INTEGER PRIMARY KEY, value TEXT); INSERT INTO records(value) VALUES("v1");'
  else
    printf 'database-v1\n' >"$db"
  fi

  # Source functions without running update main.
  # shellcheck disable=SC1090
  source "$ROOT/update.sh"
  BACKUP_DIR="$fixture"
  BACKUP_KEEP=3
  BACKUP_MAX_AGE_DAYS=30
  BACKUP_MAX_BYTES=1073741824

  backup_db "$db" >/dev/null
  backup_db "$db" >/dev/null
  [[ "$(find "$fixture" -name 'pool-*.sqlite3.gz' | wc -l)" -eq 1 ]] || fail "identical update snapshots accumulated"

  local n
  for n in 2 3 4; do
    if command -v sqlite3 >/dev/null 2>&1; then
      sqlite3 "$db" "INSERT INTO records(value) VALUES('v${n}');"
    else
      printf 'database-v%s\n' "$n" >>"$db"
    fi
    backup_db "$db" >/dev/null
  done
  [[ "$(find "$fixture" -name 'pool-*.sqlite3.gz' | wc -l)" -eq 3 ]] || fail "BACKUP_KEEP=3 was not enforced"

  touch -d '40 days ago' "$(find "$fixture" -name 'pool-*.sqlite3.gz' | sort | head -1)"
  BACKUP_MAX_AGE_DAYS=30 prune_backups "$fixture" >/dev/null
  [[ "$(find "$fixture" -name 'pool-*.sqlite3.gz' | wc -l)" -eq 2 ]] || fail "expired backup was not pruned"

  BACKUP_MAX_AGE_DAYS=0 BACKUP_MAX_BYTES=1 prune_backups "$fixture" >/dev/null
  [[ "$(find "$fixture" -name 'pool-*.sqlite3.gz' | wc -l)" -eq 1 ]] || fail "byte ceiling did not preserve exactly the newest snapshot"
  gzip -t "$(find "$fixture" -name 'pool-*.sqlite3.gz')" || fail "retained snapshot is corrupt"
}

test_managed_source_pruning() {
  local fixture="${TMP}/source" manifest="${TMP}/manifest.txt"
  mkdir -p "$fixture/cmd/app" "$fixture/internal/api" "$fixture/web-spa/src" "$fixture/deploy/mailbox" "$fixture/scripts" "$fixture/config" "$fixture/data" "$fixture/plugins"
  printf 'package main\n' >"$fixture/cmd/app/main.go"
  printf 'package api\n' >"$fixture/internal/api/removed.go"
  printf 'export const keep = true;\n' >"$fixture/web-spa/src/keep.ts"
  printf 'export const stale = true;\n' >"$fixture/web-spa/src/removed.js"
  printf 'stale deploy\n' >"$fixture/deploy/mailbox/removed.js"
  printf '#!/bin/sh\n' >"$fixture/scripts/keep.sh"
  printf '{}\n' >"$fixture/config/config.json"
  printf 'runtime\n' >"$fixture/data/pool.sqlite3"
  printf 'plugin\n' >"$fixture/plugins/custom.go"
  printf '%s\n' 'cmd/app/main.go' 'scripts/keep.sh' 'web-spa/src/keep.ts' >"$manifest"

  PROJECT_ROOT="$fixture" MANAGED_SOURCE_MANIFEST="$manifest" "$ROOT/scripts/prune-managed-source.sh" >/dev/null
  [[ -f "$fixture/cmd/app/main.go" && -f "$fixture/scripts/keep.sh" && -f "$fixture/web-spa/src/keep.ts" ]] || fail "manifest files were removed"
  [[ ! -e "$fixture/internal/api/removed.go" && ! -e "$fixture/web-spa/src/removed.js" && ! -e "$fixture/deploy/mailbox/removed.js" ]] || fail "stale managed files survived"
  [[ -f "$fixture/config/config.json" && -f "$fixture/data/pool.sqlite3" && -f "$fixture/plugins/custom.go" ]] || fail "operator/runtime files were touched"

  printf 'package api\n' >"$fixture/internal/api/optout.go"
  PROJECT_ROOT="$fixture" MANAGED_SOURCE_MANIFEST="$manifest" PRUNE_MANAGED_SOURCE=0 "$ROOT/scripts/prune-managed-source.sh" >/dev/null
  [[ -f "$fixture/internal/api/optout.go" ]] || fail "PRUNE_MANAGED_SOURCE=0 was ignored"
}

test_install_dispatch
test_backup_rotation
test_managed_source_pruning
printf 'PASS: install dispatch, bounded backups, and managed-source convergence\n'
