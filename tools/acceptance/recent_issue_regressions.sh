#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
GO_BIN="${GO_BIN:-go}"

cd "$ROOT_DIR"
if ! "$GO_BIN" version | grep -q 'go1\.25\.12'; then
  echo "Go 1.25.12 is required; set GO_BIN to the pinned toolchain" >&2
  exit 1
fi

API_TESTS='Test(StatuslessResponsesSSEOverloadFailsOverToDifferentAccount|ClaudeEmptyEndTurnContinuesOnSameAccount|EmptyCompletedResponsesFailsOverBeforeDownstreamCommit|CustomMidStream429WithoutFallbackTerminatesAndLeavesServerResponsive|OversizedFirstDownstreamWebSocketTurnUsesHTTPSBridge|WriteFilteredErrorAlwaysRedactsUpstreamTopology|WriteUpstreamHeadersDropsAccountScopedModelsETag|ResponsesStreamToChatSSERecognizesStableToolKinds|AnthropicStreamToChatSSERequiresMessageStop|ChatStreamToResponsesSSERequiresDone|ChatStreamToAnthropicSSERequiresDone|ResponsesStreamToAnthropicSSERequiresTerminalEvent|ResponsesStreamToChatSSERequiresTerminalEvent|TerminalCommitWriterCommitsBeforeCompletedFrame|CodexSessionMappingAdvancesWindowAfterBodyTriggeredCompaction|DeepSeekV4ToolReplayAddsRequiredReasoningContent|DeepSeekReasoningAliasIsNarrowAndNeverOverwritesProviderValue)$'
UPSTREAM_TESTS='Test(StripCodexUnsupportedPromptCacheControlsIsNarrowAndExact|NormalizeCodexSourceSelectsTargetedFallbackForNestedPromptCacheBreakpoint|CodexSourceNormalizationPreservesSkillsPluginsAndToolMatrix|NormalizeCodexReasoningEffortForWire)$'
STORAGE_TESTS='Test(CommitFreshCodexSessionBindingsPersistsWholeBatch|CommitFreshCodexSessionBindingsRollsBackConflictingBatch|CodexSessionMappingResolvesAfterStoreRestart|CodexUpstreamAttemptEventIsIdempotent)$'
LEAK_TESTS='Test(ParseCodexFailureFrameRetriesStatuslessOverloadVariants|RedactUpstreamTopology|NeutralizeErrorBodyServerOverloaded)$'
BAN_TESTS='Test(Classify|ClassifyHeaderRegion)$'

GOTOOLCHAIN=local "$GO_BIN" test ./internal/api -run "$API_TESTS" -count=1
GOTOOLCHAIN=local "$GO_BIN" test ./internal/upstream -run "$UPSTREAM_TESTS" -count=1
GOTOOLCHAIN=local "$GO_BIN" test ./internal/storage -run "$STORAGE_TESTS" -count=1
GOTOOLCHAIN=local "$GO_BIN" test ./internal/leakfilter -run "$LEAK_TESTS" -count=1
GOTOOLCHAIN=local "$GO_BIN" test ./internal/ban -run "$BAN_TESTS" -count=1

echo "recent issue regressions passed: CLIProxyAPI, Sub2API, Codex-Manager (2026-06-01..2026-08-14 sample)"
