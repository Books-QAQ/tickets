// Package metrics 收集客服链路的运行指标并渲染成 Prometheus 文本（§14.2）。
//
// 设计取舍（都要理由）：
//  1. **不引 prometheus client 库**：本项目只需要「计数 + 少量分位数 + 若干 gauge」，
//     手写 exposition 文本约 60 行；引库会带进依赖与"必须按它的方式组织指标"的约束。
//  2. **计数真相源仍是 Go（单一计数点）**：这里的计数器直接读 `kb.Aux`（M1 起就用它），
//     本包只做「键 → 指标名 / 标签」的翻译 + 分位数与 gauge 的维护，不另建一套计数。
//  3. **分位数用固定窗口样本（默认 512）现算**：客服单轮延迟分布不需要 HDR 直方图精度，
//     但**要能看到 p95**（只看均值会被长尾骗）。窗口固定 ⇒ 反映"最近"的运行状态。
//  4. **gauge 惰性求值**：`kb_chunks` / `qdrant_points` 这类只有在被 scrape 时才去查，
//     平时零成本；查失败就报 `-1`（**不谎报 0**，0 与"查不到"是两件事）。
package metrics

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Books-QAQ/tickets/internal/ai/kb"
)

// window 分位数样本窗口大小（每个指标一份）
const window = 512

// Latency 固定窗口的延迟样本（毫秒）
type Latency struct {
	mu  sync.Mutex
	buf []float64
	n   int
	pos int
}

func (l *Latency) Observe(ms float64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.buf == nil {
		l.buf = make([]float64, window)
	}
	l.buf[l.pos] = ms
	l.pos = (l.pos + 1) % window
	if l.n < window {
		l.n++
	}
}

// Percentile 返回分位数（p ∈ [0,1]）；无样本返回 -1（不是 0）
func (l *Latency) Percentile(p float64) float64 {
	l.mu.Lock()
	s := make([]float64, l.n)
	copy(s, l.buf[:l.n])
	l.mu.Unlock()
	if len(s) == 0 {
		return -1
	}
	sort.Float64s(s)
	idx := int(p * float64(len(s)-1))
	if idx < 0 {
		idx = 0
	}
	return s[idx]
}

// Avg 平均值（无样本 -1）
func (l *Latency) Avg() float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.n == 0 {
		return -1
	}
	var sum float64
	for i := 0; i < l.n; i++ {
		sum += l.buf[i]
	}
	return sum / float64(l.n)
}

// Count 样本数
func (l *Latency) Count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.n
}

// Collector 指标收集器
type Collector struct {
	started time.Time
	mu      sync.RWMutex
	lat     map[string]*Latency
	gauges  map[string]func() float64
	aux     *kb.Aux
}

func NewCollector(aux *kb.Aux) *Collector {
	return &Collector{
		started: time.Now(),
		lat:     map[string]*Latency{},
		gauges:  map[string]func() float64{},
		aux:     aux,
	}
}

// ObserveStage 记录某个编排阶段/节点的耗时（ms）。名字来自图事件的 node/decision。
func (c *Collector) ObserveStage(stage string, ms float64) {
	if stage == "" || ms < 0 {
		return
	}
	c.mu.Lock()
	l, ok := c.lat[stage]
	if !ok {
		l = &Latency{}
		c.lat[stage] = l
	}
	c.mu.Unlock()
	l.Observe(ms)
}

// StageStats 返回某阶段的分位数（供 admin JSON 与日志用）
func (c *Collector) StageStats(stage string) (p50, p95, avg float64, n int) {
	c.mu.RLock()
	l := c.lat[stage]
	c.mu.RUnlock()
	if l == nil {
		return -1, -1, -1, 0
	}
	return l.Percentile(0.50), l.Percentile(0.95), l.Avg(), l.Count()
}

// Inc 计数（转发给 Aux —— 单一计数点，不另建一套）
func (c *Collector) Inc(key string) {
	if c.aux != nil {
		c.aux.Inc(key)
	}
}

// RegisterGauge 注册惰性 gauge（scrape 时才求值）
func (c *Collector) RegisterGauge(name string, fn func() float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gauges[name] = fn
}

// UptimeSeconds 运行时长
func (c *Collector) UptimeSeconds() float64 { return time.Since(c.started).Seconds() }

// Snapshot 返回计数快照（原样透出 Aux 的键，供 admin JSON 大盘）
func (c *Collector) Snapshot() map[string]int64 {
	if c.aux == nil {
		return map[string]int64{}
	}
	return c.aux.Snapshot()
}

// stageOfNode 把图事件的 node/decision 归并成稳定的阶段名（避免指标基数爆炸）
var stageOfNode = map[string]string{
	"load_context": "load_context",
	"pre_intent":   "pre_intent",
	"classify":     "classify",
	"coref":        "coref",
	"cache_lookup": "cache_lookup",
	"route_tool":   "route_tool",
	"exec_tool":    "exec_tool",
	"retrieve":     "retrieve",
	"rewrite":      "rewrite",
	"generate":     "generate",
	"verify":       "verify",
	"transfer":     "transfer",
	"finalize":     "finalize",
}

// StageName 归一化阶段名（未知节点丢弃，防基数爆炸）
func StageName(node string) string {
	if s, ok := stageOfNode[node]; ok {
		return s
	}
	return ""
}

// Render 渲染 Prometheus exposition 文本（text/plain; version=0.0.4）
func (c *Collector) Render() string {
	var b strings.Builder

	// —— 计数（来自 Aux：单一计数点）——
	counters := c.Snapshot()
	keys := make([]string, 0, len(counters))
	for k := range counters {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	labeled := map[string][]int{} // 指标名 → 出现的行号（用于只打一次 TYPE）
	for _, k := range keys {
		name, label := splitLabeled(k)
		metric := "cs_" + name
		if _, done := labeled[metric]; !done {
			fmt.Fprintf(&b, "# TYPE %s counter\n", metric)
			labeled[metric] = nil
		}
		if label != "" {
			fmt.Fprintf(&b, "%s{%s} %d\n", metric, label, counters[k])
		} else {
			fmt.Fprintf(&b, "%s %d\n", metric, counters[k])
		}
	}

	// —— 分段延迟（p50/p95/avg 三个 gauge，比直方图更好读，口径见包注释）——
	c.mu.RLock()
	stages := make([]string, 0, len(c.lat))
	for s := range c.lat {
		stages = append(stages, s)
	}
	c.mu.RUnlock()
	sort.Strings(stages)
	if len(stages) > 0 {
		b.WriteString("# TYPE cs_stage_ms gauge\n")
		for _, s := range stages {
			p50, p95, avg, n := c.StageStats(s)
			fmt.Fprintf(&b, "cs_stage_ms{stage=%q,quantile=\"0.5\"} %.2f\n", s, p50)
			fmt.Fprintf(&b, "cs_stage_ms{stage=%q,quantile=\"0.95\"} %.2f\n", s, p95)
			fmt.Fprintf(&b, "cs_stage_ms{stage=%q,quantile=\"avg\"} %.2f\n", s, avg)
			fmt.Fprintf(&b, "cs_stage_samples{stage=%q} %d\n", s, n)
		}
	}

	// —— gauge（惰性求值）——
	b.WriteString("# TYPE cs_uptime_seconds gauge\n")
	fmt.Fprintf(&b, "cs_uptime_seconds %.0f\n", c.UptimeSeconds())
	c.mu.RLock()
	gkeys := make([]string, 0, len(c.gauges))
	for k := range c.gauges {
		gkeys = append(gkeys, k)
	}
	c.mu.RUnlock()
	sort.Strings(gkeys)
	for _, k := range gkeys {
		c.mu.RLock()
		fn := c.gauges[k]
		c.mu.RUnlock()
		val := -1.0
		if fn != nil {
			val = fn() // 失败由注册方返回 -1（不谎报 0）
		}
		fmt.Fprintf(&b, "# TYPE cs_%s gauge\ncs_%s %.0f\n", k, k, val)
	}
	return b.String()
}

// splitLabeled 把 `tool_calls_by_tool:my_tickets` 解析成 ("tool_calls_by_tool_total", `tool="my_tickets"`)
func splitLabeled(key string) (string, string) {
	if i := strings.Index(key, ":"); i > 0 {
		name, label := key[:i], key[i+1:]
		if !strings.HasSuffix(name, "_total") {
			name += "_total"
		}
		return name, fmt.Sprintf("tool=%q", label)
	}
	return key, ""
}
