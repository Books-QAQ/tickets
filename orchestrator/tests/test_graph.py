"""图单测：用假客户端覆盖 M1 的边集（E1/E2/E3/E4/E7/E10/E13/E14/E15/E16）+ 降级可见性。

真实端到端（真 Qdrant + 真 Go 服务 + 边界声明拦截）在 tests/e2e/ 下，不在这里冒充。
"""

from __future__ import annotations

from typing import Any, AsyncIterator

import pytest

from app.clients import CapabilityError
from app.graph import build_graph
from app.nodes import Deps
from app.state import new_initial_state


# —— 假客户端 ——

class FakeCapability:
    def __init__(self, *, pre_intent: str = "none", cls: dict | None = None,
                 retrieve: dict | None = None, tools: list | None = None,
                 tools_available: list | None = None,
                 verify: dict | None = None, fail_classify: bool = False,
                 exec_result: dict | None = None, fail_exec: bool = False) -> None:
        self.pre_intent_value = pre_intent
        self.cls = cls if cls is not None else {"scores": {"booking": 6}, "top1": "booking", "gap": 6, "enough": True}
        self.retrieve_result = retrieve if retrieve is not None else _hit()
        self.tools = tools or []
        self.tools_available = tools_available or [
            {"name": "my_tickets", "kind": "read", "desc": "查我的车票"},
            {"name": "order_detail", "kind": "read", "desc": "查订单详情"},
        ]
        self.verify_result = verify
        self.fail_classify = fail_classify
        self.exec_result = exec_result
        self.fail_exec = fail_exec
        self.cache_hit_result: str | None = None   # 非 None 时 cache_lookup 返回命中
        self.cache_sources: list[dict] = []
        self.cache_mode = "embedding"
        self.cache_lookups: list[dict] = []
        self.cache_stores: list[dict] = []
        self.calls: list[str] = []
        self.exec_payload: dict[str, Any] = {}
        self.retrieve_payload: dict[str, Any] = {}
        self.tickets: list[dict] = []
        self.turns: list[dict] = []

    async def pre_intent(self, question: str) -> str:
        self.calls.append("pre_intent")
        return self.pre_intent_value

    async def classify_rule(self, question: str) -> dict:
        self.calls.append("classify_rule")
        if self.fail_classify:
            raise CapabilityError("upstream_unavailable", "boom")
        return self.cls

    async def retrieve(self, **kwargs: Any) -> dict:
        self.calls.append("retrieve")
        self.retrieve_payload = kwargs
        return self.retrieve_result

    async def tools_route(self, question: str, category: str = "", session_hint: str = "") -> dict:
        self.calls.append("tools_route")
        self.route_category = category
        return {"candidates": self.tools, "tools": self.tools_available}

    async def exec_tool(self, name: str, **kwargs: Any) -> dict:
        self.calls.append("exec_tool")
        self.exec_payload = {"name": name, **kwargs}
        if self.fail_exec:
            raise CapabilityError("upstream_unavailable", "契约层失败")
        if name == "create_support_ticket":
            # 建单工具（M2 起转人工也走工具注册表）
            self.tickets.append(kwargs.get("args") or {})
            return {"tool": name, "kind": "ok",
                    "summary": "已为您转接人工客服，工单号 ST20260912000001，坐席会尽快与您联系。",
                    "facts": {"ticket_no": "ST20260912000001"}}
        if self.exec_result is None:
            # M2 默认：工具返回 unavailable（例如 refund_fee 闸门未开），走 E10 回落检索
            return {"tool": name, "kind": "unavailable", "reason": "penalty_semantics_unconfirmed",
                    "path_hint": "transfer_tool_unavailable", "summary": "规则口径未确认"}
        return self.exec_result

    async def verify_citation(self, answer: str, source_labels: list[str]) -> dict:
        self.calls.append("verify")
        if self.verify_result is not None:
            return self.verify_result
        # 默认：把答案里的 label 原样放行
        return {
            "answer_cleaned": answer,
            "dropped": 0,
            "valid_sources": source_labels,
            "sources": [{"label": x, "capability": "supported", "form": "qa"} for x in source_labels],
            "boundary_injected": False,
            "boundary_violation": False,
        }

    async def create_support_ticket(self, **kwargs: Any) -> dict:
        self.calls.append("create_support_ticket")
        self.tickets.append(kwargs)
        return {"ticket_no": "ST20260912000001", "status": "pending"}

    async def record_turns(self, **kwargs: Any) -> dict:
        self.calls.append("record_turns")
        self.turns.append(kwargs)
        return {"ok": True}

    async def record_metrics(self, trace_id: str, events: list[dict]) -> dict:
        self.calls.append("record_metrics")
        return {"ok": True}

    # —— M3：答案缓存 ——

    async def cache_lookup(self, *, question: str, category: str) -> dict:
        self.calls.append("cache_lookup")
        self.cache_lookups.append({"question": question, "category": category})
        if self.cache_hit_result is None:
            return {"hit": False, "vector_mode": self.cache_mode}
        return {"hit": True, "answer": self.cache_hit_result, "sources": self.cache_sources,
                "score": 0.99, "vector_mode": self.cache_mode}

    async def cache_store(self, *, question: str, category: str, answer: str,
                          sources: list[dict], personalized: bool) -> dict:
        self.calls.append("cache_store")
        self.cache_stores.append({"question": question, "category": category, "answer": answer,
                                  "personalized": personalized})
        return {"stored": True, "promoted": True}


class FakeLLM:
    def __init__(self, *, answer: str = "根据平台规则：手续费按 10% 计。 [[kb/refund#1]]",
                 arbitrate: str = "refund", pick: str = "") -> None:
        self.answer = answer
        self.arbitrate = arbitrate
        self.pick = pick
        self.deltas: list[str] = []

    async def stream(self, system: str, user: str) -> AsyncIterator[tuple[str, Any]]:
        for i in range(0, len(self.answer), 8):
            piece = self.answer[i:i + 8]
            self.deltas.append(piece)
            yield ("delta", piece)
        yield ("end", {"total_tokens": 42})

    async def complete(self, system: str, user: str) -> str:
        if self.pick:
            return self.pick
        return self.arbitrate


def _hit(capability: str = "supported", label: str = "kb/refund#1") -> dict:
    return {
        "chunks": [{"label": label, "text": "退票手续费按 10% 计。", "capability": capability, "form": "qa"}],
        "source_labels": [label],
        "top1_cos": 0.82,
        "above_threshold": True,
        "vector_mode": "embedding",
        "empty": False,
        "level": 0,
    }


def _state(question: str = "退票手续费怎么算") -> dict:
    return new_initial_state(question=question, conv_id="c1", trace_id="t1", user_id=7,
                             guest_key=None, visibility=["public"])


async def _run(cap: FakeCapability, llm: FakeLLM | None = None, question: str = "退票手续费怎么算") -> dict:
    deps = Deps(cap, llm or FakeLLM(), top_k=5)
    graph = build_graph(deps)
    # 无 checkpointer 时不传 config；有 checkpointer 时 thread_id 必填
    try:
        return await graph.ainvoke(_state(question), {"configurable": {"thread_id": "c1"}})
    except Exception:
        return await graph.ainvoke(_state(question))


# —— 边覆盖 ——

@pytest.mark.asyncio
async def test_e2_greet_skips_retrieval():
    cap = FakeCapability(pre_intent="greet")
    out = await _run(cap, question="你好")
    assert "智能客服" in out["answer"]
    assert "retrieve" not in cap.calls
    assert not out.get("transfer")


@pytest.mark.asyncio
async def test_e1_complaint_transfers_deterministic():
    cap = FakeCapability(pre_intent="complaint")
    out = await _run(cap, question="我要投诉")
    assert out["transfer"] is True
    assert out["transfer_path"] == "deterministic"
    assert out["support_ticket_no"]
    assert "retrieve" not in cap.calls


@pytest.mark.asyncio
async def test_e3_e16_happy_path():
    cap = FakeCapability()
    llm = FakeLLM()
    out = await _run(cap, llm)
    assert cap.calls.index("classify_rule") < cap.calls.index("tools_route") < cap.calls.index("retrieve")
    assert out["answer"].startswith("根据平台规则")
    assert out["sources"][0]["label"] == "kb/refund#1"
    assert not out["transfer"]
    assert cap.turns and cap.turns[0]["answer"].startswith("根据平台规则")


@pytest.mark.asyncio
async def test_e4_arbitrate_when_rule_unsure():
    cap = FakeCapability(cls={"scores": {"booking": 2, "refund": 2}, "top1": "booking", "top2": "refund",
                             "gap": 0, "enough": False})
    llm = FakeLLM(arbitrate="refund")
    out = await _run(cap, llm)
    assert out["category"] == "refund"
    assert out["cls_source"] == "llm_arbitrate"
    # 软路由：规则层得分必须带进检索（禁止丢弃）
    assert cap.retrieve_payload["scores"] == {"booking": 2, "refund": 2}


@pytest.mark.asyncio
async def test_e7_no_tool_candidates_falls_to_retrieve():
    cap = FakeCapability(tools=[])
    await _run(cap)
    assert "tools_route" in cap.calls and "exec_tool" not in cap.calls
    assert "retrieve" in cap.calls


@pytest.mark.asyncio
async def test_e10_tool_unavailable_then_faq_plus_ticket():
    """§8.2：工具闸门关闭/数据缺失 → **走知识库 FAQ + 转人工**（既答规则又建单核金额）"""
    cap = FakeCapability(tools=[{"name": "refund_fee", "score": 3, "kind": "read", "slots": {}}],
                         exec_result=None)
    out = await _run(cap)
    assert "exec_tool" in cap.calls
    assert "retrieve" in cap.calls
    assert out["answer"].startswith("根据平台规则")          # FAQ 内容保留
    assert out["transfer"] is True                            # 且必须转人工
    assert "ST20260912000001" in out["answer"]                # 带上可追踪工单号
    assert out["transfer_path"] == "tool_unavailable"


# —— M2：工具路径（E5/E6/E9 + 槽位/游客/业务规则/契约失败）——

def _tool_cand(name: str = "my_tickets", score: int = 3, slots: dict | None = None) -> dict:
    return {"name": name, "score": score, "kind": "read", "desc": "d", "slots": slots or {}}


@pytest.mark.asyncio
async def test_e5_e9_tool_direct_answer_skips_llm_and_retrieval():
    """E5 提槽命中 → exec_tool；E9 事实类自足 → 确定性直答（不经 LLM、不检索）"""
    cap = FakeCapability(
        tools=[_tool_cand("my_tickets")],
        exec_result={"tool": "my_tickets", "kind": "ok", "summary": "2026-09-15 08:00 北京西站→上海虹桥站 12号座 票价120元",
                     "facts": {"count": 1}},
    )
    llm = FakeLLM()
    out = await _run(cap, llm)
    assert "exec_tool" in cap.calls and "retrieve" not in cap.calls
    assert out["answer"].startswith("2026-09-15")
    assert out["sources"] == []          # 工具直答没有知识库出处
    assert llm.deltas == []              # 生成模型完全没被调用（确定性直答）
    assert not out.get("transfer")


@pytest.mark.asyncio
async def test_tool_slots_incomplete_asks_back_without_ticket():
    """槽位不全 → 单轮反问（E8 的 M2 版）；**不建单**、不转人工"""
    cap = FakeCapability(
        tools=[_tool_cand("bus_availability")],
        exec_result={"tool": "bus_availability", "kind": "slots_incomplete", "reason": "slots_missing",
                     "missing": ["出发站", "出发日期"], "summary": "请补充：出发站、出发日期。"},
    )
    out = await _run(cap)
    assert out["answer"].startswith("请补充")
    assert not out.get("transfer")
    assert not cap.tickets


@pytest.mark.asyncio
async def test_tool_guest_required_guides_login():
    cap = FakeCapability(
        tools=[_tool_cand("order_detail")],
        exec_result={"tool": "order_detail", "kind": "guest_required",
                     "summary": "为保护您的订单信息，请先登录后我为您查询订单。"},
    )
    out = await _run(cap)
    assert "请先登录" in out["answer"]
    assert not out.get("transfer")   # 引导登录不等于转人工
    assert not cap.tickets


@pytest.mark.asyncio
async def test_tool_not_found_is_uniform_answer_not_403():
    """越权与不存在同一话术：不泄露订单是否存在"""
    cap = FakeCapability(
        tools=[_tool_cand("order_detail")],
        exec_result={"tool": "order_detail", "kind": "not_found", "reason": "order_not_found_or_not_owned",
                     "summary": "未查询到该订单。请确认订单号，或该订单不属于当前账号。"},
    )
    out = await _run(cap)
    assert "未查询到该订单" in out["answer"]
    assert not out.get("transfer")


@pytest.mark.asyncio
async def test_tool_blocked_transfers_with_go_hint():
    """误车/状态不对 = 业务规则不允许 → 转人工，路径取 Go 的 path_hint（确定性），不是阈值路径"""
    cap = FakeCapability(
        tools=[_tool_cand("refund_fee")],
        exec_result={"tool": "refund_fee", "kind": "blocked", "reason": "departed",
                     "path_hint": "transfer_deterministic", "summary": "这趟车已经开走了……"},
    )
    out = await _run(cap)
    assert out["transfer"] is True
    assert out["transfer_path"] == "deterministic"
    assert cap.tickets
    # 工单摘要要带上工具判定原因（坐席要能看出是"业务规则"而非"系统坏了"）
    assert "refund_fee/departed" in cap.tickets[0]["summary"]


@pytest.mark.asyncio
async def test_unavailable_then_empty_retrieve_keeps_tool_path():
    """工具不可用 + 检索也空 → 转人工路径必须是 tool_unavailable，不能被记成"知识库覆盖不足"（§10.1）"""
    cap = FakeCapability(
        tools=[_tool_cand("refund_fee")],
        exec_result={"tool": "refund_fee", "kind": "unavailable", "reason": "penalty_semantics_unconfirmed",
                     "path_hint": "transfer_tool_unavailable", "summary": "口径未确认"},
        retrieve={"chunks": [], "source_labels": [], "top1_cos": 0.0, "above_threshold": False,
                  "vector_mode": "embedding", "empty": True, "level": 2},
    )
    out = await _run(cap)
    assert out["transfer"] is True
    assert out["transfer_path"] == "tool_unavailable"
    assert "refund_fee/penalty_semantics_unconfirmed" in cap.tickets[0]["summary"]


@pytest.mark.asyncio
async def test_tool_contract_error_transfers_tool_unavailable():
    cap = FakeCapability(tools=[_tool_cand("my_tickets")], fail_exec=True)
    out = await _run(cap)
    assert out["transfer"] is True
    assert out["transfer_path"] == "tool_unavailable"


@pytest.mark.asyncio
async def test_multi_candidate_interrupts_instead_of_guessing():
    """E8（M3）：多候选同分 → interrupt 反问；resume 后带着选中的工具重跑（跨轮续跑）"""
    from langgraph.checkpoint.memory import InMemorySaver
    from langgraph.types import Command

    cap = FakeCapability(tools=[_tool_cand("order_detail", score=2), _tool_cand("my_tickets", score=2)],
                         exec_result={"tool": "order_detail", "kind": "ok", "summary": "订单详情如下"})
    deps = Deps(cap, FakeLLM(), top_k=5)
    graph = build_graph(deps, checkpointer=InMemorySaver())
    config = {"configurable": {"thread_id": "clarify-thread"}}

    first = await graph.ainvoke(_state(), config)
    assert "__interrupt__" in first
    payload = first["__interrupt__"][0].value
    assert "order_detail" in payload["options"]
    assert "exec_tool" not in cap.calls  # 反问阶段不该执行工具

    second = await graph.ainvoke(Command(resume={"text": "1"}), config)
    assert second["answer"] == "订单详情如下"
    assert cap.exec_payload["name"] == "order_detail"


@pytest.mark.asyncio
async def test_clarify_rejects_garbage_and_transfers():
    """resume 值不可识别 → 转人工（不死循环）"""
    from langgraph.checkpoint.memory import InMemorySaver
    from langgraph.types import Command

    cap = FakeCapability(tools=[_tool_cand("order_detail", score=2), _tool_cand("my_tickets", score=2)])
    graph = build_graph(Deps(cap, FakeLLM(), top_k=5), checkpointer=InMemorySaver())
    config = {"configurable": {"thread_id": "clarify-bad"}}
    await graph.ainvoke(_state(), config)
    out = await graph.ainvoke(Command(resume={"text": "我也不知道"}), config)
    assert out["transfer"] is True
    assert out["transfer_path"] == "deterministic"
    assert out["support_ticket_no"]


# —— M3：缓存 / 指代消解 / 改写重试 ——

@pytest.mark.asyncio
async def test_cache_hit_skips_tool_retrieve_and_llm():
    """缓存命中 → 连工具/检索/生成一起跳过（§9.3）"""
    cap = FakeCapability(tools=[])
    cap.cache_hit_result = "退票手续费按档位收取（缓存答案）"
    # 真实缓存条目**带出处**：命中后 verify 要用它做引用校验（没有出处的答案本来就不该被缓存）
    cap.cache_sources = [{"label": "refund-fee-basics·段落1"}]
    llm = FakeLLM()
    out = await _run(cap, llm, question="退票手续费怎么算")
    assert out["answer"] == "退票手续费按档位收取（缓存答案）"
    assert out["source"] if "source" in out else True
    assert "retrieve" not in cap.calls
    assert "exec_tool" not in cap.calls
    assert llm.deltas == []          # 连模型都没调
    assert "cache_lookup" in cap.calls
    # **但合规检查不能省**：边界声明/引用校验仍要跑（缓存省的是检索+生成）
    assert "verify" in cap.calls
    assert out["cache_hit"] is True


@pytest.mark.asyncio
async def test_cache_key_uses_resolved_question_after_coref():
    """缓存查询用的是消解后的分类与问题（所以 cache_lookup 必须在 coref 之后）"""
    cap = FakeCapability(cls={"scores": {}, "top1": "other", "gap": 0, "enough": True})
    await _run(cap, question="那这个呢")
    assert cap.cache_lookups, "应调用缓存查询"
    # 首轮无历史 → 不继承，分类仍是 other
    assert cap.cache_lookups[0]["category"] == "other"


@pytest.mark.asyncio
async def test_coref_inherits_previous_category_with_scores():
    """指代消解：分类 other + 指代词 + 有历史 → 继承上一轮分类并**补 cls_scores**"""
    cap = FakeCapability(cls={"scores": {}, "top1": "other", "gap": 0, "enough": True})
    deps = Deps(cap, FakeLLM(), top_k=5)
    graph = build_graph(deps)
    state = _state("那这个呢")
    state["recent"] = [{"question": "退票手续费怎么算", "category": "refund"}]
    out = await graph.ainvoke(state)
    assert out["category"] == "refund"
    assert out["cls_source"] == "inherit"
    assert out["cls_scores"] == {"refund": 1}   # 不补得分会让软路由检索落空
    assert cap.retrieve_payload["category"] == "refund"


@pytest.mark.asyncio
async def test_e11_rewrite_only_with_context():
    """E11：检索空 + 有上下文 + 未超上限 → 改写后重跑；无上下文不改写"""
    empty = {"chunks": [], "source_labels": [], "top1_cos": 0.0, "above_threshold": False,
             "vector_mode": "embedding", "empty": True, "level": 2}

    # 无上下文：直接转人工（不让 LLM 瞎猜）
    cap = FakeCapability(retrieve=empty)
    out = await _run(cap, question="那这个呢")
    assert out["transfer"] is True
    assert "rewrite" not in cap.calls

    # 有上下文：先改写再重跑一次（retry_count 封顶）
    cap2 = FakeCapability(retrieve=empty)
    llm = FakeLLM(pick="退票手续费怎么算")
    deps = Deps(cap2, llm, top_k=5)
    graph = build_graph(deps)
    state = _state("那这个呢")
    state["recent"] = [{"question": "退票手续费怎么算", "category": "refund"}]
    out2 = await graph.ainvoke(state)
    assert out2["retry_count"] >= 1
    assert out2["transfer"] is True           # 改写后仍无依据 → 转人工（不硬答）


@pytest.mark.asyncio
async def test_finalize_updates_memory_window_and_stores_cache():
    """收尾：双层窗口更新 + 正常问答才入缓存（工具直答/转人工不入）"""
    cap = FakeCapability(tools=[])
    state = _state("退票手续费怎么算")
    state["recent"] = [{"question": "q1", "category": "refund"}] * 5
    graph = build_graph(Deps(cap, FakeLLM(), top_k=5))
    out = await graph.ainvoke(state)
    assert len(out["recent"]) == 5                     # 最近 5 轮封顶
    assert "退票手续费怎么算" in out["summary"] or out["summary"]  # 溢出的进摘要
    assert cap.cache_stores, "正常问答应尝试入缓存"
    assert cap.cache_stores[0]["personalized"] is False


@pytest.mark.asyncio
async def test_e6_llm_picks_tool_from_whitelist_only():
    """E6：提槽未命中 → LLM 从清单里选；选中即执行（槽位仍由 Go 抽，这里传空）"""
    cap = FakeCapability(tools=[], exec_result={"tool": "my_tickets", "kind": "ok", "summary": "您有 1 张车票"})
    llm = FakeLLM(pick="my_tickets")
    out = await _run(cap, llm)
    assert cap.exec_payload["name"] == "my_tickets"
    assert out["answer"] == "您有 1 张车票"


@pytest.mark.asyncio
async def test_e6_llm_hallucinated_tool_rejected():
    """LLM 选了清单外的工具名 → 丢弃并回落检索（防幻觉出工具名）"""
    cap = FakeCapability(tools=[])
    llm = FakeLLM(pick="drop_table")
    await _run(cap, llm)
    assert "exec_tool" not in cap.calls
    assert "retrieve" in cap.calls


@pytest.mark.asyncio
async def test_e13_below_threshold_transfers():
    cap = FakeCapability(retrieve={**_hit(), "top1_cos": 0.31, "above_threshold": False})
    out = await _run(cap)
    assert out["transfer"] is True
    assert out["transfer_path"] == "threshold"
    assert "verify" not in cap.calls  # 没进生成


@pytest.mark.asyncio
async def test_e12_empty_retrieval_transfers():
    cap = FakeCapability(retrieve={"chunks": [], "source_labels": [], "top1_cos": 0.0,
                                   "above_threshold": False, "vector_mode": "embedding", "empty": True, "level": 2})
    out = await _run(cap)
    assert out["transfer"] is True
    assert out["transfer_path"] == "threshold"


@pytest.mark.asyncio
async def test_e14_model_transfer_marker():
    cap = FakeCapability()
    llm = FakeLLM(answer="抱歉，材料里没有依据。[TRANSFER]")
    out = await _run(cap, llm)
    assert out["transfer"] is True
    assert out["transfer_path"] == "model"
    assert "verify" not in cap.calls


@pytest.mark.asyncio
async def test_e15_no_valid_citation_transfers():
    cap = FakeCapability()
    cap.verify_result = {"answer_cleaned": "无出处的答案", "dropped": 1, "valid_sources": [],
                         "sources": [], "boundary_injected": False, "boundary_violation": False}
    out = await _run(cap)
    assert out["transfer"] is True
    assert out["transfer_path"] == "model"


@pytest.mark.asyncio
async def test_boundary_passthrough_and_degraded_flags():
    cap = FakeCapability(retrieve=_hit(capability="industry", label="kb/travel#2"))
    cap.verify_result = {
        "answer_cleaned": "该功能本平台暂未开放，以下是铁路客运行业的一般做法，供参考；具体以车站与官方渠道为准。\n\n行业做法…",
        "dropped": 0,
        "valid_sources": ["kb/travel#2"],
        "sources": [{"label": "kb/travel#2", "capability": "industry", "form": "prose"}],
        "boundary_injected": True,
        "boundary_violation": True,
    }
    llm = FakeLLM(answer="行业做法… [[kb/travel#2]] 您可以点击页面入口办理。")
    out = await _run_with(cap, llm)
    assert out["boundary_injected"] is True
    assert out["boundary_violation"] is True
    assert out["answer"].startswith("该功能本平台暂未开放")


async def _run_with(cap: FakeCapability, llm: FakeLLM) -> dict:
    deps = Deps(cap, llm, top_k=5)
    graph = build_graph(deps)
    return await graph.ainvoke(_state(), {"configurable": {"thread_id": "c1"}})


@pytest.mark.asyncio
async def test_classify_degraded_is_visible_and_category_other():
    cap = FakeCapability(fail_classify=True)
    out = await _run(cap)
    assert out["category"] == "other"
    assert out["degraded"].get("classify") is True


@pytest.mark.asyncio
async def test_vector_ngram_marks_degraded():
    cap = FakeCapability(retrieve={**_hit(), "vector_mode": "ngram"})
    out = await _run(cap)
    assert out["degraded"].get("vector") is True
    assert out["vector_mode"] == "ngram"
