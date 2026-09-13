"""能力层客户端（Go `/internal/*`，§6.1）与 LLM 客户端。

超时/重试按 §6.2：
  - classify / tools.route：1s，失败可重试 1 次 → 降级跳过该判断
  - retrieve：1.5s，不重试（图内重试由 E11 表达）
  - tools/{name}：1.5s，不重试 → 转人工并单独计数
  - LLM：20s（流式为整轮预算）

错误只按 `kind` 决策，**不解析错误文案**（§6.1）。
"""

from __future__ import annotations

import json
from typing import Any, AsyncIterator, Protocol

import httpx

from .settings import Settings


class CapabilityError(Exception):
    def __init__(self, kind: str, message: str = "", status: int = 0) -> None:
        super().__init__(f"{kind}: {message}")
        self.kind = kind
        self.message = message
        self.status = status


class CapabilityClient:
    def __init__(self, settings: Settings, client: httpx.AsyncClient | None = None) -> None:
        self.settings = settings
        headers = {"X-Internal-Key": settings.internal_key, "Content-Type": "application/json"}
        # trust_env=False：内网调用**绝不能走系统代理**。
        # 实测踩过：本机开着 Clash 类代理时，httpx 会把 127.0.0.1 的请求也发给代理，统一返回 502。
        self._client = client or httpx.AsyncClient(base_url=settings.go_base_url, headers=headers,
                                                   trust_env=False)

    async def aclose(self) -> None:
        await self._client.aclose()

    async def _post(self, path: str, payload: dict, timeout: float) -> dict:
        try:
            resp = await self._client.post(path, json=payload, timeout=timeout)
        except httpx.TimeoutException as exc:
            raise CapabilityError("upstream_unavailable", f"timeout: {path}") from exc
        except httpx.HTTPError as exc:
            raise CapabilityError("upstream_unavailable", str(exc)) from exc

        if resp.status_code >= 300:
            kind = "upstream_unavailable"
            message = resp.text[:200]
            try:
                body = resp.json()
                err = body.get("error") or {}
                kind = err.get("kind", kind)
                message = err.get("message", message)
            except (json.JSONDecodeError, AttributeError):
                pass
            raise CapabilityError(kind, message, resp.status_code)
        return resp.json()

    # —— 契约方法 ——

    async def pre_intent(self, question: str) -> str:
        data = await self._post("/internal/classify/pre-intent", {"question": question},
                                self.settings.timeout_classify_s)
        return data.get("pre_intent", "none")

    async def classify_rule(self, question: str) -> dict:
        return await self._post("/internal/classify/rule", {"question": question},
                                self.settings.timeout_classify_s)

    async def retrieve(self, *, question: str, category: str, scores: dict, visibility: list[str],
                       top_k: int, scope_kind: str = "", scope_ref: str = "") -> dict:
        return await self._post("/internal/retrieve", {
            "question": question,
            "category": category,
            "scores": scores,
            "visibility": visibility,
            "top_k": top_k,
            "scope_kind": scope_kind,
            "scope_ref": scope_ref,
        }, self.settings.timeout_retrieve_s)

    async def tools_route(self, question: str, category: str = "", session_hint: str = "") -> dict:
        return await self._post("/internal/tools/route",
                                {"question": question, "category": category, "session_hint": session_hint},
                                self.settings.timeout_classify_s)

    async def exec_tool(self, name: str, *, args: dict, user_id: int | None, guest_key: str | None,
                        trace_id: str) -> dict:
        return await self._post(f"/internal/tools/{name}", {
            "args": args,
            "user_id": user_id,
            "guest_key": guest_key,
            "trace_id": trace_id,
        }, self.settings.timeout_tool_s)

    async def verify_citation(self, answer: str, source_labels: list[str]) -> dict:
        return await self._post("/internal/citation/verify", {
            "answer": answer,
            "source_labels": source_labels,
        }, self.settings.timeout_classify_s)

    # —— M3：答案缓存（§9.3，查/存都由图内节点发起）——

    async def cache_lookup(self, *, question: str, category: str) -> dict:
        return await self._post("/internal/cache/lookup",
                                {"question": question, "category": category},
                                self.settings.timeout_classify_s)

    async def cache_store(self, *, question: str, category: str, answer: str,
                          sources: list[dict], personalized: bool) -> dict:
        return await self._post("/internal/cache/store", {
            "question": question,
            "category": category,
            "answer": answer,
            "sources": sources,
            "personalized": personalized,
        }, self.settings.timeout_classify_s)

    async def create_support_ticket(self, *, conv_id: str, user_id: int | None, order_no: str,
                                    category: str, path: str, summary: str) -> dict:
        return await self._post("/internal/support-tickets", {
            "conv_id": conv_id,
            "user_id": user_id,
            "order_no": order_no,
            "category": category,
            "path": path,
            "summary": summary,
        }, self.settings.timeout_classify_s)

    async def record_turns(self, *, conv_id: str, user_id: int | None, question: str, answer: str,
                           category: str, source: str, trace_id: str) -> dict:
        return await self._post("/internal/session/turns", {
            "conv_id": conv_id,
            "user_id": user_id,
            "question": question,
            "answer": answer,
            "category": category,
            "source": source,
            "trace_id": trace_id,
        }, self.settings.timeout_classify_s)

    async def record_metrics(self, trace_id: str, events: list[dict]) -> dict:
        return await self._post("/internal/metrics", {"trace_id": trace_id, "events": events},
                                self.settings.timeout_classify_s)


class LLMClient(Protocol):
    """编排层只依赖这个协议，方便测试注入假模型。"""

    async def stream(self, system: str, user: str) -> AsyncIterator[tuple[str, Any]]: ...

    async def complete(self, system: str, user: str) -> str: ...


class GatewayLLM:
    """经 Go 网关（OpenAI 兼容 /internal/v1/chat/completions）的 LLM 客户端。

    走 langchain-openai 的 ChatOpenAI —— 这是 §5.10.1 明确"要用 LangChain"的部分
    （模型调用 + prompt + 结构化输出），而工具/检索/记忆/缓存都不用它的抽象。
    """

    def __init__(self, settings: Settings) -> None:
        import httpx
        from langchain_openai import ChatOpenAI  # 延迟导入：不配 LLM 时也能起服务

        self.settings = settings
        # http_client 同样禁用代理：网关在本机/内网，走代理会 502（实测）
        self._model = ChatOpenAI(
            model=settings.llm_model,
            base_url=settings.gateway_chat_base(),
            api_key=settings.llm_api_key or "internal",
            default_headers={"X-Internal-Key": settings.internal_key},
            timeout=settings.timeout_llm_s,
            temperature=0.6,  # 关思考模型的推荐取值（§11.3）
            http_client=httpx.Client(trust_env=False, timeout=settings.timeout_llm_s),
            http_async_client=httpx.AsyncClient(trust_env=False, timeout=settings.timeout_llm_s),
        )
        self.name = f"gateway:{settings.llm_model}"

    async def complete(self, system: str, user: str) -> str:
        """非流式一次调用：用于"只决定调哪个工具"这类短输出决策。

        与 stream 分开的理由：决策类调用不需要 SSE，也不需要 delta 事件
        （§5.10.3 的流事件只服务用户可见的生成过程）。
        """
        msg = await self._model.ainvoke([("system", system), ("user", user)])
        text = msg.content
        if isinstance(text, list):  # 部分供应商返回分段内容
            text = "".join(part.get("text", "") for part in text if isinstance(part, dict))
        return text or ""

    async def stream(self, system: str, user: str) -> AsyncIterator[tuple[str, Any]]:
        messages = [("system", system), ("user", user)]
        usage: dict[str, int] = {}
        async for chunk in self._model.astream(messages):
            text = chunk.content
            if isinstance(text, list):  # 部分供应商返回分段内容
                text = "".join(part.get("text", "") for part in text if isinstance(part, dict))
            if text:
                yield ("delta", text)
            meta = getattr(chunk, "usage_metadata", None)
            if meta:
                usage = {
                    "prompt_tokens": meta.get("input_tokens", 0),
                    "output_tokens": meta.get("output_tokens", 0),
                    "total_tokens": meta.get("total_tokens", 0),
                }
        yield ("end", usage)

    async def complete(self, system: str, user: str) -> str:
        result = await self._model.ainvoke([("system", system), ("user", user)])
        content = result.content
        if isinstance(content, list):
            content = "".join(part.get("text", "") for part in content if isinstance(part, dict))
        return str(content).strip()
