package answercache

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------- 测试替身 ----------

type memStore struct {
	mu   sync.Mutex
	kv   map[string]string
	incr map[string]int64
	ttl  map[string]time.Duration
}

func newMemStore() *memStore {
	return &memStore{kv: map[string]string{}, incr: map[string]int64{}, ttl: map[string]time.Duration{}}
}

func (m *memStore) Get(_ context.Context, key string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.kv[key]
	return v, ok, nil
}
func (m *memStore) Set(_ context.Context, key, value string, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.kv[key] = value
	m.ttl[key] = ttl
	return nil
}
func (m *memStore) Incr(_ context.Context, key string, _ time.Duration) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.incr[key]++
	return m.incr[key], nil
}
func (m *memStore) Del(_ context.Context, keys ...string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, k := range keys {
		delete(m.kv, k)
	}
	return nil
}

// bagEmbedder 词袋向量（确定性）：用于验证"同文本余弦=1、不同文本余弦<1"的判定路径
type bagEmbedder struct{ semantic bool }

func (b bagEmbedder) Semantic() bool { return b.semantic }
func (b bagEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	dim := 64
	vec := make([]float32, dim)
	for _, tok := range strings.Fields(Normalize(text)) {
		var h int
		for _, r := range tok {
			h = (h*31 + int(r)) % dim
		}
		vec[h] += 1
	}
	// 中文字符级也入袋（无空格语言下 Fields 会整体成一个 token）
	for _, r := range text {
		if r > 127 {
			vec[int(r)%dim] += 1
		}
	}
	return vec, nil
}

type counters struct{ m map[string]int64 }

func (c *counters) Inc(k string) {
	if c.m == nil {
		c.m = map[string]int64{}
	}
	c.m[k]++
}
func (c *counters) get(k string) int64 { return c.m[k] }

func newCache(store Store, sim float64, maxPerCat int) (*Cache, *counters) {
	cnt := &counters{}
	cfg := DefaultConfig()
	cfg.Similarity = sim
	cfg.MaxPerCat = maxPerCat
	return New(store, bagEmbedder{semantic: true}, cfg, cnt), cnt
}

// ---------- 用例 ----------

// 准入：同一问题第 1 次只进候选池，第 2 次才正式收录
func TestAdmissionRequiresSecondOccurrence(t *testing.T) {
	c, cnt := newCache(newMemStore(), 0.95, 10)
	ctx := context.Background()

	first := c.Store(ctx, "退票手续费怎么算", "refund", "按档位收 10%", nil, false)
	if first.Stored || first.Reason != "pending_first_seen" {
		t.Fatalf("首次出现不该入缓存，得到 %+v", first)
	}
	if c.Lookup(ctx, "退票手续费怎么算", "refund").Hit {
		t.Fatal("候选池阶段不该命中")
	}

	second := c.Store(ctx, "退票手续费怎么算", "refund", "按档位收 10%", nil, false)
	if !second.Stored || !second.Promoted {
		t.Fatalf("第二次出现应正式收录，得到 %+v", second)
	}

	got := c.Lookup(ctx, "退票手续费怎么算", "refund")
	if !got.Hit || got.Answer != "按档位收 10%" || got.Score < 0.99 {
		t.Fatalf("应收录后命中，得到 %+v", got)
	}
	if cnt.get("cache_hit_total") == 0 || cnt.get("cache_promoted_total") == 0 {
		t.Error("命中与收录都要计数")
	}
}

// 跨分类不命中（防跨类误命中）
func TestNoCrossCategoryHit(t *testing.T) {
	c, _ := newCache(newMemStore(), 0.95, 10)
	ctx := context.Background()
	c.Store(ctx, "退款多久到账", "refund", "1-3 天", nil, false)
	c.Store(ctx, "退款多久到账", "refund", "1-3 天", nil, false)

	if got := c.Lookup(ctx, "退款多久到账", "order"); got.Hit {
		t.Error("同问法不同分类不该命中（分类是路由键，跨类答案可能不同政策）")
	}
}

// 个性化内容（含订单号/工具直答）必须被拒绝
func TestPersonalizedRefused(t *testing.T) {
	c, cnt := newCache(newMemStore(), 0.95, 10)
	ctx := context.Background()
	c.Store(ctx, "我的车票", "order", "x", nil, false) // 先让候选计数到 2
	res := c.Store(ctx, "我的车票", "order", "2026-09-15 北京西站→上海虹桥站", nil, true)
	if res.Stored || res.Reason != "personalized" {
		t.Fatalf("个性化内容必须拒绝入缓存，得到 %+v", res)
	}
	if cnt.get("cache_refused_personalized_total") == 0 {
		t.Error("拒绝要计数（否则'缓存没生效'会被误判成'问题太冷门'）")
	}
}

// LRU 淘汰按 seq（不用时间戳）：超上限时淘汰序号最小的
func TestEvictionBySeqDeterministic(t *testing.T) {
	c, cnt := newCache(newMemStore(), 0.95, 2)
	ctx := context.Background()

	seed := func(q string) {
		c.Store(ctx, q, "policy", "答"+q, nil, false)
		c.Store(ctx, q, "policy", "答"+q, nil, false)
	}
	seed("问题一")
	seed("问题二")
	seed("问题三") // 超上限 → 淘汰"问题一"

	if cnt.get("cache_evicted_total") == 0 {
		t.Error("淘汰要计数")
	}
	if got := c.Lookup(ctx, "问题一", "policy"); got.Hit {
		t.Error("最旧的条目应被淘汰")
	}
	if got := c.Lookup(ctx, "问题三", "policy"); !got.Hit {
		t.Error("最新条目应在缓存里")
	}
}

// 降级：store 为 nil 时永不命中，但不报错（静默失败是坏故障，所以这里要求"不命中"而非"报错"）
func TestDisabledCacheNeverHits(t *testing.T) {
	c := New(nil, bagEmbedder{}, DefaultConfig(), &counters{})
	ctx := context.Background()
	res := c.Store(ctx, "q", "refund", "a", nil, false)
	if res.Stored {
		t.Error("禁用状态下不该写入")
	}
	if got := c.Lookup(ctx, "q", "refund"); got.Hit || got.VectorMode != "disabled" {
		t.Errorf("禁用状态下 lookup 应明确返回 disabled，得到 %+v", got)
	}
}

// 语义匹配阈值：不同问题在字面替身下不命中（阈值 0.95 的近义判定依赖真实 embedding）
func TestSimilarityThresholdBlocksDifferentQuestion(t *testing.T) {
	c, _ := newCache(newMemStore(), 0.95, 10)
	ctx := context.Background()
	c.Store(ctx, "退票手续费怎么算", "refund", "按档位收 10%", nil, false)
	c.Store(ctx, "退票手续费怎么算", "refund", "按档位收 10%", nil, false)

	if got := c.Lookup(ctx, "今天天气怎么样", "refund"); got.Hit {
		t.Errorf("完全无关的问题不该命中，得到 %.3f", got.Score)
	}
}

// 词面替身时必须标注 vector_mode=ngram（诚实降级）
func TestVectorModeIsHonest(t *testing.T) {
	store := newMemStore()
	c := New(store, bagEmbedder{semantic: false}, DefaultConfig(), &counters{})
	got := c.Lookup(context.Background(), "退票手续费", "refund")
	if got.VectorMode != "ngram" {
		t.Errorf("非语义 embedding 必须标 ngram，得到 %q", got.VectorMode)
	}
}

func TestNormalize(t *testing.T) {
	if Normalize("  退票手续费怎么算？ ") != Normalize("退票手续费怎么算") {
		t.Error("去空白与尾部问号后应等价")
	}
	if Normalize("退款多久，到账") == Normalize("退款多久到账") {
		t.Error("标点差异应被归一（半角化并去标点）")
	}
}

func TestCosine(t *testing.T) {
	if v := Cosine([]float32{1, 0}, []float32{1, 0}); v < 0.999 {
		t.Errorf("同向量余弦应为 1，得到 %v", v)
	}
	if v := Cosine([]float32{1, 0}, []float32{0, 1}); v > 0.001 {
		t.Errorf("正交向量余弦应为 0，得到 %v", v)
	}
	if v := Cosine(nil, []float32{1}); v != 0 {
		t.Error("维度不匹配应返回 0（不 panic）")
	}
}
