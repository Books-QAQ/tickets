// Package tools 实现智能AI客服的**工具层**（Go 侧执行，§8）。
//
// 分工（§5.10 / §8.1）：
//   - Python（编排层）决定"要不要调工具、调哪个"（提槽未命中时可 LLM 兜底选工具名，但必须过白名单校验）；
//   - Go（本包）负责"怎么查、查什么参数、结果怎么脱敏"——确定性槽位抽取、越权拦截、出口脱敏、超时熔断。
//
// 安全硬线（§8.5）：
//  1. 工具入参里**没有** user_id —— 由 Go 从 JWT 解析后注入（Identity），编排层只是中继；
//  2. 每条 SQL 强制 WHERE user_id = ?；跨用户查询统一返回"未查询到"（不返回 403，不泄露存在性）；
//  3. 游客态调用个人工具 → 引导登录，不返回空结果混淆视听。
package tools

import (
	"context"
	"time"
)

// Counter 计数点接口（Go 侧单一计数来源；kb.Aux 结构上满足它）
type Counter interface {
	Inc(key string)
}

// Identity 调用身份（Go 侧解析后注入；工具与编排层都不产生它）
type Identity struct {
	UserID   int32
	Guest    bool
	GuestKey string // 游客锚点（device_id 的 hash），个人工具一律拒绝
}

// Personal 是否为可查个人数据的登录用户
func (i Identity) Personal() bool { return !i.Guest && i.UserID > 0 }

// 工具结果类型。**不用 error 表达业务分支**：分支要能进指标、能进 prompt 素材。
const (
	KindOK              = "ok"               // 查到数据
	KindEmpty           = "empty"            // 查询成功但没有数据（如没有待支付订单）
	KindSlotsIncomplete = "slots_incomplete" // 槽位不全（缺参数或地名歧义）→ 反问，不算错误
	KindGuestRequired   = "guest_required"   // 游客调用个人工具 → 引导登录
	KindNotFound        = "not_found"        // 越权或不存在，统一话术
	KindUnavailable     = "unavailable"      // 数据源异常/超时/熔断/闸门关闭 → 转人工（独立计数）
	KindBlocked         = "blocked"          // **业务规则不允许**（已过发车时间/状态不对/规则缺失）→ 转人工（确定性路径），不算故障
	KindForbidden       = "forbidden"        // 明确不允许的操作（如 AI 执行资金类写操作，ADR-8）
)

// SlotSet 一次调用用到的槽位（确定性抽取结果；LLM 只决定调哪个工具，不填槽）。
// 带 JSON tag 是为了**跨语言往返**：Go 在 /internal/tools/route 里抽出槽位 → Python 原样回传
// 到 /internal/tools/:name，避免两层各抽一遍导致口径不一致。
type SlotSet struct {
	OrderNo  string    `json:"order_no,omitempty"`
	TicketID int32     `json:"ticket_id,omitempty"`
	FromRaw  string    `json:"from_raw,omitempty"`
	ToRaw    string    `json:"to_raw,omitempty"`
	DateRaw  string    `json:"date_raw,omitempty"`
	Date     time.Time `json:"date,omitempty"`
	HasDate  bool      `json:"has_date,omitempty"`

	FromTerminal int32 `json:"from_terminal,omitempty"`
	ToTerminal   int32 `json:"to_terminal,omitempty"`

	Missing   []string `json:"missing,omitempty"`
	Ambiguous []string `json:"ambiguous,omitempty"`

	// —— 上下文槽（由编排层/Go 填，不从用户问题里抽）——
	Question string `json:"question,omitempty"`
	ConvID   string `json:"conv_id,omitempty"`
	Category string `json:"category,omitempty"`
	Path     string `json:"path,omitempty"`
	Summary  string `json:"summary,omitempty"`
}

// Result 工具执行结果
type Result struct {
	Tool      string         `json:"tool"`
	Kind      string         `json:"kind"`
	Summary   string         `json:"summary"`             // 给生成层的事实素材（**已脱敏**）
	Facts     map[string]any `json:"facts,omitempty"`      // 结构化事实（供模板直答/统计）
	Missing   []string       `json:"missing,omitempty"`    // 槽位不全时的缺口
	Candidates []string      `json:"candidates,omitempty"` // 歧义候选
	Reason    string         `json:"reason,omitempty"`     // 非 ok 时的机制级原因（可观测）
	// PathHint 转人工路径提示（§10.1 四路径；编排层据此落 path 字段，不用自己猜）：
	// transfer_tool_unavailable（工具坏/闸门关）| transfer_deterministic（业务规则，如误车）
	// | transfer_capability_absent（功能不存在，B 方案独立路径）
	PathHint  string         `json:"path_hint,omitempty"`
	Degraded  bool           `json:"degraded,omitempty"`   // 熔断/降级（诚实标记）
	ElapsedMS int64          `json:"elapsed_ms"`
}

// Tool 工具契约。Match/Slots 是**纯函数**（不碰 IO），Exec 才做 IO。
type Tool interface {
	Name() string
	Kind() string // read | write
	Desc() string
	// Match 返回该工具对问题的词表命中强度（0 = 不作候选）。注册顺序即优先级。
	Match(question string) int
	// Slots 确定性抽取槽位（正则优先，§8.1）
	Slots(question string) SlotSet
	// Exec 执行（超时与熔断由 Registry 统一包裹）
	Exec(ctx context.Context, id Identity, s SlotSet) Result
}

// Candidate 一次路由的候选工具
type Candidate struct {
	Name    string   `json:"name"`
	Score   int      `json:"score"`
	Kind    string   `json:"kind"`
	Desc    string   `json:"desc"`
	Missing []string `json:"missing,omitempty"`
	Slots   SlotSet  `json:"slots"` // 抽出后原样回传给执行端（避免两层各抽一遍）
	// Preferred 同分破平标记：分类与工具亲和时由 Go 标出，编排层据此**不再反问**（§8.1）
	Preferred bool `json:"preferred,omitempty"`
}

// Config 工具层配置
type Config struct {
	Timeout time.Duration // 单工具超时（§8.5.4：1.5s）
	// PenaltySemanticsConfirmed = kb/capability 中 19.2 的闸门。
	// false（默认）时 refund_fee 工具不下结论：返回 unavailable(penalty_semantics_unconfirmed)，
	// 退票费问题走知识库 FAQ + 转人工。**宁可转人工，不可猜金额**（§8.2）。
	PenaltySemanticsConfirmed bool
	// GuestTicketAllowed 19.3：游客能否建单（默认 false = 引导登录）
	GuestTicketAllowed bool
	MaxItems           int // 列表类输出条数上限
}

// DefaultConfig 与文档一致的安全默认值（退票费闸门默认关闭）
func DefaultConfig() Config {
	return Config{
		Timeout:  1500 * time.Millisecond,
		MaxItems: 5,
		// 19.3（游客能否建单）尚未拍板 → 默认**沿用 M1 已验收的行为**（允许游客建单，
		// 工单 user_id 为空但可追踪）。拍板后设 GUEST_TICKET_ALLOWED=0 即改为"引导登录"。
		GuestTicketAllowed: true,
	}
}
