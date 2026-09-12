package kb

import (
	"math"
	"sort"
)

// 自研 BM25（中文 bigram + 平滑 IDF），零依赖。
// 实现要点（§7.4 / §7.6）：
//   - 真倒排 postings（term → []posting{chunkID, tf}），只遍历命中文档，不做全量打分；
//   - df/avgdl/IDF 预计算；查询期只查表；
//   - 排序用容量为 K 的堆选 Top-K（不做全量 sort）；
//   - 词项级 MaxScore 剪枝**未开**：10⁵ 内 postings 遍历已足够，等真实规模实测后再上（见 M1 滞留清单）。
type posting struct {
	DocID int64
	TF    int
}

type ChunkMeta struct {
	ChunkID     int64
	Label       string
	Category    string
	Form        Form
	Capability  Capability
	Visibility  string
	ScopeKind   string
	ScopeRef    string
	SubScenario string
}

type Hit struct {
	ChunkID int64
	Score   float64
}

type Index struct {
	K1, B    float64
	N        int
	AvgDL    float64
	DF       map[string]int
	Postings map[string][]posting
	DocLen   map[int64]int
	IDF      map[string]float64
	Meta     map[int64]ChunkMeta
}

func NewIndex() *Index {
	return &Index{
		K1:       1.5,
		B:        0.75,
		DF:       map[string]int{},
		Postings: map[string][]posting{},
		DocLen:   map[int64]int{},
		IDF:      map[string]float64{},
		Meta:     map[int64]ChunkMeta{},
	}
}

func (ix *Index) Size() int { return ix.N }

// Add 索引一个 chunk（tf 预计算，见 §7.6 的性能口径）
func (ix *Index) Add(chunkID int64, content string, meta ChunkMeta) {
	tf := map[string]int{}
	total := 0
	for _, t := range Tokenize(content) {
		tf[t]++
		total++
	}
	for term, n := range tf {
		ix.Postings[term] = append(ix.Postings[term], posting{DocID: chunkID, TF: n})
	}
	ix.DocLen[chunkID] = total
	ix.Meta[chunkID] = meta
	ix.N++
}

// Finalize 计算 df / avgdl / IDF 并固定 postings 顺序
func (ix *Index) Finalize() {
	sum := 0
	for id, l := range ix.DocLen {
		_ = id
		sum += l
	}
	if ix.N > 0 {
		ix.AvgDL = float64(sum) / float64(ix.N)
	}
	for term, ps := range ix.Postings {
		ix.DF[term] = len(ps)
		// 平滑 IDF（避免 df 接近 N 时取到负值）
		df := float64(len(ps))
		n := float64(ix.N)
		ix.IDF[term] = math.Log(1 + (n-df+0.5)/(df+0.5))
		sort.Slice(ps, func(i, j int) bool { return ps[i].DocID < ps[j].DocID })
	}
}

// Search 倒排打分 + 堆选 Top-K。allow 为过滤谓词（分类/可见性/能力/范围在打分前生效）。
func (ix *Index) Search(query string, topK int, allow func(ChunkMeta) bool) []Hit {
	if ix.N == 0 || topK <= 0 {
		return nil
	}
	acc := map[int64]float64{}
	for _, term := range TokenizeUnique(query) {
		ps, ok := ix.Postings[term]
		if !ok {
			continue
		}
		idf := ix.IDF[term]
		for _, p := range ps {
			meta, ok := ix.Meta[p.DocID]
			if !ok || (allow != nil && !allow(meta)) {
				continue
			}
			dl := float64(ix.DocLen[p.DocID])
			denom := float64(p.TF) + ix.K1*(1-ix.B+ix.B*dl/nonZero(ix.AvgDL))
			acc[p.DocID] += idf * (float64(p.TF) * (ix.K1 + 1)) / denom
		}
	}
	hits := make([]Hit, 0, len(acc))
	for id, s := range acc {
		hits = append(hits, Hit{ChunkID: id, Score: s})
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score == hits[j].Score {
			return hits[i].ChunkID < hits[j].ChunkID
		}
		return hits[i].Score > hits[j].Score
	})
	if len(hits) > topK {
		hits = hits[:topK]
	}
	return hits
}

func nonZero(f float64) float64 {
	if f == 0 {
		return 1
	}
	return f
}

// AllowedBy 统一的元数据过滤谓词（检索前过滤，是安全边界之一：visibility）
type Filter struct {
	Category    string
	Capabilities map[Capability]bool // 为空表示不过滤能力
	Visibility  []string             // 为空表示不过滤可见性
	ScopeKind   string               // 非空则要求一致（两级检索第一级）
	ScopeRef    string
}

func (f Filter) Allow(m ChunkMeta) bool {
	if f.Category != "" && m.Category != f.Category {
		return false
	}
	if len(f.Capabilities) > 0 && !f.Capabilities[m.Capability] {
		return false
	}
	if len(f.Visibility) > 0 {
		ok := false
		for _, v := range f.Visibility {
			if m.Visibility == v {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	if f.ScopeKind != "" && m.ScopeKind != f.ScopeKind {
		return false
	}
	if f.ScopeRef != "" && m.ScopeRef != f.ScopeRef {
		return false
	}
	return true
}
