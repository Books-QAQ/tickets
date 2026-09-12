package kb

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Aux 轻量计数点（M1 立骨架；完整指标三表在 M3）
type Aux struct {
	mu       sync.Mutex
	counters map[string]int64
}

func NewAux() *Aux { return &Aux{counters: map[string]int64{}} }

func (a *Aux) Inc(key string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.counters[key]++
	a.mu.Unlock()
}

func (a *Aux) Snapshot() map[string]int64 {
	if a == nil {
		return map[string]int64{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[string]int64, len(a.counters))
	for k, v := range a.counters {
		out[k] = v
	}
	return out
}

// RetrieveConfig 检索配置（阈值按形态分派 —— §7.5；初始值待 M1 实测标定，见滞留清单）
type RetrieveConfig struct {
	RecallK          int     // 每路召回条数
	TopK             int     // 最终进生成的条数
	RerankTopN       int     // 精排窗口
	ThresholdQA      float64 // qa 形态的 top1 余弦阈值
	ThresholdProse   float64 // prose 形态（长块余弦天然偏低，必须单独标定）
	LiteralThreshold float64 // 字面替身模式（无 embedding）下的 BM25 分数门槛
	SoftRouteMax     int     // 软路由最多融合几个分类
}

func DefaultRetrieveConfig() RetrieveConfig {
	return RetrieveConfig{
		RecallK:          50,
		TopK:             5,
		RerankTopN:       50,
		ThresholdQA:      0.60,
		ThresholdProse:   0.50,
		LiteralThreshold: 3.0,
		SoftRouteMax:     3,
	}
}

// RetrieveResult 检索结果
type RetrieveResult struct {
	Chunks       []Chunk
	SourceLabels []string
	Top1Cos      float64
	VectorMode   string // embedding | ngram
	Empty        bool
	Level        int // 0=category+supported+scope 1=放宽 scope 2=放宽 capability（行业参考）
	AboveThreshold bool // 阈值判定结果（形态分派后）；编排层据此走 E13/E16，不重复实现阈值逻辑
	RerankUsed   bool
	Form         Form // 命中块的形态（决定用哪个阈值二次判定）
	Degraded     map[string]bool
	StageMS      map[string]int64
}

// Retriever 混合检索编排：分类软路由 → (BM25 ∥ 向量) → 两级放宽 → RRF → rerank → 取文本
type Retriever struct {
	Store  *Store
	Index  func() *Index
	Vec    VectorRetriever
	Emb    Embedder
	Rerank Reranker
	Cfg    RetrieveConfig
	Aux    *Aux
	Logf   func(format string, args ...any)
}

func (r *Retriever) logf(format string, args ...any) {
	if r.Logf != nil {
		r.Logf(format, args...)
	}
}

// Retrieve 主入口。clsScores 走软路由（**禁止丢弃** —— §7.4）；visibility 由 Go 侧按身份决定。
func (r *Retriever) Retrieve(ctx context.Context, question, category string, clsScores map[string]int,
	visibility []string, scopeKind, scopeRef string) (*RetrieveResult, error) {

	start := time.Now()
	res := &RetrieveResult{VectorMode: "embedding", Degraded: map[string]bool{}, StageMS: map[string]int64{}}

	categories := softRouteCategories(category, clsScores, r.Cfg.SoftRouteMax)

	idx := (*Index)(nil)
	if r.Index != nil {
		idx = r.Index()
	}
	if idx == nil || idx.Size() == 0 {
		r.Aux.Inc("retrieve_index_empty_total")
	}

	// 向量路：无 embedding 或 无向量库 → 字面替身（诚实降级，不静默）
	useVector := r.Emb != nil && r.Vec != nil
	var queryVec []float32
	if useVector {
		embStart := time.Now()
		vecs, err := r.Emb.Embed(ctx, []string{question})
		res.StageMS["embed"] = time.Since(embStart).Milliseconds()
		if err != nil || len(vecs) == 0 {
			r.logf("embed 失败，降级为字面替身: %v", err)
			r.Aux.Inc("vector_fallback_total")
			useVector = false
		} else {
			queryVec = vecs[0]
			if r.Emb.Dim() > 0 && len(queryVec) != r.Emb.Dim() {
				return nil, fmt.Errorf("embedding 维度不一致: got %d want %d", len(queryVec), r.Emb.Dim())
			}
		}
	}
	if !useVector {
		res.VectorMode = "ngram"
		res.Degraded["vector"] = true
	} else if r.Emb != nil && !r.Emb.Semantic() {
		// 词面替身 embedding：向量路语义不成立 → 诚实标注（不静默降级），
		// 阈值判定会走字面门槛（见下方 LiteralThreshold）
		res.VectorMode = "ngram"
		res.Degraded["vector"] = true
	}

	// 两级放宽：0 原过滤 → 1 放宽 scope → 2 放宽 capability（行业参考）
	for level := 0; level <= 2; level++ {
		filter := Filter{
			Category:     "",
			Capabilities: map[Capability]bool{CapSupported: true},
			Visibility:   visibility,
		}
		if level < 2 {
			filter.ScopeKind = scopeKind
			filter.ScopeRef = scopeRef
		}
		if level >= 2 {
			filter.Capabilities = nil // 允许 roadmap / industry 进入（生成层会加边界声明）
		}

		legs := [][]Hit{}
		cosByID := map[int64]float64{}
		var bm25MS, vecMS int64
		bm25Top := 0.0 // BM25 原始分最高值：字面替身模式的阈值判定用它（RRF 分数不能比门槛）

		for _, cat := range categories {
			f := filter
			f.Category = cat

			if idx != nil && idx.Size() > 0 {
				t0 := time.Now()
				hits := idx.Search(question, r.Cfg.RecallK, f.Allow)
				bm25MS += time.Since(t0).Milliseconds()
				if len(hits) > 0 {
					if hits[0].Score > bm25Top {
						bm25Top = hits[0].Score
					}
					legs = append(legs, hits)
				}
			}
			if useVector {
				t0 := time.Now()
				// 向量侧不做 category 反转：分类为空时不过滤分类（软路由多类）
				vf := f
				vHits, err := r.Vec.Search(ctx, queryVec, vf, r.Cfg.RecallK)
				vecMS += time.Since(t0).Milliseconds()
				if err != nil {
					r.logf("向量检索失败（降级为字面替身）: %v", err)
					r.Aux.Inc("vector_fallback_total")
					useVector = false
					res.VectorMode = "ngram"
					res.Degraded["vector"] = true
				} else {
					for _, vh := range vHits {
						if vh.Cos > cosByID[vh.ChunkID] {
							cosByID[vh.ChunkID] = vh.Cos
						}
					}
					if len(vHits) > 0 {
						legs = append(legs, toHits(vHits))
					}
				}
			}
		}
		res.StageMS["bm25"] += bm25MS
		res.StageMS["vector"] += vecMS

		if len(legs) == 0 {
			if level < 2 {
				r.Aux.Inc("filter_relaxed_total")
				res.Level = level + 1
				continue
			}
			res.Empty = true
			res.StageMS["total"] = time.Since(start).Milliseconds()
			return res, nil
		}

		fused := Fuse(legs...)
		if len(fused) == 0 {
			if level < 2 {
				r.Aux.Inc("filter_relaxed_total")
				res.Level = level + 1
				continue
			}
			res.Empty = true
			res.StageMS["total"] = time.Since(start).Milliseconds()
			return res, nil
		}

		// top1 余弦（阈值判定用余弦，不是 RRF 分数）
		top1Cos := 0.0
		if c, ok := cosByID[fused[0].ChunkID]; ok {
			top1Cos = c
		} else {
			for _, h := range fused {
				if c, ok := cosByID[h.ChunkID]; ok && c > top1Cos {
					top1Cos = c
				}
			}
		}

		// 精排（窗口内）：不可用则回退融合分并计数
		ranked := fused
		if r.Rerank != nil && len(fused) > 1 {
			window := fused
			if r.Cfg.RerankTopN > 0 && len(window) > r.Cfg.RerankTopN {
				window = window[:r.Cfg.RerankTopN]
			}
			if _, isFallback := r.Rerank.(FusionReranker); !isFallback {
				t0 := time.Now()
				cands, err := r.candidates(ctx, window)
				if err == nil {
					reranked, rerr := r.Rerank.Rerank(ctx, question, cands, r.Cfg.TopK)
					if rerr != nil {
						r.logf("rerank 失败，回退融合分: %v", rerr)
						r.Aux.Inc("rerank_fallback_total")
					} else {
						ranked = make([]Hit, 0, len(reranked))
						for i, c := range reranked {
							ranked = append(ranked, Hit{ChunkID: c.ChunkID, Score: float64(len(reranked) - i)})
						}
						res.RerankUsed = true
					}
				}
				res.StageMS["rerank"] = time.Since(t0).Milliseconds()
			}
		}

		topK := r.Cfg.TopK
		if topK <= 0 || topK > len(ranked) {
			topK = len(ranked)
		}
		ids := make([]int64, 0, topK)
		for _, h := range ranked[:topK] {
			ids = append(ids, h.ChunkID)
		}

		fetchStart := time.Now()
		chunks, err := r.Store.ChunksByIDs(ctx, ids)
		if err != nil {
			return nil, err
		}
		// 时效在取文本后过滤（Qdrant 侧已按 expire_at_ts 过滤，这里双保险）
		live := make([]Chunk, 0, len(chunks))
		labels := make([]string, 0, len(chunks))
		for _, c := range chunks {
			if c.ExpireAt != nil && c.ExpireAt.Before(time.Now()) {
				r.Aux.Inc("expired_chunk_filtered_total")
				continue
			}
			live = append(live, c)
			labels = append(labels, c.SourceLabel)
		}
		res.StageMS["fetch"] = time.Since(fetchStart).Milliseconds()

		if len(live) == 0 {
			if level < 2 {
				r.Aux.Inc("filter_relaxed_total")
				res.Level = level + 1
				continue
			}
			res.Empty = true
			res.StageMS["total"] = time.Since(start).Milliseconds()
			return res, nil
		}

		// 阈值判定：形态分派（命中块多为 prose 时用 prose 阈值）
		form := live[0].Form
		threshold := r.Cfg.ThresholdQA
		if form == FormProse {
			threshold = r.Cfg.ThresholdProse
		}
		effectiveCos := top1Cos
		if res.VectorMode == "ngram" {
			// 字面替身模式：余弦不存在，用 **BM25 原始分**门槛代替；
			// 且不允许把"有结果即相关"写死为 true（§7.5）。
			// 注意：不能用 RRF 融合分去比门槛 —— RRF 分在 0.01~0.05 量级，会永远不达标（实测踩过）。
			if bm25Top >= r.Cfg.LiteralThreshold {
				effectiveCos = 1
			} else {
				effectiveCos = 0
			}
		}
		res.Chunks = live
		res.SourceLabels = labels
		res.Top1Cos = effectiveCos
		res.Form = form
		res.Level = level
		res.AboveThreshold = effectiveCos >= threshold
		res.StageMS["total"] = time.Since(start).Milliseconds()
		if !res.AboveThreshold {
			r.Aux.Inc("below_threshold_total")
		}
		return res, nil
	}
	return res, nil
}

func (r *Retriever) candidates(ctx context.Context, window []Hit) ([]RerankCandidate, error) {
	ids := make([]int64, 0, len(window))
	for _, h := range window {
		ids = append(ids, h.ChunkID)
	}
	chunks, err := r.Store.ChunksByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	byID := map[int64]Chunk{}
	for _, c := range chunks {
		byID[c.ID] = c
	}
	out := make([]RerankCandidate, 0, len(window))
	for _, h := range window {
		c, ok := byID[h.ChunkID]
		if !ok {
			continue
		}
		out = append(out, RerankCandidate{ChunkID: h.ChunkID, Text: c.Content, FusionScore: h.Score})
	}
	return out, nil
}

func toHits(vh []VectorHit) []Hit {
	out := make([]Hit, 0, len(vh))
	for _, v := range vh {
		out = append(out, Hit{ChunkID: v.ChunkID, Score: v.Cos})
	}
	return out
}

// softRouteCategories 软路由：分类得分 > 0 的类别一起检索（分数必须从分类层一路带下来）
func softRouteCategories(primary string, scores map[string]int, max int) []string {
	type kv struct {
		cat string
		sc  int
	}
	var list []kv
	for cat, sc := range scores {
		if sc > 0 && ValidCategory(cat) && cat != "other" {
			list = append(list, kv{cat, sc})
		}
	}
	// 稳定排序：分数高优先，同分按分类名
	for i := 0; i < len(list); i++ {
		for j := i + 1; j < len(list); j++ {
			if list[j].sc > list[i].sc || (list[j].sc == list[i].sc && list[j].cat < list[i].cat) {
				list[i], list[j] = list[j], list[i]
			}
		}
	}
	out := make([]string, 0, max)
	seen := map[string]bool{}
	if primary != "" && primary != "other" && ValidCategory(primary) {
		out = append(out, primary)
		seen[primary] = true
	}
	for _, item := range list {
		if len(out) >= max {
			break
		}
		if seen[item.cat] {
			continue
		}
		out = append(out, item.cat)
		seen[item.cat] = true
	}
	if len(out) == 0 {
		out = append(out, "") // 不过滤分类（兜底：全库检索）
	}
	return out
}
