# Super-Instruct headless local runtime

This directory is the local resource bundle migrated from
`other/Super-Instruct-Codex-5.6`. The server reuses the project without its
Tauri tray, frontend, or second loopback proxy:

- all bundled `codex-skills` are installed with every release;
- `bridge.md` is copied byte-for-byte from the source tree and the Go gateway
  performs its request injection directly on the existing request path;
- Memory and Monitor reuse gateway-owned state; local mode runs them on the same
  parsed response context after M3;
- local-mode SSE is accumulated for M4 just like the source proxy; an M3 match
  is emitted with the source-compatible four-event Responses SSE wrapper;
- no desktop session, WebView, Rust sidecar, or modification of `~/.codex` is
  required.

The enabled request path keeps the source module order:

1. **M1 SystemPromptInjector** replaces every supported system carrier
   (`instructions`, `system`, `system_prompt`, `personality`, Chat `messages`,
   and Responses `input`). Its value is `bridge.md` followed by additions
   compiled from the request's resolved project user group: group system prompt,
   model-family instruction files, and the group's Super-Instruct skill profile.
2. **M4 UniversalSseParser** parses JSON, Responses, Chat, and SSE once.
3. **M3 TamperEngine** runs the complete source-project regular-expression set
   and self-gates after a response was changed.
4. **M5 MemoryKernel** self-gates changed/short responses and atomically persists
   successful interactions under the server data directory.
5. **M6 MonitorPanel** observes every interaction. Its history/stats are available
   at `/admin/super-instruct/monitor`; the headless real-time replacement for
   Tauri events is `/admin/super-instruct/monitor/events` (SSE events
   `interaction` and `stats`).

## Enable during installation

An interactive install asks whether to enable the deployment-wide local mode.
Automation can make the choice explicitly:

```bash
sudo scripts/install.sh --with-super-instruct
sudo scripts/install.sh --without-super-instruct
WITH_SUPER_INSTRUCT=1 sudo -E scripts/install.sh
```

The choice is written to fresh configuration and exported to managed workers as
`CODEX_POOL_SUPER_INSTRUCT_LOCAL_ENABLED`. The equivalent JSON key is
`super_instruct_local_enabled` (default `false`).

When enabled, an unconfigured group receives every installed skill through
progressive disclosure. Configured per-model-family profiles determine the M1
skill additions for that group, while the local M3/M5/M6 chain remains active as
one complete deployment. This keeps project-group instruction selection intact
without requiring the removed desktop GUI.

`bridge.md` stays inside the server release rather than a client home directory.
Supporting UTF-8 resources remain available for explicitly selected skills,
while the deployment-wide all-skills fallback sends each `SKILL.md` without
eagerly inflating every request with helper files.
