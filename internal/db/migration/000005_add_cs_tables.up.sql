-- 000005_add_cs_tables.up.sql
-- 智能AI客服系统 · 业务表（Go 侧 golang-migrate 管理）
-- 依据：docs/智能AI客服系统-技术设计文档（V1.0）.md §13.1
-- 注意：checkpoint 系列表由 Python 侧 AIOMySQLSaver.setup() 创建，此处不建不改（ADR-2）

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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- 知识块（文本与元数据 = 真相源；向量不落 MySQL，见 ADR-10）
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- 能力台账（7.9：41 子场景 × 状态；生成层据此决定是否加边界声明）
CREATE TABLE cs_capabilities (
    id INT AUTO_INCREMENT PRIMARY KEY,
    sub_scenario VARCHAR(64) NOT NULL UNIQUE,
    category VARCHAR(32) NOT NULL,
    status VARCHAR(16) NOT NULL,
    system_entry VARCHAR(255) NULL,                   -- supported 必填：前端路径/接口/工具名
    owner VARCHAR(64) NULL,
    target_milestone VARCHAR(32) NULL,                -- roadmap 必填（M5-x）
    source VARCHAR(512) NULL,                         -- 行业权威来源
    note VARCHAR(512) NULL,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    INDEX idx_caps_status (status),
    CONSTRAINT chk_caps_status CHECK (status IN ('supported','roadmap','industry'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- 客服工单（人工转接）
CREATE TABLE support_tickets (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    ticket_no VARCHAR(32) NOT NULL UNIQUE,
    conv_id CHAR(36) NULL,
    user_id INT NULL,
    order_no VARCHAR(64) NULL,
    category VARCHAR(32) NOT NULL,
    path VARCHAR(32) NOT NULL,       -- deterministic|threshold|model|tool_unavailable|orchestrator_unavailable|capability_absent
    status VARCHAR(16) NOT NULL DEFAULT 'pending',
    summary TEXT NOT NULL,
    assigned_to INT NULL,                             -- 为坐席工作台预留
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    resolved_at DATETIME NULL,
    INDEX idx_st_status (status, created_at),
    INDEX idx_st_user (user_id, created_at),
    CONSTRAINT chk_st_status CHECK (status IN ('pending','assigned','resolved','closed')),
    FOREIGN KEY (conv_id) REFERENCES cs_conversations(id) ON DELETE SET NULL,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE SET NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- 答案反馈
CREATE TABLE cs_feedback (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    trace_id CHAR(36) NOT NULL,
    user_id INT NULL,
    rating VARCHAR(8) NOT NULL,                       -- up|down
    comment VARCHAR(512) NULL,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE KEY uq_cs_feedback (trace_id, user_id),
    INDEX idx_cs_fb_rating (rating, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
