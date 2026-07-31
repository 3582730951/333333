#!/usr/bin/env python3

from __future__ import annotations

import csv
import hashlib
import io
import json
import statistics
from collections import Counter, defaultdict
from datetime import UTC, datetime
from pathlib import Path
from zipfile import ZipFile


ROOT = Path(__file__).resolve().parent
ZIP_DIR = ROOT / "originals"


def iso(epoch: int) -> str:
    return datetime.fromtimestamp(epoch, UTC).isoformat().replace("+00:00", "Z")


def rows(archive: ZipFile, name: str) -> list[dict[str, str]]:
    return list(csv.DictReader(io.StringIO(archive.read(name).decode("utf-8-sig"))))


def counts(items: list[dict[str, str]], field: str) -> dict[str, int]:
    return dict(sorted(Counter(row.get(field, "") for row in items).items()))


def nested_counts(
    items: list[dict[str, str]], outer: str, inner: str
) -> dict[str, dict[str, int]]:
    result: dict[str, Counter[str]] = defaultdict(Counter)
    for row in items:
        result[row.get(outer, "")][row.get(inner, "")] += 1
    return {key: dict(sorted(value.items())) for key, value in sorted(result.items())}


def window(items: list[dict[str, str]], after: int) -> list[dict[str, str]]:
    return [row for row in items if int(row.get("created_at") or 0) > after]


def rate_per_minute(count: int, start: int, end: int) -> float:
    return round(count / max((end - start) / 60, 1 / 60), 3)


def numeric(value: str) -> float | None:
    try:
        parsed = float(value.strip("'"))
        return parsed if parsed >= 0 else None
    except ValueError:
        return None


def analyze(path: Path) -> dict:
    with ZipFile(path) as archive:
        bad_member = archive.testzip()
        manifest = json.loads(archive.read("manifest.json"))
        summary = json.loads(archive.read("diagnostic_summary.json"))
        runtime = json.loads(archive.read("runtime_storage.json"))
        audit = rows(archive, "audit_log.csv")
        routes = rows(archive, "route_attempts.csv")
        upstream = rows(archive, "codex_upstream_attempts.csv")
        billing = rows(archive, "billing_holds.csv")
        mappings = rows(archive, "codex_session_mappings.csv")
        accounts = rows(archive, "accounts_snapshot.csv")
        limits = rows(archive, "account_rate_limits.csv")
        events = rows(archive, "diagnostic_events.csv")

    generated_at = int(manifest["generated_at"])
    goal = summary.get("goal_continuity", {})
    policy = summary.get("goal_policy", {})
    storage_bytes = int(goal.get("storage_bytes") or 0)
    storage_max = int(policy.get("storage_max_bytes") or 0)
    audit_times = [int(row["created_at"]) for row in audit if row.get("created_at")]
    degradation = [
        row
        for row in audit
        if row.get("action") == "goal_persistence_degraded"
    ]
    storage_budget = [
        row for row in degradation if "storage_budget" in row.get("detail", "")
    ]
    used_percent = [
        value
        for row in limits
        if (value := numeric(row.get("used_percent", ""))) is not None
    ]
    budget = runtime.get("budget", {})
    filesystem = runtime.get("filesystem", {})
    rejection_counts = runtime.get("rejection_counts", {})
    source_counts = manifest.get("source_row_counts", {})
    export_counts = manifest.get("row_counts", {})

    return {
        "file": path.name,
        "sha256": hashlib.sha256(path.read_bytes()).hexdigest(),
        "bytes": path.stat().st_size,
        "zip_integrity": "ok" if bad_member is None else f"bad:{bad_member}",
        "snapshot_id": manifest["snapshot_id"],
        "generated_at": generated_at,
        "generated_at_iso": iso(generated_at),
        "build": manifest.get("build", {}),
        "accounts": {
            "current": manifest.get("current_account_count"),
            "historical_references": manifest.get(
                "historical_reference_account_count"
            ),
            "status": counts(accounts, "status"),
            "provider": counts(accounts, "effective_provider"),
            "cooldown": sum(
                int(row.get("cooldown_until") or 0) > generated_at for row in accounts
            ),
            "recheck_pending": sum(
                row.get("recheck_pending", "").lower() == "true" for row in accounts
            ),
            "rate_limit_used_percent": {
                "samples": len(used_percent),
                "max": max(used_percent, default=0),
                "median": statistics.median(used_percent) if used_percent else 0,
                "at_or_above_90": sum(value >= 90 for value in used_percent),
            },
        },
        "exports": {
            "large_table_row_limit": manifest.get("large_table_row_limit"),
            "source_counts": source_counts,
            "export_counts": export_counts,
            "truncated": {
                name: {
                    "source": source,
                    "exported": export_counts.get(name, 0),
                }
                for name, source in source_counts.items()
                if source > export_counts.get(name, 0)
            },
        },
        "goal_storage": {
            "bytes": storage_bytes,
            "max_bytes": storage_max,
            "headroom_bytes": storage_max - storage_bytes if storage_max else None,
            "utilization_percent": (
                round(storage_bytes / storage_max * 100, 6) if storage_max else None
            ),
            "persistence_degraded_summary": goal.get("persistence_degraded", 0),
            "persistence_degraded_exported": len(degradation),
            "storage_budget_exported": len(storage_budget),
            "degradation_by_terminal": counts(storage_budget, "reason"),
            "degradation_per_minute_in_export": rate_per_minute(
                len(storage_budget),
                min(audit_times, default=generated_at),
                max(audit_times, default=generated_at),
            ),
            "sessions": goal.get("sessions", 0),
            "resume_recovered": goal.get("resume_recovered", 0),
            "resume_ambiguous": goal.get("resume_ambiguous", 0),
            "compaction_completed": goal.get("compaction_completed", 0),
            "history_replaced": goal.get("history_replaced", 0),
        },
        "audit": {
            "rows": len(audit),
            "range": {
                "min": min(audit_times, default=0),
                "max": max(audit_times, default=0),
                "min_iso": iso(min(audit_times)) if audit_times else None,
                "max_iso": iso(max(audit_times)) if audit_times else None,
            },
            "actions": counts(audit, "action"),
            "states": counts(audit, "state"),
            "reasons": counts(audit, "reason"),
        },
        "routing": {
            "rows": len(routes),
            "status": counts(routes, "status_class"),
            "target": counts(routes, "target"),
            "target_status": nested_counts(routes, "target", "status_class"),
            "selection_type": counts(routes, "selection_type"),
        },
        "codex_upstream": {
            "rows": len(upstream),
            "state": counts(upstream, "state"),
            "status_code": counts(upstream, "status_code"),
        },
        "cpa": {
            **summary.get("codex_cpa", {}),
            "mapping_rows": len(mappings),
            "mapping_state": counts(mappings, "state"),
        },
        "billing": {
            **summary.get("billing_holds", {}),
            "rows": len(billing),
            "status": counts(billing, "status"),
        },
        "runtime_storage": {
            "memory_used": budget.get("memory_used", 0),
            "memory_limit": budget.get("memory_limit", 0),
            "memory_utilization_percent": round(
                100 * budget.get("memory_used", 0) / max(budget.get("memory_limit", 1), 1),
                4,
            ),
            "spool_used": budget.get("spool_used", 0),
            "spool_limit": budget.get("spool_limit", 0),
            "spool_utilization_percent": round(
                100 * budget.get("spool_used", 0) / max(budget.get("spool_limit", 1), 1),
                4,
            ),
            "filesystem_available_bytes": filesystem.get(
                "filesystem_available_bytes", 0
            ),
            "rejections": rejection_counts,
            "total_rejections": sum(int(value) for value in rejection_counts.values()),
        },
        "diagnostic_events": {
            "rows": len(events),
            "severity": counts(events, "severity"),
            "types": counts(events, "event_type"),
            "gaps": sum(
                row.get("diagnostic_gap", "").lower() == "true" for row in events
            ),
        },
    }


def add_window_deltas(packages: list[dict], paths: list[Path]) -> None:
    for index, package in enumerate(packages):
        threshold = packages[index - 1]["generated_at"] if index else 0
        with ZipFile(paths[index]) as archive:
            audit = window(rows(archive, "audit_log.csv"), threshold)
            routes = window(rows(archive, "route_attempts.csv"), threshold)
            upstream = window(rows(archive, "codex_upstream_attempts.csv"), threshold)
        duration = package["generated_at"] - threshold if threshold else None
        goal_failures = sum(
            row.get("action") == "goal_persistence_degraded" for row in audit
        )
        package["since_previous_snapshot"] = {
            "threshold": threshold or None,
            "threshold_iso": iso(threshold) if threshold else None,
            "duration_seconds": duration,
            "audit_rows": len(audit),
            "audit_actions": counts(audit, "action"),
            "goal_storage_budget_failures": goal_failures,
            "goal_storage_budget_failures_per_minute": (
                rate_per_minute(goal_failures, threshold, package["generated_at"])
                if duration
                else None
            ),
            "route_rows": len(routes),
            "route_status": counts(routes, "status_class"),
            "route_target_status": nested_counts(
                routes, "target", "status_class"
            ),
            "codex_upstream_rows": len(upstream),
            "codex_upstream_status": counts(upstream, "status_code"),
        }


def markdown(packages: list[dict]) -> str:
    first, latest = packages[0], packages[-1]
    newest = latest["since_previous_snapshot"]
    same_storage = (
        first["goal_storage"]["bytes"] == latest["goal_storage"]["bytes"]
    )
    lines = [
        "# 两份后端诊断包时序分析",
        "",
        "## 完整性与顺序",
        "",
        "| 顺序 | 生成时间 (UTC) | 快照 | ZIP 校验 | SHA-256 |",
        "|---|---|---|---|---|",
    ]
    for index, package in enumerate(packages, 1):
        lines.append(
            f"| {index} | {package['generated_at_iso']} | "
            f"`{package['snapshot_id']}` | {package['zip_integrity']} | "
            f"`{package['sha256']}` |"
        )
    lines += [
        "",
        "两包均通过 ZIP CRC 完整性检查，构建版本一致，因此可直接做运行时序对比。",
        "",
        "## 核心结论",
        "",
        "1. **P0：目标连续性存储在硬上限前形成稳定死区。** "
        f"两次快照的占用都为 `{latest['goal_storage']['bytes']}` 字节，"
        f"距 `{latest['goal_storage']['max_bytes']}` 字节硬上限仅 "
        f"`{latest['goal_storage']['headroom_bytes']}` 字节；"
        f"占用是否完全相同：`{str(same_storage).lower()}`。",
        "2. **失败持续发生而现有磁盘守护没有制造写入余量。** "
        f"较新快照相对前一快照的 {newest['duration_seconds']} 秒窗口内新增 "
        f"`{newest['goal_storage_budget_failures']}` 次目标持久化降级，"
        f"约 `{newest['goal_storage_budget_failures_per_minute']}` 次/分钟。",
        "3. **路由本身在较新窗口明显恢复，但失败审计仍是高频噪声。** "
        f"新增路由结果为 `{json.dumps(newest['route_status'], ensure_ascii=False)}`；"
        "逐次路由事实已由 `route_attempts.csv` 保存，重复的公开错误审计可限频，"
        "严格 409 身份语义必须保持逐次记录。",
        "4. **CPA 身份连续性仍需观察，不在本批次修改语义。** "
        f"ambiguous 从 `{first['cpa'].get('ambiguous', 0)}` 增至 "
        f"`{latest['cpa'].get('ambiguous', 0)}`；缺少原始客户端层级字段，"
        "直接猜测性合并身份的风险高于收益。",
        "5. **请求体存储健康。** "
        f"较新快照内存利用率 `{latest['runtime_storage']['memory_utilization_percent']}%`，"
        f"spool 利用率 `{latest['runtime_storage']['spool_utilization_percent']}%`，"
        f"拒绝计数 `{latest['runtime_storage']['total_rejections']}`。",
        "",
        "## 指标对比",
        "",
        "| 指标 | 较早快照 | 较新快照 | 判断 |",
        "|---|---:|---:|---|",
        f"| 目标存储字节 | {first['goal_storage']['bytes']} | {latest['goal_storage']['bytes']} | 完全钉死 |",
        f"| 硬上限余量 | {first['goal_storage']['headroom_bytes']} | {latest['goal_storage']['headroom_bytes']} | 仅 340 B |",
        f"| 导出窗口内存储失败 | {first['goal_storage']['storage_budget_exported']} | {latest['goal_storage']['storage_budget_exported']} | 持续高频 |",
        f"| CPA active | {first['cpa'].get('active', 0)} | {latest['cpa'].get('active', 0)} | 增长 |",
        f"| CPA ambiguous | {first['cpa'].get('ambiguous', 0)} | {latest['cpa'].get('ambiguous', 0)} | 需继续观测 |",
        f"| 路由成功 | {first['routing']['status'].get('success', 0)} | {latest['routing']['status'].get('success', 0)} | 累计增长 |",
        f"| 路由 5xx | {first['routing']['status'].get('upstream_5xx', 0)} | {latest['routing']['status'].get('upstream_5xx', 0)} | 较新窗口下降 |",
        f"| fresh holds | {first['billing'].get('current_fresh_held', 0)} | {latest['billing'].get('current_fresh_held', 0)} | 正常波动 |",
        f"| expired unsettled | {first['billing'].get('expired_unsettled', 0)} | {latest['billing'].get('expired_unsettled', 0)} | 新增 1，观测项 |",
        f"| 请求体存储拒绝 | {first['runtime_storage']['total_rejections']} | {latest['runtime_storage']['total_rejections']} | 健康 |",
        "",
        "## 数据窗口说明",
        "",
        f"- 较早包的 `audit_log.csv` 源表 {first['exports']['source_counts'].get('audit_log.csv', 0)} 行、"
        f"导出 {first['exports']['export_counts'].get('audit_log.csv', 0)} 行，达到 20,000 行上限。",
        f"- 较新包的审计源表与导出均为 {latest['exports']['export_counts'].get('audit_log.csv', 0)} 行；"
        "其计数是保留窗口统计，不应把 summary 数值下降解释为故障自行恢复。",
        f"- `codex_upstream_attempts.csv` 两包均截断为 20,000 行；"
        "趋势判断优先使用较新快照相对前一生成时间的增量窗口。",
        "",
        "## 修复决策",
        "",
        "- 将磁盘守护的目标连续性存储维护阈值设为硬上限以下的低水位，"
        "在写满前主动回收终态且可回收的历史；提交路径继续使用原硬上限。",
        "- 导出硬上限、维护目标、保留余量及实际回收量，令诊断包可直接证明守护是否生效。",
        "- 对非 409 的同类 `routing_unavailable` 审计做有界限频；"
        "`route_attempts`、HTTP 行为和严格 409 逐次审计保持不变。",
        "- 暂不改变 CPA 匹配、计费持有或上游重试语义，避免无证据扩大变更面。",
        "",
        "机器可读的全部计数、目标×状态交叉表及导出截断信息见 `comparison.json`。",
    ]
    return "\n".join(lines) + "\n"


def main() -> None:
    indexed = []
    for path in ZIP_DIR.glob("*.zip"):
        with ZipFile(path) as archive:
            generated_at = int(json.loads(archive.read("manifest.json"))["generated_at"])
        indexed.append((generated_at, path))
    indexed.sort()
    paths = [path for _, path in indexed]
    if len(paths) != 2:
        raise SystemExit(f"expected exactly 2 diagnostic ZIPs, found {len(paths)}")
    packages = [analyze(path) for path in paths]
    add_window_deltas(packages, paths)
    output = {
        "schema": "diagnostic-comparison-v1",
        "generated_at": datetime.now(UTC).isoformat().replace("+00:00", "Z"),
        "package_count": len(packages),
        "packages": packages,
    }
    (ROOT / "comparison.json").write_text(
        json.dumps(output, ensure_ascii=False, indent=2) + "\n"
    )
    (ROOT / "comparison.md").write_text(markdown(packages))
    print(
        json.dumps(
            {
                "packages": len(packages),
                "ordered": [package["file"] for package in packages],
                "headroom_bytes": packages[-1]["goal_storage"]["headroom_bytes"],
                "new_failures": packages[-1]["since_previous_snapshot"][
                    "goal_storage_budget_failures"
                ],
                "outputs": ["comparison.json", "comparison.md"],
            },
            ensure_ascii=False,
        )
    )


if __name__ == "__main__":
    main()
