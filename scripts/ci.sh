#!/usr/bin/env bash
# Full automated-test pipeline for pool_server: formatting, static analysis, the Go
# test suite (with the race detector on the concurrency-heavy packages), a production
# build, and the admin SPA build. One command to validate the whole tree.
#
# Usage:  scripts/ci.sh            # full pipeline
#         SKIP_SPA=1 scripts/ci.sh # skip the (slow) SPA build
#         SKIP_VISUAL_SMOKE=1 scripts/ci.sh # skip browser screenshot smoke
#         SKIP_RACE=1 scripts/ci.sh
set -euo pipefail
cd "$(dirname "$0")/.."

export PATH="$PATH:/usr/local/go/bin:$HOME/go/bin"
# This tree is distributed without a .git dir; disable VCS stamping so builds succeed.
export GOFLAGS="-buildvcs=false"

step() { printf '\n\033[1;36m=== %s ===\033[0m\n' "$1"; }

step "1/6 gofmt"
unformatted="$(gofmt -l . | grep -v '^web-spa/' || true)"
if [ -n "$unformatted" ]; then
  echo "gofmt needs running on:"; echo "$unformatted"; exit 1
fi
echo "ok"

step "2/6 go vet"
go vet ./...
echo "ok"

step "3/6 go build"
go build ./...
go build -trimpath -o /tmp/pool-server ./cmd/pool-server
echo "ok ($(du -h /tmp/pool-server | cut -f1))"

step "4/6 go test"
go test ./...

if [ "${SKIP_RACE:-0}" != "1" ]; then
  step "5/6 go test -race (concurrency-heavy packages)"
  go test -race ./internal/scheduler/... ./internal/api/... ./internal/upstream/... ./internal/registration/...
else
  step "5/6 go test -race — SKIPPED (SKIP_RACE=1)"
fi

if command -v staticcheck >/dev/null 2>&1; then
  step "staticcheck"
  staticcheck ./internal/... || echo "(staticcheck reported findings — review above)"
fi

if [ "${SKIP_SPA:-0}" != "1" ]; then
  step "6/6 SPA build + visual smoke (web-spa → internal/console/dist)"
  if command -v npm >/dev/null 2>&1; then
    [ -d web-spa/node_modules ] || npm --prefix web-spa install
    if [ "${SKIP_VISUAL_SMOKE:-0}" != "1" ]; then
      npm --prefix web-spa run check:visual-smoke
    else
      echo "visual smoke check skipped (SKIP_VISUAL_SMOKE=1)"
    fi
    npm --prefix web-spa run build
    echo "ok — rebuilt embedded console; re-run 'go build' to embed it"
  else
    echo "npm not found — skipping SPA build"
  fi
else
  step "6/6 SPA build — SKIPPED (SKIP_SPA=1)"
fi

printf '\n\033[1;32mAll automated checks passed.\033[0m\n'
