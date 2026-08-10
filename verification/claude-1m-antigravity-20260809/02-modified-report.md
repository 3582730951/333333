# Modified report

## Result

Branch: `cache-hit-optimization`

The decisive branch is now account-scoped:

- When the scheduler selects a verified native 1M capability (`!virtual1M && NativeMaxContextWindow >= 1_000_000`), the relay does not estimate, truncate, or generate a compaction error. The request and 1M beta reach upstream. An upstream `context_length_exceeded` response keeps its HTTP status, Anthropic error type/code, and message so Claude Code remains the sole compaction authority.
- When a downstream 1M request must fall back to a smaller selected account, the 1M beta is removed before that provider and the relay uses the selected capability's real window. The reactive limits are Claude 200K → 167K, Kiro → 80%, and Antigravity → 50%. Claude Code's detected summary request bypasses the guard so compaction cannot recursively block itself.

The integration regression sends an estimated >1M body through a verified native-1M account. The mock upstream receives it with the 1M beta and returns:

```json
{"type":"error","error":{"type":"invalid_request_error","code":"context_length_exceeded","message":"Prompt is too long: 1000001 tokens > 1000000"}}
```

The downstream receives HTTP 400 with the same four semantic fields and no `X-MiCliProxy-Auto-Compact` header.

## Antigravity update

Reference: CLIProxyAPI commit `ecc9aa72b32f34b680d03b0724b531a21ae74472`.

- A process-wide updater reads the public Antigravity Hub updater manifest outside the request path. The verified 2026-08-09 fallback is `2.6.0`; imported OAuth accounts no longer persist the old `2.2.1` UA.
- Empty and legacy managed UAs resolve to `antigravity/hub/<cached-version> darwin/arm64`; explicit custom UAs remain unchanged.
- Request IDs now use `agent-<uuid>`; inference stays HTTP/1.1 with ALPN restricted to `http/1.1`.
- Configured sensitive words are split with U+200B only in the translated `systemInstruction`; user messages and tool payloads remain unchanged.
- Read-only GJSON access no longer copies the full request, and `request.contents` is attached after small envelope mutations.
- The updater goroutine uses the repository supervisor boundary.

## Performance sample

Identical 1 MiB conversion benchmark, 10 fixed iterations:

| Build | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| Original | 19,744,100 | 10,580,451 | 74 |
| Modified | 19,023,280 | 4,025,422 | 60 |

Observed change: latency `-3.65%`, allocated bytes `-61.95%`, allocation count `-18.92%`.

The isolated 1 MiB JSON lookup benchmark changed from `1,024,367 ns/op, 1,056,800 B/op, 1 alloc/op` to `399,600 ns/op, 0 B/op, 0 alloc/op` in that run.

## Validation

- Final same-scope test: PASS, exit 0.
- Full `go test ./...`: PASS, exit 0.
- Race test for the new identity/cache and no-copy view packages: PASS, exit 0.
- Rollback executed on a separate worktree: PASS, exit 0; transaction paths clean and key SHA-256 values match the original.

Exact commands, literal results, source hashes, and the four canonical transaction paths are recorded in `VERIFICATION.txt`.
