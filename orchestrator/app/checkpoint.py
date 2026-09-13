"""checkpoint 接入与治理（§5.5）。

职责：
  1. `open_saver()` —— 按 DSN 打开 MySQL saver（M0 已验证首选档可用，见 ADR-2）；
  2. `ensure_setup()` —— **幂等**建表：先探测 `checkpoint_migrations` 是否存在，存在就跳过。
     （M0 实测：`setup()` 重复执行会打 `Table ... already exists`，所以不能每次启动无脑调。）
  3. `cleanup_expired_threads()` —— 保留期清理。

cleanup 的关键实现事实（M0/M3 实测）：**checkpoint 表没有时间戳列**，
时间藏在 `checkpoint` JSON 的 `$.ts` 里（形如 `2026-09-12T14:23:39.922481+00:00`，UTC）。
因此"超期 thread"必须先由 SQL 从 JSON 里取最大 ts 再判断，
真正的删除走官方 `adelete_thread()`（不自己 DELETE，避免漏掉 blobs/writes 或将来 schema 变化）。
"""

from __future__ import annotations

import asyncio
import re
from datetime import datetime, timedelta, timezone
from typing import Any

import aiomysql

_DSN_RE = re.compile(r"^mysql://([^:]+):(.*)@([^:/]+):(\d+)/(.+)$")


def parse_dsn(dsn: str) -> dict[str, Any]:
    """解析 mysql://user:pass@host:port/db（口令可能含特殊字符，用非贪婪前的最后一段判断）。"""
    m = _DSN_RE.match(dsn)
    if not m:
        raise ValueError("CHECKPOINT_DSN 形如 mysql://user:pass@host:port/db")
    user, pwd, host, port, db = m.groups()
    return {"user": user, "password": pwd, "host": host, "port": int(port), "db": db}


async def _connect(dsn: str):
    p = parse_dsn(dsn)
    return await aiomysql.connect(host=p["host"], port=p["port"], user=p["user"],
                                  password=p["password"], db=p["db"], autocommit=True)


async def open_saver(dsn: str):
    """返回 saver 的**异步上下文管理器**（调用方负责 with 生命周期 = 进程生命周期）。"""
    from langgraph.checkpoint.mysql.aio import AIOMySQLSaver

    return AIOMySQLSaver.from_conn_string(dsn)


async def ensure_setup(dsn: str) -> tuple[bool, str]:
    """幂等建表。返回 (是否执行了 setup, 说明)。

    判定依据用 `information_schema`：只要四张表齐了就当已初始化 ——
    比"捕获异常"更明确（异常可能来自网络/权限，不该被吞）。
    """
    required = {"checkpoints", "checkpoint_blobs", "checkpoint_writes", "checkpoint_migrations"}
    conn = await _connect(dsn)
    try:
        async with conn.cursor() as cur:
            await cur.execute(
                "SELECT table_name FROM information_schema.tables WHERE table_schema=%s", (parse_dsn(dsn)["db"],))
            existing = {r[0] for r in await cur.fetchall()}
    finally:
        conn.close()

    if required <= existing:
        return False, f"checkpoint 表已存在（{len(required)} 张），跳过 setup()"

    async with await open_saver(dsn) as saver:
        await saver.setup()
    return True, "checkpoint 表已创建（首次初始化）"


async def _expired_threads(dsn: str, days: int, limit: int = 500) -> list[str]:
    """超期 thread（按 checkpoint JSON 里的 ts 取每个 thread 的最新时间）。"""
    cutoff = (datetime.now(timezone.utc) - timedelta(days=days)).strftime("%Y-%m-%dT%H:%M:%S")
    conn = await _connect(dsn)
    try:
        async with conn.cursor() as cur:
            await cur.execute(
                """
                SELECT thread_id, MAX(JSON_UNQUOTE(JSON_EXTRACT(checkpoint, '$.ts'))) AS last_ts
                FROM checkpoints
                GROUP BY thread_id
                HAVING last_ts IS NOT NULL AND last_ts < %s
                ORDER BY last_ts ASC
                LIMIT %s
                """,
                (cutoff, limit),
            )
            return [r[0] for r in await cur.fetchall()]
    finally:
        conn.close()


async def cleanup_expired_threads(dsn: str, days: int) -> dict[str, Any]:
    """删除超过保留期的 thread（默认 ≥7 天，§5.5 / 19.8）。"""
    if not dsn:
        return {"skipped": True, "reason": "CHECKPOINT_DSN 未配置"}
    if days <= 0:
        return {"skipped": True, "reason": "保留期未开启"}

    threads = await _expired_threads(dsn, days)
    if not threads:
        return {"deleted": 0, "retention_days": days}

    deleted = 0
    async with await open_saver(dsn) as saver:
        for tid in threads:
            try:
                await saver.adelete_thread(tid)
                deleted += 1
            except Exception as exc:  # noqa: BLE001 —— 单个 thread 失败不该中断整轮清理
                print(f"[cleanup] 删除 thread {tid} 失败: {type(exc).__name__}: {exc}")
    return {"deleted": deleted, "retention_days": days, "scanned": len(threads)}


async def cleanup_loop(dsn: str, days: int, interval_s: int) -> None:
    """后台定时清理（M3 用应用内后台任务；生产可换成 cron/外部调度）。"""
    while True:
        try:
            res = await cleanup_expired_threads(dsn, days)
            print(f"[cleanup] checkpoint 清理：{res}")
        except Exception as exc:  # noqa: BLE001
            print(f"[cleanup] 清理任务异常（下轮重试）: {type(exc).__name__}: {exc}")
        await asyncio.sleep(max(60, interval_s))
