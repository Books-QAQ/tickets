# 票务预订系统

这是一个基于 `Go (Golang)` 构建的票务预订系统后端项目，演示了用户注册登录、线路查询、下单占座、支付出票与退票等完整业务流程。

## 项目简介

本项目面向客运票务场景，重点展示以下能力：

- 用户注册、登录与 JWT 鉴权
- 线路与班次查询
- 下单占座、支付出票与退票
- 基于事务与条件更新的并发一致性控制
- 基于 Redis 的缓存、限流、座位预占与订单链路闭环

## 技术栈

- 语言：`Go`
- Web 框架：`Fiber`
- 数据库：`MySQL 8`
- 缓存与队列：`Redis`
- 认证方式：`JWT`
- 容器化：`Docker` / `Docker Compose`
- 数据库迁移：`golang-migrate`
- SQL 映射：`sqlc`

## 项目结构

```text
tickets/
├── internal/                # 核心业务代码
│   ├── api/                 # HTTP 接口处理
│   ├── cache/               # Redis 缓存与队列能力
│   ├── db/                  # 数据库连接、迁移与 sqlc 代码
│   ├── routes/              # 路由注册
│   ├── token/               # JWT 生成与校验
│   ├── util/                # 配置与通用工具
│   └── worker/              # 订单过期关单与补出票
├── scripts/                 # 初始化与辅助脚本
├── tests/                   # 测试代码
├── web/                     # 前端页面
├── Dockerfile
├── docker-compose.yml
├── README.md
├── app.env
└── main.go
```

## 环境准备

### 1. 克隆项目

```bash
git clone https://github.com/Books-QAQ/tickets.git
cd tickets
```

### 2. 配置环境变量

根据本地环境修改 `app.env`，至少确认以下配置正确：

- `DB_HOST`
- `DB_PORT`
- `DB_DATABASE`
- `DB_USERNAME`
- `DB_PASSWORD`
- `REDIS_HOST`
- `REDIS_PORT`

## 启动方式

### 方式一：本地运行 Go 服务

先启动 MySQL：

```bash
docker compose up -d mysql
```

再启动 Go 服务：

```bash
go run main.go
```

说明：

- Compose 中的 MySQL 映射到宿主机 `localhost:3307`
- 这样可以避免与本机已安装的 `3306` 端口 MySQL 冲突
- 应用启动时会自动执行数据库迁移

### 方式二：使用 Docker Compose 启动完整环境

```bash
docker compose up --build
```

## 访问方式

服务启动后，可以通过以下地址访问：

- 首页：`http://localhost:8080`
- 订票页：`http://localhost:8080/booking`

## 初始化演示数据

如果需要给前端页面导入示例城市、车站、线路、班次和座位数据，可执行：

```bash
mysql -h 127.0.0.1 -P 3307 -u root -p tickets < scripts/seed_demo_data.sql
```

该脚本支持重复执行，针对命名演示数据做了幂等处理。

导入后大致会生成：

- 5 个城市
- 7 个车站
- 8 条线路
- 9 个班次
- 每个演示班次 16 个座位

## 批量创建用户

项目提供了批量注册脚本。先确保服务已经启动，再准备一个包含以下列的 CSV 文件：

- `username`
- `password`
- `full_name`

执行命令：

```powershell
.\scripts\bulk_register_users.ps1 -CsvPath .\scripts\users.example.csv
```

脚本会逐行调用 `POST /register` 接口，因此会复用系统现有的参数校验与密码哈希逻辑。

## 测试说明

项目中包含多类测试：

- 购票事务一致性测试
- 超卖测试
- 并发购票测试
- JWT 鉴权中间件测试

例如：

```powershell
go test -v .\tests\purchase_tx_test.go
go test -v .\tests\oversell_test.go
go test -v .\tests\concurrency_test.go
go test -v .\tests\authMiddleware_test.go
```

## 功能特点

### 并发一致性

- 使用 MySQL 事务保证票据、预定记录、座位状态一致
- 使用条件更新防止同一座位被重复购买
- 下单时使用 Redis 临时预占锁快速分流，最终以 MySQL 条件更新裁决座位归属

### 缓存与限流

- 基于 Redis 实现 `Cache Aside` 查询缓存
- 登录接口使用固定窗口限流
- ä¸åä½¿ç¨ Redis é¢å é + MySQL æ¡ä»¶æ´æ°é²è¶å
- 订单过期由 RabbitMQ 延迟队列触发关单，定时扫描器兜底双保险

## 备注

如果你在本机使用 `go run main.go` 运行服务，请不要同时启动 Compose 里的 `app` 服务，否则会同时竞争 `8080` 端口。
