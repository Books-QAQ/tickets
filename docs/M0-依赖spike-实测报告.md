# M0 依赖 spike · 实测报告

| 项目 | 内容 |
|---|---|
| 版本 | V1.0（对应技术设计文档 V1.0-r2 的 §17 M0） |
| 日期 | 2026-09-12 |
| 目的 | 判定 **ADR-2（checkpoint 存储选型）** 首选档是否成立、锁定依赖版本组合、实测 **ADR-10 / §7.7** 的 Qdrant 十万级检索预算 |
| 结论 | **两条链路全部通过，无需回退**；依赖组合已 pin；Qdrant 参数已定 |

---

## 1. 结论摘要

| # | 断言 | 结果 | 对文档的影响 |
|---|---|---|---|
| A1 | `langgraph 1.2.11 + langgraph-checkpoint 4.2.0 + langgraph-checkpoint-mysql 3.0.0` 可解析安装 | ✅ 通过（`uv sync` 无冲突） | §16.2 依赖清单按实测 pin |
| A2 | `AIOMySQLSaver.setup()` 在 MySQL 8.4.11 上建表 | ✅ 通过；真实表名 `checkpoints` / `checkpoint_blobs` / `checkpoint_writes` / `checkpoint_migrations` | **ADR-2 首选档（MySQL）成立 → 不回退档 B/C/D/E**；文档此前对这些表名的"推定"已被实测确认 |
| A3 | **跨进程** 断点恢复（`interrupt` → 新进程 `Command(resume=...)`） | ✅ 通过：进程1（PID 28924）中断退出 → 进程2（PID 38608）从 MySQL 读回 `next=('clarify',)` 并续跑出正确答案 | §5.10.4 的分派规则可落地；§5.5 checkpoint 治理按 MySQL 实施 |
| A4 | `interrupt` 之前的代码在 resume 时被重放（§5.4 引官方说明） | ✅ **实测确认**：节点入口探针写入 **2 次**，interrupt 之后的语句写 1 次 | §5.4「interrupt 之前副作用必须幂等」由"官方文档说"升级为"本机实测" |
| B1 | Qdrant 十万级（10⁵ × 1024 维）过滤式 ANN 延迟 | ✅ 实测完成（见 §4） | §7.7 预算表按实测校正（ANN 从"2~8ms"改为"6~14ms，典型 8ms"） |
| B2 | int8 标量量化前后对比 | ✅ 完成：**延迟基本持平，价值在内存（约 1/3）** | §16.1 固定 Qdrant 参数与量化决策 |
| B3 | 过滤 vs 调 `ef` 哪个更有效 | ✅ 完成，且**结论明确：过滤优先**（见 §4.5） | 反向支撑 §7.1 的两级检索设计；§16.1 固定 `ef` 取值 |

**一句话**：ADR-2 不需要任何回退档，MySQL checkpointer 在本项目环境（MySQL 8.4.11）上可用，包括跨进程恢复这一最硬的语义；Qdrant 在 10⁵ 规模下过滤式检索稳定在个位数~十几毫秒，**"先过滤"比"堆 ef"收益大一个数量级**。

---

## 2. 环境

| 项 | 值 |
|---|---|
| 主机 | Windows 10 (19045)，16.5 GB RAM |
| Shell | git-bash / MSYS |
| Python | 3.11.15（uv 管理的 cpython-3.11-windows-x86_64） |
| uv | 0.12.6 |
| Docker | Docker Desktop，server 29.7.2（**本次由 `Start-Process` 拉起**，见 §6 遗留） |
| MySQL | 容器 `mysql:8.4`，实际 **8.4.11**（≥ saver 要求的 8.0.19 ✅） |
| Qdrant | 容器 `qdrant/qdrant:latest`，实际 **1.19.1**；客户端 `qdrant-client 1.19.0` |

---

## 3. M0-A：Python 编排层依赖组合（checkpointer / interrupt / resume）

### 3.1 实测 pin 后的依赖（`orchestrator/uv.lock`）

| 包 | 版本 | 备注 |
|---|---|---|
| `langgraph` | **1.2.11** | 强制 `langgraph-checkpoint>=4.1.0,<5.0.0` |
| `langgraph-checkpoint` | **4.2.0** | 已切到 `ormsgpack` 序列化（1.12.2） |
| `langgraph-checkpoint-mysql` | **3.0.0** | 社区包（MIT），**上限开放**（`>=2.1.2`）→ 这是 M0 要验的版本互斥风险点 |
| `aiomysql` | 0.3.2 | 异步驱动 |
| `cryptography` | **50.0.1** | ⚠️ **文档原清单漏了它**，见 3.3 发现① |
| `langchain-core` | 1.6.3 | |
| `langchain-openai` | 1.6.2 | §5.10.1 说要用它接 Go 网关 |
| `qdrant-client` | 1.19.0 | |
| `numpy` | 2.4.6 | |
| `fastapi` / `uvicorn` / `sse-starlette` | 0.141.1 / 0.52.4 / 3.4.11 | M1 使用 |
| `ormsgpack` / `orjson` | 1.12.2 / 3.12.0 | 两个序列化器同时在场（4.x 用前者，saver 用后者） |

**判定**：`uv sync` 一次通过、无版本冲突 ⇒ **A1 通过**。但**安装成功 ≠ 接口兼容**，因此继续做了运行时验证（A2~A4）。

### 3.2 运行时验证（关键）

```
# 建表
[setup] saver.setup() OK
[setup] MySQL 中实际的 checkpoint 表：
          - checkpoint_blobs
          - checkpoint_migrations
          - checkpoint_writes
          - checkpoints

# 进程 1：跑到 interrupt 后退出
[start] thread_id=t1，进程 PID=28924
[start] 已停下；__interrupt__ = [Interrupt(value={'question': '您说的是哪一趟？',
         'options': ['A 次 10:30', 'B 次 14:00']}, id='0b78577f...')]
--- 进程 1 已退出 ---

# 进程 2：全新进程，从 MySQL 恢复并续跑
[resume] thread_id=t1，**新的进程** PID=38608
[resume] 从 MySQL 读回状态：values={'question': '我要改签'} next=('clarify',)
[resume] 续跑完成：answer='已确认选择：A 次 10:30'
[resume] 断言 answer 正确：PASS
```

**判定**：A2 ✅、A3 ✅。**ADR-2 首选档成立，不需要回退到 sqlite / PG / 自研 saver / 无 checkpointer。**

### 3.3 实测发现（3 条，均需回写文档/代码规范）

**① 缺 `cryptography` 直接连不上库（部署级坑）**

```
RuntimeError: 'cryptography' package is required for sha256_password or
              caching_sha2_password auth methods
```

- 原因：MySQL 8 默认认证插件 `caching_sha2_password`，在**非 TLS** 连接下需要 RSA 加密口令交换，`aiomysql`/`pymysql` 依赖 `cryptography` 实现该路径。
- 影响：容器内网直连（本项目形态）必然踩到；**这是"本地能连、容器连不上"这类典型故障的根因**。
- 处理：已加入 `pyproject.toml`（`cryptography>=42`），并回写 §16.2。
- 备选（未采用，记录理由）：改用户认证插件为 `mysql_native_password` —— MySQL 8.4 已默认不启用该插件，且属于改库配置而非改应用，更脆弱。

**② 文档对 checkpoint 表名的"推定"已实测确认** — 与官方 postgres saver 同形（`checkpoints` / `checkpoint_blobs` / `checkpoint_writes` / `checkpoint_migrations`）。ADR-2 中"schema 归属"的表述可直接落地：**这些表由 Python 侧 `setup()` 建，Go 侧迁移不碰**。

**③ `setup()` 重复执行会打印告警**

```
Warning: Table 'checkpoint_migrations' already exists
```

- 现象：第二次（及以上）执行 `setup()` 时出现，**不影响建表结果**。
- 含义：`setup()` 在"表已存在"时不是静默幂等的，而是靠 SQL 层忽略错误。
- 落地要求：启动流程中 `setup()` 放在**一次性的初始化任务**里，不要每次请求/每次启动都无脑调用；若必须在启动时调用，需容忍该告警（不要把它当致命错误）。

### 3.4 附带确认：int8 之外的两处"诚实性"

- `qdrant_client` 连接时会打印 `Failed to obtain server version ... check_compatibility=False`：客户端（1.19.0）与服务器（1.19.1）版本探测在 gRPC 下失败，**功能不受影响**。生产上建议显式 pin 客户端版本或设置 `check_compatibility=False` 以消除噪声。
- 探针文件 `spike/_m0a_probe.log` 是**测试产物**，不参与入库；`.gitignore` 需覆盖（见 §5）。

---

## 4. M0-B：Qdrant 十万级过滤式 ANN

### 4.1 数据与参数口径（可复现）

| 项 | 值 |
|---|---|
| 规模 | **100,000 点 × 1024 维**（BGE-M3 / Qwen embed-v3 量级） |
| 向量 | 随机生成后**单位化**（`float32`），距离 = COSINE |
| HNSW 建索引 | `m=16`，`ef_construct=100` |
| 检索参数 | `hnsw_ef` 显式固定（见 4.4 扫描），`top-k=50` |
| payload 索引 | `category` / `visibility` / `capability` / `scope_kind` / `scope_ref` / `form`（KEYWORD） |
| 过滤选择性 | 宽过滤（category=refund & visibility=public & capability=supported）≈ **2.3%**（≈2,300 候选）；窄过滤（scope_ref=station_7）≈ **0.1%**（≈100 候选） |
| 灌库 | 1000 点/批，**plain 118.2s（≈840 点/秒）**，int8 108.3s |
| 索引 | `indexed ≈ 98,000`，状态 green |

### 4.2 延迟（200 次查询，top-50）

| 场景 | plain P50 / P95 | int8(int8 标量量化) P50 / P95 |
|---|---|---|
| 无过滤 | 14.61 / 21.85 ms | 13.03 / 17.74 ms |
| 宽过滤（≈2.3% 选择性） | **9.06 / 13.25 ms** | 9.00 / 17.12 ms（max 364ms 离群） |
| 窄过滤（≈0.1% 选择性） | **6.36 / 8.46 ms** | 6.03 / 8.20 ms |

**观察**：
1. **过滤比不过滤更快**（9.06 vs 14.61 ms）——预过滤把候选集从 10⁵ 压到千级，HNSW 只需在小集合上搜。这与"过滤会更慢"的直觉相反，**直接支撑设计的"先过滤"策略**。
2. **int8 的延迟收益很小**（P50 基本持平），且**宽过滤下 P95 反而变差**（17.12ms，且出现 364ms 离群）。
3. 窄过滤（用户明确提到站点）只要 **6~8ms**，是最划算的一档。

### 4.3 内存

| 场景 | 容器内存 |
|---|---|
| 两个 collection 同时在（10⁵ plain + 10⁵ int8） | **1.016 GiB** |
| 推算：单个 10⁵ × 1024 plain collection | ≈ 0.6~0.7 GB |
| 推算：int8 化后 | ≈ 1/3（≈0.2~0.3 GB） |

⇒ **10⁵ 规模下内存是"要不要 int8"的真正决策变量**，不是延迟。

### 4.4 `ef` 召回/延迟曲线（用于固定参数）

以**精确检索（exact）为基准**，50 次查询，top-50：

| hnsw_ef | 无过滤 召回 / P50 | 过滤后 召回 / P50 |
|---|---|---|
| 64 | 23.9% / 9.05 ms | 82.2% / 7.44 ms |
| **128** | 37.4% / 11.19 ms | 83.8% / 7.94 ms |
| **256** | 55.9% / 13.40 ms | **87.1% / 8.12 ms** |
| 512 | 78.6% / 18.29 ms | **91.4% / 8.26 ms** |

**关键发现（本次 M0-B 最有价值的一条）**：

- **过滤条件下，把 `ef` 从 64 提到 512 几乎不要钱**（7.44 → 8.26 ms），召回却从 82.2% 升到 91.4%；
- **无过滤条件下，堆 `ef` 又贵又慢**（ef=512 才 78.6%，且 P50 已 18.29 ms，是过滤后的 2.2 倍）。
- ⇒ **"先把候选过滤到千级，再适度提高 ef"** 是唯一划算的组合；这同时决定了：**分类路由/scoped 检索/scope 过滤不是"优化"，而是检索质量与延迟的前提**。

### 4.5 局限性（必须诚实标注，避免误用这些数字）

1. **随机均匀向量是高维检索的"最难"数据**：维度高时向量近似两两正交，HNSW 图缺乏可利用的聚簇结构，因此**上表的"召回 84%/91%"是悲观下界，不能外推为真实知识库的表现**；反过来，**也不能用这批数据"证明"检索质量** —— 真实召回必须在真实 embedding + 真实问法上测（M1 的检索评测集，§15.1）。
2. 未测：并发 QPS / 多连接、P99 长尾（int8 已出现 364ms 离群，疑似后台优化与合并，生产需盯 P99 与 compaction）、真实 embedding 的过滤选择性分布、规模 >10⁵、多副本高可用。
3. `on_disk` 向量未测：本次内存够用（约 0.7GB/10⁵），留到数据量再上一个数量级时评估。
4. 造数据 bug 的教训（值得记）：第一版 `scope_kind` 与 `scope_ref` 用了**互相排斥的取模条件**，导致窄过滤命中 0 条 —— 看起来像"Qdrant 过滤不工作"，实际是数据问题。**排查检索异常时，先确认"过滤条件与数据分布是否有交集"，再怀疑引擎。**

---

## 5. 对主文档的回写（已执行）

| 章节 | 回写内容 |
|---|---|
| §16.2 依赖锁定 | 按实测 pin 具体版本；**补 `cryptography>=42`** 并注明原因 |
| ADR-2 | 增加"实测结论：首选档成立、无回退"；补 `cryptography`、`setup()` 告警、真实表名三条 |
| §16.1 部署 | Qdrant 固定参数：`m=16` / `ef_construct=100` / 检索 `hnsw_ef=256`（过滤下 87.1% 召回、P95 9.4ms）；**开启 int8 标量量化**（价值在内存） |
| §7.7 检索预算 | ANN 从"2~8ms"校正为"**6~14ms**（典型 8ms，窄过滤）"；补充"过滤优先于堆 ef"的实测依据 |
| §17 实施计划 | M0 标记完成，附本报告链接 |
| 附录A | 增补 `orchestrator/spike/` 三个脚本 |
| `.gitignore` | 增补 `orchestrator/.venv/`、`spike/_m0a_probe.log`、`spike/__pycache__/` |

---

## 6. 产物与遗留

**产物**

```
orchestrator/
├── pyproject.toml                     # 依赖（含 M0 发现的 cryptography）
├── uv.lock                            # 锁文件（497 KB，195+ 包）
└── spike/
    ├── m0a_checkpointer.py            # A：setup / start / resume / probe 四态
    ├── m0b_qdrant.py                  # B：建 → 灌 → 测（plain / int8）
    └── m0b_ef_sweep.py                # B：ef 召回-延迟曲线
docs/M0-依赖spike-实测报告.md           # 本报告
```

**环境现状（本次操作产生的副作用，如实列出）**

| 项 | 状态 | 恢复方式 |
|---|---|---|
| Docker Desktop | **本次由我拉起**（此前未运行） | 直接关闭即可 |
| MySQL 容器 | 为腾内存已 `stop`（M0-A 用完后） | `docker compose up -d mysql` |
| Qdrant 容器 `qdrant-m0` | 已 `stop`，**保留未删除**（便于复跑 M0-B） | `docker start qdrant-m0` / 彻底移除 `docker rm -f qdrant-m0` |
| `tickets` 库 | **新增 4 张 checkpoint 表**（由 saver 建） | 不影响业务表；如需回退：`DROP TABLE checkpoint_blobs, checkpoints, checkpoint_writes, checkpoint_migrations;` |
| 实测数据 | 两个 collection 已 drop（释放约 1GB 内存） | 重新生成：`python spike/m0b_qdrant.py plain` |

**未做（明确不属于 M0 边界）**：编排层服务实现（M1）、真实 embedding 的检索质量评测（M1）、Go 侧接口（M1）、并发压测（M3/M4）。
