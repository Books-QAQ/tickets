# 客运票务系统 · Go 后端技术设计文档

| 项目 | 内容 |
|---|---|
| 文档名称 | 客运票务系统 - Go 后端技术设计文档 |
| 版本 | V1.0 |
| 日期 | 2026-09-13 |
| 状态 | 设计稿（**现状盘点部分为已实现事实，前瞻部分为实现状态矩阵中标注 🚧 的规划**） |
| 目标系统 | `D:\golangproject\tickets`（Go + Fiber 票务后端，含 AI 客服平台层） |
| 关联文档 | `docs/智能AI客服系统-技术设计文档（V1.0）.md`（V1.0-r5，**编排层与 RAG 细节以那份为准**）、`docs/M0~M4-实现报告.md` |
| 读者 | 后端/平台评审、接手开发、面试讲解 |
| 变更记录 | **V1.0（2026-09-13）**：首次成文。以代码为真相源盘点现状（带 `file:line`），并把功能域扩展到「当前已实现之外」（改签/候补/对账/风控/通知/多租户等），逐条标实现状态与前置条件 |

## 术语表

| 术语 | 含义 |
|---|---|
| 班次（bus） | 某条线路在某时刻的一次发车，是座位与订单的归属单位 |
| 座位（bus_seat） | 班次下的物理座位，`status` 只有 `available / reserved / sold` 三态 |
| 预占（seat hold） | Redis 层的**分流**手段（`SetNX + TTL`），**不是**座位归属的裁决权 |
| 裁决权（arbitration） | 座位归属的**唯一**判定来源 = MySQL 条件更新（`UPDATE … WHERE status='available'`）+ `RowsAffected` |
| 关单（close） | 超时未支付订单转 `canceled` 并把座位从 `reserved` 放回 `available` |
| 平台层 | Go 侧：身份、限流、检索、工具执行、缓存、校验、工单、指标（本文件主体） |
| 编排层 | Python/LangGraph 侧：图结构、条件边、分类仲裁、断点续跑（见关联文档） |

---

## 1. 结论摘要

### 1.1 关键决策（每条锚到 §3 的 ADR）

| # | 决策 | 一句话理由 | ADR |
|---|---|---|---|
| 1 | **Go 是唯一公网入口**，编排层只内网可达并校验 `X-Internal-Key` | 身份/限流/审计必须单点，多入口必然两个真相源 | ADR-1 |
| 2 | **座位归属只由 MySQL 条件更新裁决**，Redis 预占仅做分流 | 预占锁会丢、会过期，拿它当真相必超卖 | ADR-2 |
| 3 | 查询侧 **Cache Aside + 空值缓存 + 互斥重建 + TTL 抖动** | 分别治穿透/击穿/雪崩，是三个独立故障不是一件事 | ADR-3 |
| 4 | 超时关单用 **DLX + 队列级 TTL，另配兜底扫描** | 队列级 TTL 避免 per-message TTL 的队头阻塞；扫描兜消息丢失 | ADR-4 |
| 5 | **支付状态以主动查单为准**，回调只作触发；已扣款但订单已关 → 自动退款 | 回调会丢/会重放，资金状态不能靠"收到过通知"来判定 | ADR-5 |
| 6 | 对外接口 **schema 里没有 `user_id`**，身份一律服务端解析 | 传参式身份 = 横向越权入口 | ADR-6 |
| 7 | **时间比较下沉 SQL**（`NOW()` / `TIMESTAMPDIFF`） | 应用时区与容器会话时区不一致，Go 侧比时钟会错 8 小时 | ADR-7 |
| 8 | **指标只有一个计数点（Go）**，降级必须显式可见 | 两层各算一套口径必然对不上；静默降级是最坏故障 | ADR-8 |
| 9 | 密钥落 `app.env`（**不入库**）+ 管理端启用即强制 token | 密钥进过 git 就得按已泄露处置 | ADR-9 |
| 10 | 多租户走 **`tenant_id` 贯穿 + 知识库物理隔离**（前瞻） | 逻辑隔离的漏写一处就是越权，物理隔离成本更低 | ADR-10 |

### 1.2 分期（里程碑 + 可判定验收）

| 阶段 | 范围 | 验收判据 | 状态 |
|---|---|---|---|
| **G0 票务底座** | 用户/鉴权、城市车站线路班次座位、下单占座、支付出票退票、DLX 关单 | 并发一致性测试（超卖 0）+ 关单链路可复现 | ✅ 已实现 |
| **G1 并发与缓存加固** | 条件更新裁决 + Redis 预占分流 + 缓存三件套 + 登录限流 | 抢座压测无超卖；缓存命中/穿透/击穿各有对应指标 | ✅ 已实现 |
| **G2 客服平台层** | `/internal/*` 契约（13 端点）、检索、工具层、答案缓存、引用校验、指标 | 越权矩阵 22 项全过；工具不可用有独立计数 | ✅ 已实现（见 M1~M4 报告） |
| **G3 前瞻业务域（第一批）** | 改签、通知通道、坐席工作台、对账 | 见 §5 各域验收 | 🚧 待建 |
| **G4 前瞻业务域（第二批）** | 候补购票、风控反黄牛、运营后台 | 候补：队列有序放行且无超卖 | 🚧 待建 |
| **G5 平台化** | 多租户演进、数据报表 | 越权矩阵在租户维度复跑全过 | 🚧 待建 |
| **G6 真实模型与标定** | LLM/embedding 选型、阈值标定、评测集扩到门槛 | 引用数字前必须重跑评测（§10.2） | 🚧 阻塞在待确认项 19.1 |

> **实现状态口径**：✅ 已实现（代码可查）/ ⚠️ 部分实现（有基础但有已知缺口）/ 🚧 待建（含设计意图与前置条件）。**本表是评审与验收的唯一依据**，🚧 不得当成已交付。

---

## 2. 现状盘点（以代码为真相源）

### 2.1 技术栈

`Go` · `Fiber(v2)` · `MySQL 8.4` · `Redis 7` · `RabbitMQ` · `sqlc` · `golang-migrate` · `JWT(golang-jwt)` · `Zerolog` · `Docker/Compose` ·（客服侧）`Qdrant` · `Prometheus 文本暴露`

### 2.2 可复用资产（带证据）

| 能力 | 落位 | 证据 |
|---|---|---|
| 座位裁决（条件更新 + 行数判胜负） | `internal/db/sqlc/buses.sql.go` / `buses_extra.go` | `buses.sql.go:389`（`AND status = 'available'`）、`:398`（`RowsAffected`）、`buses_extra.go:23` 注释「靠行锁 + RowsAffected 判胜负」 |
| 事务封装 | `internal/db/sqlc/exec_tx.go:10` | `store.execTx(ctx, fn)` → `BeginTx` |
| 座位预占锁（分流） | `internal/cache/redis.go:104` | `client.SetNX(ctx, SeatHoldKey(busID, seatID), owner, ttl)` |
| 缓存互斥重建（防击穿） | `internal/cache/redis.go:128-134` | 注释明写「只放行一个请求查数据库」，实现即 `SetNX` 重建锁 |
| 固定窗口限流 | `internal/cache/redis.go`（`AllowFixedWindow`） | 登录处调用 `internal/api/handlers/user.go:80-101`（IP 桶 + 用户桶**双闸**） |
| 延迟关单（DLX + 队列级 TTL） | `internal/queue/*.go` | `:12-16` 命名与意图注释；`:54-60` `x-dead-letter-exchange` / `x-dead-letter-routing-key` |
| 关单消费者 + 兜底扫描 | `internal/worker/order_expiry.go` | `:22` 消费者、`:79` 兜底扫描、`:106` `closeExpiredOrders`（兜底窗口 = 一个完整订单周期） |
| 支付适配（查单/验签/退款） | `internal/payment/alipay.go` | `:97 QueryOrder`、`:119 VerifyNotify`、`:135 Refund`；另有 `mock.go` 便于无沙箱开发 |
| 中间件 | `internal/api/middleware/` | `authMiddleware.go`（JWT）、`internalKey.go`（内网）、`adminAuth.go`（管理端）、`csRateLimit.go`（AI 链路双闸） |
| AI 客服平台层 | `internal/ai/{kb,classify,cite,llm,tools,answercache}`、`internal/metrics/` | 见 M1~M4 实现报告 |
| 知识库同步工具 | `cmd/kb-sync/` | 入库 + 变更后清答案缓存 |
| 迁移 | `internal/db/migration/000001..000005` | 5 组 up/down |

### 2.3 数据模型（18 张表，**含归属**：谁建谁别碰）

| 域 | 表 | 归属 |
|---|---|---|
| 账号 | `users`、`sessions` | Go（注意：`sessions` 是 JWT 会话，**与客服会话无关**） |
| 基础数据 | `cities`、`terminals`、`routes`、`buses`、`bus_seats` | Go |
| 交易 | `orders`、`tickets`、`seat_reservations`（预占记录）、`penalties`（退票费规则） | Go |
| 知识库 | `kb_documents`、`kb_chunks`、`cs_capabilities` | Go（检索侧；Qdrant 是可重建的派生索引） |
| 客服会话 | `cs_conversations`、`cs_messages`、`cs_feedback`、`support_tickets` | Go（**图状态**在 Python 的 LangGraph checkpoint 表：`checkpoints*`，Python 归属） |

**命名冲突预警**（已处理，写下来防后人踩）：`tickets` 已被"车票"占用、`sessions` 已被"JWT 会话"占用 ⇒ 客服域一律 `cs_*` 前缀，工单用 `support_tickets`。

### 2.4 缺口清单（当前实现的已知边界）

| # | 缺口 | 影响 | 处置 |
|---|---|---|---|
| 1 | `penalties`（退票费规则）**行语义未与业务确认** | 金额类能力不敢上线 | 已用 `PENALTY_SEMANTICS_CONFIRMED` 闸门默认关闭 → FAQ + 转人工（§5.5） |
| 2 | 游客能否建单未拍板 | 影响合规与滥用面 | 默认允许 + 启动告警；一个开关可翻转（`GUEST_TICKET_ALLOWED`） |
| 3 | LLM/embedding 未选型，阈值未标定 | 检索与生成质量数字不可引用 | 阻塞在待确认项 19.1（§13） |
| 4 | 无改签/部分退/误车处理 | 业务闭环不完整 | G3 第一批（§5.5） |
| 5 | 无对账与退款重试 | 资金侧无兜底 | G3 第一批（§5.10） |
| 6 | 无风控/反黄牛 | 高并发抢座可被脚本占满 | G4（§5.11） |
| 7 | 无坐席工作台前端 | 工单只能查状态、不能真正处理 | G3（§5.8） |
| 8 | 检票/核销缺失 | 出票后有票无验 | G4（§5.5 补充） |
| 9 | `app.env` 历史提交含旧凭据 | 已泄露面 | 需轮换 DB 口令与 `TOKEN_SECRET_KEY`（§9.3） |
| 10 | 评测集规模未达门槛 | 分类/检索准确率不可对外 | 与 19.1 一起做（§10） |

---

## 3. 架构决策记录（ADR）

### ADR-1 Go 为唯一公网入口，编排层只内网可达
- **决策**：所有客户端流量进 Go；Python 编排层只监听内网端口（compose 不映射宿主机端口），调用须带 `X-Internal-Key`。
- **理由**：身份解析、限流、审计、脱敏必须单点。若客户端能直连编排层，则出现两个身份真相源，且脱敏/审计规则要维护两份。
- **被否方案**：编排层直接对公网暴露（省一跳）——否决：跳过的正是安全边界。
- **后果**：跨语言一跳（内网 HTTP）有额外延迟与失败面 ⇒ 必须有降级路径（ADR-8）与分级超时（§6.2）。

### ADR-2 座位归属只由条件更新裁决
- **决策**：`UPDATE bus_seats SET status='reserved', … WHERE id=? AND status='available'` + 检查 `RowsAffected`；Redis `SetNX + TTL` 预占只用于**把绝大多数并发挡在数据库之外**。
- **证据**：`buses.sql.go:389/398`、`buses_extra.go:23`。
- **被否方案**：以 Redis 预占为准（性能最好）——否决：锁会过期、会因宕机丢失，必然超卖；且关单要回滚 Redis 状态，一致性无法闭合。
- **后果**：极端热点座位仍会打到数据库 ⇒ 前置限流与排队（§5.11）。

### ADR-3 缓存三件套分治三类故障
- **决策**：Cache Aside 为主；**空值缓存**治穿透、**`SetNX` 互斥重建**治击穿、**TTL 随机抖动**治雪崩。
- **证据**：`internal/cache/redis.go:128-134`（互斥重建）。
- **被否方案**：逻辑过期（不设物理 TTL）统一治击穿——否决：需要额外的异步重建协程与陈旧读语义，本项目的查询侧没有"旧值可接受"的业务前提（余票会带来超卖错觉）。
- **后果**：一致性语义 = 最终一致 + 短 TTL；**涉及钱的读一律不走缓存**（订单/支付状态直读数据库）。

### ADR-4 DLX + 队列级 TTL + 兜底扫描
- **决策**：延迟队列用**队列级** TTL 统一过期（所有订单一个过期时长），到点死信转发到消费队列；另起定时扫描器兜"消息丢失"。
- **证据**：`internal/queue/*.go:12-16,54-60`（注释明写「避免 per-message TTL 的队头阻塞」）、`order_expiry.go:79,106`（兜底窗口=一个订单周期）。
- **被否方案**：per-message TTL——否决：队头阻塞，前一条长 TTL 会拖住后面的短 TTL 消息。
- **后果**：订单过期时长必须全局一致（`ORDER_EXPIRE_DURATION` 单值）；若要支持"不同类型不同过期时长"，需改成多队列或时间轮（§12 风险）。

### ADR-5 支付状态以主动查单为准
- **决策**：`/pay/alipay/notify` 只作触发；关单前必须 `QueryOrder` 复核；若"已扣款但订单已关"则自动 `Refund`（避免长款）。
- **证据**：`internal/payment/alipay.go:97/119/135`、`internal/worker/order_expiry.go:51`（关单处理里带 alipay provider）。
- **被否方案**：只信回调——否决：回调会丢、会重放、会被伪造（必须验签），资金状态不能建立在"收到过通知"上。
- **后果**：查单增加一次外部调用 ⇒ 关单链路要有超时与重试上限（§5.6）。

### ADR-6 身份单一真相源：接口无 `user_id` 入参
- **决策**：对外接口不接收 `user_id`；用户在 Go 侧由 JWT 解析（`/cs/ask` 兼容游客 `device_id` 锚点），随后**注入**给编排层；跨用户一律返回"未查询到"（不 403、不泄露存在性）。
- **证据**：`internal/api/handlers/cs.go:130,133`（`payload["user_id"] = ident.UserID`）；编排层文件头声明「不解析用户身份」（`orchestrator/app/main.py:4`）。
- **被否方案**：内部接口也要求 Python 重新解析 JWT——否决：两个身份真相源；且 Python 侧一旦被绕过即可伪造身份。
- **后果**：内部接口的 `user_id` 属于"高信任入参"，必须靠网络隔离 + `X-Internal-Key` 保护（不得暴露到公网）。

### ADR-7 时间比较下沉 SQL
- **决策**：到期/剩余时间判断一律用 `NOW()` / `TIMESTAMPDIFF(MINUTE, NOW(), expired_at)`；`ORDER BY` 后再取窗口，禁止依赖未排序结果。
- **背景**：应用 DSN `loc=Asia/Shanghai`，而 MySQL 容器会话默认 UTC，`time.Until(expired_at)` 在 Go 里比会差 8 小时，实测把「15 分钟后到期」判成「已超时」。
- **后果**：SQL 里出现少量业务语义（可读性略降），换来时区一致性；测试夹具需 `SET time_zone='+08:00'`。

### ADR-8 指标单点 + 降级诚实
- **决策**：指标只有一个计数点（Go 的 `kb.Aux` → `/metrics` 与 `/admin/metrics`）；编排层只把图事件回传 `/internal/metrics`，**不自己算业务计数**。任何降级都要在响应里显式可见。
- **证据**：`internal/api/handlers/internal.go:409`（`RecordMetrics` 折进 `Aux`）、`orchestrator/app/clients.py:146`（回传事件）；`grep counters|Counter orchestrator/app/*.py` 为空。
- **降级字段**：`degraded{vector, llm, orchestrator}` + `vector_mode`；编排层不可用时 Go **降级建单**（不 503）并单独计数 `orchestrator_unavailable`。
- **理由**：静默降级是最坏的一类故障——"编排层挂了"会伪装成"知识库覆盖不足"，排障方向全错。

### ADR-9 密钥治理
- **决策**：真实配置只落 `app.env`（`.gitignore` 成对规则 + `*.env` + `!*.env.example`），仓库只留 `app.env.example`（逐项注明"留空降级成什么"）；`ADMIN_ENABLED=1` 而 `ADMIN_TOKEN` 为空 ⇒ **拒绝启动**（不降级放行）。
- **理由**：管理端暴露全量会话与内部指标；宁可不启动也不裸奔。
- **已发生的事故与处置**：`app.env` 曾进过公开仓库历史 ⇒ **DB 口令与 `TOKEN_SECRET_KEY` 必须轮换**（见 §9.3）。

### ADR-10 多租户演进路径（前瞻）
- **决策**：需要多租户时，`tenant_id` 贯穿"请求上下文 → 每张业务表 → 每条 SQL 条件 → 缓存键 → 知识库目录"；知识库**物理隔离**（`kb/{tenant}/`），不做行级可见性。
- **理由**：逻辑隔离只要漏写一处 WHERE 就是跨租户越权；物理隔离让"写错也读不到"。
- **前置**：先补"越权矩阵在租户维度复跑"的自动化测试（当前矩阵是用户维度）。

---

## 4. 总体架构

### 4.1 分层图

```mermaid
flowchart TB
  subgraph Client["客户端"]
    WEB["web/ 页面 + 客服抽屉"]
    APP["第三方调用方（JWT）"]
  end

  subgraph Go["Go 平台层（唯一公网入口 :8080）"]
    MW["中间件链<br/>CORS/恢复 → JWT 可选 → 限流 → trace_id"]
    BIZ["业务域<br/>用户/线路/座位/订单/支付/退票"]
    CSG["客服网关<br/>/cs/ask（JSON | SSE）"]
    INT["内网契约 /internal/*（X-Internal-Key）"]
    CAP["能力层<br/>检索 · 工具 · 答案缓存 · 引用校验"]
    OPS["运维面<br/>/readyz · /metrics · /admin/*"]
  end

  subgraph Py["Python 编排层（内网 :8001）"]
    GRAPH["LangGraph 图<br/>条件边 / 断点续跑 / 消歧反问"]
  end

  subgraph Data["存储与外部"]
    MY[("MySQL 8.4<br/>业务真相源")]
    RD[("Redis 7<br/>缓存/限流/预占")]
    MQ[("RabbitMQ<br/>DLX 延迟关单")]
    QD[("Qdrant<br/>向量派生索引")]
    PAY["支付宝（回调 + 主动查单）"]
  end

  WEB --> MW --> BIZ & CSG
  APP --> MW
  CSG --> INT
  INT <--> GRAPH
  MW --> OPS
  BIZ --> MY & RD & MQ
  CAP --> MY & RD & QD
  INT --> CAP
  BIZ --> PAY
```

### 4.2 主链路时序（下单 → 支付 → 出票 / 超时关单）

```mermaid
sequenceDiagram
  participant U as 用户
  participant G as Go
  participant R as Redis
  participant DB as MySQL
  participant Q as RabbitMQ
  participant P as 支付宝

  U->>G: POST /orders（班次+座位+乘车人）
  G->>G: JWT 解析身份（无 user_id 入参）
  G->>R: SetNX SeatHoldKey（TTL）— 仅分流
  alt 预占失败
    G-->>U: 409 座位已被占用（快速失败）
  else 预占成功
    G->>DB: 事务：条件更新座位(status=available) + 写订单/预占记录
    alt RowsAffected=0
      G->>R: 释放预占；返回 409
    else 成功
      G->>Q: 投递延迟队列（队列级 TTL=订单周期）
      G-->>U: 订单号 + 支付参数
    end
  end
  U->>P: 支付
  P->>G: POST /pay/alipay/notify（验签）
  G->>P: QueryOrder 复核状态
  G->>DB: 事务：订单 paid + 出票 + 座位 sold
  Note over Q,G: 超时分支：消息死信 → 关单消费者<br/>→ QueryOrder 复核 → 已扣款则 Refund → 座位 available
```

### 4.3 分步表（归属 / 失败降级）

| 步骤 | 归属 | 失败时 |
|---|---|---|
| 身份解析 | Go（JWT） | 无 token → 游客（受限能力，见 §5.1） |
| 限流 | Go + Redis | **Redis 不可用 → 放行并单独计数**（可用性优先于限流） |
| 座位预占 | Go + Redis | Redis 不可用 → 跳过预占，直接走数据库裁决（降级但仍正确） |
| 座位裁决 | MySQL 条件更新 | 失败 → 409/500，绝不"猜成功" |
| 延迟关单 | RabbitMQ | 消息丢失 → 兜底扫描（窗口=一个订单周期） |
| 支付复核 | Go → 支付宝 | 查单超时 → 不关单、进重试队列（宁可晚关，不误关） |
| 检索/工具 | Go（`/internal/*`） | 失败 → 转人工并**独立计数**，不降级成"没搜到" |
| 生成/编排 | Python | 不可用 → Go 降级建单 + `degraded.orchestrator=true` |

---

## 5. 业务域设计（当前 → 前瞻）

> 每域给：**现状**（✅/⚠️ + 证据）、**前瞻**（🚧 + 判据 + 复用点）。前瞻部分只写"能做且知道怎么接"的，不写空想。

### 5.1 用户与鉴权

- ✅ **现状**：注册 / 登录 / `POST /tokens/renew_access` 续期 / 改密；密码仅存 bcrypt；中间件 `authMiddleware.go`；游客以 `device_id` 为锚点。
- ✅ **限流**：登录处 IP 桶 + 用户桶双闸（`user.go:80-101`），固定窗口。
- 🚧 **前瞻**：
  - **登录风控**：失败次数画像 + 图形/短信验证码（复用限流计数，新增 `user_risk_counters` 表）。
  - **设备与会话管理**：`sessions` 已存在，扩展到"查看/踢出我的登录设备"。
  - **密码策略与找回**：找回必须走通知通道（§5.9）与一次性 token（`pwd_reset_tokens`，单次+短 TTL）。
  - 判据：越权矩阵新增「用他人 token 走找回链路」用例必须失败。

### 5.2 查询域（城市/车站/线路/班次/座位图）

- ✅ **现状**：`GET /cities`、`GET /terminals`、`GET /routes`、`GET /routes/:route_id/buses/:bus_id/seats`；Cache Aside 缓存。
- 🚧 **前瞻**：
  - **票价日历 / 最低价**：按班次批量聚合（新增物化视图 `bus_daily_price`，定时刷新；避免请求时聚合）。
  - **中转联程**：一张查询拆成"首段+接续段"两次检索，按接续时间窗过滤（复用现有 `routes` 与时间比较规则 ADR-7）。
  - **余票订阅**：座位释放事件（关单/退票）→ 通知通道（§5.9）；需先有事件出口（当前是队列，扩展 routing key 即可）。

### 5.3 下单占座

- ✅ **现状**：`POST /orders`；事务 + 条件更新裁决 + Redis 预占分流（ADR-2）；`ORDER_EXPIRE_DURATION` 决定到期时间。
- ⚠️ **已知边界**：单次下单座位数有限（防脚本占座）；同一请求 ID 幂等依赖客户端传入。
- 🚧 **前瞻**：
  - **多座位/多人订单**：一个订单挂多张票 ⇒ 座位裁决改为"批量条件更新，全部成功才提交"，任一失败即整单回滚（当前 `execTx` 已具备事务能力）。
  - **幂等键服务端化**：`request_id` 落唯一索引（`orders(request_id)` unique），重复请求返回同一订单。
  - **候补**：见 §5.13。

### 5.4 支付域

- ✅ **现状**：`/pay/alipay/notify` 回调（验签）+ `QueryOrder` 主动复核 + `Refund`；`internal/payment/mock.go` 供无沙箱开发。
- 🚧 **前瞻**：
  - **多支付渠道**：抽象 `payment.Provider`（已有接口雏形）→ 新增微信/银联实现；渠道选择落订单字段 `pay_channel`。
  - **支付超时与重试**：查单失败的订单进"待复核"表，指数退避重试（不要无限重试同一笔）。
  - **退款单**：退款从"即时调用"升级为**有状态退款单**（`refund_orders`），支持部分退款与人工介入（§5.10）。

### 5.5 出票与退改

- ✅ **现状**：支付成功后出票（`tickets`）；`DELETE /tickets/:id` 退票；退票费规则表 `penalties` 存在。
- ⚠️ **现状闸门**：`penalties` 行语义未确认 ⇒ 金额类计算**默认关闭**（`PENALTY_SEMANTICS_CONFIRMED`），关闭态行为 = FAQ + 转人工。**宁可转人工，不猜金额。**
- 🚧 **前瞻**：
  - **改签**：等价于"退旧票 + 开新票"的事务组合；难点是改签费口径与"余票不确定性"，需先确认业务规则（§13）。
  - **部分退 / 多票订单退单张**：订单维度与票维度解耦（当前已是 1 订单 N 票的雏形）。
  - **误车处理**：需发车时间与检票状态，属规则+退款组合。
  - **检票核销**：`tickets` 增 `checked_at`，闸机接口幂等核销（`WHERE checked_at IS NULL` 条件更新，与抢座同一裁决思想）。
  - 判据：退票费样例表逐行确认（12 行）后闸门才允许打开，同一链路单测 + 真实服务给出同一数字。

### 5.6 超时关单与补偿

- ✅ **现状**：DLX + 队列级 TTL（`queue/*.go:54-60`）+ 兜底扫描（`order_expiry.go:79/106`）+ 关单前 `QueryOrder` + 已扣款自动退款。
- ⚠️ **已知边界**：过期时长全局单值；兜底扫描窗口固定为一个订单周期。
- 🚧 **前瞻**：**分级过期**（如"热门班次 5 分钟、普通 15 分钟"）需要改多队列或时间轮——这是 ADR-4 的演进路径，不是当前缺陷。

### 5.7 智能客服平台层（Go 侧职责）

- ✅ **现状**：13 个内网契约端点（`internal/routes/web.go:96-108`）；`/cs/ask`（JSON | SSE）为公网入口；`/cs/*` 注册在 `authGroup` **之前**（游客可用，且避免被 JWT 中间件拦成 401）。
- ✅ **职责边界**（不可越界）：Go 管身份、限流、检索、工具执行（提槽/脱敏/熔断/超时）、答案缓存、引用校验、工单、指标；Python 管图结构与决策。**跨语言细节见关联文档 §6。**
- 🚧 **前瞻**：会话接管（坐席加入同一 `conv_id`）、多语言（同一套 KB 多语言切片）、FAQ 自动挖掘（从 `cs_messages` 高频未命中问题反推知识库缺口）。

### 5.8 工单与坐席

- ✅ **现状**：`support_tickets` + 状态机 `pending → assigned → resolved → closed`；管理端提供全量查询与**条件更新的状态流转**（非法流转 409 并附允许的下一跳）；用户侧 `GET /cs/support-tickets` 只查自己的。
- 🚧 **前瞻**：
  - **坐席工作台前端**：`web/admin.html` 已是大盘雏形，扩展为工单列表 + 详情 + 回复。
  - **分配策略**：轮询/按负载/按技能组（`support_tickets.assigned_to` 已是 int 坐席 id，直接可用）。
  - **SLA**：`first_response_at`/`resolved_at` 落列 + 超时告警（§8 指标）。
  - **会话接管**：坐席回复写回同一 `cs_messages`，前端轮询或 SSE 拉取。

### 5.9 通知通道（前瞻）

- 🚧 **设计**：抽象 `notify.Sender`（短信/邮件/站内信/IM webhook），事件驱动：出票成功、关单、退款完成、工单状态变更、余票放出。
- **判据**：通知失败**不得阻塞主链路**（异步 + 失败进重试表），且必须有"发信量"指标防刷。
- **复用点**：现有 RabbitMQ 直接加 routing key；发送模板与幂等键（`biz_type + biz_id + channel`）。

### 5.10 对账与资金（前瞻）

- 🚧 **设计**：日切对账任务拉"渠道流水"与"本地订单/退款单"比对 → 差异表（长款/短款/状态不一致）；退款失败重试与人工介入队列。
- **前置**：先有**有状态退款单**（§5.4），否则对账无对象。
- **纪律**：金额一律整数分（当前已按分存），禁止浮点。

### 5.11 风控与反黄牛（前瞻）

- 🚧 **设计**：抢座前置三层——① 账号维度频次（复用限流计数）× ② 设备指纹（`device_id` + UA/IP 画像）× ③ 行为序列（同 IP 高频同班次）。命中 → 排队或验证码。
- **复用点**：ADR-2 已把裁决权放在数据库，风控**只需在数据库之前拦截**，不影响正确性（只影响体验与公平性）。

### 5.12 运营后台与报表（前瞻）

- 🚧 **设计**：班次/定价/座位图管理（写接口 + 审计日志）、销售看板（按班次/线路/日期聚合，用物化视图而非实时 COUNT）、客服质量看板（转人工率、未命中问题 TOP N、工单 SLA）。
- **命名**：`admin/*` 已有鉴权分组，直接扩。

### 5.13 候补购票（前瞻，最需要"复用裁决权"的一个）

- 🚧 **设计**：座位释放（关单/退票）→ 触发候补队列**有序放行**：按入队顺序给候选用户一个短 TTL 的"优先购买权"（Redis），持权者下单仍走 ADR-2 的条件更新裁决 ⇒ **候补只是改变"谁先试"，不改变"谁赢"**。
- **判据**：候补 + 正常抢座并发下，超卖仍为 0；放行顺序可复现（同输入同顺序）。
- **前置**：座位释放事件必须先有出口（当前关单链路已有，退票链路待补）。

### 5.14 多租户（前瞻）

- 见 ADR-10。**前置**：租户维度越权矩阵自动化。

---

## 6. 接口规范

### 6.1 三面划分

| 面 | 前缀 | 鉴权 | 暴露 |
|---|---|---|---|
| 用户面 | `/`、`/orders`、`/user/*`、`/cs/*` | JWT（`/cs/ask` 游客可用） | 公网 |
| 内网面 | `/internal/*` | `X-Internal-Key` | **仅内网**（compose 不映射端口） |
| 管理面 | `/admin/*`、`/metrics` | `Authorization: Bearer <ADMIN_TOKEN>` | 公网可达但**启用即强制 token** |

### 6.2 调用纪律（跨语言契约）

| 纪律 | 内容 |
|---|---|
| 错误语义 | 按 `error.kind` 决策，**不解析错误文案** |
| 超时分级 | 分类/工具路由 1s（可重试 1 次）；检索/工具执行 1.5s（不重试）；LLM 20s |
| 行尾 | SSE 出口统一 `\n`；**空行是帧分隔符，必须转发** |
| 契约显式 | `stream` 必须显式传（默认非流式），避免"拿到 SSE 当 JSON 解析" |
| 代理 | 内网调用一律 `trust_env=false`（本机代理会把 127.0.0.1 也转发） |

### 6.3 错误码（Go 侧统一形态）

| code | 语义 | 客户端应做 |
|---|---|---|
| 400 | 参数错（含 `conv_id`/`trace_id` 缺失） | 修正请求 |
| 401 | 未认证 / token 失效 | 重新登录 |
| 403 | 有身份但无权（**跨用户查询不用 403**，统一"未查询到"防探测） | — |
| 404 | 资源不存在或**不属于当前用户** | 提示"未查询到" |
| 409 | 冲突（座位被占 / 工单非法流转） | 刷新后重试 |
| 429 | 限流 | 依 `Retry-After` 退避 |
| 503 | 依赖不可用（**客服链路不返回 503**，改降级建单） | 展示降级提示 |

### 6.4 示例：SSE 事件序列（客服流式）

```
event: meta    data: {"conv_id":"...","trace_id":"..."}
event: delta   data: {"text":"支持"}
event: delta   data: {"text":"支付宝"}
event: done    data: {"answer":"...","category":"order","source":"kb","transfer":false,
                      "degraded":{"vector":true,"llm":true,"orchestrator":false},
                      "vector_mode":"ngram","cache_hit":false}
```
（末尾空行是帧分隔符，**Go 出口必须保留**。）

---

## 7. 数据模型（要点 + 归属）

- **迁移命名**：`00000N_描述.up.sql` / `.down.sql`，只增不改（已用过 5 组）。
- **归属**：见 §2.3。**Python 只拥有 `checkpoints*` 系表**，Go 不读不写；`cs_messages` 是**可重建的审计视图**（最终一致），不是第二真相源——这条要写进评审记录，免得被当成双写缺陷。
- **索引要点**：`orders(order_no)` 唯一、`orders(user_id, created_at)`、`bus_seats(bus_id, status)`、`cs_messages(conv_id, created_at)`、`support_tickets(status, created_at)`。
- **金额**：一律 `int` 分。
- **时间**：`DATETIME`（不用 `TIMESTAMP`，避免时区隐式转换），比较下沉 SQL（ADR-7）。

---

## 8. 可观测与运维

### 8.1 三件套

| 面 | 实现 | 证据 |
|---|---|---|
| 指标 | `/metrics`（Prometheus 文本，手写 exposition）+ `/admin/metrics`（JSON） | `internal/metrics/`、`handlers/ops.go` |
| 健康 | `/readyz` 逐项布尔（db/redis/kb_chunks/llm/embedding/orchestrator/qdrant_points），**不回显任何密钥** | `handlers/ops.go` |
| 追踪 | `X-Trace-Id` 响应头 + 一行一条结构化日志（`evt=cs_turn` 含 category/source/cache_hit/transfer_path/tool/top1_cos/stage_ms/tokens） | `handlers/cs.go` |

### 8.2 降级矩阵（必须诚实可见）

| 依赖挂 | 行为 | 可见性 |
|---|---|---|
| Redis | 限流放行、缓存失效、预占降级为直连数据库裁决 | 计数 `redis_unavailable_total` + 日志 WARN |
| RabbitMQ | 关单只靠兜底扫描 | 指标 + 日志 |
| 编排层（Python） | Go 降级建单，返回标准话术 | `degraded.orchestrator=true` + 独立计数 |
| LLM / embedding key | 生成走 mock、检索走字面替身 | `degraded.llm/vector=true` + `vector_mode=ngram` |
| Qdrant | 检索退回 BM25-only | `readyz.qdrant_points=-1`（**不报 0**，否则"没数据"会伪装成"正常空库"） |

### 8.3 安全与脱敏

- 订单号/手机号出口脱敏（工具层统一出口，非各分支各写一遍）。
- 工具层每条 SQL 强制 `WHERE user_id = ?`；跨用户返回同一话术。
- 管理面与内网面分别用 `ADMIN_TOKEN` / `X-Internal-Key`（恒定时间比较）。
- 日志**绝不打印**密钥/明文凭据；配置摘要只报布尔（如"LLM 生成：是/否"）。

---

## 9. 部署与配置

### 9.1 本地/容器

- `docker-compose.yml`：`mysql`(3307) / `redis` / `rabbitmq` / `qdrant` / `app` / `orchestrator`；**`orchestrator` 不映射宿主机端口**（ADR-1）。
- 口令一律变量注入（`${DB_PASSWORD}`），compose 依赖仓库外/被忽略的 `.env`。
- Go 启动必配 `PYTHON_BASE_URL`，否则**每轮静默降级**——已加启动 `log.Error` 显式报警。

### 9.2 健康检查

`/readyz`（Go）与编排层 `/healthz`（含 `capability` / `checkpointer` / `graph` 三项）——`checkpointer:false` 表示退化为无状态执行，必须告警。

### 9.3 密钥治理现状（**待办**）

| 项 | 状态 |
|---|---|
| `app.env` 已移出版本控制 | ✅（本地保留，`.gitignore` 成对规则） |
| 历史提交里的旧值 | ⚠️ **DB 口令、`TOKEN_SECRET_KEY` 需轮换**（曾进公开仓库）；`ALIPAY_PRIVATE_KEY` 从未公开 |
| 自查清单 | 已补「URL 内嵌凭据」形态（`mysql://user:pass@host`） |

---

## 10. 评测与验收

### 10.1 Go 侧门槛

| 项 | 判据 | 现状 |
|---|---|---|
| 单元测试 | `go test ./internal/...` 全绿 | ✅ 6 个包 |
| 越权矩阵 | 跨用户/无 token/伪造 token 三组全过 | ✅ 22 项（M2） |
| 并发一致性 | 抢座压测超卖 = 0 | ✅ 已有超卖/并发测试；**压测参数待标定** |
| 关单链路 | DLX 与兜底扫描各能独立关单 | ✅ 可复现 |
| 工具降级 | 闸门关闭**不计入**熔断，原因不漂移 | ✅ 有单测 `TestGateClosedDoesNotTripBreaker` |
| 缓存 | 命中、穿透、击穿各有计数 | ✅ |

### 10.2 数字引用纪律

- **任何影响口径的变更后必须重跑评测再引用数字**；基线未实测前留「待实测填入」。
- 客服质量类数字（分类准确率/检索命中率）当前是 mock + 字面 embedding 下的结果，**不可对外引用**，须待 LLM/embedding 选型（§13 第 1 条）后重跑。

---

## 11. 实施计划（前瞻部分）

| 批次 | 内容 | 前置 | 验收 |
|---|---|---|---|
| G3-1 | 改签 + 部分退（事务组合） | 改签费口径确认 | 改签后原票作废、新票可用、金额单据一致 |
| G3-2 | 通知通道抽象 + 出票/关单事件 | — | 通知失败不阻塞主链路；有发信计数 |
| G3-3 | 坐席工作台前端 + SLA 列 | 管理端已有基础 | 工单可在前端完成 assign→resolve；SLA 超时可见 |
| G3-4 | 有状态退款单 + 日对账 | 退款单表 | 对账差异表能产出长款/短款各 1 例 |
| G4-1 | 候补购票 | 座位释放事件出口 | 候补与抢座并发超卖仍为 0、放行顺序可复现 |
| G4-2 | 风控三层 + 验证码 | — | 同 IP 高频脚本被拦；正常用户不受影响 |
| G4-3 | 检票核销 | `tickets.checked_at` | 重复核销幂等（`WHERE checked_at IS NULL`） |
| G4-4 | 运营后台 + 报表 | 物化视图 | 报表查询不触发全表扫描（给出执行计划） |
| G5-1 | 多租户 | 租户维度越权矩阵 | 越权矩阵全过 |

---

## 12. 风险与对策

| 风险 | 影响 | 对策 |
|---|---|---|
| Redis 单点（限流/缓存/预占都依赖） | 全站降级 | 已定义降级行为（§8.2）；预占失效不影响正确性（ADR-2） |
| 热点班次抢座打到数据库 | 抖动/超卖错觉 | 前置限流与风控（§5.11）；数据库裁决保证正确性 |
| 队列级 TTL 只能全局单值 | 无法分级过期 | 演进：多队列/时间轮（ADR-4 后果） |
| 回调伪造 | 资金损失 | 必须验签 + 主动查单（ADR-5） |
| 跨语言一跳 | 延迟与失败面 | 分级超时 + 降级建单 + 独立计数（ADR-8） |
| 密钥历史泄露 | 账号被冒用 | 轮换（§9.3） |
| 评测数字不可用期间对外引用 | 诚信风险 | §10.2 纪律 |

---

## 13. 待确认事项

| # | 事项 | 影响 | 状态 |
|---|---|---|---|
| 1 | **LLM / embedding 选型 + 阈值标定** | 检索与生成质量、缓存语义等价判定 | 阻塞（不解决则质量数字不可引用） |
| 2 | **`penalties` 退票费行语义（12 行样例表）** | 金额类能力上线 | 待业务确认；当前闸门关闭 |
| 3 | 游客能否建单 | 合规与滥用 | 默认允许 + 启动告警，可一键翻转 |
| 4 | 改签费/部分退口径 | G3-1 能否开工 | 待确认 |
| 5 | 对账口径（日切时间、差异容忍） | G3-4 | 待确认 |
| 6 | 通知渠道清单（短信/邮件/IM） | G3-2 | 待确认 |
| 7 | 实例化知识的权威来源与更新流程 | 知识库能否铺量 | **不解决不允许上线** |

---

## 附录 A 代码落位

```text
internal/
├── api/
│   ├── handlers/   admin.go  admin 写接口与全量查询
│   │               bus.go city.go route.go terminal.go   查询域
│   │               cs.go        /cs/* 公网入口（JSON | SSE）
│   │               internal.go  /internal/* 契约实现（13 端点）
│   │               ops.go       /readyz /metrics
│   │               orders.go ticket.go user.go token.go  交易与账号
│   ├── middleware/ authMiddleware.go internalKey.go adminAuth.go csRateLimit.go
│   └── cs.go       AI 客服组件装配（检索/工具/缓存/LLM 网关）
├── ai/             kb/ classify/ cite/ llm/ tools/ answercache/
├── cache/          redis.go（预占、限流、互斥重建、TTL 抖动）
├── db/
│   ├── migration/  000001..000005（up/down）
│   └── sqlc/       Store(execTx) / Queries / *_extra.go（RawDB 与聚合查询）
├── metrics/        Prometheus 文本 + 分段延迟 + 惰性 gauge
├── payment/        alipay.go / mock.go
├── queue/          RabbitMQ + DLX 延迟队列
├── routes/web.go   路由注册（**顺序即安全边界**：/cs/* 与 /internal/* 在 authGroup 之前）
├── token/ util/ worker/order_expiry.go bootstrap/
cmd/kb-sync/        知识库入库 + 缓存失效
web/                页面 + assets/js/cs.js（客服抽屉）+ admin.html（运营大盘）
```

> **路由注册顺序是安全边界**：`/internal/*`（内网密钥）与 `/cs/*`（允许游客）必须在 `authGroup`（JWT）**之前**注册，否则会被 JWT 中间件截成 401。这是本项目踩过两次的坑。

## 附录 B 与《智能AI客服系统-技术设计文档（V1.0-r5）》的关系

| 主题 | 以哪份为准 |
|---|---|
| Go 平台层职责、票务业务域、并发/缓存/队列/支付、前瞻功能域 | **本文件** |
| 编排层图结构（16 条件边）、分类仲裁、检索与 RAG 细节、记忆与答案缓存、工具层语义、评测集设计 | 关联文档（本文件只述 Go 侧边界与契约） |
| 实现状态与实测数字 | `docs/M0~M4-实现报告.md`（**唯一取数来源**） |
