#!/usr/bin/env bash
set -Eeuo pipefail

ROOT=/root/autodl-tmp/legacy-install-upgrade-20260731
if [[ -x "$ROOT/uploads/remote-service-control.sh" ]]; then
  bash "$ROOT/uploads/remote-service-control.sh" stop
fi
rm -rf "$ROOT"
rm -f /root/autodl-tmp/frontend-ui-shot-20260731/browser-runner/capture-upgrade-fixture.mjs

python3 - <<'PY'
import json
import urllib.error
import urllib.request

result = {}
for port in (34273, 34274):
    with urllib.request.urlopen(f"http://127.0.0.1:{port}/readyz", timeout=3) as response:
        data = json.load(response)
    result[str(port)] = {
        "status": response.status,
        "ready": data.get("ready"),
        "release_id": data.get("release_id"),
    }
try:
    urllib.request.urlopen("http://127.0.0.1:34276/readyz", timeout=1)
    result["34276"] = "unexpected_listener"
except Exception:
    result["34276"] = "stopped"

assert result["34273"]["release_id"] == "apple-email-overflow-final-main"
assert result["34274"]["release_id"] == "apple-email-overflow-final-frontend"
assert result["34276"] == "stopped"
print(json.dumps(result, separators=(",", ":")))
PY

[[ ! -e "$ROOT" ]]
printf 'FIXTURE_CLEANUP_OK=1\n'
