# 智能AI客服系统 · 技术设计文档（tickets · V1.0）

## 0. 文档信息

| 项目 | 内容 |
|---|---|
| 文档名称 | 智能AI客服系统 - 技术设计文档 |
| 版本 | V1.0-r4（双运行时 + 十万级检索 + 工具层/会话层落地回写） |
| 日期 | 2026-09-13 |
| 状态 | 设计稿，待评审；**M0 依赖 spike 已完成**（2026-09-12，见 `docs/M0-依赖spike-实测报告.md`）；**M1/M2/M3 已实现并端到端验收**（见 `docs/M1-实现报告.md`、`docs/M2-实现报告.md`、`docs/M3-实现报告.md`） |
| 目标系统 | `D:\golangproject\tickets`（Go 客运票务后端） |
| 参考文档 | 《智能AI客服系统-产品需求文档（初版V1.0）》（电商多商户版本） |
| 变更记录 | **V1.0-r2**：知识库确定为「逐站点/逐票种实例化」⇒ 目标规模 **10⁵ chunk**。检索基础设施相应改为 **自研 BM25（升级真倒排 + 剪枝）+ Qdrant ANN + rerank**；明确 **MySQL 存文本与元数据（真相源）、Qdrant 存向量与检索快照（可重建派生索引）**。受影响章节：新增 ADR-10，改写 §7、§13.1、§16.1，增补 §14.2、§17、§18、§19、附录A<br/>**V1.0-r3（M2 落地回写）**：工具层按实现口径补齐 §8.6（提槽跨语言往返、**同分用分类破平**、**时间比较下沉 SQL**、`kind` 六分类 + `path_hint`、建单走注册表、19.2/19.3 两道闸门的默认值与翻转方式）；§17 的 M2 行标记完成<br/>**V1.0-r4（M3 落地回写）**：新增 §9.5（会话/记忆/缓存/SSE 的 8 条落地口径：图内缓存节点、命中仍过 verify、记忆不入初始输入、改写机检、确定性拒绝不计熔断、**SSE 出口由 Go 规范化（CRLF→LF + 转发空行）**、降级路径不许崩、`PYTHON_BASE_URL` 未配要报 ERROR）；§17 的 M3 行标记完成 |
| 文档定位 | **实现依据**。描述"在 tickets 现有代码上如何落地智能客服"，含架构决策、模块设计、跨语言契约、数据模型、验收口径、实施分期 |

### 0.1 架构一句话

**Go 平台层**（对外接入 + 确定性能力 + LLM 网关）+ **Python 应用编排层**（LangGraph Agent 图）。**Agent 的编排、状态、路由决策、断点与人工介入由 Python + LangChain/LangGraph 主导**；确定性能力（检索、工具执行、脱敏、存储、埋点）与横切基础设施留在 Go。

分层的依据是**职责**而非语言偏好：**确定性留在 Go（可评测、可复现），编排与语义决策放 Python（生态成熟、迭代快）**。

### 0.2 本设计与参考 PRD 的关系

参考 PRD 描述的是**多商户电商平台**的客服系统。本次落地的 tickets 客服**只有一个官方客服、一层知识库、一个票务业务域**——所以本文档**保留**其双运行时架构、混合检索、引用校验、四条转人工路径、held-out 评测纪律，**删除**多租户、渠道抽象、双层知识库，并把编排层按 tickets 的实际技术栈（MySQL 而非 PG、无向量库、无坐席工作台）重新落地。逐条对照见 [附录B](#附录b与参考-prd-的条目级对照)。

### 0.3 术语表

| 术语 | 含义 |
|---|---|
| 平台层 / 能力层 | Go 实现：接入、确定性能（分类打分、检索、工具执行、存储、引用校验）、LLM 网关 |
| 编排层 | Python + LangGraph 实现的 Agent 图执行层：状态、条件边、循环、checkpoint、中断与恢复 |
| checkpoint | 图的执行状态快照（每 superstep 落库），是图状态的**唯一真相源** |
| thread_id | checkpoint 的寻址游标，本项目 = `conv_id`（UUID） |
| 分类路由 | 「规则关键词权重（Go） → 置信度阈值 → LLM 兜底 → 跨类仲裁（Python）」四级链路，固定 4 类 |
| 混合检索 | BM25（中文 bigram）与向量语义召回并行，RRF 融合（Go 侧） |
| 相关性阈值 | top-1 chunk 与 query 的**余弦相似度**门槛，低于则不作答、转人工（判定在图内） |
| 引用校验 | 模型声称的出处须真实存在于本轮检索结果，否则整条剥除并计数（Go 侧） |
| 工具直答 | 事实类查询由工具结果直接组装作答，**不经 LLM 改写** |
| 转人工四路径 | 确定性 / 阈值不过 / 模型标记 / 工具不可用，分别计数 |
| held-out 评测集 | 与调优集严格去重的独立评测集；本文档不填未实测的数字 |
| 书写形态 `form` | `qa`（FAQ 式：标准问 + 买家问法 + 答案）/ `prose`（段落式：层级标题 + 条款叙述）——决定切块、阈值标定、prompt 与出处格式 |
| 子场景 `sub_scenario` | 分类之下的业务细分（如「候补购票」「遗失物品」），**不参与分类路由**，只用于目录组织、检索过滤与运营统计 |

---

## 1. 结论摘要

### 1.1 系统定位

在 tickets 上新增一套**双运行时**智能客服：**Go 保持唯一公网入口与全部确定性能**，**Python 服务（FastAPI + LangGraph）作为内网编排层**，以内部契约通信、`trace_id` 全链路透传；编排层不可用时，Go 降级为**转人工 + 标准话术**，接口不中断。

### 1.2 关键决策

| # | 决策 | 一句话理由 | 详见 |
|---|---|---|---|
| D1 | **双运行时**：Go 平台层 + Python/LangGraph 编排层 | Agent 需要条件边、循环、断点恢复、中断与恢复，Go 生态无对等物；编排与 prompt 要长期快速迭代 | [ADR-1](#adr-1双运行时go-平台层--python-编排层) |
| D2 | checkpoint 落 **MySQL**（`langgraph-checkpoint-mysql`），不新增 PG | 复用现有 MySQL 实例；**但存在版本互斥风险 → M0 spike 必做**，并备好四档回退 | [ADR-2](#adr-2checkpoint-存储选型--mysql-含版本互斥风险) |
| D3 | **确定性在 Go、编排决策在 Python** | 检索/工具/脱敏/计数必须可复现；路由与失败分支必须能快速改图 | [ADR-3](#adr-3职责边界确定性在-go编排决策在-python) |
| D4 | **Go 是唯一公网入口**，Python 仅内网可达 | 复用现有 JWT/限流/静态页；内部能力面不暴露公网 | [ADR-4](#adr-4对外入口唯一--go-是唯一公网入口) |
| D5 | 一切 LLM 调用经 **Go LLM 网关**（OpenAI 兼容端点） | 计费/配额/限流/缓存统一在一处；Python 用标准客户端接入，横切能力对编排层透明 | [ADR-5](#adr-5llm-统一经-go-网关openai-兼容端点) |
| D6 | **知识库单层**，四维 = 领域 × 类型 × 时效 × **身份可见性** | 只有官方客服，无商户维度；单租户下真正的隔离维度是"游客可见 / 需登录" | [ADR-6](#adr-6知识库--单层四维去掉商户维度) |
| D7 | 命名 `cs_*` / `support_tickets` | `tickets`（车票）、`sessions`（JWT）已被占用 | [ADR-7](#adr-7命名冲突处理) |
| D8 | **AI 只读**，不执行退票/改签/支付 | 概率组件不驱动资金变更 | [ADR-8](#adr-8ai-工具只读资金类写操作不由-ai-执行) |
| D9 | 图状态唯一真相源 = checkpoint，**Redis 不存会话状态** | 避免双写不一致；Redis 只做答案缓存、限流、座位预占 | [ADR-9](#adr-9图状态唯一真相源--checkpointredis-不存会话状态) |
| D10 | **检索基础设施按 10⁵ 规模设计**：自研 BM25（真倒排 + 剪枝）+ **Qdrant ANN** + **rerank** | 知识库按站点/票种实例化 ⇒ 目标 10⁵ chunk；实测进程内全量余弦在 2×10⁵ 条时 **295ms/次**，吃掉整个检索预算 | [ADR-10](#adr-10检索基础设施自研-bm25--qdrant-ann--rerank) |
| D11 | **MySQL 存文本与元数据（真相源）、Qdrant 存向量与检索快照（可重建派生索引）** | 10⁵×1024 维 float32 ≈ 400MB，不该进 BLOB；重建输入 = 文本 + 当前 embedding 模型 | [ADR-10](#adr-10检索基础设施自研-bm25--qdrant-ann--rerank) |
| D12 | 分类闭集 **4 类 → 7 类**，并引入 `sub_scenario` 子场景（41 项，**不参与路由**，只用于目录/过滤/统计） | 10⁵ 级下分类作为「预过滤键」收益变大；但类别扩张会摊薄评测，故子场景不进路由 | [§5.8](#58-分类路由与场景分类7-类闭集) |
| D13 | 知识库承认**两种书写形态**：`form: qa`（FAQ 式）与 `form: prose`（段落式），**切块与阈值分形态处理** | 站点规则/政策条款是段落式，且是 10⁵ 里的规模主体；只用 Q-A 规范等于主体内容没有入库规范 | [§7.2](#72-文档规范两种形态) |
| D14 | 知识库定位为**铁路客运行业知识库**（覆盖完整业务域，独立于代码；平台未实现的功能照写行业规则） | 41 子场景中 22 项平台当前无功能；先攒业务知识、再给 tickets 补功能（路线见 7.9） | [§7.8](#78-场景分类总览7-类--子场景--形态--系统支撑)、[§7.9](#79-能力台账与功能演进路线b-方案的配套) |
| D15 | **能力边界靠机制兜住**：chunk 带 `capability` + 检索优先 supported + prompt 物理分区 + **后处理拦截操作性措辞** | 行业知识讲成本平台能力＝把幻觉写进权威文档，用户照做会找不到入口 | [§5.9](#59-能力边界声明b-方案的核心防线) |

### 1.3 交付分期

| 期 | 内容 | 验收要点 |
|---|---|---|
| **M0** | **依赖 spike**：Python 编排层选型组合实测（launcher/checkpointer/interrupt） | 组合可跑通 interrupt→resume 冒烟；版本 pin 落到 `pyproject.toml`/`uv.lock` |
| M1 | 跨语言契约 + 图骨架 + **知识库入库与检索（Qdrant 集成、BM25 倒排升级、rerank）** + 生成 + 转人工建单 | 冷启动可答票务 FAQ；无依据必转人工；引用剥除可计数；**批量入库可断点续跑**（中断后重跑不重复嵌入）；**游客检索不到 authenticated chunk**（过滤安全边界测试） |
| M2 | 工具层（订单/车票/余票/退票费）+ 身份与越权矩阵 | 越权矩阵全通过；退票费与人工核对一致 |
| M3 | 会话与 checkpoint 治理 + 答案缓存 + SSE 全链路流式 + H5 入口 | 多轮指代消解可用；同义问题二次命中缓存；流式首字可见 |
| M4 | 可观测 + 限流 + 三套评测集 + 密钥治理 | 大盘口径与日志一致；三档评测数字出数；`app.env` 出库并轮换 |

---

## 2. 项目现状盘点

### 2.1 现有技术栈与可复用资产

| 层 | 现状（含证据） | 客服侧如何使用 |
|---|---|---|
| Web 框架 | Fiber v2，路由集中注册在 `internal/routes/web.go` | 新增 `/cs/*`（对外）与 `/internal/*`（内网）两组路由 |
| 鉴权 | JWT（`internal/token/`）；`internal/api/middleware/authMiddleware.go:37` 注入 `authorizationPayloadKey`；handler 经 `internal/api/handlers/ticket.go:108` `currentUser` 由 `payload.Username` 反查用户 | **身份只在 Go 解析**，解析结果作为可信参数传给编排层 |
| 数据库 | MySQL 8 + sqlc（`internal/db/query/*.sql` → `internal/db/sqlc/`）+ golang-migrate（`file://internal/db/migration`，现有 000001~000004） | 业务表 + `cs_*`（Go 迁移管理）；checkpoint 表由 Python 侧 `setup()` 创建（见 ADR-2） |
| 向量检索 | **本项目新增**：Qdrant（ANN 索引服务，仅容器内网） | 承载 10⁵ 级 chunk 的向量召回；chunk 文本在 MySQL 是重建输入（ADR-10） |
| 缓存/限流 | `internal/cache/redis.go`：`GetJSON`/`SetJSON`/`DeleteKey`、`AcquireSeatHold`、`AcquireCacheRebuildLock`、`AllowFixedWindow`(:149) | 答案缓存、AI 链路限流、座位预占 |
| 异步 | `internal/queue/rabbitmq.go`（DLX+TTL 延迟队列）+ `internal/worker/` | V1 非必需；可用于工单通知（可选） |
| 支付 | `internal/payment/provider.go:26` `Provider`（CreatePayment/QueryOrder/VerifyNotify/Refund） | 退款进度工具可读渠道；**渠道侧无退款查询接口**（见 8.4） |
| 日志 | zerolog（`main.go:34`） | 加 `trace_id`/`category`/`stage_ms` 字段 |
| 配置 | viper + `app.env`（`internal/util/config.go`） | 新增 `CS_*` / `LLM_*` / `EMBEDDING_*` / `ORCH_*` 键 |
| 文档 | `docs/business-flowcharts.md`（Mermaid） | 本文档沿用 Mermaid |

### 2.2 业务数据模型（工具层的真实数据源）

| 表 | 关键列 | 客服用途 |
|---|---|---|
| `orders`（`000004_add_orders.up.sql`） | `order_no`(UUID) / `user_id` / `bus_id` / `seat_id` / `amount` / `status` ∈ {pending, paid, canceled, refunded} / `pay_channel` / `paid_at` / `expired_at` | 订单查询、待支付查询、支付倒计时、退款进度 |
| `tickets` | `user_id` / `bus_id` / `seat_reservation_id` / `status` | 我的车票 |
| `seat_reservations` / `bus_seats` | `status` ∈ {available, reserved, purchased, maintenance, broken}；`seat_number` | 车票座位、余票 |
| `buses` / `routes` / `terminals` / `cities` | `departure_time` / `arrival_time` / `price` / `bus_type` / `service_number` / `is_vip` / `sale_open_at`（`000003:2`）/ `duration` / `distance` | 班次、票价、开售时间、行程时刻 |
| `penalties`（`internal/db/query/tickets.sql:34` `GetBusPenalties`） | `bus_id` / `hours_before` / `actual_hours_before` / `percent` / `custom_text` | **退票手续费规则**——确定性计算的素材（语义待确认，见 19.2） |
| `users` | `username` / `full_name` / `hashed_password` | 身份锚定；**无手机号/证件号/地址字段**（见 14.5） |

> 余票是**动态算的**（`bus_seats.status='available'` 聚合），不是 `buses` 上的冗余字段。

### 2.3 缺口清单

| 缺口 | 说明 |
|---|---|
| 无任何 LLM / 知识库 / 向量代码 | 全仓 `llm|chat|embedding|knowledge` 检索无命中（仅支付渠道名 `wechat`） |
| 无指标端点 / 无 trace_id | zerolog 只有零散 error 日志 |
| 无流式接口 | 现有接口均一次性 JSON；SSE 需新建（含 Go 侧透传） |
| 无内网能力面 | `/internal/*` 与内部密钥需新建 |
| 无第二运行时 | Python 服务、依赖锁定、容器编排需新建 |
| 无 ANN 检索基础设施 | 进程内全量扫描在 10⁵ 级不可用（实测 2×10⁵ 条 ≈ 295ms/次），需 Qdrant + 召回后 rerank |
| 无知识库实例化维度与治理 | 站点/票种实例化带来 10⁵ 级条目：需要维度建模、批量入库（断点/限速）、索引重建与对账 |

### 2.4 命名冲突预警

| 冲突名 | 已被谁占用 | 客服域采用 |
|---|---|---|
| `tickets` | 车票表（`000001_init_schema.up.sql:72`） | 客服工单 → **`support_tickets`** |
| `sessions` | JWT 登录会话表（`000001:96`） | 客服会话 → **`cs_conversations`** / **`cs_messages`**，Redis 键前缀 `cs:` |
| `/tickets/:id` | 已有退票接口（`internal/routes/web.go:56`） | 客服接口走 `/cs/*`；内网能力走 `/internal/*` |
| checkpoint 表（`checkpoints` / `checkpoint_blobs` / `checkpoint_writes` / `checkpoint_migrations`） | 由 Python saver 的 `setup()` 创建于同一 MySQL 库 | **禁止 Go 侧迁移去建/改这些表**；schema 归属单一来源（见 ADR-2） |

---

## 3. 架构决策记录（ADR）

### ADR-1：双运行时（Go 平台层 + Python 编排层）

**决策**：Agent 编排层由 **Python + LangChain/LangGraph** 主导；Go 保留接入与确定性能力。

**理由（机制级）**：

1. **本场景确实需要图能力。** 需要的能力清单：条件边路由（意图 → 分类 → 工具/检索 → 生成 → 转人工）、**受控循环**（检索空 → 改写重试，带轮次上限）、**断点恢复**（多轮消歧反问要跨轮续跑）、**中断与恢复**（人工介入挂起）。这些在 LangGraph 里是原生构件：`StateGraph` + `add_conditional_edges` + `checkpointer` + `interrupt()` / `Command(resume=...)`。
2. **Go 生态无对等物**（已核对）：`langgraphgo` 自 2024-03 停更、单文件 141 行、**无条件边、无循环退出条件、无 checkpoint**，state 写死消息列表；`langchaingo` v0.1.14 全模块**无任何关键词检索（BM25/TF-IDF）、无图编排**，`retrievers/` 包不存在。⇒ "在 Go 里引框架做编排"这条路不存在。
3. **迭代效率是本项目的核心诉求**：图结构与 prompt 会长期频繁调整（规则词表、阈值、分支优先级、话术）。Python 侧改图 + 热重载是分钟级；Go 侧要重编译、重启。
4. **分层收益**：Go 承接高并发接入与 CPU 密集确定性能力（自研混合检索、工具直查、脱敏），Python 按会话量水平扩容，两层各自按负载特征伸缩。

**被否方案**：
- ❌ **单运行时 Go 自研管线**：能跑通（直线管线 + 手写分支），但需要"跨轮断点续跑/中断恢复"时要自己在 MySQL 里发明 checkpoint 语义；且每条分支改动都要重编译，迭代慢。**本方案的确定性部分与单运行时版完全一致，本方案只是把"决策与状态"交给了更合适的运行时。**
- ❌ 双运行时但编排用 Go（自研状态机 + 手写断点）：等于自己实现一个残缺的 LangGraph。
- ❌ 不用图、用"每轮无状态"的管线：多轮消歧反问退化为主链路外的特例分支，越写越乱。

**后果 / 纪律**：
- 引入第二运行时的代价是**跨语言契约**（内部密钥、超时、降级、trace 透传、流式转发），必须一次性定义清楚（见 §6）。
- **编排层不可用 ⇒ Go 必须能独立服务**（降级为转人工 + 标准话术），这条写进验收。
- 能力层接口化（§5.7），未来换编排框架/多套编排复用能力层都不改 Go。

---

### ADR-2：checkpoint 存储选型 = MySQL（含版本互斥风险）

> **M0 实测结论（2026-09-12）：首选档成立，无需回退到 sqlite / PG / 自研 saver / 无 checkpointer。** `langgraph 1.2.11 + langgraph-checkpoint 4.2.0 + langgraph-checkpoint-mysql 3.0.0` 在 MySQL 8.4.11 上建表成功，且**跨进程** `interrupt` → `Command(resume=...)` 续跑通过。原始输出与四条发现见 `docs/M0-依赖spike-实测报告.md`。

**决策**：checkpoint 落**现有 MySQL 实例**，使用社区包 `langgraph-checkpoint-mysql`（MIT，`AIOMySQLSaver`，async）；**M0 必须先做依赖 spike 验证版本组合**。

**事实核对（2026-09-12 实测 PyPI）**：

| 包 | 最新版 | 依赖窗口 | 结论 |
|---|---|---|---|
| `langgraph` | 1.2.11（2026-08-11） | `langgraph-checkpoint >=4.1.0, <5.0.0` | 强制 checkpoint 4.x |
| `langgraph-checkpoint` | 4.2.0（2026-08-07） | 依赖 `ormsgpack>=1.12.0`（新序列化时代） | 4.x 与 2.x/3.x 非同代 |
| `langgraph-checkpoint-mysql` | 3.0.0（2026-01-23） | `langgraph-checkpoint >=2.1.2`（**上限开放**） | 发版早于 checkpoint 4.x 7 个月；依赖 `orjson` 而非 `ormsgpack` |
| `langgraph-checkpoint-postgres` | 3.1.2（2026-08-07） | `langgraph-checkpoint >=4.1.0, <5.0.0` | 官方，与 4.x 同步维护 |
| `langgraph-checkpoint-sqlite` | 3.1.1 | `langgraph-checkpoint >=4.1.0, <5.0.0`；另需 `sqlite-vec` | 官方，与 4.x 同步维护 |

**⇒ 风险点**：`langgraph 1.2.11` 会解析出 `checkpoint 4.2.0`，而 MySQL saver 是按 2.x 世代写的（克隆官方 postgres saver 的代码结构）。**pip 能装上 ≠ 接口兼容**——这是必须实测的原因。

**MySQL saver 已知的硬约束（来自其 README，实测背书）**：
- 需要 **MySQL ≥ 8.0.19**（用了生成列 + `MD5`/`UNHEX`）；现行 `docker-compose.yml` 是 `mysql:8.4` ✅
- **MySQL ≥ 9.6.0 移除了生成列中的 `MD5` 使用，该 saver 尚无迁移路径** ⇒ **不要升 MySQL 9.x**（写进部署约束）
- 首次使用必须调 `checkpointer.setup()` 建表；手工传连接时必须 `autocommit=True`（否则 `setup()` 建表不落库）
- 驱动：`PyMySQLSaver`(sync) / `AIOMySQLSaver`(aiomysql) / `AsyncMySaver`(asyncmy)；本项目选 **`AIOMySQLSaver` + `[aiomysql]`**

**M0 spike 要验的三件事**（半天，产出写回本文档）：
1. 装 `langgraph==1.2.11 + langgraph-checkpoint==4.2.0 + langgraph-checkpoint-mysql==3.0.0[aiomysql]`，`setup()` 建表成功；
2. **`interrupt()` → `Command(resume=...)` 跨进程重启后仍能恢复**（这是 checkpoint 的实质，比"能存能读"更强）；
3. 若失败：退到回退链，并记录失败现象（异常栈/接口签名不符点）。

**回退链（按优先级）**：

| 档 | 方案 | 代价 |
|---|---|---|
| B | `langgraph-checkpoint-sqlite 3.1.1` + `sqlite-vec` | 单写者；容器多副本需改单实例 + 可写卷；官方且与 4.x 同代 |
| C | `langgraph-checkpoint-postgres 3.1.2`，新增 PG 容器**只放 checkpoint** | 多一个存储类型与容器（但官方维护、最稳） |
| D | 自研 MySQL saver（实现 `BaseCheckpointSaver`：`aput`/`aget_tuple`/`aput_writes`/`alist`/`adelete_thread`/`get_next_version`） | 约 300 行，完全可控，但自担正确性 |
| E | **去 checkpointer**：图改无状态，会话状态由 Go 持有（`cs_conversations` + Redis） | 损失 `interrupt`/断点续跑 ⇒ 消歧反问退回"Go 侧 pending question"手写实现 |

**schema 归属（避免双写与版本漂移）**：
- checkpoint 相关表由 **Python 侧 `setup()`** 创建，Go 侧迁移**不管**这些表；
- 业务表（`cs_*` / `kb_*` / `support_tickets`）由 **Go 侧 golang-migrate** 创建，Python 侧不建表；
- 两侧都说清自己的表清单，写进 README，避免"谁建的、能不能改"扯皮。

---

### ADR-3：职责边界——确定性在 Go，编排决策在 Python

**决策**：按下表切分，边界之外不越界。

| 能力 | 归属 | 为什么必须在这一侧 |
|---|---|---|
| JWT 校验、身份解析、`user_id` 注入 | **Go** | 身份是信任根：**客户端传的任何用户标识都不采信**；编排层收到的 `user_id` 只能来自 Go |
| 限流（身份桶 + IP 桶）、请求预算入口 | **Go** | 成本保护必须在最外层拦；编排层被绕过时也要有效 |
| 答案缓存**查询/写入**（向量余弦匹配） | **Go** | 与 embedding 客户端、Redis 同侧；确定性匹配逻辑 |
| 规则分类器（关键词权重打分） | **Go** | 零 LLM、可单测、可评测；返回 `scores/top1/top2/gap` 由 Python 决策是否升级 |
| 混合检索（BM25 + 向量 + RRF + scoped/软路由） | **Go** | 确定性 + CPU 密集 + 与 KB 索引同进程 |
| 工具执行（提槽正则、数据源、脱敏、超时熔断） | **Go** | 出口脱敏与审计是安全边界；数据访问只能在一个地方 |
| 引用校验（声称出处是否在本轮结果） | **Go** | 纯集合判定，计数集中一处 |
| 工单状态机（建单/状态变更/条件更新） | **Go** | 与业务表同事务边界；幂等靠 DB 条件更新 |
| 对话流水（`cs_messages`）、指标计数、审计 | **Go** | 单一计数点，口径一致（避免两层各算一套） |
| **图结构、条件边、循环与重试上限** | **Python** | 编排即产品；改动要快（热重载） |
| **意图判定与失败分支选择**（是否升级 LLM、是否重试、是否转人工） | **Python** | 决策而非能力；调用 Go 拿事实、自己下判断 |
| **分类的 LLM 兜底与跨类仲裁** | **Python** | 需要 LLM + prompt 迭代；规则结果由 Go 提供 |
| **工具选择的 LLM 兜底**（规则漏判时"调哪个"） | **Python** | 决策；参数仍由 Go 的确定性提槽填 |
| **指代消解 / 问题改写** | **Python** | 语义决策，依赖会话上下文 |
| **答案生成的编排**（prompt 组装、流式、约束） | **Python** | prompt 高频迭代；LLM 经 Go 网关调用 |
| **checkpoint 状态与断点、interrupt/resume** | **Python** | 图状态归图引擎 |

**两条反例（写下来防后人反向"优化"）**：
- ❌ 把 JWT 校验搬到 Python：会造出"两个身份真相源"，且内部服务面对公网信任问题。
- ❌ 把工具执行搬到 Python：脱敏、审计、熔断就会散到两个进程，越权风险与口径不一致随之而来。

---

### ADR-4：对外入口唯一 = Go 是唯一公网入口

**决策**：浏览器只访问 Go 服务（现有 `:8080`）。Python 服务**只监听容器内网**，`docker-compose` **不映射宿主机端口**，并通过 `X-Internal-Key` 校验内网调用。

**理由**：现有 JWT、限流、静态页（`web/`）、SSE 出口全在 Go 侧；Python 只作为编排引擎被 Go 调用；内部能力面（`/internal/*`）与编排面都不出现在公网。

**后果**：所有对外接口实现都在 Go（含 SSE 透传）；Python 侧接口是"内网机器对机器"，不做用户级鉴权，只做内部密钥 + trace 透传。

---

### ADR-5：LLM 统一经 Go 网关（OpenAI 兼容端点）

**决策**：Go 暴露 `POST /internal/v1/chat/completions`（OpenAI 兼容）；Python 侧用标准 `openai` 客户端指向它。**编排层不直连任何模型供应商**。

**理由**：
1. **横切能力一处实现**：多模型渠道路由、配额与计费、限流、token 记账、超时与重试策略全部在 Go；Python 侧换模型只改配置。
2. **成本可观测**：token 用量由网关统一记账（`usage_unknown` 单独计数，**不用 0 冒充**），避免两层各记一套对不上。
3. **密钥单点**：模型密钥只存在于 Go 侧配置，Python 容器不需要任何模型密钥。
4. **可替换**：未来真要做成独立网关服务，只是把这段代码从本进程搬出去，端点和客户端都不改。

**被否方案**：Python 直连供应商（密钥分散两处、token 记账重复、限流绕过）。

---

### ADR-6：知识库 = 单层七维（去掉商户维度）

**决策**：一层官方知识库，**七维**打标（完整定义见 7.1）：

| 维度 | 取值 | 作用 |
|---|---|---|
| 领域分类 `category` | 7 类：booking / order / refund / account / travel / app / policy（见 5.8） | 分类路由 → 检索范围（物理目录一一对应） |
| 子场景 `sub_scenario` | 41 项（如候补购票、遗失物品），见 7.8 | 目录二级、维度过滤、运营统计（**不进路由**） |
| 书写形态 `form` | `qa` FAQ 式 / `prose` 段落式 | **切块、阈值标定、prompt、出处格式**（见 7.2） |
| 文档类型 `doc_type` | `policy` / `howto` / `faq` / `notice` | 排序加权、运营统计 |
| 时效 `expire_at` | 可空 | 过期文档**检索期过滤**（10⁵ 级不再靠加载期过滤） |
| 身份可见性 `visibility` | `public` 游客可见 / `authenticated` 仅登录 | **Qdrant filter 层强制**（安全边界，不是加载期过滤） |
| 实例化范围 `scope_kind` / `scope_ref` | general / station / ticket_type / route + 标识 | 两级检索收窄候选（**10⁵ 规模的主要来源**） |

**关键约束（防串号）**：**"我的数据"永远不进知识库**——个人订单/车票问题只能由工具层（携带 Go 注入的 `user_id`）作答。知识库里没有私域数据，"知识库泄露他人数据"从架构上不可能发生。

---

### ADR-7：命名冲突处理

**决策**：客服域一律 `cs_` 前缀（HTTP `/cs/*`、表 `cs_conversations`/`cs_messages`/`cs_feedback`、Redis 键 `cs:*`），人工工单表 `support_tickets`。

**理由**：`tickets`（车票）、`sessions`（JWT 会话）、`seat_reservations`、`penalties` 已被票务业务占用；客服工单若也叫 `tickets`，会在 sqlc 生成层、grep、"用户查车票还是工单"的接口语义上持续制造歧义。

---

### ADR-8：AI 工具只读（资金类写操作不由 AI 执行）

**决策**：AI 工具集 = 6 个读工具 + 1 个建单工具；**退票、改签、支付、改价一律不由 AI 执行**，只做"给入口 + 讲规则 + 必要时建单"。

**理由**：
1. 资金操作要求幂等、可追溯、可撤销；LLM 抽参是概率性的——**让概率组件驱动资金变更是把最贵的错误交给最不确定的组件**。
2. 现有退票/支付链路有强一致性设计（条件更新 + 状态机），AI 直调会绕过前端用户确认。
3. 唯一允许的写操作是建单：纯新增记录、不改资金/座位状态、失败可重入。

---

### ADR-9：图状态唯一真相源 = checkpoint，Redis 不存会话状态

**决策**：图执行状态（会话上下文、决策中间结果、待恢复的中断）**只落 checkpoint**；**不在 Redis 再存一份会话状态**。Redis 只承担答案缓存、限流、座位预占。

**理由**：两层各存一份会话状态必然出现"Redis 说该继承上一轮、checkpoint 说没有"这类不一致；且 `interrupt`/resume 的正确性依赖 checkpoint 是唯一权威。**代价**：每轮多一次本地 MySQL 读（毫秒级），换来状态语义唯一。

**"两套记录"的职责必须在文档里写死**（避免被当成双写缺陷）：

| 存储 | 内容 | 定位 | 一致性要求 |
|---|---|---|---|
| checkpoint 表（Python 写，`setup()` 建） | 图状态快照：会话上下文、决策中间结果、中断 | **图执行真相源** | 强一致（图正确性依赖） |
| `cs_messages`（Go 写，golang-migrate 建） | 每轮问答流水：问题、答案、分类、来源、trace_id | **审计与前端展示**（可重建） | 最终一致（展示用，允许滞后） |

> 前端"历史会话/接着对话"读 `cs_messages`（Go 直查），**不依赖编排层在线**。

### ADR-10：检索基础设施——自研 BM25 + Qdrant ANN + rerank

**决策**：检索层按 **10⁵ 级 chunk** 设计（知识库按站点/票种实例化，见 7.1）。

| 环节 | 决策 |
|---|---|
| 向量召回 | **Qdrant**（单 collection + HNSW + payload 预过滤；标量量化 int8，可选 on-disk） |
| 关键词召回 | **保留自研 BM25 在 Go**，但升级为**真倒排 postings + 剪枝（WAND/BlockMax）**，不再全量打分 |
| 融合 | **RRF 仍在 Go**（确定性、可评测、单一计数点）；不交给 Qdrant |
| 精排 | 融合后 top-50 → **rerank（cross-encoder）** → top-5 进生成；rerank 不可用则回退融合分并计数 |
| 数据归属 | MySQL `kb_chunks` = **文本/元数据真相源**；Qdrant = **可重建派生索引**（payload 带 `chunk_id` / `content_hash` / `embedding_model` / scope / visibility / `expire_at`） |

**依据（本机实测，Go 1.22 / 1024 维 / 单位化向量纯点积 / 单线程）**：

| chunk 数 | 单次全量扫描 | 向量常驻 |
|---|---|---|
| 300 | 415 µs | 1.2 MB |
| 800 | 1.15 ms | 3.1 MB |
| 5,000 | 7.3 ms | 19.5 MB |
| 50,000 | 86 ms | 195 MB |
| 200,000 | **295 ms** | 781 MB |

⇒ 10⁵ 量级下进程内扫描会吃掉整个检索预算（预算 ≤400ms），**必须 ANN**。同时 10⁵×1024×4B ≈ 400MB 向量也不该常驻业务进程内存。

**被否方案**：

| 方案 | 否决理由 |
|---|---|
| 继续进程内扫描 | 实测数据不支持（见上表） |
| pgvector | 要新增 PG（本项目只有 MySQL，见 ADR-2 讨论）；过滤 + ANN 的组合不如 Qdrant 直接 |
| ES / OpenSearch | 倒排 + kNN + 过滤一体确实省事，但 JVM 重、资源占用高，单机 Compose 场景不划算 |
| Milvus | 更重（依赖 etcd/MinIO） |
| FAISS | 是库不是服务：无过滤、无持久化、无快照、无多副本 |
| 把关键词路也搬进 Qdrant（稀疏向量 + 内置融合） | 稀疏权重仍需我方分词器产出，融合下沉后"为什么这样排"不可解释，且丢掉"自研检索是可评测资产"这一项；指标口径也会散到两处 |

**选 Qdrant 的判据**：能装进现有 Compose（单二进制）；**payload 过滤是一等公民**（实例化维度 + 可见性过滤全靠它）；支持 int8 量化与 on-disk 向量；支持 **collection alias 原子切换**（重建/换模型的安全网）；Go 客户端成熟。

**治理（10⁵ 级必须的四件事）**：

1. **可重建**：任意时刻都能从 MySQL 文本 + 当前 embedding 模型重建 collection。重建走 **alias 原子切换**（`kb_current` → `kb_v2`），失败回滚即切回 alias，**不原地删**。
2. **对账**：定时抽样比对 MySQL chunk 集合与 Qdrant point 集合（数量 / ID 集合 / `embedding_model` 版本），漂移上报 `index_drift_detected_total` 并触发重建。
3. **批量入库与限速**：首次全量 10⁵ 条是**离线批任务**（分批 embedding + 批量 upsert），必须**断点续跑**（以 `content_hash` 判断进度）与**限速**（供应商配额）；入库不在服务启动主链路上。
4. **过滤即安全边界**：`visibility=authenticated` 必须**在 Qdrant filter 层强制**，不能只在 Go 侧过滤；并且**必须有测试**：游客实例检索不到 authenticated chunk（与"知识库不含私有数据"共同构成双重防线）。

**代价（承认清楚）**：存储从 2 个变 3 个（MySQL / Redis / Qdrant）；向量数据 400MB（int8 量化 ≈100MB）；检索多一次网络往返（同机 1~3ms，可忽略）；新增"漂移与重建"这类运维动作（已在治理里定死责任与指标）。

---

## 4. 总体架构

### 4.1 分层图

```mermaid
flowchart TB
    subgraph Browser["浏览器"]
        B1["票务页面 index/booking/profile"]
        B2["/cs 客服抽屉<br/>web/cs.html + assets/js/cs.js"]
    end

    subgraph GoSvc["Go 服务 tickets（唯一公网入口 :8080）"]
        R["路由 internal/routes/web.go<br/>/cs/*（对外） · /internal/*（内网）"]
        MW["横切：JWT 鉴权 · 限流(Redis) · trace_id · 请求预算 · SSE 透传"]
        subgraph Cap["平台能力层（确定性）"]
            C1["规则分类器（关键词权重打分）"]
            C2["混合检索 BM25+向量+RRF+软路由"]
            C3["工具注册表与执行（提槽/数据源/脱敏/熔断）"]
            C4["会话读写 · 工单状态机 · 答案缓存"]
            C5["引用校验 · 指标与审计"]
        end
        GW["LLM 网关<br/>OpenAI 兼容 /internal/v1/chat/completions"]
    end

    subgraph PySvc["Python 编排层 orchestrator（仅内网 :8001）"]
        F["FastAPI /ask /resume /healthz"]
        LG["LangGraph StateGraph<br/>条件边 · 受控循环 · interrupt/resume"]
        CK["checkpoint（AIOMySQLSaver）"]
        CAPC["能力客户端（内部 HTTP）"]
        GWC["LLM 客户端（openai SDK → Go 网关）"]
    end

    subgraph Infra["存储与外部"]
        DB[("MySQL 8.4<br/>业务表 + cs_* + kb_*（文本真相源）+ checkpoint_*")]
        RD[("Redis<br/>答案缓存 · 限流 · 座位预占")]
        QD[("Qdrant<br/>10⁵ 级 chunk 向量 ANN<br/>（可重建派生索引）")]
        MODEL["模型供应商<br/>（仅 Go 侧持有密钥）"]
    end

    B1 --> R
    B2 --> R
    R --> MW
    MW -->|"命中答案缓存→短路返回"| C4
    MW -->|"转发（X-Internal-Key + trace_id）"| F
    F --> LG
    LG --> CK --> DB
    LG --> CAPC -->|"分类/检索/工具/校验/建单/流水/埋点"| Cap
    LG --> GWC --> GW --> MODEL
    Cap --> DB
    Cap --> RD
    C2 --> QD
    GW --> RD
```

### 4.2 `POST /cs/ask` 全链路时序

```mermaid
sequenceDiagram
    participant U as 买家
    participant Go as Go 接入层
    participant Py as Python 编排层
    participant G as 图执行
    participant Cap as Go 平台能力
    participant LLM as Go LLM 网关
    participant DB as MySQL

    U->>Go: POST /cs/ask（Bearer JWT 可选）
    Go->>Go: trace_id · 身份解析(服务端) · 限流
    Go->>Py: 转发（internal key + trace_id + user_id 结果）
    Py->>G: ainvoke / astream（thread_id = conv_id）
    G->>DB: 读 checkpoint（图状态恢复）
    G->>Cap: /internal/classify/pre-intent（确定性词表）
    G->>Cap: /internal/classify/rule（规则打分）
    alt 置信度不足
        G->>LLM: 跨类仲裁（闭集输出）
    end
    G->>Cap: /internal/tools/route（正则提槽）
    alt 工具命中
        G->>Cap: /internal/tools/{name}（归属校验 + 脱敏）
        Cap-->>G: 结构化结果
        Note over G: 事实类直答，不经 LLM
    else 需要知识库
        G->>Cap: /internal/retrieve（BM25+向量+RRF）
        Cap-->>G: chunks + source_labels + top1_cos
        alt top1_cos < 阈值
            Note over G: 不作答 → 转人工路径②
        else
            G->>LLM: 生成（流式，只用知识库 + 强制标出处）
            LLM-->>Go: token 流 → 透传前端
            G->>Cap: /internal/citation/verify（剥除 + 计数）
        end
    end
    alt 转人工（四路径任一）
        G->>Cap: /internal/support-tickets（建单）
        Cap-->>G: ticket_no
    end
    G->>Cap: /internal/session/turns（写流水）
    G->>Cap: /internal/metrics（图侧决策埋点回传）
    G-->>Go: 最终事件（meta/delta/done）
    Go-->>U: SSE / JSON
```

### 4.3 主链路分步（归属 / 失败降级）

| # | 步骤 | 归属 | 失败降级 |
|---|---|---|---|
| 1 | trace_id 生成、身份解析、限流 | Go | 无 token → 游客态（仅公共知识） |
| 2 | 转发编排层 | Go | **编排层不可用 → 转人工 + 标准话术**（接口不中断） |
| 3 | 读 checkpoint（状态恢复） | Python | checkpoint 不可用 → 本轮退化为无状态执行（记告警） |
| 4 | 前置意图（投诉/要人工/问候） | Go 词表 | — |
| 5 | 规则分类打分 | Go | — |
| 6 | 置信度不足 → LLM 仲裁 | Python + Go 网关 | LLM 失败 → 用规则 top1；规则无命中 → `other` 兜底 |
| 7 | 工具路由（正则提槽） | Go | 槽位缺失 → 消歧反问（interrupt）或 LLM 选工具 |
| 8 | 工具执行 | Go | 超时/数据源异常 → **工具不可用路径单独计数** → 转人工 |
| 9 | 混合检索 | Go | Embedding 不可用 → 字面替身（接口不变）；空结果 → 改写重试（上限 1）→ 转人工 |
| 10 | 生成（流式） | Python + Go 网关 | LLM 超时/报错 → 兜底话术 + 转人工 |
| 11 | 引用校验 | Go | 剥除后无有效出处 → 转人工 |
| 12 | 建单 / 流水 / 埋点 | Go | 落库失败不影响响应（error 日志） |

---

## 5. Python 编排层设计（Agent 主体）

### 5.1 服务形态

| 项 | 设计 |
|---|---|
| 框架 | FastAPI + uvicorn（异步），`sse-starlette` 出流 |
| 依赖管理 | `uv`（`pyproject.toml` + `uv.lock`，锁死组合） |
| Python | 容器 `python:3.12-slim`；本机开发用 3.11 |
| 监听 | `0.0.0.0:8001`（容器内），**compose 不映射宿主机端口** |
| 接口 | `POST /ask`（JSON / SSE）、`POST /resume`（显式恢复，可选）、`GET /healthz`（含 checkpointer 可达性） |
| 鉴权 | `X-Internal-Key` 校验（与 Go 侧同值配置） |
| 无状态 | 进程不持有会话状态（全在 checkpoint）；可多副本水平扩容 |

### 5.2 图状态（State）

```python
class CSState(TypedDict, total=False):
    # 输入（由 Go 注入，编排层不得自行解析身份）
    question: str
    conv_id: str                 # = thread_id
    user_id: int | None          # 服务端解析结果，None = 游客
    guest_key: str | None        # 游客设备 hash（Go 侧算好）
    trace_id: str

    # 决策中间态
    pre_intent: str              # complaint | human | greet | none
    category: str                # booking | order | refund | account | travel | app | policy | other
    sub_scenario: str | None     # 41 项子场景（不进路由，仅过滤/统计）
    form: str                    # qa | prose（切块与阈值按形态分派）
    cls_source: str              # rule | llm_arbitrate | inherit | fallback
    cls_scores: dict[str, int]   # 规则层多类得分（软路由要用，禁止丢弃）
    retry_count: int             # 改写重试次数（上限 1）

    # 供给
    tool_name: str | None
    tool_args: dict
    tool_result: dict | None
    chunks: list[dict]
    source_labels: list[str]     # 供引用校验
    top1_cos: float

    # 输出
    answer: str
    sources: list[dict]
    transfer: bool
    transfer_path: str           # deterministic | threshold | model | tool_unavailable
    support_ticket_no: str | None

    # 观测
    degraded: dict
    stage_ms: dict[str, int]
    tokens: dict
```

**铁律**：`user_id` 只能来自 Go 注入；图内**不得**从 `question` 里解析用户身份。

### 5.3 节点与条件边（编排资产的核心）

```mermaid
flowchart TD
    START([入站]) --> load["load_context<br/>读 checkpoint + 会话窗口"]
    load --> pre["pre_intent<br/>(Go 词表, 零 LLM)"]
    pre -->|complaint/human| transfer["transfer<br/>建单"]
    pre -->|greet| greet["greet<br/>友好回应"]
    pre -->|none| cls["classify_rule<br/>(Go 规则打分)"]
    cls -->|"gap 充足"| route["route_tool<br/>(Go 正则提槽)"]
    cls -->|"置信度不足"| arb["llm_arbitrate<br/>(闭集 + 跨类)"]
    arb --> route
    route -->|命中| exec["exec_tool<br/>(Go 执行)"]
    route -->|未命中| llmroute["llm_route_tool<br/>(只选工具)"]
    llmroute -->|选中| exec
    llmroute -->|无合适工具| retr["retrieve<br/>(Go 混合检索)"]
    exec -->|多候选| clar["clarify<br/>interrupt 反问"]
    clar -->|"resume(user 选择)"| exec
    exec -->|事实类| tans["tool_answer<br/>确定性直答(不过 LLM)"]
    exec -->|需融合知识| retr
    retr -->|空 & retry==0 & 有上下文| rw["rewrite<br/>(LLM 只改写)"]
    rw --> cls
    retr -->|有结果| gate{"gate<br/>top1_cos >= 阈值?"}
    retr -->|空 & 不可重试| transfer
    gate -->|否| transfer
    gate -->|是| gen["generate<br/>(流式, 经网关)"]
    gen --> ver["verify_citations<br/>(Go 校验)"]
    ver -->|无有效出处| transfer
    ver -->|通过| fin["finalize<br/>写流水/缓存/埋点"]
    tans --> fin
    greet --> fin
    transfer --> fin
    fin --> END([返回])
```

**条件边清单（评审与测试的对照表）**

| 边 | 条件 | 目标 |
|---|---|---|
| E1 | `pre_intent ∈ {complaint, human}` | `transfer`（路径①） |
| E2 | `pre_intent == greet` | `greet` |
| E3 | `cls_source == rule 且 gap 充足` | `route_tool` |
| E4 | `置信度不足`（gap < 阈值 或 top1 == 0） | `llm_arbitrate` → `route_tool` |
| E5 | 提槽命中候选 | `exec_tool` |
| E6 | 提槽未命中 且 LLM 选出工具 | `exec_tool` |
| E7 | 提槽未命中 且 无合适工具 | `retrieve` |
| E8 | 工具结果多候选（如多张票） | `clarify`（**interrupt**） |
| E9 | 工具结果为事实类且自足 | `tool_answer` → `finalize` |
| E10 | 工具结果需与知识库融合 | `retrieve` |
| E11 | 检索空 且 `retry_count == 0` 且会话有上下文 | `rewrite`（`retry_count += 1`）→ `classify_rule`（**循环，上限 1**） |
| E12 | 检索空 且（无上下文 或 已重试） | `transfer`（路径②） |
| E13 | `top1_cos < 相关性阈值` | `transfer`（路径②） |
| E14 | 生成输出 `[TRANSFER]` 标记 | `transfer`（路径③） |
| E15 | 引用校验后无有效出处 | `transfer`（路径③） |
| E16 | 其余 | `finalize` |

**循环与预算（必须有硬上限，否则"Agent"会变成不可控的烧钱机）**

| 约束 | 值 | 说明 |
|---|---|---|
| 改写重试次数 | ≤ 1 | `retry_count` 单调递增，超限强制 `transfer` |
| 工具调用次数 / 轮 | ≤ 2 | 消歧反问的 resume 不重复计数（见 5.4） |
| 整轮墙钟预算 | 8s | 超预算 → 兜底话术 + 转人工（Go 侧也有一层超时） |
| 整轮 token 预算 | 配置项 | 网关侧按 trace 累计，超限即拒 |
| interrupt 次数 / 轮 | ≤ 1 | 避免反复反问 |

### 5.4 中断与恢复（`interrupt` / `Command(resume=...)`）

**用途**：多轮**消歧反问**（"您说的是 8月20日 10:30 那趟，还是 14:00 那趟？"）——用户回答后**从断点续跑**，不必重放整条链路。

**官方语义（必须照做，否则会踩坑）**：

| 规则 | 说明（来自 LangGraph 文档） |
|---|---|
| `interrupt(payload)` 的 payload 必须 **JSON 可序列化**，不要传复杂对象 | 复杂值会带来序列化/反序列化不一致 |
| **不要用 `try/except` 包 `interrupt`** | 它靠异常机制暂停，捕获会吞掉暂停信号 |
| 同一个节点内**不要调整 `interrupt` 的调用顺序** | 恢复时按调用序号匹配 resume 值，顺序变了就错位 |
| **`interrupt` 之前的副作用必须幂等** | **恢复时该节点从函数开头重新执行**（interrupt 之前的代码会再跑一遍）——这是最容易踩的一条：反问节点里若先写了库/调了工具有副作用的接口，resume 会重放 |
| 恢复必须用**同一个 `thread_id`** | `thread_id` 是持久化游标；换 ID = 开新线程、空状态 |
| **只用 `Command(resume=...)` 作为输入**；多轮新问题**传普通 dict** | 官方明确：`Command(update=...)` 等其它形式不是给 `invoke/stream` 当输入用的——混用会让多轮行为错乱 |

**设计上的应对**：
- 反问节点只做"读数据 + 组装选项 + interrupt"，**任何写操作都放到 `finalize`**（在 interrupt 之后、图末尾）；
- 反问所需的候选集在**上一节点**算好（`exec_tool` 返回多候选时顺手带回），反问节点不再重复调工具 ⇒ 重放也无副作用；
- resume 值经校验后使用（用户可能回复任意文本：选项序号、日期、"都不是"），校验失败 → 再次 `interrupt` 或转人工（避免死循环，interrupt 次数上限 1 后走转人工）。

**流式下的中断**：`stream_events(..., version="v3")` 会给出 `stream.interrupted`（是否暂停）与 `stream.interrupts`（payload），据此向 Go 回一个 `interrupt` 事件（前端把反问当普通回复渲染）；用户下一条消息带同一 `conv_id` 进图，编排层以 `Command(resume=...)` 续跑。**若该用法在 M0 spike 中与所选版本不符，以 spike 实测 API 为准并回写本文档。**

### 5.5 checkpoint 治理

| 项 | 设计 |
|---|---|
| `thread_id` | = `conv_id`（UUID，36 字符）——**远低于 saver 的 255 字符列限制**（官方明确提示 PostgresSaver 的 `thread_id` 过长会报库错，MySQL saver 同形） |
| 建表 | 首次启动调 `setup()`；手工连接必须 `autocommit=True` |
| 增长 | **checkpoint 会无界增长**（每个 superstep 一条快照）⇒ 必须有清理任务：Python 侧定时任务（`adelete_thread` 或直接按 `ts` 清理超期 thread），保留期建议 ≥7 天（对齐审计需要），配置化 |
| 恢复粒度 | 每轮对话一个 thread；同一 `conv_id` 的下一轮复用同一 thread（`thread_id` 就是会话 ID） |
| 与业务表关系 | 见 ADR-9（职责表），Go 侧迁移不碰 checkpoint 表 |

### 5.6 流式输出

- 图内 `generate` 节点通过**Go 网关**以流式请求 LLM，逐段产出 `delta`；
- 编排层把 LLM token 流与控制事件（`meta` / `interrupt` / `done`）转成 SSE 给 Go；
- **首字节不是首答案**：`meta` 事件在分类完成后即可下发（前端可先渲染"正在为您查询…"），真正 `delta` 在 LLM 首字到达时开始（约 1.7s，见 11.2）；
- 引用校验发生在 `done` 之前：流式期间累积完整文本，校验若发生剥除，`done` 事件带**修正后的答案 + `citation_dropped` 计数**，前端以 `done` 覆盖渲染（避免"先吐错答案再悄悄改"）。

### 5.7 能力层接口化（Go 侧的生产者契约）

编排层只依赖**接口**（HTTP 契约 + Python 侧 `CapabilityClient` 抽象），不关心 Go 内部实现：

| Python 侧抽象 | 背后的 Go 接口 |
|---|---|
| `CapabilityClient.pre_intent(q)` | `POST /internal/classify/pre-intent` |
| `CapabilityClient.classify_rule(q)` | `POST /internal/classify/rule` |
| `CapabilityClient.retrieve(q, category, scores, visibility)` | `POST /internal/retrieve` |
| `CapabilityClient.route_tool(q, ctx)` | `POST /internal/tools/route` |
| `CapabilityClient.run_tool(name, args, ident)` | `POST /internal/tools/{name}` |
| `CapabilityClient.verify_citations(answer, labels)` | `POST /internal/citation/verify` |
| `CapabilityClient.create_ticket(...)` | `POST /internal/support-tickets` |
| `CapabilityClient.record_turns(...)` | `POST /internal/session/turns` |
| `CapabilityClient.metrics(events)` | `POST /internal/metrics` |
| `LLMClient.chat(...)` / `.chat_stream(...)` | `POST /internal/v1/chat/completions`（OpenAI 兼容） |

⇒ 未来把能力层里的某一项搬成独立服务、或换编排框架，都只改客户端基址。

### 5.8 分类路由与场景分类（7 类闭集）

**分类是路由键**：它把检索范围从 10⁵ 收窄到一个领域，同时决定软路由行为与评测口径。**闭集 = 7 类 + `other` 兜底**（LLM 只允许在该集合内选择，不得发明新类别；解析失败回退 `other`）：

| 类别 | 覆盖 | 知识库目录 |
|---|---|---|
| `booking` 车票预订 | 购票流程、选座与席位、特殊票种、团体/企业、联程接续、限售区间、列车调整 | `kb/booking/` |
| `order` 订单与支付 | 订单查询、支付问题、支付方式、电子客票与报销凭证、通知提醒 | `kb/order/` |
| `refund` 退票改签 | 退票窗口与阶梯手续费、改签、变更到站、停运晚点政策、退款到账 | `kb/refund/` |
| `account` 账号与身份 | 注册登录与密码、身份核验、常用联系人、证件问题、账号锁定风控、手机/邮箱 | `kb/account/` |
| `travel` 乘车与车站服务 | 检票进出站、行李与违禁品、儿童/老人/宠物、重点旅客、**遗失物品**、晚点停运、候车换乘 | `kb/travel/` |
| `app` 系统与技术问题 | 页面报错、接口异常、缓存与浏览器兼容、验证码与推送 | `kb/app/` |
| `policy` 投诉与政策 | 投诉渠道与反馈、活动规则、会员积分、政策公告 | `kb/policy/` |
| `other` 其他（**无知识库**） | 问候 / 闲聊 / 无关 | 无 → 走兜底话术链 |

**两层结构**：`category`（7 类，**路由闭集**）× `sub_scenario`（41 项子场景，**不进路由**，只用于目录二级、维度过滤与运营统计）。子场景完整清单与「系统支撑」对照见 **§7.8**。

**前置意图优先于分类**：投诉 / 明确要人工 / 问候在 `pre_intent` 层（E1/E2）就被拦下，不进分类；其中**投诉行为直接转人工建单**，只有「投诉渠道与政策」这类**信息类问题**才走分类 → `policy` 检索。

**四级链路**（归属见 5.3 节点）：① 规则关键词权重打分（**Go**，强信号权重 3 / 模糊词 1，取最高分并看 top1-top2 间距）→ ② 置信度不足则 **LLM few-shot 兜底**（**Python**，闭集输出，保留规则层 `cls_scores` 供软路由）→ ③ 多类竞争则 **LLM 跨类仲裁**（**Python**，按主意图定类）→ ④ 指代继承（`other` + 指代词 + 有历史话题 → 继承上一轮类别，并补 `cls_scores`）。

**类别扩张的代价要认账**：4 类 → 7 类让跨类边界样本变多（退款到账时效 refund↔order、扣款成功无订单 order↔account、能否开发票 refund↔order、遗失物品 travel↔policy）。因此分类 held-out 必须扩到 **≥400 条（每类 ≥40）** 并显式覆盖这些边界（15.1）。

### 5.9 能力边界声明（B 方案的核心防线）

**背景**：知识库定位为**铁路客运行业知识库**（覆盖完整业务域），因此**必然存在「知识库知道、平台没实现」的功能**。若不加约束，用户会拿到「候补购票流程：…」这种语气笃定的答案，然后在这个平台上找不到入口——**比答不上来更糟**（它是被检索出来、带出处的"官方答案"）。

**四级防线**：

| 级 | 机制 | 落地 |
|---|---|---|
| ① 数据 | 每个 chunk 带 `capability ∈ {supported, roadmap, industry}`（入库时从能力台账 7.9 快照） | `kb_chunks.capability` + Qdrant payload |
| ② 检索 | 默认 `filter(capability=supported)` 优先；无结果或分数低才**放宽 filter**（复用 7.1 的两级检索），放宽后命中一律标「行业参考」 | `/internal/retrieve` 返回 `capability` 字段与 `widened` 标志 |
| ③ 生成 | prompt **物理分区两段**：【本平台能力】（可给入口与路径）／【行业参考】（`roadmap` + `industry`，**禁止操作性措辞**，必须前置边界声明） | 见下方模板 |
| ④ 后处理 | 生成后扫答案：来源含 industry/roadmap **且**命中操作性词表（点击 / 进入 / 在…页面 / 我们的 / 本站支持）→ **拦截并强制改写**，计数 `boundary_violation_blocked_total` | 与引用校验同一层，纯确定性 |

**边界声明模板**（由生成层注入，**不写进知识库正文**）：

> 该功能本平台暂未开放，以下是铁路客运行业的一般做法，供参考；具体以车站与官方渠道为准。
> （如需办理，我可以为您转接人工客服，工单号 …）

**三条硬约束**：

1. **不得并列矛盾结论**：同一回答里不允许同时出现「系统数据显示无记录」和「你可以候补」这类组合。涉及个人数据的内容**只能来自工具**，工具说没有就是没有；行业参考只回答「一般规则」，且两句之间必须有明确分隔语（「另外，行业通行规则是…」）。
2. **`supported` 项必须能说出入口**（前端路径 / 接口 / 工具名），由入库 lint 机检——**说不出入口就不算支持**。
3. **功能缺失走独立计数**：用户要的是本平台功能（如「我的候补订单」）而台账中该子场景为 `roadmap`/`industry` → 转人工，路径计 `capability_absent`（与「工具不可用」区分：一个是**功能不存在**，一个是**下游坏了**）。

**机制要点**：边界话术由生成层模板注入、**不写进知识库正文** ⇒ 台账状态翻转（`roadmap → supported`）时**只改措辞层，不需要重嵌**（Qdrant 侧只更新 payload 的 `capability`）。

### 5.10 编排工程约定（现在定骨架，实现细节留给编码期）

**结论先行**：图结构与决策逻辑属于**架构**，已在 5.2~5.9 定完（State 字段、E1~E16 条件边、循环与预算、interrupt 语义、降级路径、边界声明）；而 **LangChain/LangGraph 的具体 API 用法属于实现**，现在只定「边界与形状」，不写具体调用——因为 **M0 spike 的结论会改它们**（checkpointer 可用性、流水线 API 版本、serde 选型、依赖 pin）。

#### 5.10.1 组件边界：用什么、不用什么

| 用 LangChain 的 | 为什么 | **不用**的 | 为什么 |
|---|---|---|---|
| `langchain-openai` 的 `ChatOpenAI`（`base_url` 指向 Go 网关） | 天然支持流式、usage 回调、超时与重试配置 | LangChain 的 **Tool 抽象 / `ToolNode` / `bind_tools`** | 工具执行在 Go、参数由 Go 的确定性提槽填（ADR-3）；引入会把「决策」与「执行」重新混回 Python，并制造第二套工具 schema |
| `ChatPromptTemplate` | prompt 是高频迭代对象，需独立于代码版本管理与单测 | `AgentExecutor` / `create_react_agent` 等 prebuilt | 正是本项目明确不要的「自主循环」（ADR-4 的受控循环由手写条件边实现） |
| `with_structured_output`（分类兜底 / 跨类仲裁 / 槽位补全） | 比手写 JSON 解析稳，schema 即契约 | LangChain 的 **Retriever / VectorStore** | 检索在 Go + Qdrant（ADR-10）；引它等于把检索拆成两处 |
| `RunnableConfig` / callbacks（`trace_id` 贯穿、阶段耗时） | 与观测层（14.1）对齐 | LangChain 的 **Memory 抽象** | 会话状态在 checkpoint（ADR-9），`add_messages` 已足够 |
| — | — | LangChain 的 **Cache** | 答案缓存是业务级语义缓存，在 Go 侧（9.3） |

一句话：**用 LangChain 做「模型调用 + prompt + 结构化输出」，不用它做「工具、检索、记忆、缓存」**——后四件都是已定死在 Go 或图状态里的东西。

#### 5.10.2 State 形状（外部契约，现在定）

- `TypedDict` + **显式 reducer**：对话消息用 `add_messages`；`stage_ms` / `degraded` 用合并 reducer；`retry_count` / `top1_cos` 用覆盖语义（后写胜）。
- **State 里只放可序列化数据**（它会进 checkpoint）：不放 httpx client、连接池、logger——那些走依赖注入（`Runtime` / 闭包），否则会污染 checkpoint 并在 resume 时反序列化失败。
- 消息用 LangChain message 类型（`HumanMessage` / `AIMessage`），但**业务字段（`category` / `tool_result` / `chunks` / `capability`）放 State 顶层**，不要塞进 message metadata——评测与埋点要直接读。

#### 5.10.3 流事件映射（前端与 Go 透传的契约，现在定）

| 图侧事件 | SSE 事件 | 时机 |
|---|---|---|
| 分类完成 / 工具命中 | `meta` | **尽早发**，让前端先渲染「正在为您查询…」 |
| LLM token | `delta` | 生成节点流式 |
| `stream.interrupted` | `interrupt` | 消歧反问（可选） |
| 图结束 | `done` | 带 `answer` / `sources` / `transfer` / `citation_dropped` |

> 引用校验（11）与**边界声明注入（5.9）都在 `done` 之前完成**：流式期间累积全文，校验/改写后再定稿（见 5.6），避免「先吐错答案再悄悄改」。

#### 5.10.4 `/ask` 与 `/resume` 的分派规则（最容易出 bug，现在定）

```
收到 /ask(conv_id):
  1. 读该 thread 状态；若存在 pending interrupt → 本次输入用 Command(resume=<用户消息>)
  2. 否则 → 普通 dict 输入（多轮新问题【必须】用普通输入，见 5.4）
  3. resume 值校验失败（答非所问）→ 允许再次 interrupt 一次；仍失败 → 转人工
```

对应验收：**反问一次 → 重启编排层进程 → 用同一 `conv_id` 继续 → 正确续跑**（5.10.5）。

#### 5.10.5 测试与评测挂点（现在定，否则 M1 无法验收）

| 挂点 | 做法 |
|---|---|
| 节点单测 | 假 `CapabilityClient`（httpx `MockTransport`）+ 假 LLM（固定响应），零真实依赖 |
| 条件边覆盖 | **E1~E16 每条边至少一例**（边就是产品行为） |
| interrupt / resume | 真实 MySQL checkpointer + **跨进程重启**后 resume 成功；且反问节点重放无重复副作用 |
| 预算 | 注入超时 / 超额，断言降级为转人工 |
| 端到端 | **黑盒打 Go `/cs/ask`**（唯一算数的端到端口径，15.3） |

#### 5.10.6 明确留到编码期（M0 spike 之后再定）

- 具体 API 形式（事件流 / 消息流的接口版本）、serde 选型、checkpointer 最终档位（ADR-2）；
- prompt 文本、节点内部实现、图的微调（节点拆分/合并，如 `gate` 是否并进 `retrieve`）；
- 是否引入 subgraph、是否启用 LangSmith（默认关闭：企业场景对话数据不出内网）。

---

## 6. 跨语言契约

### 6.1 内部接口清单（Go 提供，仅内网）

| 组 | 接口 | 请求要点 | 响应要点 |
|---|---|---|---|
| 分类 | `POST /internal/classify/pre-intent` | `{question}` | `{pre_intent}`（确定性词表命中） |
| 分类 | `POST /internal/classify/rule` | `{question}` | `{scores, top1, top2, gap}` |
| 检索 | `POST /internal/retrieve` | `{question, category, scores, visibility, top_k}` | `{chunks[{label, text, score}], source_labels, top1_cos, vector_mode, empty}` |
| 工具 | `POST /internal/tools/route` | `{question, session_hint}` | `{candidates[{tool, args, confidence}]}` |
| 工具 | `POST /internal/tools/{name}` | `{args, user_id, guest_key, trace_id}` | `{ok, data(脱敏), direct_answer, candidates?, error_kind}` |
| 校验 | `POST /internal/citation/verify` | `{answer, source_labels}` | `{answer_cleaned, dropped, valid_sources}` |
| 工单 | `POST /internal/support-tickets` | `{conv_id, user_id, order_no, category, path, summary}` | `{ticket_no, status}` |
| 流水 | `POST /internal/session/turns` | `{conv_id, user_id, question, answer, category, source, trace_id}` | `{ok}` |
| 埋点 | `POST /internal/metrics` | `{trace_id, events[{node, decision, ms, meta}]}` | `{ok}` |
| 模型 | `POST /internal/v1/chat/completions` | OpenAI 兼容（支持 `stream: true`） | OpenAI 兼容（含 `usage`） |

**统一约定**：
- 头部：`X-Internal-Key: <secret>`、`X-Trace-Id: <uuid>`；
- 时间：毫秒时间戳 / RFC3339 二选一，全链统一（建议 RFC3339）；
- 错误：`{error: {kind, message}}`，`kind ∈ {invalid_input, not_found, unauthorized, upstream_unavailable, budget_exceeded}` —— 编排层据 `kind` 决定"重试 / 转人工 / 降级"，**不解析错误文案**。

### 6.2 超时、重试与降级

| 调用 | 超时 | 重试 | 失败表现 |
|---|---|---|---|
| Go → Python `/ask`（整轮） | 8s（流式按空闲超时 15s） | **不重试**（人工转接是兜底，重试会双倍成本） | Go 返回转人工话术 + 建单 |
| Python → Go `/internal/classify/*`、`/tools/route` | 1s | 1 次 | 降级：跳过该判断（分类落 `other`、工具落检索） |
| Python → Go `/internal/retrieve` | 1.5s | 0（可在图内重试） | 检索空 → 走 E11/E12 |
| Python → Go `/internal/tools/{name}` | 1.5s | 0 | **工具不可用路径计数** → 转人工 |
| Python → Go 网关（非流式） | 20s | 1 次（幂等） | 兜底话术 + 转人工 |
| Python → Go 网关（流式） | 首字节 8s / 空闲 10s | 0 | 中止流出 + 兜底话术 |
| Go → 模型供应商 | 20s（流式同） | 1 次 | 同上 |

**预算**：整轮墙钟 8s（Go 与 Python 各持一份，取先到者）；token 预算在网关按 `trace_id` 累计，超限返回 `budget_exceeded`。

### 6.3 流式转发机制（Go 侧）

Fiber 基于 fasthttp，转发 Python 的 SSE 用**流式 body writer**（`c.Context().SetBodyStreamWriter(...)`）逐块 `Flush`，**不能先 `ReadAll` 再写**（那样会把流式退化成一次性响应）。

必须处理的四件事：
1. **不缓冲**：转发路径上禁用压缩与响应缓冲，并显式 `Flush`；
2. **客户端断开**：前端关闭时取消对 Python 的请求（`context` 传播），避免编排层继续跑完一整轮白花钱；
3. **超时**：读空闲超时（10~15s）而非整轮超时，否则长答案会被腰斩；
4. **响应头**：`Content-Type: text/event-stream`、`Cache-Control: no-cache`、`X-Accel-Buffering: no`。

### 6.4 降级：编排层不可用

| 场景 | Go 行为 | 用户感知 |
|---|---|---|
| Python 服务不可达 / 健康检查失败 | 直接建单（路径 `tool_unavailable` 语义扩展为 `orchestrator_unavailable`）+ 标准话术 | "已为您转接人工客服，工单号 …" |
| Python 返回 `budget_exceeded` | 同上 | 同上 |
| Python 超时（8s） | 同上 | 同上 |

⇒ 明确一条验收：**停掉编排层，`/cs/ask` 仍必须 200 且给出可追踪工单号**。

---

## 7. 知识库与检索设计（Go 侧）

### 7.1 目录组织与元数据（七维）

**规模由维度实例化决定，不由覆盖多少领域决定**：多覆盖几个领域，KB 从 800 涨到 ~1,500 条、进程内扫描 1→2ms，什么都不用改；本项目明确选择**逐站点/逐票种实例化**，因此按 **10⁵ chunk** 设计。

```
kb/
├── booking/    车票预订（含 sub_scenario/ 二级：普通购票、选座席位、特殊票种、团体、联程、限售）
├── order/      订单与支付（订单查询、支付问题、支付方式、报销凭证、通知）
├── refund/     退票改签（阶梯退票、改签、变更到站、停运政策、退款到账）
├── account/    账号与身份（注册登录、身份核验、常用联系人、证件、风控、手机邮箱）
├── travel/     乘车与车站服务（检票、行李、儿童老人、宠物、重点旅客、遗失物品、换乘）
├── app/        系统与技术（页面报错、接口异常、缓存与兼容、验证码与推送）
├── policy/     投诉与政策（投诉渠道、活动规则、会员积分、公告）
└── stations/   站点实例化规则（按站点展开，含票种差异）→ 10⁵ 规模主体
```

**元数据（七维）**：

| 维度 | 取值 | 作用 |
|---|---|---|
| 领域 `category` | booking / order / refund / account / travel / app / policy | 分类路由、软路由（**闭集，7 类**） |
| **子场景 `sub_scenario`** | 41 项（如 `waitlist` 候补、`lost_item` 遗失物品），见 §7.8 | 目录二级、维度过滤、运营统计（**不进路由**） |
| **形态 `form`** | `qa` / `prose` | **切块、阈值标定、prompt、出处格式**（见 7.2） |
| 类型 `doc_type` | policy / howto / faq / notice | 排序加权、运营统计 |
| 时效 `expire_at` | 可空 | 过期文档**检索期过滤**（10⁵ 级不再靠加载期过滤） |
| 可见性 `visibility` | public / authenticated | **Qdrant filter 层强制**（安全边界，见 ADR-10） |
| 范围 `scope_kind` / `scope_ref` | general / station / ticket_type / route + 标识 | 实例化维度的两级检索，收窄候选 |
| **能力 `capability`** | `supported` / `roadmap` / `industry` | 检索优先 supported；`roadmap`/`industry` 命中必须加**边界声明**（B 方案核心，见 5.9） |

**两级检索（实例化的关键机制）**：用户问"北京西站几点停止检票"→ `filter(scope_kind=station, scope_ref=北京西站)` 召回；用户没提站点 → 先按 `general` 优先，命中不足或分数不佳再**放宽 filter 重搜一次**（记账 `filter_relaxed_total`，放宽后必须重算 top1 余弦再判阈值）。

**取舍（说清楚）**：实例化的好处是答案精确、可按站点独立维护；代价是 10⁵ 级规模 + **内容陈旧风险**——`stations/` 这上千条规则由谁维护、怎么更新，必须先定死（见 19.9），否则"规模大"会变成"陈旧答案多"。


### 7.2 文档规范（两种形态）

知识库承认**两种书写形态**，它们的切块、阈值、出处与 prompt 处理都不同。**`form` 必须显式声明**（默认按 `doc_type` 推导：`faq → qa`，`policy/howto/notice → prose`），不允许隐式猜测。

#### 形态 A：FAQ 式 `form: qa`（一问一答，用于通用高频问答）

```markdown
---
id: refund-fee-rule
title: 退票手续费怎么算
category: refund
sub_scenario: refund_fee
form: qa
type: policy
visibility: public
expire_at:
---

## 退票手续费怎么算

问：退票要扣多少钱？
问：退票手续费怎么算
问：我退票手续费多少啊
答：距发车时间越近手续费越高，具体比例以班次退票规则为准；退票后票款按原支付渠道退回。
```

- **切块**：一个 `##` = 一个 chunk，结构边界清晰，**不需要重叠**；短块（约 100~300 字）语义纯净
- **出处**：`refund/refund-fee-rule·段落1`
- **相似度**：问与答同块，余弦普遍偏高 → 阈值好标
- **prompt**：直接作答

#### 形态 B：段落式 `form: prose`（层级小标题 + 条款叙述，用于站点规则/政策/流程）

```markdown
---
id: station-beijingxi-checkin
title: 北京西站检票规则
category: travel
sub_scenario: checkin_rule
form: prose
scope_kind: station
scope_ref: 北京西站
visibility: public
expire_at:
---

# 北京西站检票规则

## 检票时间

一、本站实行电子客票，凭购票证件原件检票进站。
二、开车前 15 分钟停止检票，节假日客流高峰期间提前至 25 分钟。
三、重点旅客（轮椅、担架）可由站内爱心通道优先检票。

## 特殊情况

一、列车晚点时，检票时间顺延至列车实际到站后。
```

- **切块**：三层回退——`###`/`####` 子标题 → 条款编号（一、二、1. 2.）→ 句子边界（。！？；换行）；**按 rune 计数**；单块上限建议 **400~600 字**；相邻块**保留 1 句重叠**（防条款被切断）
- **必须拼「标题路径前缀」**：chunk 正文前拼接层级路径（`北京西站检票规则 > 检票时间`）→ 对抗长段落语义稀释（踩过的坑：稀释后余弦掉到阈值下 → 误判「无有效结果」→ 转人工）
- **出处**：`stations/北京西站·检票规则#2`（文档·章节路径 + 序号）
- **相似度**：长文本与短问题的余弦会被摊薄、分布整体偏低 ⇒ **阈值必须分形态标定**（用 FAQ 标出的 0.60 会误杀 prose 的合法命中）
- **prompt**：段落式常带条件分支（「若…则…；否则…」）⇒ 要求**先对齐用户情形再给结论**，且**禁止省略适用条件**（否则会把「某站点的例外」当通用规则答出去）

#### 入库 lint（10⁵ 级必须机器校验）

| 形态 | 校验规则 |
|---|---|
| `qa` | 每个 `##` 至少 2 条 `问：` 变体 + 非空 `答：`；同文档内 `##` 标题不重复；正文长度 ≤ 上限 |
| `prose` | 标题深度 ≤3；**chunk 内不得含指代词**（如上所述 / 见第 3 条 / 该站 → 脱离上下文不可理解，告警）；chunk ≤ 上限（rune）；每条规则须带适用条件或明确标「通用」 |

> **不入知识库的第三形态**：表格 / 结构化参数（退票费档位、票价、班次时刻）——走工具层查表。表格进 KB 会带来「参数改了内容就陈旧 + 重嵌成本 + 靠语义匹配数字本身不可靠」三个问题（与既有粒度原则一致）。

### 7.3 入库流水线（含 Qdrant 同步与断点续跑）

```mermaid
flowchart LR
    A["kb/**.md（含 stations/ 实例化）"] --> B["loader：frontmatter 解析<br/>visibility / expire 校验"]
    B --> C["chunk：按 ## 切（rune 计数）"]
    C --> D["content_hash 比对（MySQL）"]
    D -->|未变| E["跳过（不重嵌）"]
    D -->|新增/变更| F["MySQL 写 kb_documents / kb_chunks<br/>文本与元数据 = 真相源"]
    F --> G["分批 Embedding<br/>（限速 + 断点续跑）"]
    G --> H["Qdrant 批量 upsert<br/>point id = kb_chunks.id"]
    D -->|删除/下架| I["MySQL 软删 + Qdrant delete(by id)"]
    F --> J["自研 BM25 倒排增量更新<br/>（只动受影响 term 的 postings）"]
```

**增量与重建原则**：

- `content_hash` 命中即跳过；**只重嵌变更 chunk**——10⁵ 级下 embedding 是最大成本项，增量必须精确；
- 首次全量入库是**离线批任务**（`cmd/kb-sync`）：分批 embedding、批量 upsert、**断点续跑**（以 `content_hash` 判断已完成部分）、**限速**（供应商配额）；
- **embedding 模型变更 = 重建 collection + alias 原子切换**（ADR-10），不做原地覆盖；
- 入库为独立命令或管理接口 `POST /cs/admin/kb/reload`，**不在服务启动主链路上阻塞**。


### 7.4 混合检索

| 组件 | 实现要点 |
|---|---|
| 中文分词 | 字符 bigram（无词典标准做法）+ 英文/数字整词；**CJK 与 ASCII 边界不组 bigram**（`调用API报500` → `api` / `报` / `500` 各自成 token） |
| BM25 | 平滑 IDF `log(1+(N-n+0.5)/(n+0.5))`（天生非负，无需 epsilon 打地板）+ 长度归一化 `k1=1.5, b=0.75`；**tf 构建期预计算**；**10⁵ 级必须升级为真倒排 postings + 剪枝（WAND/BlockMax）**——全量打分与进程内余弦同样过不了规模（见 ADR-10 实测表） |
| 向量 | 真 embedding（OpenAI 兼容）余弦；无 key → **字面 NGram 替身**（接口不变，`vector_mode` 上报，不隐藏）；chunk 向量**构建期单位化** |
| 融合 | RRF：`score = Σ 1/(k + rank)`，`k=60`；只吃排名不吃分数，免跨量纲归一化 |
| scoped 检索 | 先按分类缩小到目录 chunk 集合；返回时把集合下标映射回全局下标（映射错会导致引用校验误杀） |
| 软路由 | 按分类**得分 > 0 的类别**同时检索多个目录并融合；**规则层 `scores` 必须一路带到检索**（图状态里 `cls_scores` 禁止丢弃——LLM 仲裁后丢分会导致软路由检索空） |
| 向量 ANN（Qdrant） | 单 collection + HNSW；查询携带 `filter`（category / visibility / scope / expire）做**预过滤**；标量量化 int8（可选 on-disk）；同机往返 1~3ms |
| 维度过滤 | 用户提到站点/票种时用 `scope_ref` 收窄候选；未命中时**放宽 filter 重搜一次**并记账，放宽后重算 top1 余弦再判阈值 |
| 精排（rerank） | 融合后 **top-50 → cross-encoder rerank → top-5** 进生成；rerank 不可用则回退融合分并计数（`rerank_fallback_total`） |
| 融合位置 | **RRF 仍在 Go**（确定性、可评测、单一计数点）；不交给 Qdrant，也不把关键词路搬进 Qdrant（ADR-10 被否方案） |
| 形态分派 | 切块与**阈值标定按 `form` 分派**：`qa` 走 `##` 结构切块与 FAQ 阈值；`prose` 走三层回退切块 + 标题路径前缀 + prose 阈值 |
| 子场景过滤 | `sub_scenario` 作为**可选过滤维度**（如用户明确说「候补」→ 只在该子场景内召回）；未声明则不参与过滤，避免过滤过窄 |

### 7.5 相关性阈值（防幻觉第一道闸）

**判定用 query 与 top-1 chunk 的余弦相似度，不用 RRF 分数**（RRF 只保留排名，相关与无关问题分数同区间、区分度极低）。

- 默认阈值 **0.60**（上线前按真实分布标定后写配置）；
- 判定在图内（E13），Go 只提供 `top1_cos` 事实；
- 实现坑：向量检索器若在排序后丢弃原始余弦（只返回 rank），需额外导出 `Cosine`，或在编排层对 top-1 单独 embed——**同一 query 的 Embed 调用点会散开（缓存/检索/阈值各一次），必须在请求内传递复用 query 向量**，否则白花 3 次网络往返；
- **字面替身模式下不允许把"有结果即相关"写死为 true**：无 embedding 时阈值形同虚设 → 无关 chunk 喂给 LLM + "尽量作答"的 prompt = 直接诱发编造。
- **10⁵ 级的两级检索**：先按实例维度（站点/票种）过滤召回；命中不足或用户没提维度时放宽 filter 重搜一次（`filter_relaxed_total`），**放宽后必须重算 top1 余弦再判阈值**（否则阈值判的是被过滤子集的相关性，结论会偏）。
- **ANN 召回后阈值判定依然成立**：Qdrant 只负责「把可能相关的候选拿回来」，「知识库到底覆不覆盖这个问题」仍由 top1 余弦 + 阈值判定（这是防幻觉的第一道闸，不能因为换了索引就省掉）。
- **阈值必须分形态标定**：`prose` 长 chunk 与短问题的余弦天然偏低，与 `qa` 不在同一分布上。**同一阈值套两形态 = 必然误杀 prose**（表现为「站点规则明明有写，却总说没找到」）。做法：分别用两形态的评测样本标定两个阈值（`threshold.qa` / `threshold.prose`），并分形态上报 `below_threshold_total{form}`。

### 7.6 已知坑清单（照抄，别重踩）

| 坑 | 现象 | 修法 |
|---|---|---|
| 切块按字节 | 中文 3 字节/字，`chunkSize=120` 只装 ~40 字，答案被切走 → 命中"问法"chunk 但答案不在 Top-K → LLM 只能拒答 | 一律 `[]rune` 累计 / `utf8.RuneCountInString`，单测锁死 |
| 按长度硬切 | 相邻两个 Q-A 被切进同一 chunk，语义稀释 → 相似度掉到阈值下 → 误判"无有效结果" | 优先按 `## ` 切 |
| 用正则按 `## ` 切 | Go RE2 **不支持 lookahead**，`\n(?=## )` **直接 panic**（不是返回 error） | `strings.Split(s, "\n## ")` 后补回前缀 |
| 分类抖动清空某类检索 | 同问题两次问结果不同 | 规则词表补**稳定锚点词**让规则优先；软路由用"过滤而非加权"时尤其致命 |
| 评测按「文档级」命中算 | 命中率虚高（命中文档里的错误 chunk） | 检索评测按**知识点/chunk 级**打分 |
| prose 长块语义稀释 | 长段落把问题语义摊薄 → 余弦掉到阈值下 → 误判「无有效结果」 | chunk 前拼**标题路径前缀** + 单块上限 400~600 字（rune） |
| prose 块含指代词 | 「如上所述」「见第 3 条」脱离上下文不可理解 → 检索命中却答不出 | 入库 lint 拦截 + 切块时丢弃或改写指代词 |

### 7.7 十万级规模下的检索预算

| 阶段 | 预算 | 说明 |
|---|---|---|
| query embedding | 50~300ms（可缓存/复用） | 一次请求内只算一次（键 = 规范化问题） |
| Qdrant 过滤式 ANN | **6~14ms**（窄过滤 6~8ms / 宽过滤 9~13ms） | HNSW + payload 预过滤；**M0 实测值**（10⁵×1024、`m=16` / `ef_construct=100` / `hnsw_ef=256`，见报告 §4.2 与 §4.4） |
| 自研 BM25（倒排 + 剪枝） | 5~20ms | 不再全量打分 |
| RRF 融合 | <1ms | 纯内存 |
| rerank（top-50 → top-5） | 50~200ms | API 调用；不可用则回退 |
| **检索小计（不含 query embedding）** | **60~230ms** | 仍在 ≤400ms 预算内 |

> query embedding 的调用点会散开（答案缓存 / 检索 / 阈值判定各一次），**必须在请求内传递复用 query 向量**，否则 10⁵ 级下白花 3 次网络往返（见 §7.4 与 11.3）。

### 7.8 场景分类总览（7 类 × 子场景 × 形态 × 系统支撑）

> **支撑口径**：✅ 有数据/功能支撑（工具或系统行为可验证）；⚠️ 部分支撑（能做但要如实声明限制）；❌ **系统当前无此功能**——知识库**不得把它描述成可用能力**，否则等于把幻觉写进权威文档（比模型幻觉更糟：用户照着做会发现没有入口）。详见 19.12。

| 类 | 子场景（`sub_scenario`） | 形态 | 系统支撑 |
|---|---|---|---|
| booking | 普通购票与下单流程 | qa | ✅ 班次/余票/下单链路（工具） |
| booking | 选座与席位（一等/二等/卧铺/无座） | qa | ⚠️ 只有 `seat_number` + `bus_type`/`is_vip`，无席别体系 |
| booking | 特殊票种（学生/儿童/军人/残障优惠） | qa | ❌ 无票种与优惠字段 |
| booking | 团体票 / 企业客户订票 | qa | ❌ 无 |
| booking | **候补购票（排队 / 兑现 / 失败）** | qa | ❌ **系统无候补功能** |
| booking | 联程 / 接续 / 分段买票 | qa | ❌ `routes` 为点对点 |
| booking | 限售、区间限制、开售时间 | qa | ⚠️ 仅 `buses.sale_open_at` 可答 |
| booking | 列车临时调整 | prose | ❌ 无晚点/停运字段 |
| order | 订单查询（待支付 / 已完成 / 已取消） | qa | ✅ 工具 |
| order | 支付问题（扣款成功无订单 / 重复扣款 / 超时） | prose | ⚠️ 幂等与关单链路可解释；渠道侧无对账接口 |
| order | 支付方式规则 | qa | ⚠️ 实际仅 mock + 支付宝沙箱（**微信等未接入，必须如实说**） |
| order | 电子客票 / 报销凭证（打印、补打、电子报销） | prose | ❌ 无发票实体 |
| order | 短信通知 / 行程信息提示 | qa | ❌ 无短信通道 |
| refund | 退票时间窗口与阶梯手续费 | prose（数值走工具） | ✅ `penalties` + 确定性计算工具 |
| refund | 改签业务规则（开车前 / 开车后 / 限制） | prose | ❌ 无改签表 → 规则 + 转人工 |
| refund | 变更到站 | prose | ❌ 无 |
| refund | 停运 / 晚点情形下的退改特殊政策 | prose | ⚠️ 无事件字段，仅政策文本 |
| refund | 退款到账时效 / 退款失败 / 原路退回 | prose | ⚠️ 本地状态可查；渠道侧无接口 |
| refund | 部分退票 / 多人订单部分退改 | prose | ⚠️ 订单为单座位（`orders.seat_id` 单值），多人 = 多订单 |
| account | 注册 / 登录 / 密码找回与修改 | qa | ✅ 现有链路 |
| account | 身份信息核验状态 | qa | ❌ `users` 无证件与核验字段 |
| account | 常用联系人增删与上限 | qa | ❌ 无表 |
| account | 人证不一致 / 证件过期 / 护照与港澳台证件 | prose | ❌ |
| account | 账号锁定 / 风控限制 / 无法购票 | prose | ⚠️ 有 `sessions.is_blocked` + 登录限流 429，无风控体系 |
| account | 手机号变更 / 邮箱绑定 | qa | ❌ `users` 无手机/邮箱 |
| travel | 电子客票检票进出站 | prose | ⚠️ 无检票实体（可讲凭订单/座号乘车） |
| travel | 行李携带（重量 / 尺寸 / 违禁品） | prose | ❌ 纯规则且非本系统管辖 |
| travel | 儿童 / 老人随同乘车 | prose | ❌ 无票种 |
| travel | 宠物携带 | prose | ❌ |
| travel | 重点旅客服务（轮椅 / 担架 / 爱心） | prose | ❌ |
| travel | **遗失物品登记与查找** | prose | ❌ 无失物表 → 流程 + 转人工 |
| travel | 晚点 / 停运 / 临时变更通知 | prose | ❌ |
| travel | 候车 / 中转 / 站内换乘 | prose | ⚠️ `terminals` 有点位信息 |
| app | 页面报错 / 接口异常 | qa | ✅ 可映射真实错误码（401 / 403 / 409 / 429） |
| app | 验证码 / 图形码收不到 | qa | ❌ 无验证码机制 |
| app | 推送消息异常 | qa | ❌ 无推送通道 |
| app | 下载与版本（**本项目为 Web，无 APP**） | qa | ⚠️ 需改写为「浏览器兼容 / 缓存问题」（有 no-cache 策略可讲） |
| policy | 投诉渠道与反馈提交 | qa | ✅ 工单系统（**投诉行为走前置意图直接转人工**，不经检索） |
| policy | 活动规则 | qa | ⚠️ 无活动实体 |
| policy | 会员积分（获取 / 兑换） | qa | ❌ 无会员体系 |
| policy | 政策公告 | prose | ⚠️ 可用 `notice` + `expire_at` 承载 |

**统计（按「系统支撑度」）**：子场景 41 项 —— ✅ 完整支撑 **6** 项、⚠️ 部分支撑 **13** 项、❌ 当前无功能 **22** 项。

**台账口径映射**（`kb/capability.yaml`）：`supported` **19** 项（= ✅ 6 + ⚠️ 13，后者在 `note` 里写明限制，如「支付仅 mock + 支付宝沙箱」）、`roadmap` **12** 项（计划落地，M5 分批）、`industry` **10** 项（行业通行、本平台无计划）。两者必须保持一致——`kb/_lint.py` 会校验每个文档的 `capability` 与台账 `status` 是否相同。

**B 方案下这 22 项 ❌ 怎么处理（已拍板，见 19.12）**：知识库定位为**铁路客运行业知识库**，所以 ❌ 项**照写**（行业规则、办理流程、时限、常见口径），但：

- 其 `capability` 标为 `roadmap`（计划落地）或 `industry`（行业通行、本平台无计划）；
- **生成层必须加边界声明**，禁止讲成本平台能力（机制见 **5.9**）；
- 台账里同时记录「落地需要什么」，构成功能待办（路线见 **7.9**）。

换言之：**同一份知识库，两种措辞**——平台已实现的按「本平台…」讲，未实现的按「行业一般做法…（本平台暂未开放）」讲；状态翻转只改措辞层，不重嵌。

### 7.9 能力台账与功能演进路线（B 方案的配套）

**能力台账 = 单一事实来源**（`kb/capability.yaml`，入库时同步为表 `cs_capabilities`）：

| 字段 | 说明 |
|---|---|
| `sub_scenario` | 41 项子场景 ID |
| `status` | `supported` / `roadmap` / `industry` |
| `system_entry` | 本平台入口（前端路径 / 接口 / 工具名）；**`supported` 时必填**（lint 机检） |
| `owner` / `target` | 负责人与目标里程碑（`roadmap` 必填） |
| `source` | 行业内容的权威来源（《铁路旅客运输规程》《铁路旅客运输办理细则》、12306 公告、车站公告）——**行业文档必须有出处**，否则 10⁵ 级规模无法审计 |
| `note` | 限制说明（如「支付仅 mock + 支付宝沙箱」） |

机制：台账 → 入库快照进 chunk 的 `capability`；台账变更只触发 **payload 更新**（不重嵌）；状态翻转由一次命令完成（`kb-sync --refresh-capability`）。

**功能演进路线（22 项 ❌ 正好是一份功能待办，按 ROI 分三批）**：

| 批次 | 功能 | 需要什么 | 知识库状态翻转 |
|---|---|---|---|
| **第 1 批**（低成本、直接提升客服覆盖率） | 常用联系人、身份/证件字段与核验状态、手机号与邮箱绑定 | `contacts` 表 + `users` 加证件与核验字段 + CRUD 接口 | `industry → supported`（措辞自动切换） |
| **第 2 批**（中等） | 遗失物品登记与查找、报销凭证、短信/推送通知 | `lost_items` 表（与 `support_tickets` 联动）、`invoices` 表（接支付宝开票）、通知通道 | 同上 |
| **第 3 批**（重，但最能延伸现有卖点） | **候补购票**、联程/接续/分段、变更到站、会员积分 | 候补：`waitlist` 表 + 状态机（排队/兑现/失败）+ **与座位释放联动**；联程：`routes` 图化 + 分段计价；积分：`points` 表与订单联动 | 同上 |

**顺序建议（理由）**：优先做**能复用现有并发/事务链路**的那批——**候补购票的本质，是把「条件更新的裁决权」从「单个座位的一次抢占」扩展到「候补队列的有序放行」**，与本项目已有的 Redis 预占 + MySQL 条件更新 + 延迟关单是同一套机制的直接延伸（技术含金量最高）。而联系人/证件是纯 CRUD，收益在客服覆盖率而非技术深度；变更到站/联程牵扯既有订单模型（`orders` 目前是单座位单订单），应排最后。

---

## 8. 工具层（Go 执行 / Python 决策）

### 8.1 工具注册表

| # | 工具 | 类型 | 数据源 | 提槽（正则优先） | 输出（脱敏后） |
|---|---|---|---|---|---|
| 1 | `my_tickets` | 读 | `tickets ⋈ seat_reservations ⋈ bus_seats ⋈ buses ⋈ routes ⋈ terminals`（复用 `internal/db/query/tickets.sql:47`） | "我的票""我买的票""我订的哪趟" | 班次时刻、座号、票价、状态 |
| 2 | `order_detail` | 读 | `orders`（`GetOrderByNo`） | 订单号（UUID）；无号 → 最近订单消歧 | 订单号、金额、状态、渠道、时间 |
| 3 | `unpaid_orders` | 读 | `orders WHERE status='pending' AND expired_at > NOW()` | "没付款""还要不要付""过期" | 待支付订单 + **剩余支付时间** |
| 4 | `bus_availability` | 读 | `routes ⋈ buses ⋈ bus_seats`（余票动态聚合）+ `sale_open_at` 校验 | 出发站/到达站/日期（地名需消歧） | 班次、时刻、票价、余票数、是否已开售 |
| 5 | `refund_fee` | 读（计算） | `penalties` + `buses.departure_time/price` | 订单号/车票号；无号 → 最近可退车票 | 距发车小时数、适用 `percent`、手续费、**实退金额** |
| 6 | `refund_progress` | 读 | `orders`（`status='refunded'` / `pay_channel`） | 订单号 | 本地退款状态；渠道侧无接口（见 8.4） |
| 7 | `create_support_ticket` | 写（可追踪） | `support_tickets` | 会话上下文（摘要 + 关联订单号） | 工单号 + 标准话术 |

**注册顺序即优先级**（精确触发先于宽泛触发，`order_detail` 先于 `my_tickets`）。多候选时由编排层触发 `clarify`（interrupt 反问），**不猜**。

**决策/执行分离**：`route_tool`（Go，正则提槽）给候选；提槽未命中时 Python 侧 LLM 只决定**调哪个工具名**（须过注册表白名单校验），**参数槽位仍由 Go 的确定性抽取填充**——LLM 决定"调哪个"，不决定"查什么"。

### 8.2 退票费确定性计算（本项目含金量点）

**原则：金额不让 LLM 算。** 手续费与实退金额由确定性函数计算，LLM 只负责把结果组织成人话（甚至不经 LLM，直接模板输出）。

```
输入：订单/车票 → bus_id、amount（取自 orders.amount）、buses.departure_time
规则：penalties(bus_id) 行集合
输出：{ hours_before_departure, matched_rule, percent, fee, refundable_amount }
```

**边界（必须处理）**：
1. **规则行语义待业务确认**（见 19.2）：现列为 `actual_hours_before` / `hours_before` / `percent` / `custom_text`。文档按"距发车 `hours_before` 小时（含）以前退票扣 `percent`%"实现；**若语义不同 → 该工具直接下线**，退票费问题走知识库 FAQ + 转人工：**宁可转人工，不可猜金额**。
2. 同 `bus_id` 多行规则按阈值排序取首个满足项；无规则行 → 不下结论，转人工。
3. 车票状态非 `purchased`（未支付/已退）→ 不计算，说明当前状态。
4. 已过发车时间 → 不计算手续费，按"误车"话术 + 转人工。
5. `custom_text` 存在时**原文透传**（车站自定义说明常含例外条款）；与计算值冲突时以 `custom_text` 为准并提示。

### 8.3 余票查询

余票 = `bus_seats.status='available'` 聚合（动态），与前端线路查询同源；缓存键复用 `cache.RoutesQueryCacheKey`（`internal/cache/redis.go:83`），下单/支付/退票后由现有 `invalidateRoutesCache` 失效。**客服工具与前端共用同一缓存**——另建一套会导致余票数字前后不一致。

### 8.4 工具能力边界（诚实清单）

| 用户可能问 | V1 能否答 | 原因 |
|---|---|---|
| 我的订单/车票/余票/票价/退票费 | ✅ 工具直查 | 有表支撑 |
| 改签 | ⚠️ 仅知识库规则 + 建单 | **无改签表** |
| 发票/报销 | ⚠️ 仅知识库 FAQ | 无发票实体与开票流程 |
| 渠道侧退款到账时间 | ⚠️ 仅本地状态 + 知识库说明 | `payment.Provider` 只有 `CreatePayment/QueryOrder/VerifyNotify/Refund`，**无退款查询接口**（`internal/payment/provider.go:26`） |
| 催付/营销提醒 | ❌ 不做 | 无营销策略表 |
| 行李/儿童票/证件 | ⚠️ 仅知识库 | 规则性内容，无数据 |

### 8.5 提槽、校验与身份（安全硬线）

1. 工具入参**没有 `user_id` 这一项**——`user_id` 由 Go 从 JWT 解析后注入（对齐 `internal/api/handlers/ticket.go:108` 的 `currentUser` 模式），编排层只是**中转**它（收到的 `user_id` 来自 Go，图内不得自行解析）。
2. 每条 SQL 强制 `WHERE user_id = ?`；**跨用户查询统一返回"未查询到"**（不返回 403，不泄露订单存在性）。
3. 游客态调用个人工具 → 引导登录（不返回空结果混淆视听）。
4. 统一：`context` 超时（1.5s）+ 错误归类（**工具不可用单独计数**）+ **出口脱敏**。
5. 必测矩阵（M2）：本人查得到 / 他人 token 查 0 条 / 无 token 引导登录 / 伪造 token 401 / 越权订单号统一"未查询到"。

---

### 8.6 实现补充（M2 落地口径，回写自 `docs/M2-实现报告.md`）

以下是 M2 实现期定下来的**机制级口径**，后续阶段改这些等于改契约，需同步改两侧代码与测试。

**① 提槽是"Go 抽、Python 原样回传"，不允许两层各抽一遍。**
`/internal/tools/route` 返回候选时**已带好槽位**（`slots` 字段，含订单号/车票号/站名/日期与 `missing`/`ambiguous`），编排层把它原样回传到 `/internal/tools/:name` 的 `args`。理由：参数口径只有一处真相，跨语言重复实现必然漂移。

**② 多候选"不猜"，但先用分类破平。**
同分候选里恰好一个与分类亲和（`order→order_detail/my_tickets/unpaid_orders`、`refund→refund_fee/refund_progress`、`booking→bus_availability`）时，Go 标 `preferred`，编排层据此直接执行；**只有仍无法区分才反问**（E8）。实测教训：「退票手续费怎么算，订单号 xxx」若一律反问，会把一个完全可答的问题挡在门外。

**③ 时间比较一律下沉 SQL（时区纪律）。**
应用 DSN 用 `loc=Asia/Shanghai`，而 MySQL 容器会话默认 UTC ⇒ **同一列"在哪比较"结果不同**。因此：待支付的"未过期"用 `expired_at > NOW()`、剩余时间用 `TIMESTAMPDIFF(MINUTE, NOW(), expired_at)`，都由数据库算。任何"把 DB 时间取回 Go 再和 `time.Now()` 比"的写法都是隐患（M2 实测踩过：15 分钟后到期的订单被判成已过期）。

**④ 工具结果用 `kind` 六分类表达；转人工路径由 Go 用 `path_hint` 给定。**

| `kind` | 语义 | 编排层走向 |
|---|---|---|
| `ok` | 查到数据 | E9 确定性直答（**不经 LLM**） |
| `empty` / `not_found` | 查询成功但无此数据；**越权与不存在同一话术** | 直答（不泄露存在性） |
| `slots_incomplete` | 缺参数或地名歧义 | 单轮反问（E8-M2 版），**不建单** |
| `guest_required` | 游客调个人工具 | 引导登录，**不建单**、不算转人工 |
| `blocked` | 业务规则不允许（误车/状态不对/无规则档位） | 转人工（`path_hint=transfer_deterministic`），独立计数 |
| `unavailable` | 数据源异常/超时/熔断/闸门关闭 | 回落检索（E10）→ 仍无依据则按 `path_hint` 转人工 |

**⑤ 工具结果是事实类就模板直答，不交给模型改写。**
金额、订单状态、时刻都是可核对事实；让 LLM 复述只增加出错面（与 §8.2「金额不让 LLM 算」同一条纪律）。

**⑥ 转人工的建单也走工具注册表**（`create_support_ticket`，`Match` 恒 0，只由转人工路径按名调用）。好处：游客策略、白名单、熔断、计数四处治理只有一套。

**⑦ 两道闸门的默认值与翻转方式。**

| 闸门 | 默认 | 默认下的行为 | 翻转 |
|---|---|---|---|
| 19.2 `penalties` 语义 | **关闭** | `refund_fee` 返回 `unavailable(penalty_semantics_unconfirmed)` → 走知识库 FAQ **+ 转人工** | 确认报告 §3 的 12 行样例表后 `PENALTY_SEMANTICS_CONFIRMED=1` |
| 19.3 游客建单 | **允许**（沿用 M1 已验收行为，启动打告警） | 游客也能拿到可追踪工单号 | `GUEST_TICKET_ALLOWED=0` → 改为"请先登录后转接" |

**⑧ 熔断与超时参数（待压测标定）**：单工具 1.5s 超时；连续 3 次失败打开熔断，冷却 30s 后放一个试探请求（半开）。

---

## 9. 会话、记忆与缓存

### 9.1 三层数据职责

| 层 | 载体 | 内容 | 谁写 |
|---|---|---|---|
| 图状态（真相源） | checkpoint 表 | 会话上下文（Recent/Summary/LastAnswer）、决策中间结果、中断 | Python |
| 审计流水 | `cs_messages` | 每轮问答 + 分类 + 来源 + trace_id | Go |
| 答案缓存 | Redis | 同义问题的答案 | Go |

**Redis 不存会话状态**（ADR-9）：避免"Redis 与 checkpoint 谁说了算"的不一致。

### 9.2 会话上下文（图状态内）

- **双层窗口**：最近 5 轮完整保留（精确指代消解靠它）；第 6~15 轮压成**确定性模板摘要**（`问题(分类)；…` 最多 10 条，**不调 LLM**）；第 16 轮起丢最旧；
- 消费点两处同步适配：指代消解 prompt（分"更早摘要"+"最近对话"）、工单摘要（Summary 置首）；
- **游客 → 登录不迁移**（`conv_id`/锚点不同天然隔离，防串号）；`DELETE /cs/session` 清 Redis 侧缓存 + 标记会话结束，`cs_messages` 与 checkpoint 按保留策略清理；
- checkpoint 不可用 → 本轮退化为**无状态执行**（等价单轮），记告警，不阻塞。

### 9.3 答案语义缓存（Go）

| 项 | 规则 |
|---|---|
| 键 | `cs:ac:{sha1(消解后规范问题)}:{category}`（**不是原始输入**，消解后与用户无关才共享安全） |
| 匹配 | 问题向量**余弦 ≥ 0.95**（远高于检索阈值 0.60），同分类内匹配防跨类误命中 |
| 准入 | 同一问题**第 2 次命中**才正式入缓存（首次进候选池），避免长尾白占内存 |
| 淘汰 | TTL 滑动过期 + LRU 上限（**LRU 用单调递增 `seq` 排序，不用时间戳**——同 tick 时间戳会让淘汰不确定，单测 flaky） |
| **不入缓存** | 个性化（含订单号/车票号）、工具直答、转人工结果、问候/拒答兜底 |
| 查询时机 | **图内节点**（`cache_lookup`），不用 Go 在转发前短路——否则该轮不进 checkpoint，下一轮的指代消解会缺上下文 |

### 9.4 多轮指代消解（Python，三层）

1. **确定性继承**：分类落 `other` + 含指代词（那/它/这个/还有/呢/同样/也）+ 有历史 → 继承上一轮类别，并用上一轮问题作为检索上下文；**继承时补 `cls_scores`**（否则软路由检索空）。
2. **选项选择**：用户回"1"/"第一个"→ 从 `LastAnswer`（客服上一条回复）提取选项；**数字/极值走确定性**（"最贵那单"用真实数据排序，不让 LLM 比金额）。
3. **LLM 兜底**：复杂指代（"那你觉得呢"）交 LLM 改写，**只改写不回答**，答案仍走防幻觉生成。
4. **必须带会话上下文才触发改写**：无历史的纯指代/无效输入不触发（否则 LLM 只会瞎猜）。

---

### 9.5 实现补充（M3 落地口径，回写自 `docs/M3-实现报告.md`）

M3 落地后新增/确认的口径（每条都是实测踩坑换来的，改之前先看理由）：

1. **`cache_lookup` 是图内节点，不做 Go 转发前短路**：短路的那一轮不进 checkpoint，下一轮的指代消解会缺上下文。
2. **缓存命中仍然要过 `verify`**：缓存省的是**检索+生成**，不是合规检查。命中直连 `finalize` 会让能力边界声明整段消失（实测：M1 回归 `候补购票怎么操作` 判"缺边界声明"）。命中时用缓存条目里的 `source_labels` 复算边界（Go 的边界判定只依赖来源 chunk 的 `capability`），并把声明注入做成**幂等**（答案已带声明则不重复拼）。
3. **记忆字段不能出现在 `new_initial_state()` 里**：入口把它当输入传给 `ainvoke`，langgraph 会视作一次 state 更新 → 写 `recent=[]/summary=""` 等于每轮清空记忆。记忆由 `load_context` 从 checkpoint 读、`finalize` 写回；M1 的 append-only `history` 弃用（无上限撑大 checkpoint）。
4. **E11 改写结果必须机检**（`_valid_rewrite`）：非空、是短问句（≤60 字）、不含引用标记/答案话术（"抱歉/未找到/暂时没有"）、确实与原句不同。**被拒直接转人工**（新增 `rewrite_failed` + 条件边 `rewrite → [classify_rule|transfer]`）——空跑第二遍会把指代继承来的分类覆盖回 `other`。
5. **确定性拒绝不计熔断**：`penalty_semantics_unconfirmed`（19.2 闸门关闭）等"业务上不下结论"的结果进 `deterministicUnavailable` 白名单，只计 `tool_gate_closed_total`、不 `br.fail`。否则连问同一条未确认规则会打开熔断，返回原因从 `penalty_semantics_unconfirmed` 漂成 `circuit_open`，**坐席看到的判定原因失真**。
6. **SSE 出口由 Go 规范化**：① 上游 CRLF → LF；② **空行是帧分隔符，必须转发**（丢掉它客户端永远拼不出完整帧：`curl` 看着有内容、任何按 `\n\n` 切帧的解析器读到空）；③ `stream` 是 opt-in（编排层默认 `False`，Go 显式传 `false`），显式声明避免"默认值事故"（旧默认 `True` 曾导致 Go 非流式分支拿到 SSE 正文 → 每轮静默降级成工单）。
7. **`/cs/ask` 的降级路径必须永不崩**：所有截断走 `clampRunes()`（按 rune，不按字节）。实测：`question[:500]` 在 84 字节输入上 panic 直接把 Go 进程打崩——"编排层不可用"不该升级成"客服整体不可用"。
8. **`PYTHON_BASE_URL` 未配置要报 ERROR**：它没配 = 每一轮都降级建单（用户只会拿到工单号）。启动日志必须显式报警，禁止静默降级。

## 10. 转人工与客服工单

### 10.1 四条路径（分别计数，口径不一致就无法定位问题）

| 路径 | 判定点 | 归属 |
|---|---|---|
| ① 确定性（投诉/明确要人工） | `pre_intent` | Go 词表 + Python 边 E1 |
| ② 阈值不过 / 检索空 | `top1_cos < 阈值` 或 检索空且不可重试 | Python 边 E12/E13（事实来自 Go） |
| ③ 模型标记 / 引用校验失败 | `[TRANSFER]` 或 无有效出处 | **LLM 只输出结构化标记**（不许自由发挥转人工话术）+ Go 校验 |
| ④ 工具不可用（含编排层不可用） | 工具超时/数据源异常/编排层 5xx | Go 计数 |

四条路径**全部走工单系统**——漏任何一条，用户拿到的"转人工"就不可追踪。

### 10.2 工单状态机与建单

```
pending 待处理 → assigned 已分配 → resolved 已解决 → closed 已关闭
```

| 字段 | 说明 |
|---|---|
| `ticket_no` | 对外工单号（如 `CS20260912-0001`），话术回带 |
| `conv_id` / `user_id` | 会话与身份锚定（`user_id` 由 Go 注入） |
| `order_no` | 关联订单（可空，从上下文抽取） |
| `category` | 分类（分流与统计） |
| `path` | 触发路径（①~④，用于口径核对） |
| `summary` | 问题摘要（**确定性模板拼装**：分类 + 最近 1~2 轮问题 + 关联订单号，不依赖 LLM 总结） |

**状态约束**：`closed` 工单拒绝再回复/再分配；状态变更走**条件更新**（`WHERE status='xxx'`），影响行数为判定依据（照抄现有支付/关单链路纪律）。

### 10.3 话术

- 建单成功：`已为您转接人工客服，工单号 {ticket_no}，坐席会尽快与您联系。`
- 游客：`为保护您的订单信息，请先登录后我为您转接人工客服。`（是否允许游客建单见 19.3）
- 编排层不可用：与建单成功同话术（路径④），保证**用户永远拿到可追踪工单号**。

### 10.4 坐席工作台（V1 不做，接口预留）

V1 只到"工单落库 + 用户查自己的工单 + 管理端查全量"。坐席接待台、排队、接管、移交属后续迭代；预留 `support_tickets.assigned_to` 列与 `POST /cs/admin/support-tickets/:no/assign` 空实现，避免后续改表。

---

## 11. LLM 网关与模型接入

### 11.1 网关职责（Go）

| 项 | 设计 |
|---|---|
| 端点 | `POST /internal/v1/chat/completions`（OpenAI 兼容，含 `stream: true`） |
| 多渠道路由 | 按配置选择供应商/模型（chat 与 embedding 分开配置） |
| 配额与限流 | 按 `trace_id`/会话聚合 token 预算；超限返回 `budget_exceeded` |
| 记账 | `tokens_prompt/completion`、`llm_calls_total`（**由 usage 钩子驱动**）、`usage_unknown_total`（供应商未返回 usage 时**不用 0 冒充**） |
| 超时/重试 | 单次 20s；非流式可重试 1 次（幂等）；流式不重试 |
| 密钥 | 只存在于 Go 侧配置；Python 容器无模型密钥 |

### 11.2 模型选型

| 用途 | 选型 | 关键参数 |
|---|---|---|
| 生成（默认） | 快模型 + **关闭思考**（如 `kimi-k2.6` 关思考） | `temperature=0.6`（关思考时；开思考才 1.0）；首字 ~1.7s |
| 分类兜底 / 跨类仲裁 | 同上（短 prompt、小 `max_tokens`） | 输出闭集 |
| 指代消解 / 问题改写 | 同上 | 只改写不回答 |

**硬约束**：
- **reasoning 模型不能当客服默认生成模型**：先吐思维链才出正文，简单问答也"思考"10~30s；且流式只吐 `delta.content`，思考阶段前端收不到任何 token → 用户干等"正在思考…"，会被误判成"检索慢/没流式"。
- reasoning 适配三件套（若显式切开思考）：`content` 空时回退 `reasoning_content`；`max_tokens` 给足（≥4096）；超时放宽。

### 11.3 Embedding

| 项 | 方案 |
|---|---|
| 首选 | BGE-M3（硅基流动，中文客服场景免费额度充足） |
| 备选 | Qwen 系列 embedding（部分模型**不在免费额度内**；报 `Arrearage` 时先查免费额度页该模型能否开启，别误判账号欠费） |
| 不可用 | Kimi/DeepSeek **无 embedding 接口**；不能拿 chat key 当 embedding key（会静默降级且日志误报"语义检索: 是"） |
| 降级 | 无 key → 字面 NGram 替身（接口不变，`vector_mode=ngram` 上报，不隐藏） |
| 超时 | `http.Client.Timeout = 10s`（正常单条 ≤2s；30s 会让偶发慢响应拖垮整个请求） |
| 缓存 | 路径必须落**可写卷**（容器里 `kb/` 挂 `:ro` 会让缓存完全失效 → 每次查询实时调 API → 前端"一直思考"） |
| 换模型 | `embedding_model` 落库并检索前校验；**换模型必须整库重嵌**（向量空间不通，即使维度相同也会让余弦全错） |

### 11.4 密钥治理（⚠️ 现状已有问题，必须一并处理）

**现状证据**：`app.env` **已被 git 跟踪**（`git ls-files` 输出含 `app.env`），`.gitignore` 当前只有 `Makefile / .idea / tmp` 三行 ⇒ 其中的支付宝沙箱私钥**已经入库**。新增 `LLM_API_KEY` / `EMBEDDING_API_KEY` / `CS_INTERNAL_KEY` 若沿用同一文件，等于把新密钥一起提交。

**治理要求（顺序不能颠倒）**：
1. **先改 `.gitignore`，再往文件里写密钥**；规则成对：
   ```gitignore
   app.env
   *.env
   !*.env.example
   ```
2. 提供 `app.env.example` 占位模板入库，每项注明"留空＝降级成什么"；
3. `app.env` 出库（`git rm --cached app.env`）；**已泄露的支付宝沙箱私钥必须轮换**（沙箱同理）；
4. 启动日志 / `/readyz` / 指标 / 前端静态资源**一律不打印密钥值**，只报布尔（`llm: true` / `embedding: true` / `orchestrator: true`）；
5. 验证用 `git add --dry-run`（`git check-ignore` 对否定规则有歧义，会误报"模板被忽略"）。

---

## 12. 接口规范

### 12.1 对外（买家侧，Go 实现）

| 方法 | 路径 | 鉴权 | 说明 |
|---|---|---|---|
| POST | `/cs/ask` | 可选（游客可问公共问题） | 问答主入口；`?stream=1` 走 SSE（`meta` → `delta` → `interrupt`? → `done`） |
| GET | `/cs/conversations` | ✅ | 登录用户会话列表（读 `cs_messages` 分组） |
| GET | `/cs/conversations/:id/messages` | ✅ | 会话消息（**只查自己的**，`conv_id` 归属校验） |
| DELETE | `/cs/session` | ✅ | 结束会话（清缓存 + 标记结束） |
| POST | `/cs/feedback` | ✅ | 凭 `trace_id` 赞/踩（幂等去重） |
| POST | `/cs/support-tickets` | ✅ | 主动转人工（等价路径①） |
| GET | `/cs/support-tickets` | ✅ | 我的工单列表/详情 |

**请求示例**

```json
POST /cs/ask
Authorization: Bearer <jwt>        // 可选
{
  "question": "我昨天买的票退票要扣多少手续费",
  "conv_id": "8f1c...uuid",
  "device_id": "web-xxx"           // 游客锚点（Go 侧只存 hash）
}
```

**响应示例（非流式）**

```json
{
  "conv_id": "8f1c...uuid",
  "trace_id": "0f6a...",
  "answer": "您最近这张 8月20日 10:30 出发的票，距发车还有 36 小时，按该班次规则收 10% 手续费……",
  "category": "refund",
  "source": "tool",
  "sources": [{"label": "refund/refund-fee-rule·段落1", "doc_id": "refund-fee-rule"}],
  "transfer": false,
  "support_ticket_no": null,
  "degraded": {"vector": true, "llm": false, "orchestrator": false},
  "vector_mode": "ngram",
  "stage_ms": {"classify": 2, "tool": 18, "retrieve": 0, "generate": 1720, "total": 1810}
}
```

**SSE 事件协议**

```
event: meta      data: {"trace_id":"...","category":"refund","source":"tool"}     ← 分类完成即下发
event: delta     data: {"text":"距发车还有 36 小时"}
event: interrupt data: {"payload":{"question":"您说的是哪一趟？","options":[...]}}  ← 消歧反问（可选）
event: done      data: {"answer":"<最终答案>","sources":[...],"transfer":false,"citation_dropped":0}
```

> `degraded` 是**诚实字段**，语义 = 「该能力**是否降级**」：`vector: true` 表示走了字面替身、`llm: true` 表示模型不可用已走兜底、`orchestrator: true` 表示编排层不可用已由 Go 直接建单。**静默降级是最坏的一类故障**——任何降级都必须让调用方看得见（`vector_mode` 另作独立字段供观测）。

### 12.2 管理 / 运维侧（Go）

| 方法 | 路径 | 鉴权 | 说明 |
|---|---|---|---|
| GET | `/metrics` | Bearer（管理 token）或仅内网 | Prometheus 文本格式 |
| GET | `/readyz` | 无（只报布尔） | `{db, redis, llm, embedding, kb_chunks, orchestrator}` |
| GET | `/cs/admin/overview` | 管理 token | 指标摘要（分类分布 / 转人工四路径 / 缓存命中 / 工具命中） |
| POST | `/cs/admin/kb/reload` | 管理 token | 触发知识库增量入库 |
| GET | `/cs/admin/support-tickets` | 管理 token | 全量工单查询 |

**纪律**：管理面**独立端口 + 默认只监听 `127.0.0.1`**；`ADMIN_ENABLED=1` 但未配置 token 时**拒绝启动**，不允许"降级放行"。

### 12.3 错误码

| 码 | 场景 |
|---|---|
| 400 | 参数缺失/非法（`question` 空、`conv_id` 格式错） |
| 401 | 需要登录的接口无 token / token 无效 |
| 403 | 越权（**仅用于非数据类越权**；订单/车票越权统一 200 + "未查询到"） |
| 429 | 限流（带 `Retry-After`） |
| 503 | 基础设施整体不可用且无法降级（**正常情况应降级为转人工而非 503**） |

---

## 13. 数据模型

### 13.1 Go 侧迁移 `000005_add_cs_tables`（业务表）

```sql
-- 知识库文档（文本与元数据 = 真相源）
CREATE TABLE kb_documents (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    doc_key VARCHAR(128) NOT NULL UNIQUE,
    title VARCHAR(255) NOT NULL,
    category VARCHAR(32) NOT NULL,                    -- booking|order|refund|account|travel|app|policy
    doc_type VARCHAR(32) NOT NULL,                    -- policy|howto|faq|notice
    visibility VARCHAR(16) NOT NULL DEFAULT 'public', -- public|authenticated
    form VARCHAR(16) NOT NULL DEFAULT 'qa',           -- qa|prose（切块与阈值分派）
    sub_scenario VARCHAR(64) NULL,                    -- 41 项子场景（不进路由）
    capability VARCHAR(16) NOT NULL DEFAULT 'industry', -- supported|roadmap|industry（来自能力台账 7.9）
    source VARCHAR(512) NULL,                         -- 行业内容的权威来源（行业文档必填，可审计）
    scope_kind VARCHAR(16) NOT NULL DEFAULT 'general',-- general|station|ticket_type|route
    scope_ref VARCHAR(64) NULL,                       -- 站点/票种/线路标识（实例化维度）
    expire_at DATETIME NULL,
    content_hash CHAR(64) NOT NULL,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    INDEX idx_kb_docs_category (category, doc_type),
    INDEX idx_kb_docs_scenario (sub_scenario),
    INDEX idx_kb_docs_scope (scope_kind, scope_ref)
);

-- 知识块（**文本与元数据 = 真相源**；向量不落 MySQL，见 ADR-10）
CREATE TABLE kb_chunks (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,             -- 稳定 ID = Qdrant point id（幂等 upsert）
    doc_id BIGINT NOT NULL,
    seq INT NOT NULL,
    source_label VARCHAR(255) NOT NULL,               -- "refund/refund-fee-rule·段落1"
    content MEDIUMTEXT NOT NULL,
    category VARCHAR(32) NOT NULL,
    doc_type VARCHAR(32) NOT NULL,
    form VARCHAR(16) NOT NULL DEFAULT 'qa',           -- qa|prose：切块与阈值按形态分派
    sub_scenario VARCHAR(64) NULL,
    capability VARCHAR(16) NOT NULL DEFAULT 'industry', -- 检索过滤 + prompt 分区（5.9）
    visibility VARCHAR(16) NOT NULL DEFAULT 'public',
    scope_kind VARCHAR(16) NOT NULL DEFAULT 'general',
    scope_ref VARCHAR(64) NULL,
    heading_path VARCHAR(255) NULL,                   -- prose 的标题路径前缀（对抗语义稀释）
    expire_at DATETIME NULL,
    embedding_model VARCHAR(64) NULL,                 -- 与 Qdrant payload 同步，用于漂移对账
    content_hash CHAR(64) NOT NULL,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    UNIQUE KEY uq_kb_chunk (doc_id, seq),
    INDEX idx_kb_chunks_category (category, visibility),
    INDEX idx_kb_chunks_scope (scope_kind, scope_ref),
    FOREIGN KEY (doc_id) REFERENCES kb_documents(id) ON DELETE CASCADE
);

> **Qdrant 侧**：单 collection（`kb_current` 为 alias）；point payload 镜像上表字段
> （`chunk_id` / `content_hash` / `embedding_model` / `category` / `form` / `sub_scenario` / `capability` / `visibility` / `scope_kind` / `scope_ref` / `expire_at` / `source_label`），
> **检索过滤只用 payload，不 JOIN MySQL**；payload 索引建在 `category` / `form` / `sub_scenario` / `capability` / `visibility` / `scope_kind` / `scope_ref` / `expire_at` 上。
> 台账状态翻转时**只 `set_payload` 更新 `capability`**（不重嵌、不动向量）——这是 5.9「只改措辞层」的落地依据。
> 向量不落 MySQL：重建的输入是"文本 + 当前 embedding 模型"，备份向量没有意义（ADR-10）。


-- 能力台账（7.9：41 子场景 × 状态；生成层据此决定是否加边界声明）
CREATE TABLE cs_capabilities (
    id INT AUTO_INCREMENT PRIMARY KEY,
    sub_scenario VARCHAR(64) NOT NULL UNIQUE,
    category VARCHAR(32) NOT NULL,
    status VARCHAR(16) NOT NULL CHECK (status IN ('supported','roadmap','industry')),
    system_entry VARCHAR(255) NULL,                  -- supported 必填：前端路径/接口/工具名
    owner VARCHAR(64) NULL,
    target_milestone VARCHAR(32) NULL,               -- roadmap 必填（M5-x）
    source VARCHAR(512) NULL,                        -- 行业权威来源
    note VARCHAR(512) NULL,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    INDEX idx_caps_status (status)
);

-- 客服会话
CREATE TABLE cs_conversations (
    id CHAR(36) PRIMARY KEY,                          -- = conv_id = 图 thread_id
    user_id INT NULL,
    guest_key CHAR(64) NULL,                          -- 游客 device_id 的 hash（不存明文）
    title VARCHAR(255) NOT NULL,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    INDEX idx_cs_conv_user (user_id, updated_at),
    INDEX idx_cs_conv_guest (guest_key, updated_at),
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
);

-- 对话流水（审计真相源；图状态在 checkpoint，见 ADR-9）
CREATE TABLE cs_messages (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    conv_id CHAR(36) NOT NULL,
    role VARCHAR(16) NOT NULL,                        -- user|assistant|system
    content MEDIUMTEXT NOT NULL,
    category VARCHAR(32) NULL,
    source VARCHAR(16) NULL,                          -- rule|llm|tool|cache|transfer|fallback
    trace_id CHAR(36) NOT NULL,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    INDEX idx_cs_msg_conv (conv_id, created_at),
    INDEX idx_cs_msg_trace (trace_id),
    FOREIGN KEY (conv_id) REFERENCES cs_conversations(id) ON DELETE CASCADE
);

-- 客服工单（人工转接）
CREATE TABLE support_tickets (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    ticket_no VARCHAR(32) NOT NULL UNIQUE,
    conv_id CHAR(36) NULL,
    user_id INT NULL,
    order_no VARCHAR(64) NULL,
    category VARCHAR(32) NOT NULL,
    path VARCHAR(32) NOT NULL,      -- deterministic|threshold|model|tool_unavailable|orchestrator_unavailable
    status VARCHAR(16) NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending','assigned','resolved','closed')),
    summary TEXT NOT NULL,
    assigned_to INT NULL,                             -- 为坐席工作台预留
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    resolved_at DATETIME NULL,
    INDEX idx_st_status (status, created_at),
    INDEX idx_st_user (user_id, created_at),
    FOREIGN KEY (conv_id) REFERENCES cs_conversations(id) ON DELETE SET NULL,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE SET NULL
);

-- 答案反馈
CREATE TABLE cs_feedback (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    trace_id CHAR(36) NOT NULL,
    user_id INT NULL,
    rating VARCHAR(8) NOT NULL,                       -- up|down
    comment VARCHAR(512) NULL,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE KEY uq_cs_feedback (trace_id, user_id),    -- 幂等去重
    INDEX idx_cs_fb_rating (rating, created_at)
);
```

**迁移纪律**：`up.sql`/`down.sql` 成对；**先加列再建索引**（索引引用不存在的列会让迁移失败、容器起不来）。

### 13.2 Python 侧 checkpoint 表

由 `AIOMySQLSaver.setup()` 创建（`checkpoints` / `checkpoint_blobs` / `checkpoint_writes` / `checkpoint_migrations`），**落同一个 tickets 库**。Go 侧迁移不建、不改这些表（ADR-2）。

---

## 14. 可观测性与运维

### 14.1 trace_id 与埋点（单一计数点）

- Go 在入口生成 `trace_id`（UUID），写入响应头 `X-Trace-Id`，**随每次内部调用透传**，并落 `cs_messages.trace_id`（反馈凭它反查）；
- **计数集中在 Go**：编排层把图侧决策事件（`{node, decision, ms, meta}`）批量回传 `/internal/metrics`，Go 侧 `PipelineTrace` 是**唯一计数点**——避免两层各算一套导致口径对不上；
- 结构化日志字段：`trace_id` / `category` / `source` / `cache_hit` / `top1_cos` / `transfer_path` / `tool` / `node` / `stage_ms{...}` / `tokens`。

### 14.2 指标清单

| 分组 | 指标 |
|---|---|
| 流量与质量 | `cs_requests_total`、`cs_errors_total`、`cls_source_total{rule,llm_arbitrate,inherit}`、`cls_distribution_total{category}`、`cls_llm_fallback_ratio` |
| 编排 | `graph_runs_total`、`graph_nodes_total{node}`、`graph_retry_total`、`graph_interrupt_total`、`graph_resume_total`、`checkpoint_errors_total` |
| 跨语言 | `internal_calls_total{api,kind}`、`internal_call_ms{api,p50,p95}`、`orchestrator_unavailable_total` |
| 缓存 | `answer_cache_hit_total` / `miss_total` / `hit_ratio` |
| 转人工 | `transfer_total{path=deterministic\|threshold\|model\|tool_unavailable\|orchestrator_unavailable}` |
| 检索 | `retrieval_empty_total`、`citation_dropped_total`、`top1_cos_p50`、`below_threshold_total`、`vector_mode{embedding\|ngram}` |
| 工具 | `tool_calls_total{tool}`、`tool_hit_total`、`tool_direct_answer_total`、`tool_error_total{tool}` |
| 模型 | `llm_calls_total`、`llm_errors_total`、`tokens_prompt_total`、`tokens_completion_total`、`usage_unknown_total`、`budget_exceeded_total` |
| 会话 | `conversations_active`、`turns_total` |
| 运行 | `uptime_seconds`、`stage_ms{p50,p95}`、`/metrics` 状态 |
| **检索基础设施** | `qdrant_points_total`、`ann_ms{p50,p95}`、`filter_relaxed_total`、`rerank_ms`、`rerank_fallback_total`、`index_drift_detected_total`、`rebuild_jobs_total`、`ingest_pending_total`、`embedding_calls_total` |
| **形态与场景** | `below_threshold_total{form}`、`top1_cos_p50{form}`、`retrieval_empty_total{sub_scenario}`、`qa_prose_ratio`、`lint_rejected_total{rule}`（入库被 lint 拦下的条目，10⁵ 级必须可见） |
| **能力边界** | `answers_with_boundary_total`（带边界声明的回答）、`supported_answers_total`、`capability_absent_total`（转人工路径⑤：功能不存在）、`boundary_violation_blocked_total`（后处理拦下的操作性措辞）、`capability_widened_total`（放宽到行业层的检索次数） |

### 14.3 四个已经踩过的口径 bug（写进实现约束，别重犯）

1. **`llm_calls` 必须由 usage 钩子驱动**：否则会出现"分类来源里 LLM 计 10 次、`llm_calls` 只 7 次"的自相矛盾（token 由钩子记，覆盖分类兜底 + 指代消解 + 生成三条路径）。
2. **加 `Retrieved bool`**：投诉/要人工在意图层就被拦截、根本没走检索，不能算成"检索空结果"。
3. **`empty_retrieval` 条件 = `Retrieved && ChunkNum==0 && Fallback==""`**：其他类（问候/拒答）走检索必然空，会污染"知识库覆盖缺口"信号。
4. **编排层新增：`orchestrator_unavailable` 必须**独立计数**（与"业务上该转人工"区分），否则"编排层挂了"会伪装成"知识库覆盖不足"。

**验收方法**：跑 ≥20 条覆盖各分支的真实问题，**以结构化日志为客观事实**逐条统计再与大盘比对（不要拿"预期分类"当基准，LLM 有随机性）。

### 14.4 降级矩阵

| 依赖故障 | 降级行为 | 对用户可见 |
|---|---|---|
| **编排层不可用** | Go 直接建单 + 标准话术（路径④） | 是（`degraded.orchestrator=true`） |
| checkpoint 不可用 | 本轮无状态执行（等价单轮） | 日志告警 |
| LLM 不可用 | 兜底话术 + 转人工（工具直答仍可用） | 是（`degraded.llm=true`） |
| Embedding 不可用 | 字面替身（阈值改 bigram 重叠率粗筛，低重叠转人工） | 是（`vector=ngram`） |
| Redis 不可用 | 无答案缓存；限流放行；座位预占降级（现有逻辑） | 日志告警 |
| MySQL 不可用 | 整链路 503（业务不可用） | 是 |
| 工具数据源异常 | 该工具降级 + 独立计数 | 是（转人工） |
| 知识库未入库 | 启动告警，`readyz.kb_chunks=0`，问答一律转人工 | 是 |

### 14.5 安全与脱敏

**脱敏口径（贴合本项目实际字段——`users` 表当前只有 `username` / `full_name`，无手机号/证件号/地址）**：

| 字段 | 规则 |
|---|---|
| `users.full_name` | 只保留姓氏（"张**"） |
| `users.username` | 对外/日志掩码（保留首末各 1 字符） |
| `orders.order_no` | 本人可见；**日志/工单摘要中段掩码** |
| 金额 / 座位号 / 班次时刻 | 可见（本人数据） |
| 未来新增（乘客证件、联系人手机、地址） | **上线前必须补掩码规则**，并禁止进入 prompt |

**其他硬线**：
- 客户端传入的任何 `user_id` 一律不采信（**对外接口签名里就没有这个入参**）；`user_id` 只由 Go 解析并注入编排层；
- 内部接口仅内网可达 + `X-Internal-Key` 校验；Python 端口不映射宿主机；
- 知识库内容与用户输入**物理分隔**在 prompt 不同区块；工具结果**不得被当作指令执行**（防注入）；
- AI 链路独立限流（身份桶 + IP 桶双闸）；限流值是**成本保护值**，不等于容量上限，压测后调整并在报告写明口径；
- 请求预算：整轮墙钟 8s + token 上限，超限降级转人工。

---

## 15. 评测体系

### 15.1 三套数据集（`testdata/cs/`，与代码同仓版本化）

| 集合 | 规模 | 内容 | 指标 |
|---|---|---|---|
| 分类 held-out | **≥400 条（7 类每类 ≥40）** | 口语化 / 错别字 / 省略指代 / 中英混写 / **跨类边界** / 长尾；与调优集**零重复文本**；跨类按**主意图**标注；**边界样本显式覆盖**：退款到账时效（refund↔order）、扣款成功无订单（order↔account）、能否开发票（refund↔order）、遗失物品（travel↔policy） | 分类准确率、混淆矩阵、**各类 P/R**、LLM 兜底占比 |
| 检索 | ≥200 条 | **真实买家问法**（含同义改写），按**知识点/chunk 级**判命中；**`qa` / `prose` 两形态分别有样本（prose ≥1/3）**，否则 prose 的问题会被 FAQ 样本掩盖 | Top-1/Top-3 命中率、空结果率、**分形态的 top-1 余弦分布**（用于分形态标定阈值） |
| 端到端 | ≥80 条 | `{问题, 期望分类, 期望 chunk, 关键事实}`；"其他"类关键事实填**兜底话术关键词**，与线上口径一致 | 生成事实符合率、引用剥除率、转人工路径分布 |

### 15.2 门槛（数字必须实测后填入，禁止沿用历史值）

| 维度 | 达标线 | 当前基线 |
|---|---|---|
| 分类准确率 | ≥ 90% | **待 M1 实测填入** |
| 检索召回（真实问法） | ≥ 90% | **待 M1 实测填入** |
| 检索召回（**prose 形态**） | ≥ 85% | **待 M1 实测填入**（与 qa 分开报，验证分形态阈值有效） |
| 端到端生成事实符合率 | ≥ 85% | **待 M1 实测填入** |
| 引用真实性 | 引用剥除率可解释并纳入监控 | — |
| 答案缓存 | 同义判定准确；个性化与转人工结果不入缓存 | 人工抽检无错答 |

> **纪律**：任何影响分类、检索、生成口径的变更（规则词表、切块、阈值、prompt、模型、图结构）之后，**必须重跑评测再引用数字**。基线未实测前，本文档不填任何准确率数字。

### 15.3 测试清单

**Go 侧**

| 测试 | 要点 |
|---|---|
| `kb/chunk_test.go` | 切块按 `##`；`utf8.RuneCountInString(chunk) <= chunkSize`；长 Q-A 边界 |
| `kb/bm25_test.go` | tf 预计算与现算打分等价（差异 0）；IDF 非负 |
| `kb/tokenize_test.go` | CJK/ASCII 边界不组 bigram |
| `tools/*_test.go` | 提槽正则；越权矩阵（本人/他人/无 token/伪 token）；退票费边界（无规则/已过发车/未支付） |
| `cache/answer_cache_test.go` | 准入第 2 次；LRU 用 `seq`（`-count=50` 连跑稳定，不 flaky） |
| `handlers/cs_test.go` | SSE 透传不缓冲；客户端断开即取消上游；编排层不可用 → 建单 + 200 |

**Python 侧（pytest）**

| 测试 | 要点 |
|---|---|
| 节点单测 | 每个节点用**假 CapabilityClient**（httptest 等价物）驱动，零真实 Go 依赖 |
| 条件边覆盖 | **逐条覆盖 E1~E16**（每条边至少一个用例）——图的分支就是产品行为 |
| 循环上限 | 检索空 + 有上下文 → 只重试 1 次后转人工（`retry_count` 断言） |
| interrupt/resume | 反问 → 重启进程 → `Command(resume=...)` 续跑成功；且**反问节点重放不产生重复副作用**（幂等断言） |
| 预算 | token/墙钟超限 → 转人工 |
| checkpoint | 建表 `setup()`、thread 复用、超期清理任务 |

**跨层**

| 测试 | 要点 |
|---|---|
| 契约测试 | 用 Go 侧真实 httptest 能力服务跑 Python 图（字段名/类型/错误 kind 全对齐） |
| 黑盒端到端 | 直接打 Go `/cs/ask` 跑端到端评测集（口径与线上完全一致，**这是唯一算数的端到端口径**） |
| 降级演练 | 停编排层 / 停 Redis / 不给 embedding key，三种场景下接口行为符合 14.4 |

### 15.4 诚实性验收（B 方案专属，必须自动化）

行业知识库最大的风险是「讲了一个平台没有的功能」。这一项**不能靠人工抽检**，要机检：

| 检查 | 做法 | 达标线 |
|---|---|---|
| 入口真实性 | 从答案抽操作性措辞（点击 / 进入 / 在…页面），对照能力台账 `system_entry` 清单核验 | **0 条指向不存在的入口** |
| 边界声明覆盖率 | 来源 `capability != supported` 的回答中带边界声明的比例 | **100%** |
| 矛盾组合 | 同一回答里同时含工具结论与 `industry` 建议的比例 | **0** |
| 两组评测集 | ① **平台能力类**问题（答案须与真实能力一致，且 `supported` 项能说出入口）② **行业知识类**问题（必须带边界声明） | 两组分别报准确率（不合并成一个数） |

> 与 14.3 的口径纪律同源：**任一影响 `capability` 判定、边界模板或后处理词表的改动，都必须重跑这两组集合**再引用数字。

---

## 16. 部署、配置与依赖

### 16.1 部署形态

| 组件 | 形态 | 端口 |
|---|---|---|
| Go 服务 | 现有 Dockerfile，公网入口 | `8080`（映射宿主机） |
| Python 编排层 | 新增 `orchestrator/Dockerfile`（`python:3.12-slim` + uv 安装锁定依赖 + uvicorn） | `8001`（**仅容器内网，不映射**） |
| MySQL | 现有 `mysql:8.4`（**≥8.0.19 是 checkpoint saver 硬要求；不要升 9.x**，见 ADR-2） | `3307:3306` |
| Redis | 现有 `redis:7-alpine` | `6379` |
| **Qdrant** | **新增** `qdrant/qdrant`（**仅容器内网，不映射宿主机端口**；卷 `qdrant_data`；API Key 校验） | `6333`(HTTP) / `6334`(gRPC) |

**Qdrant 参数（M0 实测定，见报告 §4）**：`m=16`、`ef_construct=100`、检索 **`hnsw_ef=256`**（过滤条件下召回 87.1%、P95 9.4ms）；**开启 int8 标量量化**（`quantile=0.99`、`always_ram=True`）——延迟与不量化基本持平、内存约 1/3；向量 `on_disk` 本次未启用（10⁵ ≈ 0.6~0.7GB 内存，量级再上一个数量级时评估）。

**M0 实测支撑的设计取向**：过滤（把候选压到千级）比堆 `ef` 划算得多——宽过滤 P50 9.06ms vs 无过滤 14.61ms；无过滤要达到相近召回需 `ef=512` 且 P50 已 18.29ms（报告 §4.4）。⇒ 「分类/形态/scope 过滤 + 两级检索」不是优化项，而是延迟与召回的前提。
| RabbitMQ | 现有（客服 V1 不依赖） | `5672` |

`docker-compose.yml` 增补要点：

```yaml
  orchestrator:
    build: { context: ./orchestrator, dockerfile: Dockerfile }
    environment:
      - CS_INTERNAL_KEY=${CS_INTERNAL_KEY}
      - GO_BASE_URL=http://app:8080            # 能力层基址
      - LLM_GATEWAY_URL=http://app:8080/internal/v1
      - CKPT_DSN=mysql://root:***@mysql:3306/tickets
      - CKPT_RETENTION_DAYS=7
    depends_on: [app, mysql]
    restart: always
    # 不写 ports —— 编排层不暴露到宿主机
  qdrant:
    image: qdrant/qdrant:latest          # 版本按 M0 spike 固定，勿用 latest 上生产
    environment:
      - QDRANT__SERVICE__API_KEY=${QDRANT_API_KEY}
    volumes: [ "qdrant_data:/qdrant/storage" ]
    restart: always
    # 不写 ports —— 编排层与平台层都只走容器内网
  app:
    environment:
      - ORCH_BASE_URL=http://orchestrator:8001
      - QDRANT_URL=http://qdrant:6334
      - CS_INTERNAL_KEY=${CS_INTERNAL_KEY}
      - ORCH_TIMEOUT=8s
```

**健康检查**：`GET /readyz`（Go）逐项报布尔：`db` / `redis` / `llm` / `embedding` / `kb_chunks>0` / `orchestrator`（后者探测 `orchestrator/healthz`）。

### 16.2 依赖锁定（M0 spike 产出）

`orchestrator/pyproject.toml`（**已由 M0 实测锁定**，见 `docs/M0-依赖spike-实测报告.md`）：

```toml
[project]
requires-python = ">=3.11"
dependencies = [
  "langgraph==1.2.11",                           # M0 实测
  "langchain-core>=1.4.7",
  "langchain-openai>=1.0",                       # base_url 指向 Go 网关（5.10.1）
  "langgraph-checkpoint==4.2.0",                 # M0 实测
  "langgraph-checkpoint-mysql[aiomysql]==3.0.0", # M0 实测：与 checkpoint 4.2.0 兼容
  "aiomysql>=0.3",
  "cryptography>=42",   # M0 实测补入：MySQL 8 的 caching_sha2_password 非 TLS 连接必需
  "fastapi>=0.141",
  "uvicorn[standard]",
  "sse-starlette>=3.4",
  "openai>=1.0",          # 只指向 Go 网关
  "qdrant-client>=1.12",
  "numpy>=1.26",
  "pydantic>=2.7.4",
  "orjson",
]

[dependency-groups]
dev = ["pytest>=8", "pytest-asyncio>=0.24"]

[tool.uv]
package = false
```

**M0 产出（已完成 2026-09-12）**：① 组合跑通 `setup()` + **跨进程** `interrupt/resume`；② `uv.lock` 已生成；③ **未回退**——ADR-2 首选档（MySQL saver）成立。四项实测发现：`cryptography` 必需；真实表名与推定一致（`checkpoints` / `checkpoint_blobs` / `checkpoint_writes` / `checkpoint_migrations`）；`setup()` 重复执行会告警 `Table 'checkpoint_migrations' already exists`（非致命，但应放在一次性初始化任务里，不要每次启动无脑调用）；`interrupt` 之前的代码在 resume 时**确实被重放**（节点入口探针命中 2 次）。详见 `docs/M0-依赖spike-实测报告.md`。

### 16.3 本机开发提示

- 本机 Python 3.11（`python`，非 `python3`），用 `uv` 管依赖；
- 非 Hermes 的 venv 运行前需 `env -u PYTHONPATH -u PYTHONHOME`（否则会串到 Hermes 的 venv）；
- Go 编译：`GOTOOLCHAIN=local GOPROXY=https://goproxy.cn`（本机 Go 1.22，`go.mod` 声明 `go 1.22.0`）；
- 起本地全链路：`docker compose up -d mysql redis rabbitmq` + `go run main.go` + `cd orchestrator && uv run uvicorn app.main:app --port 8001`。

---

## 17. 实施计划

| 里程碑 | 交付物 | 验收（可判定） |
|---|---|---|
| ~~**M0 依赖 spike**~~ **已完成 2026-09-12** | `orchestrator/pyproject.toml` + `uv.lock` + 三个 spike 脚本 + `docs/M0-依赖spike-实测报告.md` | ① `setup()` 建表成功（真实表名 `checkpoints` / `checkpoint_blobs` / `checkpoint_writes` / `checkpoint_migrations`）；② **跨进程** `Command(resume=...)` 续跑通过（进程1 中断退出 → 进程2 恢复并答对）；③ 10⁵×1024 过滤式 ANN：窄过滤 P50 6.36 / P95 8.46ms，宽过滤 9.06 / 13.25ms；int8 延迟持平、内存约 1/3；④ `hnsw_ef=256` 过滤召回 87.1%——**先过滤优于堆 ef** |
| **M1 契约 + 图骨架 + 入库检索 + 生成** | Go：`/internal/*` 接口 + `kb_*` / `cs_capabilities` 迁移 + loader/chunk（**两形态**）/ BM25 倒排 / RRF / **Qdrant 集成** / **rerank** + 引用校验 + 建单；Python：图（E1/E2/E3/E4/E7/E10/E13/E14/E16）+ **能力边界声明（5.9）** + 单测 | 20 条覆盖各分支的问题行为正确（以结构化日志为证）；无依据必转人工；引用剥除可计数；契约测试通过；**批量入库可断点续跑**；**游客检索不到 authenticated chunk**；**边界声明覆盖率 100%、入口真实性 0 违规**（15.4） |
| **M2 工具层 + 身份**（**已完成 2026-09-13**） | 6 个读工具 + 建单工具 + 提槽 + 脱敏 + 熔断 + 越权矩阵测试；退票费确定性计算 | 越权矩阵 5 项全通过；退票费与人工核对一致（≥10 样例）；工具不可用独立计数 —— 实测 **22/22 通过**，19.2 闸门默认关闭（样例表待业务确认），见 `docs/M2-实现报告.md` |
| **M3 记忆 + 缓存 + 流式**（**已完成 2026-09-13**） | checkpoint 治理（幂等接入 + 保留期清理任务）+ 答案缓存（`cs:ac:*`，二次命中准入）+ SSE 全链路（Python 出流 → Go 透传 → 前端抽屉）+ `/cs` 抽屉 + interrupt/resume 消歧反问 | 多轮指代消解可用；反问后重启进程仍能续跑；同义问题二次命中缓存；流式首字可见；客户端断开后上游被取消 —— 实测 **phase1 12/12 + 重启后 3/3**（`M3_e2e_exit=0`），M1/M2 回归无退化，见 `docs/M3-实现报告.md` |
| **M4 可观测 + 评测 + 治理** | `/metrics` + `/readyz` + 管理 token + trace 全链路 + 三套评测集 + 诚实性验收（15.4）+ 限流 + 密钥治理（`app.env` 出库 + 轮换）+ 降级演练 | 大盘与日志口径一致（20 条抽验）；三档评测数字出数并归档；**入口真实性 0 违规、边界声明覆盖率 100%**；**停编排层后 `/cs/ask` 仍 200 且有工单号**；`git add --dry-run app.env` 被忽略 |
| **M5 tickets 功能补全（从能力台账反推，见 7.9）** | 第 1 批：常用联系人 / 证件与核验 / 手机邮箱；第 2 批：遗失物品登记 / 报销凭证 / 通知通道；第 3 批：**候补购票** / 联程接续 / 变更到站 / 会员积分 | 每批落地后台账状态翻转 `roadmap → supported`（**措辞自动切换、零重嵌**）；新增能力必须填 `system_entry` 且通过入口真实性检查（15.4） |

**排序理由**：M0 必须先做——它决定 ADR-2 是否成立，做错顺序会在 M3 才发现 saver 不可用（返工最贵）。M1 打通"能答 + 不乱答"；M2 是差异化但与 LLM 解耦；M3 是体验/成本；M4 的埋点骨架**至少要在 M1 就立起来**，事后补分支口径很贵。

---

## 18. 风险与对策

| 风险 | 影响 | 对策 |
|---|---|---|
| **checkpoint saver 与 checkpoint 4.x 不兼容**（ADR-2） | M3 才暴露 → 返工最贵 | **M0 spike 前置**；备好四档回退链（sqlite / postgres / 自研 / 无 checkpointer） |
| 跨语言复杂度（契约、超时、降级、流式） | 故障面翻倍、排查变难 | 契约一次性定义（§6）；`trace_id` 全链路；降级矩阵逐项演练（M4） |
| 编排层单点 | 客服整体不可用 | Go 侧降级为转人工 + 标准话术；`readyz` 暴露；compose `restart: always` |
| 知识库覆盖不足 | 大量转人工 | 上线前覆盖高频问题；`retrieval_empty_total` 定位缺口；held-out 驱动补文档 |
| LLM 幻觉 / 编造出处 | 错误承诺手续费、错误引导 | 三层防线：相关性阈值 + 无依据输出 `[TRANSFER]` 由服务端建单 + **引用校验** |
| `penalties` 规则语义不确定 | 退票费算错 = 客诉 | **不确定就不算**：工具下线，走 FAQ + 转人工（8.2） |
| interrupt 重放副作用 | resume 时重复建单/重复扣减 | 反问节点零副作用；写操作全部后置到 `finalize`；单测断言幂等 |
| checkpoint 无界增长 | 延迟上升、存储膨胀 | 保留期 + 定时清理任务（5.5）；纳入 M3 验收 |
| 工具直连业务表 | 越权读他人数据 | 接口签名无 `user_id`；SQL 强制归属过滤；越权统一"未查询到"；必测矩阵 |
| 密钥泄露 | 费用被刷 / 账号风险 | 密钥治理（11.4）；AI 链路独立限流；token 预算熔断 |
| 模型成本失控 | 账单不可控 | 网关统一记账 + 按 trace 预算 + 答案缓存 + 限流双闸 |
| **派生索引漂移**（Qdrant 与 MySQL 不一致） | 检索到已删除/旧版内容 | 幂等 upsert（point id = `kb_chunks.id`）+ 定期对账 + alias 重建；`index_drift_detected_total` 监控告警 |
| **实例化知识陈旧**（上千条站点规则谁维护） | 规模大但答案是旧的 → 客诉 | 权威来源与更新流程定死后才允许上线（19.9）；`expire_at` + 版本与变更审计 |
| Qdrant 内存/成本 | 10⁵×1024×4B ≈ 400MB 常驻 | int8 量化（≈100MB）+ 可选 on-disk；HNSW 参数按 M0 实测定 |
| embedding/rerank 配额与成本 | 全量入库耗时、在线调用超配额 | 批量入库限速 + 断点续跑；rerank 可降级（`rerank_fallback_total`）；query 向量在请求内复用 |

---

## 19. 待确认事项

| # | 事项 | 建议 |
|---|---|---|
| 19.1 | LLM 供应商与模型（Kimi k2.6 关思考 / DeepSeek / 其他） | 关思考快模型作默认；评测后再定 |
| 19.2 | **`penalties` 表规则行语义**（`hours_before` 与 `actual_hours_before` 各自含义） | 与业务确认后实现 `refund_fee`；**确认不了就下线该工具** |
| 19.3 | 游客能否建工单 | 建议允许（`user_id=NULL`，用 `conv_id` 关联），否则游客拿不到可追踪工单号 |
| 19.4 | AI 响应时间目标 | 建议"**流式首字 ≤2.5s**（含跨语言与分类检索）、整轮 P95 ≤5s"；参考 PRD 的"≤1s"在关思考模型首字 ~1.7s 的事实下不可达，不应写进验收 |
| 19.5 | 前端入口形态 | 建议 `web/` 各页右下角悬浮入口 + 全屏抽屉（`web/cs.html` + `assets/js/cs.js`），复用现有静态资源与 no-cache 策略 |
| 19.6 | 是否本轮处理 `app.env` 出库与密钥轮换 | 建议本轮处理（新增密钥前必须做，见 11.4） |
| 19.7 | 管理面端口 / 编排层是否需 `POST /resume` 显式接口 | 建议管理面独立端口 + 仅 `127.0.0.1`；`/resume` 保留（运维手工续跑/排障用），正常链路走 `/ask` 带同 `conv_id` |
| 19.8 | checkpoint 保留期 | 建议 ≥7 天（对齐审计需要），清理任务随 M3 上线 |
| 19.9 | **实例化知识（`stations/` 等 10⁵ 条）的权威来源与更新流程**：谁维护、怎么导入、多久更新一次 | **不解决不允许上线**——否则"规模大"会变成"陈旧答案多"；建议：权威数据源表 → 生成文档 → 入库，并保留变更审计 |
| 19.10 | rerank 方案（bge-reranker API / 本地 cross-encoder）与降级阈值 | 建议先用 API（50~200ms）并把"不可用则回退融合分"做成默认行为 |
| 19.11 | Qdrant 部署参数（int8 量化、on-disk、HNSW `m`/`ef`、内存上限、API Key） | 由 M0 spike 实测后固定，写进 compose 与文档 |
| ~~19.12~~ | ~~知识库定位与「功能可用性边界」~~ | **已决策：方案 B** ——知识库定位为**铁路客运行业知识库**，覆盖完整业务域、独立于代码；平台未实现的功能照写行业规则，但必须靠 **§5.9 的四级防线**兜住（chunk 带 `capability` + 检索优先 supported + prompt 物理分区 + 后处理拦截操作性措辞）；tickets 侧功能补齐排入 **M5**（路线见 7.9） |
| 19.13 | 子场景清单（41 项）的维护来源与优先级 | 建议：先做 ✅/⚠️ 的高频子场景，❌ 项用统一话术兜住；子场景字典入库管理并版本化 |
| 19.14 | 两形态阈值的标定样本量 | 建议各形态 ≥100 条真实问法再定阈值，避免用 FAQ 阈值套 prose（§7.5） |
| 19.15 | 能力台账的维护人（41 项状态谁定、多久复核一次） | 建议：产品/客服负责人维护，**每次功能上线必须同步翻转状态**（写进 M5 验收），否则台账会变成新的陈旧源 |
| 19.16 | 边界声明话术模板与后处理词表的初始版本 | 建议：模板用 5.9 给的版本；操作性词表初始收「点击 / 进入 / 在…页面 / 我们的 / 本站支持 / 我的订单页」，后续按误报漏报迭代 |
| 19.17 | 何时把行业知识拆成独立 collection（`kb_industry`） | 现在的做法是**单 collection + `capability` 维度 + 两级检索**（复用已有机制、少一套 alias 管理）。判据：行业内容体量超过 native 的 3 倍、或需要独立的模型/量化配置时再拆 |

---

## 附录A：代码落位

```
tickets/
├── kb/                                   # 新增：知识库（7 类 × 41 子场景 × {qa, prose}）
│   ├── booking/**/*.md                   # 含 sub_scenario 二级目录
│   ├── order/**/*.md
│   ├── refund/**/*.md
│   ├── account/**/*.md
│   ├── travel/**/*.md
│   ├── app/**/*.md
│   ├── policy/**/*.md
│   ├── stations/*.md                     # 站点实例化（prose 主体，10⁵ 规模来源）
│   └── capability.yaml                   # 能力台账：41 子场景 × {supported, roadmap, industry}
│                                         #            + system_entry + source + owner/target（7.9）
├── internal/ai/                          # 新增：Go 平台能力层
│   ├── classifier/                       # rules.go（关键词权重打分）
│   ├── kb/                               # loader.go / chunk.go / tokenize.go
│   │                                     # bm25.go / postings.go（真倒排 + 剪枝）
│   │                                     # vector.go（接口）/ vector_qdrant.go（ANN 实现）
│   │                                     # rerank.go / rrf.go / store_mysql.go / reconcile.go（对账）
│   ├── ingest/                            # 批量入库：分批 embed / 限速 / 断点续跑 / alias 切换
│   ├── tools/                            # registry.go / my_tickets.go / order.go
│   │                                     # availability.go / refund_fee.go / refund_progress.go
│   │                                     # sanitize.go
│   ├── citation/verify.go                # 引用校验 + 计数
│   ├── session/store.go                  # 会话与流水读写（cs_conversations / cs_messages）
│   ├── ticket/ support_ticket.go         # 工单状态机（条件更新）
│   ├── cache/answer_cache.go             # 答案语义缓存
│   ├── llmproxy/                         # client.go / gateway.go（OpenAI 兼容端点 + 记账）
│   └── observe/                          # metrics.go / trace.go / readyz.go
├── internal/api/handlers/
│   ├── cs.go                             # /cs/*（对外，含 SSE 透传）
│   └── internal.go                       # /internal/*（内网能力面）
├── internal/routes/web.go                # 修改：注册 /cs/* 与 /internal/*
├── internal/db/migration/000005_add_cs_tables.{up,down}.sql
├── internal/db/query/cs.sql              # 新增：sqlc 查询（cs_* / kb_* / support_tickets）
├── internal/util/config.go               # 修改：新增 CS_* / LLM_* / EMBEDDING_* / ORCH_*
├── web/cs.html + web/assets/js/cs.js + web/assets/css/cs.css
├── testdata/cs/                          # 三套评测集
├── cmd/kb-sync/main.go                   # 知识库增量入库命令（含断点续跑）
├── cmd/kb-rebuild/main.go                # 全量重建 + Qdrant alias 原子切换
├── app.env.example                       # 配置模板（app.env 出库）
└── orchestrator/                         # 新增：Python 编排层
    ├── pyproject.toml + uv.lock          # 依赖锁定（M0 产出，已实测）
    ├── spike/
    │   ├── m0a_checkpointer.py           # A：setup / start / resume / probe 四态
    │   ├── m0b_qdrant.py                 # B：建 → 灌 10⁵ → 测（plain / int8）
    │   └── m0b_ef_sweep.py               # B：hnsw_ef 召回-延迟曲线
    ├── Dockerfile                        # python:3.12-slim + uv
    ├── app/
    │   ├── main.py                       # FastAPI：/ask /resume /healthz + X-Internal-Key 校验
    │   ├── settings.py                   # 配置（GO_BASE_URL / CKPT_DSN / 超时与预算）
    │   ├── graph/
    │   │   ├── build.py                  # StateGraph 组装 + 条件边（E1~E16）
    │   │   ├── state.py                  # CSState（TypedDict）
    │   │   └── nodes/                    # load_context / pre_intent / classify / arbitrate
    │   │                                 # route_tool / exec_tool / clarify / retrieve / gate
    │   │                                 # generate / verify / transfer / finalize
    │   ├── clients/
    │   │   ├── capability.py             # Go 能力层客户端（超时/重试/错误 kind 分流）
    │   │   └── llm.py                    # openai SDK → Go 网关
    │   ├── checkpoint/
    │   │   ├── mysql.py                  # AIOMySQLSaver 装配 + setup()
    │   │   └── retention.py              # 超期 thread 清理任务
    │   ├── prompts/                      # 分类兜底 / 跨类仲裁 / 指代消解 / 生成 / 改写
    │   └── observe.py                    # 图侧决策埋点回传 /internal/metrics
    └── tests/                            # pytest：节点 + 条件边 + interrupt/resume + 预算
```

---

## 附录B：与参考 PRD 的条目级对照

| 参考 PRD 条目 | 处置 | 说明 |
|---|---|---|
| **双运行时（Go 平台层 + Python 编排层）** | **保留** | 本文档采纳（ADR-1），并补上 tickets 侧的落地细节 |
| 编排由 LangGraph 承担（条件边 / 循环 / 断点 / 人工介入） | **保留** | 补 checkpoint 存储选型与版本互斥风险（ADR-2）、interrupt 重放纪律（5.4） |
| LangGraph checkpoint 落 **PG** | **改写** | → 落 **MySQL**（复用现有实例）；风险与回退链见 ADR-2。*与既有约定"图状态唯一真相源落 PG 不双写"的差异在此说明：本项目只有 MySQL，且新增 PG 仅为一个 saver 成本偏高，故改 MySQL 并明确回退链* |
| LLM 网关为**独立服务**、对编排层暴露 OpenAI 兼容端点 | **改写** | → 先做**同进程**的 OpenAI 兼容端点（`/internal/v1/...`），端点与客户端不变；需要时再抽独立服务（ADR-5） |
| 多商户 / 租户隔离 / 商户子知识库 | **删除** | 只有一个官方客服（本次需求） |
| 公共库 + 商户库双层与优先级 | **改写** | 单层 + 「身份可见性 public/authenticated」维度替代（ADR-6） |
| 知识库四维（商户×领域×类型×可见性） | **改写** | 去商户，保留领域/类型/时效/可见性 |
| 渠道接入层 + 自建 H5 + mock 渠道 | **简化** | V1 复用现有 `web/` 静态页 + `/cs` 抽屉；不做渠道抽象 |
| 外部平台准入调研（淘宝/拼多多/抖店） | **保留为背景** | 本项目入口是自有 Web 站 |
| 自研混合检索（BM25 + 向量 + RRF） | **保留并升级** | 关键细节（阈值 0.60、bigram、tf 预计算、按 `##` 切）保留；BM25 升级真倒排 + 剪枝，向量路改 **Qdrant ANN**（10⁵ 级），**融合与阈值判定仍在 Go**，新增 **rerank**（ADR-10） |
| 分类路由四级链路 + 固定 4 类 | **保留**（换分类内容） | 技术/业务/财务/其他 → 购票/退改/乘车/财务；规则打分在 Go、LLM 仲裁在 Python |
| 工具调用（7 个注册工具，数据源 mock） | **改写** | 7 个工具（6 读 + 1 建单），**数据源为真实业务表**；退票费为确定性计算；AI 只读 |
| 违规内容拦截 / 催付 / 改价 / 会话小结 | **删除或后续** | 无对应业务支撑（8.4） |
| 转人工四路径分别计数 | **保留** | 增补 `orchestrator_unavailable` 独立计数（14.3） |
| 引用校验（citation drop） | **保留** | 落在 Go（确定性） |
| 工单四态状态机 | **保留** | 表名 `support_tickets`（避开 `tickets` 冲突） |
| 坐席工作台 / 排队 / 接管 / 移交 | **后续迭代** | V1 只到建单 + 查询；预留 `assigned_to` |
| 多租户与权限体系（商户/坐席/角色矩阵） | **删除** | 单租户；管理面单一 token |
| 可观测性（20+ 指标 / Prometheus / 独立控制面 / 审计） | **保留并适配** | 指标清单见 14.2；补跨语言与编排层指标 |
| 生产硬化（限流双闸 / 请求预算 / panic 恢复 / 优雅停机） | **保留** | 限流复用 Redis 固定窗口；AI 链路单独限流；整轮 8s 预算 |
| 非功能：编排层不可用时接入层降级为转人工与标准话术 | **保留** | 作为 M4 验收项（停编排层演练） |
| held-out 评测纪律 + 数字引用前重跑 | **保留** | 见 15.2 |
| 数据报表与导出 | **删除** | 保留 `/metrics` 与最小控制面 |
| 飞书推送派单 | **不做** | 可选后续 |
| 语音 / 多语言 / 外呼 / ERP-CRM 对接 | **不做** | 后续迭代 |
| PII 脱敏口径（手机号/收货人/地址/快递单号） | **改写** | 本项目当前无这些字段；改 `full_name`/`username`/`order_no` 口径，并约定新增 PII 字段必须补规则（14.5） |
