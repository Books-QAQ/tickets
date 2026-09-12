package handlers

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/Books-QAQ/tickets/internal/ai/cite"
	"github.com/Books-QAQ/tickets/internal/ai/classify"
	"github.com/Books-QAQ/tickets/internal/ai/kb"
	"github.com/Books-QAQ/tickets/internal/ai/llm"
)

// InternalHandler 跨语言契约（Go 提供，仅内网；§6.1）
type InternalHandler struct {
	Store     *kb.Store
	Retriever *kb.Retriever
	LLM       llm.Provider
	Aux       *kb.Aux
	DB        *sql.DB
}

func NewInternalHandler(store *kb.Store, retriever *kb.Retriever, provider llm.Provider, aux *kb.Aux, db *sql.DB) *InternalHandler {
	return &InternalHandler{Store: store, Retriever: retriever, LLM: provider, Aux: aux, DB: db}
}

// errKind 统一错误信封 {error:{kind,message}}（§6.1：编排层据 kind 决策，不解析文案）
func errKind(c *fiber.Ctx, status int, kind, msg string) error {
	return c.Status(status).JSON(fiber.Map{"error": fiber.Map{"kind": kind, "message": msg}})
}

type classifyReq struct {
	Question string `json:"question"`
}

// POST /internal/classify/pre-intent
func (h *InternalHandler) PreIntent(c *fiber.Ctx) error {
	var req classifyReq
	if err := c.BodyParser(&req); err != nil || strings.TrimSpace(req.Question) == "" {
		return errKind(c, fiber.StatusBadRequest, "invalid_input", "question 不能为空")
	}
	return c.JSON(fiber.Map{"pre_intent": classify.PreIntent(req.Question)})
}

// POST /internal/classify/rule
func (h *InternalHandler) ClassifyRule(c *fiber.Ctx) error {
	var req classifyReq
	if err := c.BodyParser(&req); err != nil || strings.TrimSpace(req.Question) == "" {
		return errKind(c, fiber.StatusBadRequest, "invalid_input", "question 不能为空")
	}
	res := classify.Score(req.Question)
	return c.JSON(fiber.Map{
		"scores": res.Scores,
		"top1":   res.Top1,
		"top2":   res.Top2,
		"gap":    res.Gap,
		"enough": res.Enough(),
	})
}

type retrieveReq struct {
	Question   string         `json:"question"`
	Category   string         `json:"category"`
	Scores     map[string]int `json:"scores"`
	Visibility []string       `json:"visibility"`
	TopK       int            `json:"top_k"`
	ScopeKind  string         `json:"scope_kind"`
	ScopeRef   string         `json:"scope_ref"`
}

// POST /internal/retrieve
func (h *InternalHandler) Retrieve(c *fiber.Ctx) error {
	var req retrieveReq
	if err := c.BodyParser(&req); err != nil || strings.TrimSpace(req.Question) == "" {
		return errKind(c, fiber.StatusBadRequest, "invalid_input", "question 不能为空")
	}
	if h.Retriever == nil {
		return errKind(c, fiber.StatusServiceUnavailable, "upstream_unavailable", "检索器未初始化")
	}
	if len(req.Visibility) == 0 {
		req.Visibility = []string{"public"}
	}
	if req.TopK > 0 {
		h.Retriever.Cfg.TopK = req.TopK
	}
	res, err := h.Retriever.Retrieve(c.Context(), req.Question, req.Category, req.Scores,
		req.Visibility, req.ScopeKind, req.ScopeRef)
	if err != nil {
		return errKind(c, fiber.StatusServiceUnavailable, "upstream_unavailable", err.Error())
	}

	chunks := make([]fiber.Map, 0, len(res.Chunks))
	for _, ch := range res.Chunks {
		chunks = append(chunks, fiber.Map{
			"chunk_id":     ch.ID,
			"label":        ch.SourceLabel,
			"text":         ch.Content,
			"category":     ch.Category,
			"form":         string(ch.Form),
			"capability":   string(ch.Capability),
			"scope_kind":   ch.ScopeKind,
			"scope_ref":    ch.ScopeRef,
			"heading_path": ch.HeadingPath,
		})
	}
	return c.JSON(fiber.Map{
		"chunks":          chunks,
		"source_labels":   res.SourceLabels,
		"top1_cos":        res.Top1Cos,
		"above_threshold": res.AboveThreshold,
		"vector_mode":     res.VectorMode,
		"empty":           res.Empty,
		"level":           res.Level, // 0 原过滤 / 1 放宽 scope / 2 放宽 capability
		"rerank_used":     res.RerankUsed,
		"form":            string(res.Form),
		"degraded":        res.Degraded,
		"stage_ms":        res.StageMS,
	})
}

type routeReq struct {
	Question    string `json:"question"`
	SessionHint string `json:"session_hint"`
}

// POST /internal/tools/route
// M1：工具层属 M2，这里恒返回空候选（编排层据 E7 落到检索）。**不是**静默降级——
// 空候选 + note 明确说明，编排层可把它计入 capacity_absent 之外的观测。
func (h *InternalHandler) RouteTool(c *fiber.Ctx) error {
	var req routeReq
	if err := c.BodyParser(&req); err != nil {
		return errKind(c, fiber.StatusBadRequest, "invalid_input", "请求体非法")
	}
	return c.JSON(fiber.Map{
		"candidates": []fiber.Map{},
		"note":       "tool layer not implemented in M1 (see M2)",
	})
}

// POST /internal/tools/:name
// M1：工具未实现 → not_implemented（编排层据此走 E7 检索，而不是当成"工具坏了下游"）。
func (h *InternalHandler) ExecTool(c *fiber.Ctx) error {
	name := c.Params("name")
	return c.Status(fiber.StatusNotImplemented).JSON(fiber.Map{
		"ok":         false,
		"error_kind": "not_implemented",
		"message":    fmt.Sprintf("工具 %s 在 M1 未实现（M2 交付）", name),
	})
}

type verifyReq struct {
	Answer       string   `json:"answer"`
	SourceLabels []string `json:"source_labels"`
}

// POST /internal/citation/verify —— 引用校验 + 能力边界声明后处理（§5.9 ④）
func (h *InternalHandler) VerifyCitation(c *fiber.Ctx) error {
	var req verifyReq
	if err := c.BodyParser(&req); err != nil {
		return errKind(c, fiber.StatusBadRequest, "invalid_input", "请求体非法")
	}
	allowed, err := h.Store.ChunksByLabel(c.Context(), req.SourceLabels)
	if err != nil {
		return errKind(c, fiber.StatusServiceUnavailable, "upstream_unavailable", err.Error())
	}
	res := cite.Verify(req.Answer, allowed)
	if res.Dropped != nil {
		h.Aux.Inc("citation_dropped_total")
	}
	if res.BoundaryViolation {
		h.Aux.Inc("boundary_violation_blocked_total")
	}
	if res.BoundaryInjected {
		h.Aux.Inc("answers_with_boundary_total")
	}
	sources := make([]fiber.Map, 0, len(res.ValidSources))
	for _, label := range res.ValidSources {
		if ch, ok := allowed[label]; ok {
			sources = append(sources, fiber.Map{
				"label":      ch.SourceLabel,
				"capability": string(ch.Capability),
				"form":       string(ch.Form),
			})
		}
	}
	return c.JSON(fiber.Map{
		"answer_cleaned":      res.AnswerCleaned,
		"dropped":             len(res.Dropped),
		"dropped_labels":      res.Dropped,
		"valid_sources":       res.ValidSources,
		"sources":             sources,
		"needs_boundary":      res.NeedsBoundary,
		"boundary_injected":   res.BoundaryInjected,
		"boundary_violation":  res.BoundaryViolation,
		"operational_hits":    res.OperationalHits,
	})
}

type ticketReq struct {
	ConvID   string `json:"conv_id"`
	UserID   *int   `json:"user_id"`
	OrderNo  string `json:"order_no"`
	Category string `json:"category"`
	Path     string `json:"path"`
	Summary  string `json:"summary"`
}

// POST /internal/support-tickets —— 转人工建单（§10）
func (h *InternalHandler) CreateSupportTicket(c *fiber.Ctx) error {
	var req ticketReq
	if err := c.BodyParser(&req); err != nil {
		return errKind(c, fiber.StatusBadRequest, "invalid_input", "请求体非法")
	}
	if strings.TrimSpace(req.Summary) == "" {
		return errKind(c, fiber.StatusBadRequest, "invalid_input", "summary 不能为空")
	}
	category := req.Category
	if category == "" {
		category = "other"
	}
	path := req.Path
	if path == "" {
		path = "deterministic"
	}
	ticketNo := "ST" + time.Now().Format("20060102150405") + fmt.Sprintf("%04d", time.Now().UnixNano()%10000)

	// 会话必须先存在（外键）—— 编排层首个请求就会带 conv_id 落会话
	if req.ConvID != "" {
		_, err := h.DB.ExecContext(c.Context(), `
			INSERT INTO cs_conversations (id, user_id, title)
			VALUES (?, ?, ?)
			ON DUPLICATE KEY UPDATE updated_at=CURRENT_TIMESTAMP`,
			req.ConvID, req.UserID, truncate(req.Summary, 60))
		if err != nil {
			return errKind(c, fiber.StatusServiceUnavailable, "upstream_unavailable", err.Error())
		}
	}
	var orderNo any
	if req.OrderNo != "" {
		orderNo = req.OrderNo
	}
	var convID any
	if req.ConvID != "" {
		convID = req.ConvID
	}
	_, err := h.DB.ExecContext(c.Context(), `
		INSERT INTO support_tickets (ticket_no, conv_id, user_id, order_no, category, path, status, summary)
		VALUES (?,?,?,?,?,?,'pending',?)`,
		ticketNo, convID, req.UserID, orderNo, category, path, req.Summary)
	if err != nil {
		return errKind(c, fiber.StatusServiceUnavailable, "upstream_unavailable", err.Error())
	}
	h.Aux.Inc("support_ticket_created_total")
	h.Aux.Inc("transfer_path_" + path)
	return c.JSON(fiber.Map{"ticket_no": ticketNo, "status": "pending"})
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

type turnReq struct {
	ConvID   string `json:"conv_id"`
	UserID   *int   `json:"user_id"`
	Question string `json:"question"`
	Answer   string `json:"answer"`
	Category string `json:"category"`
	Source   string `json:"source"`
	TraceID  string `json:"trace_id"`
}

// POST /internal/session/turns —— 对话流水（审计真相源；图状态不在这里）
func (h *InternalHandler) RecordTurns(c *fiber.Ctx) error {
	var req turnReq
	if err := c.BodyParser(&req); err != nil {
		return errKind(c, fiber.StatusBadRequest, "invalid_input", "请求体非法")
	}
	if req.ConvID == "" || req.TraceID == "" {
		return errKind(c, fiber.StatusBadRequest, "invalid_input", "conv_id / trace_id 必填")
	}
	if _, err := h.DB.ExecContext(c.Context(), `
		INSERT INTO cs_conversations (id, user_id, title)
		VALUES (?, ?, ?)
		ON DUPLICATE KEY UPDATE updated_at=CURRENT_TIMESTAMP`,
		req.ConvID, req.UserID, truncate(req.Question, 60)); err != nil {
		return errKind(c, fiber.StatusServiceUnavailable, "upstream_unavailable", err.Error())
	}
	if _, err := h.DB.ExecContext(c.Context(), `
		INSERT INTO cs_messages (conv_id, role, content, category, source, trace_id)
		VALUES (?, 'user', ?, ?, 'rule', ?), (?, 'assistant', ?, ?, ?, ?)`,
		req.ConvID, req.Question, nullStr(req.Category), req.TraceID,
		req.ConvID, req.Answer, nullStr(req.Category), nullStr(req.Source), req.TraceID); err != nil {
		return errKind(c, fiber.StatusServiceUnavailable, "upstream_unavailable", err.Error())
	}
	return c.JSON(fiber.Map{"ok": true})
}

type metricsReq struct {
	TraceID string `json:"trace_id"`
	Events  []struct {
		Node     string         `json:"node"`
		Decision string         `json:"decision"`
		MS       int64          `json:"ms"`
		Meta     map[string]any `json:"meta"`
	} `json:"events"`
}

// POST /internal/metrics —— 埋点（M1 立骨架；三表口径在 M3）
func (h *InternalHandler) RecordMetrics(c *fiber.Ctx) error {
	var req metricsReq
	if err := c.BodyParser(&req); err != nil {
		return errKind(c, fiber.StatusBadRequest, "invalid_input", "请求体非法")
	}
	for _, e := range req.Events {
		h.Aux.Inc("node_" + e.Node)
		if e.Decision != "" {
			h.Aux.Inc("decision_" + e.Node + "_" + e.Decision)
		}
	}
	return c.JSON(fiber.Map{"ok": true})
}

// GET /internal/metrics/snapshot —— 便于 M1 验收直接读计数（M3 会换成 Prometheus 端点）
func (h *InternalHandler) MetricsSnapshot(c *fiber.Ctx) error {
	return c.JSON(fiber.Map{"counters": h.Aux.Snapshot()})
}

// POST /internal/v1/chat/completions —— OpenAI 兼容（支持 stream:true）
func (h *InternalHandler) ChatCompletions(c *fiber.Ctx) error {
	var req llm.ChatRequest
	if err := c.BodyParser(&req); err != nil {
		return errKind(c, fiber.StatusBadRequest, "invalid_input", "请求体非法")
	}
	if len(req.Messages) == 0 {
		return errKind(c, fiber.StatusBadRequest, "invalid_input", "messages 不能为空")
	}
	if h.LLM == nil {
		return errKind(c, fiber.StatusServiceUnavailable, "upstream_unavailable", "LLM 未初始化")
	}

	if !req.Stream {
		ctx, cancel := context.WithTimeout(c.Context(), 20*time.Second)
		defer cancel()
		resp, err := h.LLM.Chat(ctx, req)
		if err != nil {
			return errKind(c, fiber.StatusServiceUnavailable, "upstream_unavailable", err.Error())
		}
		return c.JSON(fiber.Map{
			"id":      "chatcmpl-local",
			"object":  "chat.completion",
			"model":   h.LLM.Name(),
			"choices": []fiber.Map{{"index": 0, "message": fiber.Map{"role": "assistant", "content": resp.Content}, "finish_reason": "stop"}},
			"usage":   fiber.Map{"prompt_tokens": resp.PromptTokens, "completion_tokens": resp.OutputTokens, "total_tokens": resp.TotalTokens},
		})
	}

	c.Set("Content-Type", "text/event-stream")
	c.Set("Cache-Control", "no-cache")
	c.Set("X-Accel-Buffering", "no")
	c.Context().SetBodyStreamWriter(func(w *bufio.Writer) {
		// ⚠️ 这里**不能**用 c.Context()：流式写入发生在 handler 返回之后，
		// fasthttp 的 RequestCtx 此时已被回收，由它派生的 ctx 会立刻 Done
		// （实测表现：只吐出第一个 chunk 就打印"[生成中断]"）。必须用独立 ctx。
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		writeChunk := func(content string) {
			// ⚠️ 必须是标准 SSE 帧："data: " 前缀 + 空行结尾。
			// 只写裸 JSON 时 OpenAI SDK（langchain-openai 底层）解析不到任何 chunk，
			// 报 "No generation chunks were returned"（M1 实测踩过）。
			body, _ := json.Marshal(map[string]any{
				"id":      "chatcmpl-local",
				"object":  "chat.completion.chunk",
				"model":   h.LLM.Name(),
				"choices": []map[string]any{{"index": 0, "delta": map[string]any{"content": content}}},
			})
			_, _ = w.WriteString("data: ")
			_, _ = w.Write(body)
			_, _ = w.WriteString("\n\n")
			_ = w.Flush()
		}
		resp, err := h.LLM.ChatStream(ctx, req, func(delta string) error {
			writeChunk(delta)
			return nil
		})
		if err != nil {
			writeChunk("\n[生成中断]")
		}
		final, _ := json.Marshal(map[string]any{
			"id": "chatcmpl-local", "object": "chat.completion.chunk", "model": h.LLM.Name(),
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
			"usage":   map[string]any{"total_tokens": tokenOf(resp)},
		})
		_, _ = w.WriteString("data: ")
		_, _ = w.Write(final)
		_, _ = w.WriteString("\n\ndata: [DONE]\n\n")
		_ = w.Flush()
	})
	return nil
}

func tokenOf(r *llm.ChatResponse) int {
	if r == nil {
		return 0
	}
	return r.TotalTokens
}

func nullStr(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}
