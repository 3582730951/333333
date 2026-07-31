#!/usr/bin/env bash
set -Eeuo pipefail

ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
REPO=$(cd -- "$ROOT/../.." && pwd)

cd "$REPO"
sha256sum -c "$ROOT/SHA256SUMS"

bash -n "$ROOT/production-control.sh"
bash -n "$ROOT/remote-install-current.sh"
bash -n "$ROOT/inspect-production.sh"

python3 - "$ROOT/modified/codex-pool-server-linux-amd64" <<'PY'
import sys

header = open(sys.argv[1], "rb").read(20)
assert header[:4] == b"\x7fELF"
assert header[4] == 2, "artifact is not ELF64"
assert header[5] == 1, "artifact is not little-endian"
assert int.from_bytes(header[18:20], "little") == 62, "artifact is not x86-64"
print("ELF_VERIFY=ok class=64 machine=x86-64")
PY

(
  cd "$ROOT/research"
  sha256sum -c VERIFIED_SHA256SUMS
)

sha256sum -c "$ROOT/diagnostics/input-zips.sha256"

python3 - "$ROOT" <<'PY'
import json
import pathlib
import sys

root = pathlib.Path(sys.argv[1])
report = json.loads(
    (root / "remote-final/screenshots/final/final-ui-visual-report.json").read_text()
)
assert report["total"] == 32
assert report["passed"] == 32
assert report["issues"] == 0
assert report["expectedScreenshots"] == 33
assert len(list((root / "remote-final/screenshots/final").glob("*.png"))) == 33

seed = json.loads((root / "remote-final/records/direct-seed-manifest.json").read_text())
assert seed["integrity_check"] == "ok"
assert seed["counts"]["accounts"] == 16
assert seed["counts"]["usage_records"] == 288
assert seed["counts"]["team_lifecycle_workflows"] == 8

status_paths = (
    "remote-final/records/install.status",
    "remote-final/records/final-ui-visual-rerun.status",
    "remote-verification-records/go-all-green.status",
    "remote-verification-records/go-vet-race.status",
    "remote-verification-records/go-api-race-exact.status",
    "remote-verification-records/frontend-verify-green.status",
    "remote-verification-records/installer-reauth-static.status",
    "remote-production/records/deploy.status",
    "remote-production/records/rollback.status",
    "remote-production/records/redeploy.status",
    "remote-production/records/final-verify.status",
    "remote-production/records/final-live-verify.status",
)
for relative in status_paths:
    value = (root / relative).read_text().strip()
    assert value == "0", (relative, value)

manifest = json.loads(
    (root / "remote-production/records/deployment-manifest.json").read_text()
)
expected_binary = "b2c0d6d718f0b880a3901768e2ce7007bda19cd2349b779d3693ec958e2b5543"
assert manifest["files"]["modified_main"]["sha256"] == expected_binary
assert manifest["files"]["modified_frontend"]["sha256"] == expected_binary
assert manifest["modified_releases"]["main"] == "backend-mail-automation-final-main"
assert manifest["modified_releases"]["frontend"] == "backend-mail-automation-final-frontend"

delivery_manifest = json.loads((root / "manifest.json").read_text())
for role in delivery_manifest["roles"].values():
    artifact = root / role["path"]
    assert artifact.stat().st_size == role["bytes"], role["path"]
    import hashlib
    assert hashlib.sha256(artifact.read_bytes()).hexdigest() == role["sha256"], role["path"]

print("JSON_AND_STATUS_VERIFY=ok")
PY

grep -q 'GO_ALL_GREEN_EXIT=0' \
  "$ROOT/remote-verification-records/go-all-green.literal.log"
grep -q 'GO_API_RACE_EXACT_EXIT=0' \
  "$ROOT/remote-verification-records/go-api-race-exact.literal.log"
grep -q 'FRONTEND_VERIFY_GREEN_EXIT=0' \
  "$ROOT/remote-verification-records/frontend-verify-green.literal.log"
grep -q 'DIRECT_SEED_RERUN_EXIT=0' \
  "$ROOT/remote-final/records/direct-seed-rerun.literal.log"
grep -q 'FINAL_UI_VISUAL total=32 passed=32 issues=0 screenshots=33' \
  "$ROOT/remote-final/records/final-ui-visual-rerun.literal.log"
grep -q 'ROLLBACK_OK' \
  "$ROOT/remote-production/records/rollback.literal.log"
grep -q 'REDEPLOY_OK' \
  "$ROOT/remote-production/records/redeploy.literal.log"
grep -q 'FINAL_VERIFY_OK' \
  "$ROOT/remote-production/records/final-verify.literal.log"
grep -q 'FINAL_VERIFY_OK' \
  "$ROOT/remote-production/records/final-live-verify.literal.log"

temporary="$REPO/.run/backend-mail-delivery-verify-$$"
rm -rf "$temporary"
mkdir -p "$temporary"
trap 'rm -rf "$temporary"' EXIT
git archive HEAD | tar -x -C "$temporary"
relative_temporary=${temporary#"$REPO/"}
git apply --check --directory="$relative_temporary" "$ROOT/backend-mail-automation.patch"
git apply --directory="$relative_temporary" "$ROOT/backend-mail-automation.patch"
while read -r expected path; do
  actual=$(sha256sum "$temporary/$path" | awk '{print $1}')
  [[ "$actual" == "$expected" ]] || {
    echo "patched source hash mismatch: $path" >&2
    exit 1
  }
done <"$ROOT/source-files.sha256"

echo "PATCH_REPLAY_VERIFY=ok files=$(wc -l <"$ROOT/source-files.manifest")"
echo "DELIVERY_VERIFY_EXIT=0"
