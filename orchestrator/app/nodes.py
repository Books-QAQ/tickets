"""图节点实现（§5.3）。每个节点只做一件事，分支全部由条件边决定（便于按 E1~E16 对照测试）。

M1 范围提醒：工具执行（E5/E6/E9）、消歧反问（E8，interrupt）、改写重试（E11）属 M2/M3，
本文件里的 exec_tool 已按 E10 语义接好（工具不可用 → 回落检索），但工具本身在 M1 不会命中。
"""

from __future__ import annotations

import time
from typing import Any, Callable, Awaitable

from .clients import CapabilityError, CapabilityClient, LLMClient
from .prompts import ARBITRATE_PROMPT, SYSTEM_PROMPT, build_user_prompt
from .state import CSState

VALID_CATEGORIES = {"booking", "order", "refund", "account", "travel", "app", "policy", "other"}


class Deps:
    """依赖注入容器。

    **不进 State**：State 会进 checkpoint，把 client/回调塞进去会在 resume 时炸（M0 已实测重放）。
    """

    def __init__(self, capability: CapabilityClient, llm: LLMClient, *,
                 top_k: int = 5, emit: Callable[[str, Any], Awaitable[None]] | None = None) -> None:
        self.capability = capability
        self.llm = llm
        self.top_k = top_k
        self._emit = emit

    async def emit(self, event: str, payload: Any) -> None:
        if self._emit is not None:
            await self._emit(event, payload)


def _ms(t0: float) -> int:
    return int((time.perf_counter() - t0) * 1000)


def _event(node: str, decision: str, ms: int, **meta: Any) -> dict:
    return {"node": node, "decision": decision, "ms": ms, "meta": meta}


async def load_context(state: CSState, deps: Deps) -> dict:
    """读会话窗口（多轮上下文来自 checkpoint 的 history；滚动摘要属 M3）。"""
    t0 = time.perf_counter()
    history = state.get("history") or []
    return {
        "stage_ms": {"load": _ms(t0)},
        "events": [_event("load_context", "ok", _ms(t0), turns=len(history))],
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
        res = await deps.capability.classify_rule(state["question"])
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


async def route_tool(state: CSState, deps: Deps) -> dict:
    """工具路由（提槽）。M1：Go 侧恒返回空候选 → 走 E7 到检索。"""
    t0 = time.perf_counter()
    try:
        res = await deps.capability.tools_route(state["question"])
    except CapabilityError:
        return {
            "tool_candidates": [],
            "stage_ms": {"route_tool": _ms(t0)},
            "events": [_event("route_tool", "degraded", _ms(t0))],
        }
    cands = res.get("candidates") or []
    out: dict[str, Any] = {
        "tool_candidates": cands,
        "stage_ms": {"route_tool": _ms(t0)},
        "events": [_event("route_tool", "hit" if cands else "miss", _ms(t0), n=len(cands))],
    }
    if cands:
        top = cands[0]
        out["tool_name"] = top.get("tool")
        out["tool_args"] = top.get("args") or {}
    return out


async def exec_tool(state: CSState, deps: Deps) -> dict:
    """工具执行（M1：工具未实现 → not_implemented → 回落检索 E10；真实实现属 M2）。"""
    t0 = time.perf_counter()
    name = state.get("tool_name") or ""
    if not name:
        return {"stage_ms": {"tools": _ms(t0)}}
    try:
        res = await deps.capability.exec_tool(
            name,
            args=state.get("tool_args") or {},
            user_id=state.get("user_id"),
            guest_key=state.get("guest_key"),
            trace_id=state.get("trace_id", ""),
        )
    except CapabilityError as exc:
        if exc.kind == "not_implemented":
            # 能力缺失与"下游坏了"必须区分（§5.9）
            return {
                "tool_result": None,
                "tool_missing": True,
                "stage_ms": {"tools": _ms(t0)},
                "events": [_event("exec_tool", "not_implemented", _ms(t0), tool=name)],
            }
        return {
            "tool_result": None,
            "transfer": True,
            "transfer_path": "tool_unavailable",
            "stage_ms": {"tools": _ms(t0)},
            "events": [_event("exec_tool", "unavailable", _ms(t0), tool=name)],
        }
    return {
        "tool_result": res.get("data"),
        "tool_direct_answer": res.get("direct_answer"),
        "stage_ms": {"tools": _ms(t0)},
        "events": [_event("exec_tool", "ok", _ms(t0), tool=name)],
    }


async def retrieve(state: CSState, deps: Deps) -> dict:
    t0 = time.perf_counter()
    try:
        res = await deps.capability.retrieve(
            question=state["question"],
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


async def generate(state: CSState, deps: Deps) -> dict:
    """生成（流式，经 Go 网关）。输出 [TRANSFER] 即转人工（E14）。"""
    t0 = time.perf_counter()
    user = build_user_prompt(state["question"], state.get("chunks") or [], state.get("history") or [])
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
    summary = f"[{path}] {state['question']}"[:500]
    ticket_no = None
    try:
        res = await deps.capability.create_support_ticket(
            conv_id=state.get("conv_id", ""),
            user_id=state.get("user_id"),
            order_no="",
            category=state.get("category") or "other",
            path=path,
            summary=summary,
        )
        ticket_no = res.get("ticket_no")
    except CapabilityError:
        ticket_no = None  # 建单失败也要给用户一个可读回复（并在 degraded 里标注）

    answer = state.get("answer") or ""
    if ticket_no:
        answer = TRANSFER_TEMPLATE.format(ticket_no=ticket_no)
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


async def finalize(state: CSState, deps: Deps) -> dict:
    """收尾：写流水 + 埋点 + 追加多轮上下文。**所有写操作都在这里**（interrupt 之前的节点不得有副作用）。"""
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

    return {
        "answer": answer,
        # 收尾统一补齐对外字段：`transfer` 必须在最终状态里存在，
        # 否则不同分支的调用方要各自处理"键不存在"（greet/直答分支不会有 transfer=True）
        "transfer": bool(state.get("transfer")),
        "sources": state.get("sources") or [],
        "history": [{"question": state["question"], "answer": answer}],
        "stage_ms": {"finalize": _ms(t0)},
    }


def _infer_transfer_path(state: CSState) -> str:
    """条件边是纯函数（不能写状态），所以转人工路径在这里统一推断（§10 四路径计数）。"""
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
    if state.get("pre_intent") == "greet":
        return "rule"
    if state.get("tool_result") is not None:
        return "tool"
    if state.get("cls_source") == "llm_arbitrate":
        return "llm"
    return "rule"
