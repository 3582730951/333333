#!/usr/bin/env bash
set -Eeuo pipefail

ROOT="${ROOT:-/root/autodl-tmp/legacy-install-upgrade-20260731}"
APP="${ROOT}/prefix/lib/codex-pool"
OLD_RELEASE="legacy-cache-hit-optimization"
FINAL_RELEASE="apple-backend-optimized-v2-20260731"
EXPECTED_FINGERPRINT="9295ea6d34bdefe8e7152905920fef7b8c39e743fb2beb8806c2bc19071af869"
CONTROL="${ROOT}/uploads/remote-service-control.sh"
VERIFY="${ROOT}/uploads/verify-upgrade-data.py"
LOG="${ROOT}/logs/rollback-redeploy.literal.log"

exec > >(tee "$LOG") 2>&1

switch_release() {
  local release="$1"
  local target="${APP}/releases/${release}"
  local next="${APP}/current.next.$$"
  [[ -x "${target}/codex-pool-server" ]]
  rm -f "$next"
  ln -s "$target" "$next"
  mv -Tf "$next" "${APP}/current"
  printf 'SWITCHED_RELEASE=%s\n' "$release"
}

assert_phase() {
  local phase="$1"
  PHASE_NAME="$phase" EXPECTED="$EXPECTED_FINGERPRINT" ROOT_PATH="$ROOT" python3 - <<'PY'
import json
import os
from pathlib import Path

root = Path(os.environ["ROOT_PATH"])
phase = os.environ["PHASE_NAME"]
expected = os.environ["EXPECTED"]
data = json.loads((root / "records" / f"data-{phase}.json").read_text())
assert data["quick_check"] == "ok"
assert data["integrity_check"] == "ok"
assert data["logical_fingerprint_sha256"] == expected
assert data["counts"]["accounts"] == 16
assert data["counts"]["email_pool"] == 18
assert data["counts"]["usage_records"] == 168
assert data["config_sha256"] == "46b179c50b737b88daca98c5d4cf48f6190a414ec275550b948e70ebc8226b40"
print(f"PHASE_ASSERTED={phase}")
PY
}

curl --noproxy '*' -fsS "http://127.0.0.1:34276/console/" >"${ROOT}/records/console-final-before.html"
final_before_sha="$(sha256sum "${ROOT}/records/console-final-before.html" | awk '{print $1}')"

bash "$CONTROL" stop
switch_release "$OLD_RELEASE"
bash "$CONTROL" start
PHASE=rollback-old python3 "$VERIFY"
assert_phase rollback-old
curl --noproxy '*' -fsS "http://127.0.0.1:34276/console/" >"${ROOT}/records/console-rollback-old.html"
old_console_sha="$(sha256sum "${ROOT}/records/console-rollback-old.html" | awk '{print $1}')"
grep -q 'index-lKLFFv96.js' "${ROOT}/records/console-rollback-old.html"
[[ "$old_console_sha" != "$final_before_sha" ]]
printf 'ROLLBACK_BEHAVIOR=old-release-ready old-console-restored data-preserved\n'

bash "$CONTROL" stop
switch_release "$FINAL_RELEASE"
bash "$CONTROL" start
PHASE=redeploy-final python3 "$VERIFY"
assert_phase redeploy-final
curl --noproxy '*' -fsS "http://127.0.0.1:34276/console/" >"${ROOT}/records/console-redeploy-final.html"
final_after_sha="$(sha256sum "${ROOT}/records/console-redeploy-final.html" | awk '{print $1}')"
[[ "$final_after_sha" == "$final_before_sha" ]]

cat >"${ROOT}/records/rollback-redeploy.json" <<EOF
{
  "rollback_release": "${OLD_RELEASE}",
  "rollback_console_sha256": "${old_console_sha}",
  "final_release": "${FINAL_RELEASE}",
  "final_console_sha256": "${final_after_sha}",
  "logical_fingerprint_sha256": "${EXPECTED_FINGERPRINT}",
  "rollback_ready": true,
  "redeploy_ready": true,
  "data_preserved": true
}
EOF
printf 'REDEPLOY_BEHAVIOR=final-release-ready final-console-restored data-preserved\n'
printf 'ROLLBACK_REDEPLOY_OK=1\n'
