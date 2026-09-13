package handlers

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	db "github.com/Books-QAQ/tickets/internal/db/sqlc"
	"github.com/Books-QAQ/tickets/internal/token"
	"github.com/Books-QAQ/tickets/internal/util"
)

// CSHandler 智能AI客服的用户侧接口（公网入口，Go 是唯一公网入口——ADR-4）。
//
// 职责（确定性部分）：
//   - 身份解析：JWT 可选（游客可问公共问题）；游客锚点只存 device_id 的 hash
//   - 会话行维护（cs_conversations）、流水读取（cs_messages）
//   - **SSE 透传**：把编排层的事件流原样转发给前端（不缓冲、不 ReadAll）
//   - **降级**：编排层不可用时直接建单并给可追踪工单号（§6.4，绝不给 503）
type CSHandler struct {
	Store       *db.Store
	Redis       *redis.Client
	TokenMaker  token.Maker
	Config      util.Config
	InternalKey string
	Aux         Counter
}

// Counter 计数点（M1 的 kb.Aux 满足）
type Counter interface{ Inc(key string) }

func NewCSHandler(store *db.Store, rdb *redis.Client, tm token.Maker, cfg util.Config, aux Counter) *CSHandler {
	return &CSHandler{Store: store, Redis: rdb, TokenMaker: tm, Config: cfg,
		InternalKey: cfg.InternalKey, Aux: aux}
}

func (h *CSHandler) inc(key string) {
	if h.Aux != nil {
		h.Aux.Inc(key)
	}
}

// identity 解析身份：**服务端解析，接口不接受 user_id 入参**（§8.5.1 同一纪律）
type identity struct {
	UserID   int32
	Guest    bool
	GuestKey string // sha256(device_id)，不存明文
}

func (h *CSHandler) resolveIdentity(c *fiber.Ctx, deviceID string) identity {
	if raw := bearerToken(c); raw != "" && h.TokenMaker != nil {
		if payload, err := h.TokenMaker.VerifyToken(raw); err == nil && payload != nil {
			if user, err := h.Store.GetUserByUsername(c.Context(), payload.Username); err == nil {
				return identity{UserID: user.ID}
			}
		}
	}
	id := identity{Guest: true}
	if deviceID != "" {
		sum := sha256.Sum256([]byte(deviceID))
		id.GuestKey = hex.EncodeToString(sum[:])
	}
	return id
}

func bearerToken(c *fiber.Ctx) string {
	h := c.Get("Authorization")
	if strings.HasPrefix(strings.ToLower(h), "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

type askReq struct {
	Question string `json:"question"`
	ConvID   string `json:"conv_id"`
	DeviceID string `json:"device_id"`
	Stream   *bool  `json:"stream"` // 不传 = 看 query ?stream=1
}

// POST /cs/ask —— 问答主入口（非流式 JSON / ?stream=1 走 SSE）
func (h *CSHandler) Ask(c *fiber.Ctx) error {
	var req askReq
	if err := c.BodyParser(&req); err != nil || strings.TrimSpace(req.Question) == "" {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "question 不能为空"})
	}
	wantStream := c.Query("stream") == "1"
	if req.Stream != nil {
		wantStream = *req.Stream
	}
	convID := strings.TrimSpace(req.ConvID)
	if convID == "" {
		convID = uuid.NewString()
	}
	if _, err := uuid.Parse(convID); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "conv_id 必须是 UUID"})
	}
	traceID := uuid.NewString()
	ident := h.resolveIdentity(c, req.DeviceID)
	h.ensureConversation(c.Context(), convID, ident, req.Question)

	payload := map[string]any{
		"question": req.Question,
		"conv_id":  convID,
		"trace_id": traceID,
		"visibility": func() []string {
			if ident.UserID > 0 {
				return []string{"public", "authenticated"}
			}
			return []string{"public"}
		}(),
	}
	if ident.UserID > 0 {
		payload["user_id"] = ident.UserID
	}
	if ident.GuestKey != "" {
		payload["guest_key"] = ident.GuestKey
	}

	if !wantStream {
		payload["stream"] = false // 显式声明：不回 JSON 的默认值留给调用方猜是事故来源
		return h.askJSON(c, payload, convID, traceID, ident)
	}
	return h.askStream(c, payload, convID, traceID, ident)
}

// askJSON 非流式：编排层 8s 预算 + 降级建单
func (h *CSHandler) askJSON(c *fiber.Ctx, payload map[string]any, convID, traceID string, ident identity) error {
	ctx, cancel := context.WithTimeout(context.Background(), 9*time.Second)
	defer cancel()
	res, err := h.callPython(ctx, payload)
	if err != nil {
		h.inc("orchestrator_unavailable_total")
		out, derr := h.degradeToTicket(ctx, convID, traceID, ident, payload["question"].(string), "orchestrator_unavailable")
		if derr != nil {
			return c.Status(http.StatusServiceUnavailable).JSON(fiber.Map{"error": "服务暂时不可用"})
		}
		return c.JSON(out)
	}
	res["conv_id"] = convID
	res["trace_id"] = traceID
	return c.JSON(res)
}

// askStream SSE 透传（§6.3）：不缓冲、逐事件 Flush；写失败即取消上游；读空闲超时而非整轮超时
func (h *CSHandler) askStream(c *fiber.Ctx, payload map[string]any, convID, traceID string, ident identity) error {
	payload["stream"] = true
	body, _ := json.Marshal(payload)

	c.Set("Content-Type", "text/event-stream")
	c.Set("Cache-Control", "no-cache")
	c.Set("Connection", "keep-alive")
	c.Set("X-Accel-Buffering", "no")

	pyURL := strings.TrimSuffix(h.Config.PythonBaseURL, "/") + "/ask"
	if pyURL == "/ask" { // 未配置编排层 → 直接降级（不能 503）
		pyURL = ""
	}

	c.Context().SetBodyStreamWriter(func(w *bufio.Writer) {
		// ⚠️ 这里不能用 c.Context()：流式写入发生在 handler 返回之后，fasthttp 的 RequestCtx 已被回收
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		writeErr := func(event string, data any) error {
			raw, _ := json.Marshal(data)
			if _, err := w.WriteString("event: " + event + "\n"); err != nil {
				return err
			}
			if _, err := w.WriteString("data: " + string(raw) + "\n\n"); err != nil {
				return err
			}
			return w.Flush()
		}

		if pyURL == "" {
			h.streamDegrade(ctx, writeErr, convID, traceID, ident, payload["question"].(string))
			return
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, pyURL, bytes.NewReader(body))
		if err != nil {
			h.streamDegrade(ctx, writeErr, convID, traceID, ident, payload["question"].(string))
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Internal-Key", h.InternalKey)

		resp, err := h.streamClient().Do(req)
		if err != nil {
			h.inc("orchestrator_unavailable_total")
			h.streamDegrade(ctx, writeErr, convID, traceID, ident, payload["question"].(string))
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 400 {
			h.inc("orchestrator_unavailable_total")
			h.streamDegrade(ctx, writeErr, convID, traceID, ident, payload["question"].(string))
			return
		}

		// 逐事件转发：读在 goroutine 里做，主循环用 select 实现"读空闲超时"与"客户端断开取消"
		type chunk struct {
			line string
			err  error
		}
		ch := make(chan chunk, 16)
		go func() {
			sc := bufio.NewScanner(resp.Body)
			sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
			for sc.Scan() {
				ch <- chunk{line: sc.Text()}
			}
			ch <- chunk{err: sc.Err()}
			close(ch)
		}()

		idle := time.NewTimer(readIdleTimeout)
		defer idle.Stop()
		prevBlank := true // 流首视为"空白"，避免开头先吐一个空行
		for {
			select {
			case <-ctx.Done():
				return
			case <-idle.C:
				// 读空闲超时：上游既没输出也没关闭 → 主动取消（省下后续 token 花费）
				h.inc("sse_idle_timeout_total")
				_ = writeErr("error", map[string]any{"kind": "upstream_idle_timeout"})
				return
			case item, ok := <-ch:
				if !ok {
					return
				}
				if item.err != nil {
					return
				}
				if strings.TrimSpace(item.line) == "" {
					// **空行是 SSE 的帧分隔符，必须转发**（旧实现直接 continue 丢掉了 →
					// 客户端永远拼不出一个完整帧：curl 看着有内容、任何按 \n\n 切帧的
					// 解析器（前端/验收脚本）都读到空。M3 实测踩过）
					// 只在上一行非空时补一个空行，避免连吐空行产生无意义的分隔。
					if prevBlank {
						continue
					}
					prevBlank = true
					if _, werr := w.WriteString("\n"); werr != nil {
						h.inc("sse_client_aborted_total")
						return
					}
					if err := w.Flush(); err != nil {
						h.inc("sse_client_aborted_total")
						return
					}
					if !idle.Stop() {
						select {
						case <-idle.C:
						default:
						}
					}
					idle.Reset(readIdleTimeout)
					continue
				}
				prevBlank = false
				// 统一行尾为 "\n"：编排层（sse-starlette）用的是 CRLF，原样透传会让
				// "帧分隔 = \r\n\r\n"，而前端/验收脚本普遍按 "\n\n" 切帧 → 一直切不开、
				// 只能等整轮结束才渲染（M3 实测踩过）。Go 作为出口把协议规范成 LF。
				line := strings.TrimRight(item.line, "\r")
				if _, werr := w.WriteString(line + "\n"); werr != nil {
					// 客户端断开 → 取消上游请求（验收项：断开后上游必须被取消）
					h.inc("sse_client_aborted_total")
					return
				}
				if strings.HasPrefix(line, "event: ") || strings.HasPrefix(line, "data: ") {
					if err := w.Flush(); err != nil {
						h.inc("sse_client_aborted_total")
						return
					}
				}
				if !idle.Stop() {
					select {
					case <-idle.C:
					default:
					}
				}
				idle.Reset(readIdleTimeout)
			}
		}
	})
	return nil
}

const readIdleTimeout = 15 * time.Second // §6.3：读空闲超时（而非整轮超时）

func (h *CSHandler) streamClient() *http.Client {
	// 不设整轮 Timeout（长答案会被腰斩）；空闲由上面的 select 控制
	return &http.Client{Transport: &http.Transport{
		ResponseHeaderTimeout: 8 * time.Second,
		DisableCompression:    true,
	}}
}

// streamDegrade 编排层不可用时的 SSE 降级：**仍然给可追踪工单号**（§6.4）
func (h *CSHandler) streamDegrade(ctx context.Context, write func(string, any) error,
	convID, traceID string, ident identity, question string) {
	out, err := h.degradeToTicket(ctx, convID, traceID, ident, question, "orchestrator_unavailable")
	if err != nil {
		_ = write("error", map[string]any{"kind": "orchestrator_unavailable"})
		return
	}
	_ = write("meta", map[string]any{"conv_id": convID, "trace_id": traceID, "degraded": map[string]any{"orchestrator": true}})
	_ = write("delta", map[string]any{"text": out["answer"]})
	_ = write("done", out)
}

// clampRunes 截断到最多 n 个字符（**按 rune 不按字节**：中文 3 字节/字，按字节切会产生
// 半个汉字 + 而且要命的越界 panic）。
// M3 实测：降级建单里写 `question[:500]`，问题只有 84 字节时 panic → 整个服务崩掉 —— 
// **降级路径自己崩是最糟的结果**（本来只是编排层不可用），所有"截断"都必须走这里。
func clampRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// degradeToTicket 降级：Go 直接建单（路径 orchestrator_unavailable），用户永远拿到可追踪工单号
func (h *CSHandler) degradeToTicket(ctx context.Context, convID, traceID string, ident identity,
	question, path string) (map[string]any, error) {
	arg := db.CreateSupportTicketParams{
		ConvID:   sql.NullString{String: convID, Valid: convID != ""},
		Category: "other",
		Path:     path,
		Summary:  clampRunes(fmt.Sprintf("[%s] %s（编排层不可用，Go 直接建单）", path, question), 500),
	}
	if ident.UserID > 0 {
		arg.UserID = sql.NullInt32{Int32: ident.UserID, Valid: true}
	}
	rec, err := h.Store.CreateSupportTicket(ctx, arg)
	if err != nil {
		return nil, err
	}
	answer := fmt.Sprintf("已为您转接人工客服，工单号 %s，坐席会尽快与您联系。", rec.TicketNo)
	return map[string]any{
		"conv_id": convID, "trace_id": traceID, "answer": answer, "category": "other",
		"source": "fallback", "sources": []any{}, "transfer": true, "transfer_path": path,
		"support_ticket_no": rec.TicketNo,
		"degraded":          map[string]any{"orchestrator": true},
	}, nil
}

// callPython 非流式调用编排层
func (h *CSHandler) callPython(ctx context.Context, payload map[string]any) (map[string]any, error) {
	base := strings.TrimSuffix(h.Config.PythonBaseURL, "/")
	if base == "" {
		return nil, fmt.Errorf("编排层未配置")
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/ask", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Key", h.InternalKey)

	resp, err := (&http.Client{Timeout: 9 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("编排层返回 %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		// 契约不一致要**说清楚是什么**：最常见的是编排层回了 SSE 而我们按 JSON 解析
		// （M3 实测：编排层 stream 默认值曾是 True，Go 不带该字段 → 每轮静默降级成工单）
		if bytes.HasPrefix(bytes.TrimSpace(raw), []byte("event:")) {
			return nil, fmt.Errorf("编排层返回 SSE 但调用方期望 JSON（stream 契约不一致，body 前 40 字节: %q）", clampRunes(string(raw), 40))
		}
		return nil, fmt.Errorf("编排层返回非 JSON（%d 字节）: %w", len(raw), err)
	}
	return out, nil
}

// ensureConversation 会话行（首轮写入标题；已存在则只更新 updated_at）
func (h *CSHandler) ensureConversation(ctx context.Context, convID string, ident identity, question string) {
	title := []rune(strings.TrimSpace(question))
	if len(title) > 60 {
		title = title[:60]
	}
	_, err := h.Store.RawDB().ExecContext(ctx, `
		INSERT INTO cs_conversations (id, user_id, guest_key, title)
		VALUES (?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE updated_at = CURRENT_TIMESTAMP`, convID,
		sql.NullInt32{Int32: ident.UserID, Valid: ident.UserID > 0},
		sql.NullString{String: ident.GuestKey, Valid: ident.GuestKey != ""},
		string(title))
	if err != nil {
		h.inc("cs_conversation_upsert_error_total")
	}
}

// DELETE /cs/session —— 结束会话（清会话状态；审计流水保留，§9.2）
func (h *CSHandler) DeleteSession(c *fiber.Ctx) error {
	var req struct {
		ConvID   string `json:"conv_id"`
		DeviceID string `json:"device_id"`
	}
	_ = c.BodyParser(&req)
	if req.ConvID == "" {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "conv_id 不能为空"})
	}
	ident := h.resolveIdentity(c, req.DeviceID)
	// 归属校验：只能删自己的会话
	var owner sql.NullInt32
	var guest sql.NullString
	if err := h.Store.RawDB().QueryRowContext(c.Context(),
		`SELECT user_id, guest_key FROM cs_conversations WHERE id = ?`, req.ConvID).Scan(&owner, &guest); err != nil {
		if err == sql.ErrNoRows {
			return c.JSON(fiber.Map{"ok": true, "note": "会话不存在"})
		}
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "查询会话失败"})
	}
	if owner.Valid && owner.Int32 != ident.UserID {
		return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "无权操作该会话"})
	}
	if !owner.Valid && ident.GuestKey != "" && guest.Valid && guest.String != ident.GuestKey {
		return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "无权操作该会话"})
	}
	// 只清"会话状态"：checkpoint 由编排层按保留期清理，流水表保留供审计
	_, _ = h.Store.RawDB().ExecContext(c.Context(), `UPDATE cs_conversations SET updated_at = CURRENT_TIMESTAMP WHERE id = ?`, req.ConvID)
	h.inc("cs_session_deleted_total")
	return c.JSON(fiber.Map{"ok": true})
}

// POST /cs/feedback —— 凭 trace_id 赞/踩（幂等去重）
func (h *CSHandler) PostFeedback(c *fiber.Ctx) error {
	var req struct {
		TraceID  string `json:"trace_id"`
		Rating   string `json:"rating"`
		Comment  string `json:"comment"`
		DeviceID string `json:"device_id"`
	}
	if err := c.BodyParser(&req); err != nil || req.TraceID == "" {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "trace_id 不能为空"})
	}
	if req.Rating != "up" && req.Rating != "down" {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "rating 只能是 up/down"})
	}
	ident := h.resolveIdentity(c, req.DeviceID)
	_, err := h.Store.RawDB().ExecContext(c.Context(), `
		INSERT INTO cs_feedback (trace_id, user_id, rating, comment)
		VALUES (?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE rating = VALUES(rating), comment = VALUES(comment)`,
		req.TraceID, sql.NullInt32{Int32: ident.UserID, Valid: ident.UserID > 0}, req.Rating, req.Comment)
	if err != nil {
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "保存反馈失败"})
	}
	h.inc("cs_feedback_total")
	return c.JSON(fiber.Map{"ok": true})
}

// GET /cs/conversations —— 我的会话列表（只查自己的；user_id 只来自 JWT）
func (h *CSHandler) ListConversations(c *fiber.Ctx) error {
	payload, ok := c.Locals("authorizationPayloadKey").(*token.Payload)
	if !ok || payload == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	user, err := h.Store.GetUserByUsername(c.Context(), payload.Username)
	if err != nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "user not found"})
	}
	limit := 30
	if v := c.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 100 {
			limit = n
		}
	}
	rows, err := h.Store.RawDB().QueryContext(c.Context(), `
		SELECT c.id, c.title, c.updated_at,
		       (SELECT COUNT(*) FROM cs_messages m WHERE m.conv_id = c.id) AS msg_count
		FROM cs_conversations c
		WHERE c.user_id = ?
		ORDER BY c.updated_at DESC
		LIMIT ?`, user.ID, limit)
	if err != nil {
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "查询会话失败"})
	}
	defer rows.Close()
	items := []fiber.Map{}
	for rows.Next() {
		var id, title string
		var updatedAt time.Time
		var msgCount int
		if err := rows.Scan(&id, &title, &updatedAt, &msgCount); err != nil {
			continue
		}
		items = append(items, fiber.Map{"conv_id": id, "title": title, "updated_at": updatedAt, "message_count": msgCount})
	}
	return c.JSON(fiber.Map{"conversations": items, "count": len(items)})
}

// GET /cs/conversations/:id/messages —— 会话消息（**归属校验**：不是自己的会话返回 403）
func (h *CSHandler) ListMessages(c *fiber.Ctx) error {
	payload, ok := c.Locals("authorizationPayloadKey").(*token.Payload)
	if !ok || payload == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	user, err := h.Store.GetUserByUsername(c.Context(), payload.Username)
	if err != nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "user not found"})
	}
	convID := c.Params("id")
	var owner sql.NullInt32
	if err := h.Store.RawDB().QueryRowContext(c.Context(),
		`SELECT user_id FROM cs_conversations WHERE id = ?`, convID).Scan(&owner); err != nil {
		if err == sql.ErrNoRows {
			return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "会话不存在"})
		}
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "查询会话失败"})
	}
	if !owner.Valid || owner.Int32 != user.ID {
		return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "无权查看该会话"})
	}
	rows, err := h.Store.RawDB().QueryContext(c.Context(), `
		SELECT role, content, category, source, created_at
		FROM cs_messages WHERE conv_id = ? ORDER BY id ASC LIMIT 200`, convID)
	if err != nil {
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "查询消息失败"})
	}
	defer rows.Close()
	items := []fiber.Map{}
	for rows.Next() {
		var role, content string
		var category, source sql.NullString
		var createdAt time.Time
		if err := rows.Scan(&role, &content, &category, &source, &createdAt); err != nil {
			continue
		}
		items = append(items, fiber.Map{"role": role, "content": content,
			"category": category.String, "source": source.String, "created_at": createdAt})
	}
	return c.JSON(fiber.Map{"conv_id": convID, "messages": items, "count": len(items)})
}

// POST /cs/support-tickets —— 主动转人工（等价路径①确定性）
func (h *CSHandler) PostSupportTicket(c *fiber.Ctx) error {
	var req struct {
		ConvID   string `json:"conv_id"`
		DeviceID string `json:"device_id"`
		Summary  string `json:"summary"`
		Category string `json:"category"`
	}
	_ = c.BodyParser(&req)
	ident := h.resolveIdentity(c, req.DeviceID)
	if ident.UserID == 0 && ident.GuestKey == "" {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "需要登录或提供 device_id"})
	}
	if req.Category == "" {
		req.Category = "other"
	}
	out, err := h.degradeToTicket(c.Context(), req.ConvID, uuid.NewString(), ident,
		firstNonEmpty(req.Summary, "用户主动请求人工客服"), "deterministic")
	if err != nil {
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "建单失败"})
	}
	h.inc("cs_manual_transfer_total")
	return c.JSON(out)
}

// GET /cs/support-tickets —— 我的工单列表（§10.4 V1 范围）
//
// 鉴权纪律（照抄"用户查自己对话记录"的写法）：
//   - **接口签名上没有 user_id 入参**，一律由服务端从 JWT 解析；
//   - SQL 强制 WHERE user_id = ?，因此换他人 token 必然查不到（横向越权在 SQL 层就不成立）。
func (h *CSHandler) ListSupportTickets(c *fiber.Ctx) error {
	payload, ok := c.Locals("authorizationPayloadKey").(*token.Payload)
	if !ok || payload == nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	user, err := h.Store.GetUserByUsername(c.Context(), payload.Username)
	if err != nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "user not found"})
	}

	limit := 20
	if v := c.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 100 {
			limit = n
		}
	}

	rows, err := h.Store.ListSupportTicketsByUser(c.Context(), user.ID, limit)
	if err != nil {
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "failed to list support tickets"})
	}

	items := make([]fiber.Map, 0, len(rows))
	for _, t := range rows {
		items = append(items, fiber.Map{
			"ticket_no":  t.TicketNo,
			"category":   t.Category,
			"path":       t.Path,
			"status":     t.Status,
			"summary":    t.Summary,
			"created_at": t.CreatedAt,
		})
	}
	return c.JSON(fiber.Map{"tickets": items, "count": len(items)})
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
