"""图节点实现（§5.3）。每个节点只做一件事，分支全部由条件边决定（便于按 E1~E16 对照测试）。

范围：
  - M1 已实现：分类（规则/LLM 仲裁）、检索、生成、引用校验、转人工建单；
  - **M2 新增**：工具路由与执行（E5/E6/E7/E9/E10）、确定性直答、槽位不全的单轮反问
    （E8 的 interrupt 版属 M3）、工具不可用回落检索后仍失败则按 path_hint 转人工；
  - M3：E8（interrupt 反问）、E11（改写重试）。
"""

from __future__ import annotations

import time
from typing import Any, Callable, Awaitable

from .clients import CapabilityError, CapabilityClient, LLMClient
from .prompts import ARBITRATE_PROMPT, SYSTEM_PROMPT, TOOL_PICK_SYSTEM, build_tool_pick_prompt, build_user_prompt
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
        res = await deps.capability.tools_route(state["question"], category=state.get("category") or "")
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
    """E8（M2 单轮版）：路由给出多个同分候选 → **不猜**，列出来让用户说清。

    与 M3 的差别：这里是"问完就结束这一轮"（无 interrupt、不挂起）；
    M3 会把它升级成 `interrupt()` + `Command(resume=...)` 的跨轮续跑
    （M0 已验证 resume 可行，且 interrupt 前的节点必须零副作用，所以这里只读不写）。
    """
    t0 = time.perf_counter()
    opts = state.get("clarify_options") or []
    names = {
        "order_detail": "订单详情",
        "my_tickets": "我的车票",
        "unpaid_orders": "待支付订单",
        "bus_availability": "余票/班次查询",
        "refund_fee": "退票手续费",
        "refund_progress": "退款进度",
    }
    text = "您的问题可能属于以下情况，请补充一下具体想查什么：" + "、".join(names.get(o, o) for o in opts) + "。"
    return {
        "answer": text,
        "sources": [],
        "verify_valid": True,
        "stage_ms": {"clarify": _ms(t0)},
        "events": [_event("clarify", "single_turn", _ms(t0), n=len(opts))],
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
    if state.get("pre_intent") == "greet":
        return "rule"
    if state.get("tool_result") is not None:
        return "tool"
    if state.get("cls_source") == "llm_arbitrate":
        return "llm"
    return "rule"
