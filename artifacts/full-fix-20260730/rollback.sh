#!/usr/bin/env bash
set -Eeuo pipefail

artifact_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo="${1:-}"
if [[ -z "$repo" ]]; then
  repo="$(git -C "$artifact_dir/../.." rev-parse --show-toplevel)"
fi
repo="$(cd -- "$repo" && pwd)"
patch="$artifact_dir/baseline-to-final.patch"
manifest="$artifact_dir/rollback-baseline-scope.jsonl"
expected_patch_file="$artifact_dir/baseline-to-final.patch.sha256"

[[ -f "$patch" && -f "$manifest" && -f "$expected_patch_file" ]]
(cd "$artifact_dir" && sha256sum -c "$(basename "$expected_patch_file")")
git -C "$repo" apply --check --binary -R "$patch"

rolled_back=0
restore_final_on_error() {
  local rc=$?
  if [[ "$rc" -ne 0 && "$rolled_back" -eq 1 ]]; then
    git -C "$repo" apply --check --binary "$patch" >/dev/null 2>&1 &&
      git -C "$repo" apply --binary "$patch" >/dev/null 2>&1 || true
  fi
  return "$rc"
}
trap restore_final_on_error EXIT

git -C "$repo" apply --binary -R "$patch"
rolled_back=1
python3 - "$repo" "$manifest" <<'PY'
import hashlib
import json
import os
import pathlib
import stat
import sys

root = pathlib.Path(sys.argv[1]).resolve()
manifest = pathlib.Path(sys.argv[2])
for line_number, line in enumerate(
    manifest.read_text(encoding="utf-8").splitlines(), 1
):
    expected = json.loads(line)
    rel = expected["path"]
    path = root / rel
    try:
        info = path.lstat()
    except FileNotFoundError:
        actual = {"path": rel, "state": "absent"}
    else:
        if stat.S_ISLNK(info.st_mode):
            raw = os.fsencode(os.readlink(path))
            state = "symlink"
        elif stat.S_ISREG(info.st_mode):
            raw = path.read_bytes()
            state = "file"
        else:
            raise SystemExit(f"unsupported rollback path type: {rel}")
        actual = {
            "path": rel,
            "state": state,
            "size": len(raw),
            "sha256": hashlib.sha256(raw).hexdigest(),
        }
    expected_content = {
        key: value for key, value in expected.items() if key != "mode"
    }
    if actual != expected_content:
        raise SystemExit(
            f"rollback verification mismatch at line {line_number}: "
            f"expected={expected_content!r} actual={actual!r}"
        )
print("ROLLBACK_SCOPE_CONTENT=verified")
PY

rolled_back=0
trap - EXIT
printf 'ROLLBACK_EXIT=0\n'
printf 'ROLLBACK_REPO=%s\n' "${ROLLBACK_REPORT_REPO:-$repo}"
printf 'ROLLBACK_STATE=original\n'
