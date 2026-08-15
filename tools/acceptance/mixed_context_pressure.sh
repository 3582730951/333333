#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
GO_BIN="${GO_BIN:-go}"
FIXTURE_COUNT_PER_TIER="${FIXTURE_COUNT_PER_TIER:-4}"
FIXTURE_WORKERS="${FIXTURE_WORKERS:-4}"
RUN_TRANSPORT_LOAD="${RUN_TRANSPORT_LOAD:-0}"
KEEP_ARTIFACTS="${KEEP_ARTIFACTS:-1}"
RUN_DIR="${REPORT_DIR:-$(mktemp -d "${TMPDIR:-/tmp}/codex-pool-mixed-pressure.XXXXXX")}"
STARTED_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

cleanup() {
  if [[ "$KEEP_ARTIFACTS" != "1" && "$RUN_DIR" == "${TMPDIR:-/tmp}"/codex-pool-mixed-pressure.* ]]; then
    rm -rf -- "$RUN_DIR"
  fi
}
trap cleanup EXIT INT TERM

mkdir -p "$RUN_DIR/bin" "$RUN_DIR/fixtures" "$RUN_DIR/logs"
cd "$ROOT_DIR"

if ! "$GO_BIN" version | grep -q 'go1\.25\.12'; then
  echo "Go 1.25.12 is required; set GO_BIN to the pinned toolchain" >&2
  exit 1
fi

GOTOOLCHAIN=local "$GO_BIN" build -o "$RUN_DIR/bin/extreme-load" ./cmd/extreme-load

for tier in 128000 256000 1000000; do
  tier_dir="$RUN_DIR/fixtures/$tier"
  mkdir -p "$tier_dir"
  "$RUN_DIR/bin/extreme-load" generate \
    -dir "$tier_dir" \
    -count "$FIXTURE_COUNT_PER_TIER" \
    -tokens "$tier" \
    -tolerance 0.005 \
    -model gpt-5.6-sol \
    -encoding o200k_base \
    -profile mixed-agent \
    -workers "$FIXTURE_WORKERS" \
    >"$RUN_DIR/logs/fixtures-$tier.log" 2>&1
  "$RUN_DIR/bin/extreme-load" verify \
    -dir "$tier_dir" \
    -minimum "$FIXTURE_COUNT_PER_TIER" \
    -workers "$FIXTURE_WORKERS" \
    >>"$RUN_DIR/logs/fixtures-$tier.log" 2>&1
done

API_TESTS='Test(CodexOneMiBContextConcurrentAccountSwitchPreservesToolPairs|CodexDurableRootRecoversWhenBoundAccountIsAlreadyBenched|ClaudeOneMillionTokenConcurrentAccountSwitchPreservesToolUseResults|CodexMappedDownstreamWebSocketQuotaRotatesAccountAndRestoresToolContext|CodexMappedRiskAccountSwitchPreservesPairedToolContext|CodexSessionMappingSeparatesConcurrentCLIThreadsWithSharedWeakSession|GoalContinuitySeparatesConcurrentCLIThreadsWithSharedWeakSession|GoalContinuitySchedulesBoundedCheckpointCompaction|AutoKiroGPTHighPressureAdmitsKiroToFairPool|AutoKiroGPTMidConversationHandoffPreservesHistoryAndSticks|AutoKiroGPTMidConversationContextErrorTriggersNativeCodexThenReturnsToKiro|UserGroupHTTPHandlerFallsBackAcrossModelProviders|CustomProviderRuleFailoverRetriesDifferentAccount|CustomProviderSharedSkillsGetAndAgentsRoute|StandaloneSearchAutoRoutesMappedModelToResponsesCustomProvider|StandaloneSearchUserGroupFallsThroughUnsupportedProviderTier|AdminSkillsCompatDoctorReportsOfficialRawAndProviderTiers|GatewayDirectAllowsTwoHundredConcurrentStreams|GatewayDirectAllowsOneThousandConcurrentStreams|CodexSessionMappingAdvancesWindowAfterBodyTriggeredCompaction|ClaudeProOpus48UsesVirtualOneMillionAndCompactsBeforeNativeWindow)$'
SCHEDULER_TESTS='Test(ConcurrentMultiCLIRootChildAffinityUnderNewRequestPressure|UnifiedCodexParentAffinityStaysStickyWhileFakeAbsorbsIndependentLease|ProviderPressureSnapshotTriggersForLowAvailabilityAndOverFiftyPercentPressure|CodexPressureDoesNotUseSecondaryGroupOutletWhenPrimaryIsFull|CompactionSkipsLocalTokenBudget|RecoverableRequiredAccountCooldownReturnsWithoutStickyWait)$'

GOTOOLCHAIN=local "$GO_BIN" test ./internal/api -run "$API_TESTS" -count=1 -timeout=30m -v \
  >"$RUN_DIR/logs/api-mixed-pressure.log" 2>&1
GOTOOLCHAIN=local "$GO_BIN" test ./internal/scheduler -run "$SCHEDULER_TESTS" -count=1 -timeout=10m -v \
  >"$RUN_DIR/logs/scheduler-mixed-pressure.log" 2>&1
GOTOOLCHAIN=local "$GO_BIN" test ./cmd/extreme-load ./tools/e2e/codex-multiagent ./tools/e2e/cli-matrix -count=1 \
  >"$RUN_DIR/logs/cli-tools.log" 2>&1

if [[ "$RUN_TRANSPORT_LOAD" == "1" ]]; then
  for tier in 128000 256000 1000000; do
    REPORT_DIR="$RUN_DIR/transport-$tier" \
    TARGET_TOKENS="$tier" \
    FIXTURE_PROFILE=mixed-agent \
    FIXTURE_COUNT="${TRANSPORT_FIXTURE_COUNT:-32}" \
    TARGET_RPS="${TARGET_RPS:-100}" \
    LOAD_CONCURRENCY="${LOAD_CONCURRENCY:-256}" \
    LOAD_DURATION="${LOAD_DURATION:-10s}" \
    KEEP_ARTIFACTS=1 \
      tools/acceptance/extreme_load.sh >"$RUN_DIR/logs/transport-$tier.log" 2>&1
  done
fi

python3 - "$RUN_DIR" "$STARTED_AT" "$RUN_TRANSPORT_LOAD" <<'PY'
import json
import pathlib
import sys
from datetime import datetime, timezone

root = pathlib.Path(sys.argv[1])
tiers = []
for token_count in (128000, 256000, 1000000):
    manifest = json.loads((root / "fixtures" / str(token_count) / "manifest.json").read_text())
    actual = [item["tokens"] for item in manifest["fixtures"]]
    tiers.append({
        "target_tokens": token_count,
        "profile": manifest.get("profile"),
        "fixtures": len(actual),
        "verified_min_tokens": min(actual),
        "verified_max_tokens": max(actual),
        "tool_contracts": ["web_search", "function_call", "skill_call"],
    })

report = {
    "schema_version": 1,
    "started_at": sys.argv[2],
    "completed_at": datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
    "passed": True,
    "mode": "transport+cgroup" if sys.argv[3] == "1" else "deterministic-mock",
    "context_tiers": tiers,
    "covered": {
        "context_compaction_while_quota_switches": True,
        "high_concurrency_and_new_request_selection": True,
        "kiro_pressure_spillover": True,
        "other_provider_failover": True,
        "multiple_downstream_clis": True,
        "main_child_same_account_affinity": True,
        "tool_pair_recovery": True,
        "network_search_routing": True,
        "skills_routing": True,
    },
    "logs": sorted(str(path.relative_to(root)) for path in (root / "logs").iterdir()),
}
(root / "report.json").write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n")
print(json.dumps(report, ensure_ascii=False, indent=2))
PY

echo "mixed context pressure acceptance passed; report: $RUN_DIR/report.json"
