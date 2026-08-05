# Super-Instruct headless local runtime

This directory is the local resource bundle migrated from
`other/Super-Instruct-Codex-5.6`. The server reuses the project without its
Tauri tray, frontend, or second loopback proxy:

- all bundled `codex-skills` are installed with every release;
- `bridge.md` is copied byte-for-byte from the source tree and the Go gateway
  injects it only when both the Codex client and its model-family group profile
  enable M1;
- Memory and Monitor reuse gateway-owned state and run only when their resolved
  group-profile fields are authorized;
- SSE remains native streaming passthrough. Memory/Monitor may observe a bounded
  tee after bytes are forwarded; response rewrite is deliberately non-streaming;
- no desktop session, WebView, Rust sidecar, or modification of `~/.codex` is
  required.

The enabled modules keep the source order while retaining the pool's native
streaming contract:

1. **M1 SystemPromptInjector** replaces every supported system carrier
   (`instructions`, `system`, `system_prompt`, `personality`, Chat `messages`,
   and Responses `input`). Its value is `bridge.md` followed by additions
   compiled from the request's resolved project user group: group system prompt,
   model-family instruction files, and the group's Super-Instruct skill profile.
2. **M4 UniversalSseParser** parses non-streaming responses for enabled response
   modules and observes an already-forwarded bounded SSE tee when required.
3. **M3 TamperEngine** runs the complete source-project regular-expression set
   for authorized non-streaming rewrite profiles and self-gates after a response
   was changed.
4. **M5 MemoryKernel** self-gates changed/short responses and atomically persists
   successful interactions under the server data directory.
5. **M6 MonitorPanel** observes every interaction. Its history/stats are available
   at `/admin/super-instruct/monitor`; the headless real-time replacement for
   Tauri events is `/admin/super-instruct/monitor/events` (SSE events
   `interaction` and `stats`).

## Enable per API key and user group

The cloud-server installer always places the bridge and skills in each release;
it has no global Super-Instruct prompt, flag, or environment switch. The legacy
`super_instruct_local_enabled` configuration field remains readable only for
configuration compatibility and does not grant runtime capability.

An administrator grants the desired model-family features in the API key's user
group. The user then runs the copied `/file/<API_KEY>` one-click command and the
generated installer asks, while configuring Codex, whether to opt in. Effective
behavior is the intersection of these two decisions: the client must explicitly
select Super-Instruct and the resolved user-group profile must allow each module.
A disabled or absent client selection leaves M1/M3/M5/M6 inactive.

Configured per-model-family profiles determine the M1 skill additions and the
allowed M3/M5/M6 modules. This keeps project-group instruction selection intact
without requiring a desktop GUI or a server-wide override.

`bridge.md` stays inside the server release rather than a client home directory.
Supporting UTF-8 resources remain available for explicitly selected skills,
while an enabled group's empty skill selection sends each `SKILL.md` without
eagerly inflating every request with helper files.
