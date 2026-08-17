package worker

import (
	"context"
	"encoding/json"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/rs/zerolog/log"

	db "github.com/Books-QAQ/tickets/internal/db/sqlc"
	"github.com/Books-QAQ/tickets/internal/queue"
	"github.com/Books-QAQ/tickets/internal/util"
)

const maxExpiredOrdersPerBatch = 100

// StartOrderExpiryConsumer 主路径：DLX+TTL 延迟队列消费者。
// 下单时发布的关单消息，TTL 到期后经死信交换机投递到这里，触发关单。
func StartOrderExpiryConsumer(ctx context.Context, store *db.Store, mq *queue.RabbitMQ) {
	deliveries, err := mq.ConsumeOrderExpiry()
	if err != nil {
		log.Error().Err(err).Msg("failed to start order expiry consumer")
		return
	}

	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Error().Interface("panic", r).Msg("order expiry consumer recovered from panic")
			}
		}()

		for {
			select {
			case <-ctx.Done():
				return
			case delivery, ok := <-deliveries:
				if !ok {
					log.Warn().Msg("order expiry consumer channel closed")
					return
				}
				handleOrderExpiry(ctx, store, delivery)
			}
		}
	}()
}

func handleOrderExpiry(ctx context.Context, store *db.Store, delivery amqp.Delivery) {
	var msg queue.OrderExpiryMessage
	if err := json.Unmarshal(delivery.Body, &msg); err != nil {
		log.Error().Err(err).Msg("failed to unmarshal order expiry message")
		_ = delivery.Nack(false, false) // 无法解析，丢弃不重试
		return
	}

	order, err := store.CancelExpiredOrderTx(ctx, msg.OrderNo)
	if err != nil {
		log.Error().Err(err).Str("order_no", msg.OrderNo).Msg("failed to cancel expired order")
		_ = delivery.Nack(false, true) // 处理失败，重新入队重试
		return
	}
	if order.Status == "canceled" {
		log.Info().Str("order_no", msg.OrderNo).Msg("expired order canceled via DLX+TTL")
	}
	_ = delivery.Ack(false)
}

// StartOrderExpiryFallbackScanner 兜底：定时扫描，防止 RabbitMQ 消息丢失导致的漏关。
// 扫的是"过期了整整一个订单周期还没关"的订单，与 DLX+TTL（到期即投递）不冲突，
// 依赖 CancelExpiredOrderTx 的条件更新幂等保证两条路径不会重复关同一单。
func StartOrderExpiryFallbackScanner(ctx context.Context, store *db.Store, config util.Config) {
	interval := config.OrderExpireInterval
	if interval <= 0 {
		interval = time.Minute
	}

	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Error().Interface("panic", r).Msg("order expiry fallback scanner recovered from panic")
			}
		}()

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				closeExpiredOrders(ctx, store, config)
			}
		}
	}()
}

func closeExpiredOrders(ctx context.Context, store *db.Store, config util.Config) {
	// 兜底窗口 = 一个完整订单周期，确保 DLX+TTL 已先处理过（消息丢失才会轮到兜底）
	fallbackWindow := config.OrderExpireDuration
	if fallbackWindow <= 0 {
		fallbackWindow = 15 * time.Minute
	}
	deadline := time.Now().Add(-fallbackWindow)

	orders, err := store.ListExpiredOrders(ctx, deadline, maxExpiredOrdersPerBatch)
	if err != nil {
		log.Error().Err(err).Msg("fallback scanner failed to list expired orders")
		return
	}

	for _, order := range orders {
		canceled, err := store.CancelExpiredOrderTx(ctx, order.OrderNo)
		if err != nil {
			log.Error().Err(err).Str("order_no", order.OrderNo).Msg("fallback failed to cancel expired order")
			continue
		}
		if canceled.Status == "canceled" {
			log.Info().Str("order_no", order.OrderNo).Msg("expired order canceled via fallback scan")
		}
	}
}
