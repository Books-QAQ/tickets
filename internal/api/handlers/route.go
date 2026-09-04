package handlers

import (
	"fmt"
	"math/rand"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/Books-QAQ/tickets/internal/cache"
	db "github.com/Books-QAQ/tickets/internal/db/sqlc"
	"github.com/Books-QAQ/tickets/internal/token"
	"github.com/Books-QAQ/tickets/internal/util"
	"github.com/redis/go-redis/v9"
)

type RouteHandler struct {
	store      *db.Store
	redis      *redis.Client
	tokenMaker token.Maker
	config     util.Config
}

type routeResponse struct {
	RouteID         int32     `json:"route_id"`
	BusID           int32     `json:"bus_id"`
	OriginCity      string    `json:"origin_city"`
	DestinationCity string    `json:"destination_city"`
	DepartureTime   time.Time `json:"departure_time"`
	ArrivalTime     time.Time `json:"arrival_time"`
	SaleOpenAt      time.Time `json:"sale_open_at"`
	CanPurchase     bool      `json:"can_purchase"`
	AvailableSeats  int64     `json:"available_seats"`
	Price           int32     `json:"price"`
}

func NewRouteHandler(store *db.Store, redisClient *redis.Client, tokenMaker token.Maker, config util.Config) *RouteHandler {
	return &RouteHandler{
		store:      store,
		redis:      redisClient,
		tokenMaker: tokenMaker,
		config:     config,
	}
}

func parseDepartureDate(value string) (time.Time, error) {
	if parsed, err := time.ParseInLocation("2006-01-02", value, beijingLocation); err == nil {
		return parsed, nil
	}

	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed.In(beijingLocation), nil
	}

	if parsed, err := time.ParseInLocation("2006-01-02T15:04:05", value, beijingLocation); err == nil {
		return parsed, nil
	}

	return time.Time{}, fiber.NewError(http.StatusBadRequest, "invalid departure date, use YYYY-MM-DD")
}

func (h *RouteHandler) SearchRoutes(c *fiber.Ctx) error {
	originTerminalID, err := strconv.ParseInt(c.Query("origin_city_id"), 10, 32)
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid origin terminal"})
	}

	destinationTerminalID, err := strconv.ParseInt(c.Query("destination_city_id"), 10, 32)
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid destination terminal"})
	}

	// 防穿透：出发/到达同一站点必然无线路，直接拒绝，不查缓存也不查库
	if originTerminalID == destinationTerminalID {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "origin and destination cannot be the same"})
	}

	departureDate, err := parseDepartureDate(c.Query("departure_time"))
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	}

	cacheKey := cache.RoutesQueryCacheKey(int32(originTerminalID), int32(destinationTerminalID), departureDate)
	cachedRoutes, found, err := cache.GetJSON[[]routeResponse](c.Context(), h.redis, cacheKey)
	if err == nil && found {
		return c.Status(http.StatusOK).JSON(cachedRoutes)
	}

	// 防击穿：缓存 miss 后抢重建锁，只放行一个请求回源 DB 并写缓存；
	// 其余请求短暂等待后重读缓存，避免热点 key 过期瞬间所有请求同时打 DB。
	lockKey := cacheKey + ":lock"
	lockOwner := fmt.Sprintf("%d", time.Now().UnixNano())
	locked, lerr := cache.AcquireCacheRebuildLock(c.Context(), h.redis, lockKey, lockOwner, 5*time.Second)
	if lerr == nil && !locked {
		// 有人在重建：等待 50ms 后重读一次，命中则直接返回
		time.Sleep(50 * time.Millisecond)
		if again, ok, _ := cache.GetJSON[[]routeResponse](c.Context(), h.redis, cacheKey); ok {
			return c.Status(http.StatusOK).JSON(again)
		}
	}

	routes, err := h.store.ListRoutes(c.Context(), db.ListRoutesParams{
		OriginTerminalID:      int32(originTerminalID),
		DestinationTerminalID: int32(destinationTerminalID),
		DepartureTime:         departureDate,
	})
	if err != nil {
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "Failed to fetch routes"})
	}

	response := make([]routeResponse, 0, len(routes))
	nowInBeijing := time.Now().In(beijingLocation)

	// 并发查询每个班次的售票窗口，消除 N+1 串行查询
	type saleWindow struct {
		saleOpenAt time.Time
		err        error
	}
	windows := make([]saleWindow, len(routes))
	var wg sync.WaitGroup
	for i := range routes {
		wg.Add(1)
		go func(idx int, busID int32) {
			defer wg.Done()
			windows[idx].saleOpenAt, windows[idx].err = h.store.GetBusSaleOpenAt(c.Context(), busID)
		}(i, routes[i].BusID)
	}
	wg.Wait()

	for i, route := range routes {
		if windows[i].err != nil {
			return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "Failed to fetch route sale window"})
		}
		saleOpenAt := windows[i].saleOpenAt

		response = append(response, routeResponse{
			RouteID:         route.RouteID,
			BusID:           route.BusID,
			OriginCity:      route.OriginTerminalName,
			DestinationCity: route.DestinationTerminalName,
			DepartureTime:   route.DepartureTime,
			ArrivalTime:     route.ArrivalTime,
			SaleOpenAt:      saleOpenAt,
			CanPurchase:     !nowInBeijing.Before(saleOpenAt.In(beijingLocation)),
			AvailableSeats:  route.AvailableSeats,
			Price:           route.Price,
		})
	}

	// 防穿透（空值缓存）+ 防雪崩（TTL 随机抖动）：空结果缓存短 TTL，有结果缓存 3min±60s 抖动，
	// 使大量 key 的过期时刻错开，避免同一瞬间集体失效打爆 DB。
	if len(response) == 0 {
		_ = cache.SetJSON(c.Context(), h.redis, cacheKey, response, 30*time.Second)
	} else {
		ttl := 3*time.Minute + time.Duration(rand.Intn(60))*time.Second
		_ = cache.SetJSON(c.Context(), h.redis, cacheKey, response, ttl)
	}
	// 释放重建锁（仅当本请求抢到锁时才释放，Lua CAS 防误删他人锁）
	if lerr == nil && locked {
		_ = cache.ReleaseCacheRebuildLock(c.Context(), h.redis, lockKey, lockOwner)
	}

	return c.Status(http.StatusOK).JSON(response)
}
