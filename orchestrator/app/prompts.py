"""Prompt 组装：按 §5.9 做**物理分区**（【本平台能力】/【行业参考】）。

为什么必须物理分区而不是"在 prompt 里补一句"：
  行业知识库覆盖的平台未实现功能，若与平台已实现能力混在一起，模型会把行业规则讲成本平台能力，
  用户照着操作会发现入口不存在 —— 比答不上来更糟。分区 + 后处理拦截构成两道防线。
"""

from __future__ import annotations

BOUNDARY_PREFIX = "该功能本平台暂未开放，以下是铁路客运行业的一般做法，供参考；具体以车站与官方渠道为准。"

SYSTEM_PROMPT = """你是铁路客运票务平台的官方客服助手。只依据下面提供的【本平台能力】与【行业参考】作答。

硬性约束：
1. 只允许使用【本平台能力】与【行业参考】两个区块里的事实；区块里没有的内容，必须输出 [TRANSFER]（表示转人工），不得凭常识编造。
2. 引用必须使用 [[出处标签]] 格式，标签只能取自已提供区块中方括号里的 label，不得自造。
3. 【本平台能力】里的内容可以说明操作入口与路径，措辞用"本平台/您可以在…"。
4. 【行业参考】里的内容**禁止**表述为平台能力：不得出现"点击/进入/页面/入口/按钮/我们的/本站支持"等操作性措辞，且必须先说明这是行业通行做法、本平台是否支持以实际为准。
5. 涉及金额（如退票手续费）必须以【本平台能力】中给出的确定金额为准，不得自行计算。
6. 回答简洁、口语化，直接给结论，不要复述整段规则。

如果无法在给定材料中找到依据，只输出：[TRANSFER]"""


def build_context_block(chunks: list[dict]) -> str:
    """把检索结果按 capability 分成两个物理区块（§5.9 ③）。"""
    native: list[str] = []
    industry: list[str] = []
    for ch in chunks:
        label = ch.get("label", "")
        text = (ch.get("text") or "").strip()
        line = f"- [label: {label}] text: {text}"
        if ch.get("capability") == "supported":
            native.append(line)
        else:
            industry.append(line)

    parts: list[str] = []
    parts.append("【本平台能力】")
    parts.append("\n".join(native) if native else "（无）")
    parts.append("")
    parts.append("【行业参考】")
    parts.append("\n".join(industry) if industry else "（无）")
    return "\n".join(parts)


def build_user_prompt(question: str, chunks: list[dict], history: list[dict] | None = None) -> str:
    lines: list[str] = []
    if history:
        lines.append("【最近对话】")
        for turn in history[-4:]:
            q = turn.get("question", "")
            a = turn.get("answer", "")
            if q:
                lines.append(f"用户：{q}")
            if a:
                lines.append(f"客服：{a}")
        lines.append("")
    lines.append("【知识材料】")
    lines.append(build_context_block(chunks))
    lines.append("")
    lines.append(f"【用户问题】{question}")
    return "\n".join(lines)


ARBITRATE_PROMPT = """你是意图分类器。把用户问题归入且仅归入下面 7 类之一：
booking 车票预订 / order 订单与支付 / refund 退票改签 / account 账号与身份 / travel 乘车与车站服务 / app 系统与技术 / policy 投诉与政策。
只输出类别英文名，不要解释，不要标点。若无法归类，输出 other。"""

REWRITE_PROMPT = """把用户问题改写成一个更完整、更适合检索的查询：补全被省略的主体（如"它/这个/那趟"指代的对象），
不要添加原文没有的信息，不要回答问题。只输出改写后的查询。"""


def route_tool_prompt(question: str) -> str:
    return f"""可用的工具只有：order_query（查订单）。判断该问题是否需要调用工具。
只输出工具名，或输出 none。不要解释。

问题：{question}"""
