#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""kb/ 入库 lint —— 零依赖（只用标准库）。

用法（仓库根目录）:
    python kb/_lint.py [kb_root]

校验项（依据技术设计文档 §7.2 / §7.9 / §5.9）:
  1. frontmatter 必填字段与取值域（category 7 类闭集 / form / visibility / capability）
  2. 形态规则:
     - qa   : 每个 "## 标准问" 下 >=2 条 "问：" + 非空 "答："
     - prose: 标题深度 <=3；chunk 内禁止指代词；单 chunk <=600 字符（rune）
  3. 与 capability.yaml 台账一致（supported 必须有 system_entry；roadmap 必须有 target）
  4. roadmap/industry 文档必须填 source
  5. scope_kind != general 时必须填 scope_ref
  6. id 全局唯一；单文件内 "## " 标题不重复

退出码: 0 = 全部通过；1 = 存在 FAIL。
"""
import re
import sys
from pathlib import Path

CATEGORIES = {"booking", "order", "refund", "account", "travel", "app", "policy"}
FORMS = {"qa", "prose"}
VISIBILITY = {"public", "authenticated"}
CAPABILITY = {"supported", "roadmap", "industry"}
SCOPE_KINDS = {"general", "station", "ticket_type", "route"}
REQUIRED = ["id", "title", "category", "sub_scenario", "form", "type", "visibility", "capability"]

# prose 块内禁止出现的指代词（脱离上下文不可独立理解）
REFERENTIAL = ["如上所述", "上述", "前述", "见第", "参见", "同上", "该站", "此站", "本规定第", "前款"]

PROSE_CHUNK_LIMIT = 600  # 字符（rune），不是字节


def parse_frontmatter(text):
    if not text.startswith("---"):
        return None, "文件未以 '---' 开头（缺少 frontmatter）"
    end = text.find("\n---", 3)
    if end == -1:
        return None, "frontmatter 未闭合（缺少结束的 '---'）"
    fm = {}
    for line in text[3:end].split("\n"):
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        if ":" not in line:
            continue
        k, v = line.split(":", 1)
        fm[k.strip()] = v.strip().strip('"').strip("'")
    return fm, None


def parse_capability_ledger(path):
    """解析 capability.yaml 的扁平子集（- sub_scenario: / 缩进 key: value）。"""
    ledger = {}
    cur = None
    if not path.exists():
        return None
    for raw in path.read_text(encoding="utf-8").split("\n"):
        line = raw.rstrip()
        if not line.strip() or line.strip().startswith("#"):
            continue
        m = re.match(r"^\s*-\s+sub_scenario:\s*(.+)$", line)
        if m:
            cur = m.group(1).strip().strip('"').strip("'")
            ledger[cur] = {}
            continue
        m = re.match(r"^\s+([a-z_]+):\s*(.*)$", line)
        if m and cur:
            k, v = m.group(1), m.group(2).strip().strip('"').strip("'")
            if k != "sub_scenario":
                ledger[cur][k] = v
    return ledger


def prose_chunks(body):
    """按 ## / ### 子标题切块，返回 [(标题路径, 正文)]。"""
    chunks, title, buf = [], None, []
    for line in body.split("\n"):
        if re.match(r"^#{1,3}\s+", line):
            if title is not None:
                chunks.append((title, "\n".join(buf)))
            title, buf = line.strip("# ").strip(), []
        else:
            buf.append(line)
    if title is not None:
        chunks.append((title, "\n".join(buf)))
    return chunks


def qa_sections(body):
    """按 ## 切 FAQ 知识点，返回 [(问题, 正文)]。"""
    return prose_chunks(body)


def lint_file(path, kb_root, ledger, seen_ids):
    text = path.read_text(encoding="utf-8")
    fails = []

    fm, err = parse_frontmatter(text)
    if err:
        return [err]

    for k in REQUIRED:
        if not fm.get(k):
            fails.append(f"frontmatter 缺必填字段: {k}")
    if fails:
        return fails

    rel = path.relative_to(kb_root).as_posix()

    # 取值域
    if fm["category"] not in CATEGORIES:
        fails.append(f"category 非法: {fm['category']}（只允许 {sorted(CATEGORIES)}）")
    if fm["form"] not in FORMS:
        fails.append(f"form 非法: {fm['form']}")
    if fm["visibility"] not in VISIBILITY:
        fails.append(f"visibility 非法: {fm['visibility']}")
    if fm["capability"] not in CAPABILITY:
        fails.append(f"capability 非法: {fm['capability']}")

    # scope
    if fm.get("scope_kind") and fm["scope_kind"] not in SCOPE_KINDS:
        fails.append(f"scope_kind 非法: {fm['scope_kind']}")
    if fm.get("scope_kind") and fm["scope_kind"] != "general" and not fm.get("scope_ref"):
        fails.append("scope_kind != general 但缺少 scope_ref")

    # 台账一致性
    entry = ledger.get(fm["sub_scenario"]) if ledger is not None else None
    if ledger is None:
        fails.append("找不到 kb/capability.yaml（无法校验能力台账）")
    elif entry is None:
        fails.append(f"sub_scenario 不在能力台账中: {fm['sub_scenario']}")
    else:
        st = entry.get("status", "")
        if fm["capability"] != st:
            fails.append(f"capability({fm['capability']}) 与台账 status({st}) 不一致")
        if st == "supported" and not entry.get("system_entry"):
            fails.append("台账 supported 项缺少 system_entry（说不出入口就不算支持）")
        if st == "roadmap" and not entry.get("target"):
            fails.append("台账 roadmap 项缺少 target（缺目标里程碑）")
    if fm["capability"] in ("roadmap", "industry") and not fm.get("source"):
        fails.append(f"capability={fm['capability']} 必须填 source（行业内容需可审计）")

    # id 唯一
    if fm["id"] in seen_ids:
        fails.append(f"id 重复: {fm['id']}（已在 {seen_ids[fm['id']]}）")
    else:
        seen_ids[fm["id"]] = rel

    # 正文（去掉 frontmatter）
    body = text[text.find("\n---", 3) + 4:]

    # 形态规则
    if fm["form"] == "qa":
        for title, chunk in qa_sections(body):
            asks = len(re.findall(r"^\s*问[:：]", chunk, re.M))
            has_ans = bool(re.search(r"^\s*答[:：]\s*\S", chunk, re.M))
            if asks < 2:
                fails.append(f"qa 块「{title}」只有 {asks} 条「问：」，要求 >=2（需要买家问法变体）")
            if not has_ans:
                fails.append(f"qa 块「{title}」缺少非空「答：」")
    else:
        for title, chunk in prose_chunks(body):
            depth = 0
            for line in body.split("\n"):
                if line.strip("# ").strip() == title:
                    depth = len(line) - len(line.lstrip("#"))
                    break
            if depth > 3:
                fails.append(f"prose 标题层级过深({depth}): {title}")
            for w in REFERENTIAL:
                if w in chunk:
                    fails.append(f"prose 块「{title}」含指代词「{w}」（chunk 必须自包含）")
            n = len(chunk.strip())
            if n > PROSE_CHUNK_LIMIT:
                fails.append(f"prose 块「{title}」长度 {n} > {PROSE_CHUNK_LIMIT} 字符")

    # 标题重复
    heads = re.findall(r"^##\s+(.+)$", body, re.M)
    for h in set(heads):
        if heads.count(h) > 1:
            fails.append(f"同一文件内「## {h}」标题重复")

    return fails


def main():
    kb_root = Path(sys.argv[1]) if len(sys.argv) > 1 else Path("kb")
    ledger = parse_capability_ledger(kb_root / "capability.yaml")

    if ledger is not None:
        print(f"能力台账: {len(ledger)} 个子场景")
        by_status = {}
        for v in ledger.values():
            by_status[v.get("status", "?")] = by_status.get(v.get("status", "?"), 0) + 1
        print("台账状态分布: " + ", ".join(f"{k}={v}" for k, v in sorted(by_status.items())))
    else:
        print("能力台账: 未找到 kb/capability.yaml")

    docs = [p for p in sorted(kb_root.rglob("*.md")) if p.name != "README.md"]
    seen_ids = {}
    failed = 0
    for p in docs:
        fails = lint_file(p, kb_root, ledger, seen_ids)
        rel = p.relative_to(kb_root).as_posix()
        if fails:
            failed += 1
            print(f"[FAIL] {rel}")
            for f in fails:
                print(f"       - {f}")
        else:
            print(f"[OK]   {rel}")

    print(f"\n合计 {len(docs)} 篇，通过 {len(docs) - failed}，失败 {failed}")
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
