#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
GO_BIN="${GO_BIN:-go}"
BUNDLE="${DIAGNOSTIC_BUNDLE:-$ROOT_DIR/example_zip/codex-pool-diagnostics-v2.zip}"
EXPECTED_SHA256="${DIAGNOSTIC_BUNDLE_SHA256:-09b530cee7b04223e9bfc8b52b2307ef7daaeb8daa5f4d46b69015f1c66ea9bb}"
AUDIT_CSV="${DIAGNOSTIC_AUDIT_CSV:-$ROOT_DIR/example_zip/audit (2).csv}"
EXPECTED_AUDIT_SHA256="${DIAGNOSTIC_AUDIT_CSV_SHA256:-56416b7012ac1b35d786076ea0cf766f716825e322ee471af510bdd7f57529af}"

cd "$ROOT_DIR"
SOURCE_FINGERPRINT=""
if [[ -f "$BUNDLE" ]]; then
  command -v unzip >/dev/null 2>&1 || {
    echo "unzip is required to validate $BUNDLE" >&2
    exit 1
  }
  unzip -tq "$BUNDLE" >/dev/null
  ACTUAL_SHA256="$(sha256sum "$BUNDLE" | awk '{print $1}')"
  if [[ "$ACTUAL_SHA256" != "$EXPECTED_SHA256" ]]; then
    echo "diagnostic bundle changed: expected $EXPECTED_SHA256, got $ACTUAL_SHA256" >&2
    exit 1
  fi
  SOURCE_FINGERPRINT="zip:$ACTUAL_SHA256"
elif [[ -f "$AUDIT_CSV" ]]; then
  ACTUAL_SHA256="$(sha256sum "$AUDIT_CSV" | awk '{print $1}')"
  if [[ "$ACTUAL_SHA256" != "$EXPECTED_AUDIT_SHA256" ]]; then
    echo "diagnostic audit CSV changed: expected $EXPECTED_AUDIT_SHA256, got $ACTUAL_SHA256" >&2
    exit 1
  fi
  for marker in \
    codex_downstream_websocket_upgrade \
    codex_tool_context_unrecoverable \
    codex_session_mapping_commit_failed \
    goal_history_replaced \
    goal_checkpoint_committed \
    goal_alias_bound; do
    grep -Fq ",$marker," "$AUDIT_CSV" || {
      echo "diagnostic audit CSV is missing incident marker: $marker" >&2
      exit 1
    }
  done
  SOURCE_FINGERPRINT="audit_csv:$ACTUAL_SHA256"
else
  echo "no diagnostic source found: expected $BUNDLE or $AUDIT_CSV" >&2
  exit 1
fi

API_TESTS='Test(DiagnosticsWSRegressionDataset|RandomizedWebSocketContextLossRebuildsToolHistoryWhileRescueExportRuns|DiagnosticRescueExportDoesNotRequireBackgroundWorker|CodexMappedDownstreamWebSocketQuotaRotatesAccountAndRestoresToolContext|CodexMappedConcurrentRootSerializesCommitAndReleasesCancelledWaiter|CodexMappedRecoveryRejectsUnpairedToolOutputWithoutCheckpoint|DownstreamResponsesWebSocketNeverLeaksRepeatedOrphanedToolOutput|GoalContinuityRejectsUnpairedCustomToolCallBeforeUpstream|DegradedReplayNeutralizesOrphanedToolOutput|DiagnosticsExportStreamsLargeUsageTableInBoundedWrites|DiagnosticsExportDeduplicatesAffinityAliasesAndOmitsExpiredRows|UsageJournalReplaysCrashAndRemainsIdempotent|UsageJournalConcurrentEnqueueAndShutdownDoesNotLoseEvents|GoalContinuitySchedulesBoundedCheckpointCompaction|GoalCompactionWorkerRequeuesChunksFairly|GoalResponseFromSSEPartsPreservesBeyondAliasSample|ForEachSSEFrameSpoolsFrameBeyondLegacyScannerLimit|ChatResponsesBridgeSpoolsLargeToolArguments|ResponsesAnthropicBridgeSpoolsLargeToolArguments)$'
STORAGE_TESTS='Test(DiagnosticSnapshotAllowsWritesAndKeepsStableView|RepairLegacyUsageEventsMatchesDiagnosticStateMachine|UsageEventConcurrentDuplicatesAreChargedOnce|UsageEventEstimateIsReplacedByLateRealUsage|AffinityAliasUsesTTLWithoutGrowingPrimaryBindings|AffinityBindingEpochChangesOnlyWhenRoutingIdentityChanges|CleanupCodexUpstreamAttemptsAggregatesExactlyOnceAndKeepsSevenDayDetail|GoalV2UsesBoundedEncryptedChunksAndAdvancesCheckpointWithoutRewriting)$'
SCHEDULER_TESTS='Test(SchedulerPollingRecoversLostCoordinatorNotification|CandidateIndexKeepsNormalSelectionConstantWork|CandidateIndexFallsBackOnlyWhenBothSamplesAreUnavailable|AffinityCacheTotalCapacityIsBounded)$'

GOTOOLCHAIN=local "$GO_BIN" test ./internal/api -run "$API_TESTS" -count=1
GOTOOLCHAIN=local "$GO_BIN" test ./internal/storage -run "$STORAGE_TESTS" -count=1
GOTOOLCHAIN=local "$GO_BIN" test ./internal/scheduler -run "$SCHEDULER_TESTS" -count=1
GOTOOLCHAIN=local "$GO_BIN" test ./internal/usagejournal -count=1
GOTOOLCHAIN=local "$GO_BIN" test ./internal/kiro -run 'Test(ResponseProcessorSpoolsLargeAccumulatedOutputAndCleansUp|ResponseProcessorRejectsAccumulatedOutputBeyondLimit)$' -count=1
echo "diagnostic replay passed: source=$SOURCE_FINGERPRINT"
