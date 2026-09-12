# -*- coding: utf-8 -*-
"""M0-A 依赖 spike：langgraph + langgraph-checkpoint(4.x) + langgraph-checkpoint-mysql

验证三件事（对应技术设计文档 ADR-2 / §5.4 / §5.10.4）：
  1. AIOMySQLSaver.setup() 能否在 MySQL 8.4 上建表（以及真实表名 —— 文档此前是按官方
     postgres saver 同形推定的，这里实测确认）
  2. **跨进程** 断点恢复：进程 A 跑到 interrupt() 停下并退出；进程 B 用同一 thread_id
     `Command(resume=...)` 续跑并完成（这是 checkpoint 的实质，比"能存能读"更强）
  3. 实测「resume 会从节点开头重放 interrupt 之前的代码」这条坑（文档 §5.4 引的是官方说明，
     这里给出本机实测证据：反问节点入口的探针会被执行两次）

用法（在 orchestrator/ 下）:
    .venv/Scripts/python.exe spike/m0a_checkpointer.py dsn          # 打印将使用的 DSN（脱敏）
    .venv/Scripts/python.exe spike/m0a_checkpointer.py setup        # 建表 + 报告表名
    .venv/Scripts/python.exe spike/m0a_checkpointer.py start <tid>  # 跑到中断后退出（进程 1）
    .venv/Scripts/python.exe spike/m0a_checkpointer.py resume <tid> # 新进程续跑（进程 2）
    .venv/Scripts/python.exe spike/m0a_checkpointer.py probe <tid>  # 打印探针文件内容
"""
import asyncio
import os
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]      # tickets/
PROBE = Path(__file__).resolve().parent / "_m0a_probe.log"


def load_env_file(path: Path) -> dict:
    """极简 app.env 解析（KEY=VALUE，容忍引号与 export 前缀）——与项目约定一致。"""
    env = {}
    if not path.exists():
        return env
    for raw in path.read_text(encoding="utf-8", errors="replace").splitlines():
        line = raw.strip()
        if not line or line.startswith("#"):
            continue
        if line.lower().startswith("export "):
            line = line[7:]
        if "=" not in line:
            continue
        k, v = line.split("=", 1)
        env[k.strip()] = v.strip().strip('"').strip("'")
    return env


def build_dsn() -> str:
    cfg = load_env_file(ROOT / "app.env")
    host = cfg.get("DB_HOST", "localhost")
    port = cfg.get("DB_PORT", "3307")
    user = cfg.get("DB_USERNAME", "root")
    pwd = cfg.get("DB_PASSWORD", "")
    db = cfg.get("DB_DATABASE", "tickets")
    return f"mysql://{user}:{pwd}@{host}:{port}/{db}"


def mask(dsn: str) -> str:
    if "@" not in dsn:
        return dsn
    head, tail = dsn.split("@", 1)
    if ":" in head:
        scheme, _ = head.split(":", 1)
        return f"{scheme}:***@{tail}"
    return dsn


# ---------------- 图定义（最小复现：prepare → clarify(interrupt) → finalize） ----------------
def build_graph(saver):
    from langgraph.graph import END, START, StateGraph
    from langgraph.types import interrupt
    from typing import TypedDict

    class S(TypedDict, total=False):
        question: str
        choice: str
        answer: str

    def prepare(state: S):
        # 无副作用节点（真实图里这里是 load_context）
        return {"question": state.get("question", "")}

    def clarify(state: S):
        # 探针 1：interrupt 之前的副作用 —— resume 时本节点会从函数开头重放，
        # 所以这一行会被写两次（这正是 §5.4 要提醒的坑）
        with PROBE.open("a", encoding="utf-8") as f:
            f.write("clarify-node-enter\n")
        # 探针 2：interrupt 之后（resume 时才执行）
        payload = {"question": "您说的是哪一趟？", "options": ["A 次 10:30", "B 次 14:00"]}
        choice = interrupt(payload)
        with PROBE.open("a", encoding="utf-8") as f:
            f.write(f"clarify-node-after-interrupt:{choice}\n")
        return {"choice": str(choice)}

    def finalize(state: S):
        return {"answer": f"已确认选择：{state.get('choice')}"}

    g = StateGraph(S)
    g.add_node("prepare", prepare)
    g.add_node("clarify", clarify)
    g.add_node("finalize", finalize)
    g.add_edge(START, "prepare")
    g.add_edge("prepare", "clarify")
    g.add_edge("clarify", "finalize")
    g.add_edge("finalize", END)
    return g.compile(checkpointer=saver)


async def open_saver(dsn: str):
    """返回 (context manager, 说明)。AIOMySQLSaver.from_conn_string 在不同版本可能是
    同步/异步上下文管理器，这里都探一下，并把差异原样报告出来（spike 的目的就是暴露差异）。"""
    from langgraph.checkpoint.mysql.aio import AIOMySQLSaver

    cm = AIOMySQLSaver.from_conn_string(dsn)
    return cm


async def cmd_setup(dsn: str):
    from langgraph.checkpoint.mysql.aio import AIOMySQLSaver
    import aiomysql

    print(f"[setup] DSN = {mask(dsn)}")
    async with AIOMySQLSaver.from_conn_string(dsn) as saver:
        await saver.setup()
        print("[setup] saver.setup() OK")

    # 查真实表名（文档此前按官方 postgres 同形推定）
    import re
    m = re.match(r"mysql://([^:]+):([^@]*)@([^:]+):(\d+)/(.+)", dsn)
    user, pwd, host, port, db = m.group(1), m.group(2), m.group(3), int(m.group(4)), m.group(5)
    conn = await aiomysql.connect(host=host, port=port, user=user, password=pwd, db=db)
    async with conn.cursor() as cur:
        await cur.execute(
            "SELECT table_name FROM information_schema.tables "
            "WHERE table_schema=%s AND (table_name LIKE 'checkpoint%%') ORDER BY table_name",
            (db,),
        )
        rows = await cur.fetchall()
        print("[setup] MySQL 中实际的 checkpoint 表：")
        for (t,) in rows:
            print(f"          - {t}")
    conn.close()


async def cmd_start(dsn: str, thread_id: str):
    print(f"[start] thread_id={thread_id}，进程 PID={os.getpid()}")
    async with await open_saver(dsn) as saver:
        graph = build_graph(saver)
        config = {"configurable": {"thread_id": thread_id}}
        result = await graph.ainvoke({"question": "我要改签"}, config=config)
        interrupts = result.get("__interrupt__")
        print(f"[start] 已停下；__interrupt__ = {interrupts}")
        print(f"[start] 进程即将退出（不 resume）")


async def cmd_resume(dsn: str, thread_id: str):
    from langgraph.types import Command

    print(f"[resume] thread_id={thread_id}，**新的进程** PID={os.getpid()}")
    async with await open_saver(dsn) as saver:
        graph = build_graph(saver)
        config = {"configurable": {"thread_id": thread_id}}

        # 先确认能读到上个进程留下的状态
        snap = await graph.aget_state(config)
        print(f"[resume] 从 MySQL 读回状态：values={snap.values} next={snap.next}")
        if not snap.next:
            print("[resume] !! 未读到待恢复的下一步 —— checkpoint 恢复失败")
            return 1

        result = await graph.ainvoke(Command(resume="A 次 10:30"), config=config)
        print(f"[resume] 续跑完成：answer={result.get('answer')!r}")
        snap2 = await graph.aget_state(config)
        print(f"[resume] 最终状态：values={snap2.values} next={snap2.next}")
        ok = result.get("answer") == "已确认选择：A 次 10:30"
        print(f"[resume] 断言 answer 正确：{'PASS' if ok else 'FAIL'}")
        return 0 if ok else 1


def cmd_probe(_thread_id: str = ""):
    if not PROBE.exists():
        print("[probe] 探针文件不存在")
        return 0
    lines = PROBE.read_text(encoding="utf-8").split("\n")
    lines = [l for l in lines if l.strip()]
    print("[probe] 探针记录：")
    for l in lines:
        print(f"        {l}")
    enters = sum(1 for l in lines if l == "clarify-node-enter")
    print(f"[probe] clarify 节点入口被执行 {enters} 次 —— "
          f"{'说明 resume 重放了 interrupt 之前的代码（与官方说明一致）' if enters >= 2 else '未观察到重放'}")
    return 0


def main():
    if len(sys.argv) < 2:
        print(__doc__)
        return 2
    cmd = sys.argv[1]
    dsn = build_dsn()
    if cmd == "dsn":
        print(mask(dsn))
        return 0
    if cmd == "setup":
        asyncio.run(cmd_setup(dsn))
        return 0
    if cmd == "start":
        asyncio.run(cmd_start(dsn, sys.argv[2]))
        return 0
    if cmd == "resume":
        return asyncio.run(cmd_resume(dsn, sys.argv[2]))
    if cmd == "probe":
        return cmd_probe()
    print(f"未知命令: {cmd}")
    return 2


if __name__ == "__main__":
    sys.exit(main())
