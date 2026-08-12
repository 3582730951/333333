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

test_explicit_listen_addr_resolution() {
  # Source update helpers without executing main. Explicit CLI values must be
  # exported because scripts/install.sh and the no-systemd summary both consume
  # LISTEN_ADDR after the friendly wrapper dispatches into update.sh.
  # shellcheck disable=SC1090
  source "$ROOT/update.sh"

  local actual summary
  unset LISTEN_ADDR
  resolve_listen_addr --minimal --listen-addr 127.0.0.1:19001 --no-systemd
  actual="$(bash -c 'printf %s "$LISTEN_ADDR"')"
  [[ "$actual" == "127.0.0.1:19001" ]] ||
    fail "--listen-addr VALUE was not normalized and exported"

  unset LISTEN_ADDR
  resolve_listen_addr --listen-addr=127.0.0.1:19002 --minimal
  actual="$(bash -c 'printf %s "$LISTEN_ADDR"')"
  [[ "$actual" == "127.0.0.1:19002" ]] ||
    fail "--listen-addr=VALUE was not normalized and exported"

  if (unset LISTEN_ADDR; resolve_listen_addr --listen-addr --no-systemd) \
      >/dev/null 2>&1; then
    fail "--listen-addr accepted the next option as its address"
  fi
  if (unset LISTEN_ADDR; resolve_listen_addr --listen-addr=) >/dev/null 2>&1; then
    fail "empty --listen-addr= value was accepted"
  fi

  # Force the no-systemd path and prove the printed health/frontend target uses
  # the explicit loopback address instead of the zero-config public fallback.
  discover_listen_from_systemd() { return 1; }
  DB="${TMP}/listen-summary.sqlite3"
  BACKUP=""
  BEFORE=""
  AFTER=""
  summary="$(print_summary)"
  grep -Fq '监听 Listen:        127.0.0.1:19002' <<<"$summary" ||
    fail "no-systemd summary ignored the explicit listen target"
  if grep -Fq '0.0.0.0:8787' <<<"$summary"; then
    fail "no-systemd summary leaked the default listen target"
  fi
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

test_managed_source_manifest_current() {
  local generated="${TMP}/managed-source-manifest.generated.txt"
  LC_ALL=C sort -c -u "$ROOT/scripts/managed-source-manifest.txt" ||
    fail "managed source manifest is not sorted and unique"
  bash "$ROOT/scripts/generate-managed-source-manifest.sh" "$generated" >/dev/null
  if ! cmp -s "$ROOT/scripts/managed-source-manifest.txt" "$generated"; then
    diff -u "$ROOT/scripts/managed-source-manifest.txt" "$generated" >&2 || true
    fail "managed source manifest is stale; regenerate it before packaging"
  fi
}

test_console_release_guard() {
  local fixture="${TMP}/console-release" manifest="${TMP}/console-release-manifest.txt"
  mkdir -p "$fixture/internal/console/dist/assets"
  printf '%s\n' \
    '<!doctype html><script type="module" src="/console/assets/index-new.js"></script>' \
    '<link rel="stylesheet" href="/console/assets/index-new.css">' \
    >"$fixture/internal/console/dist/index.html"
  printf 'console.log("ok")\n' >"$fixture/internal/console/dist/assets/index-new.js"
  printf 'body { color: black; }\n' >"$fixture/internal/console/dist/assets/index-new.css"
  printf '%s\n' \
    'internal/console/dist/assets/index-new.css' \
    'internal/console/dist/assets/index-new.js' \
    'internal/console/dist/index.html' \
    >"$manifest"

  PROJECT_ROOT="$fixture" MANAGED_SOURCE_MANIFEST="$manifest" \
    bash "$ROOT/scripts/verify-console-release.sh" >/dev/null ||
    fail "complete embedded console release was rejected"

  printf 'stale chunk\n' >"$fixture/internal/console/dist/assets/index-old.js"
  if PROJECT_ROOT="$fixture" MANAGED_SOURCE_MANIFEST="$manifest" \
      bash "$ROOT/scripts/verify-console-release.sh" >/dev/null 2>&1; then
    fail "strict console release verification accepted an undeclared file"
  fi
  PROJECT_ROOT="$fixture" MANAGED_SOURCE_MANIFEST="$manifest" \
    CONSOLE_RELEASE_ALLOW_STALE=1 \
    bash "$ROOT/scripts/verify-console-release.sh" >/dev/null ||
    fail "pre-prune console verification rejected a harmless stale chunk"
  rm -f "$fixture/internal/console/dist/assets/index-old.js"

  rm -f "$fixture/internal/console/dist/assets/index-new.js"
  if PROJECT_ROOT="$fixture" MANAGED_SOURCE_MANIFEST="$manifest" \
      CONSOLE_RELEASE_ALLOW_STALE=1 \
      bash "$ROOT/scripts/verify-console-release.sh" >/dev/null 2>&1; then
    fail "console release verification accepted a missing entry asset"
  fi
}

test_console_prune_is_atomic() {
  local fixture="${TMP}/console-prune" manifest="${TMP}/console-prune-manifest.txt"
  mkdir -p "$fixture/internal/console/dist/assets" "$fixture/scripts"
  cp "$ROOT/scripts/verify-console-release.sh" "$fixture/scripts/verify-console-release.sh"
  printf '<script type="module" src="/console/assets/index-new.js"></script>\n' \
    >"$fixture/internal/console/dist/index.html"
  printf 'new release\n' >"$fixture/internal/console/dist/assets/index-new.js"
  printf 'old release\n' >"$fixture/internal/console/dist/assets/index-old.js"
  printf '%s\n' \
    'internal/console/dist/assets/index-old.js' \
    'internal/console/dist/index.html' \
    >"$manifest"

  if PROJECT_ROOT="$fixture" MANAGED_SOURCE_MANIFEST="$manifest" \
      "$ROOT/scripts/prune-managed-source.sh" >/dev/null 2>&1; then
    fail "source pruning accepted a mismatched console entry and asset manifest"
  fi
  [[ -f "$fixture/internal/console/dist/assets/index-new.js" ]] ||
    fail "source pruning deleted a new asset before validating the release"
}

test_manifest_generation_without_git() {
  local fixture="${TMP}/source-archive" generated="${TMP}/source-archive-manifest.txt"
  mkdir -p "$fixture/scripts" "$fixture/cmd/app" "$fixture/internal/console/dist/assets"
  cp "$ROOT/scripts/generate-managed-source-manifest.sh" "$fixture/scripts/generate-managed-source-manifest.sh"
  printf 'package main\n' >"$fixture/cmd/app/main.go"
  printf '<script src="/console/assets/index.js"></script>\n' >"$fixture/internal/console/dist/index.html"
  printf 'console.log("archive")\n' >"$fixture/internal/console/dist/assets/index.js"
  printf '<svg/>\n' >"$fixture/internal/console/dist/assets/logo.svg"

  bash "$fixture/scripts/generate-managed-source-manifest.sh" "$generated" >/dev/null
  for expected in \
    cmd/app/main.go \
    internal/console/dist/index.html \
    internal/console/dist/assets/index.js \
    internal/console/dist/assets/logo.svg; do
    grep -Fqx "$expected" "$generated" ||
      fail "source-archive manifest omitted ${expected}"
  done
}

test_build_selected_duplicate_scan() {
  command -v go >/dev/null 2>&1 || return 0
  # shellcheck disable=SC1090
  source "$ROOT/update.sh"

  local fixture="${TMP}/duplicate-scan" output real_output
  mkdir -p "$fixture/platform" "$fixture/ignored" "$fixture/duplicate"
  printf 'module duplicate-scan\n\ngo 1.25\n' >"$fixture/go.mod"
  printf '//go:build unix\n\npackage platform\n\nfunc processAlive() {}\n' >"$fixture/platform/process_unix.go"
  printf '//go:build windows\n\npackage platform\n\nfunc processAlive() {}\n' >"$fixture/platform/process_windows.go"
  printf '//go:build ignore\n\npackage ignored\n\nfunc main() {}\n' >"$fixture/ignored/one.go"
  printf '//go:build ignore\n\npackage ignored\n\nfunc main() {}\n' >"$fixture/ignored/two.go"
  printf 'package duplicate\n\nfunc staleSource() {}\n' >"$fixture/duplicate/one.go"
  printf 'package duplicate\n\nfunc staleSource() {}\n' >"$fixture/duplicate/two.go"

  output="$(PROJECT_ROOT="$fixture" find_duplicate_go_decls)"
  [[ "$output" == *'staleSource 重复声明于'* ]] ||
    fail "active same-package duplicate was not diagnosed: ${output:-<empty>}"
  [[ "$output" != *'processAlive'* && "$output" != *'/ignored/'* ]] ||
    fail "build-constrained files were falsely diagnosed: ${output}"

  real_output="$(PROJECT_ROOT="$ROOT" find_duplicate_go_decls)"
  [[ -z "$real_output" ]] ||
    fail "clean repository contains active-build duplicate declarations: ${real_output}"
}

test_install_dispatch
test_explicit_listen_addr_resolution
test_backup_rotation
test_managed_source_pruning
test_console_release_guard
test_console_prune_is_atomic
test_manifest_generation_without_git
test_managed_source_manifest_current
test_build_selected_duplicate_scan
printf 'PASS: install dispatch, bounded backups, managed-source convergence, console release closure, manifest freshness, and build-aware duplicate scanning\n'
