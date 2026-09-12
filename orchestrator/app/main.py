"""编排层 HTTP 服务（§5.1）：POST /ask（JSON / SSE）、GET /healthz。

不做的事（边界）：
  - 不解析用户身份（user_id 由 Go 注入，图内不解析）；
  - 不做前端/Go 侧的 SSE 透传（M3）；
  - 不读 checkpoint 表做业务查询（业务查询走 cs_messages，由 Go 提供）。
"""

from __future__ import annotations

import asyncio
import contextlib
import json
import uuid
from typing import Any, AsyncIterator

from fastapi import FastAPI, Header, HTTPException, Request
from fastapi.responses import JSONResponse
from sse_starlette.sse import EventSourceResponse

from .clients import CapabilityClient, CapabilityError, GatewayLLM
from .graph import build_graph
from .nodes import Deps
from .settings import Settings
from .state import new_initial_state


def create_app(*, settings: Settings | None = None, capability: Any = None, llm: Any = None,
               checkpointer: Any = None) -> FastAPI:
    settings = settings or Settings.from_env()
    capability = capability or CapabilityClient(settings)
    llm = llm or GatewayLLM(settings)
    graph = build_graph(Deps(capability, llm, top_k=settings.top_k), checkpointer=checkpointer)

    @contextlib.asynccontextmanager
    async def lifespan(app: FastAPI):
        # checkpoint 表由 AIOMySQLSaver.setup() 建（ADR-2）。M0 实测：重复执行会告警
        # "Table 'checkpoint_migrations' already exists" —— 非致命，但应只在初始化时跑。
        if checkpointer is not None and hasattr(checkpointer, "setup"):
            try:
                await checkpointer.setup()
            except Exception as exc:  # noqa: BLE001
                print(f"[warn] checkpointer.setup() 失败（表可能已存在）: {exc}")
        yield
        with contextlib.suppress(Exception):
            await capability.aclose()

    app = FastAPI(title="tickets-cs-orchestrator", lifespan=lifespan)
    app.state.settings = settings
    app.state.graph = graph
    app.state.capability = capability
    app.state.llm = llm
    app.state.checkpointer = checkpointer

    def _auth(key: str | None) -> None:
        if not settings.internal_key:
            raise HTTPException(status_code=503, detail="internal key 未配置，拒绝调用")
        if key != settings.internal_key:
            raise HTTPException(status_code=401, detail="internal key 校验失败")

    @app.get("/healthz")
    async def healthz() -> JSONResponse:
        checks = {"capability": False, "checkpointer": checkpointer is not None, "graph": True}
        try:
            await capability.pre_intent("健康检查")
            checks["capability"] = True
        except Exception:  # noqa: BLE001
            checks["capability"] = False
        ready = checks["capability"] and checks["graph"]
        return JSONResponse({"ready": ready, "checks": checks}, status_code=200 if ready else 503)

    @app.post("/ask")
    async def ask(request: Request, x_internal_key: str | None = Header(default=None)):
        _auth(x_internal_key)
        try:
            body = await request.json()
        except Exception as exc:  # noqa: BLE001
            # 请求体不是合法 UTF-8 JSON（例如用 curl 直发 GBK 中文）→ 400，而不是 500
            raise HTTPException(status_code=400, detail=f"请求体必须是 UTF-8 JSON: {exc}") from exc
        question = (body.get("question") or "").strip()
        if not question:
            raise HTTPException(status_code=400, detail="question 不能为空")
        conv_id = body.get("conv_id") or str(uuid.uuid4())
        trace_id = body.get("trace_id") or str(uuid.uuid4())
        want_stream = bool(body.get("stream", True))
        visibility = body.get("visibility") or ["public"]
        if body.get("user_id"):
            # 登录用户可见 authenticated 文档（身份由 Go 解析后注入）
            visibility = ["public", "authenticated"]

        config = {"configurable": {"thread_id": conv_id}}
        initial = new_initial_state(
            question=question,
            conv_id=conv_id,
            trace_id=trace_id,
            user_id=body.get("user_id"),
            guest_key=body.get("guest_key"),
            visibility=visibility,
        )

        # §5.10.4 分派规则：有未决 interrupt → Command(resume=...)，否则普通 dict
        graph_input: Any = initial
        snapshot = None
        with contextlib.suppress(Exception):
            snapshot = await graph.aget_state(config)
        if snapshot is not None and getattr(snapshot, "tasks", None):
            has_interrupt = any(getattr(t, "interrupts", None) for t in snapshot.tasks)
            if has_interrupt:
                from langgraph.types import Command

                graph_input = Command(resume={"text": question})

        if not want_stream:
            result = await graph.ainvoke(graph_input, config)
            return JSONResponse(_final_payload(result, conv_id, trace_id))

        return EventSourceResponse(_stream(graph, graph_input, config, conv_id, trace_id, capability, llm))

    async def _stream(graph, graph_input, config, conv_id, trace_id, capability, llm) -> AsyncIterator[dict]:
        queue: asyncio.Queue = asyncio.Queue()

        async def emit(event: str, payload: Any) -> None:
            await queue.put({"event": event, "data": payload})

        deps = Deps(capability, llm, top_k=settings.top_k, emit=emit)
        # 用带 emit 的 deps 重建图（每请求一份，避免并发串台）
        request_graph = build_graph(deps, checkpointer=checkpointer)

        async def run() -> dict:
            return await request_graph.ainvoke(graph_input, config)

        task = asyncio.create_task(run())
        yield {"event": "meta", "data": json.dumps({"conv_id": conv_id, "trace_id": trace_id}, ensure_ascii=False)}

        while not task.done() or not queue.empty():
            try:
                item = await asyncio.wait_for(queue.get(), timeout=0.05)
            except asyncio.TimeoutError:
                continue
            except asyncio.CancelledError:
                task.cancel()
                raise
            yield {"event": item["event"], "data": json.dumps(item["data"], ensure_ascii=False)}

        try:
            result = await task
        except Exception as exc:  # noqa: BLE001 —— 编排层异常也必须给出可读回复
            yield {"event": "error", "data": json.dumps({"kind": "orchestrator_error", "message": str(exc)[:200]},
                                                       ensure_ascii=False)}
            yield {"event": "done", "data": json.dumps({"answer": "抱歉，服务暂时不可用，请稍后再试。",
                                                        "transfer": True, "sources": []}, ensure_ascii=False)}
            return

        payload = _final_payload(result, conv_id, trace_id)
        yield {"event": "done", "data": json.dumps(payload, ensure_ascii=False)}

    return app


def _final_payload(result: dict, conv_id: str, trace_id: str) -> dict:
    return {
        "conv_id": conv_id,
        "trace_id": trace_id,
        "answer": result.get("answer") or "",
        "category": result.get("category") or "other",
        "source": _source(result),
        "sources": result.get("sources") or [],
        "transfer": bool(result.get("transfer")),
        "transfer_path": result.get("transfer_path"),
        "support_ticket_no": result.get("support_ticket_no"),
        "citation_dropped": result.get("citation_dropped", 0),
        "boundary_injected": bool(result.get("boundary_injected")),
        "boundary_violation": bool(result.get("boundary_violation")),
        "degraded": result.get("degraded") or {},
        "vector_mode": result.get("vector_mode"),
        "stage_ms": result.get("stage_ms") or {},
        "tokens": result.get("tokens") or {},
    }


def _source(result: dict) -> str:
    if result.get("transfer"):
        return "transfer"
    if result.get("tool_result") is not None:
        return "tool"
    if result.get("cls_source") == "llm_arbitrate":
        return "llm"
    return "rule"
