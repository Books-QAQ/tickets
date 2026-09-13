#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""M4 评测：三套数据集 + 诚实性验收（§15.1 / §15.4）。

口径与纪律：
  - 分类走**真实管线**（打编排层 /ask，读它给出的 category/cls_source），不另跑一套分类器；
  - 检索打 Go `/internal/retrieve`（无生成），按 **chunk/label 级**判命中，qa/prose 分开报；
  - 端到端打编排层 /ask，按「关键事实是否出现」判符合；转人工类看路径与工单号；
  - **诚实性验收**（§15.4）：入口真实性 / 边界声明覆盖率 / 矛盾组合，全部机检；
  - 数字必须在**同一轮运行**里出，不许沿用历史值（§15.2）。

用法：uv run python tests/eval/run_eval.py
      （需要 Go :8080 与编排层 :8001 都在跑）
"""

from __future__ import annotations

import json
import os
import statistics
import sys
from collections import Counter
from pathlib import Path

import httpx

ORCH = os.environ.get("ORCH_URL", "http://127.0.0.1:8001")
GO = os.environ.get("GO_URL", "http://127.0.0.1:8080")
KEY = os.environ.get("INTERNAL_KEY", "dev-internal-key")
DATA = Path(__file__).with_name("datasets")
OUT = Path(os.environ.get("EVAL_OUT", "/tmp/cs_eval_result.json"))

# 与 internal/ai/cite/verify.go 的 BoundaryPrefix 保持一致（改一处必须改两处）
BOUNDARY_PREFIX = "该功能本平台暂未开放，以下是铁路客运行业的一般做法，供参考；具体以车站与官方渠道为准。"

# 操作性措辞（§15.4 入口真实性）：出现这些词就意味着"指向某个入口"
ENTRY_WORDS = ["点击", "进入", "打开", "在“我的", "在\'我的", "首页", "个人中心", "设置页"]

CATEGORY_OF_LABEL = {
    "order-": "order", "refund-": "refund", "app-": "app", "policy-": "policy",
    "booking-": "booking", "account-": "account", "station-": "travel",
}


def load(name: str) -> list[dict]:
    f = DATA / name
    return [json.loads(l) for l in f.read_text(encoding="utf-8").splitlines() if l.strip()]


def ask(q: str, *, user_id: int | None = None, guest_key: str | None = None) -> dict:
    body: dict = {"question": q, "stream": False}
    if user_id:
        body["user_id"] = user_id
    if guest_key:
        body["guest_key"] = guest_key
    with httpx.Client(base_url=ORCH, trust_env=False, timeout=90) as c:
        r = c.post("/ask", json=body, headers={"X-Internal-Key": KEY})
        r.raise_for_status()
        return r.json()


def retrieve(q: str, category: str, top_k: int = 5) -> dict:
    with httpx.Client(base_url=GO, trust_env=False, timeout=30) as c:
        r = c.post("/internal/retrieve", json={"question": q, "category": category,
                                               "visibility": ["public"], "top_k": top_k},
                   headers={"X-Internal-Key": KEY})
        r.raise_for_status()
        return r.json()


def pct(n: int, d: int) -> str:
    return f"{(100.0 * n / d):.1f}%" if d else "—"


def main() -> int:
    res: dict = {"classify": {}, "retrieval": {}, "e2e": {}, "honesty": {}}

    # ---------- 1. 分类 held-out ----------
    cls = load("classify_heldout.jsonl")
    hit, per_cat, llm_fallback = 0, Counter(), 0
    conf = Counter()
    for s in cls:
        out = ask(s["q"])
        got = out.get("category") or "other"
        per_cat[(s["expect"], got)] += 1
        conf[(s["expect"], got)] += 1
        if got == s["expect"]:
            hit += 1
        if out.get("cls_source") == "llm_arbitrate":
            llm_fallback += 1
    cats = sorted({s["expect"] for s in cls})
    per_cat_metric = {}
    for c in cats:
        tp = sum(v for (e, g), v in conf.items() if e == c and g == c)
        total = sum(v for (e, g), v in conf.items() if e == c)
        per_cat_metric[c] = {"support": total, "precision_recall_hit": round(tp / total, 3) if total else 0}
    res["classify"] = {"n": len(cls), "accuracy": round(hit / len(cls), 3) if cls else 0,
                       "llm_fallback_ratio": round(llm_fallback / len(cls), 3) if cls else 0,
                       "per_category": per_cat_metric, "confusion": {f"{e}->{g}": v for (e, g), v in conf.items() if e != g}}
    print(f"[分类] n={len(cls)} 准确率={pct(hit, len(cls))} LLM兜底占比={pct(llm_fallback, len(cls))}")
    for c in cats:
        m = per_cat_metric[c]
        print(f"    {c:<8} 样本={m['support']:<3} 准确率={m['precision_recall_hit']:.0%}")

    # ---------- 2. 检索（qa / prose 分开报） ----------
    ret = load("retrieval.jsonl")
    by_form: dict[str, dict] = {}
    cos_by_form: dict[str, list[float]] = {"qa": [], "prose": []}
    empty = 0
    for s in ret:
        cat = next((v for k, v in CATEGORY_OF_LABEL.items() if s["expect_label"].startswith(k)), "")
        try:
            r = retrieve(s["q"], cat)
        except Exception as exc:  # noqa: BLE001
            print(f"    [检索失败] {s['q']}: {type(exc).__name__}")
            continue
        labels = [str(x) for x in (r.get("source_labels") or r.get("labels") or [])]
        top1 = labels[0].split("·")[0] if labels else ""
        top3 = {l.split("·")[0] for l in labels[:3]}
        form = s.get("form", "qa")
        m = by_form.setdefault(form, {"n": 0, "hit1": 0, "hit3": 0, "empty": 0})
        m["n"] += 1
        if top1 == s["expect_label"]:
            m["hit1"] += 1
        if s["expect_label"] in top3:
            m["hit3"] += 1
        if not labels:
            m["empty"] += 1
            empty += 1
        cos = r.get("top1_cos")
        if isinstance(cos, (int, float)) and cos > 0:
            cos_by_form.setdefault(form, []).append(float(cos))
    for form, m in by_form.items():
        m["top1"] = round(m["hit1"] / m["n"], 3)
        m["top3"] = round(m["hit3"] / m["n"], 3)
        cs = cos_by_form.get(form) or []
        m["top1_cos_median"] = round(statistics.median(cs), 3) if cs else None
        print(f"[检索:{form}] n={m['n']} Top1={m['top1']:.0%} Top3={m['top3']:.0%} 空结果={m['empty']} "
              f"top1余弦中位数={m['top1_cos_median']}")
    res["retrieval"] = {"by_form": by_form, "empty_total": empty, "vector_mode": os.environ.get("VECTOR_MODE", "unknown")}

    # ---------- 3. 端到端 + 诚实性 ----------
    e2e = load("e2e.jsonl")
    cat_ok = fact_ok = 0
    boundary_needed = boundary_present = 0
    contradictory = 0
    entry_violations: list[str] = []
    transfer_with_ticket = transfer_total = 0
    for s in e2e:
        out = ask(s["q"], user_id=s.get("user_id"), guest_key="eval-guest" if s.get("guest") else None)
        ans = out.get("answer") or ""
        if out.get("category") == s["expect_category"]:
            cat_ok += 1
        if any(k in ans for k in s.get("key_facts", [])):
            fact_ok += 1
        if out.get("transfer"):
            transfer_total += 1
            if out.get("support_ticket_no"):
                transfer_with_ticket += 1
        # 诚实性：来源含非 supported 的回答必须带边界声明
        # **判据取自系统自己的信号**（回答的来源 chunk 的 capability），不取数据集里的手写标注：
        # 手写"这条该带边界"很容易标错（本项目实测：把平台其实支持的"撤销退票"标成 industry，
        # 于是把"没带声明"记成缺陷——分母错、结论也就错了）。§15.4 的原话就是"来源 capability != supported"。
        src_caps = {x.get("capability") for x in (out.get("sources") or [])}
        needs = bool(src_caps & {"roadmap", "industry"})
        if needs and not out.get("transfer"):
            boundary_needed += 1
            if BOUNDARY_PREFIX.split("，")[0] in ans:  # 前缀首段出现即认定带了声明
                boundary_present += 1
        # 诚实性：入口真实性（不该出现指向不存在入口的操作指引）
        if any(w in ans for w in ENTRY_WORDS):
            # 平台能力类问题允许出现入口；行业知识类出现入口词即为违规（会把行业做法讲成平台功能）
            if needs or s.get("capability") in ("industry",):
                entry_violations.append(f"{s['q'][:18]}…: {ans[:60]}")
        # 诚实性：矛盾组合（同一回答里既有工具结论又有行业建议）
        if BOUNDARY_PREFIX.split("，")[0] in ans and ("****" in ans or "元" in ans and "票面" in ans):
            contradictory += 1
    n_e2e = len(e2e)
    res["e2e"] = {"n": n_e2e, "category_ok": round(cat_ok / n_e2e, 3), "fact_ok": round(fact_ok / n_e2e, 3),
                  "transfer_total": transfer_total, "transfer_with_ticket": transfer_with_ticket}
    res["honesty"] = {"boundary_coverage": (round(boundary_present / boundary_needed, 3) if boundary_needed else None),
                      "boundary_needed": boundary_needed, "boundary_present": boundary_present,
                      "entry_violations": entry_violations, "contradictory": contradictory}
    print(f"[端到端] n={n_e2e} 分类符合={pct(cat_ok, n_e2e)} 事实符合={pct(fact_ok, n_e2e)} "
          f"转人工={transfer_total}（带工单号 {transfer_with_ticket}）")
    print(f"[诚实性] 边界声明覆盖率={pct(boundary_present, boundary_needed) if boundary_needed else '无样本'} "
          f"入口违规={len(entry_violations)} 矛盾组合={contradictory}")

    OUT.write_text(json.dumps(res, ensure_ascii=False, indent=2), encoding="utf-8")
    print(f"\n结果已写入 {OUT}")
    print("提醒：以上数字在 mock LLM + 词面 embedding 下取得，**不代表真实模型水平**（19.1 选型后必须重跑）")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
