"""图装配（§5.3）。

M1 实现的边集（§17）：**E1 / E2 / E3 / E4 / E7 / E10 / E13 / E14 / E15 / E16**
暂缓（属后续里程碑，见 M1 滞留清单）：
  - E5/E6/E9（工具执行与确定性直答）→ M2 工具层
  - E8（消歧反问 interrupt）→ M2/M3
  - E11（改写重试循环 retry_count ≤1）→ M3 记忆/多轮
条件边是纯函数（不写状态），所以"转人工路径"由节点显式写入 state 的 transfer_path 表达。
"""

from __future__ import annotations

from typing import Any, Literal

from langgraph.graph import END, START, StateGraph

from . import nodes as N
from .state import CSState


# —— 条件边（E1~E16 的实现，评审与测试按此对照）——

def edge_after_pre_intent(state: CSState) -> Literal["transfer", "greet", "classify_rule"]:
    intent = state.get("pre_intent") or "none"
    if intent in ("complaint", "human"):
        return "transfer"  # E1
    if intent == "greet":
        return "greet"  # E2
    return "classify_rule"


def edge_after_classify(state: CSState) -> Literal["coref", "llm_arbitrate"]:
    if state.get("cls_source") == "fallback":
        # 规则层不可用 → 按 §6.2 跳过该判断（分类落 other），不再调 LLM 兜底
        return "coref"
    if state.get("cls_enough"):
        return "coref"  # E3
    return "llm_arbitrate"  # E4


def edge_after_cache(state: CSState) -> Literal["verify", "route_tool"]:
    """M3：缓存命中就连工具/检索/生成一起跳过（§9.3）。

    **但仍然要过 verify**：边界声明注入与引用校验是"答案合规"的一部分，
    缓存省的是检索+生成，不是合规检查（命中直接 finalize 会让能力边界声明消失，M3 实测踩过）。
    """
    if state.get("cache_hit"):
        return "verify"
    return "route_tool"  # E7 之前


def edge_after_route_tool(state: CSState) -> Literal["exec_tool", "clarify", "retrieve"]:
    if state.get("tool_name"):
        return "exec_tool"  # E5/E6
    if state.get("clarify_options"):
        return "clarify"  # E8（M3：interrupt 版）
    return "retrieve"  # E7


def edge_after_exec_tool(state: CSState) -> Literal["tool_answer", "clarify", "retrieve", "transfer"]:
    if state.get("transfer"):
        return "transfer"  # 业务规则不允许/契约失败 → 转人工（路径由 path_hint 定）
    if state.get("clarify_options"):
        return "clarify"  # E8：工具给了多候选（多张可退车票/多笔订单）
    if state.get("tool_direct_answer"):
        return "tool_answer"  # E9：事实类且自足 → 确定性直答
    return "retrieve"  # E10：工具不可用/需与知识库融合 → 回落检索


def edge_after_clarify(state: CSState) -> Literal["exec_tool", "transfer"]:
    """E8 的出口：用户选定了 → 带槽位重跑工具；没听懂/乱答 → 转人工（不死循环）。"""
    if state.get("clarify_rejected") or not state.get("clarify_resolved"):
        return "transfer"
    if state.get("tool_name"):
        return "exec_tool"
    return "transfer"


def edge_after_rewrite(state: CSState) -> Literal["classify_rule", "transfer"]:
    """E11 出口：改写有效 → 重跑分类/检索；改写被拒 → 转人工（不再空跑一遍）。"""
    if state.get("rewrite_failed"):
        return "transfer"
    return "classify_rule"


def edge_after_retrieve(state: CSState) -> Literal["rewrite", "transfer", "generate"]:
    if state.get("retrieve_empty"):
        # E11：检索空 + 有上下文 + 未超上限 → 改写后重跑（只改写不回答）
        if not state.get("retrieve_failed") \
                and state.get("has_context") \
                and int(state.get("retry_count") or 0) < int(state.get("rewrite_max") or 1):
            return "rewrite"
        return "transfer"  # E12
    if not state.get("above_threshold"):
        return "transfer"  # E13
    return "generate"


def edge_after_generate(state: CSState) -> Literal["transfer", "verify"]:
    if state.get("transfer"):
        return "transfer"  # E14
    return "verify"


def edge_after_verify(state: CSState) -> Literal["transfer", "finalize"]:
    if not state.get("verify_valid"):
        return "transfer"  # E15
    # §8.2：工具闸门关闭/数据缺失时是"**走知识库 FAQ + 转人工**"——
    # 既要把规则讲清楚（FAQ 已答出来），也要建单让人工核金额（不能只答不转）。
    if state.get("tool_path_hint") == "transfer_tool_unavailable":
        return "transfer"
    return "finalize"  # E16


def build_graph(deps: N.Deps, checkpointer: Any = None):
    """装配并编译图。checkpointer 为 None 时图无状态（仅测试用）。"""
    builder = StateGraph(CSState)

    async def load_context(state: CSState) -> dict:
        return await N.load_context(state, deps)

    async def pre_intent(state: CSState) -> dict:
        return await N.pre_intent(state, deps)

    async def greet(state: CSState) -> dict:
        return await N.greet(state, deps)

    async def classify_rule(state: CSState) -> dict:
        return await N.classify_rule(state, deps)

    async def llm_arbitrate(state: CSState) -> dict:
        return await N.llm_arbitrate(state, deps)

    async def coref(state: CSState) -> dict:
        return await N.coref(state, deps)

    async def cache_lookup(state: CSState) -> dict:
        return await N.cache_lookup(state, deps)

    async def rewrite(state: CSState) -> dict:
        return await N.rewrite(state, deps)

    async def route_tool(state: CSState) -> dict:
        return await N.route_tool(state, deps)

    async def exec_tool(state: CSState) -> dict:
        return await N.exec_tool(state, deps)

    async def tool_answer(state: CSState) -> dict:
        return await N.tool_answer(state, deps)

    async def clarify(state: CSState) -> dict:
        return await N.clarify(state, deps)

    async def retrieve(state: CSState) -> dict:
        return await N.retrieve(state, deps)

    async def generate(state: CSState) -> dict:
        return await N.generate(state, deps)

    async def verify(state: CSState) -> dict:
        return await N.verify(state, deps)

    async def transfer(state: CSState) -> dict:
        return await N.transfer(state, deps)

    async def finalize(state: CSState) -> dict:
        return await N.finalize(state, deps)

    for name, fn in [
        ("load_context", load_context),
        ("pre_intent", pre_intent),
        ("greet", greet),
        ("classify_rule", classify_rule),
        ("llm_arbitrate", llm_arbitrate),
        ("coref", coref),
        ("cache_lookup", cache_lookup),
        ("rewrite", rewrite),
        ("route_tool", route_tool),
        ("exec_tool", exec_tool),
        ("tool_answer", tool_answer),
        ("clarify", clarify),
        ("retrieve", retrieve),
        ("generate", generate),
        ("verify", verify),
        ("transfer", transfer),
        ("finalize", finalize),
    ]:
        builder.add_node(name, fn)

    builder.add_edge(START, "load_context")
    builder.add_edge("load_context", "pre_intent")
    builder.add_conditional_edges("pre_intent", edge_after_pre_intent,
                                  ["transfer", "greet", "classify_rule"])
    builder.add_conditional_edges("classify_rule", edge_after_classify,
                                  ["coref", "llm_arbitrate"])
    builder.add_edge("llm_arbitrate", "coref")
    builder.add_edge("coref", "cache_lookup")
    builder.add_conditional_edges("cache_lookup", edge_after_cache, ["verify", "route_tool"])
    builder.add_conditional_edges("route_tool", edge_after_route_tool, ["exec_tool", "clarify", "retrieve"])
    builder.add_conditional_edges("exec_tool", edge_after_exec_tool, ["tool_answer", "clarify", "retrieve", "transfer"])
    builder.add_conditional_edges("clarify", edge_after_clarify, ["exec_tool", "transfer"])
    builder.add_conditional_edges("retrieve", edge_after_retrieve, ["rewrite", "transfer", "generate"])
    builder.add_conditional_edges("rewrite", edge_after_rewrite, ["classify_rule", "transfer"])  # E11
    builder.add_conditional_edges("generate", edge_after_generate, ["transfer", "verify"])
    builder.add_conditional_edges("verify", edge_after_verify, ["transfer", "finalize"])
    builder.add_edge("greet", "finalize")
    builder.add_edge("tool_answer", "finalize")
    # 注意：clarify 的出边是**条件边**（E8：选定 → exec_tool / 不可识别 → transfer），
    # 不能再加静态边 "clarify"→"finalize"，否则 resume 时 transfer 与 finalize 会在同一
    # 超步并行执行，两个节点都写 answer → InvalidUpdateError（M3 实测踩过）
    builder.add_edge("transfer", "finalize")
    builder.add_edge("finalize", END)

    if checkpointer is not None:
        return builder.compile(checkpointer=checkpointer)
    return builder.compile()
