# kb/ 知识库目录规范（铁路客运行业知识库）

本目录是智能AI客服的**知识源**。规范依据 `docs/智能AI客服系统-技术设计文档（V1.0）.md` 的 §7.1（目录与元数据）、§7.2（两种形态）、§7.9（能力台账）、§5.9（能力边界声明）。

> 定位：**铁路客运行业知识库，覆盖完整业务域，独立于代码**（决策见文档 19.12）。
> 平台尚未实现的功能**照写行业规则**，但必须靠能力台账 + 生成层边界声明兜住——**绝不允许讲成本平台能力**。

## 1. 目录结构

```
kb/
├── capability.yaml           # 能力台账（41 子场景的唯一事实来源）—— 必读
├── booking/                  # 车票预订
├── order/                    # 订单与支付
├── refund/                   # 退票改签
├── account/                  # 账号与身份
├── travel/                   # 乘车与车站服务
├── app/                      # 系统与技术问题
├── policy/                   # 投诉与政策
└── stations/                 # 站点实例化规则（prose 主体，规模来源）
    └── <站名>.md
```

- 一级目录 = 分类路由的 7 类闭集，**不得新增**（新增类要动分类器词表与评测集）。
- 二级目录建议按 `sub_scenario` 组织（如 `kb/order/order_query/`），便于维度过滤与运营统计。
- 站点实例化统一放 `kb/stations/<站名>.md`，用 `scope_ref` 标注站点。

## 2. frontmatter 字段

| 字段 | 必填 | 取值 | 说明 |
|---|---|---|---|
| `id` | ✅ | 全局唯一 | 建议 `<类>-<子场景>-<短名>` |
| `title` | ✅ | 文本 | 人类可读标题 |
| `category` | ✅ | booking / order / refund / account / travel / app / policy | 路由闭集（7 类） |
| `sub_scenario` | ✅ | 见 `capability.yaml` 的 41 项 | 不进路由，用于过滤与统计 |
| `form` | ✅ | `qa` / `prose` | 决定切块与阈值（见下） |
| `type` | ✅ | policy / howto / faq / notice | 内容性质 |
| `visibility` | ✅ | public / authenticated | authenticated 不进游客检索 |
| `capability` | ✅ | supported / roadmap / industry | **必须与台账一致**（lint 机检） |
| `scope_kind` / `scope_ref` | 条件 | general / station / ticket_type / route + 标识 | `scope_kind != general` 时 `scope_ref` 必填 |
| `source` | 条件 | 权威来源 | `roadmap` / `industry` **必填**（行业内容要可审计） |
| `expire_at` | 可选 | `YYYY-MM-DD` | 过期后退出检索 |

## 3. 两种形态

### A. `form: qa`（FAQ 式，通用高频问答）

- 一个 `## 标准问` = 一个 chunk
- 每个 `##` 下**至少 2 条** `问：` 买家问法（口语 / 错别字 / 中英混写变体）+ 非空 `答：`
- 短块（100~300 字），**不需要重叠**；出处写 `类别/文件名·段落N`

### B. `form: prose`（段落式，站点规则 / 政策 / 流程）

- 层级标题 `#`→`##`→`###`，**深度 ≤3**
- 按子标题 / 条款编号（一、二、1. 2.）/ 句子边界三层切块，单块 **≤600 字（按字符，不是字节）**
- **禁止指代词**：「如上所述 / 见第 N 条 / 该站 / 上述 / 前述 / 同上」——chunk 必须脱离上下文可独立理解
- 必须拼「标题路径前缀」（如 `北京西站乘车与检票规则 > 检票时间`）对抗语义稀释
- 出处写 `stations/<站名>·<章节路径>`

### 不入知识库的内容

表格与结构化参数（退票费档位、票价、班次时刻）→ 走**工具层查表**，不要写进 KB。

## 4. 能力边界（B 方案的核心约束）

- `capability: supported` → 必须能说清**入口**（前端路径 / 接口 / 工具名），且与台账 `system_entry` 一致；
- `capability: roadmap` / `industry` → 正文写行业规则，**但生成层会加边界声明**：
  > 该功能本平台暂未开放，以下是铁路客运行业的一般做法，供参考；具体以车站与官方渠道为准。
- **正文不要写"本平台支持/本平台可办理"**，也不要写操作路径——那些措辞由生成层按台账状态注入，台账翻转（`roadmap → supported`）时**只改措辞层、零重嵌**。

## 5. 校验与入库

```bash
python kb/_lint.py            # 校验（零依赖）：frontmatter / 形态规则 / 台账一致性
go run ./cmd/kb-sync          # 增量入库（断点续跑；仅重嵌变更 chunk）
go run ./cmd/kb-sync --refresh-capability   # 台账状态翻转：只更新 payload，不重嵌
```

`_lint.py` 是**入库前置闸门**：lint 不过的文档不会被入库（指标 `lint_rejected_total{rule}`）。
在 10⁵ 级规模下，**机器校验是唯一可行的质量保证方式**，不要依赖人工 review。
