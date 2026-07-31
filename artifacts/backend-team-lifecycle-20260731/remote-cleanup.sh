#!/usr/bin/env bash
set -Eeuo pipefail

DARK=/root/autodl-tmp/dark-mode-fix-20260731
INSTALL=/root/autodl-tmp/team-lifecycle-install-20260731

if [[ -s "$DARK/runtime/server.pid" ]]; then
  pid="$(cat "$DARK/runtime/server.pid")"
  kill "$pid" 2>/dev/null || true
  for _ in $(seq 1 80); do
    kill -0 "$pid" 2>/dev/null || break
    sleep 0.1
  done
fi

if [[ -x "$INSTALL/service-control.sh" ]]; then
  ROOT="$INSTALL" PORT=34277 "$INSTALL/service-control.sh" stop
fi

rm -rf \
  "$DARK/runtime" \
  "$INSTALL/etc" \
  "$INSTALL/data" \
  "$INSTALL/run" \
  /root/autodl-tmp/team-lifecycle-patch-verify-20260731
rm -f \
  "$INSTALL/records/admin.token" \
  /root/autodl-tmp/source-patch-baseline.tar.gz \
  /root/autodl-tmp/team-lifecycle-full-source.patch \
  /root/autodl-tmp/modified-source-files.sha256 \
  /root/autodl-tmp/verify-patch.sh \
  /root/autodl-tmp/run-final-tests-remote.sh \
  /root/autodl-tmp/team-lifecycle-production-verify.py

for port in 34276 34277; do
  if curl --noproxy '*' -fsS --max-time 1 "http://127.0.0.1:$port/healthz" >/dev/null 2>&1; then
    printf 'TEMP_ENDPOINT_%s=reachable\n' "$port"
    exit 1
  fi
  printf 'TEMP_ENDPOINT_%s=stopped\n' "$port"
done

if matches="$(pgrep -af 'dark-mode-fix-20260731/codex-pool-server.backend-final|team-lifecycle-install-20260731/prefix/bin/codex-pool-server.*--config')"; then
  printf '%s\n' "$matches"
  exit 1
fi
printf 'TEMP_PROCESS_MATCHES=0\n'

test -x "$INSTALL/prefix/bin/codex-pool-server"
printf 'INSTALL_ARTIFACT_PRESERVED=1\n'

/root/autodl-tmp/team-lifecycle-production-service-control.sh status
