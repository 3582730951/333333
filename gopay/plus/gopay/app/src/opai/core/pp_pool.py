"""Read/write PP自动注册 MySQL pool_emails — reuse registered, not-yet-Plus accounts."""
from __future__ import annotations

import os
from dataclasses import dataclass
from datetime import datetime, timezone
from typing import Any, Optional


@dataclass
class PoolAccount:
    id: int
    email: str
    access_token: str
    activation_status: str
    zero_dollar_offer: Optional[bool]


def _mysql_connect():
    try:
        import pymysql
    except ImportError as e:
        raise RuntimeError("pip install pymysql  （读取 PP 邮箱池需要）") from e

    cfg = {}
    try:
        from .pipeline import load_pipeline_config
        cfg = load_pipeline_config().get("pp_mysql") or {}
    except Exception:
        pass

    password = os.environ.get("PP_MYSQL_PASSWORD") or str(cfg.get("password") or "")
    if not password:
        raise RuntimeError("Set PP_MYSQL_PASSWORD or pp_mysql.password in config/pipeline.json")

    return pymysql.connect(
        host=os.environ.get("PP_MYSQL_HOST") or str(cfg.get("host") or "127.0.0.1"),
        port=int(os.environ.get("PP_MYSQL_PORT") or cfg.get("port") or 3306),
        user=os.environ.get("PP_MYSQL_USER") or str(cfg.get("user") or "root"),
        password=password,
        database=os.environ.get("PP_MYSQL_DATABASE") or str(cfg.get("database") or "plus_papay"),
        charset="utf8mb4",
        cursorclass=pymysql.cursors.DictCursor,
        autocommit=False,
    )


def _pending_where(require_free_offer: bool) -> tuple[str, list]:
    """SQL WHERE for registered ChatGPT accounts not yet Plus-activated."""
    clauses = [
        "registered = 1",
        "is_active = 1",
        "asset_ignored = 0",
        "in_use = 0",
        "openai_access_token IS NOT NULL",
        "LENGTH(TRIM(openai_access_token)) > 0",
        """COALESCE(activation_status, 'registered') IN (
            'registered', 'activation_failed', 'payment_mandate_cooldown'
        )""",
        """(
            activation_cooldown_until IS NULL
            OR activation_cooldown_until <= CURRENT_TIMESTAMP
        )""",
        """COALESCE(activation_status, 'registered') NOT IN (
            'activated', 'pending_protocol', 'activating',
            'no_offer', 'payment_no_paypal', 'phone_verify', 'needs_login'
        )""",
    ]
    if require_free_offer:
        # IDR 0 元试用：排除已确认无 offer；NULL=未检测仍可用
        clauses.append(
            "(zero_dollar_offer IS NULL OR zero_dollar_offer = 1)"
        )
    return " AND ".join(clauses), []


def pool_stats(*, require_free_offer: bool = True) -> dict[str, int]:
    conn = _mysql_connect()
    where, _ = _pending_where(require_free_offer)
    try:
        with conn.cursor() as cur:
            cur.execute(f"SELECT COUNT(*) AS n FROM pool_emails WHERE {where}")
            pending = int(cur.fetchone()["n"])
            cur.execute(
                "SELECT COUNT(*) AS n FROM pool_emails "
                "WHERE registered=1 AND is_active=1 "
                "AND COALESCE(activation_status,'registered')='activated'"
            )
            activated = int(cur.fetchone()["n"])
        return {"pending_plus": pending, "activated": activated}
    finally:
        conn.close()


def list_pending(
    *,
    limit: int = 20,
    require_free_offer: bool = True,
) -> list[dict[str, Any]]:
    conn = _mysql_connect()
    where, _ = _pending_where(require_free_offer)
    try:
        with conn.cursor() as cur:
            cur.execute(
                f"""
                SELECT id, email, activation_status, zero_dollar_offer,
                       zero_dollar_due_amount, registered_at, openai_access_token_updated_at
                FROM pool_emails
                WHERE {where}
                ORDER BY sort_order ASC, id ASC
                LIMIT %s
                """,
                (max(1, min(limit, 500)),),
            )
            rows = cur.fetchall()
        out = []
        for r in rows:
            out.append({
                "id": r["id"],
                "email": r["email"],
                "activation_status": r.get("activation_status") or "registered",
                "zero_dollar_offer": r.get("zero_dollar_offer"),
                "zero_dollar_due_amount": r.get("zero_dollar_due_amount") or "",
            })
        return out
    finally:
        conn.close()


def claim_next(
    *,
    email: str = "",
    require_free_offer: bool = True,
    lock: bool = True,
) -> Optional[PoolAccount]:
    """Pick one pool account; optionally mark in_use + activating."""
    conn = _mysql_connect()
    where, _ = _pending_where(require_free_offer)
    try:
        with conn.cursor() as cur:
            if email:
                cur.execute(
                    f"""
                    SELECT id, email, openai_access_token, activation_status, zero_dollar_offer
                    FROM pool_emails
                    WHERE email = %s AND {where}
                    FOR UPDATE
                    """,
                    (email.strip(),),
                )
            else:
                cur.execute(
                    f"""
                    SELECT id, email, openai_access_token, activation_status, zero_dollar_offer
                    FROM pool_emails
                    WHERE {where}
                    ORDER BY sort_order ASC, id ASC
                    LIMIT 1
                    FOR UPDATE
                    """,
                )
            row = cur.fetchone()
            if not row:
                conn.rollback()
                return None

            token = str(row.get("openai_access_token") or "").strip()
            if not token:
                conn.rollback()
                return None

            acct = PoolAccount(
                id=int(row["id"]),
                email=str(row["email"]),
                access_token=token,
                activation_status=str(row.get("activation_status") or "registered"),
                zero_dollar_offer=row.get("zero_dollar_offer"),
            )

            if lock:
                cur.execute(
                    """
                    UPDATE pool_emails
                    SET in_use = 1,
                        locked_at = CURRENT_TIMESTAMP,
                        activation_status = 'activating',
                        activation_last_error = NULL
                    WHERE id = %s
                    """,
                    (acct.id,),
                )
            conn.commit()
            return acct
    except Exception:
        conn.rollback()
        raise
    finally:
        conn.close()


def mark_result(
    pool_id: int,
    *,
    success: bool,
    error: str = "",
) -> None:
    conn = _mysql_connect()
    try:
        with conn.cursor() as cur:
            if success:
                cur.execute(
                    """
                    UPDATE pool_emails
                    SET in_use = 0,
                        locked_at = NULL,
                        activation_status = 'activated',
                        activated_at = CURRENT_TIMESTAMP,
                        activation_last_error = NULL
                    WHERE id = %s
                    """,
                    (pool_id,),
                )
            else:
                cur.execute(
                    """
                    UPDATE pool_emails
                    SET in_use = 0,
                        locked_at = NULL,
                        activation_status = 'activation_failed',
                        activation_last_error = %s
                    WHERE id = %s
                    """,
                    ((error or "gopay pipeline failed")[:2000], pool_id),
                )
        conn.commit()
    except Exception:
        conn.rollback()
        raise
    finally:
        conn.close()


def release_claim(pool_id: int, *, status: str = "registered") -> None:
    """Release lock without marking activated (e.g. checkout-only test)."""
    conn = _mysql_connect()
    allowed = {"registered", "activation_failed", "registered"}
    st = status if status in allowed else "registered"
    try:
        with conn.cursor() as cur:
            cur.execute(
                """
                UPDATE pool_emails
                SET in_use = 0, locked_at = NULL, activation_status = %s
                WHERE id = %s AND COALESCE(activation_status,'') = 'activating'
                """,
                (st, pool_id),
            )
        conn.commit()
    finally:
        conn.close()


def account_to_token_dict(acct: PoolAccount) -> dict[str, Any]:
    return {
        "email": acct.email,
        "access_token": acct.access_token,
        "pool_id": acct.id,
    }
