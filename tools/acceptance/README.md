# Extreme forwarding acceptance

`extreme_load.sh` is the reproducible hard gate for the large-context hot path. It:

- generates at least 256 high-entropy JSON fixtures and verifies every body with the target model's `o200k_base` tokenizer at `1,000,000 ±0.5%` tokens before timing;
- rejects fixtures whose gzip/raw ratio is below 0.70;
- runs the real pool server with SQLite inside a Docker cgroup limited to 2 CPU and 2 GiB;
- keeps the load generator and a TLS/HTTP2 protocol-real mock upstream outside that cgroup;
- requires every timed response to succeed, the completed-request rate to meet `MINIMUM_ACHIEVED_RPS` (strictly equal to `TARGET_RPS` by default), no OOM kill, and all upstream requests to negotiate HTTP/2. Set a slightly higher `TARGET_RPS` while keeping `MINIMUM_ACHIEVED_RPS=100` when proving a strict 100-RPS completion gate.

Run:

```bash
tools/acceptance/extreme_load.sh
```

Use `KEEP_ARTIFACTS=1` to retain fixtures, logs, Docker samples, and JSON results. A quick developer smoke can lower `FIXTURE_COUNT`, `TARGET_TOKENS`, `TARGET_RPS`, and `LOAD_DURATION`; only the defaults constitute the hard gate.

## Mixed context, provider, and multi-CLI pressure

`mixed_context_pressure.sh` is the deterministic matrix for agent-style traffic. It generates and tokenizer-verifies independent `128k`, `256k`, and `1M` fixture sets. Every fixture contains a hosted web-search declaration, a paired function call/output, a paired Skill call/output, a prompt-cache key, and a downstream CLI identity. It then exercises:

- context compaction while quota exhaustion forces an account switch, including tool-pair reconstruction and the case where another mapped root reaches selection only after the old account's 5xx circuit breaker has opened;
- Kiro pressure spillover and sticky mid-conversation handoff, plus built-in and custom-provider failover;
- 200- and 1,000-stream gateway pressure while unrelated new requests enter selection;
- multiple downstream CLIs sharing a weak process session without crossing context;
- root/child-agent canonical affinity: children stay on their root account even while a less-busy account exists, while independent requests continue balancing across the pool.

The default uses only deterministic local mocks and writes a machine-readable `report.json`; it consumes no real account quota:

```bash
GO_BIN=/path/to/go1.25.12/bin/go tools/acceptance/mixed_context_pressure.sh
```

Set `RUN_TRANSPORT_LOAD=1` to additionally run all three tiers through the TLS/HTTP2, cgroup-limited transport gate. `FIXTURE_COUNT_PER_TIER`, `TARGET_RPS`, `LOAD_CONCURRENCY`, and `LOAD_DURATION` tune the non-release smoke profile; release evidence should keep the documented defaults. Live provider/CLI probing remains a separate opt-in because it consumes credentials and quota.

`diagnostics_replay.sh` verifies the source diagnostics archive checksum and runs the focused regression matrix for every issue extracted from it:

- WebSocket inner-429 account rotation and paired tool-context reconstruction;
- stable local rejection of unknown historical tool IDs without leaking a vendor error or corrupting context;
- concurrent mapping commits and cancellation;
- atomic diagnostic snapshots and bounded streaming export;
- idempotent usage settlement, journal crash replay, affinity/attempt retention, and incremental Goal v2 compaction.

Run:

```bash
tools/acceptance/diagnostics_replay.sh
```

`skills_tools_replay.sh` simulates the full local/hosted capability boundary: opaque Skills and plugin metadata on native Codex HTTP and WebSocket requests, large tool schemas, function/namespace/custom/local-shell/MCP/client tool-search, parallel tools, official Claude Skills passthrough, unknown historical call IDs, explicit hosted-capability loss, installer preservation of MCP/plugin/feature TOML, and the Codex multi-agent event protocol.

Run:

```bash
tools/acceptance/skills_tools_replay.sh
```

`recent_issue_regressions.sh` is the focused gate derived from a recent (2026-06-01 through 2026-08-14) sample of CLIProxyAPI, Sub2API, and `qxcnm/Codex-Manager` issues. It covers statusless SSE overload rotation, same-account Claude empty-`end_turn` continuation, upstream topology redaction, prompt-cache controls, premature stream terminals, tool-name deltas, compaction signals, reasoning round trips, and atomic batched context commits. The source-to-test matrix is in `docs/reports/production-issue-regression-2026-08-14.md`.

Run:

```bash
tools/acceptance/recent_issue_regressions.sh
```

`cluster_failover.sh` creates an isolated PostgreSQL 16 primary plus physical streaming standby and a Redis 7 AOF node. It validates SQLite migration, two application nodes settling the same usage event exactly once, PostgreSQL primary loss plus standby promotion, Redis restart durability, fencing, lease renewal, Pub/Sub-independent recovery, and lease expiry after node loss. Every container, network, and volume is uniquely named and removed on exit.

Run on a remote Docker host:

```bash
tools/acceptance/cluster_failover.sh
```

`extreme_load_host.sh` is the fallback for a remote host whose root cgroup cannot create a Docker domain child. It pins the pool to two CPUs, sets `GOMAXPROCS=2`, kills the run above 2 GiB RSS, and records RSS, FD, thread, goroutine, and spool peaks. Its result is explicitly labeled `taskset+RSS-watchdog`; it does not replace the default Docker cgroup gate.
