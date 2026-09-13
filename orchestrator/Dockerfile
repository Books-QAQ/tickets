# 编排层镜像（§16.1）：python:3.12-slim + uv 安装**锁定依赖** + uvicorn
#
# 说明：
#  - 用 uv.lock 安装（`--frozen`）保证与本地开发完全一致；镜像里不装 dev 依赖；
#  - 只暴露 8001 给容器内网（compose 不映射端口）；
#  - 健康检查打 /healthz（readyz 在 Go 侧，探针由 Go 统一对外）。
FROM python:3.12-slim

ENV PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    UV_SYSTEM_PYTHON=1 \
    UV_COMPILE_BYTECODE=1

# uv：从官方镜像拷二进制，避免 pip 装 uv 带来的额外层
COPY --from=ghcr.io/astral-sh/uv:0.9.9 /uv /usr/local/bin/uv

WORKDIR /app

# 先装依赖（利用层缓存：只是改了代码时不必重装依赖）
COPY pyproject.toml uv.lock ./
RUN uv sync --frozen --no-dev --no-install-project

COPY app ./app

EXPOSE 8001
# 注：编排层**不签发/不校验用户身份**（身份由 Go 解析后注入），所以这里不需要任何 JWT 密钥
CMD ["uv", "run", "--frozen", "--no-dev", "uvicorn", "app.main:create_app", "--factory", "--host", "0.0.0.0", "--port", "8001"]
