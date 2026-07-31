#!/usr/bin/env bash
set -Eeuo pipefail

ROOT=/root/autodl-tmp/final-apple-email-deploy-20260731
RECORDS="$ROOT/records"
LOGS="$ROOT/logs"
CONTROL="$ROOT/service-control.sh"
MAIN_CFG=/root/autodl-tmp/cpupg-20260730/etc/config.json
FRONT_CFG=/root/autodl-tmp/frontend-ui-shot-20260731/runtime/config.json
MAIN_FINAL=/root/autodl-tmp/cpupg-20260730/prefix/lib/codex-pool/releases/apple-email-overflow-final/codex-pool-server
FRONT_FINAL=/root/autodl-tmp/frontend-ui-shot-20260731/releases/apple-email-overflow-final/codex-pool-server

bash "$CONTROL" status > >(tee "$LOGS/final-status.literal.log") 2>&1

python3 - <<'PY'
from __future__ import annotations

import hashlib
import json
from pathlib import Path

root = Path("/root/autodl-tmp/final-apple-email-deploy-20260731")
records = root / "records"
rollback = root / "rollback"

def load(name: str):
    return json.loads((records / name).read_text())

def sha(path: Path):
    return hashlib.sha256(path.read_bytes()).hexdigest()

pre_main = load("pre-main-data.json")
pre_front = load("pre-frontend-data.json")
final_main = load("status-main-data.json")
final_front = load("status-frontend-data.json")
screens = load("screenshot-deployed-report.json")
rollback_main = load("rollback-main.ready.json")
rollback_front = load("rollback-frontend.ready.json")
redeploy_main = load("redeploy-main.ready.json")
redeploy_front = load("redeploy-frontend.ready.json")

main_config = Path("/root/autodl-tmp/cpupg-20260730/etc/config.json")
front_config = Path("/root/autodl-tmp/frontend-ui-shot-20260731/runtime/config.json")
main_binary = Path("/root/autodl-tmp/cpupg-20260730/prefix/lib/codex-pool/releases/apple-email-overflow-final/codex-pool-server")
front_binary = Path("/root/autodl-tmp/frontend-ui-shot-20260731/releases/apple-email-overflow-final/codex-pool-server")

result = {
    "deployment_mode": "native_no_docker",
    "final_binary_sha256": sha(main_binary),
    "main_binary_matches_frontend": sha(main_binary) == sha(front_binary),
    "main_release": load("redeploy-main.ready.json")["release_id"],
    "frontend_release": load("redeploy-frontend.ready.json")["release_id"],
    "main_console_sha256": (records / "status-main-console.sha256").read_text().strip(),
    "frontend_console_sha256": (records / "status-frontend-console.sha256").read_text().strip(),
    "data_preserved": {
        "main": pre_main == final_main,
        "frontend": pre_front == final_front,
        "main_counts": final_main["counts"],
        "frontend_counts": final_front["counts"],
        "quick_check": [final_main["quick_check"], final_front["quick_check"]],
        "integrity_check": [final_main["integrity_check"], final_front["integrity_check"]],
    },
    "config_preserved": {
        "main": sha(main_config) == sha(rollback / "main-config.json"),
        "frontend": sha(front_config) == sha(rollback / "frontend-config.json"),
    },
    "rollback": {
        "main_release": rollback_main["release_id"],
        "frontend_release": rollback_front["release_id"],
        "verified": (
            rollback_main["release_id"] == "backend-headroom-final-main"
            and rollback_front["release_id"] == "backend-headroom-final-frontend"
        ),
    },
    "redeploy": {
        "main_release": redeploy_main["release_id"],
        "frontend_release": redeploy_front["release_id"],
        "verified": (
            redeploy_main["release_id"] == "apple-email-overflow-final-main"
            and redeploy_front["release_id"] == "apple-email-overflow-final-frontend"
        ),
    },
    "screenshots": {
        "total": screens["total"],
        "passed": screens["passed"],
        "issues": screens["issues"],
        "escaped_cell_content": sum(
            len(item.get("escapedCellContent", [])) for item in screens["results"]
        ),
    },
}
assert result["final_binary_sha256"] == "429f98e8fb44b62b2fe4f71ca8745923dd6feac83629964e2ec82876e8cd9046"
assert result["main_binary_matches_frontend"]
assert result["main_release"] == "apple-email-overflow-final-main"
assert result["frontend_release"] == "apple-email-overflow-final-frontend"
assert result["main_console_sha256"] == "c56c968e9703f066dca5e601fb836c1665ff332843512d98ea769a308ed0037f"
assert result["frontend_console_sha256"] == result["main_console_sha256"]
assert result["data_preserved"]["main"] and result["data_preserved"]["frontend"]
assert result["config_preserved"]["main"] and result["config_preserved"]["frontend"]
assert result["rollback"]["verified"] and result["redeploy"]["verified"]
assert result["screenshots"] == {
    "total": 6, "passed": 6, "issues": 0, "escaped_cell_content": 0,
}
(records / "final-deployment-summary.json").write_text(
    json.dumps(result, ensure_ascii=False, indent=2) + "\n", encoding="utf-8"
)
print(json.dumps(result, ensure_ascii=False, separators=(",", ":")))
PY

rm -rf "$ROOT/sanitized-evidence"
mkdir -p "$ROOT/sanitized-evidence"
cp -a "$RECORDS" "$ROOT/sanitized-evidence/"
cp -a "$ROOT/screenshots" "$ROOT/sanitized-evidence/"
cp -a "$LOGS" "$ROOT/sanitized-evidence/"
cp -a "$CONTROL" "$ROOT/sanitized-evidence/"

for config in "$MAIN_CFG" "$FRONT_CFG"; do
  token="$(python3 - "$config" <<'PY'
import json
import sys
print(json.load(open(sys.argv[1])).get("admin_token", ""))
PY
)"
  if [[ -n "$token" ]] && grep -R -F -a -q -- "$token" "$ROOT/sanitized-evidence"; then
    printf 'live token found in deployment evidence\n' >&2
    exit 1
  fi
done

tar -C "$ROOT/sanitized-evidence" -czf "$ROOT/final-cloud-deploy-evidence.tar.gz" .
sha256sum "$ROOT/final-cloud-deploy-evidence.tar.gz" >"$ROOT/final-cloud-deploy-evidence.tar.gz.sha256"
printf 'FINAL_DEPLOY_EVIDENCE_SHA256=%s\n' "$(sha256sum "$ROOT/final-cloud-deploy-evidence.tar.gz" | awk '{print $1}')"
printf 'FINAL_DEPLOY_EVIDENCE_OK=1\n'
