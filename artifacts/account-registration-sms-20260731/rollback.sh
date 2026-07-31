#!/usr/bin/env bash
set -euo pipefail

repo_root="$(git rev-parse --show-toplevel)"
patch="$repo_root/artifacts/account-registration-sms-20260731/source.patch"

test -s "$patch"
git -C "$repo_root" apply --check -R "$patch"
git -C "$repo_root" apply -R "$patch"
printf 'Rolled back source changes recorded in %s\n' "$patch"

