"""图状态（§5.2）。

铁律：
  1. `user_id` 只能来自 Go 注入，图内**不得**从 question 解析身份；
  2. State 里**只放可序列化数据**（它进 checkpoint）：客户端/连接池/logger 一律走闭包注入，
     否则 resume 时会反序列化失败（M0 已实测 resume 会重放节点）。
"""

from __future__ import annotations

from typing import Annotated, Any, TypedDict


def merge_dict(left: dict | None, right: dict | None) -> dict:
    """并行分支写同一 key 时合并（后写覆盖同键）。"""
    out: dict[str, Any] = dict(left or {})
    out.update(right or {})
    return out


def append_list(left: list | None, right: list | None) -> list:
    return list(left or []) + list(right or [])


class CSState(TypedDict, total=False):
    # —— 输入（Go 注入）——
    question: str
    conv_id: str  # = thread_id
    user_id: int | None  # 服务端解析结果，None = 游客
    guest_key: str | None  # 游客设备 hash（Go 侧算好）
    trace_id: str
    visibility: list[str]  # public / public+authenticated（由 Go 按身份决定）

    # —— 决策中间态 ——
    pre_intent: str  # complaint | human | greet | none
    category: str
    sub_scenario: str | None
    form: str  # qa | prose
    cls_source: str  # rule | llm_arbitrate | inherit | fallback
    cls_scores: dict[str, int]  # 软路由要用，禁止丢弃
    retry_count: int

    # —— M3：会话记忆（图状态内存活，checkpoint 就是它的持久层 —— §9.2/ADR-9）——
    recent: list  # 最近 N 轮完整 {question, category}
    summary: str  # 第 6~15 轮的确定性摘要「问题(分类)；…」
    has_context: bool
    search_question: str  # 改写后用于分类/检索的问题（原问题仍保留在 question）
    rewrite_failed: bool  # E11：改写被拒（不是问句/带引用/太长）→ 直接转人工，不再空跑第二遍
    coref_from: str  # 指代继承自哪一轮（观测用）
    rewrite_max: int  # E11 上限（来自 settings）
    interrupt_max: int  # 每轮 interrupt 上限

    # —— M3：答案缓存 ——
    cache_hit: bool
    cache_score: float
    cache_mode: str  # embedding | ngram | disabled

    # —— M3：消歧反问（interrupt）——
    clarify_options: list  # 展示用（已脱敏）
    clarify_values: list  # 机器可用的原始值（与 options 同序，仅在图状态内使用）
    clarify_resolved: str
    clarify_rejected: bool

    # —— 派生标志（节点写入、条件边读取）——
    # 注意：**必须在这里声明**，否则 langgraph 会静默丢弃未声明的 key，
    # 条件边就会读到 None（本 M1 实测踩过：above_threshold 丢失导致 E13 走错分支）。
    cls_enough: bool
    tool_candidates: list
    tool_missing: bool
    tool_direct_answer: str | None
    retrieve_empty: bool
    retrieve_failed: bool
    above_threshold: bool
    citation_dropped: int
    boundary_injected: bool
    boundary_violation: bool
    verify_valid: bool

    # —— 供给 ——
    tool_name: str | None
    tool_args: dict
    tool_slots: dict  # Go 抽好的槽位（原样回传，避免两层各抽一遍）
    tool_result: dict | None
    tool_kind: str  # ok|empty|slots_incomplete|guest_required|not_found|unavailable|blocked
    tool_path_hint: str  # transfer_tool_unavailable | transfer_deterministic | transfer_capability_absent
    tools_available: list  # 工具清单（E6 的 LLM 兜底路由只能从这里选，白名单）
    order_no: str  # 从槽位带出的订单号（工单摘要用）
    chunks: list[dict]
    source_labels: list[str]
    top1_cos: float
    vector_mode: str
    widened_level: int

    # —— 输出 ——
    answer: str
    sources: list[dict]
    transfer: bool
    transfer_path: str  # deterministic | threshold | model | tool_unavailable | capability_absent
    support_ticket_no: str | None

    # —— 观测 ——
    degraded: Annotated[dict, merge_dict]
    stage_ms: Annotated[dict, merge_dict]
    tokens: Annotated[dict, merge_dict]
    history: list  # 【废弃】M1 的 append-only 轮次累积；M3 起改用 recent + summary（不再写入）
    events: Annotated[list, append_list]  # 埋点事件（落 /internal/metrics）


def new_initial_state(*, question: str, conv_id: str, trace_id: str, user_id: int | None,
                      guest_key: str | None, visibility: list[str]) -> CSState:
    return CSState(
        question=question,
        conv_id=conv_id,
        trace_id=trace_id,
        user_id=user_id,
        guest_key=guest_key,
        visibility=visibility,
        cls_scores={},
        tool_args={},
        chunks=[],
        source_labels=[],
        sources=[],
        degraded={},
        stage_ms={},
        tokens={},
        retry_count=0,
        # M3：**不要在这里设 recent/summary**！
        # 入口把 new_initial_state() 当输入传给 ainvoke，langgraph 会把它当作一次 state 更新
        # —— 写成 recent=[]/summary="" 就等于每轮把 checkpoint 里的记忆清空（多轮指代必失效）。
        # 记忆由 load_context 从 checkpoint 里读（`or []` 兜底），由 finalize 写回。
        search_question=question,
        cache_hit=False,
        clarify_options=[],
        clarify_values=[],
    )
