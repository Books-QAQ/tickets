package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/rs/zerolog/log"

	db "github.com/Books-QAQ/tickets/internal/db/sqlc"
	"github.com/Books-QAQ/tickets/internal/payment"
	"github.com/Books-QAQ/tickets/internal/queue"
	"github.com/Books-QAQ/tickets/internal/util"
)

const maxExpiredOrdersPerBatch = 100

// StartOrderExpiryConsumer 主路径：DLX+TTL 延迟队列消费者。
// 下单时发布的关单消息，TTL 到期后经死信交换机投递到这里，触发关单。
func StartOrderExpiryConsumer(ctx context.Context, store *db.Store, mq *queue.RabbitMQ, alipay payment.Provider) {
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
				handleOrderExpiry(ctx, store, alipay, delivery)
			}
		}
	}()
}

func handleOrderExpiry(ctx context.Context, store *db.Store, alipay payment.Provider, delivery amqp.Delivery) {
	var msg queue.OrderExpiryMessage
	if err := json.Unmarshal(delivery.Body, &msg); err != nil {
		log.Error().Err(err).Msg("failed to unmarshal order expiry message")
		_ = delivery.Nack(false, false) // 无法解析，丢弃不重试
		return
	}

	order, err := closeExpiredOrder(ctx, store, alipay, msg.OrderNo)
	if err != nil {
		log.Error().Err(err).Str("order_no", msg.OrderNo).Msg("failed to close expired order")
		_ = delivery.Nack(false, true) // 处理失败，重新入队重试
		return
	}
	if order.Status == "canceled" {
		log.Info().Str("order_no", msg.OrderNo).Msg("expired order canceled via DLX+TTL")
	}
	_ = delivery.Ack(false)
}

// SeatHoldOwner 生成座位预占锁的 owner 标识（订单链路下单时用）。
func SeatHoldOwner(userID int32, requestID string) string {
	return fmt.Sprintf("%d:%s", userID, requestID)
}

// StartOrderExpiryFallbackScanner 兜底：定时扫描，防止 RabbitMQ 消息丢失导致的漏关。
// 扫的是"过期了整整一个订单周期还没关"的订单，与 DLX+TTL（到期即投递）不冲突，
// 依赖 CancelExpiredOrderTx 的条件更新幂等保证两条路径不会重复关同一单。
func StartOrderExpiryFallbackScanner(ctx context.Context, store *db.Store, config util.Config, alipay payment.Provider) {
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
				closeExpiredOrders(ctx, store, config, alipay)
			}
		}
	}()
}

func closeExpiredOrders(ctx context.Context, store *db.Store, config util.Config, alipay payment.Provider) {
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
		canceled, err := closeExpiredOrder(ctx, store, alipay, order.OrderNo)
		if err != nil {
			log.Error().Err(err).Str("order_no", order.OrderNo).Msg("fallback failed to close expired order")
			continue
		}
		if canceled.Status == "canceled" {
			log.Info().Str("order_no", order.OrderNo).Msg("expired order canceled via fallback scan")
		}
	}
}

// closeExpiredOrder 关单协调：关单前先查支付宝（若为支付宝渠道），
// 已付款则补出票，未付款才关单；查单失败则保守跳过（等下一轮重试，避免误关已付款订单）。
// 返回处理后的订单。
func closeExpiredOrder(ctx context.Context, store *db.Store, alipay payment.Provider, orderNo string) (db.Order, error) {
	order, err := store.GetOrderByNo(ctx, orderNo)
	if err != nil {
		// 订单不存在（历史脏消息/已被删除）：视为已处理，返回空订单不报错
		return db.Order{}, nil
	}
	if order.Status != "pending" {
		return order, nil // 已支付或已关闭，跳过
	}

	// 支付宝渠道：关单前查单，避免关掉"已付款但通知未达"的订单
	if order.PayChannel.String == "alipay" && alipay != nil {
		paid, _, qerr := alipay.QueryOrder(ctx, orderNo)
		if qerr != nil {
			// 查单失败：保守跳过，不关单，等下轮重试（误关已付款订单比晚关单代价大）
			return order, fmt.Errorf("query alipay before cancel: %w", qerr)
		}
		if paid {
			// 已付款：补出票而非关单
			if _, serr := store.SettleOrderTx(ctx, db.PayOrderTxParams{
				OrderNo: orderNo,
				UserID:  order.UserID,
				BusID:   order.BusID,
				SeatID:  order.SeatID,
				Channel: "alipay",
			}); serr != nil {
				return order, fmt.Errorf("settle expired paid order: %w", serr)
			}
			log.Info().Str("order_no", orderNo).Msg("expired order settled (paid before expiry)")
			order.Status = "paid"
			return order, nil
		}
	}

	return store.CancelExpiredOrderTx(ctx, orderNo)
}
