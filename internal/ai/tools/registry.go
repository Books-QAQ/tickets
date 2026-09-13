package tools

import (
	"context"
	"sort"
	"sync"
	"time"

	db "github.com/Books-QAQ/tickets/internal/db/sqlc"
)

// Deps 工具层依赖（构建期注入）
type Deps struct {
	Store *db.Store
	Cfg   Config
	Cnt   Counter
	// Stations 站点字典（提槽用）。用函数而不是字段，便于上层加 TTL 缓存后热更新。
	Stations func() *StationIndex
	Now      func() time.Time
}

func (d Deps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d Deps) inc(key string) {
	if d.Cnt != nil {
		d.Cnt.Inc(key)
	}
}

// Registry 工具注册表：**注册顺序即优先级**（精确触发先于宽泛触发，§8.1）。
type Registry struct {
	mu     sync.RWMutex
	order  []Tool
	byName map[string]Tool
	deps   Deps
	br     *breaker
}

func NewRegistry(deps Deps) *Registry {
	if deps.Cfg.Timeout <= 0 {
		deps.Cfg = DefaultConfig()
	}
	return &Registry{
		byName: map[string]Tool{},
		deps:   deps,
		br:     newBreaker(3, 30*time.Second, deps.Cnt),
	}
}

// Register 按调用顺序注册（顺序 = 优先级）
func (r *Registry) Register(t Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.order = append(r.order, t)
	r.byName[t.Name()] = t
}

// Has 白名单校验（LLM 兜底路由必须过这一关，防幻觉出不存在工具名）
func (r *Registry) Has(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.byName[name]
	return ok
}

// List 工具清单（名称 + 一句话描述），供编排层的 LLM 兜底路由使用
func (r *Registry) List() []Candidate {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Candidate, 0, len(r.order))
	for _, t := range r.order {
		out = append(out, Candidate{Name: t.Name(), Kind: t.Kind(), Desc: t.Desc()})
	}
	return out
}

// toolCategoryAffinity 工具 ↔ 分类的亲和表：**同分时用它破平**。
// 不动用"猜"：分类本身就是路由键（§5.8），同分的两个工具里跟分类一致的那个才是用户意图。
// 若同分且都不匹配（或都匹配），才交给编排层反问（E8）。
var toolCategoryAffinity = map[string]string{
	"my_tickets":       "order",
	"order_detail":     "order",
	"unpaid_orders":    "order",
	"refund_fee":       "refund",
	"refund_progress":  "refund",
	"bus_availability": "booking",
}

// Route 按问题收集候选工具。**多候选不猜**：同分时先用分类破平，仍无法区分则返回给编排层反问（§8.1）。
func (r *Registry) Route(question string, category string) []Candidate {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var out []Candidate
	for _, t := range r.order {
		if score := t.Match(question); score > 0 {
			slots := t.Slots(question)
			out = append(out, Candidate{
				Name:    t.Name(),
				Score:   score,
				Kind:    t.Kind(),
				Desc:    t.Desc(),
				Missing: slots.Missing,
				Slots:   slots,
			})
		}
	}
	// 稳定排序：分数高的在前，同分保持注册顺序（精确触发先于宽泛触发）
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })

	// 同分破平：恰好一个与分类亲和 → 标记为首选（优先于反问）
	if len(out) > 1 && out[0].Score == out[1].Score {
		var matched []int
		for i, c := range out {
			if c.Score != out[0].Score {
				break
			}
			if toolCategoryAffinity[c.Name] == category && category != "" {
				matched = append(matched, i)
			}
		}
		if len(matched) == 1 {
			out[matched[0]].Preferred = true
			// 首选提到最前
			pick := out[matched[0]]
			rest := append(append([]Candidate{}, out[:matched[0]]...), out[matched[0]+1:]...)
			out = append([]Candidate{pick}, rest...)
		}
	}
	return out
}

// deterministicUnavailable 「确定性拒绝」的原因集合：**不计入熔断**。
// 判据：这个结果换了任何时候、任何人都一样，且重试一万次也不会变——
// 那就不是"工具坏了"，而是"业务上不下结论"。
var deterministicUnavailable = map[string]bool{
	"penalty_semantics_unconfirmed": true, // 19.2 闸门关闭（宁可转人工不猜金额）
	"policy_not_confirmed":          true,
	"guest_not_allowed":             true,
}

// RunNamed 按名执行（编排层选定后的执行入口）。
// 统一包裹：熔断 → 身份校验（游客）→ 超时 → 计数。**工具不可用单独计数**（§10.1 路径④）。
func (r *Registry) RunNamed(ctx context.Context, name string, id Identity, s SlotSet) Result {
	r.mu.RLock()
	t, ok := r.byName[name]
	r.mu.RUnlock()
	if !ok {
		r.deps.inc("tool_unknown_total")
		return Result{Tool: name, Kind: KindUnavailable, Reason: "tool_not_registered"}
	}

	if !r.br.allow(name) {
		return Result{Tool: name, Kind: KindUnavailable, Reason: "circuit_open", Degraded: true}
	}

	start := time.Now()
	res, timedOut := withTimeout(ctx, r.deps.Cfg.Timeout, func(c context.Context) Result {
		return t.Exec(c, id, s)
	})
	res.Tool = name
	res.ElapsedMS = time.Since(start).Milliseconds()

	switch {
	case timedOut:
		r.br.fail(name)
		r.deps.inc("tool_timeout_total")
		res.Kind = KindUnavailable
		res.Reason = "tool_timeout"
		res.Degraded = true
	case res.Kind == KindUnavailable:
		// **确定性拒绝不计入熔断**：闸门未确认/合规不允许属于"业务上不下结论"，不是工具故障。
		// 计进去的后果（M3 实测踩过）：高频问同一条未确认规则 → 熔断被打开 →
		// 同一工具返回的原因从 `penalty_semantics_unconfirmed` 漂成 `circuit_open`，
		// 坐席在工单里看到的判定原因失真，M2 的 D1 用例因此翻红。
		if deterministicUnavailable[res.Reason] {
			r.deps.inc("tool_gate_closed_total")
			r.deps.inc("tool_unavailable_total") // 口径不变：确实没给出结论
		} else {
			r.br.fail(name)
			r.deps.inc("tool_unavailable_total")
		}
	default:
		r.br.success(name)
		r.deps.inc("tool_calls_total")
	}

	// 口径分开：查不到 ≠ 工具坏了 ≠ 槽位不全 ≠ 业务规则不允许
	switch res.Kind {
	case KindEmpty:
		r.deps.inc("tool_empty_total")
	case KindNotFound:
		r.deps.inc("tool_not_found_total")
	case KindGuestRequired:
		r.deps.inc("tool_guest_blocked_total")
	case KindSlotsIncomplete:
		r.deps.inc("tool_slots_incomplete_total")
	case KindBlocked:
		// 业务规则不允许（如已过发车时间）：不是故障，不能计入 tool_unavailable，
		// 否则"工具坏了"与"这单退不了"混成一个数字（§10.1 路径④要求分开）
		r.deps.inc("tool_blocked_total")
	}
	return res
}

// Stations 暴露站点字典（handler 组装余票工具输出时用）
func (r *Registry) Stations() *StationIndex {
	if r.deps.Stations == nil {
		return nil
	}
	return r.deps.Stations()
}
