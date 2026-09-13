// Package answercache 实现答案级语义缓存（§9.3）。
//
// 设计要点（每条都有理由，改之前先看理由）：
//  1. **键 = 消解后的规范问题 + 分类**（不是原始输入、不是拼装后的 prompt）——
//     消解掉指代/上下文后与用户无关，跨用户/会话共享才安全，命中率也才高。
//  2. **匹配 = 问题向量余弦 ≥ 0.95 且同分类内**（远高于检索相关性阈值 0.60）：
//     只有"同一个问题"才命中；同分类内匹配防跨类误命中。
//  3. **准入 = 同一问题第 2 次出现才正式入缓存**：长尾问题只问一次，缓存了也白占内存。
//  4. **淘汰 = TTL 滑动 + 分类内 LRU 上限**，LRU 用**单调递增 seq** 排序而**不用时间戳**：
//     同一时钟 tick 的多个条目时间戳相同，叠加 map 遍历随机会让淘汰不确定（单测偶发挂）。
//  5. **个性化/工具直答/转人工/问候一律不入缓存**（由编排层判定并传 `personalized`，
//     本包再兜一道，防调用方漏判）。
package answercache

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

// Counter 计数点（复用 Go 侧统一计数来源）
type Counter interface{ Inc(key string) }

// Store 缓存存储抽象（生产实现是 Redis；单测用内存实现，算法本身与存储无关）
type Store interface {
	Get(ctx context.Context, key string) (string, bool, error)
	Set(ctx context.Context, key, value string, ttl time.Duration) error
	Incr(ctx context.Context, key string, ttl time.Duration) (int64, error)
	// Del 淘汰时必须能删精确键，否则"已淘汰"的条目仍会被精确路径命中（实测踩过：
	// 条目被 LRU 挤出索引后，object 键还留着 → 缓存看起来没上限、Redis 只涨不降）
	Del(ctx context.Context, keys ...string) error
}

// Embedder 只需"把文本变成向量"这一件事（与 kb.Embedder 结构兼容）
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
	Semantic() bool
}

// SourceRef 出处（与对外响应同形）
type SourceRef struct {
	Label      string `json:"label"`
	Capability string `json:"capability,omitempty"`
	Form       string `json:"form,omitempty"`
}

// Entry 一条缓存条目。
// **向量随条目一起存**：匹配时只 embed 查询一次，不能对每个条目各调一次 Embed
// （真实 embedding 下那是 N 次 HTTP 往返；M3 实测初版就踩了，单测也暴露了"淘汰没删精确键"）。
type Entry struct {
	Question string      `json:"q"` // 消解后的规范问题
	Category string      `json:"c"`
	Answer   string      `json:"a"`
	Sources  []SourceRef `json:"s,omitempty"`
	Vec      []float32   `json:"v,omitempty"` // 条目向量（入缓存时算一次）
	Seq      uint64      `json:"seq"`         // 单调递增（LRU 依据）
	Hits     int         `json:"h"`
	StoredAt int64       `json:"ts"` // 仅用于观测（不参与淘汰决策）
}

// Config 参数
type Config struct {
	Similarity float64       // 命中阈值（§9.3：0.95）
	TTL        time.Duration // 滑动过期
	MaxPerCat  int           // 每个分类的条目上限（LRU）
	PendingTTL time.Duration // 候选池 TTL
	Enabled    bool
}

func DefaultConfig() Config {
	return Config{Similarity: 0.95, TTL: 24 * time.Hour, MaxPerCat: 200,
		PendingTTL: 6 * time.Hour, Enabled: true}
}

// Cache 答案缓存
type Cache struct {
	store Store
	emb   Embedder
	cfg   Config
	cnt   Counter

	mu     sync.Mutex
	seq    uint64 // 进程内单调序号（跨进程可能重复，仅影响同分类内的相对顺序）
	stats  Stats
}

// Stats 进程内观测（真正的指标走 Counter → Go 单一计数点）
type Stats struct {
	Lookups, Hits, Misses, Stores, Promoted, Evicted, RefusedPersonalized int64
}

// New 构造；store 为 nil 或 cfg.Enabled=false 时整体降级为"永不相中"（不报错、不阻塞）
func New(store Store, emb Embedder, cfg Config, cnt Counter) *Cache {
	return &Cache{store: store, emb: emb, cfg: cfg, cnt: cnt}
}

func (c *Cache) enabled() bool {
	return c != nil && c.cfg.Enabled && c.store != nil && c.emb != nil
}

func (c *Cache) inc(key string) {
	if c.cnt != nil {
		c.cnt.Inc(key)
	}
}

// Normalize 规范问题：去空白/去尾部语气词/统一全半角标点。**消解后的问题**才进来。
func Normalize(q string) string {
	s := strings.TrimSpace(q)
	s = strings.TrimSuffix(s, "？")
	s = strings.TrimSuffix(s, "?")
	replacer := strings.NewReplacer("，", ",", "。", ".", "！", "!", "？", "?", "：", ":", " ", "", "\t", "")
	return strings.ToLower(replacer.Replace(s))
}

// objectKey 精确定位键（同问题同分类）
func objectKey(normQ, category string) string {
	sum := sha1.Sum([]byte(normQ))
	return fmt.Sprintf("cs:ac:obj:%s:%s", hex.EncodeToString(sum[:]), category)
}

func indexKey(category string) string { return "cs:ac:idx:" + category }

func pendingKey(normQ, category string) string {
	sum := sha1.Sum([]byte(normQ))
	return fmt.Sprintf("cs:ac:pend:%s:%s", hex.EncodeToString(sum[:]), category)
}

// LookupResult 查询结果
type LookupResult struct {
	Hit        bool
	Answer     string
	Sources    []SourceRef
	Score      float64
	VectorMode string // embedding | ngram（诚实地告诉调用方走的是哪种匹配）
}

// Lookup 按语义查缓存。命中条件：同分类内存在余弦 ≥ 阈值的条目。
func (c *Cache) Lookup(ctx context.Context, question, category string) LookupResult {
	if !c.enabled() {
		return LookupResult{VectorMode: "disabled"}
	}
	c.inc("cache_lookup_total")
	c.mu.Lock()
	c.stats.Lookups++
	c.mu.Unlock()

	mode := "embedding"
	if !c.emb.Semantic() {
		mode = "ngram" // 词面替身：匹配退化为字面重合，必须标注
	}
	norm := Normalize(question)
	if norm == "" || category == "" {
		return LookupResult{VectorMode: mode}
	}

	// 1) 精确定位（同问题同分类）——查一次就够，省掉向量计算
	if raw, ok, err := c.store.Get(ctx, objectKey(norm, category)); err == nil && ok {
		var e Entry
		if json.Unmarshal([]byte(raw), &e) == nil && e.Answer != "" {
			c.hit()
			return LookupResult{Hit: true, Answer: e.Answer, Sources: e.Sources, Score: 1.0, VectorMode: mode}
		}
	}

	// 2) 语义匹配：分类内全量比对（有上限，非热点路径）
	raw, ok, err := c.store.Get(ctx, indexKey(category))
	if err != nil || !ok {
		c.miss()
		return LookupResult{VectorMode: mode}
	}
	var entries []Entry
	if json.Unmarshal([]byte(raw), &entries) != nil {
		c.miss()
		return LookupResult{VectorMode: mode}
	}
	qv, err := c.emb.Embed(ctx, norm) // **只 embed 查询一次**
	if err != nil {
		c.miss()
		return LookupResult{VectorMode: mode}
	}

	best := LookupResult{VectorMode: mode}
	for _, e := range entries {
		if len(e.Vec) == 0 || e.Answer == "" {
			continue
		}
		score := Cosine(qv, e.Vec)
		if score >= c.cfg.Similarity && score > best.Score {
			best = LookupResult{Hit: true, Answer: e.Answer, Sources: e.Sources, Score: score, VectorMode: mode}
		}
	}
	if best.Hit {
		c.hit()
		return best
	}
	c.miss()
	return best
}

// StoreResult 写缓存结果
type StoreResult struct {
	Stored   bool   `json:"stored"`
	Promoted bool   `json:"promoted"` // 第 2 次出现被正式收录
	Evicted  int    `json:"evicted"`
	Reason   string `json:"reason,omitempty"`
}

// Store 写入。**个性化内容直接拒绝**（ADR：答案缓存不带用户态）。
func (c *Cache) Store(ctx context.Context, question, category, answer string, sources []SourceRef,
	personalized bool) StoreResult {
	if !c.enabled() {
		return StoreResult{Reason: "disabled"}
	}
	if personalized {
		c.inc("cache_refused_personalized_total")
		c.mu.Lock()
		c.stats.RefusedPersonalized++
		c.mu.Unlock()
		return StoreResult{Reason: "personalized"}
	}
	norm := Normalize(question)
	if norm == "" || category == "" || strings.TrimSpace(answer) == "" {
		return StoreResult{Reason: "invalid"}
	}

	// 准入：第 2 次命中同一问题才正式收录（第 1 次只记候选）
	n, err := c.store.Incr(ctx, pendingKey(norm, category), c.cfg.PendingTTL)
	if err != nil {
		return StoreResult{Reason: "store_error"}
	}
	if n < 2 {
		return StoreResult{Reason: "pending_first_seen"}
	}

	c.mu.Lock()
	c.seq++
	seq := c.seq
	c.stats.Stores++
	c.mu.Unlock()

	vec, err := c.emb.Embed(ctx, norm) // 条目向量只算一次，随条目存
	if err != nil {
		return StoreResult{Reason: "embed_error"}
	}
	entry := Entry{Question: question, Category: category, Answer: answer, Sources: sources,
		Vec: vec, Seq: seq, StoredAt: time.Now().Unix()}

	// 读改写分类索引（条目上限 = MaxPerCat，LRU 用 seq 淘汰）
	var entries []Entry
	if raw, ok, err := c.store.Get(ctx, indexKey(category)); err == nil && ok {
		_ = json.Unmarshal([]byte(raw), &entries)
	}
	evicted := 0
	replaced := false
	for i := range entries {
		if Normalize(entries[i].Question) == norm {
			entries[i] = entry // 同问题更新（滑动 TTL）
			replaced = true
			break
		}
	}
	if !replaced {
		entries = append(entries, entry)
	}
	var evictedKeys []string
	if c.cfg.MaxPerCat > 0 && len(entries) > c.cfg.MaxPerCat {
		// seq 小的先淘汰（单调递增避免时间戳同 tick 的不确定性）
		sort.Slice(entries, func(i, j int) bool { return entries[i].Seq < entries[j].Seq })
		evicted = len(entries) - c.cfg.MaxPerCat
		for _, e := range entries[:evicted] {
			evictedKeys = append(evictedKeys, objectKey(Normalize(e.Question), category))
		}
		entries = entries[evicted:]
		c.inc("cache_evicted_total")
	}

	blob, err := json.Marshal(entries)
	if err != nil {
		return StoreResult{Reason: "marshal_error"}
	}
	if err := c.store.Set(ctx, indexKey(category), string(blob), c.cfg.TTL); err != nil {
		return StoreResult{Reason: "store_error"}
	}
	one, _ := json.Marshal(entry)
	_ = c.store.Set(ctx, objectKey(norm, category), string(one), c.cfg.TTL)
	if len(evictedKeys) > 0 {
		// 精确键必须一起删，否则被淘汰的条目还能从精确路径命中
		_ = c.store.Del(ctx, evictedKeys...)
	}

	c.inc("cache_store_total")
	if !replaced {
		c.inc("cache_promoted_total")
	}
	c.mu.Lock()
	c.stats.Evicted += int64(evicted)
	c.mu.Unlock()
	return StoreResult{Stored: true, Promoted: !replaced, Evicted: evicted}
}

func (c *Cache) hit() {
	c.inc("cache_hit_total")
	c.mu.Lock()
	c.stats.Hits++
	c.mu.Unlock()
}

func (c *Cache) miss() {
	c.inc("cache_miss_total")
	c.mu.Lock()
	c.stats.Misses++
	c.mu.Unlock()
}

// Snapshot 进程内统计（排障用；对外指标走 Counter）
func (c *Cache) Snapshot() map[string]int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return map[string]int64{
		"lookup": c.stats.Lookups, "hit": c.stats.Hits, "miss": c.stats.Misses,
		"store": c.stats.Stores, "promoted": c.stats.Promoted, "evicted": c.stats.Evicted,
		"refused_personalized": c.stats.RefusedPersonalized,
	}
}

// Cosine 余弦相似度（两侧都不做归一化假设：这里统一算模长，条目数有上限、成本可控）
func Cosine(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
