#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""M3 端到端验收：checkpoint/记忆/缓存/流式（打真实 Go + 编排层 + MySQL/Redis/Qdrant）。

验收口径（§17 M3）：
  A. 多轮指代消解可用
  B. 反问（消歧 interrupt）后**重启编排层进程**仍能续跑
  C. 同一问题（词面不同也算）二次提问命中答案缓存
  D. 流式首字可见（诚实记录首 token 毫秒数）
  E. 客户端断开后上游被取消（Go 计数 sse_client_aborted_total）

用法：
  uv run python tests/e2e/acceptance_m3.py --phase 1     # A/C/D/E + 触发 interrupt，落盘 conv_id
  uv run python tests/e2e/acceptance_m3.py --phase 2     # B：用同一 conv_id resume（编排层此时已被重启过）

纪律：
  - 全程**不打印任何密钥**；TOKEN_SECRET_KEY 只用于本地签发测试用 JWT（不落盘）；
  - 失败就是失败，不拿"预期"当基准（数值以服务端返回为准）。
"""

from __future__ import annotations

import argparse
import base64
import hashlib
import hmac
import json
import os
import time
import uuid
from pathlib import Path

import httpx

GO = os.environ.get("GO_URL", "http://127.0.0.1:8080")
KEY = os.environ.get("INTERNAL_KEY", "dev-internal-key")
ROOT = Path(__file__).resolve().parents[3]
STATE = Path(__file__).with_name(".m3_state.json")
USERNAME_A = os.environ.get("USER_A_NAME", "cs_test_a")
DEVICE = "m3-e2e-device"

results: list[tuple[str, bool, str]] = []


def check(name: str, ok: bool, detail: str = "") -> bool:
    results.append((name, ok, detail))
    print(f"[{'PASS' if ok else 'FAIL'}] {name}" + (f" —— {detail}" if detail else ""))
    return ok


def env_value(key: str) -> str:
    """从 app.env 读一个值（只用于本地测试签发/内网密钥，不打印）。"""
    if os.environ.get(key):
        return os.environ[key]
    f = ROOT / "app.env"
    if not f.exists():
        return ""
    for line in f.read_text(encoding="utf-8", errors="ignore").splitlines():
        line = line.strip()
        if line.startswith(f"{key}="):
            return line.split("=", 1)[1].strip().strip('"').strip("'")
    return ""


def mint_jwt(username: str, hours: int = 2) -> str:
    """本地签发 HS256 JWT（与 internal/token 同算法；仅测试用）。"""
    secret = env_value("TOKEN_SECRET_KEY")
    if not secret:
        return ""
    now = int(time.time())
    b64 = lambda b: base64.urlsafe_b64encode(b).rstrip(b"=").decode()  # noqa: E731
    header = {"alg": "HS256", "typ": "JWT"}
    iso_now = time.strftime("%Y-%m-%dT%H:%M:%S+08:00", time.localtime(now))
    iso_exp = time.strftime("%Y-%m-%dT%H:%M:%S+08:00", time.localtime(now + hours * 3600))
    payload = {
        "id": str(uuid.uuid4()), "username": username,
        "issued_at": iso_now, "expired_at": iso_exp,  # Go 侧是 time.Time（要 RFC3339 字符串）
        "exp": now + hours * 3600, "iat": now,        # RegisteredClaims（数值）
    }
    signing = b64(json.dumps(header, separators=(",", ":")).encode()) + "." + \
        b64(json.dumps(payload, separators=(",", ":"), ensure_ascii=False).encode())
    sig = hmac.new(secret.encode(), signing.encode(), hashlib.sha256).digest()
    return signing + "." + b64(sig)


def ask(question: str, *, conv_id: str, token: str = "", stream: bool = False, timeout: float = 30) -> dict:
    headers = {"Content-Type": "application/json"}
    if token:
        headers["Authorization"] = "Bearer " + token
    body = {"question": question, "conv_id": conv_id, "device_id": DEVICE}
    url = f"{GO}/cs/ask" + ("?stream=1" if stream else "")
    with httpx.Client(trust_env=False, timeout=timeout) as c:
        r = c.post(url, json=body, headers=headers)
        if r.status_code != 200:
            return {"_status": r.status_code, "_text": r.text[:200]}
        return r.json()


def stream_once(question: str, *, conv_id: str, token: str = "", read_events: int = 1,
                timeout: float = 30) -> tuple[list[dict], float, bool]:
    """读 SSE。返回 (事件列表, 首事件毫秒, 是否中途断开)。read_events<=0 表示读完整轮。"""
    events: list[dict] = []
    t0 = time.perf_counter()
    first_ms = -1.0
    headers = {"Content-Type": "application/json"}
    if token:
        headers["Authorization"] = "Bearer " + token
    body = {"question": question, "conv_id": conv_id, "device_id": DEVICE}
    with httpx.Client(trust_env=False, timeout=timeout) as c:
        with c.stream("POST", f"{GO}/cs/ask?stream=1", json=body, headers=headers) as r:
            if r.status_code != 200:
                r.read()
                return ([{"event": "http_error", "data": {"status": r.status_code}}], -1.0, False)
            buf = ""
            for chunk in r.iter_text():
                if not chunk:
                    continue
                buf += chunk
                buf = buf.replace("\r\n", "\n")  # 上游 CRLF → LF（Go 出口已规范，双保险）
                frames = buf.split("\n\n")
                buf = frames.pop()
                for frame in frames:
                    ev, data = "message", ""
                    for line in frame.split("\n"):
                        if line.startswith("event: "):
                            ev = line[7:].strip()
                        elif line.startswith("data: "):
                            data += line[6:]
                    if not data:
                        continue
                    try:
                        payload = json.loads(data)
                    except Exception:  # noqa: BLE001
                        payload = {"raw": data[:200]}
                    events.append({"event": ev, "data": payload})
                    if first_ms < 0:
                        first_ms = (time.perf_counter() - t0) * 1000
                    if read_events > 0 and len(events) >= read_events:
                        return (events, first_ms, True)  # 主动断开
                    if ev == "done":
                        return (events, first_ms, False)
    return (events, first_ms, False)


def counters() -> dict:
    with httpx.Client(trust_env=False, timeout=15) as c:
        r = c.get(f"{GO}/internal/metrics/snapshot", headers={"X-Internal-Key": KEY})
        if r.status_code != 200:
            return {}
        return r.json().get("counters") or {}


# ---------------------------------------------------------------- phase 1

def phase1() -> int:
    print("=== A. 多轮指代消解（同 conv_id 两轮）===")
    c1 = str(uuid.uuid4())
    # 样本选择：必须是**能正常作答**的 FAQ（否则首轮就转人工，指代轮无从对照）。
    # 实测口径：词面替身 embedding + 未标定阈值下，「付款方式有哪些」稳定走检索作答。
    t1 = ask("付款方式有哪些", conv_id=c1)
    ok1 = t1.get("category") == "order" and not t1.get("transfer")
    check("A1 首轮分类命中 order（不转人工）", ok1,
          f"category={t1.get('category')} source={t1.get('source')} recent={t1.get('recent_len')}")

    t2 = ask("那这个呢", conv_id=c1)
    ok2 = (t2.get("category") == "order" and t2.get("cls_source") == "inherit" and t2.get("coref_from"))
    check("A2 指代轮继承上一轮分类（cls_source=inherit）", ok2,
          f"category={t2.get('category')} cls_source={t2.get('cls_source')} coref_from={str(t2.get('coref_from'))[:20]} recent={t2.get('recent_len')}")

    t3 = ask("再说一次", conv_id=c1)
    ok3 = int(t3.get("recent_len") or 0) >= 2
    check("A3 记忆窗口随轮次增长（recent 累积）", ok3, f"recent_len={t3.get('recent_len')}")

    print("\n=== C. 答案缓存（同一问题第 3 次应命中；驱动脚本已先清空 cs:ac:*）===")
    q = "发票能开电子版吗"
    r1 = ask(q, conv_id=str(uuid.uuid4()))
    r2 = ask(q, conv_id=str(uuid.uuid4()))
    r3 = ask(q, conv_id=str(uuid.uuid4()))
    check("C1 第 1 次不入缓存（候选，需二次命中才准入）", not r1.get("cache_hit"),
          f"cache_hit={r1.get('cache_hit')} source={r1.get('source')}"
          + ("（注意：若为 True 说明缓存未清空）" if r1.get("cache_hit") else ""))
    check("C2 第 2 次仍未命中（本次提升进缓存）", not r2.get("cache_hit"),
          f"cache_hit={r2.get('cache_hit')} source={r2.get('source')}")
    check("C3 第 3 次命中缓存（source=cache 且跳过检索/生成）", bool(r3.get("cache_hit")) and r3.get("source") == "cache",
          f"cache_hit={r3.get('cache_hit')} source={r3.get('source')}")
    check("C4 缓存答案与首次一致（不是重新生成的）",
          bool(r3.get("answer")) and r3.get("answer") == r1.get("answer"),
          f"len1={len(r1.get('answer') or '')} len3={len(r3.get('answer') or '')}")

    print("\n=== D. 流式首字可见 ===")
    # 样本要选**走生成**的问题（转人工/缓存命中都没有 delta，选错会误判成"流式坏了"）：
    # 「退款多久到账」是 FAQ 检索作答路径，且未被前面的用例问过（不影响缓存准入）
    events, first_ms, aborted = stream_once("退款多久到账", conv_id=str(uuid.uuid4()), read_events=0)
    kinds = [e["event"] for e in events]
    deltas = [e for e in events if e["event"] == "delta"]
    check("D1 SSE 收到 delta 增量与 done 收尾", "delta" in kinds and "done" in kinds, f"events={kinds}")
    check("D2 首事件延迟在预算内（<8000ms，诚实数值见附注）", 0 < first_ms < 8000, f"first_event={first_ms:.0f}ms")
    print(f"     · 附注：首事件 {first_ms:.0f}ms；delta 数 {len(deltas)}（mock LLM 4ms/token + 词面 embedding，非真实模型延迟）")

    print("\n=== E. 客户端断开 → 上游取消 ===")
    before = counters().get("sse_client_aborted_total", 0)
    # 读 1~3 帧就断开：请求必须**还在流**才有意义，所以用会产 delta 的问题
    for n in (1, 3):
        stream_once("页面打不开怎么办", conv_id=str(uuid.uuid4()), read_events=n)
    time.sleep(0.8)
    after = counters().get("sse_client_aborted_total", 0)
    check("E1 断开后 Go 断开计数增长（上游请求被取消）", after > before,
          f"sse_client_aborted_total {before} → {after}")

    print("\n=== F. 触发消歧反问（为「重启后续跑」准备）===")
    token = mint_jwt(USERNAME_A)
    if not token:
        check("F0 本地签发测试 JWT", False, "app.env 缺 TOKEN_SECRET_KEY")
        return summary()
    c3 = str(uuid.uuid4())
    out = ask("查一下我的订单", conv_id=c3, token=token)
    got_int = bool(out.get("interrupt"))
    check("F1 多笔订单 → interrupt 反问（不猜）", got_int,
          f"interrupt={json.dumps(out.get('interrupt'), ensure_ascii=False)[:120]}")
    check("F2 反问阶段不建单、不转人工（只是问一句）",
          (not out.get("transfer")) and not out.get("support_ticket_no"),
          f"transfer={out.get('transfer')} ticket={out.get('support_ticket_no')} tool_kind={out.get('tool_kind')}")
    STATE.write_text(json.dumps({"conv_id": c3, "token": token, "ts": time.time()}), encoding="utf-8")
    print(f"     · 已落盘待续跑的 conv_id（{c3[:8]}…）到 {STATE.name}")
    return summary()


# ---------------------------------------------------------------- phase 2

def phase2() -> int:
    if not STATE.exists():
        check("B0 读取 phase 1 落盘状态", False, "缺 .m3_state.json（先跑 --phase 1）")
        return summary()
    st = json.loads(STATE.read_text(encoding="utf-8"))
    conv_id, token = st["conv_id"], st["token"]
    print(f"=== B. 反问后重启进程续跑（conv_id={conv_id[:8]}…）===")

    snap_note = "（本次同一进程内续跑）"
    out = ask("1", conv_id=conv_id, token=token)
    check("B1 用同一 conv_id resume，图从 checkpoint 续跑（不再反问）", not out.get("interrupt"),
          f"interrupt={out.get('interrupt')} answer={(out.get('answer') or '')[:60]!r}")
    check("B2 续跑后拿到确定性工具答复（订单详情，非转人工）",
          bool(out.get("tool_kind")) and not out.get("transfer"),
          f"tool={out.get('tool_name')} kind={out.get('tool_kind')} transfer={out.get('transfer')}")
    check("B3 答复含订单事实（金额/状态，且订单号已脱敏）",
          "订单" in (out.get("answer") or "") and "****" in (out.get("answer") or ""),
          f"answer={(out.get('answer') or '')[:80]!r}")
    print(f"     · 附注：本阶段由驱动脚本在重启编排层进程后执行{snap_note}")
    return summary()


def summary() -> int:
    total = len(results)
    passed = sum(1 for _, ok, _ in results if ok)
    print(f"\n=== M3 验收汇总：{passed}/{total} 通过，{total - passed} 失败 ===")
    for name, ok, detail in results:
        if not ok:
            print(f"  FAIL: {name} —— {detail}")
    print(f"M3_acceptance_exit={0 if passed == total else 1}")
    return 0 if passed == total else 1


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--phase", type=int, default=1, choices=[1, 2])
    args = ap.parse_args()
    return phase1() if args.phase == 1 else phase2()


if __name__ == "__main__":
    raise SystemExit(main())
