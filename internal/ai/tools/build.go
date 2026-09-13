package tools

import (
	"context"
	"sync"
	"time"

	db "github.com/Books-QAQ/tickets/internal/db/sqlc"
)

// BuildRegistry 按 §8.1 的注册顺序装配（**顺序 = 优先级**：精确触发先于宽泛触发）。
//
// 顺序理由：order_detail（认订单号/订单字样）比 my_tickets（认"我的票"）更精确，
// 先用它拦下带订单号的提问；refund_fee 必须在 refund_progress 之前（"退票手续费"含"退票款"之外的
// 更精确意图）；create_support_ticket 的 Match 恒为 0，只由转人工路径按名调用，放最后。
func BuildRegistry(deps Deps) *Registry {
	r := NewRegistry(deps)
	r.Register(orderDetailTool{base{deps}})
	r.Register(refundFeeTool{base{deps}})
	r.Register(refundProgressTool{base{deps}})
	r.Register(unpaidOrdersTool{base{deps}})
	r.Register(busAvailabilityTool{base{deps}})
	r.Register(myTicketsTool{base{deps}})
	r.Register(supportTicketTool{base{deps}})
	return r
}

// BuildStationIndex 从 terminals + cities 构建站点字典（提槽用）
func BuildStationIndex(ctx context.Context, store *db.Store) (*StationIndex, error) {
	trows, err := store.ListTerminals(ctx)
	if err != nil {
		return nil, err
	}
	crows, err := store.GetAllCities(ctx)
	if err != nil {
		return nil, err
	}
	terms := make([]TerminalRow, 0, len(trows))
	for _, t := range trows {
		terms = append(terms, TerminalRow{ID: t.ID, Name: t.Name, CityID: t.CityID})
	}
	cities := make([]CityRow, 0, len(crows))
	for _, c := range crows {
		cities = append(cities, CityRow{ID: c.ID, Name: c.Name})
	}
	return NewStationIndex(terms, cities), nil
}

// NewStationProvider 站点字典提供者（60s TTL 缓存）。
// 站点是低频变更数据，但不该"新增站点要重启服务"；构建失败时沿用旧字典，不阻塞提槽。
func NewStationProvider(store *db.Store) func() *StationIndex {
	var mu sync.Mutex
	var idx *StationIndex
	var built time.Time

	return func() *StationIndex {
		mu.Lock()
		defer mu.Unlock()
		if idx != nil && time.Since(built) < 60*time.Second {
			return idx
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		fresh, err := BuildStationIndex(ctx, store)
		if err != nil {
			return idx
		}
		idx, built = fresh, time.Now()
		return idx
	}
}
