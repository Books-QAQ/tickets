package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/Books-QAQ/tickets/internal/ai/tools"
	"github.com/Books-QAQ/tickets/internal/metrics"
	"github.com/Books-QAQ/tickets/internal/util"
)

// AdminHandler 管理端（控制面）：全量查询 + 工单状态机写接口。
//
// 两条纪律：
//  1. **鉴权由路由层的 adminAuth 负责**（Bearer ADMIN_TOKEN，恒定时间比较），
//     本文件不判断 token —— 免得两处各写一套鉴权。
//  2. **状态流转一律条件更新**（`WHERE status = <from>`），并用影响行数判定胜负：
//     并发下"两个坐席同时认领"只有一个会成功（与座位/订单占用的裁决方式同源）。
type AdminHandler struct {
	DB      *sql.DB
	Config  util.Config
	Metrics *metrics.Collector
}

func NewAdminHandler(db *sql.DB, cfg util.Config, mc *metrics.Collector) *AdminHandler {
	return &AdminHandler{DB: db, Config: cfg, Metrics: mc}
}

// allowedTicketTransitions 工单状态机（§10.2）：pending → assigned → resolved → closed
var allowedTicketTransitions = map[string][]string{
	"pending":  {"assigned", "closed"},
	"assigned": {"resolved", "closed"},
	"resolved": {"closed"},
	"closed":   {},
}

// GET /admin/conversations?user_id=&limit=&offset= —— 全量会话（管理端可见任意用户）
func (h *AdminHandler) ListConversations(c *fiber.Ctx) error {
	limit, offset := paging(c, 50, 500)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	var (
		rows *sql.Rows
		err  error
	)
	q := `SELECT id, user_id, guest_key, title, created_at, updated_at FROM cs_conversations`
	args := []any{}
	if uid := strings.TrimSpace(c.Query("user_id")); uid != "" {
		q += ` WHERE user_id = ?`
		args = append(args, uid)
	}
	q += ` ORDER BY updated_at DESC LIMIT ? OFFSET ?`
	args = append(args, limit, offset)
	if rows, err = h.DB.QueryContext(ctx, q, args...); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
	defer rows.Close()

	out := []fiber.Map{}
	for rows.Next() {
		var (
			id, title            string
			uid                  sql.NullInt32
			gk                   sql.NullString
			created, updated     time.Time
		)
		if err := rows.Scan(&id, &uid, &gk, &title, &created, &updated); err != nil {
			continue
		}
		out = append(out, fiber.Map{
			"conv_id": id, "user_id": uid.Int32, "has_user": uid.Valid,
			"guest": gk.Valid, "title": title,
			"created_at": created.Format(time.RFC3339), "updated_at": updated.Format(time.RFC3339),
		})
	}
	return c.JSON(fiber.Map{"items": out, "limit": limit, "offset": offset})
}

// GET /admin/support-tickets?status=&limit= —— 全量工单（坐席视角）
func (h *AdminHandler) ListSupportTickets(c *fiber.Ctx) error {
	limit, offset := paging(c, 50, 500)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	q := `SELECT ticket_no, conv_id, user_id, order_no, category, path, status,
	             assigned_to, summary, created_at, updated_at FROM support_tickets`
	args := []any{}
	if st := strings.TrimSpace(c.Query("status")); st != "" {
		q += ` WHERE status = ?`
		args = append(args, st)
	}
	q += ` ORDER BY created_at DESC LIMIT ? OFFSET ?`
	args = append(args, limit, offset)

	rows, err := h.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
	defer rows.Close()

	out := []fiber.Map{}
	for rows.Next() {
		var (
			no, cat, path, status, summary string
			conv, order, assigned           sql.NullString
			uid                             sql.NullInt32
			created, updated                time.Time
		)
		if err := rows.Scan(&no, &conv, &uid, &order, &cat, &path, &status, &assigned, &summary,
			&created, &updated); err != nil {
			continue
		}
		out = append(out, fiber.Map{
			"ticket_no": no, "conv_id": conv.String, "user_id": uid.Int32,
			"order_no_masked": tools.MaskOrderNo(order.String), "category": cat,
			"path": path, "status": status, "assigned_to": assigned.String,
			"summary": summary, "created_at": created.Format(time.RFC3339),
			"updated_at": updated.Format(time.RFC3339),
		})
	}
	return c.JSON(fiber.Map{"items": out, "limit": limit, "offset": offset})
}

type ticketStateReq struct {
	Status string `json:"status"`
	// AssignedTo 坐席**用户 id**（`support_tickets.assigned_to` 是 int 列，§10.4 预留的就是"谁认领"）。
	// 实测教训：这里传字符串会 500（MySQL 严格模式下 int 列收 '坐席A' 直接报错）——认领人必须是 id。
	AssignedTo *int32 `json:"assigned_to"`
}

// POST /admin/support-tickets/:no/state —— 状态机写接口（条件更新 + 影响行数判定）
func (h *AdminHandler) UpdateTicketState(c *fiber.Ctx) error {
	no := strings.TrimSpace(c.Params("no"))
	var req ticketStateReq
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "请求体必须是 JSON"})
	}
	to := strings.TrimSpace(req.Status)
	if to == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "status 不能为空"})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	var cur string
	if err := h.DB.QueryRowContext(ctx, `SELECT status FROM support_tickets WHERE ticket_no = ?`, no).Scan(&cur); err != nil {
		if err == sql.ErrNoRows {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "工单不存在"})
		}
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
	allowed := false
	for _, s := range allowedTicketTransitions[cur] {
		if s == to {
			allowed = true
			break
		}
	}
	if !allowed {
		// 非法流转要明说"当前状态 + 允许的下一跳"，否则坐席只能猜
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{
			"error":   fmt.Sprintf("非法状态流转 %s → %s", cur, to),
			"allowed": allowedTicketTransitions[cur],
		})
	}

	// 条件更新：并发下只有一个人能从 cur 推到 to（与座位/订单占用同一裁决方式）
	var assignee any
	if req.AssignedTo != nil && *req.AssignedTo > 0 {
		assignee = *req.AssignedTo
	}
	res, err := h.DB.ExecContext(ctx,
		`UPDATE support_tickets SET status = ?, assigned_to = COALESCE(?, assigned_to),
		        resolved_at = CASE WHEN ? = 'resolved' THEN CURRENT_TIMESTAMP ELSE resolved_at END,
		        updated_at = CURRENT_TIMESTAMP
		 WHERE ticket_no = ? AND status = ?`, to, assignee, to, no, cur)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{
			"error": "状态已被其他坐席改变，请刷新后重试",
		})
	}
	if h.Metrics != nil {
		h.Metrics.Inc("tickets_state_changed_total:" + to) // 按目标状态分区计数（坐席处理量口径）
	}
	return c.JSON(fiber.Map{"ticket_no": no, "from": cur, "to": to, "assigned_to": req.AssignedTo})
}

// GET /admin/capability —— 能力台账状态分布（B 方案运营视角：supported/roadmap/industry 各多少）
func (h *AdminHandler) CapabilityStats(c *fiber.Ctx) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	rows, err := h.DB.QueryContext(ctx,
		`SELECT status, COUNT(*) FROM cs_capabilities GROUP BY status ORDER BY 2 DESC`)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
	defer rows.Close()
	out := []fiber.Map{}
	total := 0
	for rows.Next() {
		var st string
		var n int
		if rows.Scan(&st, &n) == nil {
			out = append(out, fiber.Map{"status": st, "count": n})
			total += n
		}
	}
	return c.JSON(fiber.Map{"items": out, "total": total})
}

// GET /admin/turns?conv_id=&limit= —— 对话流水（审计；真相源仍是 checkpoint，这里是可读视图）
func (h *AdminHandler) ListTurns(c *fiber.Ctx) error {
	limit, offset := paging(c, 50, 500)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	q := `SELECT conv_id, role, content, category, source, trace_id, created_at FROM cs_messages`
	args := []any{}
	if cid := strings.TrimSpace(c.Query("conv_id")); cid != "" {
		q += ` WHERE conv_id = ?`
		args = append(args, cid)
	}
	q += ` ORDER BY id DESC LIMIT ? OFFSET ?`
	args = append(args, limit, offset)
	rows, err := h.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
	defer rows.Close()
	out := []fiber.Map{}
	for rows.Next() {
		var (
			conv, role, content, tid string
			cat, src                 sql.NullString
			created                  time.Time
		)
		if rows.Scan(&conv, &role, &content, &cat, &src, &tid, &created) != nil {
			continue
		}
		out = append(out, fiber.Map{"conv_id": conv, "role": role, "content": content,
			"category": cat.String, "source": src.String, "trace_id": tid,
			"created_at": created.Format(time.RFC3339)})
	}
	return c.JSON(fiber.Map{"items": out, "limit": limit, "offset": offset})
}

func paging(c *fiber.Ctx, def, max int) (int, int) {
	limit := def
	if v, err := strconv.Atoi(c.Query("limit")); err == nil && v > 0 {
		limit = v
	}
	if limit > max {
		limit = max
	}
	offset := 0
	if v, err := strconv.Atoi(c.Query("offset")); err == nil && v > 0 {
		offset = v
	}
	return limit, offset
}

// RecordGraphEvents 把编排层回传的图事件折进指标（阶段延迟 + 中断/恢复计数）。
// **这是 Go 侧唯一计数点**（§14.1）——编排层不自己算指标。
func RecordGraphEvents(mc *metrics.Collector, raw []byte) int {
	if mc == nil || len(raw) == 0 {
		return 0
	}
	var evs []struct {
		Node     string  `json:"node"`
		Decision string  `json:"decision"`
		MS       float64 `json:"ms"`
	}
	if err := json.Unmarshal(raw, &evs); err != nil {
		return 0
	}
	n := 0
	for _, e := range evs {
		if stage := metrics.StageName(e.Node); stage != "" {
			mc.ObserveStage(stage, e.MS)
			n++
		}
	}
	return n
}
