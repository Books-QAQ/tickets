package api

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog/log"

	"github.com/Books-QAQ/tickets/internal/ai/answercache"
	"github.com/Books-QAQ/tickets/internal/ai/kb"
	"github.com/Books-QAQ/tickets/internal/ai/llm"
	"github.com/Books-QAQ/tickets/internal/ai/tools"
	"github.com/Books-QAQ/tickets/internal/util"

	db "github.com/Books-QAQ/tickets/internal/db/sqlc"
)

// CSComponents 智能AI客服的 Go 侧能力组件
// （M1：检索 + 分类 + 引用校验 + LLM 网关；M2：工具层；M3：答案缓存）
type CSComponents struct {
	KB          *kb.Store
	Retriever   *kb.Retriever
	LLM         llm.Provider
	Aux         *kb.Aux
	Index       *kb.Index
	Tools       *tools.Registry
	Cache       *answercache.Cache
	LLMDegraded bool // true = 走的是 mock provider（degraded.llm）
}

// AuxCounter 暴露计数点（M4 指标收集器要用它做单一计数源；CS 组件为 nil 时返回 nil）
func (c *CSComponents) AuxCounter() *kb.Aux {
	if c == nil {
		return nil
	}
	return c.Aux
}

// embAdapter 把 kb.Embedder（批量签名 Embed(ctx, []string)）适配成 answercache 需要的单条签名。
// 适配器只有一层、不做重试/缓存（embedding 缓存由 kb 侧负责），避免两处口径。
type embAdapter struct{ e kb.Embedder }

func (a embAdapter) Semantic() bool { return a.e.Semantic() }

func (a embAdapter) Embed(ctx context.Context, text string) ([]float32, error) {
	vecs, err := a.e.Embed(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	if len(vecs) == 0 {
		return nil, nil
	}
	return vecs[0], nil
}

// WithCS 注入客服组件
func (server *Server) WithCS(cs *CSComponents) *Server {
	server.CS = cs
	return server
}

// BuildCSComponents 依据配置装配组件。
//
// 降级纪律（§12.1）：任何降级都必须**可见**——
//   - 未配置真实 embedding → 用 FakeEmbedder（词面哈希）并让检索结果标 vector_mode=ngram + degraded.vector=true
//   - 未配置真实 LLM → 用 mock provider，并在响应里标 degraded.llm=true
//   - Qdrant 不可达 → 向量路关闭（字面替身），BM25 仍可服务
//   - M3：Redis 不可用/未配置 → 答案缓存整体禁用（永不相中，并在响应里标 vector_mode=disabled）
func BuildCSComponents(dbConn *sql.DB, redisClient *redis.Client, cfg util.Config) (*CSComponents, error) {
	ctx := context.Background()
	store := kb.NewStore(dbConn)
	aux := kb.NewAux()

	dim := cfg.EmbeddingDim
	if dim <= 0 {
		dim = 1024
	}

	var emb kb.Embedder
	if strings.EqualFold(cfg.EmbeddingProvider, "openai") && cfg.EmbeddingAPIKey != "" && cfg.EmbeddingBaseURL != "" {
		emb = &kb.OpenAIEmbedder{BaseURL: strings.TrimSuffix(cfg.EmbeddingBaseURL, "/"),
			APIKey: cfg.EmbeddingAPIKey, Model: cfg.EmbeddingModel, DimN: dim}
	} else {
		emb = kb.FakeEmbedder{DimN: dim}
		log.Warn().Msg("embedding 未配置真实供应商，使用词面替身（vector_mode=ngram，检索质量不代表真实水平）")
	}

	var vec kb.VectorRetriever
	if cfg.QdrantURL != "" {
		q := &kb.Qdrant{BaseURL: strings.TrimSuffix(cfg.QdrantURL, "/"), APIKey: cfg.QdrantAPIKey,
			Collection: cfg.QdrantCollection}
		if q.Collection == "" {
			q.Collection = "kb_current"
		}
		if err := q.EnsureCollection(ctx, dim); err != nil {
			log.Error().Err(err).Msg("qdrant 不可用，向量路关闭（降级为字面替身）")
		} else {
			if err := q.CreatePayloadIndexes(ctx); err != nil {
				log.Error().Err(err).Msg("qdrant payload 索引创建失败（过滤性能会受影响）")
			}
			vec = q
		}
	}

	var reranker kb.Reranker = kb.FusionReranker{}
	if cfg.RerankAPIKey != "" && cfg.RerankBaseURL != "" {
		reranker = &kb.HTTPReranker{BaseURL: strings.TrimSuffix(cfg.RerankBaseURL, "/"),
			APIKey: cfg.RerankAPIKey, Model: cfg.RerankModel}
	}

	provider, llmDegraded := llm.New(cfg.LLMProvider, cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.LLMModel, cfg.LLMMockOperational)
	if llmDegraded {
		log.Warn().Msg("LLM 未配置真实供应商，使用 mock provider（degraded.llm=true，答案不代表真实生成质量）")
	}

	index := kb.NewIndex()
	metas, contents, err := store.AllChunkMetas(ctx)
	if err != nil {
		log.Error().Err(err).Msg("cannot load kb chunks for BM25 index")
	} else {
		for i := range metas {
			if i < len(contents) {
				index.Add(metas[i].ChunkID, contents[i], metas[i])
			}
		}
		index.Finalize()
		log.Info().Int("chunks", index.Size()).Msg("BM25 index built")
	}

	retrieveCfg := kb.DefaultRetrieveConfig()
	if cfg.ThresholdQA > 0 {
		retrieveCfg.ThresholdQA = cfg.ThresholdQA
	}
	if cfg.ThresholdProse > 0 {
		retrieveCfg.ThresholdProse = cfg.ThresholdProse
	}
	if cfg.LiteralThreshold > 0 {
		retrieveCfg.LiteralThreshold = cfg.LiteralThreshold
	}

	retriever := &kb.Retriever{
		Store:  store,
		Index:  func() *kb.Index { return index },
		Vec:    vec,
		Emb:    emb,
		Rerank: reranker,
		Cfg:    retrieveCfg,
		Aux:    aux,
		Logf: func(format string, args ...any) {
			log.Warn().Msgf(format, args...)
		},
	}

	// —— 工具层（M2）——
	// 工具注册表：读工具复用同一个 aux 计数点（Go 侧单一计数来源，§5.10）
	toolCfg := tools.DefaultConfig()
	toolCfg.PenaltySemanticsConfirmed = cfg.PenaltySemanticsConfirmed
	if util.IsSet("GUEST_TICKET_ALLOWED") {
		// 只有显式配置才覆盖默认值：19.3 未拍板时保持 M1 已验收行为（游客可建单）
		toolCfg.GuestTicketAllowed = cfg.GuestTicketAllowed
	}
	if !util.IsSet("GUEST_TICKET_ALLOWED") {
		log.Warn().Msg("19.3 未拍板：游客建单沿用默认（允许）。要改为引导登录，请设 GUEST_TICKET_ALLOWED=0")
	}
	dbStore := db.NewStore(dbConn)
	toolRegistry := tools.BuildRegistry(tools.Deps{
		Store:    dbStore,
		Cfg:      toolCfg,
		Cnt:      aux,
		Stations: tools.NewStationProvider(dbStore),
	})
	if !toolCfg.PenaltySemanticsConfirmed {
		log.Warn().Msg("退票费工具闸门未开启（19.2 penalties 语义未确认）→ refund_fee 不下结论，走 FAQ + 转人工")
	}
	// PYTHON_BASE_URL 没配 = 每个 /cs/ask 都会走降级建单。
	// M3 实测踩过：M2 的验收一直**直接打编排层**，所以"Go→编排层"这条路从没被跑过，
	// 配漏了也看不出来，表现是"用户问什么都回工单号"。这种静默降级必须在启动日志里报错。
	if strings.TrimSpace(cfg.PythonBaseURL) == "" {
		log.Error().Msg("PYTHON_BASE_URL 未配置：/cs/ask 无法访问编排层，**每一轮都会降级为直接建单**（用户只会拿到工单号）")
	} else {
		log.Info().Str("python_base_url", cfg.PythonBaseURL).Msg("编排层入口已配置")
	}

	// —— M3：答案级语义缓存（§9.3）——
	// 复用检索用的同一个 embedder：缓存匹配阈值 0.95 比检索阈值 0.60 高得多，
	// 且**词面替身时必须诚实标注**（ngram 下"同义改写"匹配不成立，只对字面近同的问题有效）。
	acCfg := answercache.DefaultConfig()
	if acCfg.TTL <= 0 {
		acCfg.TTL = 24 * time.Hour
	}
	var acStore answercache.Store
	if redisClient != nil {
		acStore = answercache.RedisStore{Client: redisClient}
	}
	answerCache := answercache.New(acStore, embAdapter{emb}, acCfg, aux)
	if redisClient == nil {
		log.Warn().Msg("Redis 未就绪：答案缓存整体禁用（响应里 vector_mode=disabled，不是静默降级）")
	}

	return &CSComponents{KB: store, Retriever: retriever, LLM: provider, Aux: aux, Index: index,
		Tools: toolRegistry, Cache: answerCache, LLMDegraded: llmDegraded}, nil
}
