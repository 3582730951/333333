#!/usr/bin/env python3
"""Produce a privacy-preserving goal/context diagnosis from a v3 support ZIP."""

from __future__ import annotations

import argparse
import collections
import csv
import hashlib
import io
import json
import zipfile
from pathlib import Path
from typing import Any, Iterable


def rows(archive: zipfile.ZipFile, name: str) -> list[dict[str, str]]:
    with archive.open(name) as source:
        text = io.TextIOWrapper(source, encoding="utf-8", newline="")
        return list(csv.DictReader(text))


def integer(value: str | None) -> int:
    try:
        return int(value or 0)
    except (TypeError, ValueError):
        return 0


def counts(values: Iterable[str]) -> dict[str, int]:
    return dict(sorted(collections.Counter(values).items()))


def analyze(path: Path) -> dict[str, Any]:
    raw = path.read_bytes()
    with zipfile.ZipFile(io.BytesIO(raw)) as archive:
        corrupt = archive.testzip()
        if corrupt:
            raise ValueError(f"CRC failure in {corrupt}")
        names = archive.namelist()
        manifest = json.loads(archive.read("manifest.json"))
        summary = json.loads(archive.read("diagnostic_summary.json"))
        audit = rows(archive, "audit_log.csv")
        mappings = rows(archive, "codex_session_mappings.csv")
        usage = rows(archive, "usage_records.csv")
        settings_rows = rows(archive, "settings.csv")
        billing_holds = rows(archive, "billing_holds.csv")
        runtime_storage = json.loads(archive.read("runtime_storage.json"))

    action_counts = collections.Counter(row["action"] for row in audit)
    degraded_codes: collections.Counter[str] = collections.Counter()
    for row in audit:
        if row["action"] != "goal_persistence_degraded":
            continue
        detail = row.get("detail", "")
        code = "unknown"
        for item in detail.split():
            if item.startswith("error_code="):
                code = item.split("=", 1)[1]
                break
        degraded_codes[code] += 1

    api_key_rows: dict[str, int] = collections.Counter(row["api_key_hash"] for row in usage)
    route_keys_by_api: dict[str, set[str]] = collections.defaultdict(set)
    for row in usage:
        route_keys_by_api[row["api_key_hash"]].add(row["route_key_hash"])
    api_keys_by_route: dict[str, set[str]] = collections.defaultdict(set)
    for row in usage:
        api_keys_by_route[row["route_key_hash"]].add(row["api_key_hash"])

    model_rows = collections.Counter(row["model"] for row in usage)
    max_prompt_by_model: dict[str, int] = collections.defaultdict(int)
    max_total_input_by_model: dict[str, int] = collections.defaultdict(int)
    max_total_by_model: dict[str, int] = collections.defaultdict(int)
    for row in usage:
        model = row["model"]
        max_prompt_by_model[model] = max(max_prompt_by_model[model], integer(row["prompt_tokens"]))
        max_total_input_by_model[model] = max(
            max_total_input_by_model[model], integer(row["cache_total_input_tokens"])
        )
        max_total_by_model[model] = max(max_total_by_model[model], integer(row["total_tokens"]))
    gpt56_over_trigger = sum(
        1
        for row in usage
        if row["model"].startswith("gpt-5.6") and integer(row["prompt_tokens"]) >= 334_800
    )
    gpt56_estimated_over_window = sum(
        1
        for row in usage
        if row["model"].startswith("gpt-5.6")
        and integer(row.get("estimated")) > 0
        and integer(row["prompt_tokens"]) > 372_000
    )
    gpt56_upstream_over_window = sum(
        1
        for row in usage
        if row["model"].startswith("gpt-5.6")
        and integer(row.get("estimated")) == 0
        and integer(row["prompt_tokens"]) > 372_000
    )
    estimated_usage = [row for row in usage if integer(row.get("estimated")) > 0]
    max_estimated_prompt = max(
        (integer(row.get("prompt_tokens")) for row in estimated_usage), default=0
    )

    cpa = summary.get("codex_cpa", {})
    settings = {row["key"]: row["value"] for row in settings_rows}
    goal_policy = summary.get("goal_policy") or {}
    goal_storage_max_mb = goal_policy.get("storage_max_mb")
    if goal_storage_max_mb is None:
        goal_storage_max_mb = settings.get("goal_storage_max_mb")

    namespace_count = len({row["namespace_hmac_prefix"] for row in mappings})
    tree_count = len({row["tree_hmac_prefix"] for row in mappings})
    storage_budget_failures = degraded_codes["storage_budget"]
    hold_statuses = collections.Counter(row["status"] for row in billing_holds)
    findings: list[dict[str, Any]] = []

    if storage_budget_failures:
        findings.append(
            {
                "id": "goal_storage_budget_exhaustion",
                "severity": "critical",
                "evidence": {
                    "persistence_degraded_storage_budget": storage_budget_failures,
                    "goal_storage_max_mb": goal_storage_max_mb,
                },
                "required_regression": (
                    "authoritative compaction/edit replaces durable history and "
                    "reclaims bytes without changing the downstream terminal"
                ),
            }
        )
    if gpt56_estimated_over_window:
        findings.append(
            {
                "id": "estimated_usage_exceeds_gpt_5_6_context",
                "severity": "high",
                "evidence": {
                    "estimated_rows_over_372000": gpt56_estimated_over_window,
                    "upstream_rows_over_372000": gpt56_upstream_over_window,
                    "max_estimated_prompt_tokens": max_estimated_prompt,
                },
                "required_regression": (
                    "missing-upstream-usage fallback is bounded by the advertised "
                    "model context without modifying the CLI request or response"
                ),
            }
        )
    if goal_storage_max_mb is None:
        findings.append(
            {
                "id": "diagnostics_missing_effective_goal_policy",
                "severity": "high",
                "evidence": {
                    "goal_storage_max_mb_in_settings": False,
                    "goal_policy_in_summary": False,
                },
                "required_regression": (
                    "support ZIP exports effective non-secret Goal policy even when "
                    "the value came from bootstrap config"
                ),
            }
        )
    if tree_count >= 10 and namespace_count <= max(2, len(api_key_rows)):
        findings.append(
            {
                "id": "codex_namespace_cardinality_risk",
                "severity": "high",
                "evidence": {
                    "distinct_trees": tree_count,
                    "distinct_namespaces": namespace_count,
                    "downstream_api_keys": len(api_key_rows),
                },
                "required_regression": (
                    "protocol-native client/session identity scopes CPA and Goal "
                    "aliases while remaining zero-config for the downstream CLI"
                ),
            }
        )
    model_fallback_errors = action_counts["model_fallback_required"]
    if model_fallback_errors:
        audit_truncation = (manifest.get("truncated_tables") or {}).get(
            "audit_log.csv", {}
        )
        findings.append(
            {
                "id": "model_fallback_audit_amplification",
                "severity": "medium",
                "evidence": {
                    "audit_rows": model_fallback_errors,
                    "capability_rejected_rows": action_counts[
                        "model_capability_rejected"
                    ],
                    "audit_rows_omitted_from_bundle": audit_truncation.get(
                        "omitted_rows", 0
                    ),
                },
                "classification": (
                    "speculative target misses and a duplicate rejection action "
                    "amplify pool availability evidence; preserve transparent "
                    "target fallback and change diagnostics only"
                ),
            }
        )

    return {
        "schema": "codex-pool.goal-context-diagnosis.v2",
        "archive": {
            "name": path.name,
            "sha256": hashlib.sha256(raw).hexdigest(),
            "crc_ok": True,
            "file_count": len(names),
            "format": manifest.get("format"),
            "generated_at": manifest.get("generated_at"),
            "build": manifest.get("build", {}),
            "declared_row_counts": manifest.get("row_counts", {}),
            "source_row_counts": manifest.get("source_row_counts", {}),
            "truncated_tables": manifest.get("truncated_tables", {}),
        },
        "codex_cpa": {
            **cpa,
            "distinct_namespace_prefixes": namespace_count,
            "distinct_tree_prefixes": tree_count,
        },
        "goal_audit": {
            "actions": {
                key: action_counts[key]
                for key in sorted(action_counts)
                if key.startswith("goal_") or key.startswith("codex_context_")
            },
            "persistence_degraded_error_codes": dict(sorted(degraded_codes.items())),
        },
        "shared_downstream": {
            "api_key_count": len(api_key_rows),
            "usage_rows_per_api_key_desc": sorted(api_key_rows.values(), reverse=True),
            "distinct_route_keys_per_api_key_desc": sorted(
                (len(value) for value in route_keys_by_api.values()), reverse=True
            ),
            "route_keys_observed_under_multiple_api_keys": sum(
                1 for value in api_keys_by_route.values() if len(value) > 1
            ),
            "affinity_sources": counts(row["affinity_source"] for row in usage),
        },
        "context_pressure": {
            "usage_rows": len(usage),
            "model_rows": dict(sorted(model_rows.items())),
            "max_prompt_tokens_by_model": dict(sorted(max_prompt_by_model.items())),
            "max_total_input_tokens_by_model": dict(
                sorted(max_total_input_by_model.items())
            ),
            "max_total_tokens_by_model": dict(sorted(max_total_by_model.items())),
            "gpt_5_6_rows_at_or_above_334800": gpt56_over_trigger,
            "gpt_5_6_estimated_rows_above_372000": gpt56_estimated_over_window,
            "gpt_5_6_upstream_rows_above_372000": gpt56_upstream_over_window,
            "max_estimated_prompt_tokens": max_estimated_prompt,
        },
        "storage_policy_evidence": {
            "goal_storage_max_mb_exported": goal_storage_max_mb is not None,
            "goal_storage_max_mb_value": goal_storage_max_mb,
            "goal_policy": goal_policy,
            "payload_compression_migration": settings.get(
                "context_payload_compression_ctx2"
            ),
            "storage_accounting_migration": settings.get(
                "goal_continuity_v2_storage_accounted"
            ),
        },
        "billing_holds": {
            "rows": len(billing_holds),
            "statuses": dict(sorted(hold_statuses.items())),
            "currently_held": hold_statuses["held"],
        },
        "runtime_storage": {
            "rejection_counts": runtime_storage.get("rejection_counts", {}),
            "policy": runtime_storage.get("policy", {}),
        },
        "findings": findings,
    }


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("archive", type=Path)
    parser.add_argument("-o", "--output", type=Path)
    args = parser.parse_args()
    result = analyze(args.archive)
    encoded = json.dumps(result, ensure_ascii=False, indent=2, sort_keys=True) + "\n"
    if args.output:
        args.output.parent.mkdir(parents=True, exist_ok=True)
        args.output.write_text(encoded, encoding="utf-8")
    else:
        print(encoded, end="")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
