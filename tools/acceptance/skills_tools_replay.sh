#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
GO_BIN="${GO_BIN:-/tmp/codex-go1.24.1/go/bin/go}"

cd "$ROOT_DIR"

API_TESTS='Test(AnthropicPassthroughFilesAndSkills|SharedEndpointCodexHintDoesNotFallThroughToClaude|SharedEndpointClaudeHintRoutesToClaudePassthrough|SharedEndpointAutoWithoutProviderSignalIsExplicitError|CustomResponsesHostedToolIsOmittedWithDiagnostic|CustomMessagesTypedServerToolReturnsCapabilityUnavailable|CustomMessagesClaudeBetaHeaderReturnsCapabilityUnavailable|AdminSkillsCompatDoctorReportsOfficialRawAndProviderTiers|SetupScriptCodexAutomationWritesOnlyCodexConfig|MessagesRoutesGPTToBuiltInCodexStreamingTools|ResponsesWebSocketSourceConversionSpoolsAndPreservesUnknownToolOutput|DownstreamResponsesWebSocketNeverLeaksRepeatedOrphanedToolOutput|CodexMappedRecoveryRejectsUnpairedToolOutputWithoutCheckpoint|CodexMappedDownstreamWebSocketQuotaRotatesAccountAndRestoresToolContext|GoalContinuityRejectsUnpairedCustomToolCallBeforeUpstream|NeutralizeOrphanedToolOutputsUsesStablePairingRules|DegradedReplayKeepsPairedToolCallOutput)$'
UPSTREAM_TESTS='Test(CodexSourceNormalizationPreservesSkillsPluginsAndToolMatrix|CodexResponsesWebSocketBridge|CodexResponsesWebSocketStreamsBodySourceWithoutMaterializing|CodexHTTPClassicHostedToolDoesNotOptIntoResponsesLite|NormalizeCodexResponsesLiteMergesGatewayToolsAndInstructions)$'
PROMPT_TESTS='Test(ResponsesStableToolBridgePlanRoundTrip|ResponsesLiteToolSearchAddsDiscoveredTools|ResponsesRequestToChatCompletionOmitsHostedResponsesTools|ResponsesRequestToChatCompletionPreservesUnknownHistoryAsJSON|AnthropicRequestToChatCompletionRejectsTypedServerTools)$'
GATEWAY_TESTS='Test(CompatRuntimeEnvPointsClaudeAtPoolAndKeepsHome|RuntimeEnvDoesNotForceClaudeModelWhenUnconfigured)$'

GOTOOLCHAIN=local "$GO_BIN" test ./internal/api -run "$API_TESTS" -count=1
GOTOOLCHAIN=local "$GO_BIN" test ./internal/upstream -run "$UPSTREAM_TESTS" -count=1
GOTOOLCHAIN=local "$GO_BIN" test ./internal/prompt -run "$PROMPT_TESTS" -count=1
GOTOOLCHAIN=local "$GO_BIN" test ./cmd/gateway -run "$GATEWAY_TESTS" -count=1
GOTOOLCHAIN=local "$GO_BIN" test ./tools/e2e/codex-multiagent ./tools/e2e/cli-matrix -count=1

echo "skills/tools replay passed: native HTTP+WS, shared Skills endpoints, MCP/local shell/custom/tool-search, orphan policy, capability loss, config preservation, multi-agent"
