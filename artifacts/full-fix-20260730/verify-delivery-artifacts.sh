#!/usr/bin/env bash
set -Eeuo pipefail

# Execute this harness only inside the designated remote regression root:
#   /usr/local/bin/rtk taskset -c 190,191 nice -n 15 ionice -c3 \
#     bash artifacts/full-fix-20260730/verify-delivery-artifacts.sh \
#     "$REMOTE_REGRESSION_ROOT" \
#     "$REMOTE_REGRESSION_ROOT/artifacts/full-fix-20260730"

work_root="${1:?remote regression root is required}"
artifact_dir="${2:?artifact directory is required}"
work_root="$(cd -- "$work_root" && pwd)"
artifact_dir="$(cd -- "$artifact_dir" && pwd)"

case "$artifact_dir/" in
  "$work_root"/*) ;;
  *)
    printf >&2 'artifact directory must be inside the regression root\n'
    exit 64
    ;;
esac

required=(
  original-state.tar.gz
  original-state.sha256
  original-tree-manifest.jsonl
  original-tree.txt
  original-index-tree.txt
  modified-source.tar.gz
  modified-source.sha256
  modified-tree-manifest.jsonl
  modified-tree.txt
  baseline-to-final.patch
  baseline-to-final.patch.sha256
  rollback-baseline-scope.jsonl
  rollback-final-scope.jsonl
  rollback.sh
  binary-sha256.txt
  evidence-sha256.txt
  bin/pool-server-final-v7
  bin/gateway-final-v7
  verification-record.md
  delivery-sha256.txt
)
for name in "${required[@]}"; do
  [[ -f "$artifact_dir/$name" ]]
done

tmp="$(mktemp -d "$work_root/delivery-artifact-verify.XXXXXX")"
cleanup() {
  local rc=$?
  rm -rf -- "$tmp"
  return "$rc"
}
trap cleanup EXIT

printf 'REMOTE_ARTIFACT_HARNESS_VERSION=1\n'
allowed_cpus="$(awk '/^Cpus_allowed_list:/ {print $2}' /proc/self/status)"
nice_level="$(ps -o ni= -p $$ | tr -d '[:space:]')"
io_class="$(ionice -p $$)"
[[ "$allowed_cpus" == "190-191" ]]
[[ "$nice_level" =~ ^-?[0-9]+$ && "$nice_level" -ge 15 ]]
grep -qi 'idle' <<<"$io_class"
printf 'REMOTE_CPU_ALLOWED_LIST=%s\n' "$allowed_cpus"
printf 'REMOTE_NICE_LEVEL=%s\n' "$nice_level"
printf 'REMOTE_IO_CLASS=%s\n' "$io_class"
printf 'REMOTE_EXECUTION_POLICY=verified\n'
printf 'REMOTE_WORK_ROOT_SCOPE=verified\n'

(
  cd "$artifact_dir"
  sha256sum -c baseline-to-final.patch.sha256
  sha256sum -c original-state.sha256
  sha256sum -c modified-source.sha256
  sha256sum -c binary-sha256.txt
  sha256sum -c evidence-sha256.txt
  sha256sum -c delivery-sha256.txt
)
printf 'DELIVERY_SHA256_EXIT=0\n'

pool_self_test="$("$artifact_dir/bin/pool-server-final-v7" --self-test)"
[[ "$pool_self_test" == "codex-pool-server self-test ok" ]]
printf 'POOL_SERVER_SELF_TEST_OUTPUT=%s\n' "$pool_self_test"
printf 'POOL_SERVER_SELF_TEST_EXIT=0\n'

gateway_home="$tmp/gateway-home"
mkdir -p "$gateway_home"
set +e
gateway_usage="$(
  HOME="$gateway_home" "$artifact_dir/bin/gateway-final-v7" 2>&1
)"
gateway_status=$?
set -e
[[ "$gateway_status" -eq 1 ]]
grep -q '^Claude Gateway - Local MITM proxy for pool_server$' \
  <<<"$gateway_usage"
printf 'GATEWAY_USAGE_FIRST_LINE=%s\n' \
  "$(printf '%s\n' "$gateway_usage" | head -n 1)"
printf 'GATEWAY_USAGE_EXPECTED_EXIT=%s\n' "$gateway_status"
printf 'BINARY_SMOKE_EXIT=0\n'

bash -n "$artifact_dir/rollback.sh"
grep -q '^# Full Fix Verification Record' "$artifact_dir/verification-record.md"
printf 'ROLLBACK_SCRIPT_REOPEN_EXIT=0\n'
printf 'VERIFICATION_RECORD_REOPEN_EXIT=0\n'

mkdir -p "$tmp/original" "$tmp/modified"
tar -tzf "$artifact_dir/original-state.tar.gz" \
  > "$tmp/original-archive.contents"
tar -tzf "$artifact_dir/modified-source.tar.gz" \
  > "$tmp/modified-archive.contents"
printf 'ORIGINAL_ARCHIVE_LIST_EXIT=0\n'
printf 'MODIFIED_ARCHIVE_LIST_EXIT=0\n'

python3 - "$tmp/original-archive.contents" \
  "$tmp/modified-archive.contents" <<'PY'
import pathlib
import sys

for listing_name in sys.argv[1:]:
    listing = pathlib.Path(listing_name)
    for raw in listing.read_text(encoding="utf-8").splitlines():
        path = raw.removeprefix("./")
        parts = pathlib.PurePosixPath(path).parts
        lowered = path.lower()
        base = parts[-1].lower() if parts else ""
        if not parts or parts[0] != "source" or ".." in parts:
            raise SystemExit(f"archive member outside source prefix: {raw!r}")
        if any(part in {
            ".git", "artifacts", "verification", "node_modules", ".cache",
            "coverage", "__pycache__", ".pytest_cache",
        } for part in parts):
            raise SystemExit(f"excluded directory present in archive: {raw!r}")
        if len(parts) == 2 and (
            base == "api.test" or (base.startswith("gpt-") and base.endswith(".md"))
        ):
            raise SystemExit(f"excluded root file present in archive: {raw!r}")
        if base == ".env" or base.startswith(".env."):
            if base not in {".env.example", ".env.sample", ".env.template"}:
                raise SystemExit(f"secret-convention path present: {raw!r}")
        if base in {
            "id_rsa", "id_dsa", "id_ecdsa", "id_ed25519",
            "credentials.json", "credential.json", "secrets.json", "secret.json",
        }:
            raise SystemExit(f"secret-convention path present: {raw!r}")
        if lowered.endswith((".pem", ".p12", ".pfx", ".keystore")):
            raise SystemExit(f"secret-convention path present: {raw!r}")
print("ARCHIVE_EXCLUSION_POLICY=verified")
PY

tar -xzf "$artifact_dir/original-state.tar.gz" -C "$tmp/original"
tar -xzf "$artifact_dir/modified-source.tar.gz" -C "$tmp/modified"
printf 'ORIGINAL_ARCHIVE_EXTRACT_EXIT=0\n'
printf 'MODIFIED_ARCHIVE_EXTRACT_EXIT=0\n'

verify_manifest() {
  python3 - "$1" "$2" <<'PY'
import hashlib
import json
import os
import pathlib
import stat
import sys

root = pathlib.Path(sys.argv[1]).resolve()
manifest = pathlib.Path(sys.argv[2])
expected_paths = set()
for line_number, line in enumerate(
    manifest.read_text(encoding="utf-8").splitlines(), 1
):
    expected = json.loads(line)
    rel = expected["path"]
    expected_paths.add(rel)
    path = root / rel
    try:
        info = path.lstat()
    except FileNotFoundError:
        raise SystemExit(f"manifest path absent at line {line_number}: {rel}")
    mode = format(stat.S_IMODE(info.st_mode), "04o")
    if stat.S_ISLNK(info.st_mode):
        raw = os.fsencode(os.readlink(path))
        state = "symlink"
    elif stat.S_ISREG(info.st_mode):
        raw = path.read_bytes()
        state = "file"
    else:
        raise SystemExit(f"unsupported manifest path type: {rel}")
    actual = {
        "path": rel,
        "state": state,
        "mode": mode,
        "size": len(raw),
        "sha256": hashlib.sha256(raw).hexdigest(),
    }
    if actual != expected:
        raise SystemExit(
            f"manifest mismatch at line {line_number}: "
            f"expected={expected!r} actual={actual!r}"
        )

actual_paths = set()
for current, dirs, files in os.walk(root, topdown=True, followlinks=False):
    dirs[:] = sorted(d for d in dirs if d != ".git")
    for name in files:
        actual_paths.add((pathlib.Path(current) / name).relative_to(root).as_posix())
    for name in list(dirs):
        path = pathlib.Path(current) / name
        if path.is_symlink():
            actual_paths.add(path.relative_to(root).as_posix())
            dirs.remove(name)
if actual_paths != expected_paths:
    raise SystemExit(
        f"manifest path-set mismatch: missing={sorted(expected_paths-actual_paths)!r} "
        f"extra={sorted(actual_paths-expected_paths)!r}"
    )
print(f"MANIFEST_FILES={len(actual_paths)}")
print("MANIFEST_SHA256=verified")
PY
}

verify_manifest "$tmp/original/source" \
  "$artifact_dir/original-tree-manifest.jsonl"
printf 'ORIGINAL_MANIFEST_EXIT=0\n'
verify_manifest "$tmp/modified/source" \
  "$artifact_dir/modified-tree-manifest.jsonl"
printf 'MODIFIED_MANIFEST_EXIT=0\n'

tree_of() {
  local directory="$1"
  git -C "$directory" init -q
  git -C "$directory" add -A
  git -C "$directory" write-tree
}

expected_original_tree="$(tr -d '[:space:]' < "$artifact_dir/original-tree.txt")"
expected_modified_tree="$(tr -d '[:space:]' < "$artifact_dir/modified-tree.txt")"
reopened_original_tree="$(tree_of "$tmp/original/source")"
reopened_modified_tree="$(tree_of "$tmp/modified/source")"
[[ "$reopened_original_tree" == "$expected_original_tree" ]]
[[ "$reopened_modified_tree" == "$expected_modified_tree" ]]
printf 'ORIGINAL_EXPECTED_TREE=%s\n' "$expected_original_tree"
printf 'ORIGINAL_REOPENED_TREE=%s\n' "$reopened_original_tree"
printf 'MODIFIED_EXPECTED_TREE=%s\n' "$expected_modified_tree"
printf 'MODIFIED_REOPENED_TREE=%s\n' "$reopened_modified_tree"

patch_tree="$tmp/patch-application"
cp -a -- "$tmp/original/source" "$patch_tree"
git -C "$patch_tree" init -q
git -C "$patch_tree" config user.name delivery-verifier
git -C "$patch_tree" config user.email delivery-verifier@invalid
git -C "$patch_tree" add -A
git -C "$patch_tree" commit -qm original
git -C "$patch_tree" apply --check --binary \
  "$artifact_dir/baseline-to-final.patch"
printf 'PATCH_CHECK_EXIT=0\n'
git -C "$patch_tree" apply --binary "$artifact_dir/baseline-to-final.patch"
git -C "$patch_tree" add -A
patch_applied_tree="$(git -C "$patch_tree" write-tree)"
[[ "$patch_applied_tree" == "$expected_modified_tree" ]]
printf 'PATCH_APPLY_EXIT=0\n'
printf 'PATCH_APPLIED_TREE=%s\n' "$patch_applied_tree"

rollback_output="$(
  ROLLBACK_REPORT_REPO=REMOTE_ISOLATED_FIXTURE \
    "$artifact_dir/rollback.sh" "$patch_tree"
)"
printf '%s\n' "$rollback_output"
git -C "$patch_tree" add -A
rollback_tree="$(git -C "$patch_tree" write-tree)"
[[ "$rollback_tree" == "$expected_original_tree" ]]
printf 'ROLLBACK_TREE=%s\n' "$rollback_tree"

git -C "$patch_tree" apply --check --binary \
  "$artifact_dir/baseline-to-final.patch"
printf 'ROLLBACK_FORWARD_PREFLIGHT_EXIT=0\n'
git -C "$patch_tree" apply --binary \
  "$artifact_dir/baseline-to-final.patch"
git -C "$patch_tree" add -A
reapplied_tree="$(git -C "$patch_tree" write-tree)"
[[ "$reapplied_tree" == "$expected_modified_tree" ]]
printf 'ROLLBACK_REAPPLY_EXIT=0\n'
printf 'ROLLBACK_REAPPLIED_TREE=%s\n' "$reapplied_tree"

sha256sum \
  "$artifact_dir/modified-source.tar.gz" \
  "$artifact_dir/baseline-to-final.patch" \
  "$artifact_dir/rollback.sh" \
  "$artifact_dir/original-state.tar.gz" \
  "$artifact_dir/bin/pool-server-final-v7" \
  "$artifact_dir/bin/gateway-final-v7"
printf 'VERIFICATION_RECORD_SHA256=verified-by-delivery-manifest\n'
printf 'FOUR_ROLES_REOPENED=verified\n'
printf 'REMOTE_ARTIFACT_VERIFICATION_EXIT=0\n'
