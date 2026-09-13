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
from . import checkpoint as ckpt
from .graph import build_graph
from .nodes import Deps
from .settings import Settings
from .state import new_initial_state


def create_app(*, settings: Settings | None = None, capability: Any = None, llm: Any = None,
               checkpointer: Any = None) -> FastAPI:
    settings = settings or Settings.from_env()
    capability = capability or CapabilityClient(settings)
    llm = llm or GatewayLLM(settings)

    def _deps(emit=None) -> Deps:
        return Deps(capability, llm, top_k=settings.top_k,
                    rewrite_max=settings.rewrite_max, interrupt_max=settings.interrupt_max,
                    memory_recent_n=settings.memory_recent_n,
                    memory_summary_max=settings.memory_summary_max, emit=emit)

    graph = build_graph(_deps(), checkpointer=checkpointer)

    @contextlib.asynccontextmanager
    async def lifespan(app: FastAPI):
        # —— M3：checkpoint 接入（§5.5）——
        # 1) 建表**幂等**：先探测四张表是否齐，齐了就不调 setup()（M0 实测重复调会打告警）；
        # 2) 未显式注入 checkpointer 时按 CHECKPOINT_DSN 自己开（生产路径）；
        # 3) 开不起来就退化为无状态执行（等价单轮），**不阻塞启动**（§9.2 降级纪律）。
        saver_cm = None
        cleanup_task: asyncio.Task | None = None
        if checkpointer is not None:
            app.state.graph = build_graph(_deps(), checkpointer=checkpointer)
        elif settings.checkpoint_dsn:
            try:
                ran_setup, msg = await ckpt.ensure_setup(settings.checkpoint_dsn)
                print(f"[checkpoint] {msg}" + ("（刚执行了 setup）" if ran_setup else ""))
                cm = await ckpt.open_saver(settings.checkpoint_dsn)
                saver = await cm.__aenter__()
                app.state.graph = build_graph(_deps(), checkpointer=saver)
                app.state.checkpointer = saver
                saver_cm = cm
                print("[checkpoint] MySQL saver 已接入（thread_id = conv_id，跨进程 resume 可用）")
            except Exception as exc:  # noqa: BLE001
                print(f"[warn] checkpoint 接入失败，本轮退化为无状态执行: {type(exc).__name__}: {exc}")
        if settings.checkpoint_dsn and settings.checkpoint_cleanup_interval_s > 0:
            cleanup_task = asyncio.create_task(ckpt.cleanup_loop(
                settings.checkpoint_dsn, settings.checkpoint_retention_days,
                settings.checkpoint_cleanup_interval_s))
        yield
        if cleanup_task is not None:
            cleanup_task.cancel()
            with contextlib.suppress(asyncio.CancelledError):
                await cleanup_task
        if saver_cm is not None:
            with contextlib.suppress(Exception):
                await saver_cm.__aexit__(None, None, None)
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
        # checkpointer 要读**运行期真的装上了没有**（app.state），不能读构造参数：
        # 生产路径是在 lifespan 里按 DSN 打开的，构造参数恒为 None →
        # 旧写法会一边连着 MySQL 一边自报 "checkpointer": false（M3 实测踩过，属于假健康信号）
        live = getattr(app.state, "checkpointer", None)
        checks = {"capability": False, "checkpointer": live is not None, "graph": True}
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
        # SSE 是**显式选择**：默认返回 JSON（Go 的非流式分支不带 stream 字段，
        # 旧默认 True 会让它拿到 SSE 正文 → 解析失败 → 每轮静默降级成工单，M3 实测踩过）。
        raw_stream = body.get("stream", False)
        if isinstance(raw_stream, str):  # 容忍 "false"/"0" 这类字符串（curl 手测常见）
            raw_stream = raw_stream.strip().lower() in ("1", "true", "yes")
        want_stream = bool(raw_stream)
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
        # （M3 起这条规则是真的会被用到：消歧反问挂起后，用户下一条消息带同一 conv_id 进来）
        active_graph = app.state.graph
        graph_input: Any = initial
        snapshot = None
        with contextlib.suppress(Exception):
            snapshot = await active_graph.aget_state(config)
        if snapshot is not None and getattr(snapshot, "tasks", None):
            has_interrupt = any(getattr(t, "interrupts", None) for t in snapshot.tasks)
            if has_interrupt:
                from langgraph.types import Command

                graph_input = Command(resume={"text": question})

        if not want_stream:
            result = await active_graph.ainvoke(graph_input, config)
            return JSONResponse(_final_payload(result, conv_id, trace_id))

        return EventSourceResponse(_stream(app.state.graph, graph_input, config, conv_id, trace_id, capability, llm))

    async def _stream(graph_obj, graph_input, config, conv_id, trace_id, capability, llm) -> AsyncIterator[dict]:
        queue: asyncio.Queue = asyncio.Queue()

        async def emit(event: str, payload: Any) -> None:
            await queue.put({"event": event, "data": payload})

        deps = _deps(emit)
        # 用带 emit 的 deps 重建图（每请求一份，避免并发串台）；checkpointer 与启动时一致
        request_graph = build_graph(deps, checkpointer=app.state.checkpointer if checkpointer is None else checkpointer)

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
        # M3：图在 clarify 处暂停 → 先发 interrupt 事件（前端把反问当普通回复渲染），
        # 再发 done（带同一个 conv_id，用户回复时用它 resume）
        interrupts = result.get("__interrupt__") if isinstance(result, dict) else None
        if interrupts:
            payload["interrupt"] = [
                {"question": (getattr(i, "value", {}) or {}).get("question", ""),
                 "options": (getattr(i, "value", {}) or {}).get("options", [])}
                for i in interrupts
            ]
            payload["answer"] = _clarify_text(interrupts)
            payload["transfer"] = False
            yield {"event": "interrupt", "data": json.dumps(payload["interrupt"], ensure_ascii=False)}
        yield {"event": "done", "data": json.dumps(payload, ensure_ascii=False)}

    return app


def _clarify_text(interrupts: list) -> str:
    """把 interrupt 的 payload 渲染成一句可读的反问（前端可直接展示）。"""
    parts = []
    for i in interrupts:
        v = getattr(i, "value", {}) or {}
        q = v.get("question", "请补充一下具体信息")
        opts = v.get("options") or []
        if opts:
            lines = "\n".join(f"{n + 1}. {o}" for n, o in enumerate(opts))
            parts.append(f"{q}\n{lines}")
        else:
            parts.append(q)
    return "\n\n".join(parts)


def _final_payload(result: dict, conv_id: str, trace_id: str) -> dict:
    interrupts = result.get("__interrupt__")
    payload = {
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
        # M2：工具层可观测（验收与排障要看"走的是工具还是检索、工具判定是什么"）
        "tool_name": result.get("tool_name"),
        "tool_kind": result.get("tool_kind"),
        "tool_reason": (result.get("tool_result") or {}).get("reason"),
        # M3：多轮/缓存/改写可观测（验收要看"指代是否继承、第几次命中缓存、是否改写重跑过"）
        "cls_source": result.get("cls_source"),
        "coref_from": result.get("coref_from"),
        "cache_hit": bool(result.get("cache_hit")),
        "retry_count": int(result.get("retry_count") or 0),
        "recent_len": len(result.get("recent") or []),
        "summary_len": len(result.get("summary") or ""),
        "search_question": result.get("search_question"),
        "stage_ms": result.get("stage_ms") or {},
        "tokens": result.get("tokens") or {},
    }
    # M3：图停在消歧反问（E8）时，非流式也必须把反问交出去
    # —— 否则客户端拿到空 answer，既不显示问题也无法用 conv_id resume（M3 实测踩过）
    if interrupts:
        payload["interrupt"] = [{"question": (getattr(i, "value", {}) or {}).get("question", ""),
                                 "options": (getattr(i, "value", {}) or {}).get("options", [])}
                                for i in interrupts]
        if not payload["answer"]:
            payload["answer"] = _clarify_text(interrupts)
    return payload


def _source(result: dict) -> str:
    if result.get("transfer"):
        return "transfer"
    if result.get("cache_hit"):
        return "cache"  # M3：答案缓存命中（连检索带 LLM 一起跳过）
    if result.get("tool_result") is not None:
        return "tool"
    if result.get("cls_source") == "llm_arbitrate":
        return "llm"
    return "rule"
