#!/usr/bin/env bash
set -Eeuo pipefail

ROOT="${ROOT:-/root/autodl-tmp/legacy-install-upgrade-20260731}"
RECORDS="${ROOT}/records"
FINAL_RELEASE="apple-backend-optimized-v2-20260731"
FINAL_DIR="${ROOT}/prefix/lib/codex-pool/releases/${FINAL_RELEASE}"
TOKEN="$(cat "${RECORDS}/admin.token")"

tar \
  --exclude='web-spa/node_modules' \
  --exclude='web-spa/node_modules/*' \
  --exclude='web-spa/.run' \
  --exclude='web-spa/playwright-report' \
  --exclude='web-spa/test-results' \
  --exclude='*/__pycache__' \
  --exclude='*.pyc' \
  -C "${ROOT}/new-src" \
  -czf "${RECORDS}/new-source-built-final.tar.gz" .
sha256sum "${RECORDS}/new-source-built-final.tar.gz" >"${RECORDS}/new-source-built-final.tar.gz.sha256"

cp "${ROOT}/prefix/lib/codex-pool/releases/legacy-cache-hit-optimization/release.json" \
  "${RECORDS}/release-old.json"
cp "${FINAL_DIR}/release.json" "${RECORDS}/release-final.json"

python3 - <<'PY'
from __future__ import annotations

import hashlib
import json
import urllib.request
from pathlib import Path

root = Path("/root/autodl-tmp/legacy-install-upgrade-20260731")
records = root / "records"
final_release = "apple-backend-optimized-v2-20260731"

def sha(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()

ports = {}
for port in (34273, 34274, 34276):
    with urllib.request.urlopen(f"http://127.0.0.1:{port}/readyz", timeout=3) as response:
        payload = json.load(response)
    ports[str(port)] = {
        "status": response.status,
        "release_id": payload.get("release_id"),
        "ready": bool(payload.get("ready", payload.get("ok", True))),
    }

pre = json.loads((records / "data-pre-v2-repair.json").read_text())
post = json.loads((records / "data-redeploy-final.json").read_text())
screens = json.loads((records / "screenshot-final-redeploy-report.json").read_text())
rollback = json.loads((records / "rollback-redeploy.json").read_text())
result = {
    "deployment_mode": "native_no_docker",
    "legacy_branch_head": "390edea477cb4d16b45133096bf1351d7d593db9",
    "install_exit_statuses": {
        "old": int((records / "install-old.exit-status").read_text()),
        "new_initial": int((records / "install-new.exit-status").read_text()),
        "new_final": int((records / "install-new-v2-final.exit-status").read_text()),
    },
    "frontend_build": json.loads((records / "new-ui-build.json").read_text()),
    "data_preservation": {
        "pre_fingerprint": pre["logical_fingerprint_sha256"],
        "post_fingerprint": post["logical_fingerprint_sha256"],
        "equal": pre["logical_fingerprint_sha256"] == post["logical_fingerprint_sha256"],
        "accounts": post["counts"]["accounts"],
        "email_pool": post["counts"]["email_pool"],
        "usage_records": post["counts"]["usage_records"],
        "config_sha256_before": pre["config_sha256"],
        "config_sha256_after": post["config_sha256"],
        "config_unchanged": pre["config_sha256"] == post["config_sha256"],
        "sqlite_quick_check": post["quick_check"],
        "sqlite_integrity_check": post["integrity_check"],
    },
    "screenshots": {
        "total": screens["total"],
        "passed": screens["passed"],
        "issues": screens["issues"],
        "escaped_cell_content": sum(
            len(item.get("escapedCellContent", [])) for item in screens["results"]
        ),
    },
    "rollback_redeploy": rollback,
    "ports": ports,
    "final_release": final_release,
    "final_binary_sha256": sha(
        root
        / "prefix"
        / "lib"
        / "codex-pool"
        / "releases"
        / final_release
        / "codex-pool-server"
    ),
    "old_binary_sha256": sha(
        root
        / "prefix"
        / "lib"
        / "codex-pool"
        / "releases"
        / "legacy-cache-hit-optimization"
        / "codex-pool-server"
    ),
    "old_install_sha256": sha(root / "old-src" / "install.sh"),
    "new_install_sha256": sha(root / "new-src" / "install.sh"),
    "built_source_sha256": sha(records / "new-source-built-final.tar.gz"),
}
assert result["install_exit_statuses"] == {"old": 0, "new_initial": 0, "new_final": 0}
assert result["frontend_build"]["npm_ci_exit"] == 0
assert result["frontend_build"]["build_exit"] == 0
assert result["frontend_build"]["email_regression_test_exit"] == 0
assert result["data_preservation"]["equal"]
assert result["data_preservation"]["config_unchanged"]
assert result["data_preservation"]["sqlite_quick_check"] == "ok"
assert result["data_preservation"]["sqlite_integrity_check"] == "ok"
assert result["screenshots"] == {
    "total": 6,
    "passed": 6,
    "issues": 0,
    "escaped_cell_content": 0,
}
assert result["rollback_redeploy"]["rollback_ready"]
assert result["rollback_redeploy"]["redeploy_ready"]
assert result["ports"]["34273"]["status"] == 200
assert result["ports"]["34274"]["status"] == 200
assert result["ports"]["34276"]["release_id"] == final_release
(records / "final-summary.json").write_text(
    json.dumps(result, ensure_ascii=False, indent=2) + "\n", encoding="utf-8"
)
print(json.dumps(result, ensure_ascii=False, separators=(",", ":")))
PY

rm -rf "${ROOT}/sanitized-evidence"
mkdir -p "${ROOT}/sanitized-evidence"
cp -a "${ROOT}/logs" "${ROOT}/sanitized-evidence/"
cp -a "${ROOT}/screenshots" "${ROOT}/sanitized-evidence/"
mkdir -p "${ROOT}/sanitized-evidence/records"
find "$RECORDS" -maxdepth 1 -type f ! -name 'admin.token' -exec cp -a {} "${ROOT}/sanitized-evidence/records/" \;
cp -a "${ROOT}/uploads/remote-install-version.sh" \
  "${ROOT}/uploads/remote-service-control.sh" \
  "${ROOT}/uploads/seed-direct-sqlite.py" \
  "${ROOT}/uploads/verify-upgrade-data.py" \
  "${ROOT}/uploads/remote-rollback-redeploy.sh" \
  "${ROOT}/sanitized-evidence/"

if grep -R -F -a -q -- "$TOKEN" "${ROOT}/sanitized-evidence"; then
  printf 'sensitive token found in sanitized evidence\n' >&2
  exit 1
fi
find "${ROOT}/sanitized-evidence" -name 'admin.token' -print -quit | grep -q . && exit 1

tar -C "${ROOT}/sanitized-evidence" -czf "${ROOT}/remote-evidence-sanitized.tar.gz" .
sha256sum "${ROOT}/remote-evidence-sanitized.tar.gz" \
  >"${ROOT}/remote-evidence-sanitized.tar.gz.sha256"

printf 'BUILT_SOURCE_SHA256=%s\n' "$(sha256sum "${RECORDS}/new-source-built-final.tar.gz" | awk '{print $1}')"
printf 'REMOTE_EVIDENCE_SHA256=%s\n' "$(sha256sum "${ROOT}/remote-evidence-sanitized.tar.gz" | awk '{print $1}')"
printf 'FINAL_BINARY_SHA256=%s\n' "$(sha256sum "${FINAL_DIR}/codex-pool-server" | awk '{print $1}')"
printf 'SANITIZED_EVIDENCE_OK=1\n'
