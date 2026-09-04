# Tickets 业务流程图

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

## 4. 下单（创建订单）

```mermaid
flowchart TD
    A[用户选择班次和座位] --> B[请求 orders 接口]
    B --> C[JWT 鉴权]
    C --> D[校验班次与座位关系]
    D --> E[检查 seat 是否 available]
    E --> F[检查班次是否已开售]
    F --> G[Redis SETNX 临时预占座位<br/>key=seat hold busID seatID]
    G --> H{是否预占成功}
    H -- 否 --> I[返回 409 座位被其他请求占用]
    H -- 是 --> J[MySQL 事务]
    J --> K[条件更新 bus_seats<br/>available 改为 reserved]
    K --> L{影响行数是否为 1}
    L -- 否 --> M[回滚 释放 Redis 锁<br/>返回 409 座位不可用]
    L -- 是 --> N[写入 orders 表<br/>status=pending]
    N --> O[提交事务 返回订单号]
    O --> P[发布关单延迟消息到 RabbitMQ<br/>DLX+TTL 15 分钟]
    P --> Q[返回 201 下单成功]
```

## 5. 支付与出票

```mermaid
flowchart TD
    A[用户对待支付订单发起支付] --> B{支付渠道}
    B -- mock --> C[条件更新 orders<br/>pending 改为 paid<br/>且 expired_at 大于当前时间]
    B -- alipay --> D[支付宝预下单 precreate<br/>生成付款二维码]
    D --> E[前端展示二维码 用户扫码付款]
    E --> F[主动查单 trade query<br/>或支付宝异步通知 notify]
    F --> G{支付宝是否已付款}
    G -- 否 --> H[保持 pending 等待用户付款]
    G -- 是 --> C
    C --> I{影响行数是否为 1}
    I -- 否 --> J[已是 paid 重复支付<br/>直接返回 不重复出票]
    I -- 是 --> K[条件更新座位 reserved 改为 purchased]
    K --> L[写入 seat_reservations]
    L --> M[写入 tickets]
    M --> N[提交事务 释放 Redis 锁]
    N --> O[删除对应班次缓存]
    O --> P[返回出票成功]
```

## 6. 订单超时关单

```mermaid
flowchart TD
    A[RabbitMQ 延迟消息到期<br/>或兜底扫描器发现过期订单] --> B[读取订单]
    B --> C{订单状态是否为 pending}
    C -- 否 --> D[已支付或已关闭<br/>直接跳过]
    C -- 是 --> E{是否为支付宝渠道}
    E -- 是 --> F[主动查询支付宝交易状态]
    F --> G{是否已付款}
    G -- 是 --> H[补出票 settle<br/>钱已扣必须出票 避免悬空]
    G -- 否 --> I[执行关单事务]
    F -- 查单失败 --> J[保守跳过<br/>下一轮再查 避免误关已付款订单]
    E -- 否 --> I
    I --> K[条件更新 orders<br/>pending 改为 canceled]
    K --> L{影响行数是否为 1}
    L -- 否 --> M[竞态 支付已先到达<br/>跳过 不释放座位]
    L -- 是 --> N[条件更新座位 reserved 改为 available]
    N --> O[释放 Redis 锁 删除班次缓存]
    O --> P[关单完成]
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

    D --> G[用户选择班次和座位后发起下单]
    G --> H[JWT 鉴权]
    H --> I[Redis SETNX 临时预占座位]
    I --> J{预占是否成功?}
    J -- 否 --> K[返回 409 座位被占用]
    J -- 是 --> L[进入 MySQL 下单事务]

    L --> M[条件更新 bus_seats<br/>available 改为 reserved]
    M --> N{更新影响行数是否为 1?}
    N -- 否 --> O[回滚并释放 Redis 锁<br/>返回 409 座位已被抢走]
    N -- 是 --> P[写入 orders 表<br/>status=pending]
    P --> Q[提交事务 发布关单延迟消息到 RabbitMQ]

    Q --> R[用户对订单发起支付]
    R --> S{是否支付宝渠道}
    S -- 是 --> T[precreate 生成二维码 用户扫码付款]
    T --> U[查单或异步通知确认已付款]
    U --> V[进入出票事务]
    S -- 否 --> V

    V --> W[条件更新 orders<br/>pending 改为 paid]
    W --> X{影响行数是否为 1}
    X -- 否 --> Y[重复支付 已 paid<br/>直接返回 不重复出票]
    X -- 是 --> Z[条件更新座位 reserved 改为 purchased]
    Z --> AA[写入 seat_reservations]
    AA --> AB[写入 tickets]
    AB --> AC[提交事务 释放 Redis 锁]

    AC --> AD[删除对应 routes 缓存]
    AD --> AE[返回出票成功]
```

### 说明

- Redis 缓存只负责加速班次查询，不直接决定最终能否买到票；Redis 预占锁只做快速分流，也不承担正确性。
- 真正防超卖的关键在 MySQL 事务内的条件更新：下单时只有一个请求能把座位从 `available` 改为 `reserved`，支付时只有一个请求能把订单从 `pending` 改为 `paid`。
- 订单过期由 RabbitMQ 延迟消息触发关单，兜底扫描器双保险，关单/支付在数据库层通过 `status='pending'` 条件互斥。
- 购票成功后删除班次缓存，下一次查询会重新从数据库加载最新余票。
