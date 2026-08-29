#!/usr/bin/env bash
# Build a filtered, disposable GitHub publication snapshot.
#
# The working/deployment checkout deliberately keeps tests, verification
# fixtures, and operational documentation.  This script copies that checkout
# to a temporary git repository and removes files which are not part of the
# public source publication.  Nothing in the working tree or its manifest is
# deleted or rewritten.
set -Eeuo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
REMOTE_NAME="${GITHUB_REMOTE_NAME:-github}"
REMOTE_URL="${GITHUB_REMOTE_URL:-}"
PUBLISH_BRANCH="${GITHUB_PUBLISH_BRANCH:-main}"
CONFIRM="${PUBLISH_GITHUB_CONFIRM:-}"
DRY_RUN="${PUBLISH_DRY_RUN:-0}"
KEEP_WORK="${PUBLISH_KEEP_WORK:-0}"

command -v git >/dev/null 2>&1 || {
  printf 'ERROR: git is required to create the publication snapshot\n' >&2
  exit 1
}

WORK="$(mktemp -d "${TMPDIR:-/tmp}/codex-pool-github.XXXXXX")"
cleanup() {
  if [[ "$KEEP_WORK" == "1" || "$KEEP_WORK" == "true" || "$KEEP_WORK" == "TRUE" ]]; then
    printf 'publication snapshot retained: %s\n' "$WORK" >&2
    return
  fi
  rm -rf -- "$WORK"
}
trap cleanup EXIT

# Keep this list explicit.  In particular, never copy ignored credentials,
# local databases, runtime captures, or generated build directories into a
# publication repository.
if command -v rsync >/dev/null 2>&1; then
  rsync -a --delete \
    --exclude '.git/' \
    --exclude '.run/' \
    --exclude '.codex/' \
    --exclude '.agents/' \
    --exclude '.claude/' \
    --exclude '.build/' \
    --exclude '*.sqlite3' --exclude '*.sqlite3-*' --exclude '*.sqlite3-shm' --exclude '*.sqlite3-wal' \
    --exclude '*.log' --exclude '*.env' --exclude '.env' --exclude '.env.*' \
    --exclude '.upcloud-deploy-context' --exclude '1.txt' --exclude 'config.toml' \
    --exclude 'data/' --exclude 'diagnostics/' \
    --exclude 'auth.json' --exclude 'passwd.txt' --exclude '*.secret' --exclude '*.secrets' \
    --include 'config.example.json' --include 'config.lifecycle.json' --exclude 'config.*.json' \
    --exclude 'web-spa/node_modules/' --exclude 'web-spa/dist/' \
    --exclude 'example_zip/' --exclude 'other/' \
    --exclude 'archive/' --exclude 'bin/' --exclude 'third_party/' --exclude 'var/' \
    --exclude 'test/' --exclude 'tests/' --exclude 'testdata/' \
    --exclude 'cmd/extreme-load/' \
    --exclude 'tools/acceptance/' --exclude 'tools/e2e/' --exclude 'tools/visual/' \
    --exclude '*_test.go' --exclude '**/testdata/**' \
    --exclude '*_test.py' --exclude 'test_*.py' \
    --exclude '*_test.*' --exclude 'test_*.*' \
    --exclude 'test-*.*' --exclude '*-test.*' --exclude '*selftest*' \
    --exclude 'playwright.config.*' --exclude 'vitest.config.*' --exclude 'jest.config.*' \
    --exclude 'scripts/ci.sh' \
    --exclude '*.test.js' --exclude '*.test.jsx' \
    --exclude '*.test.ts' --exclude '*.test.tsx' \
    --exclude '*.test.mjs' --exclude '*.spec.js' --exclude '*.spec.jsx' \
    --exclude '*.spec.ts' --exclude '*.spec.tsx' --exclude '*.spec.mjs' \
    --exclude '*_spec.*' \
    --exclude 'docs/' --exclude 'verification/' \
    --exclude 'legacy-cache-hit-optimization/' --exclude 'artifacts/' \
    --exclude 'gpt-*.md' --exclude 'write_*.md' \
    "$ROOT/" "$WORK/"
else
  # Minimal Python fallback keeps the publisher usable on stripped-down VPS
  # images where rsync is not installed.  It follows the same exclusion set.
  command -v python3 >/dev/null 2>&1 || {
    printf 'ERROR: install rsync (or python3) to create the publication snapshot\n' >&2
    exit 1
  }
  python3 - "$ROOT" "$WORK" <<'PY'
import fnmatch
import os
import shutil
import sys

root, work = (os.path.realpath(p) for p in sys.argv[1:3])
excluded_dirs = {
    ".git", ".run", ".codex", ".agents", ".claude", ".build", "node_modules",
    "archive", "bin", "example_zip", "other", "third_party", "var",
    "docs", "data", "diagnostics", "verification", "artifacts",
    "test", "tests", "testdata", "acceptance", "e2e", "visual", "extreme-load",
    "legacy-cache-hit-optimization", "testdata",
}
excluded_names = {"auth.json", "passwd.txt", ".upcloud-deploy-context", "1.txt", "config.toml"}
excluded_suffixes = (".sqlite3", ".sqlite3-shm", ".sqlite3-wal", ".sqlite3-journal", ".log", ".env", ".secret", ".secrets")
excluded_globs = ("*_test.*", "test_*.*", "test-*.*", "*-test.*", "*selftest*", "playwright.config.*", "vitest.config.*", "jest.config.*", "*_spec.*", "*.test.js", "*.test.jsx", "*.test.ts", "*.test.tsx",
                  "*.test.mjs", "*.spec.js", "*.spec.jsx", "*.spec.ts", "*.spec.tsx", "*.spec.mjs",
                  ".env.*", "config.*.json", "gpt-*.md", "write_*.md")

def excluded(rel, name, is_dir):
    parts = rel.split(os.sep)
    if any(part in excluded_dirs for part in parts if part):
        return True
    if rel == "web-spa/dist" or rel.startswith("web-spa/dist" + os.sep):
        return True
    if name in excluded_names or ".sqlite3" in name or any(name.endswith(s) for s in excluded_suffixes):
        return True
    if name in {"config.example.json", "config.lifecycle.json"}:
        return False
    if rel == "scripts/ci.sh":
        return True
    return any(fnmatch.fnmatch(name, pat) or fnmatch.fnmatch(rel, pat) for pat in excluded_globs)

for dirpath, dirnames, filenames in os.walk(root, topdown=True, followlinks=False):
    rel_dir = os.path.relpath(dirpath, root)
    if rel_dir == ".":
        rel_dir = ""
    dirnames[:] = [d for d in dirnames if not excluded(os.path.join(rel_dir, d), d, True)]
    target_dir = os.path.join(work, rel_dir)
    os.makedirs(target_dir, exist_ok=True)
    for name in filenames:
        rel = os.path.join(rel_dir, name)
        if excluded(rel, name, False):
            continue
        src, dst = os.path.join(root, rel), os.path.join(work, rel)
        os.makedirs(os.path.dirname(dst), exist_ok=True)
        if os.path.islink(src):
            os.symlink(os.readlink(src), dst)
        else:
            shutil.copy2(src, dst)
PY
fi

# Only README.md is a public top-level markdown document.  The plan deliberately
# keeps the deployment checkout's operational documentation private; nested
# Markdown files are therefore removed too, with the single root README restored
# from the source checkout.  This also covers README files in verification or
# vendored skill copies if an exclusion is ever missed above.
find "$WORK" -type f -name '*.md' ! -path "$WORK/README.md" -delete
find "$WORK" -maxdepth 1 -type f -name '*.md' ! -name 'README.md' -delete

git -C "$WORK" init -q
git -C "$WORK" config user.name "codex-pool publisher"
git -C "$WORK" config user.email "codex-pool-publisher@localhost"
git -C "$WORK" add -A
if git -C "$WORK" diff --cached --quiet; then
  printf 'ERROR: filtered publication snapshot is empty\n' >&2
  exit 1
fi
git -C "$WORK" commit -q -m "publish snapshot $(date -u +%Y%m%dT%H%M%SZ)"

# Always print a deterministic inventory.  This makes the filtering step
# auditable and allows CI/operators to verify the tree without network access.
printf 'publication snapshot: %s\n' "$WORK"
printf 'tracked files: %s\n' "$(git -C "$WORK" ls-files | wc -l | tr -d ' ')"
if git -C "$WORK" ls-files | grep -E '(^|/)([^/]*_test\.[^/]+|test[_-][^/]*\.[^/]+|[^/]*-test\.[^/]+|[^/]*_spec\.[^/]+|spec[_-][^/]*\.[^/]+|[^/]*-spec\.[^/]+|[^/]*\.(test|spec)\.[^/]+|[^/]*selftest[^/]*|(playwright|vitest|jest)\.config\.[^/]+|testdata/)|^scripts/ci\.sh$|(^|/)(archive|artifacts|docs|tests|third_party|var|verification)/|(^|/)legacy-cache-hit-optimization/' >/dev/null; then
  printf 'ERROR: filtered snapshot contains excluded paths\n' >&2
  exit 1
fi
if git -C "$WORK" ls-files | awk -F/ '$0 ~ /\.md$/ && $0 != "README.md" { found=1 } END { exit found ? 0 : 1 }'; then
  printf 'ERROR: filtered snapshot contains a non-README markdown file\n' >&2
  exit 1
fi

if [[ "$DRY_RUN" == "1" || "$DRY_RUN" == "true" || "$DRY_RUN" == "TRUE" ]]; then
  printf 'dry-run: no remote push performed\n'
  exit 0
fi

if [[ "$CONFIRM" != "YES" ]]; then
  printf 'ERROR: set PUBLISH_GITHUB_CONFIRM=YES after confirming the publication topology\n' >&2
  printf '       or set PUBLISH_DRY_RUN=1 to validate filtering without a push\n' >&2
  exit 2
fi

if [[ -n "$REMOTE_URL" ]]; then
  git -C "$WORK" remote add "$REMOTE_NAME" "$REMOTE_URL"
elif ! git -C "$ROOT" remote get-url "$REMOTE_NAME" >/dev/null 2>&1; then
  printf 'ERROR: remote %q is not configured; set GITHUB_REMOTE_URL or add it explicitly\n' "$REMOTE_NAME" >&2
  exit 2
else
  git -C "$WORK" remote add "$REMOTE_NAME" "$(git -C "$ROOT" remote get-url "$REMOTE_NAME")"
fi

# This force applies only to the disposable filtered publication branch, never
# to the deployment checkout or its origin branch.
git -C "$WORK" push --force "$REMOTE_NAME" "HEAD:${PUBLISH_BRANCH}"
printf 'published %s to %s/%s\n' "$WORK" "$REMOTE_NAME" "$PUBLISH_BRANCH"
