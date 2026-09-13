package api

import (
	"context"
	"database/sql"
	"strings"

	"github.com/rs/zerolog/log"

	"github.com/Books-QAQ/tickets/internal/ai/kb"
	"github.com/Books-QAQ/tickets/internal/ai/llm"
	"github.com/Books-QAQ/tickets/internal/ai/tools"
	"github.com/Books-QAQ/tickets/internal/util"

	db "github.com/Books-QAQ/tickets/internal/db/sqlc"
)

// CSComponents 智能AI客服的 Go 侧能力组件
// （M1：检索 + 分类 + 引用校验 + LLM 网关；M2：工具层）
type CSComponents struct {
	KB          *kb.Store
	Retriever   *kb.Retriever
	LLM         llm.Provider
	Aux         *kb.Aux
	Index       *kb.Index
	Tools       *tools.Registry
	LLMDegraded bool // true = 走的是 mock provider（degraded.llm）
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
func BuildCSComponents(dbConn *sql.DB, cfg util.Config) (*CSComponents, error) {
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

	return &CSComponents{KB: store, Retriever: retriever, LLM: provider, Aux: aux, Index: index,
		Tools: toolRegistry, LLMDegraded: llmDegraded}, nil
}
