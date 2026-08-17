package worker

import (
	"context"
	"fmt"
	"time"

	"github.com/Books-QAQ/tickets/internal/cache"
	db "github.com/Books-QAQ/tickets/internal/db/sqlc"
	"github.com/Books-QAQ/tickets/internal/util"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog/log"
)

type PurchaseRequestMessage struct {
	RequestID string `json:"request_id"`
	UserID    int32  `json:"user_id"`
	RouteID   int32  `json:"route_id"`
	BusID     int32  `json:"bus_id"`
	SeatID    int32  `json:"seat_id"`
}

type PurchaseTaskStatus struct {
	RequestID string    `json:"request_id"`
	UserID    int32     `json:"user_id"`
	Status    string    `json:"status"`
	TicketID  int32     `json:"ticket_id,omitempty"`
	BusID     int32     `json:"bus_id"`
	SeatID    int32     `json:"seat_id"`
	Error     string    `json:"error,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func SeatHoldOwner(userID int32, requestID string) string {
	return fmt.Sprintf("%d:%s", userID, requestID)
}

// PurchaseShard 根据座位号和工作线程数计算分片号，保证同一座位总是落到同一分片。
func PurchaseShard(seatID int32, workerCount int) int {
	if workerCount <= 0 {
		workerCount = 1
	}
	return int(seatID) % workerCount
}

func purchaseWorkerCount(config util.Config) int {
	if config.PurchaseWorkerCount <= 0 {
		return 1
	}
	return config.PurchaseWorkerCount
}

// StartPurchaseWorker 启动 N 个分片消费协程。每个协程只消费自己的分片队列，
// 保证同一座位的消息被串行处理，不同座位的消息可以并行处理。
func StartPurchaseWorker(ctx context.Context, store *db.Store, redisClient *redis.Client, config util.Config) {
	n := purchaseWorkerCount(config)
	for shard := 0; shard < n; shard++ {
		go runPurchaseWorker(ctx, store, redisClient, config, shard)
	}
}

func runPurchaseWorker(ctx context.Context, store *db.Store, redisClient *redis.Client, config util.Config, shard int) {
	defer func() {
		if r := recover(); r != nil {
			log.Error().Interface("panic", r).Int("shard", shard).Msg("purchase worker recovered from panic")
		}
	}()

	queueKey := cache.PurchaseQueueKeyForShard(shard)
	for {
		var message PurchaseRequestMessage
		found, err := cache.DequeueJSONBlocking(ctx, redisClient, queueKey, 5*time.Second, &message)
		if err != nil {
			if ctx.Err() != nil {
				return // 收到退出信号，停止消费
			}
			log.Error().Err(err).Int("shard", shard).Msg("purchase worker failed to dequeue message")
			time.Sleep(time.Second)
			continue
		}
		if !found {
			continue
		}

		processPurchaseMessage(ctx, store, redisClient, config, message)
	}
}

func processPurchaseMessage(ctx context.Context, store *db.Store, redisClient *redis.Client, config util.Config, message PurchaseRequestMessage) {
	taskKey := cache.PurchaseTaskKey(message.RequestID)
	taskTTL := config.PurchaseTaskTTL
	if taskTTL <= 0 {
		taskTTL = 30 * time.Minute
	}

	updateTask := func(status PurchaseTaskStatus) {
		status.RequestID = message.RequestID
		status.UserID = message.UserID
		status.BusID = message.BusID
		status.SeatID = message.SeatID
		_ = cache.SetJSON(ctx, redisClient, taskKey, status, taskTTL)
	}

	updateTask(PurchaseTaskStatus{
		Status:    "processing",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	})

	expectedOwner := SeatHoldOwner(message.UserID, message.RequestID)
	owner, err := cache.GetSeatHoldOwner(ctx, redisClient, message.BusID, message.SeatID)
	if err != nil {
		updateTask(PurchaseTaskStatus{
			Status:    "failed",
			Error:     "cannot verify seat hold",
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
		})
		return
	}

	if owner != expectedOwner {
		updateTask(PurchaseTaskStatus{
			Status:    "failed",
			Error:     "seat hold is missing or owned by another request",
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
		})
		return
	}

	result, err := store.PurchaseTicketTx(ctx, db.PurchaseTicketTxParams{
		UserID: message.UserID,
		BusID:  message.BusID,
		SeatID: message.SeatID,
	})
	if err != nil {
		_ = cache.ReleaseSeatHold(ctx, redisClient, message.BusID, message.SeatID, expectedOwner)
		updateTask(PurchaseTaskStatus{
			Status:    "failed",
			Error:     err.Error(),
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
		})
		return
	}

	_ = cache.ReleaseSeatHold(ctx, redisClient, message.BusID, message.SeatID, expectedOwner)
	invalidateRoutesCache(ctx, store, redisClient, message.RouteID, message.BusID)

	updateTask(PurchaseTaskStatus{
		Status:    "succeeded",
		TicketID:  result.TicketID,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	})
}

func invalidateRoutesCache(ctx context.Context, store *db.Store, redisClient *redis.Client, routeID, busID int32) {
	bus, err := store.GetBusByID(ctx, busID)
	if err != nil {
		return
	}

	targetRouteID := routeID
	if targetRouteID == 0 {
		targetRouteID = bus.RouteID
	}

	route, err := store.GetRouteByID(ctx, targetRouteID)
	if err != nil {
		return
	}

	key := cache.RoutesQueryCacheKey(route.OriginTerminalID, route.DestinationTerminalID, bus.DepartureTime)
	_ = cache.DeleteKey(ctx, redisClient, key)
}
