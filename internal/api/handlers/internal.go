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

	"github.com/Books-QAQ/tickets/internal/ai/answercache"
	"github.com/Books-QAQ/tickets/internal/ai/cite"
	"github.com/Books-QAQ/tickets/internal/ai/classify"
	"github.com/Books-QAQ/tickets/internal/ai/kb"
	"github.com/Books-QAQ/tickets/internal/ai/llm"
	"github.com/Books-QAQ/tickets/internal/ai/tools"
)

// InternalHandler 跨语言契约（Go 提供，仅内网；§6.1）
type InternalHandler struct {
	Store     *kb.Store
	Retriever *kb.Retriever
	LLM       llm.Provider
	Aux       *kb.Aux
	Tools     *tools.Registry
	Cache     *answercache.Cache
	DB        *sql.DB
}

func NewInternalHandler(store *kb.Store, retriever *kb.Retriever, provider llm.Provider, aux *kb.Aux,
	reg *tools.Registry, cache *answercache.Cache, db *sql.DB) *InternalHandler {
	return &InternalHandler{Store: store, Retriever: retriever, LLM: provider, Aux: aux,
		Tools: reg, Cache: cache, DB: db}
}

// ---------- M3：答案缓存（§9.3，查/存都在图内节点发起）----------

type cacheLookupReq struct {
	Question string `json:"question"`
	Category string `json:"category"`
}

// POST /internal/cache/lookup
func (h *InternalHandler) CacheLookup(c *fiber.Ctx) error {
	var req cacheLookupReq
	if err := c.BodyParser(&req); err != nil || strings.TrimSpace(req.Question) == "" {
		return errKind(c, fiber.StatusBadRequest, "invalid_input", "question 不能为空")
	}
	if h.Cache == nil {
		// 缓存未装配：明确说“没启用”，不要让它看起来像“没命中”（口径要能区分）
		return c.JSON(fiber.Map{"hit": false, "vector_mode": "disabled"})
	}
	res := h.Cache.Lookup(c.Context(), req.Question, req.Category)
	return c.JSON(fiber.Map{
		"hit": res.Hit, "answer": res.Answer, "sources": res.Sources,
		"score": res.Score, "vector_mode": res.VectorMode,
	})
}

type cacheStoreReq struct {
	Question     string                   `json:"question"`
	Category     string                   `json:"category"`
	Answer       string                   `json:"answer"`
	Sources      []answercache.SourceRef  `json:"sources"`
	Personalized bool                     `json:"personalized"` // 含订单号/工具直答 → 拒绝入缓存
}

// POST /internal/cache/store
func (h *InternalHandler) CacheStore(c *fiber.Ctx) error {
	var req cacheStoreReq
	if err := c.BodyParser(&req); err != nil || strings.TrimSpace(req.Question) == "" {
		return errKind(c, fiber.StatusBadRequest, "invalid_input", "question 不能为空")
	}
	if h.Cache == nil {
		return c.JSON(fiber.Map{"stored": false, "reason": "disabled"})
	}
	res := h.Cache.Store(c.Context(), req.Question, req.Category, req.Answer, req.Sources, req.Personalized)
	return c.JSON(fiber.Map{"stored": res.Stored, "promoted": res.Promoted,
		"evicted": res.Evicted, "reason": res.Reason})
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
	Category    string `json:"category"` // 分类（同分破平用；分类器在 Python 侧已跑完）
	SessionHint string `json:"session_hint"`
}

// POST /internal/tools/route
// M2：按注册表给出候选工具 + **确定性抽好的槽位**（Python 原样回传到执行端）。
// 多候选不猜（§8.1）：候选按分数排序返回，是否反问由编排层决定。
func (h *InternalHandler) RouteTool(c *fiber.Ctx) error {
	var req routeReq
	if err := c.BodyParser(&req); err != nil || strings.TrimSpace(req.Question) == "" {
		return errKind(c, fiber.StatusBadRequest, "invalid_input", "question 不能为空")
	}
	if h.Tools == nil {
		return errKind(c, fiber.StatusServiceUnavailable, "tool_layer_unavailable", "工具层未装配")
	}
	cands := h.Tools.Route(req.Question, req.Category)
	h.Aux.Inc("tool_route_calls_total")
	if len(cands) == 0 {
		h.Aux.Inc("tool_route_no_candidate_total")
	}
	return c.JSON(fiber.Map{"candidates": cands, "session_hint": req.SessionHint})
}

// execReq 执行请求。注意 **args 就是 route 阶段抽好的槽位原样回传**，
// user_id / guest_key 由 Go 侧在公网入口从 JWT 解析后中继（§8.5.1：工具入参没有 user_id，
// 编排层不解析身份、也不产生身份）。
type execReq struct {
	Args     tools.SlotSet `json:"args"`
	UserID   *int32        `json:"user_id"`
	GuestKey string        `json:"guest_key"`
	TraceID  string        `json:"trace_id"`
}

// POST /internal/tools/:name —— 执行工具（Go 执行 / Python 决策，§8）
func (h *InternalHandler) ExecTool(c *fiber.Ctx) error {
	name := c.Params("name")
	if h.Tools == nil {
		return errKind(c, fiber.StatusServiceUnavailable, "tool_layer_unavailable", "工具层未装配")
	}
	var req execReq
	if err := c.BodyParser(&req); err != nil {
		return errKind(c, fiber.StatusBadRequest, "invalid_input", "请求体非法")
	}
	// 白名单校验：编排层的 LLM 兜底路由可能给出不存在的工具名（防幻觉，§8.1）
	if !h.Tools.Has(name) {
		h.Aux.Inc("tool_unknown_total")
		return errKind(c, fiber.StatusBadRequest, "unknown_tool",
			fmt.Sprintf("工具 %s 不在注册表中", name))
	}

	// 身份：只认服务端中继进来的 user_id；没有就是游客（个人工具会返回 guest_required）
	id := tools.Identity{Guest: true, GuestKey: req.GuestKey}
	if req.UserID != nil && *req.UserID > 0 {
		id = tools.Identity{UserID: *req.UserID}
	}

	// ⚠️ 用 WithoutCancel 派生：fasthttp 的 RequestCtx 在 handler 返回后被回收，
	// 若直接把它当父 ctx，超时 goroutine 会拿到一个已取消/被复用的 ctx
	// （M1 的流式写入就踩过这个坑）。这里由工具层自己的超时负责取消。
	ctx := context.WithoutCancel(c.Context())
	res := h.Tools.RunNamed(ctx, name, id, req.Args)
	return c.JSON(res)
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
