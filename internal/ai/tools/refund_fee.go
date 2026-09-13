package tools

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"

	db "github.com/Books-QAQ/tickets/internal/db/sqlc"
)

// 退票手续费确定性计算（§8.2，本项目含金量点）。
// **金额绝不让 LLM 算**：这里是纯函数 + 阶梯规则查表，LLM 只把结果组织成人话。
//
// ⚠️ 语义闸门（19.2）：`penalties` 行的 `hours_before` / `actual_hours_before` 含义
// 无法从代码确认（全仓无消费者、无种子数据），因此实现按文档假定语义：
//
//	「距发车 `hours_before` 小时（含）以前退票，扣 `percent`%」⇒ 取"满足条件中最紧的一档"
//
// 并且默认**闸门关闭**（Cfg.PenaltySemanticsConfirmed=false）：此时工具返回
// unavailable(penalty_semantics_unconfirmed)，退票费问题走知识库 FAQ + 转人工。
// 宁可转人工，不可猜金额（§8.2 边界 1、§19.2）。

// TierRule 一条阶梯规则（已归一化）
type TierRule struct {
	HoursBefore float64
	Percent     int
	CustomText  string
}

// FeeResult 确定性计算结果
type FeeResult struct {
	HoursBefore   float64  `json:"hours_before_departure"`
	MatchedRule   *TierRule `json:"matched_rule,omitempty"`
	Percent       int      `json:"percent"`
	FeeCents      int64    `json:"fee_cents"`
	RefundCents   int64    `json:"refundable_cents"`
	CustomText    string   `json:"custom_text,omitempty"`
	NoTierMatched bool     `json:"no_tier_matched"`
}

// ComputeRefundFee 纯函数：给定金额、发车时刻、当前时刻与规则集合，算出费用。
// amount 单位为**元**（与 orders.amount / buses.price 一致），内部用分避免浮点误差。
func ComputeRefundFee(amount int32, departure, now time.Time, rules []TierRule) FeeResult {
	res := FeeResult{HoursBefore: departure.Sub(now).Hours()}

	sorted := make([]TierRule, len(rules))
	copy(sorted, rules)
	// 阈值从大到小：第一个"满足"的即最紧的一档
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].HoursBefore > sorted[j].HoursBefore })

	for _, r := range sorted {
		if res.HoursBefore >= r.HoursBefore {
			rr := r
			res.MatchedRule = &rr
			res.Percent = r.Percent
			res.CustomText = r.CustomText
			break
		}
	}
	if res.MatchedRule == nil {
		// 没有任何档位覆盖当前时间点（例如低于最低档阈值）→ 不下结论
		res.NoTierMatched = true
		return res
	}

	// amount(元) × percent% = amount*percent（分）——整数运算，无舍入误差
	res.FeeCents = int64(amount) * int64(res.Percent)
	res.RefundCents = int64(amount)*100 - res.FeeCents
	return res
}

// Yuan 分 → "12.50" 元字符串
func Yuan(cents int64) string {
	return fmt.Sprintf("%d.%02d", cents/100, cents%100)
}

// ---------- 工具 ----------

type refundFeeTool struct{ base }

func (refundFeeTool) Name() string { return "refund_fee" }
func (refundFeeTool) Kind() string { return "read" }
func (refundFeeTool) Desc() string { return "按阶梯规则计算退票手续费与实退金额（确定性计算，不经模型）" }

func (refundFeeTool) Match(q string) int {
	// 只认"问金额/手续费"的说法。"怎么退票/退票流程"属知识库问答，不抢答。
	if contains(q, "退票手续费", "手续费怎么算", "手续费多少钱", "扣多少", "扣多少钱", "能退多少", "退多少钱", "退了扣", "扣手续费") {
		return 3
	}
	return 0
}

func (refundFeeTool) Slots(q string) SlotSet {
	s := SlotSet{}
	if no := ExtractOrderNo(q); no != "" {
		s.OrderNo = no
	}
	if id := ExtractTicketID(q); id > 0 {
		s.TicketID = id
	}
	return s
}

func (t refundFeeTool) Exec(ctx context.Context, id Identity, s SlotSet) Result {
	if !id.Personal() {
		return Result{Kind: KindGuestRequired, Reason: "guest_personal_tool",
			Summary: "请先登录后我为您核算退票手续费。", PathHint: "transfer_deterministic"}
	}

	// 闸门（19.2）：语义未确认 → 不下结论
	if !t.deps.Cfg.PenaltySemanticsConfirmed {
		return Result{
			Kind:     KindUnavailable,
			Reason:   "penalty_semantics_unconfirmed",
			PathHint: "transfer_tool_unavailable",
			Summary:  "退票手续费的规则口径尚未确认，我不能给出金额（算错金额比让您多等一会儿更糟）。已为您转人工核对。",
		}
	}

	// ---- 定位要算的那张票 ----
	ticket, res, ok := t.locateTicket(ctx, id, s)
	if !ok {
		return res
	}

	bus, err := t.deps.Store.GetBusByID(ctx, ticket.BusID)
	if err != nil {
		return Result{Kind: KindUnavailable, Reason: "db_error", PathHint: "transfer_tool_unavailable"}
	}
	now := t.deps.now()

	// 边界 4：已过发车时间 → 不计算手续费，按"误车"处理
	if !bus.DepartureTime.After(now) {
		return Result{
			Kind:     KindBlocked,
			Reason:   "departed",
			PathHint: "transfer_deterministic",
			Summary: fmt.Sprintf("这趟车（%s 发车）已经开走了，属于误车情形，退票规则与开车前不同，我不能在这里给您算金额。已为您转人工。",
				bus.DepartureTime.Format("2006-01-02 15:04")),
		}
	}

	rows, err := t.deps.Store.GetBusPenalties(ctx, ticket.BusID)
	if err != nil {
		return Result{Kind: KindUnavailable, Reason: "db_error", PathHint: "transfer_tool_unavailable"}
	}
	if len(rows) == 0 {
		// 边界 2：无规则行 → 不下结论
		return Result{
			Kind:     KindBlocked,
			Reason:   "no_penalty_rule",
			PathHint: "transfer_tool_unavailable",
			Summary:  "这趟车没有查到退票手续费规则，我不能凭猜测给金额。已为您转人工核实。",
		}
	}

	rules := make([]TierRule, 0, len(rows))
	for _, p := range rows {
		r := TierRule{Percent: int(p.Percent)}
		if p.HoursBefore.Valid {
			r.HoursBefore = p.HoursBefore.Float64
		}
		if p.CustomText.Valid {
			r.CustomText = p.CustomText.String
		}
		rules = append(rules, r)
	}

	fee := ComputeRefundFee(ticket.Amount, bus.DepartureTime, now, rules)
	if fee.NoTierMatched {
		return Result{
			Kind:     KindBlocked,
			Reason:   "no_tier_matched",
			PathHint: "transfer_tool_unavailable",
			Summary: fmt.Sprintf("距发车 %.1f 小时，没有匹配到适用的手续费档位，我不下结论。已为您转人工核实。", fee.HoursBefore),
		}
	}

	summary := fmt.Sprintf("距发车 %.1f 小时，适用规则：%.0f 小时(含)以前扣 %d%%。票面 %s 元，手续费 %s 元，实退 %s 元。",
		fee.HoursBefore, fee.MatchedRule.HoursBefore, fee.Percent,
		Yuan(int64(ticket.Amount)*100), Yuan(fee.FeeCents), Yuan(fee.RefundCents))
	if fee.CustomText != "" {
		// 边界 5：custom_text 原文透传，且与计算值冲突时以它为准
		summary += "该班次另有特殊说明（以车站说明为准）：" + fee.CustomText
	}

	facts := map[string]any{
		"hours_before_departure": round1(fee.HoursBefore),
		"matched_rule_hours":     fee.MatchedRule.HoursBefore,
		"percent":                fee.Percent,
		"amount_yuan":            ticket.Amount,
		"fee_yuan":               Yuan(fee.FeeCents),
		"refundable_yuan":        Yuan(fee.RefundCents),
		"departure_time":         bus.DepartureTime.Format("2006-01-02 15:04"),
	}
	if fee.CustomText != "" {
		facts["custom_text"] = fee.CustomText
	}
	return Result{Kind: KindOK, Summary: summary, Facts: facts}
}

// locateTicket 定位要计算的车票：显式车票号 → 订单号 → 最近可退车票（唯一才用，多张则反问）
func (t refundFeeTool) locateTicket(ctx context.Context, id Identity, s SlotSet) (refundTarget, Result, bool) {
	if s.TicketID > 0 {
		rows, err := t.deps.Store.ListUserTicketsDetailed(ctx, id.UserID)
		if err != nil {
			return refundTarget{}, Result{Kind: KindUnavailable, Reason: "db_error", PathHint: "transfer_tool_unavailable"}, false
		}
		for _, r := range rows {
			if r.UserTicketID == s.TicketID {
				return refundTarget{BusID: r.BusID, Amount: r.Price, Status: r.Status, TicketID: r.UserTicketID}, Result{}, true
			}
		}
		return refundTarget{}, Result{Kind: KindNotFound, Reason: "ticket_not_found_or_not_owned",
			Summary: "未查询到该车票。请确认车票号，或该车票不属于当前账号。"}, false
	}

	if s.OrderNo != "" {
		o, err := t.deps.Store.GetOrderByNoForUser(ctx, s.OrderNo, id.UserID)
		if err == sql.ErrNoRows {
			return refundTarget{}, Result{Kind: KindNotFound, Reason: "order_not_found_or_not_owned",
				Summary: "未查询到该订单。请确认订单号，或该订单不属于当前账号。"}, false
		}
		if err != nil {
			return refundTarget{}, Result{Kind: KindUnavailable, Reason: "db_error", PathHint: "transfer_tool_unavailable"}, false
		}
		// 边界 3：状态不是已支付 → 不算手续费，只说明当前状态
		if o.Status != "paid" {
			return refundTarget{}, Result{
				Kind:     KindBlocked,
				Reason:   "order_status_" + o.Status,
				PathHint: "transfer_deterministic",
				Summary: fmt.Sprintf("该订单当前状态是「%s」，不涉及退票手续费计算。", orderStatusText(o.Status)),
			}, false
		}
		return refundTarget{BusID: o.BusID, Amount: o.Amount, Status: o.Status, OrderNo: o.OrderNo}, Result{}, true
	}

	// 无号：取最近可退车票；唯一才用（多张则反问，不猜）
	rows, err := t.deps.Store.ListUserTicketsDetailed(ctx, id.UserID)
	if err != nil {
		return refundTarget{}, Result{Kind: KindUnavailable, Reason: "db_error", PathHint: "transfer_tool_unavailable"}, false
	}
	now := t.deps.now()
	var cands []db.UserTicketDetail
	for _, r := range rows {
		if r.Status == "purchased" && r.DepartureTime.After(now) {
			cands = append(cands, r)
		}
	}
	switch len(cands) {
	case 0:
		return refundTarget{}, Result{Kind: KindEmpty, Summary: "您当前没有可退的车票。"}, false
	case 1:
		c := cands[0]
		return refundTarget{BusID: c.BusID, Amount: c.Price, Status: c.Status, TicketID: c.UserTicketID}, Result{}, true
	default:
		opts := make([]string, 0, len(cands))
		vals := make([]string, 0, len(cands))
		for _, c := range cands {
			opts = append(opts, fmt.Sprintf("车票%d（%s %s→%s）", c.UserTicketID, c.DepartureTime.Format("01-02 15:04"), c.OriginTerminal, c.DestinationTerm))
			vals = append(vals, fmt.Sprintf("车票%d", c.UserTicketID))
		}
		return refundTarget{}, Result{
			Kind: KindSlotsIncomplete, Reason: "multiple_refundable_tickets",
			Missing: []string{"车票号"}, Candidates: opts, OptionValues: vals,
			Summary: "您有多张可退车票，请告诉我要算哪一张（可直接回复序号）。",
		}, false
	}
}

type refundTarget struct {
	BusID    int32
	Amount   int32
	Status   string
	TicketID int32
	OrderNo  string
}

func round1(f float64) float64 {
	return float64(int64(f*10+0.5)) / 10
}

// ---------- create_support_ticket（写：可追踪的工单，非资金操作） ----------

type supportTicketTool struct{ base }

func (supportTicketTool) Name() string { return "create_support_ticket" }
func (supportTicketTool) Kind() string { return "write" }
func (supportTicketTool) Desc() string { return "创建人工客服工单并返回可追踪工单号" }

// Match 恒为 0：建单不是"用户问了什么"触发的，而是**转人工路径**调用的（§10.1 四条路径统一走工单）。
func (supportTicketTool) Match(string) int { return 0 }

func (supportTicketTool) Slots(string) SlotSet { return SlotSet{} }

func (t supportTicketTool) Exec(ctx context.Context, id Identity, s SlotSet) Result {
	if !id.Personal() && !t.deps.Cfg.GuestTicketAllowed {
		// §10.3 游客话术；是否允许游客建单见 19.3（默认不允许，引导登录）
		return Result{Kind: KindGuestRequired, Reason: "guest_ticket_not_allowed",
			Summary: "为保护您的订单信息，请先登录后我为您转接人工客服。"}
	}

	category := s.Category
	if category == "" {
		category = "other"
	}
	path := s.Path
	if path == "" {
		path = "deterministic"
	}
	summary := buildTicketSummary(s)

	arg := db.CreateSupportTicketParams{
		Category: category,
		Path:     path,
		Summary:  summary,
	}
	if s.ConvID != "" {
		arg.ConvID = sql.NullString{String: s.ConvID, Valid: true}
	}
	if id.Personal() {
		arg.UserID = sql.NullInt32{Int32: id.UserID, Valid: true}
	}
	if s.OrderNo != "" {
		arg.OrderNo = sql.NullString{String: s.OrderNo, Valid: true}
	}

	rec, err := t.deps.Store.CreateSupportTicket(ctx, arg)
	if err != nil {
		return Result{Kind: KindUnavailable, Reason: "db_error", PathHint: "transfer_tool_unavailable"}
	}
	return Result{
		Kind: KindOK,
		// 工单号**完整**返回（用户要靠它追踪；遮蔽会让人无法核对）
		Summary: fmt.Sprintf("已为您转接人工客服，工单号 %s，坐席会尽快与您联系。", rec.TicketNo),
		Facts:   map[string]any{"ticket_no": rec.TicketNo, "path": path, "category": category},
	}
}

// buildTicketSummary 确定性模板拼装（§10.2：不依赖 LLM 总结）
func buildTicketSummary(s SlotSet) string {
	out := ""
	if s.Category != "" {
		out = "分类：" + s.Category + "；"
	}
	if s.Question != "" {
		out += "用户问题：" + s.Question + "；"
	}
	if s.OrderNo != "" {
		out += "关联订单：" + MaskOrderNo(s.OrderNo) + "；"
	}
	if s.Summary != "" {
		out += "补充：" + s.Summary
	}
	if out == "" {
		out = "用户请求人工协助（无更多上下文）"
	}
	return out
}
