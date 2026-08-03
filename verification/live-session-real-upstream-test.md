# Live ChatGPT Web Session → Codex upstream verification

Date: 2026-08-03
Pool image: codex-pool:session-token-e2e
Codex image: codex-cli:0.146.0-local
Upstream: https://chatgpt.com/backend-api/codex

## Secret handling

- The latest user message was parsed from the local session history without printing either credential.
- Plaintext session/import files were mode 0600.
- Both plaintext files were deleted immediately after the encrypted database rows were verified.
- The retained diagnostics bundle passed exact-value scans for the access token, session token, account IDs, and both email claims.

## Layered result

| Layer | Result | Evidence |
|---|---|---|
| JWT static validation | PASS | current validity, model.request scope, account/plan claim match |
| Project raw session import | PASS | HTTP 200, import_status=imported, credential_mode=chatgpt_auth_tokens |
| Encrypted storage | PASS | AT and session cookie differ from plaintext; SQLite/WAL/SHM exact-value scans empty |
| ChatGPT quota authentication | PASS | 5h_polled status=allowed_warning, used_percent=0; quota poll completed successfully |
| Codex Responses health, gpt-5.6-sol | FAIL | upstream HTTP 401, {"detail":"Unauthorized"} |
| Codex Responses health, gpt-5.5 | FAIL | upstream HTTP 401, {"detail":"Unauthorized"} |
| Account header variants | FAIL | original UUID, user-form ID, and omitted header all returned the same 401 |
| Session-cookie re-mint | FAIL | GET /api/auth/session from the pool egress returned HTTP 403 |
| Official Codex chat | FAIL | Codex 0.146.0 produced no assistant answer and timed out after 45 seconds |

## Important routing observation

The downstream /v1/responses connection is opened as HTTP 200 and the pool emits repeated
response.in_progress events while the scheduler waits. The diagnostic attempt ledger shows the
actual upstream terminal response:

- attempted
- transport_attempted
- response_headers: 401

Thus a client sees a hanging turn instead of the original authentication failure.

## Diagnosis

The access token is live enough for the ChatGPT quota endpoint and is not expired, but the
Codex Responses backend rejects it. This rules out parser corruption, expiry, workspace header,
and model selection. The evidence is consistent with a ChatGPT Web grant/client token that is
not accepted as a Codex Responses credential.

The missing refresh token is not the first failure: the fresh AT itself receives 401. The stored
session cookie then attempts the intended fallback, but re-minting receives 403 from the pool
egress, consistent with cookie/IP/browser-session controls.

## Retained artifact

- /workspace/verification/live-session-real-upstream-diagnostics.zip
- SHA-256: 2e5c3258186c93fc385dbcf8ad40ef6c5878f0a2ed934d73b8dae3b163c2ce22
