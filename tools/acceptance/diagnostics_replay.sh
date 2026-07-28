#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
GO_BIN="${GO_BIN:-/tmp/codex-go1.24.1/go/bin/go}"
BUNDLE="$ROOT_DIR/example_zip/codex-pool-diagnostics-v2.zip"
EXPECTED_SHA256="09b530cee7b04223e9bfc8b52b2307ef7daaeb8daa5f4d46b69015f1c66ea9bb"

cd "$ROOT_DIR"
test -f "$BUNDLE"
unzip -tq "$BUNDLE" >/dev/null
ACTUAL_SHA256="$(sha256sum "$BUNDLE" | awk '{print $1}')"
if [[ "$ACTUAL_SHA256" != "$EXPECTED_SHA256" ]]; then
  echo "diagnostic bundle changed: expected $EXPECTED_SHA256, got $ACTUAL_SHA256" >&2
  exit 1
fi

API_TESTS='Test(DiagnosticsWSRegressionDataset|CodexMappedDownstreamWebSocketQuotaRotatesAccountAndRestoresToolContext|CodexMappedConcurrentRootSerializesCommitAndReleasesCancelledWaiter|CodexMappedRecoveryRejectsUnpairedToolOutputWithoutCheckpoint|DownstreamResponsesWebSocketNeverLeaksRepeatedOrphanedToolOutput|GoalContinuityRejectsUnpairedCustomToolCallBeforeUpstream|DegradedReplayNeutralizesOrphanedToolOutput|DiagnosticsExportStreamsLargeUsageTableInBoundedWrites|DiagnosticsExportDeduplicatesAffinityAliasesAndOmitsExpiredRows|UsageJournalReplaysCrashAndRemainsIdempotent|UsageJournalConcurrentEnqueueAndShutdownDoesNotLoseEvents|GoalContinuitySchedulesBoundedCheckpointCompaction|GoalCompactionWorkerRequeuesChunksFairly|GoalResponseFromSSEPartsPreservesBeyondAliasSample|ForEachSSEFrameSpoolsFrameBeyondLegacyScannerLimit|ChatResponsesBridgeSpoolsLargeToolArguments|ResponsesAnthropicBridgeSpoolsLargeToolArguments)$'
STORAGE_TESTS='Test(DiagnosticSnapshotAllowsWritesAndKeepsStableView|RepairLegacyUsageEventsMatchesDiagnosticStateMachine|UsageEventConcurrentDuplicatesAreChargedOnce|UsageEventEstimateIsReplacedByLateRealUsage|AffinityAliasUsesTTLWithoutGrowingPrimaryBindings|AffinityBindingEpochChangesOnlyWhenRoutingIdentityChanges|CleanupCodexUpstreamAttemptsAggregatesExactlyOnceAndKeepsSevenDayDetail|GoalV2UsesBoundedEncryptedChunksAndAdvancesCheckpointWithoutRewriting)$'
SCHEDULER_TESTS='Test(SchedulerPollingRecoversLostCoordinatorNotification|CandidateIndexKeepsNormalSelectionConstantWork|CandidateIndexFallsBackOnlyWhenBothSamplesAreUnavailable|AffinityCacheTotalCapacityIsBounded)$'

GOTOOLCHAIN=local "$GO_BIN" test ./internal/api -run "$API_TESTS" -count=1
GOTOOLCHAIN=local "$GO_BIN" test ./internal/storage -run "$STORAGE_TESTS" -count=1
GOTOOLCHAIN=local "$GO_BIN" test ./internal/scheduler -run "$SCHEDULER_TESTS" -count=1
GOTOOLCHAIN=local "$GO_BIN" test ./internal/usagejournal -count=1
GOTOOLCHAIN=local "$GO_BIN" test ./internal/kiro -run 'Test(ResponseProcessorSpoolsLargeAccumulatedOutputAndCleansUp|ResponseProcessorRejectsAccumulatedOutputBeyondLimit)$' -count=1
echo "diagnostic replay passed: bundle=$EXPECTED_SHA256"
