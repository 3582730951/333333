#!/usr/bin/env bash
set -Eeuo pipefail

AUTHORIZED_KEYS=/root/.ssh/authorized_keys
KEY_FILE=/root/.ssh/codex-session-key.remove.pub

python3 - "$AUTHORIZED_KEYS" "$KEY_FILE" <<'PY'
import os
import sys
from pathlib import Path

authorized_path = Path(sys.argv[1])
key_path = Path(sys.argv[2])
target = key_path.read_text().strip()
lines = authorized_path.read_text().splitlines()
matches = sum(line.strip() == target for line in lines)
if matches != 1:
    raise SystemExit(f"expected exactly one temporary key, found {matches}")
remaining = [line for line in lines if line.strip() != target]
temporary = authorized_path.with_suffix(".codex-cleanup")
temporary.write_text("\n".join(remaining) + ("\n" if remaining else ""))
os.chmod(temporary, 0o600)
os.replace(temporary, authorized_path)
print("REMOTE_TEMP_KEY_REMOVED=1")
PY

rm -f "$KEY_FILE" "$0"
