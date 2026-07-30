#!/usr/bin/env bash
set -Eeuo pipefail

ROOT=/root/autodl-tmp/cpupg-20260730
source "$ROOT/install-env.sh"
BASE="http://127.0.0.1:$PORT"
ROLLBACK=/root/autodl-tmp/rollback-no-systemd.sh
OUT="$ROOT/logs/rollback-test"
mkdir -p "$OUT"

bash "$ROLLBACK" old | tee "$OUT/switch-old.txt"
curl -fsS "$BASE/admin/accounts" >"$OUT/old-accounts.json"
curl -fsS "$BASE/admin/egress-profiles" >"$OUT/old-egress-profiles.json"
curl -fsS "$BASE/admin/providers" >"$OUT/old-providers.json"
python3 - "$OUT" <<'PY'
import json,pathlib,sys
p=pathlib.Path(sys.argv[1])
ready=json.loads((p/"switch-old.txt").read_text().splitlines()[0])
assert ready["release_id"]=="old-0873de57",ready
joined="\n".join((p/n).read_text() for n in [
    "old-accounts.json","old-egress-profiles.json","old-providers.json"
])
for value in [
    "alpha@example.internal","beta@example.internal","legacy-direct-us",
    "legacy-http-exit","legacy-sidecar-exit","legacy-relay",
]:
    assert value in joined,value
print("old_rollback_behavior_verified")
PY

bash "$ROLLBACK" new | tee "$OUT/switch-new.txt"
curl -fsS "$BASE/admin/accounts" >"$OUT/new-accounts.json"
curl -fsS "$BASE/admin/egress-profiles" >"$OUT/new-egress-profiles.json"
curl -fsS "$BASE/admin/providers" >"$OUT/new-providers.json"
python3 - "$OUT" <<'PY'
import json,pathlib,sys
p=pathlib.Path(sys.argv[1])
ready=json.loads((p/"switch-new.txt").read_text().splitlines()[0])
assert ready["release_id"]=="new-goal-context-fix",ready
joined="\n".join((p/n).read_text() for n in [
    "new-accounts.json","new-egress-profiles.json","new-providers.json"
])
for value in [
    "alpha@example.internal","beta@example.internal","legacy-direct-us",
    "legacy-http-exit","legacy-sidecar-exit","legacy-relay",
]:
    assert value in joined,value
print("new_rollforward_behavior_verified")
PY

{
  echo "rollback_exit=0"
  echo "old_ready=$(head -n 1 "$OUT/switch-old.txt")"
  echo "rollforward_exit=0"
  echo "new_ready=$(head -n 1 "$OUT/switch-new.txt")"
  echo "final_current=$(readlink "$ROOT/prefix/lib/codex-pool/current")"
  echo "final_pid=$(cat "$ROOT/new.pid")"
} | tee "$OUT/verification.txt"
