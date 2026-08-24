# Cursor proxy module

Codex Pool runs Cursor through the independently versioned
[`anyrobert/cursor-api-proxy`](https://github.com/anyrobert/cursor-api-proxy)
bridge. The runtime artifact is the npm release `1.4.0`, whose published
`gitHead` is `b04c55599cb63e3310b798b66349d161bd323e62` (2026-08-19). The
separate maintenance review reached upstream head
`6364d9e5eb6811980fccadc413af2c0d11b995f2` later that day. The pinned release
includes the post-update ACP tool-passthrough work, and this was the only
evaluated candidate with maintenance activity during the preceding seven days.

The npm lockfile pins the published artifact and transitive dependency graph.
Production never polls GitHub releases: image construction makes one npm-registry
install, and account request paths only start the already-installed local module.
This avoids the unreliable GitHub source-download 403 path and prevents release
polling from amplifying traffic. The official installer request retries only one
transient/429/5xx failure (never HTTP 403); the larger Agent package has at most
two delayed transient retries.

Authentication modes:

- API key: add it from the Codex Pool account dialog. The key stays encrypted in
  the normal credential store and is passed only to that account's local process.
- Cursor account: run `cursor-api-proxy account login <account-name>`, complete
  Cursor's official browser login (including any email/password step there), then
  add that account name in the dialog. Codex Pool never accepts or stores the
  Cursor password.

Individual-plan quota refresh is available for browser-login accounts through
the account token cached by the module. Cursor User API Keys can run the agent,
but Cursor does not expose individual-plan usage for those keys; the UI reports
that limitation instead of retrying a private endpoint. Cursor Team Admin API
keys are a separate credential type and are not treated as agent credentials.

Each selected account gets a lazy loopback-only process. Starts are singleflighted,
failed starts are negatively cached for 30 seconds, and at most 64 local instances
are retained. The pool uses the module's native Responses path for Codex calls;
active response leases cannot be LRU-evicted. The pool-to-bridge hop and health
checks are forced direct on loopback, while only the child receives the account's
selected external egress. Every child receives a private `0700` runtime home plus a minimal
environment allowlist, and cannot auto-discover sibling account directories. Set
`CODEX_CURSOR_PROXY_BIN` only when using an externally installed compatible
executable. Set `CODEX_CURSOR_ACCOUNTS_DIR` to override the managed browser-login
directory.
