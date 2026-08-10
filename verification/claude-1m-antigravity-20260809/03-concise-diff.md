# Concise diff

Branch: `cache-hit-optimization`

Changed decision branch/fields:

- `!virtual1M && selected NativeMaxContextWindow >= 1_000_000` now bypasses the relay compaction guard and preserves the upstream context error for Claude Code.
- A requested 1M route that falls back to a smaller selected account uses that account's `NativeContextWindow`, `EffectiveContextWindowPercent`, and `AutoCompactTokenLimit`.
- Fallback thresholds are Claude 200K → 167K, Kiro → 80%, and Antigravity → 50%.
- Antigravity `User-Agent` is process-managed from the public updater manifest; `requestId` is `agent-<uuid>`; inference transport remains HTTP/1.1.
- Antigravity request conversion uses non-copying JSON views and appends `request.contents` last; configured sensitive terms are split only in translated `systemInstruction`.
- `/v1/messages/count_tokens` releases an Antigravity scheduler lease before returning the local estimate.

The complete machine-verifiable commands, literal results, hashes, rollback result, and restored behavior are in `VERIFICATION.txt`.
