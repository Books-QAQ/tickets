package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/redis/go-redis/v9"

	"github.com/Books-QAQ/tickets/internal/metrics"
	"github.com/Books-QAQ/tickets/internal/util"
)

// OpsHandler 运维面：/readyz（依赖就绪）与 /admin/metrics（大盘 JSON）。
// `/metrics` 的 Prometheus 文本也在这里渲染（鉴权由路由层的 adminAuth 负责）。
type OpsHandler struct {
	DB      *sql.DB
	Redis   *redis.Client
	Config  util.Config
	Metrics *metrics.Collector
}

func NewOpsHandler(db *sql.DB, rdb *redis.Client, cfg util.Config, mc *metrics.Collector) *OpsHandler {
	return &OpsHandler{DB: db, Redis: rdb, Config: cfg, Metrics: mc}
}

// GET /readyz —— 逐项布尔（§16.1）。**不返回任何密钥/口令**（只报布尔与必要计数）。
//
// 与 /healthz 的区别：/healthz 只回答"进程活着"，/readyz 回答"依赖齐不齐、能不能接客"。
func (h *OpsHandler) Ready(c *fiber.Ctx) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	checks := map[string]any{}
	// db：MySQL 不可用 = 整链路不可用（§14.4）
	dbOK := false
	if h.DB != nil {
		dbOK = h.DB.PingContext(ctx) == nil
	}
	checks["db"] = dbOK

	// redis：不可用时缓存/限流降级，但服务可用（§14.4）
	redisOK := false
	if h.Redis != nil {
		redisOK = h.Redis.Ping(ctx).Err() == nil
	}
	checks["redis"] = redisOK

	// kb_chunks > 0：未入库则问答一律转人工（§14.4）
	chunks := -1
	if h.DB != nil {
		var n int
		if err := h.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM kb_chunks").Scan(&n); err == nil {
			chunks = n
		}
	}
	checks["kb_chunks"] = chunks
	checks["kb_loaded"] = chunks > 0

	// llm / embedding：只报"是否配置了真实供应商"，**绝不回显 URL 与 key**（§16 泄露面）
	checks["llm"] = strings.TrimSpace(h.Config.LLMProvider) != "" && h.Config.LLMProvider != "mock"
	checks["llm_provider_is_mock"] = h.Config.LLMProvider == "mock" || h.Config.LLMProvider == ""
	checks["embedding"] = strings.TrimSpace(h.Config.EmbeddingProvider) != ""
	checks["vector_mode"] = vectorMode(h.Config)

	// qdrant：查集合信息（失败报 -1，不谎报 0）
	checks["qdrant_points"] = qdrantPoints(ctx, h.Config)

	// orchestrator：探测编排层 healthz（超时要短，别把 readyz 拖死）
	pyOK := false
	if base := strings.TrimSuffix(h.Config.PythonBaseURL, "/"); base != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/healthz", nil)
		if err == nil {
			if resp, err := (&http.Client{Timeout: 1500 * time.Millisecond}).Do(req); err == nil {
				io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
				resp.Body.Close()
				pyOK = resp.StatusCode == 200
			}
		}
	}
	checks["orchestrator"] = pyOK
	checks["orchestrator_configured"] = strings.TrimSpace(h.Config.PythonBaseURL) != ""

	ready := dbOK // 只要 DB 可用就算"能接客"（其余项各有降级路径，见 §14.4）
	status := fiber.StatusOK
	if !ready {
		status = fiber.StatusServiceUnavailable
	}
	return c.Status(status).JSON(fiber.Map{"ready": ready, "checks": checks})
}

// GET /metrics —— Prometheus 文本（text/plain; version=0.0.4）
func (h *OpsHandler) Prometheus(c *fiber.Ctx) error {
	if h.Metrics == nil {
		return c.Status(fiber.StatusServiceUnavailable).SendString("# metrics collector 未装配\n")
	}
	c.Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	return c.SendString(h.Metrics.Render())
}

// GET /admin/metrics —— 大盘 JSON（admin.html 用；与 Prometheus 同源，不是第二套口径）
func (h *OpsHandler) AdminMetrics(c *fiber.Ctx) error {
	if h.Metrics == nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "metrics collector 未装配"})
	}
	stages := map[string]any{}
	for _, s := range []string{"load_context", "pre_intent", "classify", "coref", "cache_lookup",
		"route_tool", "exec_tool", "retrieve", "rewrite", "generate", "verify", "transfer", "finalize"} {
		p50, p95, avg, n := h.Metrics.StageStats(s)
		if n > 0 {
			stages[s] = fiber.Map{"p50": p50, "p95": p95, "avg": avg, "samples": n}
		}
	}
	conf := map[string]any{}
	// 只暴露"是不是真实供应商"，不回显任何 URL/key
	conf["llm_provider"] = providerOrMock(h.Config.LLMProvider)
	conf["embedding_provider"] = providerOrMock(h.Config.EmbeddingProvider)
	conf["rerank_provider"] = providerOrMock(h.Config.RerankProvider)
	conf["vector_mode"] = vectorMode(h.Config)
	return c.JSON(fiber.Map{
		"uptime_seconds": h.Metrics.UptimeSeconds(),
		"counters":       h.Metrics.Snapshot(),
		"stage_ms":       stages,
		"config_flags":   conf,
	})
}

func providerOrMock(p string) string {
	if strings.TrimSpace(p) == "" {
		return "mock(降级)"
	}
	return p
}

func vectorMode(cfg util.Config) string {
	if strings.TrimSpace(cfg.EmbeddingProvider) == "" {
		return "ngram" // 字面替身（诚实标注，§14.4）
	}
	return "embedding"
}

// qdrantPoints 查 Qdrant 集合点数；**失败返回 -1**（"查不到"不能伪装成 0）
func qdrantPoints(ctx context.Context, cfg util.Config) int64 {
	base := strings.TrimSuffix(cfg.QdrantURL, "/")
	if base == "" {
		return -1
	}
	col := cfg.QdrantCollection
	if col == "" {
		col = "kb_chunks"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/collections/"+col, nil)
	if err != nil {
		return -1
	}
	if cfg.QdrantAPIKey != "" {
		req.Header.Set("api-key", cfg.QdrantAPIKey)
	}
	resp, err := (&http.Client{Timeout: 1500 * time.Millisecond}).Do(req)
	if err != nil {
		return -1
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return -1
	}
	var out struct {
		Result struct {
			PointsCount *int64 `json:"points_count"`
		} `json:"result"`
	}
	if json.Unmarshal(raw, &out) != nil || out.Result.PointsCount == nil {
		return -1
	}
	return *out.Result.PointsCount
}

// 便于排障时打一行就够的摘要（不含敏感值）
func (h *OpsHandler) Summary() string {
	return fmt.Sprintf("vector_mode=%s llm=%s", vectorMode(h.Config), providerOrMock(h.Config.LLMProvider))
}
