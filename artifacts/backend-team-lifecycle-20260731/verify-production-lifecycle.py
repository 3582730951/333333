#!/usr/bin/env python3
import json
import sys
import urllib.error
import urllib.request
from pathlib import Path


def stats(base_url: str, config_path: str) -> dict:
    config = json.loads(Path(config_path).read_text())
    request = urllib.request.Request(
        f"{base_url}/admin/team-lifecycle/stats",
        headers={"Authorization": f"Bearer {config['admin_token']}"},
    )
    with urllib.request.urlopen(request, timeout=10) as response:
        assert response.status == 200
        return json.load(response)


def unauthenticated_status(base_url: str) -> int:
    request = urllib.request.Request(f"{base_url}/admin/team-lifecycle/stats")
    try:
        urllib.request.urlopen(request, timeout=10)
    except urllib.error.HTTPError as error:
        return error.code
    return 200


main_url, main_config, frontend_url, frontend_config, output = sys.argv[1:6]
result = {
    "main": stats(main_url, main_config),
    "frontend": stats(frontend_url, frontend_config),
    "unauthenticated_status": unauthenticated_status(frontend_url),
}
assert result["unauthenticated_status"] == 401
assert result["main"]["credential_persistence"] == "encrypted_account_reference"
assert result["frontend"]["credential_persistence"] == "encrypted_account_reference"
Path(output).write_text(json.dumps(result, indent=2) + "\n")
print("PRODUCTION_LIFECYCLE_API_OK=1 UNAUTHENTICATED_STATUS=401")
