"""编排层配置（全部来自环境变量；密钥只在 Go，这里只放内网地址与内网密钥）。"""

from __future__ import annotations

import os
from dataclasses import dataclass


@dataclass(frozen=True)
class Settings:
    # Go 能力层
    go_base_url: str = "http://127.0.0.1:8080"
    internal_key: str = ""
    # LLM：base_url 指向 Go 网关的 OpenAI 兼容端点（.../internal/v1），密钥不进编排层
    llm_base_url: str = ""
    llm_api_key: str = ""
    llm_model: str = "mock"
    # 预算
    round_budget_ms: int = 8000
    top_k: int = 5
    # 超时（§6.2）
    timeout_classify_s: float = 1.0
    timeout_retrieve_s: float = 1.5
    timeout_tool_s: float = 1.5
    timeout_llm_s: float = 20.0

    @classmethod
    def from_env(cls) -> Settings:
        def g(key: str, default: str = "") -> str:
            return os.environ.get(key, default)

        return cls(
            go_base_url=g("GO_BASE_URL", "http://127.0.0.1:8080").rstrip("/"),
            internal_key=g("INTERNAL_KEY"),
            llm_base_url=g("LLM_BASE_URL").rstrip("/"),
            llm_api_key=g("LLM_API_KEY"),
            llm_model=g("LLM_MODEL", "mock"),
            round_budget_ms=int(g("ROUND_BUDGET_MS", "8000")),
            top_k=int(g("TOP_K", "5")),
        )

    def gateway_chat_base(self) -> str:
        """langchain-openai 会在 base_url 后拼 /chat/completions，所以这里给到 /internal/v1。"""
        return f"{self.go_base_url}/internal/v1"
