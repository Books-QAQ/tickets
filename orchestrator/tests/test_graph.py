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
                 verify: dict | None = None, fail_classify: bool = False,
                 exec_result: dict | None = None) -> None:
        self.pre_intent_value = pre_intent
        self.cls = cls if cls is not None else {"scores": {"booking": 6}, "top1": "booking", "gap": 6, "enough": True}
        self.retrieve_result = retrieve if retrieve is not None else _hit()
        self.tools = tools or []
        self.verify_result = verify
        self.fail_classify = fail_classify
        self.exec_result = exec_result
        self.calls: list[str] = []
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

    async def tools_route(self, question: str, session_hint: str = "") -> dict:
        self.calls.append("tools_route")
        return {"candidates": self.tools}

    async def exec_tool(self, name: str, **kwargs: Any) -> dict:
        self.calls.append("exec_tool")
        if self.exec_result is None:
            raise CapabilityError("not_implemented", "M1 未实现")
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


class FakeLLM:
    def __init__(self, *, answer: str = "根据平台规则：手续费按 10% 计。 [[kb/refund#1]]",
                 arbitrate: str = "refund") -> None:
        self.answer = answer
        self.arbitrate = arbitrate
        self.deltas: list[str] = []

    async def stream(self, system: str, user: str) -> AsyncIterator[tuple[str, Any]]:
        for i in range(0, len(self.answer), 8):
            piece = self.answer[i:i + 8]
            self.deltas.append(piece)
            yield ("delta", piece)
        yield ("end", {"total_tokens": 42})

    async def complete(self, system: str, user: str) -> str:
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
async def test_e10_tool_present_but_unimplemented_falls_back():
    cap = FakeCapability(tools=[{"tool": "order_query", "args": {}}], exec_result=None)
    await _run(cap)
    assert "exec_tool" in cap.calls
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
