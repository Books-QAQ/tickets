#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""M2 端到端验收：工具层 + 身份与越权矩阵（打真实 Go + MySQL + 编排层）。

验收口径（§17 M2）：
  A. 越权矩阵 5 项：本人查得到 / 他人查 0 条 / 无身份引导登录 / 越权订单号统一"未查询到" / 伪造 token 401
  B. 退票费与人工核对一致（≥10 样例）—— Go 单测的样例表为准，这里核对真实链路里的金额
  C. 工具不可用独立计数（不能与"知识库覆盖不足"混口径）
"""

from __future__ import annotations

import json
import os
import sys

import httpx

ORCH = os.environ.get("ORCH_URL", "http://127.0.0.1:8001")
GO = os.environ.get("GO_URL", "http://127.0.0.1:8080")
KEY = os.environ.get("INTERNAL_KEY", "dev-internal-key")

USER_A = int(os.environ["USER_A"])
USER_B = int(os.environ["USER_B"])
ORDER_A_PAID = "11111111-1111-1111-1111-111111111111"
ORDER_A_PENDING = "22222222-2222-2222-2222-222222222222"
ORDER_A_REFUNDED = "33333333-3333-3333-3333-333333333333"
ORDER_B = "44444444-4444-4444-4444-444444444444"
ORDER_A_DEPARTED = "55555555-5555-5555-5555-555555555555"

results: list[tuple[str, bool, str]] = []


def ask(question: str, *, user_id: int | None, guest_key: str | None = None) -> dict:
    body: dict = {"question": question, "stream": False, "conv_id": f"e2e-{abs(hash(question)) % 10**8}"}
    if user_id:
        body["user_id"] = user_id
    if guest_key:
        body["guest_key"] = guest_key
    with httpx.Client(base_url=ORCH, trust_env=False, timeout=60) as c:
        r = c.post("/ask", json=body, headers={"X-Internal-Key": KEY})
        r.raise_for_status()
        return r.json()


def check(name: str, ok: bool, detail: str = "") -> None:
    results.append((name, ok, detail))
    print(f"[{'PASS' if ok else 'FAIL'}] {name}" + (f" —— {detail}" if detail else ""))


def main() -> int:
    print("=== A. 越权矩阵（§8.5.5）===")

    # 1) 本人查得到
    out = ask("我的车票", user_id=USER_A)
    ok = out["tool_name"] == "my_tickets" and "号座" in out["answer"] and not out["transfer"]
    check("A1 本人查得到自己的车票", ok, f"tool={out['tool_name']} answer={out['answer'][:60]!r}")

    # 2) 他人查 0 条（同问法换用户 B）
    out = ask("我的车票", user_id=USER_B)
    ok = out["tool_kind"] == "empty" and "没有车票" in out["answer"]
    check("A2 他人无车票 → 空结果（不串号）", ok, f"kind={out['tool_kind']} answer={out['answer'][:40]!r}")

    # 3) 越权订单号 → 统一"未查询到"（不返回 403、不泄露存在性）
    out = ask(f"帮我查订单 {ORDER_B}", user_id=USER_A)
    ok = out["tool_kind"] == "not_found" and "未查询到该订单" in out["answer"]
    check("A3 越权订单号 → 统一未查询到", ok, f"kind={out['tool_kind']} answer={out['answer'][:40]!r}")

    # 4) 无身份（游客）→ 引导登录（不返回空结果混淆视听）
    out = ask("我的车票", user_id=None, guest_key="e2e-guest-device")
    ok = out["tool_kind"] == "guest_required" and "请先登录" in out["answer"] and not out["transfer"]
    check("A4 游客调个人工具 → 引导登录", ok, f"kind={out['tool_kind']} answer={out['answer'][:40]!r}")

    # 5) 伪造 token → 401（由既有 AuthMiddleware 保证；这里打同一服务的受保护端点）
    with httpx.Client(base_url=GO, trust_env=False, timeout=20) as c:
        r1 = c.get("/tickets", headers={"Authorization": "Bearer forged.token.value"})
        r2 = c.get("/tickets")  # 无 Authorization
    check("A5a 伪造 token → 401", r1.status_code == 401, f"status={r1.status_code}")
    check("A5b 无 token → 401", r2.status_code == 401, f"status={r2.status_code}")

    # 6) 内网契约面的白名单/密钥（工具层入口）
    with httpx.Client(base_url=GO, trust_env=False, timeout=20) as c:
        bad = c.post("/internal/tools/exec_tool", json={"args": {}}, headers={"X-Internal-Key": KEY})
        nokey = c.post("/internal/tools/exec_tool", json={"args": {}})
    check("A6a 未注册工具名 → 400（白名单）", bad.status_code == 400, f"status={bad.status_code}")
    check("A6b 内网面无密钥 → 401", nokey.status_code == 401, f"status={nokey.status_code}")

    print("\n=== B. 工具功能（真实数据）===")

    out = ask("我还有没付款的订单吗", user_id=USER_A)
    ok = out["tool_name"] == "unpaid_orders" and "剩余支付时间" in out["answer"]
    check("B1 待支付订单 + 剩余时间", ok, f"answer={out['answer'][:60]!r}")

    out = ask(f"退款进度查一下订单 {ORDER_A_REFUNDED}", user_id=USER_A)
    ok = out["tool_name"] == "refund_progress" and "已退款" in out["answer"]
    check("B2 退款进度（本地状态）", ok, f"answer={out['answer'][:70]!r}")

    out = ask(f"订单 {ORDER_A_PAID} 详情", user_id=USER_A)
    ok = out["tool_name"] == "order_detail" and "120" in out["answer"] and "3f2a" not in out["answer"]
    check("B3 订单详情（订单号已脱敏）", ok, f"answer={out['answer'][:70]!r}")

    out = ask("明天从 Shanghai South 到 Ningbo South 还有票吗", user_id=USER_A)
    ok = out["tool_name"] == "bus_availability" and ("余票" in out["answer"] or "没有查到班次" in out["answer"])
    check("B4 余票查询（真实班次+开售状态）", ok, f"answer={out['answer'][:70]!r}")

    out = ask("明天有票吗", user_id=USER_A)
    ok = out["tool_name"] == "bus_availability" and out["tool_kind"] == "slots_incomplete" and "请补充" in out["answer"]
    check("B5 槽位不全 → 反问（不猜参数、不建单）", ok, f"kind={out['tool_kind']} answer={out['answer'][:50]!r}")

    print("\n=== C. 退票费：闸门关闭（19.2 未确认）===")
    out = ask(f"退票手续费怎么算，订单号 {ORDER_A_PAID}", user_id=USER_A)
    gated = out["tool_kind"] == "unavailable" or out["transfer"] or "规则" in out["answer"]
    check("C1 闸门关闭 → 不下结论（转人工/走FAQ，绝不猜金额）", gated,
          f"tool_kind={out['tool_kind']} transfer={out['transfer']}/{out['transfer_path']} answer={out['answer'][:50]!r}")

    print("\n=== D. 误车：业务规则不允许 → 转人工（路径=确定性）===")
    out = ask(f"退票手续费怎么算，订单号 {ORDER_A_DEPARTED}", user_id=USER_A)
    ok = out["transfer"] and out["transfer_path"] in ("deterministic", "tool_unavailable")
    check("D1 误车或规则缺失 → 转人工且带上工具判定原因", ok,
          f"transfer={out['transfer']}/{out['transfer_path']} kind={out['tool_kind']} reason={out['tool_reason']}")

    print("\n=== E. 计数口径（工具不可用必须与知识库覆盖不足分开）===")
    with httpx.Client(base_url=GO, trust_env=False, timeout=20) as c:
        snap = c.get("/internal/metrics/snapshot", headers={"X-Internal-Key": KEY}).json()
    counters = snap.get("counters") or snap
    keys = {k: v for k, v in counters.items() if k.startswith("tool_")}
    print("  tool_* 计数:", json.dumps(keys, ensure_ascii=False, sort_keys=True))
    for want in ("tool_calls_total", "tool_empty_total", "tool_not_found_total", "tool_guest_blocked_total",
                 "tool_slots_incomplete_total", "tool_unavailable_total", "tool_route_calls_total"):
        check(f"E {want} 有独立计数", want in keys, str(keys.get(want)))

    failed = [r for r in results if not r[1]]
    print(f"\n=== 合计 {len(results)} 项，通过 {len(results) - len(failed)}，失败 {len(failed)} ===")
    for name, _, detail in failed:
        print(f"  FAIL: {name} —— {detail}")
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
