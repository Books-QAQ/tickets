# Ticket Master 业务流程图

下面按核心业务拆分流程图，使用 `Mermaid` 语法表示，适合直接放进 Markdown、Typora、Obsidian、GitHub 或 Mermaid Live Editor。

## 1. 用户注册

```mermaid
flowchart TD
    A[用户提交注册信息] --> B[后端解析请求参数]
    B --> C{参数校验是否通过}
    C -- 否 --> D[返回 400 参数错误]
    C -- 是 --> E[密码加密]
    E --> F[写入 users 表]
    F --> G{用户名是否已存在}
    G -- 是 --> H[返回 403 用户名重复]
    G -- 否 --> I[注册成功]
```

## 2. 用户登录

```mermaid
flowchart TD
    A[用户提交用户名和密码] --> B[后端解析请求]
    B --> C[固定窗口限流<br/>按 IP 和用户名]
    C --> D{是否超限}
    D -- 是 --> E[返回 429 登录过于频繁]
    D -- 否 --> F[查询 users 表]
    F --> G{用户是否存在}
    G -- 否 --> H[返回 401 用户名或密码错误]
    G -- 是 --> I[校验密码]
    I --> J{密码是否正确}
    J -- 否 --> H
    J -- 是 --> K[生成 access token 和 refresh token]
    K --> L[写入 sessions 表]
    L --> M[返回登录成功和 token]
```

## 3. 线路与班次查询

```mermaid
flowchart TD
    A[用户选择出发站 到达站 日期] --> B[请求 routes 接口]
    B --> C[生成缓存 key]
    C --> D{Redis 是否命中}
    D -- 是 --> E[直接返回缓存结果]
    D -- 否 --> F[查询 routes buses bus_seats 等表]
    F --> G[组装班次和余票结果]
    G --> H[写入 Redis 缓存]
    H --> I[返回班次列表]
```

## 4. 同步购票

```mermaid
flowchart TD
    A[用户选择班次和座位] --> B[请求 purchase 接口]
    B --> C[JWT 鉴权]
    C --> D[令牌桶限流]
    D --> E{是否超限}
    E -- 是 --> F[返回 429 购票过于频繁]
    E -- 否 --> G[校验班次与座位关系]
    G --> H[检查 seat 是否 available]
    H --> I[检查班次是否已开售]
    I --> J[查询当前用户]
    J --> K[执行 PurchaseTicketTx]
    K --> L[事务内更新座位状态]
    L --> M[写入 seat_reservations]
    M --> N[写入 tickets]
    N --> O[删除对应班次缓存]
    O --> P[返回购票成功]
```

## 5. 异步购票削峰

```mermaid
flowchart TD
    A[用户选择班次和座位] --> B[请求 purchase_async 接口]
    B --> C[JWT 鉴权]
    C --> D[令牌桶限流]
    D --> E[校验班次 座位 开售状态]
    E --> F[Redis SETNX 临时预占座位]
    F --> G{是否预占成功}
    G -- 否 --> H[返回 409 座位被其他请求占用]
    G -- 是 --> I[写入任务状态 queued]
    I --> J[消息入 Redis 队列 queue purchase]
    J --> K[接口立即返回 已排队]
    K --> L[后台 worker 阻塞消费队列]
    L --> M[校验预占锁是否仍归属当前请求]
    M --> N[执行 PurchaseTicketTx]
    N --> O{事务是否成功}
    O -- 否 --> P[释放座位预占锁]
    P --> Q[任务状态改为 failed]
    O -- 是 --> R[释放座位预占锁]
    R --> S[删除班次缓存]
    S --> T[任务状态改为 succeeded]
```

## 6. 座位预定

```mermaid
flowchart TD
    A[用户选择班次和座位] --> B[请求 reserve 接口]
    B --> C[JWT 鉴权]
    C --> D[令牌桶限流]
    D --> E[校验班次与座位]
    E --> F[检查座位是否可用]
    F --> G[检查班次是否已开售]
    G --> H[查询当前用户]
    H --> I[执行 ReserveTicketTx]
    I --> J[写入预定记录]
    J --> K[更新座位状态]
    K --> L[删除班次缓存]
    L --> M[返回预定成功]
```

## 7. 退票

```mermaid
flowchart TD
    A[用户点击退票] --> B[请求 DELETE tickets id]
    B --> C[JWT 鉴权]
    C --> D[查询当前用户]
    D --> E[查询票据详情]
    E --> F{票据是否存在}
    F -- 否 --> G[返回 404]
    F -- 是 --> H{是否属于当前用户}
    H -- 否 --> I[返回 403 无权限]
    H -- 是 --> J{是否已取消}
    J -- 是 --> K[返回 400 已取消]
    J -- 否 --> L[执行 CancelTicketTx]
    L --> M[更新票据状态为 canceled]
    M --> N[恢复座位为 available]
    N --> O[删除班次缓存]
    O --> P[返回退票成功]
```

## 8. 个人中心查票据

```mermaid
flowchart TD
    A[用户进入个人中心] --> B[请求 user tickets 接口]
    B --> C[JWT 鉴权]
    C --> D[根据 token 查当前用户]
    D --> E[查询用户关联 tickets]
    E --> F[关联 buses 和座位信息]
    F --> G[返回票据列表]
```

## 9. 自动补充班次数据

```mermaid
flowchart TD
    A[服务启动] --> B[执行 EnsureDemoData]
    B --> C[补充城市和车站]
    C --> D[补充线路]
    D --> E[生成未来 10 天班次]
    E --> F[生成每个班次的座位]
    F --> G[设置 sale_open_at 开售时间]
    G --> H[启动每小时巡检定时器]
    H --> I[定时补漏未来班次和座位]
```

## 10. 购票阶段缓存与数据库协同流程

```mermaid
flowchart TD
    A[用户在前端查看班次与余票] --> B[请求 routes 接口]
    B --> C{Redis 班次缓存命中?}
    C -- 是 --> D[返回班次与余票展示]
    C -- 否 --> E[查询 MySQL routes buses bus_seats]
    E --> F[组装班次结果并写入 Redis 缓存]
    F --> D

    D --> G[用户选择班次和座位后发起购票]
    G --> H[JWT 鉴权]
    H --> I[Redis 令牌桶限流]
    I --> J{是否超限?}
    J -- 是 --> K[返回 429]
    J -- 否 --> L[校验班次 座位 开售状态]

    L --> M{是否走异步购票?}
    M -- 是 --> N[Redis SETNX 临时预占座位]
    N --> O{预占是否成功?}
    O -- 否 --> P[返回 409 座位被占用]
    O -- 是 --> Q[请求写入 Redis 队列]
    Q --> R[Worker 消费队列]
    R --> S[进入 MySQL 购票事务]

    M -- 否 --> S

    S --> T[条件更新 bus_seats<br/>where status = available]
    T --> U{更新影响行数是否为 1?}
    U -- 否 --> V[事务失败<br/>说明座位已被其他并发请求抢走]
    U -- 是 --> W[写入 seat_reservations]
    W --> X[写入 tickets]
    X --> Y[提交事务]

    Y --> Z[删除对应 routes 缓存]
    Z --> AA[返回购票成功]

    V --> AB{是否异步模式?}
    AB -- 是 --> AC[释放 Redis 临时预占锁]
    AC --> AD[任务状态置为 failed]
    AB -- 否 --> AE[返回购票失败]

    Y --> AF{是否异步模式?}
    AF -- 是 --> AG[释放 Redis 临时预占锁]
    AG --> AH[任务状态置为 succeeded]
    AF -- 否 --> AA
```

### 说明

- Redis 缓存只负责加速班次查询和削峰控流，不直接决定最终能否买到票。
- 真正防超卖的关键在 MySQL 事务内的条件更新：只有一个请求能把座位从 `available` 改为 `purchased` 或 `reserved`。
- 购票成功后删除班次缓存，下一次查询会重新从数据库加载最新余票。
