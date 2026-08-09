#!/usr/bin/env python3
import datetime as dt
import json
import sqlite3
import sys


def main() -> None:
    if len(sys.argv) != 2:
        raise SystemExit("usage: seed_legacy_demo.py DATABASE")
    db = sqlite3.connect(sys.argv[1])
    db.execute("PRAGMA foreign_keys=ON")
    now = int(dt.datetime(2026, 8, 9, 12, tzinfo=dt.timezone.utc).timestamp())

    groups = [
        ("cyber", "You are the production coding assistant.", "prepend", 1, 0, 0, "[]", "", "", "eg_direct_us", "eg_direct_us,eg_proxy_de", now, now),
        ("claude-team", "Prefer concise engineering answers.", "prepend", 1, 0, 0, "[]", "claude-sonnet-4-6", "high", "eg_proxy_de", "eg_proxy_de,eg_direct_us", now, now),
        ("staging", "Staging traffic only.", "prepend", 1, 0, 0, "[]", "", "", "eg_warp_jp", "eg_warp_jp", now, now),
    ]
    db.executemany(
        "INSERT OR REPLACE INTO groups(name,system_prompt,prompt_mode,system_prompt_apply_to_compaction,virtual_2m_enabled,model_instructions_enabled,model_instructions_files,force_model,force_effort,default_egress_id,egress_ids,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)",
        groups,
    )

    egresses = [
        ("eg_direct_us", "美国主出口", "direct", "", "", "us-east", "198.51.100.10", 1, "healthy", 42, 96, "", 0, 32, now, now, "", "", "ipv4", "demo", "{}"),
        ("eg_proxy_de", "德国住宅代理", "http_proxy", "http://127.0.0.1:41001", "", "eu-central", "203.0.113.20", 1, "healthy", 118, 89, "demo-ray-de", 0, 12, now, now, "none", "", "ipv4", "demo", "{}"),
        ("eg_warp_jp", "东京 WARP", "warp_proxy", "socks5h://127.0.0.1:41002", "", "ap-northeast", "192.0.2.30", 1, "degraded", 186, 71, "demo-ray-jp", now + 900, 8, now, now, "", "", "ipv4", "demo", "{}"),
        ("eg_sidecar", "Claude 指纹包装层", "curl_cffi_sidecar", "http://127.0.0.1:8790", "socks5h://127.0.0.1:41001", "eu-central", "203.0.113.20", 1, "healthy", 126, 87, "", 0, 16, now, now, "", "", "ipv4", "demo", json.dumps({"profile": "claude_bun"})),
    ]
    db.executemany(
        "INSERT OR REPLACE INTO egress_profiles(id,name,type,endpoint,chain_proxy,region,exit_ip,stream_capable,health,latency_millis,cf_score,last_cf_ray,cooldown_until,max_concurrency,created_at,updated_at,proxy_auth_mode,proxy_api_key,ip_mode,provider_key,dynamic_config_json) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
        egresses,
    )

    accounts = [
        ("acc_codex_primary", "Codex 生产主账号", "cyber", "org-demo-001", "user-demo-001", "codex.primary@example.test", "plus", "codex", "active", 0, 0, 0, "", now - 864000, now, "manual", "", "active", now + 86400 * 90, now - 120, ""),
        ("acc_codex_backup", "Codex 备用账号", "cyber", "org-demo-002", "user-demo-002", "codex.backup@example.test", "team", "codex", "active", 0, 0, 0, "", now - 700000, now, "manual", "", "active", now + 86400 * 60, now - 300, ""),
        ("acc_claude_max", "Claude Max 主账号", "claude-team", "claude-demo-001", "", "claude.max@example.test", "max", "claude", "active", 0, 0, 0, "", now - 650000, now, "manual", "", "active", now + 86400 * 45, now - 90, ""),
        ("acc_claude_cooldown", "Claude 冷却观察", "claude-team", "claude-demo-002", "", "claude.cooldown@example.test", "pro", "claude", "active", 0, 0, now + 1800, "usage_limit", now - 600000, now, "manual", "", "active", now + 86400 * 30, now - 180, ""),
        ("acc_kiro_builder", "Kiro Builder ID", "staging", "kiro-demo-001", "", "kiro@example.test", "builder-id", "kiro", "active", 0, 0, 0, "", now - 550000, now, "protocol_v2", "", "active", 0, now - 240, ""),
        ("acc_antigravity", "Antigravity 测试账号", "staging", "ag-demo-001", "", "antigravity@example.test", "free", "antigravity", "active", 0, 0, 0, "", now - 500000, now, "manual", "", "active", 0, now - 400, ""),
        ("acc_custom_degraded", "自定义供应商（降级）", "staging", "custom-demo-001", "", "custom@example.test", "metered", "custom", "active", 0, 0, now + 3600, "upstream_unreachable", now - 450000, now, "manual", "", "unknown", 0, 0, ""),
        ("acc_disabled", "已停用演示账号", "cyber", "disabled-demo", "", "disabled@example.test", "free", "codex", "disabled", 0, 0, 0, "manual_review", now - 400000, now, "manual", "", "expired", now - 86400, now - 86400, ""),
    ]
    db.executemany(
        "INSERT OR REPLACE INTO accounts(id,label,group_name,upstream_account_id,chatgpt_user_id,email,plan_type,provider,status,is_fedramp,ignore_rate_limit_controls,quarantine_until,quarantine_reason,created_at,updated_at,registration_method,phone,subscription_status,subscription_expires_at,last_validity_check_at,registration_task_id) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
        accounts,
    )

    bindings = [
        ("acc_codex_primary", "eg_direct_us", "eg_proxy_de", "", "jar-codex-1", 0, 0, now, now),
        ("acc_codex_backup", "eg_proxy_de", "eg_direct_us", "", "jar-codex-2", 0, 0, now, now),
        ("acc_claude_max", "eg_proxy_de", "eg_direct_us", "eg_sidecar", "jar-claude-1", 0, 0, now, now),
        ("acc_claude_cooldown", "eg_proxy_de", "eg_direct_us", "eg_sidecar", "jar-claude-2", now + 1800, 1, now, now),
        ("acc_kiro_builder", "eg_warp_jp", "eg_proxy_de", "", "jar-kiro", 0, 0, now, now),
        ("acc_antigravity", "eg_warp_jp", "eg_direct_us", "", "jar-ag", 0, 0, now, now),
        ("acc_custom_degraded", "eg_warp_jp", "", "", "jar-custom", now + 3600, 1, now, now),
        ("acc_disabled", "eg_direct_us", "", "", "jar-disabled", 0, 0, now, now),
    ]
    db.executemany(
        "INSERT OR REPLACE INTO account_egress_bindings(account_id,primary_egress_id,standby_egress_ids,sidecar_egress_id,cookie_jar_key,cooldown_until,recheck_pending,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)",
        bindings,
    )

    capabilities = [
        ("acc_codex_primary", "gpt-5.6-sol", "available", "unknown", "", 400000, 400000, 100, 360000, "visible", "", "demo", "{}", "probe", now - 120),
        ("acc_codex_backup", "gpt-5.5", "available", "unknown", "", 272000, 272000, 100, 240000, "visible", "", "demo", "{}", "probe", now - 300),
        ("acc_claude_max", "claude-sonnet-4-6", "available", "available", "probe", 1000000, 1000000, 100, 900000, "visible", "", "demo", "{}", "probe", now - 90),
        ("acc_claude_cooldown", "claude-opus-4-6", "available", "unknown", "", 200000, 1000000, 100, 180000, "visible", "", "demo", "{}", "probe", now - 180),
        ("acc_kiro_builder", "claude-sonnet-4-5", "available", "unknown", "", 200000, 200000, 100, 180000, "visible", "", "demo", "{}", "probe", now - 240),
        ("acc_antigravity", "gemini-2.5-pro", "available", "unknown", "", 1000000, 1000000, 100, 900000, "visible", "", "demo", "{}", "probe", now - 400),
    ]
    db.executemany(
        "INSERT OR REPLACE INTO account_model_capabilities(account_id,model_slug,availability_state,context_1m_state,context_1m_source,native_context_window,native_max_context_window,effective_context_window_percent,auto_compact_token_limit,visibility,etag,raw_model_json_hash,raw_model_json,source,last_probe_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
        capabilities,
    )

    limits = [
        ("acc_codex_primary", "codex", "gpt-5.6-sol", "weekly", "headers", 37.5, 10000000, 6250000, -1, -1, now + 86400 * 4, "ok", "{}", now),
        ("acc_codex_backup", "codex", "gpt-5.5", "weekly", "headers", 64.0, 6000000, 2160000, -1, -1, now + 86400 * 2, "warning", "{}", now),
        ("acc_claude_max", "claude", "claude-sonnet-4-6", "five_hour", "headers", 22.0, 3000000, 2340000, -1, -1, now + 7200, "ok", "{}", now),
        ("acc_claude_cooldown", "claude", "claude-opus-4-6", "five_hour", "headers", 100.0, 2000000, 0, -1, -1, now + 1800, "limited", "{}", now),
        ("acc_kiro_builder", "kiro", "claude-sonnet-4-5", "credits", "body", 71.0, 1000, 290, -1, -1, now + 86400 * 20, "warning", "{}", now),
    ]
    db.executemany(
        "INSERT OR REPLACE INTO account_rate_limits(account_id,provider,model,limiter_type,source,used_percent,limit_tokens,remaining_tokens,limit_requests,remaining_requests,reset_at,status,raw_json,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
        limits,
    )

    models = {
        "acc_codex_primary": ("codex", "gpt-5.6-sol", 125000, 22000, 91000),
        "acc_codex_backup": ("codex", "gpt-5.5", 82000, 16000, 42000),
        "acc_claude_max": ("claude", "claude-sonnet-4-6", 160000, 28000, 132000),
        "acc_claude_cooldown": ("claude", "claude-opus-4-6", 98000, 18000, 25000),
        "acc_kiro_builder": ("kiro", "claude-sonnet-4-5", 61000, 12000, 33000),
        "acc_antigravity": ("antigravity", "gemini-2.5-pro", 73000, 14000, 41000),
        "acc_custom_degraded": ("custom", "demo-large-v2", 42000, 8000, 6000),
    }
    for day in range(7):
        for idx, (account_id, (provider, model, prompt, completion, cached)) in enumerate(models.items()):
            scale = 7 - day
            created = now - day * 86400 - idx * 900
            total = (prompt + completion) * scale
            db.execute(
                "INSERT INTO usage_records(usage_event_id,account_id,route_key_hash,model,prompt_tokens,completion_tokens,total_tokens,cached_tokens,cache_read_tokens,cache_creation_tokens,usage_provider,usage_source,cache_read_present,cache_creation_present,cache_capability,estimated,cache_miss_tokens,cache_total_input_tokens,cache_creation_5m_tokens,cache_creation_1h_tokens,affinity_source,prompt_cache_key_present,prompt_cache_key_source,stable_prefix_source,stable_prefix_reason,stable_prefix_bytes,retention_effective,retention_source,claude_cache_ttl,cache_control_injected,cache_breakpoint_count,cache_breakpoints_json,unwritten_tail_tokens,max_possible_cache_read_tokens,cache_hit_after_prewarm,singleflight_waited_requests,diagnostics_miss_reason,route_epoch,kiro_credits,kiro_credits_present,requested_model,resolved_model,model_override_source,raw_usage_json,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
                (f"demo-{day}-{idx}", account_id, f"route-{idx}", model, prompt * scale, completion * scale, total, cached * scale, cached * scale, max(0, (prompt - cached) * scale // 4), provider, "upstream", 1, 1, "reported", 0, max(0, (prompt - cached) * scale), prompt * scale, 0, 0, "session", 1, "native", "system", "stable", 4096 + idx * 256, "1h" if provider == "claude" else "", "config", "1h" if provider == "claude" else "", 1 if provider == "claude" else 0, 2 if provider == "claude" else 0, "[]", 512, cached * scale, 1 if day % 2 == 0 else 0, idx % 3, "" if cached else "prefix_changed", day + 1, 14.5 * scale if provider == "kiro" else 0, 1 if provider == "kiro" else 0, model, model, "none", "{}", created),
            )

    audits = [
        ("acc_codex_primary", "Codex 生产主账号", "health_probe", "ok", "200", "模型探测成功，延迟 438ms", now - 60),
        ("acc_claude_max", "Claude Max 主账号", "claude_cache_diagnostics", "ok", "cache_hit", "缓存命中率 82.5%", now - 120),
        ("acc_claude_cooldown", "Claude 冷却观察", "rate_limit", "cooldown", "usage_limit", "进入 30 分钟冷却窗口", now - 180),
        ("acc_kiro_builder", "Kiro Builder ID", "quota_poll", "warning", "credits_low", "剩余 290 credits", now - 240),
        ("acc_custom_degraded", "自定义供应商（降级）", "egress_recheck", "degraded", "timeout", "东京出口连续两次超时", now - 300),
        ("", "", "settings_update", "ok", "recommended_template", "已应用全模型稳定推荐配置", now - 360),
    ]
    db.executemany("INSERT INTO audit_log(account_id,account_label,action,state,reason,detail,created_at) VALUES(?,?,?,?,?,?,?)", audits)

    cf_rows = [
        ("acc_codex_backup", "eg_proxy_de", 403, "demo-ray-001", "challenge", "Cloudflare challenge detected", now - 420),
        ("acc_kiro_builder", "eg_warp_jp", 429, "demo-ray-002", "rate_limit", "Edge rate limit; failover scheduled", now - 720),
        ("acc_custom_degraded", "eg_warp_jp", 503, "demo-ray-003", "edge", "Tokyo edge unavailable", now - 1080),
    ]
    db.executemany("INSERT INTO cf_events(account_id,egress_id,status,cf_ray,category,message,created_at) VALUES(?,?,?,?,?,?,?)", cf_rows)

    db.execute("INSERT OR REPLACE INTO tenants(id,name,created_at,updated_at) VALUES(?,?,?,?)", ("tenant_demo", "演示租户", now, now))
    users = [
        ("user_admin", "tenant_demo", "admin@example.test", "演示管理员", "admin", "active", "", now, now),
        ("user_ops", "tenant_demo", "ops@example.test", "值班工程师", "user", "active", "", now, now),
    ]
    db.executemany("INSERT OR REPLACE INTO users(id,tenant_id,email,name,role,status,password_hash,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)", users)
    db.execute("INSERT OR REPLACE INTO user_groups(id,name,system_prompt,prompt_mode,system_prompt_apply_to_compaction,model_instructions_enabled,model_instructions_files,model_instruction_profiles,force_model,force_effort,block_claude_target_groups,block_gpt_target_groups,model_routing_json,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)", ("ug_demo", "演示用户组", "", "prepend", 1, 0, "[]", "{}", "", "", "[]", "[]", "[]", now, now))
    db.execute("INSERT OR REPLACE INTO api_keys(key_hash,tenant_id,project_id,key_type,label,group_name,force_model,force_effort,provider_hint,enabled,expires_at,last_used_at,secret,created_at,updated_at,user_id,user_group_id) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)", ("demo_hash_no_secret", "tenant_demo", "", "downstream", "演示 CLI Key", "cyber", "", "", "auto", 1, 0, now - 120, "", now, now, "user_ops", "ug_demo"))

    quality = [
        ("cyber", "gpt-5.6-sol", "codex", "healthy", "pass", now - 600, now - 600, 0, 0, 18, 193000, "probe-codex", "OK", "OK", "gpt-5.6-sol", 512, now),
        ("claude-team", "claude-sonnet-4-6", "claude", "healthy", "pass", now - 900, now - 900, 0, 0, 15, 166000, "probe-claude", "OK", "OK", "claude-sonnet-4-6", 684, now),
        ("staging", "demo-large-v2", "custom", "degraded", "mismatch", now - 1200, now - 7200, 2, 1, 9, 72000, "probe-custom", "READY", "PARTIAL", "demo-large-v1", 2104, now),
    ]
    db.executemany("INSERT OR REPLACE INTO model_quality_status(group_name,model_slug,provider,state,last_outcome,last_probe_at,last_pass_at,consecutive_anomalies,consecutive_errors,total_checks,total_tokens,last_probe_id,last_expected,last_actual,last_returned_model,last_latency_ms,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)", quality)

    db.executemany(
        "INSERT OR REPLACE INTO settings(key,value,updated_at) VALUES(?,?,?)",
        [
            ("egress_fingerprint_engine", json.dumps("sidecar"), now),
            ("claude_ja3", json.dumps("chrome"), now),
            ("claude_force_direct", json.dumps(True), now),
            ("conversation_isolation", json.dumps(True), now),
        ],
    )
    db.executemany(
        "INSERT OR REPLACE INTO registration_stats_daily(date,platform,method,provider_key,total,succeeded,failed,cost_usd) VALUES(?,?,?,?,?,?,?,?)",
        [
            ("2026-08-09", "codex", "protocol_v2", "demo", 12, 10, 2, 1.84),
            ("2026-08-09", "claude", "manual", "demo", 4, 4, 0, 0.0),
            ("2026-08-08", "codex", "browser_v3", "demo", 9, 7, 2, 2.21),
        ],
    )
    db.commit()
    counts = {}
    for table in ["accounts", "groups", "egress_profiles", "account_egress_bindings", "account_model_capabilities", "account_rate_limits", "usage_records", "audit_log", "cf_events", "users", "api_keys", "model_quality_status", "registration_stats_daily"]:
        counts[table] = db.execute(f"SELECT count(*) FROM {table}").fetchone()[0]
    print(json.dumps(counts, ensure_ascii=False, sort_keys=True))


if __name__ == "__main__":
    main()
