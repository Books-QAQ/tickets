"""图节点实现（§5.3）。每个节点只做一件事，分支全部由条件边决定（便于按 E1~E16 对照测试）。

范围：
  - M1 已实现：分类（规则/LLM 仲裁）、检索、生成、引用校验、转人工建单；
  - **M2 新增**：工具路由与执行（E5/E6/E7/E9/E10）、确定性直答、槽位不全的单轮反问
    （E8 的 interrupt 版属 M3）、工具不可用回落检索后仍失败则按 path_hint 转人工；
  - M3：E8（interrupt 反问）、E11（改写重试）。
"""

from __future__ import annotations

import re
import time
from typing import Any, Callable, Awaitable

from .clients import CapabilityError, CapabilityClient, LLMClient
from .memory import context_text, resolve_reference, update_memory
from .prompts import (ARBITRATE_PROMPT, REWRITE_SYSTEM, SYSTEM_PROMPT, TOOL_PICK_SYSTEM,
                      build_tool_pick_prompt, build_user_prompt)
from .state import CSState

VALID_CATEGORIES = {"booking", "order", "refund", "account", "travel", "app", "policy", "other"}


class Deps:
    """依赖注入容器。

    **不进 State**：State 会进 checkpoint，把 client/回调塞进去会在 resume 时炸（M0 已实测重放）。
    """

    def __init__(self, capability: CapabilityClient, llm: LLMClient, *,
                 top_k: int = 5, rewrite_max: int = 1, interrupt_max: int = 1,
                 memory_recent_n: int = 5, memory_summary_max: int = 10,
                 emit: Callable[[str, Any], Awaitable[None]] | None = None) -> None:
        self.capability = capability
        self.llm = llm
        self.top_k = top_k
        self.rewrite_max = rewrite_max
        self.interrupt_max = interrupt_max
        self.memory_recent_n = memory_recent_n
        self.memory_summary_max = memory_summary_max
        self._emit = emit

    async def emit(self, event: str, payload: Any) -> None:
        if self._emit is not None:
            await self._emit(event, payload)


def _ms(t0: float) -> int:
    return int((time.perf_counter() - t0) * 1000)


def _event(node: str, decision: str, ms: int, **meta: Any) -> dict:
    return {"node": node, "decision": decision, "ms": ms, "meta": meta}


def _q(state: CSState) -> str:
    """用于分类/检索的问题：**改写后的问题优先**（E11），原问题始终保留在 question 里。"""
    return state.get("search_question") or state["question"]


async def load_context(state: CSState, deps: Deps) -> dict:
    """读会话窗口（M3：双层窗口来自 checkpoint —— 图状态就是会话状态的持久层，ADR-9）。"""
    t0 = time.perf_counter()
    recent = list(state.get("recent") or [])
    summary = state.get("summary") or ""
    return {
        "recent": recent,
        "summary": summary,
        "has_context": bool(recent or summary),
        "rewrite_max": deps.rewrite_max,
        "interrupt_max": deps.interrupt_max,
        "stage_ms": {"load": _ms(t0)},
        "events": [_event("load_context", "ok", _ms(t0), recent=len(recent),
                          summary_items=len(summary.split("；")) if summary else 0)],
    }


async def pre_intent(state: CSState, deps: Deps) -> dict:
    """前置意图（Go 词表，零 LLM）。失败按 §6.2 降级为 none 并标注 degraded。"""
    t0 = time.perf_counter()
    try:
        intent = await deps.capability.pre_intent(state["question"])
    except CapabilityError:
        return {
            "pre_intent": "none",
            "degraded": {"classify": True},
            "stage_ms": {"pre_intent": _ms(t0)},
            "events": [_event("pre_intent", "degraded", _ms(t0))],
        }
    return {
        "pre_intent": intent,
        "stage_ms": {"pre_intent": _ms(t0)},
        "events": [_event("pre_intent", intent, _ms(t0))],
    }


async def greet(state: CSState, deps: Deps) -> dict:
    t0 = time.perf_counter()
    return {
        "answer": "您好，我是智能客服，可以帮您查订单、退改签规则、车站与乘车等问题，请直接说您的问题。",
        "sources": [],
        "stage_ms": {"greet": _ms(t0)},
        "events": [_event("greet", "ok", _ms(t0))],
    }


async def classify_rule(state: CSState, deps: Deps) -> dict:
    t0 = time.perf_counter()
    try:
        res = await deps.capability.classify_rule(_q(state))
    except CapabilityError:
        return {
            "category": "other",
            "cls_scores": {},
            "cls_source": "fallback",
            "cls_enough": False,
            "degraded": {"classify": True},
            "stage_ms": {"classify": _ms(t0)},
            "events": [_event("classify_rule", "degraded", _ms(t0))],
        }
    category = res.get("top1") or "other"
    if category not in VALID_CATEGORIES:
        category = "other"
    # 分类一完成就下发 meta，让前端先有反馈（§5.10.3）
    await deps.emit("meta", {"category": category, "cls_source": "rule"})
    return {
        "category": category,
        # cls_scores 必须一路带到检索（软路由要用），禁止丢弃
        "cls_scores": res.get("scores", {}),
        "cls_source": "rule",
        "cls_enough": bool(res.get("enough")),
        "stage_ms": {"classify": _ms(t0)},
        "events": [_event("classify_rule", category, _ms(t0), gap=res.get("gap", 0))],
    }


async def llm_arbitrate(state: CSState, deps: Deps) -> dict:
    """规则置信度不足时的 LLM 兜底/跨类仲裁（闭集输出）。保留 cls_scores。"""
    t0 = time.perf_counter()
    raw = await deps.llm.complete(ARBITRATE_PROMPT, state["question"])
    category = raw.strip().split()[0].strip("。.,，") if raw.strip() else "other"
    if category not in VALID_CATEGORIES:
        category = "other"
    await deps.emit("meta", {"category": category, "cls_source": "llm_arbitrate"})
    return {
        "category": category,
        "cls_source": "llm_arbitrate",
        "cls_scores": state.get("cls_scores") or {},
        "stage_ms": {"arbitrate": _ms(t0)},
        "events": [_event("llm_arbitrate", category, _ms(t0))],
    }


def _select_tool(cand: dict) -> dict:
    """把 Go 返回的候选落成 state 字段。**槽位原样回传**，不在 Python 侧重抽。"""
    slots = cand.get("slots") or {}
    return {
        "tool_name": cand.get("name"),
        "tool_kind": cand.get("kind") or "read",
        "tool_slots": slots,
        "order_no": slots.get("order_no") or "",
    }


async def _llm_pick_tool(deps: Deps, question: str, tools: list[dict]) -> str | None:
    """E6：提槽未命中时由 LLM 从**工具清单**里选一个。

    两条安全线（§8.1）：
      1. 白名单校验——选出来的名字必须在清单里，否则丢弃（防模型发明工具名）；
      2. 只决定"调哪个"，参数槽位仍由 Go 的确定性抽取填充（这里传空槽位，
         Go 侧对必填槽的工具会返回 slots_incomplete → 反问，而不是猜参数）。
    """
    if deps.llm is None or not tools:
        return None
    allowed = {t.get("name") for t in tools if t.get("name")}
    try:
        text = (await deps.llm.complete(TOOL_PICK_SYSTEM, build_tool_pick_prompt(question, tools)) or "").strip()
    except Exception as exc:  # noqa: BLE001 —— 兜底失败不能影响主链路
        print(f"[route_tool] LLM 兜底选工具失败（回落检索）: {type(exc).__name__}: {exc}")
        return None
    picked = text.split()[0].strip("。.,，\n\r") if text else ""
    if picked in allowed:
        return picked
    return None


async def route_tool(state: CSState, deps: Deps) -> dict:
    """工具路由（E5/E6/E7）。

    E5：Go 侧提槽命中候选 → exec_tool
    E6：提槽未命中 且 LLM 从清单里选出工具（过白名单）→ exec_tool
    E7：都没有 → retrieve
    """
    t0 = time.perf_counter()
    try:
        res = await deps.capability.tools_route(_q(state), category=state.get("category") or "")
    except CapabilityError:
        return {
            "tool_candidates": [],
            "stage_ms": {"route_tool": _ms(t0)},
            "events": [_event("route_tool", "degraded", _ms(t0))],
        }

    cands = res.get("candidates") or []
    tools = res.get("tools") or []
    out: dict[str, Any] = {
        "tool_candidates": cands,
        "tools_available": tools,
        "stage_ms": {"route_tool": _ms(t0)},
    }

    if cands:
        top = cands[0]
        # 多候选同分 → 不猜（E8）。但若 Go 已按分类破平（preferred），就用它，不再反问。
        # interrupt 版跨轮续跑属 M3（M0 已验证 resume 可行）。
        tie = len(cands) > 1 and int(cands[1].get("score") or 0) == int(top.get("score") or 0)
        if tie and not top.get("preferred"):
            out["clarify_options"] = [c.get("name") for c in cands]
            out["clarify_values"] = [c.get("name") for c in cands]
            out["events"] = [_event("route_tool", "multi_candidate", _ms(t0), n=len(cands))]
            return out
        out.update(_select_tool(top))
        out["events"] = [_event("route_tool", "hit", _ms(t0), tool=top.get("name"), score=top.get("score"))]
        return out

    if tools:
        picked = await _llm_pick_tool(deps, state["question"], tools)
        if picked:
            out.update({"tool_name": picked, "tool_kind": "read", "tool_slots": {}})
            out["events"] = [_event("route_tool", "llm_pick", _ms(t0), tool=picked)]
            return out

    out["events"] = [_event("route_tool", "miss", _ms(t0))]
    return out


def _path_from_hint(hint: str, fallback: str) -> str:
    """Go 的 path_hint → §10.1 的路径名（口径由 Go 定，Python 不自己猜）"""
    return {
        "transfer_tool_unavailable": "tool_unavailable",
        "transfer_deterministic": "deterministic",
        "transfer_capability_absent": "capability_absent",
    }.get(hint, fallback)


async def exec_tool(state: CSState, deps: Deps) -> dict:
    """工具执行（E5/E6 的执行端）。按 Go 返回的 kind 分流：

    | kind | 走向 |
    |---|---|
    | ok | 确定性直答（E9，**不经 LLM**） |
    | empty / not_found | 确定性话术直答（"没有待支付订单"/"未查询到该订单"） |
    | slots_incomplete | 单轮反问（E8-M2 版），列出缺什么/有哪些候选 |
    | guest_required | 引导登录直答（不建单） |
    | blocked | 业务规则不允许（误车/状态不对/无规则）→ 转人工，路径取 path_hint |
    | unavailable | 回落检索（E10）：§8.2 闸门关闭时"走 FAQ + 转人工" |
    """
    t0 = time.perf_counter()
    name = state.get("tool_name") or ""
    if not name:
        return {"stage_ms": {"tools": _ms(t0)}}

    try:
        res = await deps.capability.exec_tool(
            name,
            args=state.get("tool_slots") or {},
            user_id=state.get("user_id"),
            guest_key=state.get("guest_key"),
            trace_id=state.get("trace_id", ""),
        )
    except CapabilityError as exc:
        # 契约层失败 = "下游坏了"，不是"能力缺失"（两者转人工路径不同，§5.9）
        return {
            "tool_result": None,
            "tool_kind": "unavailable",
            "tool_path_hint": "transfer_tool_unavailable",
            "transfer": True,
            "transfer_path": "tool_unavailable",
            "stage_ms": {"tools": _ms(t0)},
            "events": [_event("exec_tool", "contract_error", _ms(t0), tool=name, kind=exc.kind)],
        }

    kind = res.get("kind") or "unavailable"
    path_hint = res.get("path_hint") or ""
    summary = res.get("summary") or ""
    base: dict[str, Any] = {
        "tool_result": res,
        "tool_kind": kind,
        "tool_path_hint": path_hint,
        "stage_ms": {"tools": _ms(t0)},
    }

    if kind in ("ok", "empty", "not_found"):
        base["tool_direct_answer"] = summary
        base["events"] = [_event("exec_tool", kind, _ms(t0), tool=name)]
        return base

    if kind == "slots_incomplete":
        opts = res.get("candidates") or []
        vals = res.get("option_values") or []
        if opts and vals and len(opts) == len(vals):
            # 有多候选可选（多张可退车票 / 多笔最近订单）→ **interrupt 反问**（E8），
            # resume 后带选中的槽位重跑本工具（本节点只读，重放无副作用）
            base["clarify_options"] = opts
            base["clarify_values"] = vals
            base["events"] = [_event("exec_tool", "clarify", _ms(t0), tool=name, n=len(opts))]
            return base
        # 缺参数/地名歧义：无"可选项"可问，给确定性补充提示（不建单）
        text = summary
        if opts and "；" not in summary[:80]:
            text = summary + "\n" + "；".join(opts)
        base["tool_direct_answer"] = text
        base["events"] = [_event("exec_tool", "slots_incomplete", _ms(t0), tool=name,
                                 missing=",".join(res.get("missing") or []))]
        return base

    if kind == "guest_required":
        base["tool_direct_answer"] = summary
        base["events"] = [_event("exec_tool", "guest_required", _ms(t0), tool=name)]
        return base

    if kind == "blocked":
        base["transfer"] = True
        base["transfer_path"] = _path_from_hint(path_hint, "deterministic")
        base["events"] = [_event("exec_tool", "blocked", _ms(t0), tool=name, reason=res.get("reason"))]
        return base

    # unavailable：回落检索（E10）——由 edge 决定进 retrieve；检索也不中时
    # transfer 节点会用 tool_path_hint 决定路径（不会把它记成"知识库覆盖不足"）
    base["events"] = [_event("exec_tool", "fallback_retrieve", _ms(t0), tool=name, reason=res.get("reason"))]
    return base


async def coref(state: CSState, deps: Deps) -> dict:
    """M3：确定性指代消解（§9.4.1）。

    触发条件（缺一不可）：分类落 other + 含指代词 + 有历史话题。
    **继承时必须补 cls_scores**：后续软路由/检索要用分类得分，只改 category 不带得分会让检索落空。
    """
    t0 = time.perf_counter()
    res = resolve_reference(question=_q(state), category=state.get("category") or "",
                            recent=state.get("recent"), summary=state.get("summary"))
    if not res:
        return {"stage_ms": {"coref": _ms(t0)}}
    return {
        **res,
        "stage_ms": {"coref": _ms(t0)},
        "events": [_event("coref", "inherit", _ms(t0), category=res.get("category"),
                          from_turn=res.get("coref_from", "")[:40])],
    }


async def cache_lookup(state: CSState, deps: Deps) -> dict:
    """M3：答案缓存查询（§9.3）。

    **放在图内节点而不是 Go 转发前短路**：否则该轮不进 checkpoint，
    下一轮的指代消解就缺上下文（设计明写的理由）。
    命中的前提是"消解后的规范问题 + 同分类"，所以本节点必须在 coref 之后。
    """
    t0 = time.perf_counter()
    try:
        res = await deps.capability.cache_lookup(question=_q(state), category=state.get("category") or "")
    except CapabilityError:
        return {"cache_hit": False, "stage_ms": {"cache": _ms(t0)},
                "events": [_event("cache_lookup", "degraded", _ms(t0))]}

    mode = res.get("vector_mode") or ""
    if res.get("hit"):
        answer = res.get("answer") or ""
        sources = res.get("sources") or []
        return {
            "cache_hit": True,
            "answer": answer,
            "sources": sources,
            # **命中也要走 verify**（见 edge_after_cache）：边界声明注入与引用校验是
            # "同一问题的答案对谁都一致"的一部分，缓存省的是检索+生成，不是合规检查。
            # M3 实测踩过：命中直接去 finalize → 能力边界声明整段消失（M1 回归因此翻红）。
            "source_labels": [s.get("label") or s.get("id") or "" for s in sources],
            "verify_valid": False,
            "cache_score": float(res.get("score") or 0.0),
            "cache_mode": mode,
            "stage_ms": {"cache": _ms(t0)},
            "events": [_event("cache_lookup", "hit", _ms(t0), score=round(res.get("score") or 0.0, 4),
                              vector_mode=mode)],
        }
    return {"cache_hit": False, "cache_mode": mode, "stage_ms": {"cache": _ms(t0)},
            "events": [_event("cache_lookup", "miss", _ms(t0), vector_mode=mode)]}


async def rewrite(state: CSState, deps: Deps) -> dict:
    """M3：E11 —— 检索空时的改写重试（上限 1 次）。

    两条纪律：
      1. **只改写不回答**：答案仍走生成层防幻觉约束，避免缺素材时编造；
      2. **必须带会话上下文**才改写（无历史的纯指代/无效输入，LLM 只会瞎猜）。
    """
    t0 = time.perf_counter()
    retry = int(state.get("retry_count") or 0)
    limit = int(state.get("rewrite_max") or deps.rewrite_max or 1)
    ctx = context_text(state.get("recent"), state.get("summary"))

    if retry >= limit or not ctx or deps.llm is None:
        return {
            "retry_count": retry + 1,
            "stage_ms": {"rewrite": _ms(t0)},
            "events": [_event("rewrite", "skipped", _ms(t0),
                              reason="limit" if retry >= limit else "no_context")],
        }
    try:
        new_q = (await deps.llm.complete(REWRITE_SYSTEM, f"{ctx}\n\n用户最后一句话：{state['question']}\n改写后的问题：") or "").strip()
    except Exception as exc:  # noqa: BLE001 —— 改写失败不能影响主链路（按"不重试"处理）
        print(f"[rewrite] 改写失败，按不重试处理: {type(exc).__name__}: {exc}")
        new_q = ""
    new_q = new_q.split("\n")[0].strip()
    ok, why = _valid_rewrite(new_q, state["question"])
    if not ok:
        # 改写被拒 → **直接走转人工**（不再空跑第二遍分类/检索）：
        # 空跑一遍会把指代继承来的分类覆盖掉，等于把用户的话当没听懂（M3 实测）
        print(f"[rewrite] 改写结果被拒（{why}），转人工: {new_q[:60]!r}")
        return {
            "rewrite_failed": True,
            "retry_count": retry + 1,
            "stage_ms": {"rewrite": _ms(t0)},
            "events": [_event("rewrite", "rejected", _ms(t0), reason=why)],
        }
    return {
        "search_question": new_q,
        "rewrite_failed": False,
        "retry_count": retry + 1,
        "stage_ms": {"rewrite": _ms(t0)},
        "events": [_event("rewrite", "rewritten", _ms(t0), to=new_q[:40])],
    }


async def tool_answer(state: CSState, deps: Deps) -> dict:
    """E9：工具结果是事实类且自足 → **确定性直答，不经 LLM**。

    理由：工具结果里是金额、订单状态、时刻这类可核对事实，让模型改写只会引入错误
    （§8.2「金额不让 LLM 算」的同一条纪律延伸到"不让 LLM 复述"）。
    """
    t0 = time.perf_counter()
    ans = state.get("tool_direct_answer") or ""
    return {
        "answer": ans,
        "sources": [],
        "verify_valid": True,
        # 工具直答没有知识库出处，也就不涉及能力边界声明（来源是"本平台数据"）
        "boundary_injected": False,
        "stage_ms": {"tool_answer": _ms(t0)},
        "events": [_event("tool_answer", "ok", _ms(t0), chars=len(ans))],
    }


async def retrieve(state: CSState, deps: Deps) -> dict:
    t0 = time.perf_counter()
    try:
        res = await deps.capability.retrieve(
            question=_q(state),
            category=state.get("category", ""),
            scores=state.get("cls_scores") or {},
            visibility=state.get("visibility") or ["public"],
            top_k=deps.top_k,
        )
    except CapabilityError:
        return {
            "chunks": [],
            "source_labels": [],
            "retrieve_empty": True,
            "retrieve_failed": True,
            "stage_ms": {"retrieve": _ms(t0)},
            "events": [_event("retrieve", "failed", _ms(t0))],
        }
    chunks = res.get("chunks") or []
    vector_mode = res.get("vector_mode", "embedding")
    degraded = {"vector": True} if vector_mode == "ngram" else {}
    return {
        "chunks": chunks,
        "source_labels": res.get("source_labels") or [],
        "top1_cos": float(res.get("top1_cos") or 0.0),
        "above_threshold": bool(res.get("above_threshold", False)),
        "vector_mode": vector_mode,
        "widened_level": int(res.get("level") or 0),
        "retrieve_empty": bool(res.get("empty")) or not chunks,
        "degraded": degraded,
        "stage_ms": {"retrieve": _ms(t0)},
        "events": [_event("retrieve", "empty" if not chunks else "hit", _ms(t0),
                          n=len(chunks), level=res.get("level", 0), vector_mode=vector_mode)],
    }


def _valid_rewrite(new_q: str, original: str) -> tuple[bool, str]:
    """校验 LLM 的"改写"，拒绝把**答案**当改写结果（M3 实测踩过）。

    真实场景：模型（尤其是能力弱的/mock）会直接吐一句答案或"抱歉，我暂时没有找到相关信息。"
    当成"改写后的问题"。若不过滤，它会**覆盖掉指代消解继承来的分类**（第二遍分类作用于
    这句垃圾输入 → 又落 other），于是"那这个呢"整轮白跑还答歪。
    判据（都可机检）：非空、是短问句、不是答案话术、不带引用标记、确实与原句不同。
    """
    q = (new_q or "").strip().strip("。！？!? \t\n")
    if not q:
        return False, "empty"
    if len(q) > 60:
        return False, "too_long"  # 改写后的问题不该是长段落
    if any(m in q for m in ("[[", "【来源", "抱歉", "未找到", "暂时没有", "无法提供")):
        return False, "answer_like"
    if q == (original or "").strip():
        return False, "unchanged"
    return True, ""


def _history_for_prompt(state: CSState) -> list[dict]:
    """把双层记忆转成生成 prompt 要的形态（§9.2）。

    不再使用 M1 的 `history`（它是 append-only，**每轮往 checkpoint 里追加、无上限**），
    改由 recent（最近 5 轮问句）+ summary（6~15 轮确定性摘要）生成：
    记忆总量封顶 15 轮，prompt 与 checkpoint 都不膨胀。
    """
    out: list[dict] = []
    if state.get("summary"):
        out.append({"question": f"（更早的对话摘要）{state['summary']}"})
    for turn in (state.get("recent") or [])[-4:]:
        if turn.get("question"):
            out.append({"question": turn["question"], "category": turn.get("category")})
    return out


async def generate(state: CSState, deps: Deps) -> dict:
    """生成（流式，经 Go 网关）。输出 [TRANSFER] 即转人工（E14）。"""
    t0 = time.perf_counter()
    user = build_user_prompt(state["question"], state.get("chunks") or [], _history_for_prompt(state))
    parts: list[str] = []
    usage: dict = {}
    try:
        async for kind, payload in deps.llm.stream(SYSTEM_PROMPT, user):
            if kind == "delta":
                parts.append(payload)
                await deps.emit("delta", {"text": payload})
            elif kind == "end":
                usage = payload or {}
    except Exception as exc:  # noqa: BLE001 —— 模型不可用必须兜底转人工，不能把异常抛给用户
        # 兜底不等于静默：错误必须留痕（M1 实测就是靠这条日志定位到网关流式 ctx 被回收的）
        print(f"[generate] LLM 调用失败，转人工: {type(exc).__name__}: {exc}")
        return {
            "answer": "",
            "transfer": True,
            "transfer_path": "model",
            "degraded": {"llm": True},
            "stage_ms": {"generate": _ms(t0)},
            "events": [_event("generate", "failed", _ms(t0), error=type(exc).__name__)],
        }

    answer = "".join(parts).strip()
    if "[TRANSFER]" in answer:
        return {
            "answer": answer,
            "transfer": True,
            "transfer_path": "model",
            "tokens": usage,
            "stage_ms": {"generate": _ms(t0)},
            "events": [_event("generate", "transfer_marker", _ms(t0))],
        }
    return {
        "answer": answer,
        "tokens": usage,
        "stage_ms": {"generate": _ms(t0)},
        "events": [_event("generate", "ok", _ms(t0), chars=len(answer))],
    }


async def verify(state: CSState, deps: Deps) -> dict:
    """引用校验 + 边界声明（Go 侧纯确定性执行；E15：无有效出处 → 转人工）。"""
    t0 = time.perf_counter()
    try:
        res = await deps.capability.verify_citation(state.get("answer", ""), state.get("source_labels") or [])
    except CapabilityError:
        # 校验不可用时不放行未校验答案：按"无有效出处"处理（宁可转人工）
        return {
            "verify_valid": False,
            "stage_ms": {"verify": _ms(t0)},
            "events": [_event("verify", "failed", _ms(t0))],
        }
    return {
        "answer": res.get("answer_cleaned") or state.get("answer", ""),
        "sources": res.get("sources") or [],
        "citation_dropped": res.get("dropped", 0),
        "boundary_injected": bool(res.get("boundary_injected")),
        "boundary_violation": bool(res.get("boundary_violation")),
        "verify_valid": len(res.get("valid_sources") or []) > 0,
        "stage_ms": {"verify": _ms(t0)},
        "events": [_event("verify", "ok" if res.get("valid_sources") else "no_valid_source", _ms(t0),
                          dropped=res.get("dropped", 0), boundary=bool(res.get("boundary_injected")))],
    }


TRANSFER_TEMPLATE = (
    "已为您转接人工客服，工单号 {ticket_no}。人工客服会尽快跟进，您也可以在“我的工单”里查看进度。"
)


TRANSFER_REASON = {
    "deterministic": "用户明确要求人工/投诉",
    "threshold": "知识库无依据（低于相关性阈值）",
    "model": "模型判定无依据（[TRANSFER]）",
    "tool_unavailable": "工具不可用",
    "capability_absent": "平台暂无该功能",
}


async def transfer(state: CSState, deps: Deps) -> dict:
    """转人工：**服务端统一建单**（§10）。游客也建单（user_id 可空）。"""
    t0 = time.perf_counter()
    path = state.get("transfer_path") or _infer_transfer_path(state)
    # 工单摘要（**确定性模板**，§10.2）：分类 + 原问题 + 关联订单号 + 工具判定原因
    # （"工具不可用"与"业务规则不允许"对坐席是完全不同的两件事，原因必须带到工单里）
    parts = [f"[{path}] {state['question']}"]
    tool_res = state.get("tool_result") or {}
    if tool_res.get("reason"):
        parts.append(f"工具判定：{tool_res.get('tool')}/{tool_res.get('reason')}")
    if tool_res.get("summary"):
        parts.append(f"工具结论：{tool_res['summary'][:200]}")
    summary = "；".join(parts)[:500]
    order_no = state.get("order_no") or ""
    ticket_no = None
    guest_blocked = False
    login_prompt = ""
    try:
        # M2 起建单**走工具注册表**（与读工具同一套规则：游客策略/白名单/熔断/单一计数点），
        # 不再由编排层直连专用端点 —— 否则"工具层"与"建单"两套治理各说各话。
        res = await deps.capability.exec_tool(
            "create_support_ticket",
            args={
                "question": state["question"],
                "category": state.get("category") or "other",
                "path": path,
                "summary": summary,
                "conv_id": state.get("conv_id", ""),
                "order_no": order_no,
            },
            user_id=state.get("user_id"),
            guest_key=state.get("guest_key"),
            trace_id=state.get("trace_id", ""),
        )
        if res.get("kind") == "ok":
            ticket_no = (res.get("facts") or {}).get("ticket_no")
        elif res.get("kind") == "guest_required":
            guest_blocked = True
            login_prompt = res.get("summary") or ""
    except CapabilityError:
        ticket_no = None  # 建单失败也要给用户一个可读回复（并在 degraded 里标注）

    answer = state.get("answer") or ""
    # 模型标记不许流到用户面前（LLM 只输出结构化信号，话术由确定性层给）
    answer = answer.replace("[TRANSFER]", "").strip()

    if guest_blocked:
        # 游客态（19.3 显式关闭游客建单时）：引导登录，**不建单**，因此也不宣示"已转接"
        return {
            "answer": login_prompt or "为保护您的订单信息，请先登录后我为您转接人工客服。",
            "transfer": False,
            "transfer_path": None,
            "support_ticket_no": None,
            "sources": state.get("sources") or [],
            "stage_ms": {"transfer": _ms(t0)},
            "events": [_event("transfer", "guest_login_required", _ms(t0))],
        }

    if ticket_no:
        # 已经有答案（如闸门关闭时先答了 FAQ 规则）→ **保留它再补上转人工话术**（§8.2）
        answer = (answer + "\n\n" + TRANSFER_TEMPLATE.format(ticket_no=ticket_no)) if answer \
            else TRANSFER_TEMPLATE.format(ticket_no=ticket_no)
    elif not answer:
        answer = "已为您转接人工客服，请稍候。"

    return {
        "answer": answer,
        "transfer": True,
        "transfer_path": path,
        "support_ticket_no": ticket_no,
        "sources": state.get("sources") or [],
        "degraded": {"ticket": True} if ticket_no is None else {},
        "stage_ms": {"transfer": _ms(t0)},
        "events": [_event("transfer", path, _ms(t0), ticket_no=ticket_no or "")],
    }


async def clarify(state: CSState, deps: Deps) -> dict:
    """E8：消歧反问（**interrupt 版**，M3）。

    纪律（§5.4）：
      - interrupt 之前的代码在 resume 时会**重跑** → 本节点只读不写（写操作全在 finalize/transfer）；
      - resume 值可能是任意文本 → 校验失败即转人工（interrupt 上限 1 次，避免死循环）；
      - 恢复必须用**同一个 thread_id**（= conv_id），换 ID 就变成新线程、空状态。
    """
    from langgraph.types import interrupt

    t0 = time.perf_counter()
    options = list(state.get("clarify_options") or [])
    values = list(state.get("clarify_values") or [])
    payload = {"question": "您说的是哪一个？请回复序号或直接说明。", "options": options}

    # 首次进入：在此暂停并等用户回复；resume 时返回用户输入
    reply = interrupt(payload)

    # —— 以下语句只在 resume 之后执行 ——
    # resume 值可能是字符串，也可能是 {"text": ...}（/ask 的既有约定）→ 两种都认
    if isinstance(reply, dict):
        reply = reply.get("text") or reply.get("value") or ""
    text = reply if isinstance(reply, str) else str(reply or "")
    matched = _match_option(text, options, values)
    if matched is None:
        return {
            "clarify_rejected": True,
            "transfer": True,
            "transfer_path": "deterministic",
            "stage_ms": {"clarify": _ms(t0)},
            "events": [_event("clarify", "rejected", _ms(t0), reply=text[:40])],
        }

    idx, value = matched
    update = _apply_choice(state, value)
    return {
        **update,
        "clarify_resolved": value,
        "stage_ms": {"clarify": _ms(t0)},
        "events": [_event("clarify", "resolved", _ms(t0), index=idx, choice=value[:40])],
    }


def _match_option(reply: str, options: list[str], values: list[str]) -> tuple[int, str] | None:
    """把用户回复映射到选项（纯函数）：支持"1"/"第一个"/"第二项"、唯一片段匹配。"""
    r = (reply or "").strip()
    if not r or not options:
        return None
    def pick(i: int):
        return (i, values[i] if i < len(values) else options[i])

    if m := re.fullmatch(r"(?:第)?\s*(\d+)\s*(?:个|项|条)?", r):
        i = int(m.group(1)) - 1
        if 0 <= i < len(options):
            return pick(i)
    zh = {"一": 1, "二": 2, "三": 3, "四": 4, "五": 5, "六": 6, "七": 7, "八": 8, "九": 9, "十": 10}
    if m := re.fullmatch(r"第?([一二三四五六七八九十])(?:个|项|条)?", r):
        i = zh[m.group(1)] - 1
        if 0 <= i < len(options):
            return pick(i)
    hits = [i for i, o in enumerate(options) if r in o]
    if len(hits) == 1:
        return pick(hits[0])
    return None


def _apply_choice(state: CSState, value: str) -> dict:
    """把选中的值落进 state：既可能是"选哪个工具"，也可能是"选哪张票/哪笔订单"。"""
    names = {t.get("name") for t in (state.get("tools_available") or []) if t.get("name")}
    if value in names:
        return {"tool_name": value, "tool_slots": {}, "clarify_options": []}
    slots = dict(state.get("tool_slots") or {})
    slots.update(_slots_from_option(value))
    out: dict = {"tool_slots": slots, "clarify_options": []}
    if slots.get("order_no"):
        out["order_no"] = slots["order_no"]
    return out


def _slots_from_option(value: str) -> dict:
    """从选项文本里解析出机器可用的槽位（车票号优先，其次订单号 UUID）。"""
    if m := re.search(r"车票\s*(\d+)", value or ""):
        return {"ticket_id": int(m.group(1))}
    if m := re.search(r"[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}", value or ""):
        return {"order_no": m.group(0)}
    return {}


async def finalize(state: CSState, deps: Deps) -> dict:
    """收尾：写流水 + 埋点 + 更新会话窗口 + 答案入缓存。**所有写操作都在这里**（interrupt 之前的节点不得有副作用）。"""
    t0 = time.perf_counter()
    answer = state.get("answer") or ""
    if not answer:
        answer = "抱歉，我暂时没能理解您的问题，可以换个说法再问一次吗？"

    events = list(state.get("events") or [])
    try:
        await deps.capability.record_metrics(state.get("trace_id", ""), events)
    except CapabilityError:
        pass
    try:
        await deps.capability.record_turns(
            conv_id=state.get("conv_id", ""),
            user_id=state.get("user_id"),
            question=state["question"],
            answer=answer,
            category=state.get("category") or "other",
            source=_source_of(state),
            trace_id=state.get("trace_id", ""),
        )
    except CapabilityError:
        pass

    # —— M3：答案入缓存（§9.3）——
    # 只有"正常知识库问答"才入缓存：转人工/问候/工具直答/带订单号的一律不入
    cache_res = {"stored": False, "reason": "not_eligible"}
    if not state.get("transfer") and not state.get("cache_hit") and not state.get("tool_result") \
            and state.get("pre_intent") in (None, "", "none") and answer:
        personalized = bool(state.get("order_no")) or _looks_personalized(answer)
        try:
            cache_res = await deps.capability.cache_store(
                question=_q(state), category=state.get("category") or "other",
                answer=answer, sources=state.get("sources") or [], personalized=personalized)
        except CapabilityError:
            cache_res = {"stored": False, "reason": "capability_error"}

    # —— M3：更新双层记忆窗口（图状态即持久层）——
    recent, summary = update_memory(
        recent=state.get("recent"), summary=state.get("summary"),
        question=state["question"], category=state.get("category") or "other",
        recent_n=deps.memory_recent_n, summary_max=deps.memory_summary_max)

    return {
        "answer": answer,
        # 收尾统一补齐对外字段：`transfer` 必须在最终状态里存在，
        # 否则不同分支的调用方要各自处理"键不存在"（greet/直答分支不会有 transfer=True）
        "transfer": bool(state.get("transfer")),
        "sources": state.get("sources") or [],
        "recent": recent,
        "summary": summary,
        "has_context": bool(recent or summary),
        # 注意：**不再写 `history`**（M1 是 append-only，会无上限撑大 checkpoint）；
        # 生成 prompt 的上下文由 `_history_for_prompt()` 从 recent + summary 现算。
        "stage_ms": {"finalize": _ms(t0)},
        "events": [_event("finalize", "ok", _ms(t0), cache_stored=bool(cache_res.get("stored")),
                          cache_reason=cache_res.get("reason", ""))],
    }


def _looks_personalized(answer: str) -> bool:
    """答案里带订单号/车票号/金额明细 → 属个性化，不入缓存（防串号）。"""
    if re.search(r"[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}", answer):
        return True
    return bool(re.search(r"订单\s*[0-9a-zA-Z\*]{4,}", answer))


def _infer_transfer_path(state: CSState) -> str:
    """条件边是纯函数（不能写状态），所以转人工路径在这里统一推断（§10 四路径计数）。

    M2 起增加一层：**工具的 path_hint 优先**（口径由 Go 定）——
    工具闸门关闭/数据源缺失属"工具不可用"，误车属"业务规则"（确定性），
    两者都不能被后面的阈值判断吞成"知识库覆盖不足"。
    """
    hint = state.get("tool_path_hint") or ""
    mapped = {
        "transfer_tool_unavailable": "tool_unavailable",
        "transfer_deterministic": "deterministic",
        "transfer_capability_absent": "capability_absent",
    }.get(hint)
    if mapped:
        return mapped
    if state.get("pre_intent") in ("complaint", "human"):
        return "deterministic"
    if state.get("tool_name") and state.get("tool_result") is None:
        return "capability_absent"
    if state.get("retrieve_empty") or state.get("above_threshold") is False:
        return "threshold"
    if "[TRANSFER]" in (state.get("answer") or ""):
        return "model"
    return "model"


def _source_of(state: CSState) -> str:
    if state.get("transfer"):
        return "transfer"
    if state.get("cache_hit"):
        return "cache"  # M3：缓存命中（§9.1 的 source 枚举里有 cache）
    if state.get("pre_intent") == "greet":
        return "rule"
    if state.get("tool_result") is not None:
        return "tool"
    if state.get("cls_source") == "llm_arbitrate":
        return "llm"
    return "rule"
