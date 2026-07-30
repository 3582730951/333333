#!/usr/bin/env bash
set -Eeuo pipefail

ROOT=/root/autodl-tmp/cpupg-20260730
GO_TARBALL=/root/autodl-tmp/go1.25.12.linux-amd64.tar.gz
GO_TARBALL_SHA256=234828b7a89e0e303d2556310ee549fbcf253d28de937bac3da13d6294262ac1
source "$ROOT/install-env.sh"
export BUILD_DIR="$ROOT/build-old"
export RELEASE_ID_OVERRIDE=old-0873de57
export GOPROXY=https://goproxy.cn,direct
export GOSUMDB=sum.golang.google.cn
test "$(sha256sum "$GO_TARBALL" | awk '{print $1}')" = "$GO_TARBALL_SHA256"
rm -rf "$GO_INSTALL_ROOT/go1.25.12"
mkdir -p "$GO_INSTALL_ROOT/go1.25.12"
tar -xzf "$GO_TARBALL" --strip-components=1 -C "$GO_INSTALL_ROOT/go1.25.12"
export GO_BIN="$GO_INSTALL_ROOT/go1.25.12/bin/go"
"$GO_BIN" version

cd "$ROOT/old-src"
set +e
./install.sh \
  --minimal \
  --no-systemd \
  --no-start \
  --no-tests \
  --no-migrate-user-groups \
  --listen-addr "127.0.0.1:$PORT" \
  2>&1 | tee "$ROOT/logs/old-install-retry.log"
status=${PIPESTATUS[0]}
set -e
printf '%s\n' "$status" >"$ROOT/logs/old-install.exit"
test "$status" -eq 0

{
  echo "hostname=$(hostname)"
  echo "port=$PORT"
  echo "config=$(realpath "$ROOT/etc/config.json")"
  echo "binary=$(realpath "$ROOT/prefix/bin/codex-pool-server")"
  echo "binary_sha256=$(sha256sum "$ROOT/prefix/bin/codex-pool-server" | awk '{print $1}')"
  "$ROOT/prefix/bin/codex-pool-server" --version 2>&1 || true
} | tee "$ROOT/logs/stage1-summary.txt"
