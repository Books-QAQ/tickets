"""多轮记忆与确定性指代消解（§9.2 / §9.4）。

两条纪律：
  1. **压缩走确定性模板、不调 LLM**：Turn 只存 {问题, 分类}，LLM 摘要增益有限；
     确定性零延迟零成本，且与"能预期"的客服目标一致。
  2. **指代继承只在"分类落 other"时触发**：分类已命中（"那退款呢"含"退款"直接命中 refund）
     就不消解 —— 否则"这个耳机多少钱"这种含"这个"的正常问法会被误伤。
"""

from __future__ import annotations

import re

# 指代词（§9.4.1）。刻意避开"这个/那个"单独出现在正常问法里的误伤：
# 触发条件是 **分类落 other**，正常问法会先被分类命中，所以这里可以放宽列词。
PRONOUNS = ("那", "它", "这个", "那个", "这些", "那些", "还有", "呢", "同样", "还是", "也", "继续")

# 明显是"接着上一句"的短问法（比单字更硬的信号）
FOLLOWUP_PATTERNS = (
    r"^那(呢|么|个|这)?[?？]?$",
    r"^还有(呢|吗|么)?[?？]?$",
    r"^然后呢[?？]?$",
    r"^继续[?？]?$",
    r"^(它|他|她)呢[?？]?$",
    r"^\d+$",           # "1" 选项选择
    r"^第[一二三四五六七八九十\d]+个[?？]?$",
)


def has_pronoun(question: str) -> bool:
    q = (question or "").strip()
    if not q:
        return False
    for pat in FOLLOWUP_PATTERNS:
        if re.match(pat, q):
            return True
    return any(p in q for p in PRONOUNS)


def summary_to_list(summary: str | list | None) -> list[str]:
    if isinstance(summary, list):
        return [str(x) for x in summary if x]
    if not summary:
        return []
    return [x for x in str(summary).split("；") if x.strip()]


def build_summary_entry(question: str, category: str) -> str:
    return f"{question}({category})"


def update_memory(*, recent: list | None, summary: str | list | None, question: str, category: str,
                  recent_n: int = 5, summary_max: int = 10) -> tuple[list, str]:
    """把本轮并进双层窗口：最近 `recent_n` 轮完整；溢出的压成摘要（最多 `summary_max` 条）。

    返回 (recent, summary_str)。**纯函数**：同一输入必得同一输出，可单测。
    """
    turns = [dict(t) for t in (recent or [])]
    turns.append({"question": question, "category": category or "other"})

    entries = summary_to_list(summary)
    while len(turns) > max(1, recent_n):
        oldest = turns.pop(0)
        entries.append(build_summary_entry(oldest.get("question", ""), oldest.get("category", "other")))
    # 第 16 轮起丢最旧摘要（保持总记忆约 15 轮）
    if len(entries) > summary_max:
        entries = entries[-summary_max:]

    return turns, "；".join(entries)


def last_category(recent: list | None, summary: str | list | None) -> str:
    """最近一轮的分类（优先 recent，其次摘要最后一条）。"""
    if recent:
        cat = (recent[-1] or {}).get("category")
        if cat:
            return str(cat)
    entries = summary_to_list(summary)
    if entries:
        m = re.search(r"\(([a-z_]+)\)\s*$", entries[-1])
        if m:
            return m.group(1)
    return ""


def last_question(recent: list | None, summary: str | list | None) -> str:
    if recent:
        q = (recent[-1] or {}).get("question")
        if q:
            return str(q)
    entries = summary_to_list(summary)
    if entries:
        m = re.match(r"^(.*)\([a-z_]+\)\s*$", entries[-1])
        if m:
            return m.group(1)
    return ""


def context_text(recent: list | None, summary: str | list | None) -> str:
    """给 LLM 的两段式上下文：更早摘要 + 最近对话（§9.2 消费点之一）。"""
    entries = summary_to_list(summary)
    parts: list[str] = []
    if entries:
        parts.append("更早对话摘要：" + "；".join(entries))
    if recent:
        lines = [f"用户：{t.get('question','')}（分类 {t.get('category','')}）" for t in recent]
        parts.append("最近对话：\n" + "\n".join(lines))
    return "\n".join(parts)


def resolve_reference(*, question: str, category: str, recent: list | None,
                      summary: str | list | None) -> dict:
    """确定性指代消解（§9.4.1）。

    触发三条件（缺一不可）：分类落 `other` + 含指代词 + 有历史话题。
    返回要写进 state 的字段；不触发时返回 {}。

    **继承时必须补 cls_scores**：软路由/检索要用分类得分，
    只改 category 不带得分会让检索落空（这是"分类+软路由"衔接的经典坑）。
    """
    if (category or "") not in ("", "other"):
        return {}
    if not has_pronoun(question):
        return {}
    prev = last_category(recent, summary)
    if not prev or prev == "other":
        return {}
    return {
        "category": prev,
        "cls_source": "inherit",
        "cls_scores": {prev: 1},
        "coref_from": last_question(recent, summary),
    }
