#!/usr/bin/env bash
set -Eeuo pipefail

ROOT=/root/autodl-tmp/cpupg-20260730
EVIDENCE="$ROOT/evidence"
ARCHIVE=/root/autodl-tmp/cpupg-evidence-20260730.tar.gz
rm -rf "$EVIDENCE"
mkdir -p "$EVIDENCE"/{install,seed,upgrade,rollback}

cp "$ROOT/logs/old-install.log" "$EVIDENCE/install/"
cp "$ROOT/logs/old-install.exit" "$EVIDENCE/install/"
cp "$ROOT/logs/stage1-summary.txt" "$EVIDENCE/install/"
cp "$ROOT/logs/old-ready.json" "$EVIDENCE/install/"

cp "$ROOT/logs/pre-upgrade-file-hashes.txt" "$EVIDENCE/seed/"
cp "$ROOT/logs/pre-upgrade-db-snapshot.json" "$EVIDENCE/seed/"
cp "$ROOT/logs/seed-summary.json" "$EVIDENCE/seed/"
cp "$ROOT/logs/seed-account-ids.txt" "$EVIDENCE/seed/"
cp "$ROOT/logs/old-settings-after-restart.json" "$EVIDENCE/seed/"

cp "$ROOT/logs/new-install.log" "$EVIDENCE/upgrade/"
cp "$ROOT/logs/new-install.exit" "$EVIDENCE/upgrade/"
cp "$ROOT/logs/upgrade-install-verification.txt" "$EVIDENCE/upgrade/"
cp "$ROOT/logs/new-ready.json" "$EVIDENCE/upgrade/"
cp "$ROOT/logs/post-upgrade/live-verification.txt" "$EVIDENCE/upgrade/"
cp "$ROOT/logs/post-upgrade/post-upgrade-db-snapshot.json" "$EVIDENCE/upgrade/"
cp "$ROOT/logs/post-upgrade/codex-installer-verification.json" "$EVIDENCE/upgrade/"
cp "$ROOT/logs/post-upgrade/setup-pool-codex.output.txt" "$EVIDENCE/upgrade/"

cp "$ROOT/logs/rollback-test/verification.txt" "$EVIDENCE/rollback/"
cp "$ROOT/logs/rollback-test/switch-old.txt" "$EVIDENCE/rollback/"
cp "$ROOT/logs/rollback-test/switch-new.txt" "$EVIDENCE/rollback/"
cp /root/autodl-tmp/rollback-no-systemd.sh "$EVIDENCE/rollback/"

python3 - "$ROOT" "$EVIDENCE/deployment-summary.json" <<'PY'
import hashlib, json, pathlib, sqlite3, sys
root=pathlib.Path(sys.argv[1])
ready=json.loads((root/"logs/rollback-test/switch-new.txt").read_text().splitlines()[0])
db=sqlite3.connect(root/"state/pool.sqlite3")
summary={
    "hostname":"autodl-container-edfb41a5b4-03240cea",
    "isolated_root":str(root),
    "listen_addr":"127.0.0.1:"+str((root/"port").read_text().strip()),
    "ready":ready,
    "config_path":str(root/"etc/config.json"),
    "database_path":str(root/"state/pool.sqlite3"),
    "current_release":str((root/"prefix/lib/codex-pool/current").resolve()),
    "rollback_release":str(root/"prefix/lib/codex-pool/releases/old-0873de57"),
    "rollback_command":"bash /root/autodl-tmp/rollback-no-systemd.sh old",
    "rollforward_command":"bash /root/autodl-tmp/rollback-no-systemd.sh new",
    "config_sha256":hashlib.sha256((root/"etc/config.json").read_bytes()).hexdigest(),
    "new_binary_sha256":hashlib.sha256((root/"prefix/lib/codex-pool/releases/new-goal-context-fix/codex-pool-server").read_bytes()).hexdigest(),
    "old_binary_sha256":hashlib.sha256((root/"prefix/lib/codex-pool/releases/old-0873de57/codex-pool-server").read_bytes()).hexdigest(),
    "sqlite_integrity":db.execute("pragma integrity_check").fetchone()[0],
    "fixture_counts":{
        "accounts":db.execute("select count(*) from accounts where group_name='legacy-team'").fetchone()[0],
        "egress_profiles":db.execute("select count(*) from egress_profiles where id like 'legacy-%'").fetchone()[0],
        "egress_pools":db.execute("select count(*) from egress_pools where id='legacy-registration-pool'").fetchone()[0],
        "custom_providers":db.execute("select count(*) from custom_providers where id='legacy-relay'").fetchone()[0],
    },
}
json.dump(summary,open(sys.argv[2],"w"),indent=2,sort_keys=True)
PY

if grep -R -E 'fixture-(access|refresh)|experimental_bearer_token = "cap_|"secret":"cap_' "$EVIDENCE"; then
  echo "credential-like material found in evidence" >&2
  exit 1
fi

(
  cd "$EVIDENCE"
  find . -type f ! -name SHA256SUMS -print0 | sort -z | xargs -0 sha256sum
) >"$EVIDENCE/SHA256SUMS"
tar -C "$ROOT" -czf "$ARCHIVE" evidence
sha256sum "$ARCHIVE" | tee /root/autodl-tmp/cpupg-evidence-20260730.tar.gz.sha256
cat "$EVIDENCE/deployment-summary.json"
