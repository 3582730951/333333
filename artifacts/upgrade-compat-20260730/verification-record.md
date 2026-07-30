# Cloud old-to-new upgrade verification record

## Bound object and inputs

- Cloud host: `autodl-container-edfb41a5b4-03240cea`
- Isolated deployment root: `/root/autodl-tmp/cpupg-20260730`
- Old source commit: `0873de57a4113f3bef6cc6f8d1133250883f0948`
- Old source archive SHA-256: `089d84e1d5e24419aaf78854a2aee42b99bf801d0f7950c8d5dca114cb2ed944`
- New source archive SHA-256: `d634fca1d6adcabcbb9b52e2696752e863adee5367ff08adb19f4a3a156eb7bb`
- Config: `/root/autodl-tmp/cpupg-20260730/etc/config.json`
- Database: `/root/autodl-tmp/cpupg-20260730/state/pool.sqlite3`
- Listen address: `127.0.0.1:34273`

## Baseline command

```bash
SERVICE_USER=root SERVICE_GROUP=root \
INSTALL_PREFIX=/root/autodl-tmp/cpupg-20260730/prefix \
CONFIG_DIR=/root/autodl-tmp/cpupg-20260730/etc \
DATA_DIR=/root/autodl-tmp/cpupg-20260730/state \
SYSTEMD_DIR=/root/autodl-tmp/cpupg-20260730/systemd \
BUILD_DIR=/root/autodl-tmp/cpupg-20260730/build-old \
DEPLOY_LOCK_FILE=/root/autodl-tmp/cpupg-20260730/install.lock \
GO_INSTALL_ROOT=/root/autodl-tmp/cpupg-20260730/toolchains \
GO_BIN=/root/autodl-tmp/cpupg-20260730/toolchains/go1.25.12/bin/go \
GOPROXY=https://goproxy.cn,direct \
SKIP_OS_PACKAGES=1 INSTALL_SYSTEMD=0 START_SERVICE=0 \
WITH_SIDECAR=0 WITH_REGISTRATION=0 WITH_WARP=0 \
RELEASE_ID_OVERRIDE=old-0873de57 \
./install.sh --minimal --no-systemd --no-start --no-tests \
  --no-migrate-user-groups --listen-addr 127.0.0.1:34273
```

Exit status: `0`

Baseline start command:

```bash
CODEX_POOL_DATABASE=/root/autodl-tmp/cpupg-20260730/state/pool.sqlite3 \
CODEX_POOL_MIGRATE_USER_GROUPS=0 \
CODEX_POOL_LISTEN_ADDR=127.0.0.1:34273 \
/root/autodl-tmp/cpupg-20260730/prefix/lib/codex-pool/releases/old-0873de57/codex-pool-server \
  --config /root/autodl-tmp/cpupg-20260730/etc/config.json
```

Literal readiness output:

```json
{"checks":{"storage":true},"deployment_state":"active","fencing_token":2,"inflight":0,"ok":true,"ready":true,"release_id":"development","started_at":"2026-07-30T19:32:43.13195226Z","worker_socket":""}
```

Seeded old-format inputs:

- Accounts: `alpha@example.internal`, `beta@example.internal`
- Account group: `legacy-team`
- Egress profiles: `legacy-direct-us`, `legacy-http-exit`, `legacy-sidecar-exit`
- Registration egress pool: `legacy-registration-pool`
- Downstream key label: `Legacy Codex downstream`
- Custom provider: `legacy-relay`
- Config values: Goal enabled, Codex session mapping enabled, model `gpt-5.6-sol`,
  effort `xhigh`, approval `never`, sandbox `danger-full-access`

## Modified command

```bash
SERVICE_USER=root SERVICE_GROUP=root \
INSTALL_PREFIX=/root/autodl-tmp/cpupg-20260730/prefix \
CONFIG_DIR=/root/autodl-tmp/cpupg-20260730/etc \
DATA_DIR=/root/autodl-tmp/cpupg-20260730/state \
SYSTEMD_DIR=/root/autodl-tmp/cpupg-20260730/systemd \
BUILD_DIR=/root/autodl-tmp/cpupg-20260730/build-new \
DEPLOY_LOCK_FILE=/root/autodl-tmp/cpupg-20260730/install.lock \
GO_INSTALL_ROOT=/root/autodl-tmp/cpupg-20260730/toolchains \
GO_BIN=/root/autodl-tmp/cpupg-20260730/toolchains/go1.25.12/bin/go \
GOPROXY=https://goproxy.cn,direct \
SKIP_OS_PACKAGES=1 INSTALL_SYSTEMD=0 START_SERVICE=0 \
WITH_SIDECAR=0 WITH_REGISTRATION=0 WITH_WARP=0 \
RELEASE_ID_OVERRIDE=new-goal-context-fix \
./install.sh --minimal --no-systemd --no-start --no-tests \
  --no-migrate-user-groups --listen-addr 127.0.0.1:34273
```

Exit status: `0`

Literal installer preservation output:

```text
upgrade_install_exit=0
config_sha256_before=cde01a26751948a159065d1a5d288e24f5d25d95ec4912ed78b281be406c5a57
config_sha256_after_install=cde01a26751948a159065d1a5d288e24f5d25d95ec4912ed78b281be406c5a57
database_sha256_before=6f6b04ed3e72f13c2b7c0427883b3ce423d2c470ab4658694c4fabfbc6b8970e
database_sha256_after_install=6f6b04ed3e72f13c2b7c0427883b3ce423d2c470ab4658694c4fabfbc6b8970e
current_release=/root/autodl-tmp/cpupg-20260730/prefix/lib/codex-pool/releases/new-goal-context-fix
previous_release=not-created-in-no-systemd-mode
rollback_release=/root/autodl-tmp/cpupg-20260730/prefix/lib/codex-pool/releases/old-0873de57
new_binary_sha256=4028a0fd4c5123b6b571dc1f0b355d463e0e60ed83efa1447b549e06109ff74a
old_binary_sha256=d39cf2ceed92dc29638c9058093079efe24ed38894e1beb513c7d3fdbff40b0c
```

Modified start command:

```bash
CODEX_POOL_DATABASE=/root/autodl-tmp/cpupg-20260730/state/pool.sqlite3 \
CODEX_POOL_MIGRATE_USER_GROUPS=0 \
CODEX_POOL_LISTEN_ADDR=127.0.0.1:34273 \
/root/autodl-tmp/cpupg-20260730/prefix/lib/codex-pool/releases/new-goal-context-fix/codex-pool-server \
  --config /root/autodl-tmp/cpupg-20260730/etc/config.json \
  --release-id new-goal-context-fix
```

Exit status: `0`

Literal readiness output:

```json
{"checks":{"storage":true},"deployment_state":"active","fencing_token":5,"inflight":0,"ok":true,"ready":true,"release_id":"new-goal-context-fix","started_at":"2026-07-30T19:42:38.00210814Z","worker_socket":""}
```

Post-upgrade checks and literal result:

```text
live_upgrade_data_verified {"accounts": 2, "config_sha256": "cde01a26751948a159065d1a5d288e24f5d25d95ec4912ed78b281be406c5a57", "egress_pool": "legacy-registration-pool", "egress_profiles": 3, "provider": "legacy-relay", "release": "new-goal-context-fix", "sqlite_integrity": "ok"}
```

Exit status: `0`

Codex-only installer command:

```bash
KEY="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["key"])' \
  /root/autodl-tmp/cpupg-20260730/logs/seed/downstream-key.json)"
curl -fsS "http://127.0.0.1:34273/file/${KEY}?client=codex" \
  > /root/autodl-tmp/cpupg-20260730/logs/post-upgrade/setup-pool-codex.sh
HOME=/root/autodl-tmp/cpupg-20260730/codex-client-home \
CODEX_HOME=/root/autodl-tmp/cpupg-20260730/codex-client-home/.codex \
bash /root/autodl-tmp/cpupg-20260730/logs/post-upgrade/setup-pool-codex.sh
```

Exit status: `0`

Verified checks: Goal/experimental enabled; model, reasoning effort, approval,
sandbox, URL, and API key set; unrelated context limits, multi-agent, MCP, and
model cache preserved; no token file, client-ID file, or Claude tree created.

## Rollback and roll-forward

Rollback command:

```bash
bash /root/autodl-tmp/rollback-no-systemd.sh old
```

Literal output and exit status:

```text
{"checks": {"storage": true}, "deployment_state": "active", "fencing_token": 4, "inflight": 0, "ok": true, "ready": true, "release_id": "old-0873de57", "started_at": "2026-07-30T19:42:37.533623843Z", "worker_socket": ""}
active_release=old-0873de57 pid=379129 current=/root/autodl-tmp/cpupg-20260730/prefix/lib/codex-pool/releases/old-0873de57
```

Exit status: `0`; all seeded accounts, egress profiles, and provider were readable.

Roll-forward command:

```bash
bash /root/autodl-tmp/rollback-no-systemd.sh new
```

Literal output and exit status:

```text
{"checks": {"storage": true}, "deployment_state": "active", "fencing_token": 5, "inflight": 0, "ok": true, "ready": true, "release_id": "new-goal-context-fix", "started_at": "2026-07-30T19:42:38.00210814Z", "worker_socket": ""}
active_release=new-goal-context-fix pid=379190 current=/root/autodl-tmp/cpupg-20260730/prefix/lib/codex-pool/releases/new-goal-context-fix
```

Exit status: `0`; final state is the new release.
