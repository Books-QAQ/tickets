package tools

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	db "github.com/Books-QAQ/tickets/internal/db/sqlc"
)

// 本文件实现 6 个**读**工具（§8.1 注册表 1~6 项）。
// 纪律：只读（ADR-8 资金类写操作不由 AI 执行）；每个工具都从 Identity 取 user_id，
// SQL 一律带 user_id 条件；输出先脱敏再进 Summary。

type base struct{ deps Deps }

func contains(q string, subs ...string) bool {
	for _, s := range subs {
		if strings.Contains(q, s) {
			return true
		}
	}
	return false
}

// ---------- 1. my_tickets：我的车票 ----------

type myTicketsTool struct{ base }

func (myTicketsTool) Name() string { return "my_tickets" }
func (myTicketsTool) Kind() string { return "read" }
func (myTicketsTool) Desc() string { return "查当前登录用户名下的车票（含待支付占座与已出票）" }

func (myTicketsTool) Match(q string) int {
	// 只认"拥有关系"的说法：泛问"车票怎么退"属于知识库问答，不该被工具抢答
	if contains(q, "我的票", "我的车票", "我买的票", "我订的票", "我订了哪", "查我的票", "买了哪趟", "我的行程") {
		return 3
	}
	return 0
}

func (myTicketsTool) Slots(_ string) SlotSet { return SlotSet{} }

func (t myTicketsTool) Exec(ctx context.Context, id Identity, _ SlotSet) Result {
	if !id.Personal() {
		return Result{Kind: KindGuestRequired, Reason: "guest_personal_tool", Summary: "请先登录后我为您查询车票。"}
	}
	rows, err := t.deps.Store.ListUserTicketsDetailed(ctx, id.UserID)
	if err != nil {
		return Result{Kind: KindUnavailable, Reason: "db_error"}
	}
	if len(rows) == 0 {
		return Result{Kind: KindEmpty, Summary: "您当前没有车票记录。", Facts: map[string]any{"count": 0}}
	}

	limit := t.deps.Cfg.MaxItems
	if limit <= 0 || limit > len(rows) {
		limit = len(rows)
	}
	var sb strings.Builder
	facts := []map[string]any{}
	for _, r := range rows[:limit] {
		sb.WriteString(fmt.Sprintf("%s 发车 %s→%s %d号座 票价%d元 状态%s\n",
			r.DepartureTime.Format("2006-01-02 15:04"),
			r.OriginTerminal, r.DestinationTerm, r.SeatNumber, r.Price, ticketStatusText(r.Status)))
		facts = append(facts, map[string]any{
			"ticket_id":      r.UserTicketID,
			"bus_id":         r.BusID,
			"departure_time": r.DepartureTime.Format("2006-01-02 15:04"),
			"from":           r.OriginTerminal,
			"to":             r.DestinationTerm,
			"seat_number":    r.SeatNumber,
			"price":          r.Price,
			"status":         r.Status,
		})
	}
	return Result{
		Kind:    KindOK,
		Summary: sb.String(),
		Facts:   map[string]any{"count": len(rows), "tickets": facts},
	}
}

func ticketStatusText(s string) string {
	switch s {
	case "reserved":
		return "已占座待支付"
	case "purchased":
		return "已出票"
	case "canceled":
		return "已取消"
	default:
		return s
	}
}

func orderStatusText(s string) string {
	switch s {
	case "pending":
		return "待支付"
	case "paid":
		return "已支付"
	case "canceled":
		return "已取消"
	case "refunded":
		return "已退款"
	default:
		return s
	}
}

// ---------- 2. order_detail：订单详情 ----------

type orderDetailTool struct{ base }

func (orderDetailTool) Name() string { return "order_detail" }
func (orderDetailTool) Kind() string { return "read" }
func (orderDetailTool) Desc() string { return "查订单详情（订单号、金额、状态、支付渠道与时间）" }

func (orderDetailTool) Match(q string) int {
	switch {
	case contains(q, "订单号", "我的订单", "查订单", "订单详情", "这单", "这订单"):
		return 3
	case contains(q, "订单"):
		return 2
	}
	return 0
}

func (orderDetailTool) Slots(q string) SlotSet {
	s := SlotSet{}
	if no := ExtractOrderNo(q); no != "" {
		s.OrderNo = no
		return s
	}
	// 无订单号 → 交给 Exec 做"最近订单消歧"
	s.Missing = []string{"订单号"}
	return s
}

func (t orderDetailTool) Exec(ctx context.Context, id Identity, s SlotSet) Result {
	if !id.Personal() {
		return Result{Kind: KindGuestRequired, Reason: "guest_personal_tool", Summary: "为保护您的订单信息，请先登录后我为您查询订单。"}
	}

	if s.OrderNo != "" {
		o, err := t.deps.Store.GetOrderByNoForUser(ctx, s.OrderNo, id.UserID)
		if err == sql.ErrNoRows {
			// 越权与不存在**同一话术**，不泄露订单是否存在（§8.5.2）
			return Result{Kind: KindNotFound, Reason: "order_not_found_or_not_owned",
				Summary: "未查询到该订单。请确认订单号，或该订单不属于当前账号。"}
		}
		if err != nil {
			return Result{Kind: KindUnavailable, Reason: "db_error"}
		}
		return t.resultFor(ctx, id, o)
	}

	// 无订单号：取最近订单；只有 1 单就直接答（不必让用户再报号），多单则消歧反问
	orders, err := t.deps.Store.ListOrdersByUser(ctx, id.UserID, "", 3)
	if err != nil {
		return Result{Kind: KindUnavailable, Reason: "db_error"}
	}
	switch len(orders) {
	case 0:
		return Result{Kind: KindEmpty, Summary: "您当前没有订单记录。"}
	case 1:
		return t.resultFor(ctx, id, orders[0])
	default:
		opts := make([]string, 0, len(orders))
		vals := make([]string, 0, len(orders))
		for _, o := range orders {
			opts = append(opts, fmt.Sprintf("%s（%d元 %s）", MaskOrderNo(o.OrderNo), o.Amount, orderStatusText(o.Status)))
			vals = append(vals, o.OrderNo) // 机器可用值：resume 后回填槽位
		}
		return Result{
			Kind:         KindSlotsIncomplete,
			Reason:       "multiple_recent_orders",
			Missing:      []string{"订单号"},
			Candidates:   opts,
			OptionValues: vals,
			Summary:      "您最近有多笔订单，请告诉我要查哪一笔（可直接回复序号）。",
		}
	}
}

func (t orderDetailTool) resultFor(ctx context.Context, id Identity, o db.Order) Result {
	summary := fmt.Sprintf("订单 %s：金额%d元，状态%s", MaskOrderNo(o.OrderNo), o.Amount, orderStatusText(o.Status))
	if o.PayChannel.Valid {
		summary += "，支付渠道" + o.PayChannel.String
	}
	if o.PaidAt.Valid {
		summary += "，支付时间" + o.PaidAt.Time.Format("2006-01-02 15:04")
	}
	if o.Status == "pending" {
		// 剩余时间**从 SQL 取**（不在 Go 里比时钟）：DSN 时区与容器会话时区可能不同，
		// M3 实测过"15 分钟后到期的订单被判已超时"（差 8 小时）。
		// 取不到就**不报数字**（宁可不显示，也不给一个错的时间）
		left, err := t.deps.Store.OrderLeftMinutesForUser(ctx, o.OrderNo, id.UserID)
		switch {
		case err != nil:
			summary += "，剩余支付时间以订单页为准"
		case left > 0:
			summary += fmt.Sprintf("，剩余支付时间约%d分钟", left)
		default:
			summary += "，该订单已超时（将自动关闭）"
		}
	}
	facts := map[string]any{
		"order_no": MaskOrderNo(o.OrderNo),
		"amount":   o.Amount,
		"status":   o.Status,
	}
	if o.PayChannel.Valid {
		facts["pay_channel"] = o.PayChannel.String
	}
	if o.Status == "pending" {
		facts["expire_at"] = o.ExpiredAt.Format("2006-01-02 15:04")
	}
	return Result{Kind: KindOK, Summary: summary, Facts: facts}
}

// ---------- 3. unpaid_orders：待支付订单 ----------

type unpaidOrdersTool struct{ base }

func (unpaidOrdersTool) Name() string { return "unpaid_orders" }
func (unpaidOrdersTool) Kind() string { return "read" }
func (unpaidOrdersTool) Desc() string { return "查待支付订单及剩余支付时间" }

func (unpaidOrdersTool) Match(q string) int {
	if contains(q, "没付款", "未付款", "没支付", "待支付", "未支付", "还要不要付", "支付倒计时", "还没付", "订单过期") {
		return 3
	}
	return 0
}

func (unpaidOrdersTool) Slots(string) SlotSet { return SlotSet{} }

func (t unpaidOrdersTool) Exec(ctx context.Context, id Identity, _ SlotSet) Result {
	if !id.Personal() {
		return Result{Kind: KindGuestRequired, Reason: "guest_personal_tool", Summary: "请先登录后我为您查询待支付订单。"}
	}
	orders, err := t.deps.Store.ListPendingOrdersForUser(ctx, id.UserID, t.deps.Cfg.MaxItems)
	if err != nil {
		return Result{Kind: KindUnavailable, Reason: "db_error"}
	}
	if len(orders) == 0 {
		return Result{Kind: KindEmpty, Summary: "您当前没有待支付订单。", Facts: map[string]any{"count": 0}}
	}
	var sb strings.Builder
	facts := []map[string]any{}
	for _, o := range orders {
		sb.WriteString(fmt.Sprintf("订单 %s：%d元，剩余支付时间约%d分钟\n",
			MaskOrderNo(o.OrderNo), o.Amount, o.LeftMinutes))
		facts = append(facts, map[string]any{
			"order_no":    MaskOrderNo(o.OrderNo),
			"amount":      o.Amount,
			"expire_at":   o.ExpiredAt.Format("2006-01-02 15:04"),
			"left_minute": o.LeftMinutes,
		})
	}
	return Result{Kind: KindOK, Summary: sb.String(), Facts: map[string]any{"count": len(orders), "orders": facts}}
}

// ---------- 4. refund_progress：退款进度 ----------

type refundProgressTool struct{ base }

func (refundProgressTool) Name() string { return "refund_progress" }
func (refundProgressTool) Kind() string { return "read" }
func (refundProgressTool) Desc() string { return "查订单退款状态（本地状态；渠道侧到账时间无接口）" }

func (refundProgressTool) Match(q string) int {
	if contains(q, "退款进度", "退款到哪", "退款状态", "钱退到哪", "退到哪了", "钱什么时候退", "退款了吗", "退票款") {
		return 3
	}
	return 0
}

func (refundProgressTool) Slots(q string) SlotSet {
	s := SlotSet{}
	if no := ExtractOrderNo(q); no != "" {
		s.OrderNo = no
	} else {
		s.Missing = []string{"订单号"}
	}
	return s
}

func (t refundProgressTool) Exec(ctx context.Context, id Identity, s SlotSet) Result {
	if !id.Personal() {
		return Result{Kind: KindGuestRequired, Reason: "guest_personal_tool", Summary: "请先登录后我为您查询退款进度。"}
	}
	if s.OrderNo != "" {
		o, err := t.deps.Store.GetOrderByNoForUser(ctx, s.OrderNo, id.UserID)
		if err == sql.ErrNoRows {
			return Result{Kind: KindNotFound, Reason: "order_not_found_or_not_owned",
				Summary: "未查询到该订单。请确认订单号，或该订单不属于当前账号。"}
		}
		if err != nil {
			return Result{Kind: KindUnavailable, Reason: "db_error"}
		}
		return t.resultFor([]db.Order{o})
	}

	// 无订单号：列已退款的订单
	orders, err := t.deps.Store.ListOrdersByUser(ctx, id.UserID, "refunded", t.deps.Cfg.MaxItems)
	if err != nil {
		return Result{Kind: KindUnavailable, Reason: "db_error"}
	}
	if len(orders) == 0 {
		return Result{Kind: KindEmpty, Summary: "您当前没有已退款的订单。若刚提交退票，请稍后再查。"}
	}
	return t.resultFor(orders)
}

func (t refundProgressTool) resultFor(orders []db.Order) Result {
	var sb strings.Builder
	facts := []map[string]any{}
	for _, o := range orders {
		line := fmt.Sprintf("订单 %s：%d元，状态%s", MaskOrderNo(o.OrderNo), o.Amount, orderStatusText(o.Status))
		if o.PayChannel.Valid {
			line += "，原支付渠道" + o.PayChannel.String
		}
		sb.WriteString(line + "\n")
		facts = append(facts, map[string]any{
			"order_no": MaskOrderNo(o.OrderNo), "amount": o.Amount,
			"status": o.Status, "pay_channel": o.PayChannel.String,
		})
	}
	// 诚实声明（§8.4）：渠道侧到账时间无查询接口，只能给本地状态
	sb.WriteString("说明：这里显示的是平台侧退款状态；具体到账时间以原支付渠道为准（平台无渠道侧到账查询接口）。")
	return Result{Kind: KindOK, Summary: sb.String(), Facts: map[string]any{"orders": facts}}
}

// ---------- 5. bus_availability：余票查询 ----------

type busAvailabilityTool struct{ base }

func (busAvailabilityTool) Name() string { return "bus_availability" }
func (busAvailabilityTool) Kind() string { return "read" }
func (busAvailabilityTool) Desc() string { return "查某日从出发站到到达站的班次、时刻、票价与余票" }

func (busAvailabilityTool) Match(q string) int {
	if contains(q, "余票", "还有票吗", "有票吗", "有没有票", "还有座位", "剩余座位", "几点的车", "有哪些班次", "查班次", "有车吗") {
		return 3
	}
	return 0
}

func (t busAvailabilityTool) Slots(q string) SlotSet {
	s := SlotSet{}
	idx := t.deps.Stations()
	if idx != nil {
		hits := idx.MatchStation(q)
		for _, h := range hits {
			if len(h.TerminalIDs) == 0 {
				continue
			}
			if len(h.TerminalIDs) > 1 {
				// 地名歧义：如"北京"对应多个车站 → 反问，不猜（§8.1）
				s.Ambiguous = append(s.Ambiguous, fmt.Sprintf("%s（可能是：%s）", h.Text, strings.Join(idx.Names(h.TerminalIDs), "、")))
				continue
			}
			if s.FromTerminal == 0 {
				s.FromTerminal = h.TerminalIDs[0]
				s.FromRaw = h.Text
			} else if s.ToTerminal == 0 {
				s.ToTerminal = h.TerminalIDs[0]
				s.ToRaw = h.Text
			}
		}
	}
	if d, raw, ok := ExtractDate(q, t.deps.now()); ok {
		s.Date, s.HasDate, s.DateRaw = d, true, raw
	}
	if s.FromTerminal == 0 {
		s.Missing = append(s.Missing, "出发站")
	}
	if s.ToTerminal == 0 {
		s.Missing = append(s.Missing, "到达站")
	}
	if !s.HasDate {
		s.Missing = append(s.Missing, "出发日期")
	}
	if len(s.Ambiguous) > 0 {
		s.Missing = append(s.Missing, "车站消歧")
	}
	return s
}

func (t busAvailabilityTool) Exec(ctx context.Context, _ Identity, s SlotSet) Result {
	// 余票与身份无关（不需要登录），只需要参数齐全
	if len(s.Ambiguous) > 0 {
		return Result{
			Kind: KindSlotsIncomplete, Reason: "station_ambiguous",
			Candidates: s.Ambiguous,
			Summary:    "您说的车站对应多个车站，请指定具体车站：" + strings.Join(s.Ambiguous, "；"),
		}
	}
	if len(s.Missing) > 0 {
		return Result{
			Kind: KindSlotsIncomplete, Reason: "slots_missing", Missing: s.Missing,
			Summary: "请补充：" + strings.Join(s.Missing, "、") + "。",
		}
	}

	rows, err := t.deps.Store.ListRoutes(ctx, db.ListRoutesParams{
		OriginTerminalID:      s.FromTerminal,
		DestinationTerminalID: s.ToTerminal,
		DepartureTime:         s.Date,
	})
	if err != nil {
		return Result{Kind: KindUnavailable, Reason: "db_error"}
	}
	if len(rows) == 0 {
		return Result{Kind: KindEmpty, Summary: fmt.Sprintf("%s 从 %s 到 %s 没有查到班次。", s.Date.Format("2006-01-02"), s.FromRaw, s.ToRaw)}
	}

	// 售票窗口并发查（照抄 route.go 的做法，消除 N+1）。
	// ⚠️ 必须先排序再取窗口：ListRoutes 无 ORDER BY，若不先排序，
	// 后面按展示顺序读 windows[i] 会与班次错位（M2 实测踩过）。
	sort.Slice(rows, func(i, j int) bool { return rows[i].DepartureTime.Before(rows[j].DepartureTime) })

	type win struct {
		openAt time.Time
		err    error
	}
	windows := make([]win, len(rows))
	var wg sync.WaitGroup
	for i := range rows {
		wg.Add(1)
		go func(idx int, busID int32) {
			defer wg.Done()
			windows[idx].openAt, windows[idx].err = t.deps.Store.GetBusSaleOpenAt(ctx, busID)
		}(i, rows[i].BusID)
	}
	wg.Wait()

	now := t.deps.now()
	limit := t.deps.Cfg.MaxItems
	if limit <= 0 || limit > len(rows) {
		limit = len(rows)
	}

	var sb strings.Builder
	facts := []map[string]any{}
	for i, r := range rows[:limit] {
		onSale := true
		var openAt time.Time
		if i < len(windows) && windows[i].err == nil {
			openAt = windows[i].openAt
			onSale = !now.Before(openAt)
		}
		saleText := "已开售"
		if !onSale {
			saleText = "未开售（" + openAt.Format("01-02 15:04") + " 起售）"
		}
		sb.WriteString(fmt.Sprintf("%s 发车 %s→%s 票价%d元 余票%d张 %s\n",
			r.DepartureTime.Format("15:04"), r.OriginTerminalName, r.DestinationTerminalName,
			r.Price, r.AvailableSeats, saleText))
		facts = append(facts, map[string]any{
			"bus_id":          r.BusID,
			"departure_time":  r.DepartureTime.Format("2006-01-02 15:04"),
			"from":            r.OriginTerminalName,
			"to":              r.DestinationTerminalName,
			"price":           r.Price,
			"available_seats": r.AvailableSeats,
			"on_sale":         onSale,
		})
	}
	return Result{
		Kind:    KindOK,
		Summary: sb.String(),
		Facts:   map[string]any{"count": len(rows), "buses": facts, "date": s.Date.Format("2006-01-02")},
	}
}
