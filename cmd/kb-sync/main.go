// kb-sync 知识库批量入库（离线批任务，不在服务启动主链路上）。
//
// 依据 §7.3：
//   - content_hash 命中即跳过（**只重嵌变更 chunk**）；
//   - 断点续跑：中断后重跑不重复嵌入（以 MySQL 里的 content_hash + embedding_model 为游标）；
//   - embedding 模型变更 = 重建（用 --reembed，配合 alias 切换在 M2 补）；
//   - 台账翻转用 --refresh-capability：**只改 Qdrant payload 的 capability，不重嵌**。
//
// 用法：
//
//	go run ./cmd/kb-sync --kb ./kb                 # 增量入库
//	go run ./cmd/kb-sync --kb ./kb --dry-run       # 只看计划，不写库
//	go run ./cmd/kb-sync --reembed                 # 全量重嵌（换 embedding 模型后）
//	go run ./cmd/kb-sync --refresh-capability      # 台账状态翻转 → 仅更新 payload
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/Books-QAQ/tickets/internal/ai/kb"
	"github.com/Books-QAQ/tickets/internal/cache"
	"github.com/Books-QAQ/tickets/internal/util"
)

type options struct {
	kbPath      string
	dryRun      bool
	reembed     bool
	refreshCaps bool
	batchSize   int
	limit       int
}

func main() {
	var opt options
	flag.StringVar(&opt.kbPath, "kb", "./kb", "知识库目录")
	flag.BoolVar(&opt.dryRun, "dry-run", false, "只打印计划，不写库")
	flag.BoolVar(&opt.reembed, "reembed", false, "全量重嵌（换 embedding 模型时用）")
	flag.BoolVar(&opt.refreshCaps, "refresh-capability", false, "按能力台账刷新 Qdrant payload 的 capability（不重嵌）")
	flag.IntVar(&opt.batchSize, "batch", 32, "embedding 批大小")
	flag.IntVar(&opt.limit, "limit", 0, "只处理前 N 个文档（调试用）")
	flag.Parse()

	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: "15:04:05"})

	cfg, err := util.LoadConfig(".")
	if err != nil {
		log.Fatal().Err(err).Msg("cannot load config")
	}
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?parseTime=true&multiStatements=true&loc=Asia%%2FShanghai",
		cfg.DBUSERNAME, cfg.DBPASSWORD, cfg.DBHOST, cfg.DBPORT, cfg.DBDATABASE)
	dbConn, err := sql.Open("mysql", dsn)
	if err != nil {
		log.Fatal().Err(err).Msg("cannot connect db")
	}
	defer dbConn.Close()
	if err := dbConn.Ping(); err != nil {
		log.Fatal().Err(err).Msg("cannot ping db")
	}

	kbs := kb.NewStore(dbConn)
	ctx := context.Background()

	if opt.refreshCaps {
		if err := refreshCapabilities(ctx, kbs, cfg, opt); err != nil {
			log.Fatal().Err(err).Msg("refresh capability failed")
		}
		return
	}

	docs, caps, err := kb.LoadDir(opt.kbPath)
	if err != nil {
		log.Fatal().Err(err).Msg("load kb failed（校验不过就不入库，宁可失败不可脏数据）")
	}
	if opt.limit > 0 && len(docs) > opt.limit {
		docs = docs[:opt.limit]
	}
	log.Info().Int("docs", len(docs)).Int("capabilities", len(caps)).Msg("kb loaded")

	// 台账入库
	if !opt.dryRun {
		for _, row := range caps {
			if err := kbs.UpsertCapability(ctx, row); err != nil {
				log.Fatal().Err(err).Str("sub_scenario", row.SubScenario).Msg("upsert capability failed")
			}
		}
	}

	emb := buildEmbedder(cfg)
	qd := buildQdrant(cfg)
	if qd != nil && !opt.dryRun {
		if err := qd.EnsureCollection(ctx, emb.Dim()); err != nil {
			log.Fatal().Err(err).Msg("ensure qdrant collection failed")
		}
		if err := qd.CreatePayloadIndexes(ctx); err != nil {
			log.Error().Err(err).Msg("create payload indexes failed")
		}
	}
	log.Info().Str("embedder", emb.Name()).Bool("semantic", emb.Semantic()).Bool("qdrant", qd != nil).Msg("providers ready")

	hashes, ids, err := kbs.ChunkHashIndex(ctx)
	if err != nil {
		log.Fatal().Err(err).Msg("load chunk hash index failed")
	}

	var stat struct {
		docs, chunks, skipped, embedded, deletedDocs, prunedChunks int
	}
	seenDocs := map[string]bool{}
	for _, doc := range docs {
		seenDocs[doc.DocKey] = true
		chunks, err := kb.ChunkDocument(doc)
		if err != nil {
			log.Fatal().Err(err).Str("doc", doc.DocKey).Msg("chunk failed")
		}
		if opt.dryRun {
			log.Info().Str("doc", doc.DocKey).Int("chunks", len(chunks)).Str("form", string(doc.Form)).Msg("[dry-run]")
			stat.docs++
			stat.chunks += len(chunks)
			continue
		}

		docID, err := kbs.UpsertDocument(ctx, doc)
		if err != nil {
			log.Fatal().Err(err).Str("doc", doc.DocKey).Msg("upsert doc failed")
		}
		stat.docs++

		type pending struct {
			id      int64
			chunk   kb.Chunk
		}
		var toEmbed []pending
		for _, ch := range chunks {
			key := fmt.Sprintf("%s#%d", doc.DocKey, ch.Seq)
			chunkID, err := kbs.UpsertChunk(ctx, docID, ch)
			if err != nil {
				log.Fatal().Err(err).Str("label", ch.SourceLabel).Msg("upsert chunk failed")
			}
			stat.chunks++
			unchanged := hashes[key] == ch.ContentHash && !opt.reembed
			if unchanged && ids[key] == chunkID {
				stat.skipped++
				continue
			}
			ch.EmbeddingModel = emb.Name()
			ch.ID = chunkID // payload 里的 chunk_id 必须是库里的真实 ID
			toEmbed = append(toEmbed, pending{id: chunkID, chunk: ch})
		}
		prunedIDs := pruneExtraIDs(ctx, kbs, docID, len(chunks))
		if len(prunedIDs) > 0 && qd != nil {
			// 文档变短后必须**同时**删 Qdrant 点，否则派生索引漂移（MySQL 查不到、向量库还留着）
			if err := qd.DeleteByChunkIDs(ctx, prunedIDs); err != nil {
				log.Error().Err(err).Msg("qdrant 删除被裁剪的 chunk 失败：索引已漂移，需重建")
			}
		}
		stat.prunedChunks += len(prunedIDs)

		// 分批嵌入 + 批量 upsert（断点续跑：已嵌入的下一轮会因 hash 命中而跳过）
		for i := 0; i < len(toEmbed); i += opt.batchSize {
			end := i + opt.batchSize
			if end > len(toEmbed) {
				end = len(toEmbed)
			}
			batch := toEmbed[i:end]
			texts := make([]string, 0, len(batch))
			for _, p := range batch {
				texts = append(texts, p.chunk.Content)
			}
			vecs, err := emb.Embed(ctx, texts)
			if err != nil {
				// 中断点：已完成的批次已落库，重跑会跳过（这就是断点续跑）
				log.Fatal().Err(err).Int("done", stat.embedded).Msg("embed 失败，已完成的批次已落库，直接重跑即可续传")
			}
			if qd != nil {
				points := make([]kb.QPoint, 0, len(batch))
				for j, p := range batch {
					points = append(points, kb.QPoint{
						ChunkID: p.id,
						Vector:  vecs[j],
						Payload: payloadOf(p.chunk),
					})
				}
				if err := qd.Upsert(ctx, points); err != nil {
					log.Fatal().Err(err).Int("done", stat.embedded).Msg("qdrant upsert 失败，重跑可续传")
				}
			}
			for _, p := range batch {
				if err := kbs.SetChunkEmbeddingModel(ctx, p.id, emb.Name()); err != nil {
					log.Error().Err(err).Msg("record embedding model failed")
				}
			}
			stat.embedded += len(batch)
			log.Info().Int("embedded", stat.embedded).Int("of", len(toEmbed)).Str("doc", doc.DocKey).Msg("progress")
		}
	}

	// 源文件已删除 → 清库 + 清 Qdrant
	if !opt.dryRun {
		all := allDocKeys(ctx, kbs)
		for _, key := range all {
			if seenDocs[key] {
				continue
			}
			chunkIDs := chunksOfDoc(ctx, kbs, key)
			if qd != nil && len(chunkIDs) > 0 {
				if err := qd.DeleteByChunkIDs(ctx, chunkIDs); err != nil {
					log.Error().Err(err).Str("doc", key).Msg("qdrant delete failed")
				}
			}
			if err := kbs.DeleteDocByKey(ctx, key); err != nil {
				log.Error().Err(err).Str("doc", key).Msg("delete doc failed")
			}
			stat.deletedDocs++
		}
	}

	// 知识库变了 ⇒ **答案缓存必须失效**（M3 滞留项）。
	// 缓存里存的是"依据旧知识库生成的答案"，知识库更新后它们会变成**过期却笃定的答案**
	// ——比答不上来更糟。这里在入库成功后清掉 cs:ac:*（M4）。
	if !opt.dryRun {
		if flushed, err := flushAnswerCache(ctx, cfg); err != nil {
			log.Warn().Err(err).Msg("答案缓存失效失败（缓存会按 TTL 自然过期，但请检查 Redis 配置）")
		} else if flushed >= 0 {
			log.Info().Int("flushed", flushed).Msg("已清空答案缓存 cs:ac:*（知识库变更 → 旧答案不得复用）")
		}
	}

	log.Info().
		Int("docs", stat.docs).Int("chunks", stat.chunks).Int("skipped", stat.skipped).
		Int("embedded", stat.embedded).Int("pruned_chunks", stat.prunedChunks).Int("deleted_docs", stat.deletedDocs).
		Bool("dry_run", opt.dryRun).
		Msg("kb-sync 完成")
}

// flushAnswerCache 清空答案级缓存的全部键（`cs:ac:*`）。返回清掉的键数；Redis 不可用返回 -1 + err。
func flushAnswerCache(ctx context.Context, cfg util.Config) (int, error) {
	rdb, err := cache.NewRedisClient(cfg)
	if err != nil {
		return -1, err
	}
	defer rdb.Close()
	// 用 EVAL 一次清完：避免 SCAN 分页期间有新键写入而漏清
	const lua = `local ks = redis.call('keys', 'cs:ac:*')
	              for i = 1, #ks do redis.call('del', ks[i]) end
	              return #ks`
	n, err := rdb.Eval(ctx, lua, nil).Int()
	if err != nil {
		return -1, err
	}
	return n, nil
}

func payloadOf(c kb.Chunk) map[string]any {
	expireTS := int64(253402300799) // 9999-12-31：null 约定为极大值，便于 range 过滤
	if c.ExpireAt != nil {
		expireTS = c.ExpireAt.Unix()
	}
	p := map[string]any{
		"chunk_id":     c.ID,
		"content_hash": c.ContentHash,
		"embedding_model": c.EmbeddingModel,
		"category":     c.Category,
		"form":         string(c.Form),
		"capability":   string(c.Capability),
		"visibility":   c.Visibility,
		"scope_kind":   c.ScopeKind,
		"source_label": c.SourceLabel,
		"expire_at_ts": expireTS,
	}
	if c.SubScenario != "" {
		p["sub_scenario"] = c.SubScenario
	}
	if c.ScopeRef != "" {
		p["scope_ref"] = c.ScopeRef
	}
	return p
}

func buildEmbedder(cfg util.Config) kb.Embedder {
	dim := cfg.EmbeddingDim
	if dim <= 0 {
		dim = 1024
	}
	if strings.EqualFold(cfg.EmbeddingProvider, "openai") && cfg.EmbeddingAPIKey != "" {
		return &kb.OpenAIEmbedder{BaseURL: strings.TrimSuffix(cfg.EmbeddingBaseURL, "/"),
			APIKey: cfg.EmbeddingAPIKey, Model: cfg.EmbeddingModel, DimN: dim}
	}
	log.Warn().Msg("未配置真实 embedding，使用词面替身（仅用于打通链路，不代表检索质量）")
	return kb.FakeEmbedder{DimN: dim}
}

func buildQdrant(cfg util.Config) *kb.Qdrant {
	if cfg.QdrantURL == "" {
		return nil
	}
	coll := cfg.QdrantCollection
	if coll == "" {
		coll = "kb_current"
	}
	return &kb.Qdrant{BaseURL: strings.TrimSuffix(cfg.QdrantURL, "/"), APIKey: cfg.QdrantAPIKey, Collection: coll}
}

// pruneExtraIDs 清掉文档变短后多余的块，返回被删的 chunk id（调用方负责同步删 Qdrant 点）
func pruneExtraIDs(ctx context.Context, s *kb.Store, docID int64, keep int) []int64 {
	rows, err := s.DB.QueryContext(ctx, `SELECT id FROM kb_chunks WHERE doc_id=? AND seq>?`, docID, keep)
	if err != nil {
		return nil
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	if len(ids) == 0 {
		return nil
	}
	if err := s.DeleteExtraChunks(ctx, docID, keep); err != nil {
		log.Error().Err(err).Msg("prune extra chunks failed")
		return nil
	}
	return ids
}

func allDocKeys(ctx context.Context, s *kb.Store) []string {
	rows, err := s.DB.QueryContext(ctx, `SELECT doc_key FROM kb_documents`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err == nil {
			out = append(out, k)
		}
	}
	return out
}

func chunksOfDoc(ctx context.Context, s *kb.Store, docKey string) []int64 {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT c.id FROM kb_chunks c JOIN kb_documents d ON d.id=c.doc_id WHERE d.doc_key=?`, docKey)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err == nil {
			out = append(out, id)
		}
	}
	return out
}

// refreshCapabilities 台账状态翻转 → 只更新 payload（不重嵌、不动向量 —— §13.1 的落地依据）
func refreshCapabilities(ctx context.Context, s *kb.Store, cfg util.Config, opt options) error {
	caps, err := kb.ParseCapabilityYAML(mustRead(opt.kbPath + "/capability.yaml"))
	if err != nil {
		return err
	}
	qd := buildQdrant(cfg)
	if qd == nil {
		return fmt.Errorf("未配置 QDRANT_URL，无法刷新 payload")
	}
	changed := 0
	for _, row := range caps {
		if err := s.UpsertCapability(ctx, row); err != nil {
			return err
		}
		ids, err := s.ChunkIDsBySubScenario(ctx, row.SubScenario)
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			continue
		}
		if err := qd.SetPayloadCapability(ctx, ids, string(row.Status)); err != nil {
			return err
		}
		// MySQL 侧同步（真相源）
		for _, id := range ids {
			if _, err := s.DB.ExecContext(ctx, `UPDATE kb_chunks SET capability=? WHERE id=?`, string(row.Status), id); err != nil {
				return err
			}
		}
		changed += len(ids)
		log.Info().Str("sub_scenario", row.SubScenario).Str("status", string(row.Status)).Int("chunks", len(ids)).Msg("capability refreshed")
	}
	log.Info().Int("chunks", changed).Msg("refresh-capability 完成（未重嵌任何 chunk）")
	return nil
}

func mustRead(path string) []byte {
	raw, err := os.ReadFile(path)
	if err != nil {
		log.Fatal().Err(err).Str("path", path).Msg("read failed")
	}
	return raw
}

var _ = url.QueryEscape
var _ = time.Now
