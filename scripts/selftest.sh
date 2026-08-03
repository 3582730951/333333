#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."

echo "[1/5] gofmt"
GO_SOURCE_ROOTS=(cmd internal tools services workers)
mapfile -d '' -t go_files < <(find "${GO_SOURCE_ROOTS[@]}" -type f -name '*.go' -print0)
unformatted="$(gofmt -l "${go_files[@]}")"
if [[ -n "$unformatted" ]]; then
  printf 'unformatted current-source files:\n%s\n' "$unformatted" >&2
  exit 1
fi

echo "[2/5] go test"
go test ./...

echo "[3/5] repeated go test"
go test ./... -count=2

echo "[4/5] go vet"
go vet ./...

echo "[5/5] build"
go build -trimpath -o /tmp/codex-pool-server ./cmd/pool-server

if [[ "${CODEX_POOL_RUN_SIDECAR_SELFTEST:-0}" == "1" ]]; then
  echo "[sidecar] curl_cffi selftest"
  python3 -m venv /tmp/codex_pool_sidecar_selftest
  # shellcheck disable=SC1091
  . /tmp/codex_pool_sidecar_selftest/bin/activate
  pip install -q -r sidecar/requirements.txt
  python sidecar/curl_cffi_sidecar.py --selftest
fi

echo "All tests passed"
