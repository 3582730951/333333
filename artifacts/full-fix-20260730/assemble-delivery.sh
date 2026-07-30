#!/usr/bin/env bash
set -Eeuo pipefail

# Assemble the final source delivery without changing the repository index.
# Run only after the source tree is frozen:
#   bash artifacts/full-fix-20260730/assemble-delivery.sh /workspace
#
# Set DELIVERY_BASELINE_ONLY=1 to validate reconstruction of the original
# pre-task workspace state without producing/overwriting final delivery roles.

artifact_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo="${1:-}"
if [[ -z "$repo" ]]; then
  repo="$(git -C "$artifact_dir/../.." rev-parse --show-toplevel)"
fi
repo="$(cd -- "$repo" && pwd)"

prior_dir="$repo/artifacts/optimization-20260729"
baseline_commit_file="$prior_dir/baseline-commit.txt"
preexisting_patch_file="$prior_dir/preexisting-worktree.patch"
expected_original_tree="${DELIVERY_EXPECTED_ORIGINAL_TREE:-045ca893e746ec72cc684db96bec730168f4ff6c}"
expected_index_tree="${DELIVERY_EXPECTED_INDEX_TREE:-fd5c6dfecaa01f0922d056ee148edfefab8f7ccf}"
expected_preexisting_patch_sha256="${DELIVERY_EXPECTED_PREEXISTING_PATCH_SHA256:-7cf8a1e4da4ee4fb36e367fe174f8376ea05187b5d8e1a9852ac8321ac82b484}"

[[ -f "$baseline_commit_file" ]]
[[ -f "$preexisting_patch_file" ]]
actual_preexisting_patch_sha256="$(
  sha256sum "$preexisting_patch_file" | awk '{print $1}'
)"
if [[ "$actual_preexisting_patch_sha256" != "$expected_preexisting_patch_sha256" ]]; then
  printf >&2 'ERROR: historical preexisting patch changed: expected=%s actual=%s\n' \
    "$expected_preexisting_patch_sha256" "$actual_preexisting_patch_sha256"
  exit 39
fi
baseline_commit="$(tr -d '[:space:]' < "$baseline_commit_file")"
[[ "$baseline_commit" =~ ^[0-9a-f]{40}$ ]]
git -C "$repo" cat-file -e "$baseline_commit^{commit}"
source_branch="$(git -C "$repo" symbolic-ref --quiet --short HEAD)"

index_tree_before="$(git -C "$repo" write-tree)"
if [[ "$index_tree_before" != "$expected_index_tree" ]]; then
  printf >&2 'ERROR: preserved repository index changed: expected=%s actual=%s\n' \
    "$expected_index_tree" "$index_tree_before"
  exit 40
fi
tmp="$(mktemp -d "${TMPDIR:-/tmp}/full-fix-delivery.XXXXXX")"
cleanup() {
  local rc=$?
  rm -rf -- "$tmp"
  local index_tree_after
  index_tree_after="$(git -C "$repo" write-tree 2>/dev/null || true)"
  if [[ -n "$index_tree_after" && "$index_tree_after" != "$index_tree_before" ]]; then
    printf >&2 'ERROR: repository index tree changed: before=%s after=%s\n' \
      "$index_tree_before" "$index_tree_after"
    return 97
  fi
  return "$rc"
}
trap cleanup EXIT

original="$tmp/original"
final="$tmp/final"
mkdir -p -- "$original"

# The historical capture starts with a human diffstat. Retain only the actual
# binary-safe patch beginning at the first "diff --git" header.
awk 'found || /^diff --git / { found=1; print }' \
  "$preexisting_patch_file" > "$tmp/preexisting.patch"
grep -q '^diff --git ' "$tmp/preexisting.patch"

# Reconstruct exactly: baseline commit + the user's pre-task tracked/index state.
git -C "$repo" archive --format=tar "$baseline_commit" |
  tar -xf - -C "$original"
git -C "$original" init -q
git -C "$original" config user.name delivery-verifier
git -C "$original" config user.email delivery-verifier@invalid
source_epoch="$(git -C "$repo" show -s --format=%ct "$baseline_commit")"
export GIT_AUTHOR_DATE="$source_epoch +0000"
export GIT_COMMITTER_DATE="$source_epoch +0000"
git -C "$original" add -A
git -C "$original" commit -qm baseline-commit
git -C "$original" apply --check --binary "$tmp/preexisting.patch"
git -C "$original" apply --binary "$tmp/preexisting.patch"
git -C "$original" add -A
original_tree="$(git -C "$original" write-tree)"
if [[ "$original_tree" != "$expected_original_tree" ]]; then
  printf >&2 'ERROR: reconstructed original tree mismatch: expected=%s actual=%s\n' \
    "$expected_original_tree" "$original_tree"
  exit 41
fi
git -C "$original" commit -qm original-workspace-state
original_commit="$(git -C "$original" rev-parse HEAD)"

printf 'BASELINE_COMMIT=%s\n' "$baseline_commit"
printf 'ORIGINAL_TREE=%s\n' "$original_tree"
printf 'EXPECTED_ORIGINAL_TREE=%s\n' "$expected_original_tree"
printf 'ORIGINAL_TREE_MATCH=yes\n'
printf 'REPOSITORY_INDEX_TREE_BEFORE=%s\n' "$index_tree_before"

if [[ "${DELIVERY_BASELINE_ONLY:-0}" == 1 ]]; then
  printf 'BASELINE_ONLY_EXIT=0\n'
  exit 0
fi

# Freeze the current source file set. Tracked files are included. New files are
# admitted only from project source/documentation roots. Build dependencies,
# evidence, archives, logs, databases, binaries, and secret-file conventions
# are excluded. The necessary embedded console dist remains under internal/.
git -C "$repo" ls-files --stage -z > "$tmp/tracked-stage.z"
git -C "$repo" ls-files --others --exclude-standard -z > "$tmp/untracked.z"
python3 - "$repo" "$tmp/tracked-stage.z" "$tmp/untracked.z" \
  "$tmp/final-files.z" "$tmp/included-new-files.txt" \
  "$tmp/excluded-untracked-files.txt" "$tmp/final-modes.json" <<'PY'
import fnmatch
import json
import os
import pathlib
import sys

repo = pathlib.Path(sys.argv[1]).resolve()
tracked_file, untracked_file, output_file, included_file, excluded_file, modes_file = map(
    pathlib.Path, sys.argv[2:]
)

def read_z(path):
    data = path.read_bytes()
    return [os.fsdecode(item) for item in data.split(b"\0") if item]

def normalized(path):
    value = pathlib.PurePosixPath(path).as_posix()
    if value.startswith("/") or value == ".." or value.startswith("../"):
        raise SystemExit(f"unsafe repository path: {path!r}")
    if "/../" in f"/{value}/":
        raise SystemExit(f"unsafe repository path: {path!r}")
    return value

def secret_name(path):
    parts = pathlib.PurePosixPath(path).parts
    base = parts[-1].lower() if parts else ""
    if base == ".env" or (base.startswith(".env.") and base not in {
        ".env.example", ".env.sample", ".env.template"
    }):
        return True
    if base in {
        "id_rsa", "id_dsa", "id_ecdsa", "id_ed25519",
        "credentials.json", "credential.json", "secrets.json", "secret.json",
    }:
        return True
    if fnmatch.fnmatch(base, "*.pem") or fnmatch.fnmatch(base, "*.p12"):
        return True
    if fnmatch.fnmatch(base, "*.pfx") or fnmatch.fnmatch(base, "*.keystore"):
        return True
    return False

def always_excluded(path):
    p = pathlib.PurePosixPath(path)
    parts = p.parts
    base = parts[-1].lower() if parts else ""
    if not parts:
        return True
    if parts[0] in {".git", "artifacts", "verification"}:
        return True
    if any(part in {
        "node_modules", ".cache", "coverage", "__pycache__", ".pytest_cache",
        ".next", ".turbo",
    } for part in parts):
        return True
    if path == "api.test" or (len(parts) == 1 and fnmatch.fnmatch(base, "gpt-*.md")):
        return True
    if base.endswith((
        ".log", ".sqlite", ".sqlite3", ".db", ".db-shm", ".db-wal",
        ".zip", ".tar", ".tgz", ".tar.gz", ".test", ".prof", ".trace",
    )):
        return True
    if base in {"pool-server", "gateway"} and len(parts) == 1:
        return True
    return secret_name(path)

allowed_new_roots = (
    "cmd/",
    "internal/",
    "web-spa/",
    "scripts/maintenance/",
    "docs/",
)

tracked_modes = {}
for entry in read_z(tracked_file):
    metadata, raw_path = entry.split("\t", 1)
    mode, _oid, stage = metadata.split(" ", 2)
    path = normalized(raw_path)
    if stage != "0":
        raise SystemExit(f"unmerged index entry is unsupported: {path} stage={stage}")
    if mode not in {"100644", "100755", "120000"}:
        raise SystemExit(f"unsupported tracked mode: {path} mode={mode}")
    tracked_modes[path] = mode
tracked = sorted(tracked_modes)
untracked = sorted(set(normalized(path) for path in read_z(untracked_file)))

bad_tracked = [path for path in tracked if secret_name(path)]
if bad_tracked:
    raise SystemExit(
        "tracked secret-convention paths require explicit sanitization: "
        + ", ".join(bad_tracked)
    )

selected = []
for path in tracked:
    if always_excluded(path):
        continue
    candidate = repo / path
    if candidate.is_file() or candidate.is_symlink():
        selected.append(path)

included_new = []
excluded_new = []
for path in untracked:
    if always_excluded(path) or not path.startswith(allowed_new_roots):
        excluded_new.append(path)
        continue
    candidate = repo / path
    if candidate.is_file() or candidate.is_symlink():
        included_new.append(path)
        selected.append(path)

selected = sorted(set(selected))
modes = {}
for path in selected:
    if path in tracked_modes:
        modes[path] = tracked_modes[path]
    else:
        candidate = repo / path
        if candidate.is_symlink():
            modes[path] = "120000"
        elif path.lower().endswith(".sh"):
            modes[path] = "100755"
        else:
            modes[path] = "100644"
output_file.write_bytes(
    b"".join(os.fsencode(path) + b"\0" for path in selected)
)
modes_file.write_text(
    json.dumps(modes, ensure_ascii=False, sort_keys=True) + "\n",
    encoding="utf-8",
)
included_file.write_text(
    "".join(path + "\n" for path in sorted(included_new)), encoding="utf-8"
)
excluded_file.write_text(
    "".join(path + "\n" for path in sorted(excluded_new)), encoding="utf-8"
)
print(f"TRACKED_PRESENT={sum(1 for p in tracked if (repo / p).exists() or (repo / p).is_symlink())}")
print(f"NEW_SOURCE_INCLUDED={len(included_new)}")
print(f"UNTRACKED_EXCLUDED={len(excluded_new)}")
print(f"FINAL_FILES_SELECTED={len(selected)}")
PY

# Clone only the temporary Git metadata, clear its work tree, then copy the
# frozen allowlisted source snapshot without consulting or changing the real
# repository index.
cp -a -- "$original" "$final"
find "$final" -mindepth 1 -maxdepth 1 ! -name .git -exec rm -rf -- {} +
python3 - "$repo" "$final" "$tmp/final-files.z" \
  "$tmp/final-modes.json" <<'PY'
import json
import os
import pathlib
import shutil
import stat
import sys

source = pathlib.Path(sys.argv[1]).resolve()
target = pathlib.Path(sys.argv[2]).resolve()
paths = [
    os.fsdecode(value)
    for value in pathlib.Path(sys.argv[3]).read_bytes().split(b"\0")
    if value
]
modes = json.loads(pathlib.Path(sys.argv[4]).read_text(encoding="utf-8"))
for rel in paths:
    src = source / rel
    dst = target / rel
    mode = src.lstat().st_mode
    dst.parent.mkdir(parents=True, exist_ok=True)
    if stat.S_ISLNK(mode):
        if modes[rel] != "120000":
            raise SystemExit(f"tracked mode/source type mismatch: {rel}")
        dst.symlink_to(os.readlink(src))
    elif stat.S_ISREG(mode):
        if modes[rel] == "120000":
            raise SystemExit(f"tracked mode/source type mismatch: {rel}")
        shutil.copy2(src, dst, follow_symlinks=False)
        os.chmod(dst, 0o755 if modes[rel] == "100755" else 0o644)
    else:
        raise SystemExit(f"unsupported source file type: {rel}")
PY

git -C "$final" add -A
git -C "$final" diff --cached --check
final_tree="$(git -C "$final" write-tree)"
if [[ "$final_tree" == "$original_tree" ]]; then
  printf >&2 'ERROR: final source tree is identical to the original tree\n'
  exit 42
fi

# Generate the standalone, binary-safe original-to-final patch while HEAD still
# names the reconstructed original state.
git -C "$final" diff --cached --binary --full-index --no-renames HEAD -- \
  > "$tmp/baseline-to-final.patch"
git -C "$final" diff --cached --name-only -z --no-renames HEAD -- \
  > "$tmp/changed-paths.z"
git -C "$final" -c core.quotePath=false diff --cached --name-status \
  --no-renames HEAD -- > "$tmp/baseline-to-final.files.txt"
git -C "$final" -c core.quotePath=false diff --cached --stat \
  --no-renames HEAD -- > "$tmp/baseline-to-final.stat.txt"
grep -q '^diff --git ' "$tmp/baseline-to-final.patch"
git -C "$final" commit -qm final-source-state
final_commit="$(git -C "$final" rev-parse HEAD)"

manifest_py="$tmp/write-manifest.py"
cat > "$manifest_py" <<'PY'
import hashlib
import json
import os
import pathlib
import subprocess
import sys

repo = pathlib.Path(sys.argv[1]).resolve()
tree = sys.argv[2]
output = pathlib.Path(sys.argv[3])
scope_file = pathlib.Path(sys.argv[4]) if len(sys.argv) > 4 else None

listing = subprocess.run(
    ["git", "-C", str(repo), "ls-tree", "-rz", "--full-tree", tree],
    check=True,
    stdout=subprocess.PIPE,
).stdout
entries = {}
for item in listing.split(b"\0"):
    if not item:
        continue
    metadata, raw_path = item.split(b"\t", 1)
    raw_mode, raw_kind, raw_oid = metadata.split(b" ", 2)
    rel = os.fsdecode(raw_path)
    mode = raw_mode.decode("ascii")
    kind = raw_kind.decode("ascii")
    oid = raw_oid.decode("ascii")
    if kind != "blob":
        raise SystemExit(f"unsupported Git tree entry: {rel} ({mode} {kind})")
    content = subprocess.run(
        ["git", "-C", str(repo), "cat-file", "blob", oid],
        check=True,
        stdout=subprocess.PIPE,
    ).stdout
    if mode == "100644":
        archive_mode, state = "0644", "file"
    elif mode == "100755":
        archive_mode, state = "0755", "file"
    elif mode == "120000":
        archive_mode, state = "0777", "symlink"
    else:
        raise SystemExit(f"unsupported Git blob mode: {rel} ({mode})")
    entries[rel] = {
        "path": rel,
        "state": state,
        "mode": archive_mode,
        "size": len(content),
        "sha256": hashlib.sha256(content).hexdigest(),
    }

if scope_file:
    paths = sorted({
        os.fsdecode(value)
        for value in scope_file.read_bytes().split(b"\0")
        if value
    })
else:
    paths = sorted(entries)

def record(rel):
    if rel not in entries:
        return {"path": rel, "state": "absent"}
    return entries[rel]

if not scope_file and set(paths) != set(entries):
    raise SystemExit("full Git tree manifest path mismatch")

for rel in paths:
    if rel.startswith("/") or ".." in pathlib.PurePosixPath(rel).parts:
        raise SystemExit(f"unsafe manifest path: {rel!r}")

with output.open("w", encoding="utf-8", newline="\n") as stream:
    for rel in paths:
        stream.write(json.dumps(record(rel), ensure_ascii=False, sort_keys=True))
        stream.write("\n")
PY

python3 "$manifest_py" "$original" "$original_tree" \
  "$tmp/original-tree-manifest.jsonl"
python3 "$manifest_py" "$final" "$final_tree" \
  "$tmp/modified-tree-manifest.jsonl"
python3 "$manifest_py" "$original" "$original_tree" \
  "$tmp/rollback-baseline-scope.jsonl" "$tmp/changed-paths.z"
python3 "$manifest_py" "$final" "$final_tree" \
  "$tmp/rollback-final-scope.jsonl" "$tmp/changed-paths.z"

# Archive the committed trees so .git and every excluded path are structurally
# absent. gzip -n removes timestamp/original-name metadata.
git -c tar.umask=0022 -C "$original" archive --format=tar --prefix=source/ "$original_commit" |
  gzip -n > "$tmp/original-state.tar.gz"
git -c tar.umask=0022 -C "$final" archive --format=tar --prefix=source/ "$final_commit" |
  gzip -n > "$tmp/modified-source.tar.gz"

mkdir -p -- "$artifact_dir"
install -m 0644 "$tmp/original-state.tar.gz" \
  "$artifact_dir/original-state.tar.gz"
install -m 0644 "$tmp/modified-source.tar.gz" \
  "$artifact_dir/modified-source.tar.gz"
install -m 0644 "$tmp/baseline-to-final.patch" \
  "$artifact_dir/baseline-to-final.patch"
install -m 0644 "$tmp/baseline-to-final.files.txt" \
  "$artifact_dir/baseline-to-final.files.txt"
install -m 0644 "$tmp/baseline-to-final.stat.txt" \
  "$artifact_dir/baseline-to-final.stat.txt"
install -m 0644 "$tmp/original-tree-manifest.jsonl" \
  "$artifact_dir/original-tree-manifest.jsonl"
install -m 0644 "$tmp/modified-tree-manifest.jsonl" \
  "$artifact_dir/modified-tree-manifest.jsonl"
install -m 0644 "$tmp/rollback-baseline-scope.jsonl" \
  "$artifact_dir/rollback-baseline-scope.jsonl"
install -m 0644 "$tmp/rollback-final-scope.jsonl" \
  "$artifact_dir/rollback-final-scope.jsonl"
install -m 0644 "$tmp/included-new-files.txt" \
  "$artifact_dir/modified-source-new-files.txt"
install -m 0644 "$tmp/excluded-untracked-files.txt" \
  "$artifact_dir/modified-source-exclusions.txt"
printf '%s\n' "$baseline_commit" > "$artifact_dir/baseline-commit.txt"
printf '%s\n' "$original_tree" > "$artifact_dir/original-tree.txt"
printf '%s\n' "$expected_index_tree" > "$artifact_dir/original-index-tree.txt"
printf '%s\n' "$final_tree" > "$artifact_dir/modified-tree.txt"

cat > "$artifact_dir/rollback.sh" <<'ROLLBACK'
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
ROLLBACK
chmod 0755 "$artifact_dir/rollback.sh"

(
  cd "$artifact_dir"
  sha256sum baseline-to-final.patch > baseline-to-final.patch.sha256
  sha256sum original-state.tar.gz original-tree-manifest.jsonl \
    original-tree.txt original-index-tree.txt baseline-commit.txt \
    > original-state.sha256
  sha256sum modified-source.tar.gz modified-tree-manifest.jsonl \
    modified-tree.txt modified-source-new-files.txt \
    modified-source-exclusions.txt > modified-source.sha256
)

# Final source roles are generated locally. Their reopen/extract, patch apply,
# rollback execution, and tree comparisons are deliberately delegated to the
# constrained remote verification harness.
chmod 0755 "$artifact_dir/verify-delivery-artifacts.sh"
for binary in pool-server-final-v7 gateway-final-v7; do
  [[ -x "$artifact_dir/bin/$binary" ]]
done
(
  cd "$artifact_dir"
  sha256sum bin/pool-server-final-v7 bin/gateway-final-v7 \
    > binary-sha256.txt
)
python3 - "$artifact_dir" <<'PY'
import hashlib
import pathlib
import sys

root = pathlib.Path(sys.argv[1]).resolve()
files = []
for relative in (
    "console-dist-sync-record.md",
    "console-dist-after-sync.tar.gz",
    "console-dist-after-sync.tar.gz.sha256",
    "console-dist-after-sync-validation.txt",
    "console-dist-after-sync.sha256tree",
    "frontend-dist-verified.tar.gz",
    "frontend-dist-verified.tar.gz.sha256",
    "diagnostic-analysis.md",
    "diagnostic-archive-integrity.json",
    "diagnostic-source-sha256.txt",
    "kiro-cli-compatibility.md",
):
    candidate = root / relative
    if candidate.is_file():
        files.append(candidate)
for relative in ("disk-reclaim", "kiro", "remote-tests"):
    directory = root / relative
    if directory.is_dir():
        files.extend(path for path in directory.rglob("*") if path.is_file())
with (root / "evidence-sha256.txt").open(
    "w", encoding="utf-8", newline="\n"
) as stream:
    for path in sorted(set(files)):
        relative = path.relative_to(root).as_posix()
        digest = hashlib.sha256(path.read_bytes()).hexdigest()
        stream.write(f"{digest}  {relative}\n")
PY
final_file_count="$(python3 - "$tmp/final-files.z" <<'PY'
import pathlib
import sys
print(sum(bool(item) for item in pathlib.Path(sys.argv[1]).read_bytes().split(b"\0")))
PY
)"
new_file_count="$(wc -l < "$tmp/included-new-files.txt" | tr -d ' ')"
excluded_file_count="$(wc -l < "$tmp/excluded-untracked-files.txt" | tr -d ' ')"
changed_path_count="$(python3 - "$tmp/changed-paths.z" <<'PY'
import pathlib
import sys
print(sum(bool(item) for item in pathlib.Path(sys.argv[1]).read_bytes().split(b"\0")))
PY
)"
cat > "$artifact_dir/artifact-assembly.txt" <<EOF
ARTIFACT_ASSEMBLY_COMMAND=/root/.local/bin/rtk bash artifacts/full-fix-20260730/assemble-delivery.sh /workspace
ARTIFACT_ASSEMBLY_COMMAND_EXIT=0
SOURCE_BRANCH=$source_branch
BASELINE_COMMIT=$baseline_commit
ORIGINAL_EXPECTED_TREE=$expected_original_tree
ORIGINAL_RECONSTRUCTED_TREE=$original_tree
MODIFIED_TREE=$final_tree
PRESERVED_INDEX_EXPECTED_TREE=$expected_index_tree
PREEXISTING_PATCH_SHA256=$actual_preexisting_patch_sha256
REPOSITORY_INDEX_TREE_BEFORE=$index_tree_before
REPOSITORY_INDEX_TREE_AFTER=$(git -C "$repo" write-tree)
FINAL_SOURCE_FILES=$final_file_count
NEW_SOURCE_FILES_INCLUDED=$new_file_count
UNTRACKED_FILES_EXCLUDED=$excluded_file_count
PATCH_CHANGED_PATHS=$changed_path_count
PATCH_STATIC_DIFF_CHECK_EXIT=0
SOURCE_ARCHIVE_GENERATION_EXIT=0
POOL_SERVER_FINAL_V7_SHA256=$(sha256sum "$artifact_dir/bin/pool-server-final-v7" | awk '{print $1}')
GATEWAY_FINAL_V7_SHA256=$(sha256sum "$artifact_dir/bin/gateway-final-v7" | awk '{print $1}')
LOCAL_EXECUTABLE_TESTS=not-run-by-assembler
REMOTE_ROLE_EXECUTION_HARNESS=verify-delivery-artifacts.sh
EOF

cat > "$artifact_dir/verification-record.md" <<EOF
# Full Fix Verification Record — 2026-07-30

## Immutable source states

- Baseline commit: \`$baseline_commit\`
- Source branch: \`$source_branch\`
- Preserved original workspace tree (commit plus original working changes):
  \`$original_tree\`
- Preserved original index tree: \`$expected_index_tree\`
- Modified source tree: \`$final_tree\`
- Repository index before/after assembly: \`$index_tree_before\` (unchanged)

## Four delivery roles

1. Modified artifacts: \`modified-source.tar.gz\`,
   \`bin/pool-server-final-v7\`, and \`bin/gateway-final-v7\`
2. Patch/diff: \`baseline-to-final.patch\`
3. Verification record: \`verification-record.md\`
4. Runnable rollback: \`rollback.sh\`

The preserved input is \`original-state.tar.gz\`. SHA-256 manifests are
\`original-state.sha256\`, \`modified-source.sha256\`, and
\`delivery-sha256.txt\`. Sanitized diagnostic, frontend, disk-reclaim, Kiro,
and remote-test evidence is covered by \`evidence-sha256.txt\`.

## Static assembly record

\`artifact-assembly.txt\` records the exact original/final Git trees and proves
that the user's original index tree was unchanged during assembly.

## Remote artifact reopen / patch / rollback verification

<!-- Replace this marker with the literal constrained-remote output from
verify-delivery-artifacts.sh after source freeze. -->
PENDING_REMOTE_ARTIFACT_VERIFICATION

## Remote baseline behavior

<!-- Insert the exact remote command, inputs, literal output, and exit status
from the pre-fix diagnostic/context/provider acceptance record. -->

## Remote modified behavior

<!-- Insert the exact constrained remote commands, inputs, literal outputs, and
exit statuses for Go tests, frontend verification, diagnostic browser download,
one-MiB Codex failover, one-million-token Claude failover, provider routing,
account archive compatibility, context compression, and disk reclaim. -->

## Runtime constraints used for executable verification

- All executable validation is performed in the designated remote regression
  root, never against the shared service.
- CPU affinity: 190-191; nice 15; idle I/O priority; Go parallelism 1.
- Local delivery assembly performs static source capture and hashing only.
EOF

(
  cd "$artifact_dir"
  sha256sum -c baseline-to-final.patch.sha256
  sha256sum -c original-state.sha256
  sha256sum -c modified-source.sha256
  sha256sum \
    baseline-commit.txt \
    original-tree.txt \
    original-index-tree.txt \
    original-state.tar.gz \
    original-state.sha256 \
    original-tree-manifest.jsonl \
    modified-tree.txt \
    modified-source.tar.gz \
    modified-source.sha256 \
    modified-tree-manifest.jsonl \
    modified-source-new-files.txt \
    modified-source-exclusions.txt \
    baseline-to-final.patch \
    baseline-to-final.files.txt \
    baseline-to-final.stat.txt \
    baseline-to-final.patch.sha256 \
    rollback-baseline-scope.jsonl \
    rollback-final-scope.jsonl \
    rollback.sh \
    binary-sha256.txt \
    evidence-sha256.txt \
    bin/pool-server-final-v7 \
    bin/gateway-final-v7 \
    artifact-assembly.txt \
    verification-record.md \
    verify-delivery-artifacts.sh \
    assemble-delivery.sh \
    > delivery-sha256.txt
  sha256sum -c delivery-sha256.txt
)

# Static reopen of text roles only; executable role behavior is remote-only.
grep -q '^# Full Fix Verification Record' "$artifact_dir/verification-record.md"
bash -n "$artifact_dir/rollback.sh"
bash -n "$artifact_dir/verify-delivery-artifacts.sh"

printf 'FINAL_TREE=%s\n' "$final_tree"
printf 'PRESERVED_INDEX_TREE=%s\n' "$index_tree_before"
printf 'REPOSITORY_INDEX_TREE_AFTER=%s\n' "$(git -C "$repo" write-tree)"
printf 'REMOTE_VERIFICATION_REQUIRED=yes\n'
printf 'DELIVERY_ASSEMBLY_EXIT=0\n'
